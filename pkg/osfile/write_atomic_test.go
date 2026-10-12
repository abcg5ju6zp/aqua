package osfile_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
)

func TestWriteFileAtomicCreatesNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := osfile.WriteFileAtomic(p, 0o600, func(f *os.File) error {
		if _, err := f.WriteString("hello"); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected file mode: %v", err)
	}
}

func TestWriteFileAtomicReplacesAndPreservesPermission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := os.WriteFile(p, []byte("old"), 0o640); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	if err := osfile.WriteFileAtomic(p, 0o600, func(f *os.File) error {
		if _, err := f.WriteString("new"); err != nil {
			return fmt.Errorf("write: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("got %q", b)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the existing permission must be preserved, got %o", info.Mode().Perm())
	}
}

func TestWriteFileAtomicWriteErrorRemovesTemporaryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	writeErr := errors.New("write failed")
	if err := osfile.WriteFileAtomic(p, 0o600, func(*os.File) error {
		return writeErr
	}); !errors.Is(err, writeErr) {
		t.Fatalf("expected the write error, got %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the destination must not be created")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("no temporary file must be left, found %s", e.Name())
	}
}
