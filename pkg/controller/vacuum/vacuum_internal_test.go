package vacuum

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/aquaproj/aqua/v2/pkg/runtime"
	pkgvacuum "github.com/aquaproj/aqua/v2/pkg/vacuum"
)

const testPkgPath = "pkgs/github_release/github.com/cli/cli/v2.65.0/gh_2.65.0_macOS_arm64.zip"

type fakeFinder struct {
	paths []string
}

func (f *fakeFinder) Finds(_, _ string) []string { return f.paths }

type fakeReader struct{}

func (fakeReader) Read(_ *slog.Logger, _ string, _ *aqua.Config) error { return nil }

type fakeRegistryInstaller struct{}

func (fakeRegistryInstaller) InstallRegistries(_ context.Context, _ *slog.Logger, _ *aqua.Config, _ string, _ *checksum.Checksums) (map[string]*registry.Config, error) {
	return map[string]*registry.Config{}, nil
}

func newTestController(t *testing.T) (*Controller, *pkgvacuum.Client, *config.Param, time.Time) {
	t.Helper()
	rootDir := t.TempDir()
	param := &config.Param{RootDir: rootDir, VacuumDays: 60}
	client := pkgvacuum.New(param)
	rt := runtime.New(context.Background())
	ctrl := New(param, rt, client, &fakeFinder{}, fakeReader{}, fakeRegistryInstaller{})
	return ctrl, client, param, time.Now()
}

func createPackage(t *testing.T, client *pkgvacuum.Client, rootDir, rel string, timestamp time.Time) {
	t.Helper()
	src := filepath.Join(rootDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(src, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "binary"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Update(rel, timestamp); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupOne_deletesExpiredPackageInTwoPhases(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	old := now.Add(-100 * 24 * time.Hour)
	createPackage(t, client, param.RootDir, testPkgPath, old)

	checker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{})

	if _, err := os.Stat(client.SrcPath(testPkgPath)); !os.IsNotExist(err) {
		t.Fatal("the package must be deleted")
	}
	if _, err := os.Stat(filepath.Join(param.RootDir, "metadata", "pkgs", filepath.FromSlash(testPkgPath))); !os.IsNotExist(err) {
		t.Fatal("the timestamp must be removed")
	}
	exists, err := client.QuarantineExists(testPkgPath)
	if err != nil || exists {
		t.Fatalf("the quarantine copy must be deleted: %v", err)
	}
	logger := slog.New(slog.DiscardHandler)
	journals, err := client.ListJournals(logger)
	if err != nil || len(journals) != 0 {
		t.Fatalf("the journal must be removed: %v %v", journals, err)
	}
}

func TestCleanupOne_protectedPackageIsKept(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	old := now.Add(-100 * 24 * time.Hour)
	createPackage(t, client, param.RootDir, testPkgPath, old)

	checker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{
		testPkgPath: {},
	})

	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("a protected package must be kept: %v", err)
	}
}

func TestCleanupOne_activeLeaseBlocksDeletion(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	old := now.Add(-100 * 24 * time.Hour)
	createPackage(t, client, param.RootDir, testPkgPath, old)

	lease, err := client.AcquireLease(testPkgPath, true)
	if err != nil {
		t.Fatal(err)
	}

	checker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{})
	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("a package with an active lease must be kept: %v", err)
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{})
	if _, err := os.Stat(client.SrcPath(testPkgPath)); !os.IsNotExist(err) {
		t.Fatal("the package must be deleted after the lease is released")
	}
}

func TestCleanupOne_renewedTimestampIsKept(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	old := now.Add(-100 * 24 * time.Hour)
	createPackage(t, client, param.RootDir, testPkgPath, old)

	// Simulate a lease renewal between the FindAll snapshot and the locked check.
	if err := client.Update(testPkgPath, now); err != nil {
		t.Fatal(err)
	}

	checker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{})
	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("a package with a renewed timestamp must be kept: %v", err)
	}
}

func TestCleanupOne_missingSourcePurgesLedger(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	old := now.Add(-100 * 24 * time.Hour)
	// Only the ledger exists; the package directory was removed manually.
	if err := client.Update(testPkgPath, old); err != nil {
		t.Fatal(err)
	}

	checker := pkgvacuum.NewTimestampChecker(now, param.VacuumDays)
	ctrl.cleanupOne(slog.New(slog.DiscardHandler), testPkgPath, old, checker, now, map[string]struct{}{})
	if _, err := os.Stat(filepath.Join(param.RootDir, "metadata", "pkgs", filepath.FromSlash(testPkgPath))); !os.IsNotExist(err) {
		t.Fatal("the ledger of a missing package must be purged")
	}
}

func TestReconcile_restoresProtectedQuarantinedPackage(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	createPackage(t, client, param.RootDir, testPkgPath, now.Add(-100*24*time.Hour))
	if err := client.MoveToQuarantine(testPkgPath); err != nil {
		t.Fatal(err)
	}
	j := pkgvacuum.NewJournal(testPkgPath, now)
	j.SetState(pkgvacuum.JournalQuarantined, now)
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	ctrl.reconcile(slog.New(slog.DiscardHandler), now, map[string]struct{}{
		testPkgPath: {},
	})

	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("the protected package must be restored: %v", err)
	}
	exists, _ := client.QuarantineExists(testPkgPath)
	if exists {
		t.Fatal("the quarantine copy must disappear after restore")
	}
	journals, _ := client.ListJournals(slog.New(slog.DiscardHandler))
	if _, ok := journals[testPkgPath]; ok {
		t.Fatal("the journal must be removed after restore")
	}
}

func TestReconcile_finishesInterruptedDeletion(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	createPackage(t, client, param.RootDir, testPkgPath, now.Add(-100*24*time.Hour))
	if err := client.MoveToQuarantine(testPkgPath); err != nil {
		t.Fatal(err)
	}
	j := pkgvacuum.NewJournal(testPkgPath, now)
	j.SetState(pkgvacuum.JournalQuarantined, now)
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	ctrl.reconcile(slog.New(slog.DiscardHandler), now, map[string]struct{}{})

	exists, _ := client.QuarantineExists(testPkgPath)
	if exists {
		t.Fatal("the quarantined content must be deleted")
	}
	if _, err := os.Stat(filepath.Join(param.RootDir, "metadata", "pkgs", filepath.FromSlash(testPkgPath))); !os.IsNotExist(err) {
		t.Fatal("the timestamp must be purged")
	}
	journals, _ := client.ListJournals(slog.New(slog.DiscardHandler))
	if len(journals) != 0 {
		t.Fatal("the journal must be removed")
	}
}

func TestReconcile_supersededByReinstall(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	createPackage(t, client, param.RootDir, testPkgPath, now.Add(-100*24*time.Hour))
	if err := client.MoveToQuarantine(testPkgPath); err != nil {
		t.Fatal(err)
	}
	// A concurrent install recreated the version.
	createPackage(t, client, param.RootDir, testPkgPath, now)
	j := pkgvacuum.NewJournal(testPkgPath, now)
	j.SetState(pkgvacuum.JournalQuarantined, now)
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	ctrl.reconcile(slog.New(slog.DiscardHandler), now, map[string]struct{}{})

	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("the reinstalled package must stay: %v", err)
	}
	exists, _ := client.QuarantineExists(testPkgPath)
	if exists {
		t.Fatal("the stale quarantine copy must be discarded")
	}
	journals, _ := client.ListJournals(slog.New(slog.DiscardHandler))
	if len(journals) != 0 {
		t.Fatal("the journal must be removed")
	}
}

func TestReconcile_plannedJournalWithoutQuarantineIsDropped(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	createPackage(t, client, param.RootDir, testPkgPath, now)
	j := pkgvacuum.NewJournal(testPkgPath, now) // still planned
	if err := client.SaveJournal(j); err != nil {
		t.Fatal(err)
	}

	ctrl.reconcile(slog.New(slog.DiscardHandler), now, map[string]struct{}{})

	if _, err := os.Stat(client.SrcPath(testPkgPath)); err != nil {
		t.Fatalf("the package must stay: %v", err)
	}
	journals, _ := client.ListJournals(slog.New(slog.DiscardHandler))
	if len(journals) != 0 {
		t.Fatal("the planned journal must be dropped")
	}
}

func TestVacuum_endToEndKeepsRecentAndRemovesExpired(t *testing.T) {
	t.Parallel()
	ctrl, client, param, now := newTestController(t)
	recentPath := "pkgs/github_release/github.com/cli/cli/v2.80.0/gh_2.80.0_macOS_arm64.zip"
	createPackage(t, client, param.RootDir, testPkgPath, now.Add(-100*24*time.Hour))
	createPackage(t, client, param.RootDir, recentPath, now.Add(-time.Hour))

	if err := ctrl.Vacuum(context.Background(), slog.New(slog.DiscardHandler), param); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(client.SrcPath(testPkgPath)); !os.IsNotExist(err) {
		t.Fatal("the expired package must be removed")
	}
	if _, err := os.Stat(client.SrcPath(recentPath)); err != nil {
		t.Fatalf("the recent package must stay: %v", err)
	}
}

func TestVacuum_failsWhenAnotherVacuumIsRunning(t *testing.T) {
	t.Parallel()
	ctrl, _, param, _ := newTestController(t)
	// Acquire the global vacuum lock with a separate client.
	other := pkgvacuum.New(param)
	h, err := other.LockVacuum()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Release() }()

	err = ctrl.Vacuum(context.Background(), slog.New(slog.DiscardHandler), param)
	if !errors.Is(err, pkgvacuum.ErrVacuumRunning) {
		t.Fatalf("expected ErrVacuumRunning, got %v", err)
	}
}
