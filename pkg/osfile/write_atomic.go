package osfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes a file by streaming content into a temporary file in
// the same directory and renaming it into place. Readers therefore only ever
// observe either the complete old file or the complete new file, never a torn
// write.
//
// perm is applied when the file is created. If the destination file already
// exists, its current permissions are preserved.
func WriteFileAtomic(p string, perm os.FileMode, write func(*os.File) error) (namedErr error) {
	dir := filepath.Dir(p)
	f, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary file: %w", err)
	}
	tmpPath := f.Name()
	defer func() {
		if namedErr != nil {
			os.Remove(tmpPath)
		}
	}()
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flush a temporary file: %w", err)
	}
	mode := perm
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode()
	} else if !os.IsNotExist(err) {
		f.Close()
		return fmt.Errorf("stat the destination file: %w", err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return fmt.Errorf("change the temporary file permission: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close a temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return fmt.Errorf("rename a temporary file: %w", err)
	}
	return nil
}
