package git

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PublishableRemotes returns the names of remotes that can actually publish
// work off this repository, in sorted order.
//
// A remote whose URL resolves to this same repository is excluded. Pushing
// there moves nothing off the host, yet it creates refs/remotes/<name>/*
// entries, so every probe that consults remote-tracking refs — `git branch -r
// --contains`, `git log --not --remotes` — reports the work published. That
// false positive cost this city two keystone fixes, found single-copy on local
// disk after their beads had been closed as published (gas-9sg).
//
// Only self-reference is disqualifying. A remote at another path is a distinct
// repository and a legitimate publication target, whether it is named by a
// plain path or by a loopback URL.
func (g *Git) PublishableRemotes() ([]string, error) {
	out, err := g.run("remote")
	if err != nil {
		return nil, fmt.Errorf("listing remotes: %w", err)
	}
	names := strings.Fields(out)
	if len(names) == 0 {
		return nil, nil
	}
	id, err := g.repoIdentity()
	if err != nil {
		return nil, err
	}

	publishable := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, "-") {
			// A remote name that parses as a git option can never be passed
			// safely to a later probe; treat it as unusable for publication.
			continue
		}
		url, err := g.run("remote", "get-url", name)
		if err != nil {
			// A remote we cannot resolve is not provably a publication target,
			// and counting it would restore the false positive this exists to
			// remove.
			continue
		}
		if !remoteIsPublishable(strings.TrimSpace(url), id) {
			continue
		}
		publishable = append(publishable, name)
	}
	sort.Strings(publishable)
	return publishable, nil
}

// repoIdentity is everything a remote URL must be measured against: the
// directories that identify this repository, and the directory that a relative
// remote URL resolves against.
type repoIdentity struct {
	// dirs identify this repository: the common git directory, shared by every
	// linked worktree, and the main worktree's root.
	dirs []string
	// urlBase is what a relative remote URL resolves against.
	urlBase string
}

// repoIdentity resolves this repository's identity directories and the base for
// relative remote URLs.
//
// A self-referential remote in production names the MAIN repo path while the
// agent works in a linked worktree, so identity comparison uses the common git
// dir and the main worktree root rather than the working directory.
//
// Relative URLs resolve against a different directory than that: git resolves
// them against the worktree root of the repository the command runs in, which
// for a linked worktree is the LINKED worktree's own root, not the main repo's.
// A bare repository has no worktree, and there the git directory is the base.
func (g *Git) repoIdentity() (repoIdentity, error) {
	commonDir, err := g.run("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repoIdentity{}, fmt.Errorf("resolving git common dir: %w", err)
	}
	id := repoIdentity{dirs: []string{strings.TrimSpace(commonDir)}}
	// The main worktree root is the common dir's parent for a standard
	// ".git" layout; a bare or separated git dir simply has no such pairing.
	if base := filepath.Base(id.dirs[0]); base == ".git" {
		id.dirs = append(id.dirs, filepath.Dir(id.dirs[0]))
	}
	if top, err := g.run("rev-parse", "--path-format=absolute", "--show-toplevel"); err == nil {
		id.urlBase = strings.TrimSpace(top)
	}
	if id.urlBase == "" {
		id.urlBase = id.dirs[0]
	}
	return id, nil
}

// remoteIsPublishable reports whether pushing to this remote URL moves work off
// this repository.
//
// It fails safe, because the answer gates destructive cleanup: callers remove
// worktrees once work reads as published, so a remote that is not provably a
// distinct repository must not be counted as one. A URL that names this host
// but cannot be resolved to a directory, and a path that cannot be stat'ed,
// both read as unpublishable — a path that does not resolve cannot have
// received anything.
func remoteIsPublishable(rawURL string, id repoIdentity) bool {
	path, onThisHost := localRemotePath(rawURL)
	if !onThisHost {
		// The URL names another machine, so pushing there publishes off-host
		// by construction and no path comparison applies.
		return true
	}
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		if id.urlBase == "" {
			return false
		}
		path = filepath.Join(id.urlBase, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, dir := range id.dirs {
		selfInfo, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if os.SameFile(info, selfInfo) {
			return false
		}
		// A remote may name the repository through its .git directory
		// (…/repo/.git) while the identity dir is the worktree root, or the
		// reverse. Compare the pairing both ways.
		if gitDir, err := os.Stat(filepath.Join(path, ".git")); err == nil && os.SameFile(gitDir, selfInfo) {
			return false
		}
	}
	return true
}

// localRemotePath extracts the filesystem path a remote URL names on THIS host,
// reporting false when the URL names another machine.
//
// Bare filesystem paths and "file://" URLs are local. So are the loopback forms
// — "ssh://localhost/…", "ssh://127.0.0.1/…", "ssh://[::1]/…" and scp-style
// "user@localhost:/…" — which containerized and sandboxed setups do use: the
// host is this machine, so such a URL can name this very repository. Schemes
// that address a server rather than a filesystem (http, https, git) stay
// off-host even on loopback, because their path is a server route and not a
// directory that can be compared.
//
// A returned path may be relative, in which case it resolves against the
// repository's worktree root, as git resolves it. An empty path with a true
// second result means the URL is on this host but carries no path this process
// can resolve, which the caller treats as not publishable.
func localRemotePath(rawURL string) (string, bool) {
	if rawURL == "" {
		return "", false
	}
	if strings.Contains(rawURL, "://") {
		return schemeRemotePath(rawURL)
	}
	// scp-style syntax is "[user@]host:path"; a Windows drive letter ("C:\…")
	// and an absolute POSIX path never take that shape.
	if idx := strings.Index(rawURL, ":"); idx > 1 {
		host := rawURL[:idx]
		if _, after, found := strings.Cut(host, "@"); found {
			host = after
		}
		if !isLoopbackHost(host) {
			return "", false
		}
		path := rawURL[idx+1:]
		if !filepath.IsAbs(path) {
			// ssh resolves a relative scp path against the remote account's
			// home directory, which this process cannot know. Report the URL as
			// local with no usable path so the caller fails safe.
			return "", true
		}
		return path, true
	}
	return rawURL, true
}

// schemeRemotePath resolves a scheme-bearing remote URL to a path on this host,
// reporting false when the URL names another machine or a server route.
func schemeRemotePath(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		// A URL this process cannot parse is not provably off-host; report it
		// as local with no usable path so the caller fails safe.
		return "", true
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		// An empty authority names the local machine (RFC 8089), and the
		// authority is a host, not the first segment of the path.
		if u.Host != "" && !isLoopbackHost(u.Hostname()) {
			return "", false
		}
		return u.Path, true
	case "ssh", "git+ssh", "ssh+git":
		if !isLoopbackHost(u.Hostname()) {
			return "", false
		}
		return u.Path, true
	default:
		return "", false
	}
}

// isLoopbackHost reports whether a URL host names this same machine.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	// url.Hostname strips the brackets from "[::1]" and an scp-style host never
	// carries them, but trim so both spellings resolve either way.
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
