package osfile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes b to path atomically: the data is first written and
// fsynced into a temporary file in the same directory, then renamed over path.
// The temporary file lives in the same directory so that the rename doesn't
// cross filesystems. Readers of path therefore observe either the previous
// content or the new content, never a partially written file.
//
// The destination directory must exist. A crash after the rename can still
// leave the rename or the temporary file unpersisted; callers that need crash
// recovery must reconcile via an auxiliary journal rather than relying on this
// function.
func WriteFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	name, err := tempFileName(dir)
	if err != nil {
		return err
	}
	// O_EXCL: a colliding name, however unlikely, must be an error instead of
	// truncating a file another process is writing.
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create a temporary file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("write a temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync a temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close a temporary file: %w", err)
	}
	if err := os.Chmod(name, perm); err != nil {
		return fmt.Errorf("chmod a temporary file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename a temporary file: %w", err)
	}
	cleanup = false
	syncDir(dir)
	return nil
}

func tempFileName(dir string) (string, error) {
	b := make([]byte, 16) //nolint:mnd
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a temporary file name: %w", err)
	}
	return filepath.Join(dir, "."+hex.EncodeToString(b)+".tmp"), nil
}

// syncDir best-effort fsyncs a directory so that a rename is persisted.
// Directories can't be opened on Windows; failures there and on filesystems
// that don't support fsync are ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
