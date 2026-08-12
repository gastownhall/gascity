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
	"github.com/gastownhall/gascity/internal/gitcred"
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
		if err := rejectSourceUserinfo(source); err != nil {
			return "", false, err
		}
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

// rejectSourceUserinfo refuses a URL-scheme source that embeds credentials in
// the URL: such a token would leak into city.toml, packs.lock, the shared
// cache's .git/config, the RepoCacheKey, and error output. For http(s)/file it
// rejects any userinfo (user or user:password) — those forms carry no legitimate
// transport identity in the userinfo. For ssh:// it rejects only a
// password-bearing userinfo; ssh://user@ (key auth) and the scp-form
// git@host:org/repo carry transport identity, not a secret, and stay legal. The
// error never echoes the secret (it redacts via gitcred.RedactUserinfo) and is
// returned as ErrInvalidSource by the caller (HTTP 400).
func rejectSourceUserinfo(source string) error {
	source = strings.TrimSpace(source)
	isSSH := strings.HasPrefix(source, "ssh://")
	if !strings.HasPrefix(source, "https://") &&
		!strings.HasPrefix(source, "http://") &&
		!strings.HasPrefix(source, "file://") &&
		!isSSH {
		return nil
	}
	// ssh://user@ is legitimate key-auth identity; only a password is a secret.
	rejectUsernameOnly := !isSSH

	u, err := url.Parse(source)
	if err != nil {
		// A malformed userinfo (invalid %-escape, raw space/^/|) makes url.Parse
		// fail, but the raw token still would leak. Fall back to a string scan so
		// a password-bearing source is still rejected. A username-only malformed
		// source is left to the normal path (nothing to leak beyond the username,
		// which the git seam already redacts).
		_, pass, ok := splitURLUserinfo(source)
		if !ok || pass == "" {
			return nil
		}
		return fmt.Errorf("credentials embedded in the source URL would leak into city.toml, packs.lock, the shared repo cache's .git/config, and error output; remove them and register a credential instead: gc import credential add <host> (source: %s)", gitcred.RedactUserinfo(source))
	}
	if u.User == nil {
		return nil
	}
	redacted := gitcred.RedactUserinfo(source)
	if _, hasPassword := u.User.Password(); hasPassword {
		return fmt.Errorf("credentials embedded in the source URL would leak into city.toml, packs.lock, the shared repo cache's .git/config, and error output; remove them and register a credential instead: gc import credential add <host> (source: %s)", redacted)
	}
	if !rejectUsernameOnly {
		return nil
	}
	return fmt.Errorf("a username embedded in the source URL would leak into city.toml, packs.lock, the shared repo cache's .git/config, and error output; remove it and register a credential instead: gc import credential add <host> (source: %s)", redacted)
}

// splitURLUserinfo extracts the userinfo of a URL by string scan, for sources
// url.Parse rejects. It returns the username, the password (empty if none), and
// whether an authority "@" was found. The userinfo is the authority segment
// before the first "/" of the path and before the "@"; a ":" splits user from
// password.
func splitURLUserinfo(source string) (user, password string, ok bool) {
	sep := strings.Index(source, "://")
	if sep < 0 {
		return "", "", false
	}
	rest := source[sep+3:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	at := strings.LastIndexByte(rest, '@')
	if at < 0 {
		return "", "", false
	}
	userinfo := rest[:at]
	if i := strings.IndexByte(userinfo, ':'); i >= 0 {
		return userinfo[:i], userinfo[i+1:], true
	}
	return userinfo, "", true
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
func defaultHeadCommit(cityRoot, source string) (string, error) {
	cloneURL := config.NormalizeRemoteSource(source)
	inj, err := gitcred.CredentialedNetworkArgs("", cityRoot, cloneURL)
	if err != nil {
		return "", fmt.Errorf("loading git credentials for %s: %w", gitcred.RedactUserinfo(cloneURL), err)
	}
	// inj.CfgArgs go BEFORE the existing args, which already carry the
	// untrusted-remote hardening (lsRemoteHeadArgs).
	cmd := exec.Command("git", append(inj.CfgArgs, lsRemoteHeadArgs(cloneURL)...)...)
	// Strip git-locating env vars so a leaked GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE
	// (or config injection) from a parent pre-commit hook or worktree tooling
	// cannot perturb how this remote HEAD probe runs. The base env stays
	// SanitizedEnv (not HermeticEnv) to preserve today's behavior byte-for-byte;
	// the SanitizedEnv/HermeticEnv asymmetry with the cache clone is a known
	// pre-existing concern flagged for a later unify.
	cmd.Env = append(git.SanitizedEnv(), inj.Env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if authErr := gitcred.ClassifyAuthError(cloneURL, inj, string(out), err); authErr != nil {
			return "", authErr
		}
		return "", fmt.Errorf("resolving HEAD for %q: %w", gitcred.RedactUserinfo(source), err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("resolving HEAD for %q: empty response", gitcred.RedactUserinfo(source))
	}
	return fields[0], nil
}
