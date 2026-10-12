package vacuum

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReferencedByShim reports whether a link in the bin directory points into the
// package directory. aqua's own shims are version independent: they are
// symbolic links to ../aqua-proxy (or hard links to the proxy on Windows), so
// normally they never match. The check protects non-standard links that point
// directly at a file of a specific version, which would dangle after vacuum.
func (c *Client) ReferencedByShim(absPkgPath string) (bool, error) {
	if err := c.scanShimTargets(); err != nil {
		return false, err
	}
	for _, target := range c.shimTargets {
		rel, err := filepath.Rel(absPkgPath, target)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) scanShimTargets() error {
	if c.shimTargets != nil {
		return nil
	}
	c.shimTargets = []string{}
	binDir := filepath.Join(c.rootDir, binDirName)
	entries, err := os.ReadDir(binDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read the bin directory: %w", err)
	}
	for _, entry := range entries {
		p := filepath.Join(binDir, entry.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			// Regular files and hard links can't be reverse-resolved cheaply.
			// aqua creates only proxy hard links, which live outside pkgs.
			continue
		}
		dst, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(dst) {
			dst = filepath.Join(binDir, dst)
		}
		c.shimTargets = append(c.shimTargets, filepath.Clean(dst))
	}
	return nil
}
