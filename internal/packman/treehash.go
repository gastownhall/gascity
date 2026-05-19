package packman

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const packTreeHashAlgorithm = "gc-pack-tree-sha256-v1"

type treeHashEntry struct {
	path       string
	mode       os.FileMode
	size       int64
	kind       byte
	target     string
	sourcePath string
}

// HashPackTree returns the canonical SHA-256 digest for a pack root.
func HashPackTree(root string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving pack root: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolving pack root symlinks: %w", err)
	}
	st, err := os.Stat(rootReal)
	if err != nil {
		return "", fmt.Errorf("checking pack root: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("pack root %q is not a directory", root)
	}

	entries, err := collectTreeHashEntries(rootReal)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].path < entries[j].path
	})

	h := sha256.New()
	writeTreeHashField(h, packTreeHashAlgorithm)
	for _, entry := range entries {
		switch entry.kind {
		case 'f':
			if err := hashRegularFile(h, entry); err != nil {
				return "", err
			}
		case 'l':
			writeTreeHashField(h, "symlink")
			writeTreeHashField(h, entry.path)
			writeTreeHashField(h, entry.target)
		default:
			return "", fmt.Errorf("unsupported tree hash entry kind %q for %s", entry.kind, entry.path)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func collectTreeHashEntries(root string) ([]treeHashEntry, error) {
	var entries []treeHashEntry
	var walk func(string) error
	walk = func(dir string) error {
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("reading directory %q: %w", dir, err)
		}
		sort.Slice(dirEntries, func(i, j int) bool {
			return dirEntries[i].Name() < dirEntries[j].Name()
		})
		for _, dirEntry := range dirEntries {
			name := dirEntry.Name()
			path := filepath.Join(dir, name)
			rel, err := treeHashRelPath(root, path)
			if err != nil {
				return err
			}
			if ignoreTreeHashPath(rel, dirEntry.Type().IsDir()) {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("checking %q: %w", rel, err)
			}
			mode := info.Mode()
			switch {
			case mode.IsDir():
				if err := walk(path); err != nil {
					return err
				}
			case mode.IsRegular():
				if name == ".git" {
					return fmt.Errorf("rejecting nested git metadata file %q", rel)
				}
				entries = append(entries, treeHashEntry{
					path:       rel,
					mode:       mode,
					size:       info.Size(),
					kind:       'f',
					sourcePath: path,
				})
			case mode&os.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("reading symlink %q: %w", rel, err)
				}
				if err := validateTreeHashSymlink(root, path, rel, target); err != nil {
					return err
				}
				entries = append(entries, treeHashEntry{
					path:   rel,
					kind:   'l',
					target: target,
				})
			default:
				return fmt.Errorf("rejecting unsupported file type %q", rel)
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return entries, nil
}

func hashRegularFile(h io.Writer, entry treeHashEntry) error {
	writeTreeHashField(h, "file")
	writeTreeHashField(h, entry.path)
	writeTreeHashField(h, fmt.Sprintf("%03o", entry.mode.Perm()&0o111))
	writeTreeHashField(h, fmt.Sprintf("%d", entry.size))
	file, err := os.Open(entry.sourcePath)
	if err != nil {
		return fmt.Errorf("reading file %q: %w", entry.path, err)
	}
	defer file.Close()
	if _, err := io.Copy(h, file); err != nil {
		return fmt.Errorf("hashing file %q: %w", entry.path, err)
	}
	writeTreeHashField(h, "")
	return nil
}

func writeTreeHashField(w io.Writer, value string) {
	_, _ = fmt.Fprintf(w, "%d\x00%s\x00", len(value), value)
}

func treeHashRelPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("checking pack boundary for %q: %w", path, err)
	}
	if rel == "." || rel == "" {
		return "", nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("rejecting path outside pack root %q", path)
	}
	return filepath.ToSlash(rel), nil
}

func ignoreTreeHashPath(rel string, isDir bool) bool {
	base := pathBase(rel)
	if base == ".DS_Store" {
		return true
	}
	if isDir && (base == ".git" || base == ".gc") {
		return true
	}
	return isDir && filepath.ToSlash(rel) == "runtime/cache"
}

func pathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func validateTreeHashSymlink(root, linkPath, rel, target string) error {
	if target == "" {
		return fmt.Errorf("rejecting empty symlink target %q", rel)
	}
	targetPath := target
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(filepath.Dir(linkPath), targetPath)
	}
	targetReal, err := filepath.EvalSymlinks(targetPath)
	if err != nil {
		return fmt.Errorf("rejecting unresolved symlink %q -> %q: %w", rel, target, err)
	}
	targetRel, err := filepath.Rel(root, targetReal)
	if err != nil {
		return fmt.Errorf("checking symlink boundary for %q: %w", rel, err)
	}
	if targetRel == ".." || strings.HasPrefix(targetRel, ".."+string(filepath.Separator)) || filepath.IsAbs(targetRel) {
		return fmt.Errorf("rejecting symlink escape %q -> %q", rel, target)
	}
	return nil
}
