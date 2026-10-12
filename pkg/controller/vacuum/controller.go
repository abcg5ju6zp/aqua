package vacuum

import (
	"context"
	"log/slog"
	"time"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/aquaproj/aqua/v2/pkg/runtime"
	"github.com/aquaproj/aqua/v2/pkg/vacuum"
	"github.com/aquaproj/aqua/v2/pkg/vacuum/lock"
)

type Controller struct {
	rootDir           string
	runtime           *runtime.Runtime
	vacuum            Vacuum
	configFinder      ConfigFinder
	configReader      ConfigReader
	registryInstaller RegistryInstaller
}

func New(param *config.Param, rt *runtime.Runtime, vc Vacuum, configFinder ConfigFinder, configReader ConfigReader, registryInstaller RegistryInstaller) *Controller {
	return &Controller{
		rootDir:           param.RootDir,
		runtime:           rt,
		vacuum:            vc,
		configFinder:      configFinder,
		configReader:      configReader,
		registryInstaller: registryInstaller,
	}
}

// Vacuum is the subset of vacuum.Client used by the controller, split into
// focused interfaces so the filesystem capabilities stay testable in parts.
type Vacuum interface {
	TimestampStore
	LeaseStore
	JournalStore
	QuarantineStore
}

type TimestampStore interface {
	FindAll(logger *slog.Logger) (map[string]time.Time, error)
	Create(pkgPath string, timestamp time.Time) error
	Purge(pkgPath string) error
	ReadTimestamp(pkgPath string) (time.Time, bool, error)
}

type LeaseStore interface {
	LockVacuum() (lock.Handle, error)
	AcquireExclusiveLease(pkgPath string) (lock.Handle, error)
	HasUnexpiredLease(pkgPath string, now time.Time) (bool, error)
}

type JournalStore interface {
	SaveJournal(j *vacuum.Journal) error
	RemoveJournal(pkgPath string) error
	ListJournals(logger *slog.Logger) (map[string]*vacuum.Journal, error)
}

type QuarantineStore interface {
	ReferencedByShim(absPkgPath string) (bool, error)
	MoveToQuarantine(pkgPath string) error
	RestoreFromQuarantine(pkgPath string) error
	RemoveQuarantined(pkgPath string) error
	QuarantineExists(pkgPath string) (bool, error)
	QuarantinePath(pkgPath string) string
	ListQuarantine() ([]vacuum.QuarantineEntry, error)
	SrcPath(pkgPath string) string
}

type ConfigReader interface {
	Read(logger *slog.Logger, configFilePath string, cfg *aqua.Config) error
}

type ConfigFinder interface {
	Finds(wd, configFilePath string) []string
}

type RegistryInstaller interface {
	InstallRegistries(ctx context.Context, logger *slog.Logger, cfg *aqua.Config, cfgFilePath string, checksums *checksum.Checksums) (map[string]*registry.Config, error)
}
