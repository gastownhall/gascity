package formula

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source abstracts how formula files are located and read. The default
// implementation is FSSource, which reads from the working tree (the
// historical behavior). GitRefSource reads from a stable git ref, which
// decouples formula resolution from in-flight code-edit branches.
//
// See gastownhall/gascity#2030 for the bug this abstraction fixes:
// without a ref-stable Source, a polecat working in a bd-managed
// worktree on a `bd-<bead>` branch can produce formula edits that are
// invisible to other sessions reading the same rig path.
type Source interface {
	// Stat reports whether a regular file exists at the given path
	// within the Source. It must not follow into directories: a "found
	// but it's a directory" result reports false.
	Stat(path string) bool

	// ReadFile returns the byte content of the file at the given path.
	ReadFile(path string) ([]byte, error)

	// ListDir returns the base names of files (not subdirectories)
	// directly inside dir. Order is unspecified. Callers that need
	// stable ordering must sort the result.
	ListDir(dir string) ([]string, error)
}

// FSSource reads formula files directly from the local working tree
// via the os.* family. This is the historical (pre-#2030) behavior
// and remains the default to avoid breaking existing dev workflows.
type FSSource struct{}

// Stat returns true when path names a regular file on disk.
func (FSSource) Stat(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// ReadFile reads the file at path from the local filesystem.
func (FSSource) ReadFile(path string) ([]byte, error) {
	// #nosec G304 -- callers pass paths derived from controlled search
	// paths or explicit user input; same trust model as the prior
	// inline os.ReadFile call this method replaces.
	return os.ReadFile(path)
}

// ListDir returns the file-only entries in dir (no subdirectories).
func (FSSource) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// GitRefSource reads formula files from a fixed git ref (e.g. "main")
// in the repository that contains each requested path. Paths that fall
// outside any git repository, or paths that exist on disk but are not
// committed at the configured ref, are reported as absent — the caller
// is expected to fall back to a lower-priority layer (existing
// last-wins resolution semantics handle this naturally).
//
// The implementation shells out to `git` rather than importing go-git
// to avoid adding a heavyweight dependency for a config-resolution
// path that runs at most a handful of times per invocation. Each
// repository toplevel is discovered once via `git rev-parse
// --show-toplevel` and cached for the lifetime of the GitRefSource.
type GitRefSource struct {
	ref string

	// toplevelCache memoizes the result of `git rev-parse
	// --show-toplevel` per directory queried. The zero string sentinel
	// means "queried, not a git repo"; a non-empty string is the
	// repository toplevel.
	toplevelCache map[string]string
}

// NewGitRefSource builds a Source that reads files at the given ref.
// The ref must be non-empty; callers that mean "use working tree"
// should construct FSSource instead.
func NewGitRefSource(ref string) *GitRefSource {
	return &GitRefSource{
		ref:           ref,
		toplevelCache: make(map[string]string),
	}
}

// Ref returns the configured ref. Useful for diagnostics.
func (g *GitRefSource) Ref() string { return g.ref }

// repoTopAndRelPath resolves the git repository toplevel containing
// path and returns (toplevel, relPath, ok). When ok is false, path is
// not inside a git repository tracked by the current environment and
// callers should treat the lookup as a miss.
func (g *GitRefSource) repoTopAndRelPath(path string) (string, string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", false
	}
	queryDir := abs
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		queryDir = filepath.Dir(abs)
	}
	top, cached := g.toplevelCache[queryDir]
	if !cached {
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		cmd.Dir = queryDir
		out, runErr := cmd.Output()
		if runErr != nil {
			g.toplevelCache[queryDir] = ""
			return "", "", false
		}
		top = strings.TrimSpace(string(out))
		g.toplevelCache[queryDir] = top
	}
	if top == "" {
		return "", "", false
	}
	rel, err := filepath.Rel(top, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false
	}
	return top, filepath.ToSlash(rel), true
}

// Stat reports whether a regular blob exists at the configured ref
// for the given path. Returns false for any non-git path (callers
// must compose with a fallback Source if they want filesystem
// resolution for non-git layers).
func (g *GitRefSource) Stat(path string) bool {
	top, rel, ok := g.repoTopAndRelPath(path)
	if !ok {
		return false
	}
	cmd := exec.Command("git", "cat-file", "-e", g.ref+":"+rel)
	cmd.Dir = top
	return cmd.Run() == nil
}

// ReadFile returns the file content at the configured ref.
func (g *GitRefSource) ReadFile(path string) ([]byte, error) {
	top, rel, ok := g.repoTopAndRelPath(path)
	if !ok {
		return nil, fmt.Errorf("read %s @ %s: path is not inside a git repository", path, g.ref)
	}
	cmd := exec.Command("git", "cat-file", "blob", g.ref+":"+rel)
	cmd.Dir = top
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read %s @ %s: %w: %s", path, g.ref, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ListDir returns the file-only entries directly under dir at the
// configured ref. Subtrees are filtered out; symlinks are reported by
// their name (git ls-tree returns blob entries for symlinks). The
// caller is responsible for further filename filtering.
func (g *GitRefSource) ListDir(dir string) ([]string, error) {
	top, rel, ok := g.repoTopAndRelPath(dir)
	if !ok {
		return nil, &os.PathError{Op: "listdir", Path: dir, Err: os.ErrNotExist}
	}
	tree := g.ref + ":" + rel
	if rel == "." {
		tree = g.ref
	}
	cmd := exec.Command("git", "ls-tree", "--name-only", "-z", tree)
	cmd.Dir = top
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// Non-existent directory at this ref: surface as ENOENT so
		// callers handle it identically to a missing filesystem dir.
		return nil, &os.PathError{Op: "listdir", Path: dir, Err: os.ErrNotExist}
	}
	raw := strings.Split(strings.TrimRight(stdout.String(), "\x00"), "\x00")
	out := make([]string, 0, len(raw))
	for _, name := range raw {
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// FallbackSource composes two Sources: it consults the primary first,
// and on a miss (Stat returns false) falls through to the fallback.
// ReadFile honors whichever Source reported Stat==true. ListDir
// concatenates and de-duplicates names from both Sources, primary
// first.
//
// The intended composition for #2030 is GitRefSource + FSSource: read
// committed state from main, but allow callers that explicitly opt
// out (e.g. an admin running `gc formula show` against a feature
// branch) to drop down to working-tree resolution. Because the
// fallback runs on every miss, callers that want strict ref-only
// resolution should pass the GitRefSource directly without wrapping.
type FallbackSource struct {
	Primary  Source
	Fallback Source
}

// Stat reports presence in either Source.
func (f FallbackSource) Stat(path string) bool {
	return f.Primary.Stat(path) || f.Fallback.Stat(path)
}

// ReadFile prefers the primary Source when the file is present there.
func (f FallbackSource) ReadFile(path string) ([]byte, error) {
	if f.Primary.Stat(path) {
		return f.Primary.ReadFile(path)
	}
	return f.Fallback.ReadFile(path)
}

// ListDir returns the union of entries from both Sources, primary
// names first, de-duplicated. An error from the fallback is suppressed
// if the primary succeeded; if both fail, the primary's error wins.
func (f FallbackSource) ListDir(dir string) ([]string, error) {
	primary, primaryErr := f.Primary.ListDir(dir)
	fallback, fallbackErr := f.Fallback.ListDir(dir)
	if primaryErr != nil && fallbackErr != nil {
		return nil, primaryErr
	}
	seen := make(map[string]struct{}, len(primary)+len(fallback))
	out := make([]string, 0, len(primary)+len(fallback))
	for _, name := range primary {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range fallback {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// SourceFromEnv returns the Source implied by the GC_FORMULA_REF
// environment variable:
//
//   - unset, empty, "working-tree", or "HEAD" → FSSource (no
//     behavior change vs. pre-#2030).
//   - any other value → GitRefSource at that ref, with FSSource as
//     a fallback for paths outside any git repository (e.g. ad-hoc
//     test directories or non-checked-in user formula layers).
//
// Default is FSSource (opt-in to ref-stable resolution). Callers that
// want a hard-coded default ref regardless of env should construct
// the Source directly.
func SourceFromEnv() Source {
	ref := strings.TrimSpace(os.Getenv("GC_FORMULA_REF"))
	switch ref {
	case "", "working-tree", "HEAD":
		return FSSource{}
	default:
		return FallbackSource{
			Primary:  NewGitRefSource(ref),
			Fallback: FSSource{},
		}
	}
}
