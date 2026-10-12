package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/aquaproj/aqua/v2/pkg/domain"
	"github.com/aquaproj/aqua/v2/pkg/osfile"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
	"go.yaml.in/yaml/v2"
)

// installGitHubContentRegistry returns a fully verified registry generation for
// a github_content source. A committed generation is served without network if
// it is complete; otherwise a new generation is built in an isolated staging
// directory and only the cache index is switched atomically.
func (is *Installer) installGitHubContentRegistry(ctx context.Context, logger *slog.Logger, regist *aqua.Registry, registryFilePath string, checksums *checksum.Checksums) (*registry.Config, error) {
	unlock := is.lockRegistrySource(registryFilePath)
	defer unlock()

	// Prefer the current or previous committed generation, or a valid legacy
	// cache. Existing valid caches keep working and take priority over updating.
	if cfg, err := is.readInstalledRegistry(logger, regist, registryFilePath, checksums); err == nil {
		return cfg, nil
	}

	cfg, err := is.publishNewGeneration(ctx, logger, regist, registryFilePath, checksums)
	if err == nil {
		return cfg, nil
	}

	// The update failed (the response was lost, the checksum didn't match, a
	// concurrent install won, or the process was restarted). Continue with the
	// original complete generation if one is still readable; never serve a half
	// updated cache.
	if cfg, fallbackErr := is.readInstalledRegistry(logger, regist, registryFilePath, checksums); fallbackErr == nil {
		slogerr.WithError(logger, err).Warn("failed to update the registry; using the previous committed generation",
			"registry_name", regist.Name, "registry_file_path", registryFilePath)
		return cfg, nil
	}
	return nil, err
}

// publishNewGeneration downloads the complete content into a private staging
// directory, verifies the source and the checksum, parses every package
// definition, and only then publishes the generation and the cache index
// atomically. The checksum record is committed last.
func (is *Installer) publishNewGeneration(ctx context.Context, logger *slog.Logger, regist *aqua.Registry, registryFilePath string, checksums *checksum.Checksums) (*registry.Config, error) {
	generationsDir, pointerPath := generationPaths(registryFilePath)
	stageDir, id, cleanup, err := prepareStaging(logger, generationsDir, pointerPath)
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		cleanup(&published)
	}()

	// 1. Download the complete content from the configured source into staging.
	content, err := is.downloadContent(ctx, logger, regist)
	if err != nil {
		return nil, err
	}

	// 2. Verify the source and the checksum before parsing or publishing.
	algorithm, sum, err := checksum.VerifyRegistry(regist, checksums, content)
	if err != nil {
		return nil, fmt.Errorf("verify a registry's checksum: %w", err)
	}

	// 3. Parse all package definitions and references before anything is
	// published, so malformed content never replaces a valid generation.
	cfg, err := parseRegistryContent(registryFilePath, content)
	if err != nil {
		return nil, err
	}

	// 4. Materialize the whole generation inside the staging directory.
	manifest, manifestBytes, err := buildGeneration(stageDir, id, pointerPath, registryFilePath, regist, algorithm, sum, cfg, content)
	if err != nil {
		return nil, err
	}

	// 5. Switch the cache index atomically and commit the checksum record.
	if err := commitGeneration(logger, generationsDir, pointerPath, manifest, manifestBytes, regist, checksums, algorithm, sum); err != nil {
		return nil, err
	}
	published = true
	return cfg, nil
}

// prepareStaging creates the generations directory and an isolated staging
// directory. The returned cleanup removes the staging directory unless the
// generation was published.
func prepareStaging(logger *slog.Logger, generationsDir, pointerPath string) (stageDir, id string, cleanup func(*bool), err error) {
	if err := osfile.MkdirAll(filepath.Dir(pointerPath)); err != nil {
		return "", "", nil, fmt.Errorf("create the parent directory of the registry: %w", err)
	}
	if err := osfile.MkdirAll(generationsDir); err != nil {
		return "", "", nil, fmt.Errorf("create the registry generations directory: %w", err)
	}
	stageDir, err = osfile.MkdirTemp(generationsDir)
	if err != nil {
		return "", "", nil, fmt.Errorf("create an isolated staging directory: %w", err)
	}
	cleanup = func(published *bool) {
		if *published {
			return
		}
		if err := os.RemoveAll(stageDir); err != nil {
			slogerr.WithError(logger, err).Warn("failed to remove a staging directory", "staging_dir", stageDir)
		}
	}
	return stageDir, filepath.Base(stageDir), cleanup, nil
}

// buildGeneration constructs the manifest, including a link to the previous
// generation, and writes content, JSON cache and manifest into the staging
// directory.
func buildGeneration(stageDir, id, pointerPath, registryFilePath string, regist *aqua.Registry, algorithm, sum string, cfg *registry.Config, content []byte) (*generationManifest, []byte, error) {
	manifest := newGenerationManifest(id, regist, !isJSON(registryFilePath), algorithm, sum)
	if previous, err := loadGenerationPointer(pointerPath); err == nil && previous.sourceMatches(regist) {
		manifest.Previous = previous.ID
	}
	manifestBytes, err := writeGeneration(stageDir, manifest, cfg, content)
	if err != nil {
		return nil, nil, err
	}
	return manifest, manifestBytes, nil
}

// commitGeneration publishes the cache index atomically. The staging directory
// becomes the committed generation the moment the pointer rename lands; before
// that every reader keeps seeing the previous generation. The checksum record
// is committed only after the generation is live, and obsolete generations are
// removed.
func commitGeneration(logger *slog.Logger, generationsDir, pointerPath string, manifest *generationManifest, manifestBytes []byte, regist *aqua.Registry, checksums *checksum.Checksums, algorithm, sum string) error {
	if err := osfile.WriteFileAtomic(pointerPath, registryFilePermission, func(f *os.File) error {
		if _, err := f.Write(manifestBytes); err != nil {
			return fmt.Errorf("write the registry generation pointer: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("publish the registry generation: %w", err)
	}
	checksum.CommitRegistry(regist, checksums, algorithm, sum)
	removeObsoleteGenerations(logger, generationsDir, manifest)
	return nil
}

// downloadContent fetches the complete registry content from the source
// configured in regist and returns its bytes. The source coordinates are never
// derived from the network response.
func (is *Installer) downloadContent(ctx context.Context, logger *slog.Logger, regist *aqua.Registry) ([]byte, error) {
	ghContentFile, err := is.registryDownloader.DownloadGitHubContentFile(ctx, logger, &domain.GitHubContentFileParam{
		RepoOwner: regist.RepoOwner,
		RepoName:  regist.RepoName,
		Ref:       regist.Ref,
		Path:      regist.Path,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer ghContentFile.Close()
	content, err := ghContentFile.Byte()
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	return content, nil
}

// parseRegistryContent fully decodes every package definition of a registry.
// JSON sources are decoded as JSON, everything else as YAML.
func parseRegistryContent(registryFilePath string, content []byte) (*registry.Config, error) {
	cfg := &registry.Config{}
	if isJSON(registryFilePath) {
		if err := json.Unmarshal(content, cfg); err != nil {
			return nil, fmt.Errorf("parse the registry configuration file as JSON: %w", err)
		}
		return cfg, nil
	}
	if err := yaml.Unmarshal(content, cfg); err != nil {
		return nil, fmt.Errorf("parse the registry configuration file as YAML: %w", err)
	}
	return cfg, nil
}

// newGenerationManifest builds the manifest of a generation before any file is
// written outside the staging directory.
func newGenerationManifest(id string, regist *aqua.Registry, jsonCache bool, algorithm, sum string) *generationManifest {
	return &generationManifest{
		Version:   generationManifestVersion,
		ID:        id,
		Type:      regist.Type,
		RepoOwner: regist.RepoOwner,
		RepoName:  regist.RepoName,
		Ref:       regist.Ref,
		Path:      regist.Path,
		JSONCache: jsonCache,
		Algorithm: algorithm,
		Checksum:  sum,
	}
}

// writeGeneration materializes content, JSON cache and manifest inside the
// private staging directory and flushes every file. It returns the exact
// manifest bytes that must be published as the cache index.
func writeGeneration(stageDir string, manifest *generationManifest, cfg *registry.Config, content []byte) ([]byte, error) {
	if err := writeStagedFile(filepath.Join(stageDir, contentFileName), content); err != nil {
		return nil, err
	}
	if manifest.JSONCache {
		cacheBytes, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("encode the registry as JSON: %w", err)
		}
		jsonSum, err := checksum.CalculateReader(bytes.NewReader(cacheBytes), manifest.Algorithm)
		if err != nil {
			return nil, fmt.Errorf("calculate the JSON cache checksum: %w", err)
		}
		manifest.JSONChecksum = jsonSum
		if err := writeStagedFile(filepath.Join(stageDir, cacheFileName), cacheBytes); err != nil {
			return nil, err
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the generation manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := writeStagedFile(filepath.Join(stageDir, manifestFileName), manifestBytes); err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

// writeStagedFile writes and flushes one file of a generation in the private
// staging directory. The staging directory becomes visible as a whole when the
// generation pointer is renamed into place.
func writeStagedFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, registryFilePermission)
	if err != nil {
		return fmt.Errorf("create a staging file: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write a staging file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flush a staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close a staging file: %w", err)
	}
	return nil
}

// removeObsoleteGenerations deletes interrupted staging directories and
// generations other than the current and previous one. It never removes what
// the just published pointer names, so a concurrent reader keeps a valid
// generation.
func removeObsoleteGenerations(logger *slog.Logger, generationsDir string, current *generationManifest) {
	entries, err := os.ReadDir(generationsDir)
	if err != nil {
		slogerr.WithError(logger, err).Warn("failed to read the registry generations directory", "generations_dir", generationsDir)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == current.ID || name == current.Previous {
			continue
		}
		if err := os.RemoveAll(filepath.Join(generationsDir, name)); err != nil {
			slogerr.WithError(logger, err).Warn("failed to remove an obsolete registry generation", "generation_dir", filepath.Join(generationsDir, name))
		} else {
			logger.Debug("removed an obsolete registry generation", "generation", name)
		}
	}
}
