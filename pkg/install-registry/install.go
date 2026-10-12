package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

var errMaxParallelismMustBeGreaterThanZero = errors.New("MaxParallelism must be greater than zero")

func (is *Installer) InstallRegistries(ctx context.Context, logger *slog.Logger, cfg *aqua.Config, cfgFilePath string, checksums *checksum.Checksums) (map[string]*registry.Config, error) {
	var wg sync.WaitGroup
	var flagMutex sync.Mutex
	var registriesMutex sync.Mutex
	var failed bool
	if is.param.MaxParallelism <= 0 {
		return nil, errMaxParallelismMustBeGreaterThanZero
	}
	maxInstallChan := make(chan struct{}, is.param.MaxParallelism)
	registryContents := make(map[string]*registry.Config, len(cfg.Registries)+1)

	for _, registry := range cfg.Registries {
		if registry == nil {
			continue
		}
		wg.Add(1)
		go func(registry *aqua.Registry) {
			defer wg.Done()
			if registry.Name == "" {
				logger.Debug("ignore a registry because the registry name is empty")
				return
			}
			maxInstallChan <- struct{}{}
			registryContent, err := is.InstallRegistry(ctx, logger, registry, cfgFilePath, checksums)
			if err != nil {
				<-maxInstallChan
				slogerr.WithError(logger, err).Error("install the registry", "registry_name", registry.Name)
				flagMutex.Lock()
				failed = true
				flagMutex.Unlock()
				return
			}
			registriesMutex.Lock()
			// The cache index is populated only after the generation was fully
			// downloaded, verified, parsed and published, so package references
			// can never resolve against a half updated registry.
			registryContents[registry.Name] = registryContent
			registriesMutex.Unlock()
			<-maxInstallChan
		}(registry)
	}
	wg.Wait()
	if failed {
		// Successfully installed sources keep their committed generations on
		// disk, but packages depending on a failed source are not installed:
		// returning an error makes the package phase fail closed instead of
		// reading a half updated cache.
		return nil, errInstallFailure
	}

	return registryContents, nil
}

// InstallRegistry installs and reads the registry file and returns the registry content.
// If a complete generation of the registry already exists, it is used without network access.
func (is *Installer) InstallRegistry(ctx context.Context, logger *slog.Logger, regist *aqua.Registry, cfgFilePath string, checksums *checksum.Checksums) (*registry.Config, error) {
	if err := regist.Validate(); err != nil {
		return nil, fmt.Errorf("validate the registry: %w", err)
	}
	registryFilePath, err := regist.FilePath(is.param.RootDir, cfgFilePath)
	if err != nil {
		return nil, fmt.Errorf("get a registry file path: %w", err)
	}

	if regist.Type == aqua.RegistryTypeLocal {
		registryContent := &registry.Config{}
		if err := is.readRegistry(registryFilePath, registryContent); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, slogerr.With(errLocalRegistryNotFound, //nolint:wrapcheck
					"local_registry_file_path", registryFilePath)
			}
			return nil, err
		}
		return registryContent, nil
	}

	return is.installGitHubContentRegistry(ctx, logger, regist, registryFilePath, checksums)
}

// lockRegistrySource serializes installs targeting the same cached registry
// within the process (e.g. the same source referenced from global and local
// configuration). Cross process safety is provided by isolated staging
// directories and the atomic generation pointer switch.
func (is *Installer) lockRegistrySource(registryFilePath string) func() {
	is.sourceLocksMu.Lock()
	l, ok := is.sourceLocks[registryFilePath]
	if !ok {
		l = &sourceMutex{}
		is.sourceLocks[registryFilePath] = l
	}
	is.sourceLocksMu.Unlock()
	l.mu.Lock()
	return l.mu.Unlock
}
