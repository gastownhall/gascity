package importsvc

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
)

func deriveImportName(source string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(source, "/"), ".git")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if i := strings.LastIndex(trimmed, ":"); i >= 0 && !strings.Contains(trimmed, string(filepath.Separator)) {
		trimmed = trimmed[i+1:]
	}
	return trimmed
}

func isRemoteImportSource(source string) bool {
	return strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "file://") ||
		strings.HasPrefix(source, "github.com/")
}

func hasRepositoryRefInSource(source string) bool {
	if i := strings.Index(source, "://"); i >= 0 {
		return strings.Contains(source[i+3:], "#")
	}
	return strings.Contains(source, "#")
}

// normalizeImportAddSource canonicalizes the user-supplied source. Remote git
// sources pass through unchanged; local paths are validated as pack targets and
// promoted to file:// repo sources when they sit at the HEAD of a git worktree.
// The boolean reports whether the resolved source is git-backed.
func normalizeImportAddSource(fs fsys.FS, cityPath, source string) (string, bool, error) {
	if isRemoteImportSource(source) {
		return source, true, nil
	}

	targetDir, err := resolveImportAddPath(cityPath, source)
	if err != nil {
		return "", false, err
	}
	if err := validateImportPackTarget(fs, targetDir); err != nil {
		return "", false, err
	}

	canonical, ok, err := canonicalizeLocalGitImportSource(targetDir)
	if err != nil {
		return "", false, err
	}
	if ok {
		return canonical, true, nil
	}
	return source, false, nil
}

func resolveImportAddPath(cityPath, source string) (string, error) {
	switch {
	case strings.HasPrefix(source, "//"):
		return filepath.Join(cityPath, strings.TrimPrefix(source, "//")), nil
	case source == "~" || strings.HasPrefix(source, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(source, "~/")), nil
	case filepath.IsAbs(source):
		return source, nil
	default:
		return filepath.Join(cityPath, source), nil
	}
}

func validateImportPackTarget(fs fsys.FS, targetDir string) error {
	info, err := fs.Stat(targetDir)
	if err != nil {
		return fmt.Errorf("resolving source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory")
	}
	packPath := filepath.Join(targetDir, "pack.toml")
	if _, err := fs.Stat(packPath); err != nil {
		return fmt.Errorf("invalid pack target: missing pack.toml")
	}
	if _, err := config.Load(fs, packPath); err != nil {
		return fmt.Errorf("invalid pack target: %w", err)
	}
	return nil
}

func canonicalizeLocalGitImportSource(targetDir string) (string, bool, error) {
	repoRoot, ok, err := localGitRepoRoot(targetDir)
	if err != nil || !ok {
		return "", ok, err
	}
	resolvedTarget, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		resolvedTarget = targetDir
	}
	rel, err := filepath.Rel(repoRoot, resolvedTarget)
	if err != nil {
		return "", false, fmt.Errorf("computing import subpath: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(repoRoot)}
	canonical := u.String()
	if rel != "." {
		canonical += "//" + filepath.ToSlash(rel)
	}
	return canonical, true, nil
}

func localGitRepoRoot(targetDir string) (string, bool, error) {
	cmd := exec.Command("git", "-C", targetDir, "rev-parse", "--show-toplevel")
	// Strip git-locating env vars (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, ...)
	// so the toplevel resolves from targetDir, not a parent repo leaked through
	// a pre-commit hook or nested worktree tooling.
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := string(out)
		if strings.Contains(text, "not a git repository") {
			return "", false, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("probing git target: %w", err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// lsRemoteHeadArgs builds the `git ls-remote <url> HEAD` argument vector for the
// remote HEAD probe, prefixed with the untrusted-remote hardening overrides so
// the probe cannot follow a redirect off the fenced host or use an unexpected
// transport.
func lsRemoteHeadArgs(cloneURL string) []string {
	args := git.UntrustedRemoteGitConfigArgs()
	return append(args, "ls-remote", cloneURL, "HEAD")
}

// defaultHeadCommit is the single network/git-fetch line for remote HEAD
// resolution. SSRF fencing for the HTTP handler must gate the source string
// before AddImport reaches this probe; the host fence alone is not sufficient,
// so this probe additionally disables HTTP redirect following and constrains
// git transports (git.UntrustedRemoteGitConfigArgs) so a fenced public host
// cannot redirect the probe to an internal target once the URL is shelled to
// git.
func defaultHeadCommit(source string) (string, error) {
	cloneURL := config.NormalizeRemoteSource(source)
	cmd := exec.Command("git", lsRemoteHeadArgs(cloneURL)...)
	// Strip git-locating env vars so a leaked GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE
	// (or config injection) from a parent pre-commit hook or worktree tooling
	// cannot perturb how this remote HEAD probe runs.
	cmd.Env = git.SanitizedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving HEAD for %q: %w", source, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("resolving HEAD for %q: empty response", source)
	}
	return fields[0], nil
}
