//go:build !windows

package vacuum

import (
	"errors"

	"golang.org/x/sys/unix"
)

// processAlive reports whether a process with the given pid exists. Signal 0
// performs no delivery but performs the existence and permission checks.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
