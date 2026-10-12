//go:build windows

package lock

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"golang.org/x/sys/windows"
)

type windowsHandle struct {
	handle windows.Handle
}

func acquire(path string, exclusive, _ bool) (Handle, error) {
	if err := osfile.MkdirAll(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create a lock directory: %w", err)
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("convert a lock file path: %w", err)
	}
	// Windows uses a child process model: aqua waits for the tool and releases
	// the lock afterwards, so the handle mustn't be inherited.
	h, err := windows.CreateFile(pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("open a lock file: %w", err)
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(h, flags, 0, ^uint32(0), ^uint32(0), &ol); err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock a file: %w", err)
	}
	return &windowsHandle{handle: h}, nil
}

func (h *windowsHandle) Release() error {
	var ol windows.Overlapped
	if err := windows.UnlockFileEx(h.handle, 0, ^uint32(0), ^uint32(0), &ol); err != nil {
		_ = windows.CloseHandle(h.handle)
		return fmt.Errorf("unlock a file: %w", err)
	}
	if err := windows.CloseHandle(h.handle); err != nil {
		return fmt.Errorf("close a lock file: %w", err)
	}
	return nil
}
