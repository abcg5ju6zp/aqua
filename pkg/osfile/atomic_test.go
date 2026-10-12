package osfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
)

func TestWriteFileAtomic_replacesContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := osfile.WriteFileAtomic(path, []byte("new content"), 0o664); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new content" {
		t.Fatalf("content = %q, want %q", string(b), "new content")
	}
}

func TestWriteFileAtomic_noTempLeftover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := osfile.WriteFileAtomic(path, []byte("x"), 0o664); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("only the target file must remain, got %v", entries)
	}
}
