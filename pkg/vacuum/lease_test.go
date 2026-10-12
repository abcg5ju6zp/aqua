package vacuum_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/vacuum"
	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
)

const leasePkgPath = "pkgs/github_release/github.com/cli/cli/v2.65.0/gh_2.65.0_macOS_arm64.zip"

func TestAcquireLease_lifecycle(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})

	lease, err := client.AcquireLease(leasePkgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	leaseDir := filepath.Join(rootDir, "metadata", "leases", filepath.FromSlash(leasePkgPath))
	entries, err := os.ReadDir(leaseDir)
	if err != nil {
		t.Fatal(err)
	}
	var records int
	for _, e := range entries {
		if e.Name() != "lock" {
			records++
		}
	}
	if records != 1 {
		t.Fatalf("expected one lease record, got %d", records)
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(leaseDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "lock" {
			t.Fatalf("the lease record must be removed: %s", e.Name())
		}
	}
}

func TestAcquireLease_blocksExclusiveLease(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})

	lease, err := client.AcquireLease(leasePkgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })

	if _, err := client.AcquireExclusiveLease(leasePkgPath); !errors.Is(err, lock.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestAcquireLease_disabledTracking(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir, DisableTracking: true})

	lease, err := client.AcquireLease(leasePkgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "metadata")); !os.IsNotExist(err) {
		t.Fatal("no lease directory must be created when tracking is disabled")
	}
}

type leaseRecordFixture struct {
	Kind      string    `json:"kind"`
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func writeLeaseRecord(t *testing.T, rootDir, host string, pid int, expiresAt time.Time) {
	t.Helper()
	dir := filepath.Join(rootDir, "metadata", "leases", filepath.FromSlash(leasePkgPath))
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	rec := leaseRecordFixture{
		Kind:      "install",
		PID:       pid,
		Host:      host,
		StartedAt: expiresAt.Add(-time.Hour),
		ExpiresAt: expiresAt,
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHasUnexpiredLease(t *testing.T) {
	t.Parallel()
	now := time.Now()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	data := leaseTestCases(now, host)
	for _, tt := range data {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rootDir := t.TempDir()
			client := vacuum.New(&config.Param{RootDir: rootDir})
			writeLeaseRecord(t, rootDir, tt.host, tt.pid, tt.expiresAt)
			if tt.corrupt {
				dir := filepath.Join(rootDir, "metadata", "leases", filepath.FromSlash(leasePkgPath))
				if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			assertLeaseState(t, client, rootDir, now, tt.expActive, tt.expErr, tt.expReaped)
		})
	}
}

type leaseTestCase struct {
	name      string
	host      string
	pid       int
	expiresAt time.Time
	corrupt   bool
	expActive bool
	expErr    bool
	expReaped bool
}

func leaseTestCases(now time.Time, host string) []leaseTestCase {
	return []leaseTestCase{
		{
			name:      "expired record is reaped",
			host:      host,
			pid:       os.Getpid(),
			expiresAt: now.Add(-time.Hour),
			expReaped: true,
		},
		{
			name:      "record of a dead local process is reaped",
			host:      host,
			pid:       2_147_483_646,
			expiresAt: now.Add(time.Hour),
			expReaped: true,
		},
		{
			name:      "unexpired record of a remote host is active",
			host:      "another-host.example.com",
			pid:       1,
			expiresAt: now.Add(time.Hour),
			expActive: true,
		},
		{
			name:      "a corrupt record is treated conservatively",
			host:      host,
			pid:       2_147_483_646,
			expiresAt: now.Add(-time.Hour),
			corrupt:   true,
			expActive: true,
			expErr:    true,
		},
	}
}

func assertLeaseState(t *testing.T, client *vacuum.Client, rootDir string, now time.Time, expActive, expErr, expReaped bool) {
	t.Helper()
	exclusive, err := client.AcquireExclusiveLease(leasePkgPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exclusive.Release() })

	active, err := client.HasUnexpiredLease(leasePkgPath, now)
	if expErr {
		if err == nil {
			t.Fatal("expected an error for the corrupt record")
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if active != expActive {
		t.Fatalf("active = %v, want %v", active, expActive)
	}
	records := countLeaseRecords(t, rootDir)
	if expReaped && records != 0 {
		t.Fatalf("the record must be reaped, but %d records remain", records)
	}
	if expActive && records == 0 {
		t.Fatal("an active record must be kept")
	}
}

func countLeaseRecords(t *testing.T, rootDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(rootDir, "metadata", "leases", filepath.FromSlash(leasePkgPath)))
	if err != nil {
		t.Fatal(err)
	}
	var records int
	for _, e := range entries {
		if e.Name() != "lock" {
			records++
		}
	}
	return records
}

func TestLockVacuum_exclusive(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})

	h, err := client.LockVacuum()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Release() })

	if _, err := client.LockVacuum(); !errors.Is(err, vacuum.ErrVacuumRunning) {
		t.Fatalf("expected ErrVacuumRunning, got %v", err)
	}
}
