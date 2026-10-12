package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aquaproj/aqua/v2/pkg/config/registry"
	"github.com/aquaproj/aqua/v2/pkg/osfile"
)

// createJSON writes the converted registry JSON cache atomically, so a crash
// can never leave a truncated cache next to a valid registry file.
func (is *Installer) createJSON(jsonPath string, content *registry.Config) error {
	if err := osfile.WriteFileAtomic(jsonPath, registryFilePermission, func(f *os.File) error {
		if err := json.NewEncoder(f).Encode(content); err != nil {
			return fmt.Errorf("encode a registry as JSON: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("create a file to convert registry YAML to JSON: %w", err)
	}
	return nil
}

func isJSON(p string) bool {
	return strings.HasSuffix(p, jsonSuffix)
}
