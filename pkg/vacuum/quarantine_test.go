package vacuum_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/vacuum"
)

func createTrackedPackage(t *testing.T, rootDir, pkgPath, content string) {
	t.Helper()
	src := filepath.Join(rootDir, filepath.FromSlash(pkgPath))
	if err := os.MkdirAll(src, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "binary"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	client := vacuum.New(&config.Param{RootDir: rootDir})
	if err := client.Create(pkgPath, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestMoveToQuarantine_andRestore(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	createTrackedPackage(t, rootDir, leasePkgPath, "v1")

	if err := client.MoveToQuarantine(leasePkgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(client.SrcPath(leasePkgPath)); !os.IsNotExist(err) {
		t.Fatal("the source must disappear after quarantine")
	}
	exists, err := client.QuarantineExists(leasePkgPath)
	if err != nil || !exists {
		t.Fatalf("the quarantined content must exist: %v", err)
	}

	if err := client.RestoreFromQuarantine(leasePkgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(client.SrcPath(leasePkgPath)); err != nil {
		t.Fatalf("the source must be restored: %v", err)
	}
	exists, err = client.QuarantineExists(leasePkgPath)
	if err != nil || exists {
		t.Fatalf("the quarantine copy must disappear: %v", err)
	}
}

func TestListQuarantine_mirrorsRelativePaths(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	createTrackedPackage(t, rootDir, leasePkgPath, "v1")
	if err := client.MoveToQuarantine(leasePkgPath); err != nil {
		t.Fatal(err)
	}

	entries, err := client.ListQuarantine()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.PkgPath == leasePkgPath {
			found = true
			if !e.IsDir {
				t.Fatal("the package leaf must be a directory")
			}
		}
	}
	if !found {
		t.Fatalf("the quarantined package must be listed: %v", entries)
	}
}

func TestPurge_removesAllTrackingArtifacts(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	logger := slog.New(slog.DiscardHandler)
	createTrackedPackage(t, rootDir, leasePkgPath, "v1")

	lease, err := client.AcquireLease(leasePkgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	j := vacuum.NewJournal(leasePkgPath, time.Now())
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}
	if err := client.MoveToQuarantine(leasePkgPath); err != nil {
		t.Fatal(err)
	}

	if err := client.Purge(leasePkgPath); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(rootDir, "metadata", "pkgs", filepath.FromSlash(leasePkgPath)),
		filepath.Join(rootDir, "metadata", "leases", filepath.FromSlash(leasePkgPath)),
		filepath.Join(rootDir, "quarantine", filepath.FromSlash(leasePkgPath)),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed", p)
		}
	}
	if journals, err := client.ListJournals(logger); err != nil || len(journals) != 0 {
		t.Fatalf("the journal must be removed: %v %v", journals, err)
	}
}

func TestPurgePath_removesDescendantsByGlob(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	logger := slog.New(slog.DiscardHandler)

	other := "pkgs/github_release/github.com/cli/cli/v2.60.0/gh_2.60.0_macOS_arm64.zip"
	createTrackedPackage(t, rootDir, leasePkgPath, "v2.65")
	createTrackedPackage(t, rootDir, other, "v2.60")
	j := vacuum.NewJournal(leasePkgPath, time.Now())
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	pattern := "github_release/github.com/cli/cli"
	if err := client.PurgePath(logger, pattern); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "metadata", "pkgs", filepath.FromSlash(leasePkgPath))); !os.IsNotExist(err) {
		t.Fatal("matching metadata must be removed")
	}
	if _, err := os.Stat(filepath.Join(rootDir, "metadata", "pkgs", filepath.FromSlash(other))); !os.IsNotExist(err) {
		t.Fatal("the sibling version must also match the pattern")
	}
	if journals, err := client.ListJournals(logger); err != nil || len(journals) != 0 {
		t.Fatalf("descendant journals must be removed: %v", journals)
	}
}

func TestReferencedByShim(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	client := vacuum.New(&config.Param{RootDir: rootDir})
	createTrackedPackage(t, rootDir, leasePkgPath, "v1")
	binDir := filepath.Join(rootDir, "bin")
	if err := os.MkdirAll(binDir, 0o775); err != nil {
		t.Fatal(err)
	}
	// A normal version-independent proxy shim.
	if err := os.Symlink(filepath.Join("..", "aqua-proxy"), filepath.Join(binDir, "gh")); err != nil {
		t.Fatal(err)
	}
	referenced, err := client.ReferencedByShim(client.SrcPath(leasePkgPath))
	if err != nil {
		t.Fatal(err)
	}
	if referenced {
		t.Fatal("a proxy shim must not reference a specific package version")
	}

	// A non-standard link directly into the version directory.
	if err := os.Symlink(filepath.Join("..", filepath.FromSlash(leasePkgPath), "binary"), filepath.Join(binDir, "direct")); err != nil {
		t.Fatal(err)
	}
	// A new client scans the bin directory again instead of using the cached
	// first scan.
	client2 := vacuum.New(&config.Param{RootDir: rootDir})
	referenced, err = client2.ReferencedByShim(client2.SrcPath(leasePkgPath))
	if err != nil {
		t.Fatal(err)
	}
	if !referenced {
		t.Fatal("a link into the package directory must be treated as a reference")
	}
}
