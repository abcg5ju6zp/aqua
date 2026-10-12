package vacuum

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// PurgePath removes tracking artifacts matching a package glob pattern:
// timestamp directories, lease directories, quarantined content and journal
// entries. The pattern is relative to the package type root and may contain
// filepath.Match metacharacters, e.g. "github_release/github.com/owner/repo".
// It is used by "aqua rm", which removes whole package subtrees by globs.
func (c *Client) PurgePath(logger *slog.Logger, path string) error {
	patterns := []string{
		filepath.Join(c.metadataDir, "pkgs", filepath.FromSlash(path)),
		filepath.Join(c.metadataDir, leasesDirName, "pkgs", filepath.FromSlash(path)),
		filepath.Join(c.rootDir, quarantineDirName, "pkgs", filepath.FromSlash(path)),
	}
	var errs []error
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("find tracking files (%s): %w", pattern, err))
			continue
		}
		for _, match := range matches {
			if err := os.RemoveAll(match); err != nil {
				errs = append(errs, fmt.Errorf("remove tracking files (%s): %w", match, err))
			}
		}
	}

	// Journal keys are hashes, so journal entries are matched by the package
	// path stored in the entry itself rather than by a filename glob.
	journalPattern := filepath.ToSlash(filepath.Join("pkgs", path))
	if err := c.purgeJournals(logger, journalPattern); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Client) purgeJournals(logger *slog.Logger, pattern string) error {
	journals, err := c.ListJournals(logger)
	if err != nil {
		return fmt.Errorf("read the journal: %w", err)
	}
	var errs []error
	for pkgPath := range journals {
		if !matchPackagePath(pattern, pkgPath) {
			continue
		}
		if err := c.RemoveJournal(pkgPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// matchPackagePath reports whether pkgPath equals the pattern or is below it.
// Each pattern segment may contain filepath.Match metacharacters.
func matchPackagePath(pattern, pkgPath string) bool {
	patternSegs := strings.Split(pattern, "/")
	pathSegs := strings.Split(pkgPath, "/")
	if len(pathSegs) < len(patternSegs) {
		return false
	}
	for i, seg := range patternSegs {
		matched, err := filepath.Match(seg, pathSegs[i])
		if err != nil || !matched {
			return false
		}
	}
	return true
}
