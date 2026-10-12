package lock_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "nested", "lock")
}

func TestAcquire_sharedAndExclusiveAreExclusive(t *testing.T) {
	t.Parallel()
	path := lockPath(t)

	shared, err := lock.Acquire(path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Release() })

	// A second shared lock from the same process is allowed.
	shared2, err := lock.Acquire(path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared2.Release() })

	// An exclusive lock must fail non-blockingly while shared locks are held.
	if _, err := lock.Acquire(path, true, false); !errors.Is(err, lock.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestAcquire_exclusiveBlocksShared(t *testing.T) {
	t.Parallel()
	path := lockPath(t)
	exclusive, err := lock.Acquire(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exclusive.Release() })

	if _, err := lock.Acquire(path, false, false); !errors.Is(err, lock.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestAcquire_releasedLockCanBeAcquired(t *testing.T) {
	t.Parallel()
	path := lockPath(t)
	h, err := lock.Acquire(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	h2, err := lock.Acquire(path, true, false)
	if err != nil {
		t.Fatalf("the released lock must be acquirable: %v", err)
	}
	if err := h2.Release(); err != nil {
		t.Fatal(err)
	}
}
