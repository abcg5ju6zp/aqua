package vacuum

import (
	"context"
	"log/slog"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

// protectedPkgPaths fixes the set of package versions that are still
// resolvable from the current configuration and registries BEFORE the cleanup
// starts. The snapshot is reused for both deletion decisions and crash
// recovery, so a broken config can never widen the set of deleted versions.
//
// A configuration file that fails to be read or resolved is logged and
// skipped: versions referenced by the other configurations are still
// protected, and unreferenced versions are handled according to their
// timestamps and leases.
func (c *Controller) protectedPkgPaths(ctx context.Context, logger *slog.Logger, param *config.Param) map[string]struct{} {
	protected := map[string]struct{}{}
	cfgFilePaths := c.configFinder.Finds(param.CWD, param.ConfigFilePath)
	cfgFilePaths = append(cfgFilePaths, param.GlobalConfigFilePaths...)
	for _, cfgFilePath := range cfgFilePaths {
		if f, err := osfile.Exists(cfgFilePath); err != nil {
			slogerr.WithError(logger, err).Warn("check if a configuration file exists", "config_file", cfgFilePath)
			continue
		} else if !f {
			// Missing global configuration files are optional.
			continue
		}
		c.collectProtectedPkgPaths(ctx, logger, cfgFilePath, param, protected)
	}
	return protected
}

func (c *Controller) collectProtectedPkgPaths(ctx context.Context, logger *slog.Logger, cfgFilePath string, param *config.Param, protected map[string]struct{}) {
	cfg := &aqua.Config{}
	if err := c.configReader.Read(logger, cfgFilePath, cfg); err != nil {
		slogerr.WithError(logger, err).Warn("read a configuration file, so packages in the file aren't protected", "config_file", cfgFilePath)
		return
	}

	checksums, updateChecksum, err := checksum.Open(
		logger, cfgFilePath,
		param.ChecksumEnabled(cfg))
	if err != nil {
		slogerr.WithError(logger, err).Warn("read a checksum JSON, so packages in the file aren't protected", "config_file", cfgFilePath)
		return
	}
	defer updateChecksum()

	registryContents, err := c.registryInstaller.InstallRegistries(ctx, logger, cfg, cfgFilePath, checksums)
	if err != nil {
		slogerr.WithError(logger, err).Warn("install registries, so packages in the file aren't protected", "config_file", cfgFilePath)
		return
	}

	pkgs, _ := config.ListPackages(logger, cfg, c.runtime, registryContents)
	for _, pkg := range pkgs {
		pkgPath, err := pkg.PkgPath(c.runtime)
		if err != nil {
			slogerr.WithError(logger, err).Warn("get a package path", "config_file", cfgFilePath)
			continue
		}
		protected[pkgPath] = struct{}{}
	}
}
