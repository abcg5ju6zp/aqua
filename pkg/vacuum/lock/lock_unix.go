//go:build !windows

package lock

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"golang.org/x/sys/unix"
)

const filePermission = 0o664

type unixHandle struct {
	fd      int
	inherit bool
}

func acquire(path string, exclusive, inherit bool) (Handle, error) {
	if err := osfile.MkdirAll(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("create a lock directory: %w", err)
	}
	// O_CLOEXEC so a normally released lock disappears on exec; the exec lease
	// clears the flag afterwards.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, filePermission) //nolint:nosnakecase
	if err != nil {
		return nil, fmt.Errorf("open a lock file: %w", err)
	}
	how := unix.LOCK_SH //nolint:nosnakecase
	if exclusive {
		how = unix.LOCK_EX //nolint:nosnakecase
	}
	if err := unix.Flock(fd, how|unix.LOCK_NB); err != nil { //nolint:nosnakecase
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock a file: %w", err)
	}
	if inherit {
		// Clear FD_CLOEXEC so the executed tool inherits the descriptor and
		// keeps holding the lock until it exits.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil { //nolint:nosnakecase
			_ = unix.Close(fd)
			return nil, fmt.Errorf("clear the close-on-exec flag: %w", err)
		}
	}
	return &unixHandle{fd: fd, inherit: inherit}, nil
}

func (h *unixHandle) Release() error {
	if err := unix.Close(h.fd); err != nil {
		return fmt.Errorf("close a lock file: %w", err)
	}
	return nil
}
