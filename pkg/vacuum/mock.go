package vacuum

import (
	"log/slog"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
)

type Mock struct {
	rootDir    string
	timestamps map[string]time.Time
	err        error
}

func NewMock(rootDir string, timestamps map[string]time.Time, err error) *Mock {
	return &Mock{
		rootDir:     rootDir,
		timestamps: timestamps,
		err:         err,
	}
}

func (m *Mock) Remove(pkgPath string) error {
	return m.err
}

func (m *Mock) Purge(pkgPath string) error {
	return m.err
}

func (m *Mock) PurgePath(_ *slog.Logger, _ string) error {
	return m.err
}

func (m *Mock) Create(pkgPath string, timestamp time.Time) error {
	return m.err
}

func (m *Mock) Update(pkgPath string, timestamp time.Time) error {
	return m.err
}

func (m *Mock) FindAll(_ *slog.Logger) (map[string]time.Time, error) {
	return m.timestamps, m.err
}

// AcquireLease returns a no-op lease. Tests that need real lease semantics use
// the Client with a temporary root directory.
func (m *Mock) AcquireLease(_ string, _ bool) (lock.Handle, error) {
	return lock.NoopHandle(), m.err
}
