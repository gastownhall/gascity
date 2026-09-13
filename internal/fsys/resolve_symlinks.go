package fsys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// maxSymlinkDepth bounds symlink chain resolution, mirroring the kernel's
// nested-link limit so cycles fail instead of looping forever.
const maxSymlinkDepth = 40

// ResolveSymlinks resolves every symlink in path — intermediate directory
// links as well as any chain at the final component — and returns the
// physical path, matching kernel resolution semantics for the active FS.
// Relative link targets are resolved against the physically-resolved
// directory that contains the link, so a `..`-bearing target behind a
// symlinked parent lands where the kernel would land instead of where a
// lexical join would. Components that do not exist are kept as-is (nothing
// below a missing component can be a link), so a missing path or dangling
// link target resolves to the would-be target and callers can create it.
//
// Use this before an atomic temp-file + rename write: renaming over a
// symlink replaces the link itself, so writers that must preserve links
// resolve the target first and write there instead.
func ResolveSymlinks(fs FS, path string) (string, error) {
	if path == "" {
		return path, nil
	}
	orig := path
	sep := string(os.PathSeparator)

	// volLen is the length of the walk anchor that ".." may never climb
	// above: the leading separator for absolute paths, nothing for
	// relative ones.
	volLen := 0
	if os.IsPathSeparator(path[0]) {
		volLen = 1
	}
	dest := path[:volLen]
	linksWalked := 0
	var end int
	for start := volLen; start < len(path); start = end {
		for start < len(path) && os.IsPathSeparator(path[start]) {
			start++
		}
		end = start
		for end < len(path) && !os.IsPathSeparator(path[end]) {
			end++
		}

		// The next path component is in path[start:end].
		if end == start {
			break
		}
		switch path[start:end] {
		case ".":
			continue
		case "..":
			// dest is physical, so its lexical parent is its physical
			// parent. Set r to the index of the last separator in dest;
			// keep ".."s that would climb above the walk anchor.
			var r int
			for r = len(dest) - 1; r >= volLen; r-- {
				if os.IsPathSeparator(dest[r]) {
					break
				}
			}
			if r < volLen || dest[r+1:] == ".." {
				if len(dest) > volLen {
					dest += sep
				}
				dest += ".."
			} else {
				dest = dest[:r]
			}
			continue
		}

		if len(dest) > volLen && !os.IsPathSeparator(dest[len(dest)-1]) {
			dest += sep
		}
		dest += path[start:end]

		info, err := fs.Lstat(dest)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A missing component cannot be a link and cannot hide
				// one below it: keep it so callers can create the path.
				continue
			}
			return "", fmt.Errorf("resolving symlinks for %s: %w", dest, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if !info.IsDir() && end < len(path) {
				return "", fmt.Errorf("resolving symlinks for %s: %w", orig, syscall.ENOTDIR)
			}
			continue
		}

		linksWalked++
		if linksWalked > maxSymlinkDepth {
			return "", fmt.Errorf("resolving symlinks for %s: too many levels of symbolic links", orig)
		}
		rl, ok := fs.(readlinkFS)
		if !ok {
			return "", fmt.Errorf("resolving symlinks for %s: filesystem does not support Readlink", dest)
		}
		target, err := rl.Readlink(dest)
		if err != nil {
			return "", fmt.Errorf("resolving symlinks for %s: %w", dest, err)
		}

		path = target + path[end:]
		if len(target) > 0 && os.IsPathSeparator(target[0]) {
			// Absolute target: restart the walk from the root.
			dest = target[:1]
			volLen = 1
			end = 1
		} else {
			// Relative target: drop the link's own component from dest
			// and continue the walk from its physical parent.
			var r int
			for r = len(dest) - 1; r >= volLen; r-- {
				if os.IsPathSeparator(dest[r]) {
					break
				}
			}
			if r < volLen {
				dest = dest[:volLen]
			} else {
				dest = dest[:r]
			}
			end = 0
		}
	}
	if dest == "" {
		return ".", nil
	}
	return filepath.Clean(dest), nil
}
