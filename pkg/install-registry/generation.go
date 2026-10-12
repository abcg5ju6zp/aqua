package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

const (
	// registryFilePermission is the mode of aqua owned registry files.
	registryFilePermission = 0o600
	// generationManifestVersion is the generation manifest format version.
	generationManifestVersion = 1
	// generationsDirSuffix is appended to the registry file path to get the
	// directory that holds content generations.
	generationsDirSuffix = ".generations"
	// generationPointerSuffix is appended to the registry file path to get the
	// cache index: a manifest that atomically names the current generation.
	generationPointerSuffix = ".generation.json"
	// contentFileName stores the exact downloaded bytes of a generation.
	contentFileName = "content"
	// cacheFileName stores the registry parsed as JSON for YAML sources.
	cacheFileName = "cache.json"
	// manifestFileName is the manifest stored inside every generation directory.
	manifestFileName = "generation.json"
)

var errBrokenGeneration = errors.New("the registry generation is broken")

// generationManifest pins one content generation of a registry source.
//
// The pointer manifest (written next to the legacy registry file) names the
// current generation directory and the previous one. Every manifest records
// the source coordinates and the checksum of the exact downloaded content, so
// readers can prove that the registry content, the JSON cache and the checksum
// record belong to the same generation.
type generationManifest struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Previous     string `json:"previous,omitempty"`
	Type         string `json:"type"`
	RepoOwner    string `json:"repo_owner"`
	RepoName     string `json:"repo_name"`
	Ref          string `json:"ref"`
	Path         string `json:"path"`
	JSONCache    bool   `json:"json_cache"`
	Algorithm    string `json:"algorithm"`
	Checksum     string `json:"checksum"`
	JSONChecksum string `json:"json_checksum,omitempty"`
}

func generationPaths(registryFilePath string) (generationsDir, pointerPath string) {
	return registryFilePath + generationsDirSuffix, registryFilePath + generationPointerSuffix
}

func (m *generationManifest) sourceMatches(regist *aqua.Registry) bool {
	return m.Type == regist.Type &&
		m.RepoOwner == regist.RepoOwner &&
		m.RepoName == regist.RepoName &&
		m.Ref == regist.Ref &&
		m.Path == regist.Path
}

// loadGenerationPointer reads and decodes the pointer to the current
// generation. A missing pointer is reported with os.ErrNotExist.
func loadGenerationPointer(pointerPath string) (*generationManifest, error) {
	m, err := loadManifest(pointerPath)
	if err != nil {
		return nil, fmt.Errorf("read the registry generation pointer: %w", err)
	}
	return m, nil
}

// loadManifest reads and decodes a generation manifest, either the cache index
// next to the registry or the manifest stored in a generation directory.
func loadManifest(p string) (*generationManifest, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	defer f.Close()
	m := &generationManifest{}
	if err := json.NewDecoder(f).Decode(m); err != nil {
		return nil, fmt.Errorf("parse the registry generation manifest: %w", err)
	}
	return m, nil
}

// readGenerationDirectory verifies and parses a committed generation
// directory. Besides checking every file against the manifest checksums, it
// rejects generations from another source and generations whose recorded
// checksum does not match the pinned checksum record, so a half updated cache
// can never be served.
func readGenerationDirectory(generationsDir string, m *generationManifest, regist *aqua.Registry, pinned *checksum.Checksum) (*registry.Config, error) {
	if err := m.validate(regist); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(generationsDir, m.ID, contentFileName))
	if err != nil {
		return nil, fmt.Errorf("read the registry generation content: %w", err)
	}
	if err := verifyChecksum(content, m.Algorithm, m.Checksum, "registry content"); err != nil {
		return nil, err
	}
	if pinned != nil && !strings.EqualFold(pinned.Checksum, m.Checksum) {
		return nil, fmt.Errorf("%w: the generation checksum doesn't match the pinned checksum record", errBrokenGeneration)
	}
	if !m.JSONCache {
		cfg := &registry.Config{}
		if err := json.Unmarshal(content, cfg); err != nil {
			return nil, fmt.Errorf("parse the registry content as JSON: %w", err)
		}
		return cfg, nil
	}
	return readJSONCacheGeneration(generationsDir, m)
}

// readJSONCacheGeneration reads and verifies the JSON cache that stores the
// parsed YAML registry of a generation.
func readJSONCacheGeneration(generationsDir string, m *generationManifest) (*registry.Config, error) {
	cacheBytes, err := os.ReadFile(filepath.Join(generationsDir, m.ID, cacheFileName))
	if err != nil {
		return nil, fmt.Errorf("read the registry JSON cache: %w", err)
	}
	if err := verifyChecksum(cacheBytes, m.Algorithm, m.JSONChecksum, "JSON cache"); err != nil {
		return nil, err
	}
	cfg := &registry.Config{}
	if err := json.Unmarshal(cacheBytes, cfg); err != nil {
		return nil, fmt.Errorf("parse the registry JSON cache: %w", err)
	}
	return cfg, nil
}

// validate checks that a manifest is supported and describes the given source.
func (m *generationManifest) validate(regist *aqua.Registry) error {
	if m.ID == "" || m.Version != generationManifestVersion {
		return fmt.Errorf("%w: unsupported generation manifest", errBrokenGeneration)
	}
	if !m.sourceMatches(regist) {
		return fmt.Errorf("%w: the generation belongs to another registry source", errBrokenGeneration)
	}
	return nil
}

// verifyChecksum proves that a generation file was not torn or swapped.
func verifyChecksum(b []byte, algorithm, expected, name string) error {
	sum, err := checksum.CalculateReader(strings.NewReader(string(b)), algorithm)
	if err != nil {
		return fmt.Errorf("calculate the %s checksum: %w", name, err)
	}
	if !strings.EqualFold(sum, expected) {
		return fmt.Errorf("%w: the %s checksum doesn't match the generation", errBrokenGeneration, name)
	}
	return nil
}

// readInstalledRegistry reads the newest fully committed and verified
// generation. It tries the current generation, the previous one and finally
// the legacy registry files, so a crash or a concurrent install either resumes
// from the new generation or keeps serving the original one.
func (is *Installer) readInstalledRegistry(logger *slog.Logger, regist *aqua.Registry, registryFilePath string, checksums *checksum.Checksums) (*registry.Config, error) {
	generationsDir, pointerPath := generationPaths(registryFilePath)
	pinned := pinnedChecksum(regist, checksums)

	if cfg, _, ok := readCurrentGeneration(logger, generationsDir, pointerPath, regist, pinned); ok {
		return cfg, nil
	}
	cfg, err := is.readLegacyRegistry(logger, registryFilePath)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// pinnedChecksum returns the checksum recorded for the source, marking it as
// still in use for pruning. A nil collection means checksum verification is
// disabled.
func pinnedChecksum(regist *aqua.Registry, checksums *checksum.Checksums) *checksum.Checksum {
	if checksums == nil {
		return nil
	}
	return checksums.Get(checksum.RegistryID(regist))
}

// readCurrentGeneration resolves the current generation, falling back to the
// previous generation when the current one is torn.
func readCurrentGeneration(logger *slog.Logger, generationsDir, pointerPath string, regist *aqua.Registry, pinned *checksum.Checksum) (*registry.Config, *generationManifest, bool) {
	m, err := loadGenerationPointer(pointerPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slogerr.WithError(logger, err).Warn("the registry generation pointer is broken; falling back to legacy registry files",
				"registry_name", regist.Name)
		}
		return nil, nil, false
	}
	if cfg, err := readGenerationDirectory(generationsDir, m, regist, pinned); err == nil {
		return cfg, m, true
	} else {
		slogerr.WithError(logger, err).Warn("the current registry generation is broken; trying the previous one",
			"registry_name", regist.Name)
	}
	if m.Previous == "" {
		return nil, nil, false
	}
	// The previous generation is verified against its own manifest, which
	// records the checksums of that exact generation.
	prev, err := loadManifest(filepath.Join(generationsDir, m.Previous, manifestFileName))
	if err != nil || prev.ID != m.Previous {
		return nil, nil, false
	}
	cfg, err := readGenerationDirectory(generationsDir, prev, regist, pinned)
	if err != nil {
		slogerr.WithError(logger, err).Warn("the previous registry generation is broken too",
			"registry_name", regist.Name)
		return nil, nil, false
	}
	return cfg, prev, true
}

// readLegacyRegistry reads registries cached by aqua versions without content
// generations: the registry file and the JSON cache next to it. This keeps
// existing valid caches compatible.
func (is *Installer) readLegacyRegistry(logger *slog.Logger, registryFilePath string) (*registry.Config, error) {
	cfg := &registry.Config{}
	if isJSON(registryFilePath) {
		if err := is.readJSONRegistry(registryFilePath, cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	jsonPath := registryFilePath + jsonSuffix
	if err := is.readJSONRegistry(jsonPath, cfg); err == nil {
		return cfg, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		removeBrokenJSONCache(logger, jsonPath, err)
	}
	if err := is.readYAMLRegistry(registryFilePath, cfg); err != nil {
		return nil, err
	}
	if err := is.createJSON(jsonPath, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// removeBrokenJSONCache deletes a JSON cache that cannot be parsed so it can
// be regenerated from the registry file.
func removeBrokenJSONCache(logger *slog.Logger, jsonPath string, readErr error) {
	slogerr.WithError(logger, readErr).Warn("failed to read a registry JSON file. Will remove and recreate the file", "registry_json_path", jsonPath)
	if err := os.Remove(jsonPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slogerr.WithError(logger, err).Warn("failed to remove a registry JSON file", "registry_json_path", jsonPath)
	}
}
