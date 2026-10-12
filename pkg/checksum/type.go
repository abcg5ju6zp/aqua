package checksum

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
)

const (
	checksumFileName    = "aqua-checksums.json"
	dotChecksumFileName = ".aqua-checksums.json"
	lockFileName        = ".aqua-checksums.json.lock"
	checksumFileMode    fs.FileMode = 0o644
	lockFileMode        os.FileMode = 0o600
)

// Checksums manages a collection of checksum data with concurrent access
// support.
//
// Multiple aqua processes can run in one configuration directory. Each
// process starts from the file content it observed when it opened the file
// (base), records the entries it actually reads (observed) and the entries it
// confirms itself (added), and merges that view with the latest file content
// under a cross-process lock when it commits. Entries another process added
// are therefore kept, and an entry this process confirmed but another process
// persisted with a different checksum or algorithm is reported as a conflict
// instead of being silently overwritten.
type Checksums struct {
	// base is the file content this process observed when it opened the file.
	base map[string]*Checksum
	// m is the in-memory working view: base plus entries added in this process.
	m map[string]*Checksum
	// observed holds IDs this process read from m (Get) or added (Set).
	observed map[string]struct{}
	// added holds entries this process produced or confirmed itself.
	added map[string]*Checksum

	doPrune bool

	changed bool
	stdout  io.Writer
	logger  *slog.Logger

	// cfgPath and cfgDigest fingerprint the configuration file the checksum
	// file belongs to. Pruning deletes only entries that were unreferenced in
	// that configuration snapshot, and deletions are skipped if the
	// configuration file changed while the command was running.
	cfgPath   string
	cfgDigest []byte

	rwmutex *sync.RWMutex
}

// New creates and initializes a new Checksums instance.
func New() *Checksums {
	return &Checksums{
		base:     map[string]*Checksum{},
		m:        map[string]*Checksum{},
		observed: map[string]struct{}{},
		added:    map[string]*Checksum{},
		rwmutex:  &sync.RWMutex{},
	}
}

// Open loads checksums from a file and returns a close function.
// If checksum validation is disabled, it returns nil and a no-op close
// function. The close function merges this process's confirmed entries with
// the latest file content and writes the result atomically; it returns an
// error instead of overwriting the file when a checksum conflict is found.
func Open(logger *slog.Logger, cfgFilePath string, enabled bool) (*Checksums, func() error, error) {
	if !enabled {
		return nil, func() error {
			return nil
		}, nil
	}
	checksumFilePath, err := GetChecksumFilePathFromConfigFilePath(cfgFilePath)
	if err != nil {
		return nil, nil, err
	}
	checksums := New()
	checksums.logger = logger
	checksums.cfgPath = cfgFilePath
	checksums.cfgDigest = readConfigDigest(cfgFilePath)
	if err := checksums.ReadFile(checksumFilePath); err != nil {
		return nil, nil, fmt.Errorf("read a checksum JSON: %w", err)
	}
	return checksums, func() error {
		return checksums.UpdateFile(checksumFilePath)
	}, nil
}

// EnableOutput enables stdout output for checksum file updates.
func (c *Checksums) EnableOutput() {
	c.stdout = os.Stdout
}

// Get retrieves a checksum by key and marks it as accessed.
// Accessing an entry records that this command observed it, so pruning never
// removes it as an unreferenced leftover of this configuration snapshot.
func (c *Checksums) Get(key string) *Checksum {
	c.rwmutex.Lock()
	chk := c.m[key]
	if chk != nil {
		c.observed[key] = struct{}{}
	}
	c.rwmutex.Unlock()
	return chk
}

// Set stores a checksum with the given key, normalizing the checksum to
// uppercase. An entry new to this process is a confirmed addition that is
// merged with the file at commit time. Re-setting an existing entry doesn't
// replace its value.
func (c *Checksums) Set(key string, chk *Checksum) {
	chk.Checksum = strings.ToUpper(chk.Checksum)
	c.rwmutex.Lock()
	if _, ok := c.m[key]; !ok {
		c.m[key] = chk
		c.added[key] = chk
		c.changed = true
	}
	c.observed[key] = struct{}{}
	c.rwmutex.Unlock()
}

// Prune marks that entries present when the file was opened but not observed
// (Get) or added (Set) by this command should be deleted at commit time.
// Deletion is committed per entry against the latest file content: an entry
// another process added or changed is never deleted.
func (c *Checksums) Prune() {
	c.rwmutex.Lock()
	c.doPrune = true
	for id := range c.base {
		if _, ok := c.observed[id]; !ok {
			c.changed = true
			break
		}
	}
	c.rwmutex.Unlock()
}

// checksumsJSON represents the JSON structure for serializing checksums to file.
type checksumsJSON struct {
	Checksums []*Checksum `json:"checksums"`
}

// Checksum represents a single checksum entry with its identifier, hash value, and algorithm.
type Checksum struct {
	ID        string `json:"id"`
	Checksum  string `json:"checksum"`
	Algorithm string `json:"algorithm"`
}

// UnmarshalJSON implements json.Unmarshaler to load checksums from JSON data.
// The loaded content becomes both the base snapshot and the working view;
// nothing is marked as observed or added yet.
func (c *Checksums) UnmarshalJSON(b []byte) error {
	chkJSON := &checksumsJSON{}
	if err := json.Unmarshal(b, chkJSON); err != nil {
		return fmt.Errorf("parse a checksum file as JSON: %w", err)
	}
	m := make(map[string]*Checksum, len(chkJSON.Checksums))
	for _, chk := range chkJSON.Checksums {
		m[chk.ID] = chk
	}
	c.rwmutex.Lock()
	c.base = m
	c.m = maps.Clone(m)
	c.observed = map[string]struct{}{}
	c.added = map[string]*Checksum{}
	c.doPrune = false
	c.changed = false
	c.rwmutex.Unlock()
	return nil
}

// ReadFile loads checksums from a JSON file.
// If the file doesn't exist, it returns without error (empty state).
func (c *Checksums) ReadFile(p string) error {
	if f, err := osfile.Exists(p); err != nil {
		return err //nolint:wrapcheck
	} else if !f {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("open a checksum file: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(c); err != nil {
		return fmt.Errorf("parse a checksum file as JSON: %w", err)
	}
	return nil
}

// committedView is an immutable copy of the process-local state used while
// committing under the file lock.
type committedView struct {
	base     map[string]*Checksum
	added    map[string]*Checksum
	observed map[string]struct{}
	prune    bool
}

// UpdateFile merges this process's view with the latest on-disk generations
// and writes the result atomically. It is a no-op when the command neither
// added nor pruned anything.
//
// The merge is performed while holding an exclusive cross-process lock:
//   - entries another process added are kept;
//   - an entry this process confirmed that disagrees with the on-disk entry
//     (checksum or algorithm) makes the commit fail with a ConflictError and
//     leaves the file untouched;
//   - pruning removes only confirmed-unreferenced entries that are unchanged
//     on disk, and is deferred when the configuration file changed during
//     the command;
//   - if both hidden and non-hidden files exist, their contents are merged
//     into the active file and the shadowed file is retired.
func (c *Checksums) UpdateFile(anchoredPath string) error {
	c.rwmutex.RLock()
	changed := c.changed
	c.rwmutex.RUnlock()
	if !changed {
		return nil
	}
	view := c.view()

	release, err := acquireLock(filepath.Dir(anchoredPath))
	if err != nil {
		return err
	}
	defer release()

	targetPath, otherPath, err := resolveActiveFile(filepath.Dir(anchoredPath), anchoredPath)
	if err != nil {
		return err
	}
	latest, err := readGenerations(targetPath, otherPath)
	if err != nil {
		return err
	}

	merged := maps.Clone(latest)
	if err := mergeConfirmedEntries(merged, latest, view.added); err != nil {
		return err
	}

	pruneDeferred := false
	if view.prune {
		pruneDeferred = applyPrune(merged, view, c)
	}

	if err := atomicWriteChecksums(targetPath, merged, c.stdout); err != nil {
		return err
	}

	retireGeneration(otherPath, c.log())

	if pruneDeferred {
		return ErrPruneDeferred
	}
	return nil
}

func (c *Checksums) view() committedView {
	c.rwmutex.RLock()
	defer c.rwmutex.RUnlock()
	view := committedView{
		base:     make(map[string]*Checksum, len(c.base)),
		added:    make(map[string]*Checksum, len(c.added)),
		observed: make(map[string]struct{}, len(c.observed)),
		prune:    c.doPrune,
	}
	maps.Copy(view.base, c.base)
	for id, chk := range c.added {
		view.added[id] = normalizeChecksum(chk)
	}
	maps.Copy(view.observed, c.observed)
	return view
}

// mergeConfirmedEntries adds entries this process confirmed. An entry with
// the same ID but a different checksum or algorithm on disk is a conflict:
// the caller must stop without writing anything.
func mergeConfirmedEntries(merged, latest, added map[string]*Checksum) error {
	for id, local := range added {
		if existing, ok := latest[id]; ok && !sameChecksum(existing, local) {
			return &ConflictError{
				ID:       id,
				Existing: existing,
				Local:    local,
			}
		}
		merged[id] = local
	}
	return nil
}

// applyPrune deletes confirmed-unreferenced base entries from merged. It
// returns true (deleting nothing) when the configuration file changed while
// the command was running; concurrently modified entries are left for a
// later prune run.
func applyPrune(merged map[string]*Checksum, view committedView, c *Checksums) bool {
	if c.configChanged() {
		return true
	}
	for id, base := range view.base {
		if _, observed := view.observed[id]; observed {
			continue
		}
		current, ok := merged[id]
		if !ok || !sameChecksum(current, base) {
			continue
		}
		delete(merged, id)
	}
	return false
}

// retireGeneration removes the shadowed checksum file after its entries were
// merged into the active file. A failure only means the next command repeats
// the collapse, so it is logged rather than failing the successful commit.
func retireGeneration(path string, logger *slog.Logger) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("remove a shadowed checksum file", "error", err, "path", path)
	}
}

func (c *Checksums) log() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return slog.Default()
}

// ConflictError reports that an entry this process confirmed disagrees with
// the entry another process persisted for the same ID. The checksum file is
// not modified; the conflict must be resolved before the command can commit.
type ConflictError struct {
	ID       string
	Existing *Checksum
	Local    *Checksum
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf(
		"a checksum conflict was detected for %q: the checksum file has %s:%s, but this command confirmed %s:%s; resolve the conflict in the checksum file and run the command again",
		e.ID,
		e.Existing.Algorithm, strings.ToUpper(e.Existing.Checksum),
		e.Local.Algorithm, strings.ToUpper(e.Local.Checksum),
	)
}

// Unwrap lets errors.Is(err, ErrChecksumConflict) classify conflict errors.
func (e *ConflictError) Unwrap() error {
	return ErrChecksumConflict
}

func sameChecksum(a, b *Checksum) bool {
	return strings.EqualFold(a.Checksum, b.Checksum) && a.Algorithm == b.Algorithm
}

func normalizeChecksum(chk *Checksum) *Checksum {
	return &Checksum{
		ID:        chk.ID,
		Checksum:  strings.ToUpper(chk.Checksum),
		Algorithm: chk.Algorithm,
	}
}

// resolveActiveFile selects the active generation using the existing rule
// (the non-hidden file wins over the hidden one) and reports the other
// existing generation so the two can be collapsed into one. When neither
// generation exists, the file identity fixed at open time is reused, so a
// process that opened a hidden-only configuration never creates a second,
// non-hidden generation.
func resolveActiveFile(dir, anchoredPath string) (target, other string, err error) {
	p1 := filepath.Join(dir, checksumFileName)
	p2 := filepath.Join(dir, dotChecksumFileName)
	f1, err := osfile.Exists(p1)
	if err != nil {
		return "", "", err //nolint:wrapcheck
	}
	f2, err := osfile.Exists(p2)
	if err != nil {
		return "", "", err //nolint:wrapcheck
	}
	switch {
	case f1:
		if f2 {
			return p1, p2, nil
		}
		return p1, "", nil
	case f2:
		return p2, "", nil
	default:
		if filepath.Base(anchoredPath) == dotChecksumFileName {
			return p2, "", nil
		}
		return p1, "", nil
	}
}

// readGenerations reads and merges the entries of the active file and, when
// it exists, the shadowed generation. The shadowed generation is legacy
// state: an unreadable one is ignored rather than failing the active file.
// Same ID with different values across the two generations is a conflict.
func readGenerations(paths ...string) (map[string]*Checksum, error) {
	m := map[string]*Checksum{}
	for i, p := range paths {
		if p == "" {
			continue
		}
		exists, err := osfile.Exists(p)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}
		if !exists {
			continue
		}
		generation, err := decodeChecksumFile(p)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			slog.Default().Warn("ignore an unreadable shadowed checksum file", "error", err, "path", p)
			continue
		}
		for id, chk := range generation {
			if existing, ok := m[id]; ok && !sameChecksum(existing, chk) {
				return nil, fmt.Errorf("the hidden and non-hidden checksum files disagree on %q: %w", id, &ConflictError{
					ID:       id,
					Existing: existing,
					Local:    chk,
				})
			}
			m[id] = chk
		}
	}
	return m, nil
}

func decodeChecksumFile(p string) (map[string]*Checksum, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open a checksum file: %w", err)
	}
	defer f.Close()
	chkJSON := &checksumsJSON{}
	if err := json.NewDecoder(f).Decode(chkJSON); err != nil {
		return nil, fmt.Errorf("parse a checksum file as JSON: %w", err)
	}
	m := make(map[string]*Checksum, len(chkJSON.Checksums))
	for _, chk := range chkJSON.Checksums {
		m[chk.ID] = chk
	}
	return m, nil
}

// atomicWriteChecksums marshals the checksums and atomically replaces the
// target file. A crash or an interrupted write leaves the previous complete
// file in place and only a stale temporary file behind, so the active file is
// always parseable. The stale temporary is removed by the next writer, which
// holds the same lock.
func atomicWriteChecksums(path string, m map[string]*Checksum, stdout io.Writer) error {
	b, err := marshalChecksums(m)
	if err != nil {
		return err
	}
	if err := replaceFile(path, b); err != nil {
		return err
	}
	if stdout != nil {
		fmt.Fprintln(stdout, path)
	}
	return nil
}

func marshalChecksums(m map[string]*Checksum) ([]byte, error) {
	arr := make([]*Checksum, 0, len(m))
	for _, chk := range m {
		arr = append(arr, chk)
	}
	sort.Slice(arr, func(i, j int) bool {
		return arr[i].ID < arr[j].ID
	})
	b, err := json.MarshalIndent(&checksumsJSON{Checksums: arr}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal checksums as JSON: %w", err)
	}
	return append(b, '\n'), nil
}

// replaceFile writes b to a temporary file in the same directory, fsyncs it,
// and renames it over path. The temporary file has a fixed, hidden name:
// writers are serialized by the lock, so its presence means a crashed
// previous writer and it is safe to remove before writing.
func replaceFile(path string, b []byte) error {
	mode := checksumFileMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	tmpPath := temporaryFilePath(path)
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove a stale temporary checksum file: %w", err)
	}
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create a temporary checksum file: %w", err)
	}
	if err := writeAndSync(f, b, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replace a checksum file: %w", err)
	}
	return nil
}

func writeAndSync(f *os.File, b []byte, tmpPath string) error {
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write a checksum file as JSON: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("flush a temporary checksum file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close a temporary checksum file: %w", err)
	}
	return nil
}

func temporaryFilePath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
}

func readConfigDigest(cfgFilePath string) []byte {
	if cfgFilePath == "" {
		return nil
	}
	b, err := os.ReadFile(cfgFilePath)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

func (c *Checksums) configChanged() bool {
	if c.cfgPath == "" || len(c.cfgDigest) == 0 {
		return false
	}
	b, err := os.ReadFile(c.cfgPath)
	if err != nil {
		return true
	}
	sum := sha256.Sum256(b)
	return !bytes.Equal(sum[:], c.cfgDigest)
}

// GetChecksumFilePathFromConfigFilePath determines the checksum file path from a config file path.
// It checks for both "aqua-checksums.json" and ".aqua-checksums.json" in the config directory,
// preferring the non-hidden version. If neither exists, it returns the non-hidden path for creation.
func GetChecksumFilePathFromConfigFilePath(cfgFilePath string) (string, error) {
	// A checksum file that exists but can't be read must not be silently
	// replaced with a newly generated one, so a stat error other than "not
	// found" is returned rather than treated as absent.
	p1 := filepath.Join(filepath.Dir(cfgFilePath), checksumFileName)
	if f, err := osfile.Exists(p1); err != nil {
		return "", err //nolint:wrapcheck
	} else if f {
		return p1, nil
	}

	p2 := filepath.Join(filepath.Dir(cfgFilePath), dotChecksumFileName)
	if f, err := osfile.Exists(p2); err != nil {
		return "", err //nolint:wrapcheck
	} else if f {
		return p2, nil
	}

	return p1, nil
}
