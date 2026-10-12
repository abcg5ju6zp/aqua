package vacuum

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
)

// LeaseKind tells why a lease is held. It is diagnostic on machines where
// flock is authoritative; it also selects the fallback TTL used across hosts
// that don't share flock semantics (e.g. NFS).
type LeaseKind string

const (
	LeaseInstall LeaseKind = "install"
	LeaseExec    LeaseKind = "exec"
)

const (
	// installLeaseTTL is the fallback lifetime for an install lease. Local
	// liveness is proven by flock, which the kernel frees on process death; the
	// TTL only protects hosts sharing the metadata directory over NFS.
	installLeaseTTL = 24 * time.Hour
	// execLeaseTTL is longer because after execve(2) aqua code no longer runs
	// and can't heart-beat the lease; the inherited file descriptor is released
	// only when the tool exits. A crashed tool on another host delays deletion
	// by at most this TTL.
	execLeaseTTL = 7 * 24 * time.Hour
)

// leaseRecord is the durable, recoverable part of a lease. It is written before
// the flock is taken, so a lease that survives a crash can be found and reaped.
type leaseRecord struct {
	Kind      string    `json:"kind"`
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// lease is an acquired package lease. Release removes the durable record and
// releases the flock.
type lease struct {
	pkgPath    string
	recordPath string
	handle     lock.Handle
}

// Release removes the lease record and releases the lock.
func (l *lease) Release() error {
	var err error
	if rmErr := os.Remove(l.recordPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = fmt.Errorf("remove a lease record: %w", rmErr)
	}
	if l.handle != nil {
		if relErr := l.handle.Release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}
	return err
}

// AcquireLease acquires a shared, non-blocking lease on the package version.
// If exec is true, an exec lease is taken: its lock file descriptor survives
// execve(2) so that the lock is held until the executed tool exits. If
// tracking is disabled, a no-op lease is returned: vacuum itself is rejected
// in that mode.
func (c *Client) AcquireLease(pkgPath string, exec bool) (lock.Handle, error) {
	if c.updateDisabled {
		return lock.NoopHandle(), nil
	}
	kind := LeaseInstall
	ttl := installLeaseTTL
	if exec {
		kind = LeaseExec
		ttl = execLeaseTTL
	}
	now := time.Now()
	dir := c.leaseDir(pkgPath)
	if err := osfile.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("create a lease directory: %w", err)
	}
	b := make([]byte, 16) //nolint:mnd
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate a lease id: %w", err)
	}
	id := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
	recordPath := filepath.Join(dir, id+".json")
	rec := leaseRecord{
		Kind:      string(kind),
		PID:       os.Getpid(),
		Host:      c.host,
		StartedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	recBody, err := json.MarshalIndent(&rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal a lease record: %w", err)
	}
	if err := osfile.WriteFileAtomic(recordPath, recBody, filePermission); err != nil {
		return nil, fmt.Errorf("write a lease record: %w", err)
	}
	h, err := lock.Acquire(c.leaseLockFile(pkgPath), false, exec)
	if err != nil {
		_ = os.Remove(recordPath)
		return nil, fmt.Errorf("acquire an %s lease: %w", kind, err)
	}
	return &lease{
		pkgPath:    pkgPath,
		recordPath: recordPath,
		handle:     h,
	}, nil
}

// AcquireExclusiveLease acquires the exclusive lease vacuum needs before
// quarantining a package version. It conflicts with every shared install/exec
// lease. The caller must release the handle.
func (c *Client) AcquireExclusiveLease(pkgPath string) (lock.Handle, error) {
	h, err := lock.Acquire(c.leaseLockFile(pkgPath), true, false)
	if err != nil {
		return nil, fmt.Errorf("acquire an exclusive lease: %w", err)
	}
	return h, nil
}

// HasUnexpiredLease inspects durable lease records and must be called while the
// caller holds the exclusive flock: flock proves no local holder is alive, so
// remaining unexpired records can only belong to other hosts (NFS) or crashed
// processes. Expired records and records of dead local processes are reaped.
// Failures to reap a record are treated conservatively: the lease is reported
// active.
func (c *Client) HasUnexpiredLease(pkgPath string, now time.Time) (bool, error) {
	entries, err := os.ReadDir(c.leaseDir(pkgPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read a lease directory: %w", err)
	}
	active := false
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == lockFileName {
			continue
		}
		kept, err := c.evaluateLeaseRecord(pkgPath, entry.Name(), now)
		if err != nil {
			// A record that couldn't be read or parsed fails safe: treat the
			// version as active and let the caller decide on the error.
			return true, err
		}
		if kept {
			active = true
		}
	}
	return active, nil
}

// evaluateLeaseRecord reports whether the record must keep the lease active.
// Reapable records (expired or dead local processes) are removed; a record
// that can't be parsed or reaped is kept, failing safe.
func (c *Client) evaluateLeaseRecord(pkgPath, name string, now time.Time) (bool, error) {
	path := filepath.Join(c.leaseDir(pkgPath), name)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read a lease record: %w", err)
	}
	var rec leaseRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		// Can't interpret the record: surface it; callers treat errors as
		// "active" and the version is revisited on the next run.
		return true, fmt.Errorf("parse a lease record: %w", err)
	}
	if !rec.ExpiresAt.After(now) || (rec.Host == c.host && !processAlive(rec.PID)) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, nil
	}
	return true, nil
}

// purgeLeases removes the whole lease directory of a package version. It is
// used by vacuum after the exclusive lease has been acquired and by "aqua rm".
func (c *Client) purgeLeases(pkgPath string) error {
	if err := os.RemoveAll(c.leaseDir(pkgPath)); err != nil {
		return fmt.Errorf("remove lease files: %w", err)
	}
	return nil
}
