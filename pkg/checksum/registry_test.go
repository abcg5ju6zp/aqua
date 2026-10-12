package checksum_test

import (
	"testing"

	"github.com/aquaproj/aqua/v2/pkg/checksum"
	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
)

func TestRegistryID(t *testing.T) {
	t.Parallel()
	data := []struct {
		name     string
		registry *aqua.Registry
		expected string
	}{
		{
			name: "basic registry",
			registry: &aqua.Registry{
				RepoOwner: "aquaproj",
				RepoName:  "aqua-registry",
				Ref:       "v4.0.0",
				Path:      "",
			},
			expected: "registries/github_content/github.com/aquaproj/aqua-registry/v4.0.0",
		},
		{
			name: "registry with empty ref",
			registry: &aqua.Registry{
				RepoOwner: "example",
				RepoName:  "tools",
				Ref:       "",
				Path:      "",
			},
			expected: "registries/github_content/github.com/example/tools",
		},
		{
			name: "registry with special characters",
			registry: &aqua.Registry{
				RepoOwner: "org-name",
				RepoName:  "tool.registry",
				Ref:       "v1.2.3-beta",
				Path:      "registry.yaml",
			},
			expected: "registries/github_content/github.com/org-name/tool.registry/v1.2.3-beta/registry.yaml",
		},
	}

	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			result := checksum.RegistryID(d.registry)
			if result != d.expected {
				t.Errorf("Expected %s, got %s", d.expected, result)
			}
		})
	}
}

func TestVerifyRegistry(t *testing.T) {
	t.Parallel()
	regist := &aqua.Registry{
		RepoOwner: "test",
		RepoName:  "registry",
		Ref:       pkgVersionV1,
		Path:      "",
	}
	content := []byte("test content")

	t.Run("new registry - no existing checksum", func(t *testing.T) {
		t.Parallel()
		algorithm, sum, err := checksum.VerifyRegistry(regist, checksum.New(), content)
		if err != nil {
			t.Fatalf("VerifyRegistry returned an error: %v", err)
		}
		if algorithm != "sha256" || sum == "" {
			t.Errorf("unexpected algorithm or checksum: %s %s", algorithm, sum)
		}
	})

	t.Run("checksum disabled", func(t *testing.T) {
		t.Parallel()
		if _, _, err := checksum.VerifyRegistry(regist, nil, content); err != nil {
			t.Errorf("VerifyRegistry with nil checksums returned an error: %v", err)
		}
	})

	t.Run("valid existing checksum", func(t *testing.T) {
		t.Parallel()
		checksums := checksum.New()
		_, sum, err := checksum.VerifyRegistry(regist, checksums, content)
		if err != nil {
			t.Fatalf("the first verification failed: %v", err)
		}
		// VerifyRegistry is read-only; the record is committed only after the
		// content generation is published.
		checksum.CommitRegistry(regist, checksums, "sha256", sum)
		if _, _, err := checksum.VerifyRegistry(regist, checksums, content); err != nil {
			t.Errorf("the second verification failed: %v", err)
		}
	})
}

func TestVerifyRegistryWithMismatch(t *testing.T) {
	t.Parallel()

	registry := &aqua.Registry{
		RepoOwner: "test",
		RepoName:  "registry",
		Ref:       pkgVersionV1,
		Path:      "",
	}

	checksums := checksum.New()

	// First, establish a checksum for one content and commit it.
	originalContent := []byte("original content")
	_, sum, err := checksum.VerifyRegistry(registry, checksums, originalContent)
	if err != nil {
		t.Fatalf("Failed to establish checksum: %v", err)
	}
	checksum.CommitRegistry(registry, checksums, "sha256", sum)

	// Now try with different content - should fail and mutate nothing.
	modifiedContent := []byte("modified content")
	if _, _, err := checksum.VerifyRegistry(registry, checksums, modifiedContent); err == nil {
		t.Error("Expected error for mismatched content but got none")
	}
}
