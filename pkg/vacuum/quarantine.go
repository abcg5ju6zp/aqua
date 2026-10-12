package vacuum

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aquaproj/aqua/v2/pkg/osfile"
)

// QuarantineEntry is a path inside the quarantine directory mirroring a
// package version path.
type QuarantineEntry struct {
	// PkgPath is the package-version path relative to $AQUA_ROOT_DIR, using
	// slashes.
	PkgPath string
	IsDir   bool
}

// MoveToQuarantine isolates the package directory from pkgs by renaming it into
// the quarantine directory. The rename is on the same filesystem and atomic;
// the destination directory is created with aqua's group-writable permissions.
func (c *Client) MoveToQuarantine(pkgPath string) error {
	src := c.srcPath(pkgPath)
	dst := c.quarantinePath(pkgPath)
	if err := osfile.MkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("create a quarantine directory: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move a package to the quarantine directory: %w", err)
	}
	return nil
}

// RestoreFromQuarantine moves a quarantined directory back to its original
// location. It is used by crash recovery when the version is referenced again
// by the current configuration.
func (c *Client) RestoreFromQuarantine(pkgPath string) error {
	src := c.quarantinePath(pkgPath)
	dst := c.srcPath(pkgPath)
	if err := osfile.MkdirAll(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("create a package parent directory: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("restore a package from the quarantine directory: %w", err)
	}
	return nil
}

// RemoveQuarantined deletes quarantined content. An error is treated as
// uncertain by callers: the directory may have been deleted partially, so the
// journal is kept and the deletion is retried on the next run. Empty parent
// directories created for the isolated path are pruned in the same run.
func (c *Client) RemoveQuarantined(pkgPath string) error {
	qPath := c.quarantinePath(pkgPath)
	if err := os.RemoveAll(qPath); err != nil {
		return fmt.Errorf("remove a quarantined package: %w", err)
	}
	pruneEmptyParents(filepath.Dir(qPath), filepath.Join(c.rootDir, quarantineDirName))
	return nil
}

// pruneEmptyParents removes empty directories from dir up to (but excluding)
// root. Failures are ignored: a non-empty directory fails the removal, which
// is the desired shared-parent behavior.
func pruneEmptyParents(dir, root string) {
	for dir != root {
		if err := os.Remove(dir); err != nil {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// QuarantineExists reports whether quarantined content of the version exists.
func (c *Client) QuarantineExists(pkgPath string) (bool, error) {
	f, err := osfile.Exists(c.quarantinePath(pkgPath))
	if err != nil {
		return false, fmt.Errorf("check the quarantine directory: %w", err)
	}
	return f, nil
}

// QuarantinePath returns the absolute quarantine path of the package version.
func (c *Client) QuarantinePath(pkgPath string) string {
	return c.quarantinePath(pkgPath)
}

// ListQuarantine lists every file and directory below the quarantine root.
// A missing quarantine directory yields no entries.
func (c *Client) ListQuarantine() ([]QuarantineEntry, error) {
	root := filepath.Join(c.rootDir, quarantineDirName)
	entries := []QuarantineEntry{}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return filepath.SkipDir
			}
			return fmt.Errorf("walk the quarantine directory: %w", err)
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("get a relative quarantine path: %w", err)
		}
		entries = append(entries, QuarantineEntry{
			PkgPath: filepath.ToSlash(rel),
			IsDir:   d.IsDir(),
		})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk the quarantine directory: %w", err)
	}
	return entries, nil
}

// Purge removes every tracking artifact of a package version: the timestamp
// directory, lease files, the journal entry and any quarantined content. It is
// used by "aqua rm" and by vacuum after the quarantined content is deleted.
// Missing artifacts are not errors; other errors are joined and returned.
func (c *Client) Purge(pkgPath string) error {
	var errs []error
	if err := os.RemoveAll(c.dir(pkgPath)); err != nil {
		errs = append(errs, fmt.Errorf("remove a timestamp directory: %w", err))
	}
	if err := c.purgeLeases(pkgPath); err != nil {
		errs = append(errs, err)
	}
	if err := c.RemoveJournal(pkgPath); err != nil {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(c.quarantinePath(pkgPath)); err != nil {
		errs = append(errs, fmt.Errorf("remove quarantined content: %w", err))
	}
	return errors.Join(errs...)
}
