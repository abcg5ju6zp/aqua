//go:build windows

package vacuum

import "golang.org/x/sys/windows"

// processAlive reports whether a process with the given pid exists. Opening a
// query-limited handle fails once the process has exited.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}
