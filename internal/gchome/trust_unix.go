//go:build (linux && !android) || (darwin && !ios)

package gchome

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type componentInfo struct {
	uid  uint32
	mode fs.FileMode
}

type componentLstat func(string) (componentInfo, error)

func inspectTrustedProductUsagePath(home, root string) (bool, error) {
	return inspectTrustedProductUsagePathWith(home, root, uint32(os.Geteuid()), lstatComponent)
}

func lstatComponent(path string) (componentInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return componentInfo{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return componentInfo{}, fmt.Errorf("gchome: lstat %q did not expose Unix ownership", path)
	}
	return componentInfo{uid: stat.Uid, mode: info.Mode()}, nil
}

func inspectTrustedProductUsagePathWith(home, root string, effectiveUID uint32, lstat componentLstat) (bool, error) {
	if root != filepath.Join(home, "product-usage") {
		return false, fmt.Errorf("gchome: product root %q is not the direct product-usage child of %q", root, home)
	}
	stickyAwaitingPrivateBoundary := ""
	for prefixIndex, path := range lexicalPathPrefixes(root) {
		info, err := lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			if prefixIndex == 0 {
				return false, fmt.Errorf("gchome: lexical root %q does not exist; no trusted existing ancestor for %q", path, root)
			}
			if stickyAwaitingPrivateBoundary != "" {
				return false, fmt.Errorf("gchome: root-owned sticky ancestor %q has no later existing effective-UID private directory", stickyAwaitingPrivateBoundary)
			}
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("gchome: inspect %q: %w", path, err)
		}
		if info.mode&fs.ModeSymlink != 0 {
			return false, fmt.Errorf("gchome: path component %q is a symlink", path)
		}
		if !info.mode.IsDir() {
			return false, fmt.Errorf("gchome: path component %q is not a directory", path)
		}
		if info.uid != 0 && info.uid != effectiveUID {
			return false, fmt.Errorf("gchome: path component %q is owned by UID %d, want UID 0 or effective UID %d", path, info.uid, effectiveUID)
		}

		if path == home || path == root {
			if info.uid != effectiveUID {
				return false, fmt.Errorf("gchome: private path %q is owned by UID %d, want effective UID %d", path, info.uid, effectiveUID)
			}
			if !privateDirectoryMode(info.mode) {
				return false, fmt.Errorf("gchome: private path %q has mode %s, want 0700-equivalent", path, info.mode)
			}
			if stickyAwaitingPrivateBoundary != "" {
				stickyAwaitingPrivateBoundary = ""
			}
			continue
		}
		if stickyAwaitingPrivateBoundary != "" && info.uid == effectiveUID && privateDirectoryMode(info.mode) {
			stickyAwaitingPrivateBoundary = ""
		}

		if info.mode.Perm()&0o022 == 0 {
			continue
		}
		if info.uid == 0 && info.mode&fs.ModeSticky != 0 {
			stickyAwaitingPrivateBoundary = path
			continue
		}
		return false, fmt.Errorf("gchome: ancestor %q has group/other write permissions in mode %s", path, info.mode)
	}
	if stickyAwaitingPrivateBoundary != "" {
		return false, fmt.Errorf("gchome: root-owned sticky ancestor %q has no later effective-UID private directory", stickyAwaitingPrivateBoundary)
	}
	return false, nil
}

func privateDirectoryMode(mode fs.FileMode) bool {
	const special = fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	return mode.Perm()&0o077 == 0 && mode&special == 0
}

func lexicalPathPrefixes(path string) []string {
	separator := string(filepath.Separator)
	root := separator
	prefixes := []string{root}
	remainder := strings.TrimPrefix(path, root)
	if remainder == "" {
		return prefixes
	}
	current := root
	for _, component := range strings.Split(remainder, separator) {
		current = filepath.Join(current, component)
		prefixes = append(prefixes, current)
	}
	return prefixes
}
