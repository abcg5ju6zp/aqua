package checksum

import (
	"fmt"
	"path"
	"strings"

	"github.com/aquaproj/aqua/v2/pkg/config/aqua"
	"github.com/suzuki-shunsuke/slog-error/slogerr"
)

// RegistryID generates a unique identifier for a registry based on its repository information.
// The ID follows the format: registries/github_content/github.com/{owner}/{name}/{ref}/{path}
func RegistryID(regist *aqua.Registry) string {
	return path.Join("registries", "github_content", "github.com", regist.RepoOwner, regist.RepoName, regist.Ref, regist.Path)
}

// VerifyRegistry verifies the integrity of downloaded registry content against
// the checksum record without mutating the checksum collection.
//
// The record is looked up (and, as a side effect, marked as still in use) only
// after the complete content has been downloaded, so an interrupted update can
// never pair a new checksum with stale or partial content. Callers must publish
// the registry content first and only then record the returned checksum with
// CommitRegistry.
//
// When checksums are disabled (checksums == nil), or no record exists yet, the
// content is trusted on first use and the calculated checksum is returned.
func VerifyRegistry(regist *aqua.Registry, checksums *Checksums, content []byte) (string, string, error) {
	algorithm := algoSHA256
	var chksum *Checksum
	if checksums != nil {
		if chk := checksums.Get(RegistryID(regist)); chk != nil {
			chksum = chk
			algorithm = chk.Algorithm
		}
	}
	chk, err := CalculateReader(strings.NewReader(string(content)), algorithm)
	if err != nil {
		return "", "", fmt.Errorf("calculate a checksum: %w", err)
	}
	chk = strings.ToUpper(chk)
	if chksum == nil {
		return algorithm, chk, nil
	}
	if !strings.EqualFold(chksum.Checksum, chk) {
		return "", "", slogerr.With(errInvalidChecksum, //nolint:wrapcheck
			"actual_checksum", chk,
			"expected_checksum", strings.ToUpper(chksum.Checksum),
		)
	}
	return algorithm, chk, nil
}

// CommitRegistry records the checksum of registry content that has already been
// published atomically. It must not be called until the new generation is
// committed, otherwise a crash could leave a new checksum pointing at old
// content.
func CommitRegistry(regist *aqua.Registry, checksums *Checksums, algorithm, chk string) {
	if checksums == nil {
		return
	}
	checksums.Set(RegistryID(regist), &Checksum{
		ID:        RegistryID(regist),
		Algorithm: algorithm,
		Checksum:  chk,
	})
}
