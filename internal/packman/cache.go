// Package packman resolves, caches, and pins remote pack imports.
package packman

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/remotesource"
)

var (
	runGit                   = defaultRunGit
	runNetworkGit            = defaultRunNetworkGit
	materializeSyntheticRepo = builtinpacks.MaterializeSyntheticRepo
)

// networkGitTimeout bounds every network git invocation.
//
// The bound belongs here rather than at the callers because the invariant it
// protects is packman-local: EnsureRepoInCache clones while holding
// WithRepoCacheWriteLock, so a git that never returns holds the machine-wide
// repo-cache lock forever and every other gc process on the host that touches
// pack state queues behind it. That is ga-r0epd — one wedged remote taking out
// `gc help`, `make test` and the pre-push hook for every agent on the machine.
//
// Ten minutes is deliberately generous: it is far longer than any healthy pack
// clone and short enough that a wedge resolves itself within one coffee break
// instead of never. It is a var so tests can shrink it.
var networkGitTimeout = 10 * time.Minute

// networkGitWaitDelay caps how long the call may stay blocked in CombinedOutput
// after the deadline fires. This is not defensive padding: git's helpers
// (git-remote-http, credential helpers) inherit the output pipes, so a read on
// those pipes can outlive git itself and the deadline alone buys nothing.
//
// WaitDelay closes the parent's ends of those pipes; it does not signal anything
// but the command's own process. Killing the descendants is
// configureNetworkGitDeadline's job, and both are needed: the group kill stops the
// writers, WaitDelay unblocks the reader if one slips through anyway. It is a
// var so tests can shrink it; the real bound on a call is networkGitTimeout +
// networkGitWaitDelay.
var networkGitWaitDelay = 10 * time.Second

// networkGitTerminateGrace is how long git's process group gets to exit on
// SIGTERM before it is SIGKILLed. git removes a partially cloned directory when
// it is interrupted, so asking politely first is what keeps a timed-out clone
// from leaving debris in the repo cache; the escalation is what makes the bound
// hold when it ignores us.
//
// What the grace actually bounds is that removal — an rm -rf of a partial
// clone, which on a multi-GB pack over slow storage can exceed a second or two.
// It is sized for that rather than for process teardown. Getting it wrong costs
// tidiness rather than correctness: debris from a SIGKILL landing mid-removal
// is reclaimed by the next holder of the cache write lock, which RemoveAll's a
// checkout with no completion marker. The cost of the larger value is bounded
// too — worst case 2x this appended to a call that has already waited
// networkGitTimeout, and it elapses before WaitDelay's clock starts.
const networkGitTerminateGrace = 5 * time.Second

// errNetworkGitTimeout marks a network git call killed by networkGitTimeout.
// It is a distinct sentinel because "the deadline fired" and "the remote said
// no" call for opposite responses, and callers that cannot tell them apart
// tend to retry the wrong one.
var errNetworkGitTimeout = errors.New("network git call timed out")

// RepoCacheRoot returns the shared machine-local repo cache root,
// honoring the GC_HOME override via config.GlobalRepoCacheRoot so the
// install and resolve sides always agree on one cache.
func RepoCacheRoot() (string, error) {
	return config.GlobalRepoCacheRoot()
}

// RepoCacheKey returns the canonical source+commit cache key.
// Delegates to config.RepoCacheKey for canonical normalization so
// the loader and packman always agree on cache paths.
func RepoCacheKey(source, commit string) string {
	return config.RepoCacheKey(source, commit)
}

// RepoCachePath returns the cache path for a specific source+commit pair.
func RepoCachePath(source, commit string) (string, error) {
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, RepoCacheKey(source, commit)), nil
}

// EnsureRepoInCache clones and checks out the requested commit when absent,
// or repairs an existing cache whose checkout has drifted from the lock entry.
// cityRoot scopes credential resolution for the network clone (it selects the
// per-city credentials.toml layer); "" skips only that layer.
func EnsureRepoInCache(cityRoot, source, commit string) (string, error) {
	parsed := normalizeRemoteSource(source)
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return "", err
	}
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating repo cache root: %w", err)
	}
	return config.WithRepoCacheWriteLock(root, func() (string, error) {
		if config.IsBundledSourceAtCanonicalPin(source, commit) {
			return ensureBundledRepoInCacheLocked(source, commit, cachePath)
		}
		return ensureRepoInCacheLocked(cityRoot, source, commit, parsed, cachePath)
	})
}

func ensureBundledRepoInCacheLocked(source, commit, cachePath string) (string, error) {
	repository, ok := builtinpacks.RepositoryForSource(source)
	if !ok {
		return "", fmt.Errorf("resolving bundled repository for %q", source)
	}
	validationErr := builtinpacks.ValidateSyntheticRepo(cachePath, repository, commit)
	if validationErr == nil {
		if err := validateCachedPackRoot(source, cachePath); err != nil {
			return "", err
		}
		return cachePath, nil
	}

	recoveryCause := validationErr
	gitInfo, gitErr := os.Stat(filepath.Join(cachePath, ".git"))
	if gitErr == nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
		if err := checkoutExistingCache(cachePath, commit); err == nil {
			if err := validateCachedPackRoot(source, cachePath); err != nil {
				recoveryCause = err
				if removeErr := os.RemoveAll(cachePath); removeErr != nil {
					return "", fmt.Errorf("removing invalid bundled repo cache %q after %w: %w", cachePath, err, removeErr)
				}
			} else {
				return cachePath, nil
			}
		} else {
			recoveryCause = err
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing stale bundled repo cache %q after %w: %w", cachePath, err, removeErr)
			}
		}
	} else if gitErr != nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
		return "", fmt.Errorf("checking bundled repo cache %q: %w", cachePath, gitErr)
	}
	if err := materializeBundledRepoInCacheLocked(source, commit, cachePath); err != nil {
		return "", fmt.Errorf("materializing bundled repo cache %q after %w: %w", cachePath, recoveryCause, err)
	}
	if err := validateCachedPackRoot(source, cachePath); err != nil {
		return "", fmt.Errorf("validating rematerialized bundled repo cache %q after %w: %w", cachePath, recoveryCause, err)
	}
	return cachePath, nil
}

func ensureRepoInCacheLocked(cityRoot, source, commit string, parsed remoteSource, cachePath string) (string, error) {
	if gitInfo, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil && !gitutil.MissingCheckoutMarker(gitInfo, err) {
		if err := checkoutExistingCache(cachePath, commit); err == nil {
			if err := validateCachedPackRoot(source, cachePath); err != nil {
				if removeErr := os.RemoveAll(cachePath); removeErr != nil {
					return "", fmt.Errorf("removing invalid repo cache %q after %w: %w", cachePath, err, removeErr)
				}
			} else {
				return cachePath, nil
			}
		} else if err := os.RemoveAll(cachePath); err != nil {
			return "", fmt.Errorf("removing stale repo cache %q: %w", cachePath, err)
		}
	} else if gitutil.MissingCheckoutMarker(gitInfo, err) {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing invalid repo cache %q: %w", cachePath, removeErr)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("checking repo cache %q: %w", cachePath, statErr)
		}
	} else if err != nil {
		return "", fmt.Errorf("checking repo cache %q: %w", cachePath, err)
	}

	if _, err := runNetworkGit(cityRoot, parsed.CloneURL, "", "clone", "--quiet", parsed.CloneURL, cachePath); err != nil {
		return "", fmt.Errorf("cloning %q: %w", gitcred.RedactUserinfo(source), err)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		return "", fmt.Errorf("checking out %q: %w", commit, err)
	}
	if err := validateCachedPackRoot(source, cachePath); err != nil {
		if removeErr := os.RemoveAll(cachePath); removeErr != nil {
			return "", fmt.Errorf("removing invalid repo cache %q after %w: %w", cachePath, err, removeErr)
		}
		return "", err
	}
	return cachePath, nil
}

func materializeBundledRepoInCacheLocked(source, commit, cachePath string) error {
	expected, err := RepoCachePath(source, commit)
	if err != nil {
		return err
	}
	if cachePath != expected {
		return fmt.Errorf("refusing to materialize bundled repo cache at non-canonical path %q, expected %q", cachePath, expected)
	}
	repository, ok := builtinpacks.RepositoryForSource(source)
	if !ok {
		return fmt.Errorf("resolving bundled repository for %q", source)
	}
	return materializeSyntheticRepo(cachePath, repository, commit)
}

func withRepoCacheReadLock(fn func() error) error {
	root, err := RepoCacheRoot()
	if err != nil {
		return err
	}
	return config.WithRepoCacheReadLock(root, fn)
}

func checkoutExistingCache(cachePath, commit string) error {
	head, headErr := runGit(cachePath, "rev-parse", "HEAD")
	if headErr == nil && gitutil.SameCommit(head, commit) {
		dirty, err := cachedRepoDirty(cachePath)
		if err != nil {
			return err
		}
		if !dirty {
			return nil
		}
		return resetCachedRepo(cachePath, commit)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		if headErr != nil {
			return fmt.Errorf("reading cached repo HEAD: %w; checking out %q: %w", headErr, commit, err)
		}
		return fmt.Errorf("checking out %q in cached repo: %w", commit, err)
	}
	return resetCachedRepo(cachePath, commit)
}

func cachedRepoDirty(cachePath string) (bool, error) {
	// Intentionally NOT --ignored: gitignored build artifacts written into
	// the cache in place (e.g. Python __pycache__/*.pyc from running a cached
	// pack's scripts, or a stray .DS_Store) are not local modifications to the
	// pack's tracked content and must not count as "dirty" — they recur and
	// would otherwise wedge the city behind a perpetual "run gc import install"
	// gate (vp-gny3). A reinstall's `git clean -ffdx` still clears them.
	status, err := runGit(cachePath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("checking cached repo worktree status: %w", err)
	}
	return strings.TrimSpace(status) != "", nil
}

func validateCachedRepoCheckout(cachePath, commit string) error {
	head, err := runGit(cachePath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading cached repo HEAD: %w", err)
	}
	if !gitutil.SameCommit(head, commit) {
		return fmt.Errorf("cached repository is checked out at %s, expected %s", strings.TrimSpace(head), commit)
	}
	dirty, err := cachedRepoDirty(cachePath)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("cached repository has local worktree changes")
	}
	return nil
}

func resetCachedRepo(cachePath, commit string) error {
	if _, err := runGit(cachePath, "reset", "--hard", "--quiet", commit); err != nil {
		return fmt.Errorf("resetting cached repo to %q: %w", commit, err)
	}
	if _, err := runGit(cachePath, "clean", "-ffdx", "--quiet"); err != nil {
		return fmt.Errorf("cleaning cached repo: %w", err)
	}
	return nil
}

func validateCachedPackRoot(source, cachePath string) error {
	packPath := filepath.Join(cachedPackDir(source, cachePath), "pack.toml")
	st, err := os.Stat(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cached pack %q is missing pack.toml at %s", source, packPath)
		}
		return fmt.Errorf("checking cached pack %q at %s: %w", source, packPath, err)
	}
	if st.IsDir() {
		return fmt.Errorf("cached pack %q has directory where pack.toml is expected at %s", source, packPath)
	}
	return nil
}

type remoteSource struct {
	CloneURL string
	Subpath  string
}

func normalizeRemoteSource(source string) remoteSource {
	parsed := remotesource.Parse(source)
	return remoteSource{CloneURL: parsed.CloneURL, Subpath: parsed.Subpath}
}

func defaultRunGit(dir string, args ...string) (string, error) {
	// The pack source URL can be attacker-influenced on the API import path, and
	// this runner drives the network fetch/clone/ls-remote for it. Harden every
	// invocation against redirect-based SSRF and transport abuse; the flags are
	// inert for the local cache operations (rev-parse, checkout, reset, ...) that
	// also flow through here. The remaining DNS-rebinding residual is documented
	// at the pack SSRF fence (internal/api/pack_source_policy.go).
	cmdArgs := append(baseHardeningGitArgs(), args...)
	cmd := exec.Command("git", cmdArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitutil.HermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// baseHardeningGitArgs returns the leading `-c` flags every network git
// invocation carries: the core.* pins plus the untrusted-remote SSRF hardening.
// It is the single source of truth shared by defaultRunGit and
// buildNetworkGitArgs so the "no credential rule ⇒ byte-identical argv"
// guarantee is a slice comparison in tests.
func baseHardeningGitArgs() []string {
	return append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, gitutil.UntrustedRemoteGitConfigArgs()...)
}

// buildNetworkGitArgs assembles the full git argv for a network invocation:
// the base hardening trio, then the credential injection's leading `-c` flags,
// then the subcommand args. With a zero Injection the result is byte-identical
// to defaultRunGit's argv — the byte-identical guarantee for public clones.
func buildNetworkGitArgs(inj gitcred.Injection, args ...string) []string {
	cmdArgs := baseHardeningGitArgs()
	cmdArgs = append(cmdArgs, inj.CfgArgs...)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

// redactNetworkGitArgs renders a git argv for an error message with any
// credential masked. git's argv carries the remote verbatim, so a source with
// userinfo (https://user:token@host/repo) puts the token in every failure line
// this function's callers build — and those errors are logged and surfaced.
//
// The callers' own wraps redact the URL they interpolate, which makes the leak
// easy to miss: the outer label reads "cloning ***@host/repo" while the inner
// string still carries the raw token. Only args are rendered, never
// inj.CfgArgs or inj.Env, which is where the injected credential material
// actually lives.
func redactNetworkGitArgs(args []string) string {
	safe := make([]string, len(args))
	for i, a := range args {
		safe[i] = gitcred.RedactUserinfo(a)
	}
	return strings.Join(safe, " ")
}

// defaultRunNetworkGit is defaultRunGit plus per-invocation credential
// injection and typed auth classification. Every network fetch/clone/ls-remote
// runs through it so a matched credential rule authenticates the call and an
// auth failure surfaces as a typed *gitcred.AuthError. remoteURL is the clone
// URL credential resolution matches on; cityRoot scopes the per-city rule
// layer.
func defaultRunNetworkGit(cityRoot, remoteURL, dir string, args ...string) (string, error) {
	inj, err := gitcred.CredentialedNetworkArgs("", cityRoot, remoteURL)
	if err != nil {
		return "", fmt.Errorf("loading git credentials for %s: %w", gitcred.RedactUserinfo(remoteURL), err)
	}
	cmdArgs := buildNetworkGitArgs(inj, args...)
	ctx, cancel := context.WithTimeout(context.Background(), networkGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(gitutil.HermeticEnv(), inj.Env...)
	// The deadline has to reach git's descendants, not just git. exec's default
	// cancel kills the command's own process, which leaves git-remote-http and
	// index-pack alive — still writing into the cache directory this call is
	// holding the write lock over. The next process to take that lock can then
	// RemoveAll a tree a live orphan is repopulating.
	configureNetworkGitDeadline(cmd)
	cmd.WaitDelay = networkGitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Auth classification runs first, and the deadline is the fallback.
		// ClassifyAuthError needs an *exec.ExitError with code 128 AND a
		// recognized auth diagnostic in the output. A killed process usually
		// exits -1, but a group SIGTERM can also race so that git observes its
		// dead helper and die()s with 128 ("the remote end hung up") before its
		// own signal lands — that exit code alone is not enough to misclassify,
		// because "hung up" matches no auth trigger. So a wedge cannot be read
		// as an auth failure, while the reverse order would be lossy: a real
		// rejection landing in the last milliseconds before the deadline would
		// be reported as a wedge and the credential hint the user needs
		// suppressed.
		if authErr := gitcred.ClassifyAuthError(remoteURL, inj, string(out), err); authErr != nil {
			return "", authErr
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w after %s: %s", redactNetworkGitArgs(args), errNetworkGitTimeout, networkGitTimeout, gitcred.ScrubSecrets(strings.TrimSpace(string(out)), remoteURL))
		}
		return "", fmt.Errorf("git %s: %s: %w", redactNetworkGitArgs(args), gitcred.ScrubSecrets(strings.TrimSpace(string(out)), remoteURL), err)
	}
	return strings.TrimSpace(string(out)), nil
}
