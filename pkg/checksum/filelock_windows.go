//go:build windows

package checksum

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.LockFileEx( //nolint:wrapcheck
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		^uint32(0),
		^uint32(0),
		ol,
	)
}

func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), ol) //nolint:wrapcheck
}
