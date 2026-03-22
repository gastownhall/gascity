package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// includeCacheDir is the subdirectory under .gc/cache/includes/ where
// remote pack includes are cached.
const includeCacheDir = citylayout.CacheIncludesRoot

// isRemoteInclude reports whether s is a remote include URL
// (git@, ssh://, https://, http://, or file://).
func isRemoteInclude(s string) bool {
	return strings.HasPrefix(s, "git@") ||
		strings.HasPrefix(s, "ssh://") ||
		strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "file://")
}

// parseRemoteInclude splits a remote include string into source, subpath,
// and ref components. Format: <source>//<subpath>#<ref>
// Both //subpath and #ref are optional.
//
// Examples:
//
//	"git@github.com:org/repo.git//topo#v1.0" → ("git@github.com:org/repo.git", "topo", "v1.0")
//	"https://github.com/org/repo.git#main"   → ("https://github.com/org/repo.git", "", "main")
//	"git@github.com:org/repo.git"            → ("git@github.com:org/repo.git", "", "")
func parseRemoteInclude(s string) (source, subpath, ref string) {
	// Split off #ref first.
	if i := strings.LastIndex(s, "#"); i >= 0 {
		ref = s[i+1:]
		s = s[:i]
	}

	// Find // for subpath. For URLs with scheme (https://...), we need
	// to find // that is NOT part of the scheme. Search after the scheme.
	searchFrom := 0
	if idx := strings.Index(s, "://"); idx >= 0 {
		searchFrom = idx + 3 // skip past scheme://
	}

	if i := strings.Index(s[searchFrom:], "//"); i >= 0 {
		pos := searchFrom + i
		subpath = s[pos+2:]
		source = s[:pos]
	} else {
		source = s
	}

	return source, subpath, ref
}

// includeCacheName returns a deterministic, human-readable cache directory
// name for a remote include source URL. Format: <slug>-<sha256[:12]>.
// Slug is the last path component of the URL with .git stripped.
func includeCacheName(source string) string {
	// Extract slug: last path component, strip .git suffix.
	slug := source
	// For SSH URLs like git@github.com:org/repo.git, use the part after ':'
	if i := strings.LastIndex(slug, ":"); i >= 0 && !strings.Contains(slug, "://") {
		slug = slug[i+1:]
	}
	// For all URLs, take the last path component.
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	slug = strings.TrimSuffix(slug, ".git")
	if slug == "" {
		slug = "include"
	}

	// Compute short hash for uniqueness.
	h := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%s-%x", slug, h[:6])
}

// isRemoteRef reports whether s is any kind of remote pack reference
// (remote include URL or GitHub tree URL).
func isRemoteRef(s string) bool {
	return isRemoteInclude(s) || isGitHubTreeURL(s)
}

// isGitHubTreeURL reports whether s looks like a GitHub tree URL.
// GitHub tree URLs have the format:
//
//	https://github.com/{owner}/{repo}/tree/{ref}[/{path}]
func isGitHubTreeURL(s string) bool {
	return strings.Contains(s, "github.com/") &&
		strings.Contains(s, "/tree/")
}

// parseGitHubTreeURL extracts repo, ref, and subpath from a GitHub tree URL.
//
// Input:  https://github.com/org/repo/tree/v1.0.0/packs/base
// Output: source=https://github.com/org/repo.git, ref=v1.0.0, subpath=packs/base
//
// Limitation: ref is parsed as a single path component. For branches
// with "/" in the name, use the source//subpath#ref format instead.
func parseGitHubTreeURL(s string) (source, subpath, ref string) {
	// Strip scheme prefix to get the path.
	u := s
	scheme := ""
	if idx := strings.Index(u, "://"); idx >= 0 {
		scheme = u[:idx+3]
		u = u[idx+3:]
	}

	// u is now like: github.com/org/repo/tree/v1.0.0/packs/base
	parts := strings.SplitN(u, "/", 6)
	// parts: [github.com, org, repo, tree, ref, ...subpath]
	if len(parts) < 5 {
		// Malformed — return as-is.
		return s, "", ""
	}

	host := parts[0] // github.com
	owner := parts[1]
	repo := parts[2]
	// parts[3] == "tree"
	ref = parts[4]

	if len(parts) > 5 {
		subpath = parts[5]
	}

	source = scheme + host + "/" + owner + "/" + repo + ".git"
	return source, subpath, ref
}

// resolvePackRef resolves a pack reference to a local directory.
// Handles local paths, GitHub tree URLs, and git source//sub#ref URLs.
func resolvePackRef(ref, declDir, cityRoot string) (string, error) {
	if isGitHubTreeURL(ref) {
		source, subpath, gitRef := parseGitHubTreeURL(ref)
		cacheDir, err := fetchRemoteInclude(source, gitRef, cityRoot)
		if err != nil {
			return "", err
		}
		if subpath != "" {
			return filepath.Join(cacheDir, subpath), nil
		}
		return cacheDir, nil
	}
	if isRemoteInclude(ref) {
		source, subpath, gitRef := parseRemoteInclude(ref)
		cacheDir, err := fetchRemoteInclude(source, gitRef, cityRoot)
		if err != nil {
			return "", err
		}
		if subpath != "" {
			return filepath.Join(cacheDir, subpath), nil
		}
		return cacheDir, nil
	}
	return resolveConfigPath(ref, declDir, cityRoot), nil
}

// fetchRemoteInclude ensures a remote pack include is cached locally.
// Returns the path to the cached pack directory (including subpath
// resolution). Clones on first access, updates on subsequent calls.
// Cache location: <cityRoot>/.gc/cache/includes/<cache-name>/
func fetchRemoteInclude(source, ref, cityRoot string) (string, error) {
	cacheName := includeCacheName(source)
	cacheDir := filepath.Join(cityRoot, includeCacheDir, cacheName)

	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); err != nil {
		// Not yet cloned.
		if err := clonePack(source, cacheDir, ref); err != nil {
			return "", fmt.Errorf("fetching include %s: %w", source, err)
		}
	} else {
		// Already cloned — update.
		if err := updatePack(cacheDir, ref); err != nil {
			return "", fmt.Errorf("updating include %s: %w", source, err)
		}
	}

	return cacheDir, nil
}
