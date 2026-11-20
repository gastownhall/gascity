package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// validateAbsoluteRigCloneDestination rejects aliases that could change which
// directory an external clone owns. Native platform aliases (/var and /tmp on
// macOS) remain normalized by the existing shared path policy.
func validateAbsoluteRigCloneDestination(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: clone destination must be absolute", configedit.ErrValidation)
	}
	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		if part == ".." {
			return fmt.Errorf("%w: clone destination must not contain parent traversal", configedit.ErrValidation)
		}
	}
	for cur := filepath.Clean(path); ; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%w: resolving clone destination: %w", configedit.ErrValidation, err)
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 && pathutil.NormalizePathForCompare(cur) != cur {
			return fmt.Errorf("%w: clone destination crosses a symlink: %s", configedit.ErrValidation, cur)
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return fmt.Errorf("%w: absolute clone destination requires an existing directory parent", configedit.ErrValidation)
	}
	if _, err := rigCloneDirectoryIdentity(parent); err != nil {
		return fmt.Errorf("%w: absolute clone ownership unavailable: %w", configedit.ErrValidation, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("%w: clone destination already exists; register existing repositories separately", configedit.ErrValidation)
		}
		return fmt.Errorf("%w: checking clone destination: %w", configedit.ErrValidation, err)
	}
	return nil
}

func rigCloneDirectoryIdentity(info os.FileInfo) (string, error) {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("clone destination is not a physical directory")
	}
	return rigCloneNativeDirectoryIdentity(info)
}

// assertProvisionedRigDirectory preserves legacy contained manifests, but an
// external directory requires the exact physical identity recorded after this
// request exclusively created it. Unknown/stale claims never authorize deletion.
func assertProvisionedRigDirectory(city string, m api.RigProvisionManifest) error {
	if m.CreatedDir == "" {
		return nil
	}
	if m.CreatedDirIdentityRequired && m.CreatedDirIdentity == "" {
		return fmt.Errorf("clone directory ownership identity was not persisted; retaining data")
	}
	if m.CreatedDirIdentity == "" {
		return assertRigPathWithinCity(city, m.CreatedDir)
	}
	if !filepath.IsAbs(m.CreatedDir) || filepath.Clean(m.CreatedDir) != m.CreatedDir {
		return fmt.Errorf("refusing noncanonical clone ownership path")
	}
	// Validate ancestors separately: the owned leaf is now expected to exist.
	for cur := filepath.Dir(m.CreatedDir); ; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("resolving owned clone ancestor: %w", err)
		}
		if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("owned clone ancestor is not a directory")
		}
		if info.Mode()&os.ModeSymlink != 0 && pathutil.NormalizePathForCompare(cur) != cur {
			return fmt.Errorf("owned clone ancestor was replaced by a symlink")
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	info, err := os.Lstat(m.CreatedDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := rigCloneDirectoryIdentity(info)
	if err != nil {
		return err
	}
	if identity != m.CreatedDirIdentity {
		return fmt.Errorf("clone directory ownership identity changed; retaining data")
	}
	return nil
}

func removeProvisionedRigDirectory(city string, m api.RigProvisionManifest) error {
	if err := assertProvisionedRigDirectory(city, m); err != nil {
		return err
	}
	if m.CreatedDirIdentity == "" {
		return os.RemoveAll(m.CreatedDir)
	}
	parent, err := os.OpenRoot(filepath.Dir(m.CreatedDir))
	if err != nil {
		return err
	}
	defer parent.Close() //nolint:errcheck // closing a read-only directory handle
	name := filepath.Base(m.CreatedDir)
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := rigCloneDirectoryIdentity(info)
	if err != nil {
		return err
	}
	if identity != m.CreatedDirIdentity {
		return fmt.Errorf("clone directory changed before removal; retaining data")
	}
	return parent.RemoveAll(name)
}
