package vacuum

import "errors"

// ErrVacuumRunning means another vacuum process holds the global vacuum lock.
var ErrVacuumRunning = errors.New("another aqua vacuum is running")
