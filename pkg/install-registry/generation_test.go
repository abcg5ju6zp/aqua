package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	cfgRegistry "github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/aquaproj/aqua/v2/pkg/cosign"
	"github.com/aquaproj/aqua/v2/pkg/domain"
	registry "github.com/aquaproj/aqua/v2/pkg/install-registry"
	"github.com/aquaproj/aqua/v2/pkg/runtime"
	"github.com/aquaproj/aqua/v2/pkg/slsa"
	"go.yaml.in/yaml/v2"
)

var errDownloadFailed = errors.New("the response was lost")

type routedDownloader struct {
	contents map[string]string
	err      error
	calls    int
	mu       sync.Mutex
}

func (d *routedDownloader) DownloadGitHubContentFile(_ context.Context, _ *slog.Logger, param *domain.GitHubContentFileParam) (*domain.GitHubContentFile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	if s, ok := d.contents[param.Path]; ok {
		return &domain.GitHubContentFile{String: s}, nil
	}
	return nil, errDownloadFailed
}

func testInstaller(t *testing.T, dir string, dl registry.GitHubContentFileDownloader) *registry.Installer {
	t.Helper()
	rt := &runtime.Runtime{GOOS: "linux", GOARCH: "amd64"}
	return registry.New(&config.Param{RootDir: dir, MaxParallelism: 5}, dl, rt, &cosign.MockVerifier{}, &slsa.MockVerifier{})
}

func testRegistry() *aqua.Registry {
	return &aqua.Registry{
		Type:      aqua.RegistryTypeGitHubContent,
		Name:      "standard",
		RepoOwner: "aquaproj",
		RepoName:  "aqua-registry",
		Ref:       "v4.0.0",
		Path:      "registry.yaml",
	}
}

func registryFilePath(t *testing.T, dir string, regist *aqua.Registry) string {
	t.Helper()
	p, err := regist.FilePath(dir, filepath.Join(dir, "aqua.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const registryYAMLv1 = `packages:
- type: github_content
  repo_owner: aquaproj
  repo_name: aqua-installer
  path: aqua-installer
`

const registryYAMLv2 = `packages:
- type: github_content
  repo_owner: aquaproj
  repo_name: aqua-installer
  path: aqua-installer-v2
`

func sha256Hex(t *testing.T, content string) string {
	t.Helper()
	sum, err := checksum.CalculateReader(strings.NewReader(content), "sha256")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToUpper(sum)
}

func readPointer(t *testing.T, registryPath string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(registryPath + ".generation.json")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func pointerString(t *testing.T, registryPath, field string) string {
	t.Helper()
	v, ok := readPointer(t, registryPath)[field].(string)
	if !ok {
		t.Fatalf("pointer field %s isn't a string", field)
	}
	return v
}

// After the first successful install a complete generation is committed, and
// later runs read it without network access (restart, lost response).
func TestInstallRegistry_GenerationIsReusedOffline(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)

	dl := &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}}
	inst := testInstaller(t, dir, dl)

	cfg1, err := inst.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg1.PackageInfos) != 1 {
		t.Fatalf("one package info expected, got %d", len(cfg1.PackageInfos))
	}

	// The second run must not touch the network at all.
	offlineDL := &routedDownloader{err: errDownloadFailed}
	offline := testInstaller(t, dir, offlineDL)
	cfg2, err := offline.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil)
	if err != nil {
		t.Fatalf("the committed generation must be reused offline: %v", err)
	}
	if cfg2.PackageInfos[0].Path != cfg1.PackageInfos[0].Path {
		t.Fatal("the reused generation doesn't match the published one")
	}

	pointer := readPointer(t, registryPath)
	if pointer["checksum"] == "" || pointer["id"] == "" {
		t.Fatal("the generation pointer must record the generation id and checksum")
	}
	if got := offlineDL.calls; got != 0 {
		t.Fatalf("the offline installer created %d downloads, want 0", got)
	}
}

// A tampered content file must never be served; the registry is re-downloaded
// and republished instead of pairing the pointer with wrong bytes.
func TestInstallRegistry_BrokenGenerationIsRepaired(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)

	dl := &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}}
	inst := testInstaller(t, dir, dl)
	if _, err := inst.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil); err != nil {
		t.Fatal(err)
	}
	id := pointerString(t, registryPath, "id")

	// Corrupt the committed content after the pointer was written.
	contentPath := filepath.Join(registryPath+".generations", id, "content")
	if err := os.WriteFile(contentPath, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	// With the network unavailable the broken cache must not be served.
	offline := testInstaller(t, dir, &routedDownloader{err: errDownloadFailed})
	if _, err := offline.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil); err == nil {
		t.Fatal("a broken generation must not be returned")
	}

	// With the network available the generation is rebuilt and verified.
	repaired := testInstaller(t, dir, &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}})
	cfg, err := repaired.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil)
	if err != nil {
		t.Fatalf("the broken generation must be repaired: %v", err)
	}
	if cfg.PackageInfos[0].Path != "aqua-installer" {
		t.Fatal("unexpected repaired content")
	}
}

// When the pointer names a torn current generation but the previous one is
// complete, readers keep serving the original generation.
func TestInstallRegistry_FallsBackToPreviousGeneration(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)

	dl := &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}}
	inst := testInstaller(t, dir, dl)
	if _, err := inst.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil); err != nil {
		t.Fatal(err)
	}
	firstID := pointerString(t, registryPath, "id")

	// Simulate an interrupted update that left the current generation
	// unreadable; the next run repairs it and records the previous generation.
	firstContent := filepath.Join(registryPath+".generations", firstID, "content")
	if err := os.WriteFile(firstContent, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	dl.contents["registry.yaml"] = registryYAMLv2
	if _, err := inst.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil); err != nil {
		t.Fatal(err)
	}
	pointer := readPointer(t, registryPath)
	if pointer["previous"] != firstID {
		t.Fatal("the new generation must point at the previous one")
	}

	// Restore the previous generation and then simulate a crash that removed
	// the current generation directory completely.
	if err := os.WriteFile(firstContent, []byte(registryYAMLv1), 0o600); err != nil {
		t.Fatal(err)
	}
	currentID, ok := pointer["id"].(string)
	if !ok {
		t.Fatal("the pointer id must be a string")
	}
	if err := os.RemoveAll(filepath.Join(registryPath+".generations", currentID)); err != nil {
		t.Fatal(err)
	}

	// Even offline, the previous complete generation is served.
	offline := testInstaller(t, dir, &routedDownloader{err: errDownloadFailed})
	cfg, err := offline.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil)
	if err != nil {
		t.Fatalf("the previous generation must be served: %v", err)
	}
	if cfg.PackageInfos[0].Path != "aqua-installer" {
		t.Fatalf("the original generation was expected, got %q", cfg.PackageInfos[0].Path)
	}
}

// Content that does not match the pinned aqua-checksums record must neither be
// published nor replace the previous generation.
func TestInstallRegistry_ChecksumMismatchRejectsAndKeepsOldGeneration(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	cfgFilePath := filepath.Join(dir, "aqua.yaml")
	registryPath := registryFilePath(t, dir, regist)

	checksums := checksum.New()
	checksums.Set(checksum.RegistryID(regist), &checksum.Checksum{
		ID:        checksum.RegistryID(regist),
		Algorithm: "sha256",
		Checksum:  sha256Hex(t, registryYAMLv1),
	})

	inst := testInstaller(t, dir, &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}})
	if _, err := inst.InstallRegistry(t.Context(), logger, regist, cfgFilePath, checksums); err != nil {
		t.Fatal(err)
	}
	firstID := pointerString(t, registryPath, "id")

	// The pinned record now expects different content, but the server serves
	// yet another one. The update must fail closed and the old generation files
	// must remain the only generation on disk.
	checksums = checksum.New()
	checksums.Set(checksum.RegistryID(regist), &checksum.Checksum{
		ID:        checksum.RegistryID(regist),
		Algorithm: "sha256",
		Checksum:  sha256Hex(t, registryYAMLv2),
	})
	bad := testInstaller(t, dir, &routedDownloader{contents: map[string]string{"registry.yaml": "packages: []\n"}})
	if _, err := bad.InstallRegistry(t.Context(), logger, regist, cfgFilePath, checksums); err == nil {
		t.Fatal("content violating the pinned checksum must be rejected")
	}
	entries, err := os.ReadDir(registryPath + ".generations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != firstID {
		t.Fatalf("only the previous generation must remain, got %v", entries)
	}

	// Serving the content matching the pinned record heals the torn state.
	good := testInstaller(t, dir, &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv2}})
	cfg, err := good.InstallRegistry(t.Context(), logger, regist, cfgFilePath, checksums)
	if err != nil {
		t.Fatalf("matching content must be published: %v", err)
	}
	if cfg.PackageInfos[0].Path != "aqua-installer-v2" {
		t.Fatal("the new pinned generation was expected")
	}
}

// Legacy caches written by aqua versions without generation pointers keep
// working and never trigger a download when valid.
func TestInstallRegistry_LegacyCacheCompatible(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, []byte(registryYAMLv1), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &cfgRegistry.Config{}
	if err := yaml.Unmarshal([]byte(registryYAMLv1), cfg); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath+".json", b, 0o600); err != nil {
		t.Fatal(err)
	}

	offline := testInstaller(t, dir, &routedDownloader{err: errDownloadFailed})
	got, err := offline.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil)
	if err != nil {
		t.Fatalf("the legacy cache must be used offline: %v", err)
	}
	if got.PackageInfos[0].Path != "aqua-installer" {
		t.Fatal("unexpected legacy content")
	}
}

// Concurrent installs of the same source converge on one verified generation.
func TestInstallRegistry_ConcurrentInstalls(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)
	cfgFilePath := filepath.Join(dir, "aqua.yaml")

	dl := &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}}
	inst := testInstaller(t, dir, dl)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			cfg, err := inst.InstallRegistry(t.Context(), logger, regist, cfgFilePath, nil)
			if err != nil {
				errs <- err
				return
			}
			if cfg.PackageInfos[0].Path != "aqua-installer" {
				errs <- errors.New("unexpected generation content")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(registryPath + ".generations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("exactly one generation expected after concurrent installs, got %v", names)
	}
	if dl.calls != 1 {
		t.Fatalf("exactly one download expected, got %d", dl.calls)
	}
}

// When one source fails with no usable generation, other sources still commit
// their generations and remain usable deterministically.
func TestInstallRegistries_OneFailureDoesNotRollBackOthers(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	cfgFilePath := filepath.Join(dir, "aqua.yaml")

	good := testRegistry()
	bad := testRegistry()
	bad.Name = "broken"
	bad.Path = "missing.yaml"

	dl := &routedDownloader{contents: map[string]string{"registry.yaml": registryYAMLv1}}
	cfg := &aqua.Config{Registries: aqua.Registries{
		"good": good,
		"bad":  bad,
	}}
	inst := testInstaller(t, dir, dl)
	if _, err := inst.InstallRegistries(t.Context(), logger, cfg, cfgFilePath, nil); err == nil {
		t.Fatal("the failed source must make InstallRegistries fail")
	}

	// The successful source keeps a deterministic, offline readable generation.
	goodPath := registryFilePath(t, dir, good)
	if _, err := os.Stat(goodPath + ".generation.json"); err != nil {
		t.Fatalf("the successful generation must be committed: %v", err)
	}
	offline := testInstaller(t, dir, &routedDownloader{err: errDownloadFailed})
	got, err := offline.InstallRegistry(t.Context(), logger, good, cfgFilePath, nil)
	if err != nil {
		t.Fatalf("the successful source must stay usable: %v", err)
	}
	if got.PackageInfos[0].Path != "aqua-installer" {
		t.Fatal("unexpected content for the successful source")
	}
}

// A failing update with no previous generation surfaces the download error
// rather than silently pretending the registry is installed, and leaves no
// staging directory or pointer behind.
func TestInstallRegistry_FailedFirstInstallHasNoCache(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	regist := testRegistry()
	registryPath := registryFilePath(t, dir, regist)

	inst := testInstaller(t, dir, &routedDownloader{err: errDownloadFailed})
	if _, err := inst.InstallRegistry(t.Context(), logger, regist, filepath.Join(dir, "aqua.yaml"), nil); err == nil {
		t.Fatal("a failed first install must return the download error")
	}
	if _, err := os.Stat(registryPath + ".generation.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no generation pointer may be published on failure")
	}
	entries, err := os.ReadDir(registryPath + ".generations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("the staging directory must be removed on failure, found %s", e.Name())
		}
	}
}
