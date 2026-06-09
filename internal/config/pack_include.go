package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

var runRepoCacheGit = defaultRunRepoCacheGit

// includeCacheDir is the subdirectory under .gc/cache/includes/ where
// remote pack includes are cached.
const includeCacheDir = citylayout.CacheIncludesRoot

// isRemoteInclude reports whether s is a remote include URL.
func isRemoteInclude(s string) bool {
	return remotesource.IsRemote(s)
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
	parsed := remotesource.Parse(s)
	return parsed.CloneURL, parsed.Subpath, parsed.Ref
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

// isGitHubTreeURL reports whether s looks like a GitHub tree or blob URL.
// GitHub tree URLs have the format:
//
//	https://github.com/{owner}/{repo}/tree/{ref}[/{path}]
func isGitHubTreeURL(s string) bool {
	return remotesource.IsGitHubTreeOrBlob(s)
}

// parseGitHubTreeURL extracts repo, ref, and subpath from a GitHub tree URL.
//
// Input:  https://github.com/org/repo/tree/v1.0.0/packs/base
// Output: source=https://github.com/org/repo.git, ref=v1.0.0, subpath=packs/base
//
// Limitation: ref is parsed as a single path component. For branches
// with "/" in the name, use the source//subpath#ref format instead.
func parseGitHubTreeURL(s string) (source, subpath, ref string) {
	parsed, ok := remotesource.ParseGitHubTreeOrBlob(s)
	if !ok {
		return s, "", ""
	}
	return parsed.CloneURL, parsed.Subpath, parsed.Ref
}

// resolvePackRef resolves a pack reference to a local directory.
// Handles local paths, GitHub tree URLs, and git source//sub#ref URLs.
func resolvePackRef(ref, declDir, cityRoot string) (string, error) {
	if isGitHubTreeURL(ref) || isRemoteInclude(ref) {
		// parseRemoteInclude handles GitHub tree/blob URLs too
		// (remotesource.Parse short-circuits to ParseGitHubTreeOrBlob),
		// so a single parse covers both remote forms.
		source, subpath, gitRef := parseRemoteInclude(ref)
		// packs.lock is authoritative for any remote source string it
		// records, with or without an embedded ref: gc import install /
		// upgrade already resolved the authored source to a commit and
		// populated the repo cache. Consulting the lock first keeps
		// locked imports (including registry-recommended GitHub tree
		// URLs) resolvable without the legacy include cache, which has
		// no remaining writer.
		if cacheDir, ok, err := resolveLockedRemoteImport(ref, cityRoot); err != nil {
			return "", err
		} else if ok {
			if subpath != "" {
				return filepath.Join(cacheDir, subpath), nil
			}
			return cacheDir, nil
		}
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

type remoteImportLockfile struct {
	Packs map[string]remoteImportLockEntry `toml:"packs"`
}

type remoteImportLockEntry struct {
	Commit string `toml:"commit"`
}

func resolveLockedRemoteImport(source, cityRoot string) (string, bool, error) {
	lockPath := filepath.Join(cityRoot, "packs.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading packs.lock: %w", err)
	}

	var lock remoteImportLockfile
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return "", false, fmt.Errorf("parsing packs.lock: %w", err)
	}
	entry, ok := lock.Packs[source]
	if !ok || entry.Commit == "" {
		return "", false, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolving home dir: %w", err)
	}

	cacheRoot := filepath.Join(home, ".gc", "cache", "repos")
	cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, entry.Commit))
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, entry.Commit); err != nil {
		return "", false, err
	}
	return cacheDir, true, nil
}

func resolveInstalledRemoteImport(source, cityRoot string) (string, error) {
	lockPath := filepath.Join(cityRoot, "packs.lock")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("remote import %s is not installed (missing packs.lock); run \"gc import install\"", source)
		}
		return "", fmt.Errorf("reading packs.lock: %w", err)
	}

	var lock remoteImportLockfile
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return "", fmt.Errorf("parsing packs.lock: %w", err)
	}
	entry, ok := lock.Packs[source]
	if !ok || entry.Commit == "" {
		return "", fmt.Errorf("remote import %s is not installed (missing packs.lock entry); run \"gc import install\"", source)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}

	cacheRoot := filepath.Join(home, ".gc", "cache", "repos")
	cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, entry.Commit))
	if err := validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, entry.Commit); err != nil {
		return "", err
	}
	return cacheDir, nil
}

// remoteCacheValidationCache memoizes successful remote-cache validations. The
// installed remote pack cache is a commit-pinned, gc-managed checkout: validating
// it costs a repo-cache flock plus two git execs (`rev-parse HEAD` and
// `status --porcelain --ignored`, the latter walking the whole tree), and config
// load runs it for every remote import on every reconcile (per rig, per pool).
// Since the cache is immutable for a given commit unless `gc import install`
// rewrites it, cache the success keyed by (cacheDir, commit) + a cheap stat
// fingerprint so a warm cache is revalidated once and reused. Only successes are
// cached; an error re-checks so a repaired cache is picked up immediately.
var remoteCacheValidationCache sync.Map // cacheDir+"\x00"+commit -> remoteCacheValidationEntry

type remoteCacheValidationEntry struct{ fingerprint string }

// remoteCacheFingerprint is a cheap change signal for a remote cache checkout:
// the size+mtime of the checkout root, its .git dir, and the git index. Git
// checkout/status touch .git and the index; `gc import install` rewrites the
// tree. A nested manual worktree edit touching none of these escapes detection
// until the process restarts — acceptable for a pinned, gc-managed cache.
func remoteCacheFingerprint(cacheDir string) string {
	var b strings.Builder
	for _, p := range []string{cacheDir, filepath.Join(cacheDir, ".git"), filepath.Join(cacheDir, ".git", "index")} {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d:%d;", fi.Size(), fi.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	return b.String()
}

// validateInstalledRemoteCacheLocked validates the remote cache under the
// repo-cache read lock, memoizing the success so a warm, unchanged cache skips
// both the flock and the git execs on subsequent loads.
func validateInstalledRemoteCacheLocked(source, cacheRoot, cacheDir, commit string) error {
	key := cacheDir + "\x00" + commit
	fp := remoteCacheFingerprint(cacheDir)
	if v, ok := remoteCacheValidationCache.Load(key); ok {
		if v.(remoteCacheValidationEntry).fingerprint == fp {
			return nil
		}
	}
	if err := WithRepoCacheReadLock(cacheRoot, func() error {
		return validateInstalledRemoteCache(source, cacheDir, commit)
	}); err != nil {
		return err
	}
	remoteCacheValidationCache.Store(key, remoteCacheValidationEntry{fingerprint: fp})
	return nil
}

// ResetRemoteCacheValidationCache clears memoized remote-cache validations
// (test isolation; also lets `gc import install` force revalidation in-process).
func ResetRemoteCacheValidationCache() {
	remoteCacheValidationCache.Range(func(k, _ any) bool {
		remoteCacheValidationCache.Delete(k)
		return true
	})
}

func validateInstalledRemoteCache(source, cacheDir, commit string) error {
	gitPath := filepath.Join(cacheDir, ".git")
	gitInfo, gitStatErr := os.Stat(gitPath)
	if builtinpacks.IsSource(source) {
		err := builtinpacks.ValidateSyntheticRepo(cacheDir, commit)
		if err == nil {
			return nil
		}
		if gitutil.MissingCheckoutMarker(gitInfo, gitStatErr) {
			return fmt.Errorf("remote import %s is locked but synthetic cache is invalid at %s: %w; run \"gc import install\"", source, cacheDir, err)
		}
		if gitStatErr != nil {
			return fmt.Errorf("checking cached import %s: %w; synthetic cache is invalid at %s: %w", source, gitStatErr, cacheDir, err)
		}
		// Synthetic cache is invalid but a real git checkout exists at this
		// path, so validate it with the ordinary remote-cache contract below.
	}
	if gitutil.MissingCheckoutMarker(gitInfo, gitStatErr) {
		return fmt.Errorf("remote import %s is locked but not cached at %s; run \"gc import install\"", source, cacheDir)
	}
	if gitStatErr != nil {
		return fmt.Errorf("checking cached import %s: %w", source, gitStatErr)
	}
	if err := validateLockedRemoteCache(source, cacheDir, commit); err != nil {
		return err
	}
	return nil
}

func validateLockedRemoteCache(source, cacheDir, commit string) error {
	head, err := runRepoCacheGit(cacheDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading cached import %s HEAD: %w", source, err)
	}
	if !gitutil.SameCommit(head, commit) {
		return fmt.Errorf("cached import %s is checked out at %s, expected %s; run \"gc import install\"", source, strings.TrimSpace(head), commit)
	}
	status, err := runRepoCacheGit(cacheDir, "status", "--porcelain", "--ignored")
	if err != nil {
		return fmt.Errorf("checking cached import %s worktree status: %w", source, err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("cached import %s has local worktree changes; run \"gc import install\"", source)
	}
	return nil
}

func defaultRunRepoCacheGit(dir string, args ...string) (string, error) {
	cmdArgs := append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && repoCacheGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

var repoCacheGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_NOSYSTEM":              true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_EXEC_PATH":                    true,
	"GIT_PAGER":                        true,
}

// RepoCacheKey computes the sha256 cache key for a remote source+commit pair.
// This is the canonical implementation — packman.RepoCacheKey must produce
// identical results. Bundled synthetic caches live in a distinct namespace so
// current-binary content never collides with same-repo git checkouts, and they
// additionally fold in the running binary's bundled-pack content hash so two gc
// binaries with different embedded pack content resolve to different cache
// directories instead of fighting over one shared marker (the citywide
// "bundled pack cache content hash does not match current binary" wedge).
func RepoCacheKey(source, commit string) string {
	identity := NormalizeRemoteSource(source) + commit
	if builtinpacks.IsSource(source) {
		identity = builtinpacks.SyntheticCacheNamespace + "\x00" + NormalizeRemoteSource(source) + "\x00" + commit
		if component := builtinpacks.SyntheticCacheKeyComponent(); component != "" {
			identity += "\x00" + component
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", sum[:])
}

// NormalizeRemoteSource extracts the clone URL from a source string,
// stripping subpath and ref suffixes. This is the canonical normalization
// for cache key computation — packman must use the same logic.
func NormalizeRemoteSource(source string) string {
	if !isRemoteRef(source) {
		return source
	}
	return remotesource.Parse(source).CloneURL
}

// fetchRemoteInclude resolves a remote pack include from the local cache.
// The loader is a pure reader: git operations must happen ahead of time.
// Cache location: <cityRoot>/.gc/cache/includes/<cache-name>/
func fetchRemoteInclude(source, ref, cityRoot string) (string, error) {
	cacheName := includeCacheName(source)
	cacheDir := filepath.Join(cityRoot, includeCacheDir, cacheName)

	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); err != nil {
		if os.IsNotExist(err) {
			if ref != "" {
				return "", fmt.Errorf("remote include %s#%s is not cached at %s", source, ref, cacheDir)
			}
			return "", fmt.Errorf("remote include %s is not cached at %s", source, cacheDir)
		}
		return "", fmt.Errorf("checking cached include %s: %w", source, err)
	}

	return cacheDir, nil
}
