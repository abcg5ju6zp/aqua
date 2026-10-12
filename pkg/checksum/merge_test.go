package checksum_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/google/go-cmp/cmp"
)

const testFileMode os.FileMode = 0o600

// commitChecksumFile writes through Checksums.UpdateFile, like another aqua
// process that only knows the entries it is given.
func commitChecksumFile(t *testing.T, p string, entries ...*checksum.Checksum) {
	t.Helper()
	c := checksum.New()
	for _, e := range entries {
		c.Set(e.ID, e)
	}
	if err := c.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
}

// rawWriteChecksumFile writes the file directly, bypassing the merge logic.
// It stands in for an editor, a legacy aqua version, or any on-disk state a
// running process hasn't observed.
func rawWriteChecksumFile(t *testing.T, p string, entries ...*checksum.Checksum) {
	t.Helper()
	type checksumsJSON struct {
		Checksums []*checksum.Checksum `json:"checksums"`
	}
	b, err := json.MarshalIndent(&checksumsJSON{Checksums: entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, '\n'), testFileMode); err != nil {
		t.Fatal(err)
	}
}

func entry(id, algo, sum string) *checksum.Checksum {
	return &checksum.Checksum{ID: id, Algorithm: algo, Checksum: sum}
}

func mustContainIDs(t *testing.T, path string, ids ...string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(string(b), id) {
			t.Fatalf("entry %s was lost from %s:\n%s", id, path, string(b))
		}
	}
}

func mustNotContainIDs(t *testing.T, path string, ids ...string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if strings.Contains(string(b), id) {
			t.Fatalf("entry %s must not be in %s:\n%s", id, path, string(b))
		}
	}
}

// A process that adds an entry keeps entries another process added after the
// file was opened.
func TestUpdateFile_MergesConcurrentAdditions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	commitChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"))

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	local.Set("id-b", entry("id-b", algoSHA256, "bbb"))

	// Another process commits id-c after this process read the file.
	rawWriteChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"), entry("id-c", algoSHA256, "ccc"))

	if err := local.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
	mustContainIDs(t, p, "id-a", "id-b", "id-c")
}

// A confirmed entry disagreeing with the on-disk entry must stop the commit
// and leave the file untouched and parseable.
func TestUpdateFile_ConflictStopsCommit(t *testing.T) {
	t.Parallel()
	data := []struct {
		name     string
		local    *checksum.Checksum
		onDisk   *checksum.Checksum
		conflict bool
	}{
		{
			name:     "different checksum",
			local:    entry("id-b", algoSHA256, "bbb"),
			onDisk:   entry("id-b", algoSHA256, "ddd"),
			conflict: true,
		},
		{
			name:     "different algorithm",
			local:    entry("id-b", algoSHA256, "bbb"),
			onDisk:   entry("id-b", algoSHA512, "bbb"),
			conflict: true,
		},
		{
			name:   "same value is not a conflict",
			local:  entry("id-b", algoSHA256, "bbb"),
			onDisk: entry("id-b", algoSHA256, "bbb"),
		},
		{
			name:   "hex case difference is not a conflict",
			local:  entry("id-b", algoSHA256, "ABC"),
			onDisk: entry("id-b", algoSHA256, "abc"),
		},
	}
	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			runConflictCase(t, d.local, d.onDisk, d.conflict)
		})
	}
}

func runConflictCase(t *testing.T, localEntry, diskEntry *checksum.Checksum, wantConflict bool) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	rawWriteChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"))

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	local.Set(localEntry.ID, localEntry)

	// Another process wins the race for the ID with the on-disk value.
	rawWriteChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"), diskEntry)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	err = local.UpdateFile(p)
	if !wantConflict {
		if err != nil {
			t.Fatalf("the commit must succeed: %v", err)
		}
		return
	}
	assertCommitConflict(t, err, p, before, localEntry.ID)
}

func assertCommitConflict(t *testing.T, err error, p string, before []byte, id string) {
	t.Helper()
	if err == nil {
		t.Fatal("a conflict error must be returned")
	}
	if !errors.Is(err, checksum.ErrChecksumConflict) {
		t.Fatalf("error must classify as ErrChecksumConflict: %v", err)
	}
	var conflictErr *checksum.ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.ID != id {
		t.Fatalf("expected a conflict for %s, got %v", id, err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Fatalf("the file must stay untouched on conflict:\n%s", diff)
	}
	if readErr := checksum.New().ReadFile(p); readErr != nil {
		t.Fatalf("the file must stay parseable: %v", readErr)
	}
}

// Pruning must not delete an entry another process adds while the command runs.
func TestUpdateFile_PruneKeepsConcurrentAdditions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	rawWriteChecksumFile(t, p,
		entry("keep", algoSHA256, "aaa"),
		entry("drop", algoSHA256, "bbb"),
	)

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	if local.Get("keep") == nil {
		t.Fatal("keep must be observed")
	}
	local.Set("added", entry("added", algoSHA256, "ccc"))
	local.Prune()

	// Another process adds an entry this process never observed.
	rawWriteChecksumFile(t, p,
		entry("keep", algoSHA256, "aaa"),
		entry("drop", algoSHA256, "bbb"),
		entry("remote", algoSHA256, "ddd"),
	)

	if err := local.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
	mustContainIDs(t, p, "keep", "added", "remote")
	mustNotContainIDs(t, p, "drop")
}

// An unreferenced entry that another process changes after our snapshot is
// outside our prune confirmation and must survive with the new value.
func TestUpdateFile_PruneKeepsConcurrentlyModifiedEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	rawWriteChecksumFile(t, p,
		entry("keep", algoSHA256, "aaa"),
		entry("changed", algoSHA256, "old"),
	)

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	local.Get("keep")
	local.Prune()

	// Another process re-confirms the unreferenced entry with a new value.
	rawWriteChecksumFile(t, p,
		entry("keep", algoSHA256, "aaa"),
		entry("changed", algoSHA256, "new"),
	)

	if err := local.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"new"`) {
		t.Fatalf("the concurrently changed entry must survive with the new value: %s", string(b))
	}
}

// When the configuration file changes while a prune command runs, the
// additions are still committed but deletions are deferred to a retry.
func TestOpen_PruneDeferredWhenConfigChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, fileAquaYaml)
	checksumPath := filepath.Join(dir, fileAquaChecksums)
	if err := os.WriteFile(cfgPath, []byte("packages: []\n"), testFileMode); err != nil {
		t.Fatal(err)
	}
	rawWriteChecksumFile(t, checksumPath,
		entry("keep", algoSHA256, "aaa"),
		entry("drop", algoSHA256, "bbb"),
	)

	logger := slog.New(slog.DiscardHandler)
	checksums, closeChecksums, err := checksum.Open(logger, cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	checksums.Get("keep")
	checksums.Set("added", entry("added", algoSHA256, "ccc"))
	checksums.Prune()

	// The configuration file is edited while the command runs.
	if err := os.WriteFile(cfgPath, []byte("packages: []\n# changed\n"), testFileMode); err != nil {
		t.Fatal(err)
	}

	err = closeChecksums()
	if !errors.Is(err, checksum.ErrPruneDeferred) {
		t.Fatalf("expected ErrPruneDeferred, got %v", err)
	}
	mustContainIDs(t, checksumPath, "keep", "added", "drop")
	if err := checksum.New().ReadFile(checksumPath); err != nil {
		t.Fatalf("the file must stay parseable: %v", err)
	}

	// A second prune run against the now-current configuration completes.
	assertPruneRetrySucceeds(t, logger, cfgPath, checksumPath)
}

func assertPruneRetrySucceeds(t *testing.T, logger *slog.Logger, cfgPath, checksumPath string) {
	t.Helper()
	checksums, closeChecksums, err := checksum.Open(logger, cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	checksums.Get("keep")
	checksums.Get("added")
	checksums.Prune()
	if err := closeChecksums(); err != nil {
		t.Fatal(err)
	}
	mustNotContainIDs(t, checksumPath, "drop")
}

// When both generations exist, both are merged into the active non-hidden
// file and the shadowed hidden file is retired.
func TestUpdateFile_CollapseMergesAndRetiresHidden(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hidden := filepath.Join(dir, fileDotAquaChecksums)
	active := filepath.Join(dir, fileAquaChecksums)
	rawWriteChecksumFile(t, hidden, entry("from-hidden", algoSHA256, "aaa"))
	rawWriteChecksumFile(t, active, entry("from-visible", algoSHA256, "bbb"))

	local := checksum.New()
	if err := local.ReadFile(active); err != nil {
		t.Fatal(err)
	}
	local.Set("added", entry("added", algoSHA256, "ccc"))
	if err := local.UpdateFile(active); err != nil {
		t.Fatal(err)
	}

	mustContainIDs(t, active, "from-hidden", "from-visible", "added")
	if _, err := os.Stat(hidden); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the shadowed hidden file must be retired: %v", err)
	}
}

// Same ID with different values across the two generations is a conflict;
// neither file is touched until it is resolved.
func TestUpdateFile_CollapseConflictReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hidden := filepath.Join(dir, fileDotAquaChecksums)
	active := filepath.Join(dir, fileAquaChecksums)
	rawWriteChecksumFile(t, hidden, entry("id-a", algoSHA256, "aaa"))
	rawWriteChecksumFile(t, active, entry("id-a", algoSHA256, "bbb"))

	local := checksum.New()
	if err := local.ReadFile(active); err != nil {
		t.Fatal(err)
	}
	local.Set("id-b", entry("id-b", algoSHA256, "ccc"))
	if err := local.UpdateFile(active); !errors.Is(err, checksum.ErrChecksumConflict) {
		t.Fatalf("expected a conflict error, got %v", err)
	}
	if _, err := os.Stat(hidden); err != nil {
		t.Fatal("the hidden file must not be removed while the conflict persists")
	}
}

// A hidden-only configuration keeps writing the hidden generation.
func TestUpdateFile_HiddenOnlyStaysHidden(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hidden := filepath.Join(dir, fileDotAquaChecksums)
	rawWriteChecksumFile(t, hidden, entry("id-a", algoSHA256, "aaa"))

	local := checksum.New()
	if err := local.ReadFile(hidden); err != nil {
		t.Fatal(err)
	}
	local.Set("id-b", entry("id-b", algoSHA256, "bbb"))
	if err := local.UpdateFile(hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileAquaChecksums)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the non-hidden file must not be created for a hidden-only config")
	}
	mustContainIDs(t, hidden, "id-a", "id-b")
}

// A stale temporary file left by a killed writer is removed and the active
// file is always complete JSON.
func TestUpdateFile_ReplacesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	commitChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"))

	stale := filepath.Join(dir, "."+fileAquaChecksums+".tmp")
	if err := os.WriteFile(stale, []byte("{broken"), testFileMode); err != nil {
		t.Fatal(err)
	}

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	local.Set("id-b", entry("id-b", algoSHA256, "bbb"))
	if err := local.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale temporary file must be removed: %v", err)
	}
	if err := checksum.New().ReadFile(p); err != nil {
		t.Fatalf("the active file must be complete JSON: %v", err)
	}
}

// Many processes committing at the same time must leave every confirmed entry
// in the file: the lock plus the merge prevents last-writer-wins loss.
func TestUpdateFile_ConcurrentWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	commitChecksumFile(t, p, entry("shared", algoSHA256, "same"))

	const processes = 16
	const perProcess = 8

	var wg sync.WaitGroup
	errCh := make(chan error, processes)
	start := make(chan struct{})
	for i := range processes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- runConcurrentWriter(p, i, perProcess, start)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for i := range processes {
		for j := range perProcess {
			id := fmt.Sprintf("p%d-%d", i, j)
			if !strings.Contains(string(b), id) {
				missing = append(missing, id)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("%d entries were lost, e.g. %v", len(missing), missing[:min(5, len(missing))])
	}
}

func runConcurrentWriter(p string, process, perProcess int, start <-chan struct{}) error {
	local := checksum.New()
	// Each process reads the file independently, like the CLI does.
	if err := local.ReadFile(p); err != nil {
		return err //nolint:wrapcheck
	}
	local.Set("shared", entry("shared", algoSHA256, "same"))
	for j := range perProcess {
		id := fmt.Sprintf("p%d-%d", process, j)
		local.Set(id, entry(id, algoSHA256, fmt.Sprintf("%x", process*perProcess+j)))
	}
	<-start
	return local.UpdateFile(p) //nolint:wrapcheck
}

// Read-only commands that don't confirm anything must not touch the file and
// must not create a lock file.
func TestUpdateFile_NoChangeIsNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, fileAquaChecksums)
	commitChecksumFile(t, p, entry("id-a", algoSHA256, "aaa"))
	// The lock file is a write artifact; the no-op phase below starts without it.
	lockPath := filepath.Join(dir, ".aqua-checksums.json.lock")
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	local := checksum.New()
	if err := local.ReadFile(p); err != nil {
		t.Fatal(err)
	}
	local.Get("id-a")
	if err := local.UpdateFile(p); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("the file must not be rewritten when nothing changed")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no lock file must be created when nothing is committed")
	}
}

func TestOpen_Disabled(t *testing.T) {
	t.Parallel()
	checksums, closeChecksums, err := checksum.Open(slog.New(slog.DiscardHandler), "/nonexistent/aqua.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if checksums != nil {
		t.Fatal("checksums must be nil when checksum validation is disabled")
	}
	if err := closeChecksums(); err != nil {
		t.Fatal(err)
	}
}
