// Package remotesource parses remote pack source strings.
package remotesource

import "strings"

// Parsed is the normalized structure of a remote pack source.
type Parsed struct {
	CloneURL   string
	Subpath    string
	Ref        string
	GitHubMode string
}

// Parse splits source into a clone URL, optional subpath, and optional ref.
func Parse(source string) Parsed {
	withoutRef := strings.TrimSpace(source)
	ref := ""
	if i := strings.LastIndex(withoutRef, "#"); i >= 0 {
		ref = withoutRef[i+1:]
		withoutRef = withoutRef[:i]
	}

	if parsed, ok := ParseGitHubTreeOrBlob(withoutRef); ok {
		if ref != "" {
			parsed.Ref = ref
		}
		return parsed
	}

	cloneURL, subpath := splitSourceSubpath(withoutRef)
	if strings.HasPrefix(cloneURL, "github.com/") {
		cloneURL = "https://" + cloneURL
	}
	return Parsed{
		CloneURL: strings.TrimRight(cloneURL, "/"),
		Subpath:  strings.Trim(subpath, "/"),
		Ref:      ref,
	}
}

// IsRemote reports whether source uses one of the supported remote source
// forms.
func IsRemote(source string) bool {
	return strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "file://") ||
		strings.HasPrefix(source, "github.com/")
}

// IsGitHubTreeOrBlob reports whether source is a GitHub tree or blob URL.
func IsGitHubTreeOrBlob(source string) bool {
	_, ok := ParseGitHubTreeOrBlob(source)
	return ok
}

// ParseGitHubTreeOrBlob parses GitHub /tree/ and /blob/ source forms.
func ParseGitHubTreeOrBlob(source string) (Parsed, bool) {
	u := strings.TrimSpace(source)
	if i := strings.LastIndex(u, "#"); i >= 0 {
		u = u[:i]
	}
	scheme := ""
	if idx := strings.Index(u, "://"); idx >= 0 {
		scheme = u[:idx+3]
		u = u[idx+3:]
	}
	parts := strings.SplitN(u, "/", 6)
	if len(parts) < 5 || parts[0] != "github.com" {
		return Parsed{}, false
	}
	mode := parts[3]
	if mode != "tree" && mode != "blob" {
		return Parsed{}, false
	}
	subpath := ""
	if len(parts) > 5 {
		subpath = strings.Trim(parts[5], "/")
	}
	if mode == "blob" {
		subpath = strings.TrimSuffix(subpath, "/pack.toml")
		if subpath == "pack.toml" {
			subpath = ""
		}
	}
	return Parsed{
		CloneURL:   scheme + parts[0] + "/" + parts[1] + "/" + parts[2] + ".git",
		Subpath:    subpath,
		Ref:        parts[4],
		GitHubMode: mode,
	}, true
}

// FormatGitHubTreeSource returns a dereferenceable GitHub tree URL for a pack
// that lives below a GitHub repository root.
func FormatGitHubTreeSource(source, ref, subpath string) (string, bool) {
	source = strings.TrimSpace(source)
	ref = strings.TrimSpace(ref)
	subpath = strings.Trim(strings.TrimSpace(subpath), "/")
	if source == "" || ref == "" || subpath == "" || strings.Contains(ref, "/") {
		return "", false
	}

	if idx := strings.Index(source, "://"); idx >= 0 {
		scheme := source[:idx]
		if scheme != "https" && scheme != "http" {
			return "", false
		}
		source = source[idx+3:]
	}
	source = strings.Trim(source, "/")
	parts := strings.Split(source, "/")
	if len(parts) != 3 || parts[0] != "github.com" {
		return "", false
	}
	owner := parts[1]
	repo := strings.TrimSuffix(parts[2], ".git")
	if owner == "" || repo == "" {
		return "", false
	}
	return "https://github.com/" + owner + "/" + repo + "/tree/" + ref + "/" + subpath, true
}

func splitSourceSubpath(source string) (cloneURL, subpath string) {
	searchFrom := 0
	if idx := strings.Index(source, "://"); idx >= 0 {
		searchFrom = idx + 3
	}
	if i := strings.Index(source[searchFrom:], "//"); i >= 0 {
		pos := searchFrom + i
		return source[:pos], source[pos+2:]
	}
	return source, ""
}
