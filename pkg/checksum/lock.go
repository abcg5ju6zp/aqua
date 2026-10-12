package checksum

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// acquireLock takes an exclusive, process-wide advisory lock on the checksum
// lock file in dir and returns a release function. The lock serializes the
// read-merge-rename critical section of UpdateFile across aqua processes
// running in the same configuration directory.
//
// The lock is tied to the open file description and is released
// automatically by the operating system when the process exits, even on a
// crash, so a killed process never leaves the lock held.
func acquireLock(dir string) (func(), error) {
	lockPath := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("create a checksum lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire a checksum file lock: %w", err)
	}
	return func() {
		if err := unlockFile(f); err != nil {
			slog.Default().Warn("unlock the checksum file", "error", err, "path", lockPath)
		}
		if err := f.Close(); err != nil {
			slog.Default().Warn("close the checksum lock file", "error", err, "path", lockPath)
		}
	}, nil
}
