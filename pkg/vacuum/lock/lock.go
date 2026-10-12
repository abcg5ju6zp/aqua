// Package lock provides advisory cross-process file locks used by vacuum.
//
// The locks are tied to open file descriptions, not to lock file paths: when a
// process exits (or is killed), the kernel releases its locks automatically.
// This is what makes leases of executed packages recoverable after a crash,
// including the execve(2) case where the aqua process image is replaced by
// the executed tool.
package lock

import "errors"

// ErrLocked means a non-blocking lock acquisition failed because another
// process holds a conflicting lock.
var ErrLocked = errors.New("the file is locked by another process")

// Handle is an acquired lock. Release unlocks and closes the underlying file.
type Handle interface {
	Release() error
}

type noopHandle struct{}

// NoopHandle returns a handle that does nothing. It is used when lease
// tracking is disabled or when lock acquisition is skipped.
func NoopHandle() Handle {
	return noopHandle{}
}

func (noopHandle) Release() error { return nil }

// Acquire acquires an advisory lock on path in non-blocking mode, creating
// the file (and its parent directory) if needed.
//
// If exclusive is false, a shared lock is acquired; multiple shared locks and
// one exclusive lock are mutually exclusive. If exclusive is true, an
// exclusive lock is acquired.
//
// If inherit is true, the file descriptor stays open across execve(2) on
// Unix, so the executed tool keeps holding the lock until it exits. It is
// ignored on Windows, which uses a child process model instead of execve.
func Acquire(path string, exclusive, inherit bool) (Handle, error) {
	return acquire(path, exclusive, inherit)
}
