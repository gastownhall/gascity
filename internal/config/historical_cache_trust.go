package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// TrustedHistoricalFormulaCacheRoots returns historical cache roots that are
// equivalent to roots derived from the city's currently trusted formula
// layers. It fails closed: malformed lockfiles, non-canonical cache paths,
// dirty checkouts, unrelated repositories, and different pack subpaths all
// produce no roots.
//
// A running workflow retains the absolute asset path resolved when its formula
// was materialized. Pack upgrades move the current formula layer to another
// content-addressed cache directory, but the old checkout remains immutable.
// This resolver admits that old path only when the candidate cache key binds
// the same locked source to its checked-out commit and the path maps to the
// same relative formula/asset root as an active layer.
func TrustedHistoricalFormulaCacheRoots(cityRoot, candidatePath string, formulaSearchPaths []string) []string {
	if strings.TrimSpace(cityRoot) == "" || !filepath.IsAbs(candidatePath) {
		return nil
	}
	cacheRoot, err := GlobalRepoCacheRoot()
	if err != nil {
		return nil
	}
	candidateCache, candidateKey, ok := repoCacheEntryForPath(cacheRoot, candidatePath)
	if !ok {
		return nil
	}

	lockData, err := os.ReadFile(filepath.Join(cityRoot, "packs.lock"))
	if err != nil {
		return nil
	}
	var lock remoteImportLockfile
	if _, err := toml.Decode(string(lockData), &lock); err != nil {
		return nil
	}

	var candidateCommit string
	if err := WithRepoCacheReadLock(cacheRoot, func() error {
		var readErr error
		candidateCommit, readErr = runRepoCacheGit(candidateCache, "rev-parse", "HEAD")
		return readErr
	}); err != nil {
		return nil
	}
	candidateCommit = strings.ToLower(strings.TrimSpace(candidateCommit))
	if !isFullGitObjectID(candidateCommit) {
		return nil
	}

	for source, entry := range lock.Packs {
		if strings.TrimSpace(entry.Commit) == "" || candidateKey != RepoCacheKey(source, candidateCommit) {
			continue
		}
		activeCache := filepath.Join(cacheRoot, RepoCacheKey(source, entry.Commit))
		roots := equivalentHistoricalRoots(activeCache, candidateCache, candidatePath, formulaSearchPaths)
		if len(roots) == 0 {
			continue
		}
		if err := validateInstalledRemoteCacheLocked(source, cacheRoot, activeCache, entry.Commit); err != nil {
			return nil
		}
		if !validateHistoricalCandidate(source, cacheRoot, candidateCache, candidateCommit, candidatePath) {
			return nil
		}
		return roots
	}
	return nil
}

func validateHistoricalCandidate(source, cacheRoot, cacheDir, commit, candidatePath string) bool {
	rel, err := filepath.Rel(cacheDir, candidatePath)
	if err != nil || rel == "." || pathutil.IsOutsideDir(rel) {
		return false
	}
	return WithRepoCacheReadLock(cacheRoot, func() error {
		// Historical caches are dormant rather than actively pinned. Revalidate
		// them on every execution instead of using the current-cache memo: a
		// nested file edit can leave both the cache root and index fingerprint
		// unchanged after an earlier successful validation.
		if err := validateInstalledRemoteCache(source, cacheDir, commit); err != nil {
			return err
		}
		_, err := runRepoCacheGit(cacheDir, "ls-files", "--error-unmatch", "--", filepath.ToSlash(rel))
		return err
	}) == nil
}

func repoCacheEntryForPath(cacheRoot, candidatePath string) (cacheDir, key string, ok bool) {
	cacheRoot = filepath.Clean(cacheRoot)
	candidatePath = filepath.Clean(candidatePath)
	if !pathutil.PathWithin(cacheRoot, candidatePath) {
		return "", "", false
	}
	rel, err := filepath.Rel(cacheRoot, candidatePath)
	if err != nil || rel == "." || pathutil.IsOutsideDir(rel) {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 || !isLowerHex(parts[0], 64) {
		return "", "", false
	}
	cacheDir = filepath.Join(cacheRoot, parts[0])
	if !pathutil.PathWithin(cacheDir, candidatePath) {
		return "", "", false
	}
	return cacheDir, parts[0], true
}

func equivalentHistoricalRoots(activeCache, candidateCache, candidatePath string, formulaSearchPaths []string) []string {
	var roots []string
	add := func(relativeRoot string) {
		root := filepath.Join(candidateCache, relativeRoot)
		if !pathutil.PathWithin(root, candidatePath) {
			return
		}
		for _, existing := range roots {
			if pathutil.SamePath(existing, root) {
				return
			}
		}
		roots = append(roots, root)
	}
	for _, formulaPath := range formulaSearchPaths {
		formulaPath = strings.TrimSpace(formulaPath)
		if formulaPath == "" || !pathutil.PathWithin(activeCache, formulaPath) {
			continue
		}
		rel, err := filepath.Rel(activeCache, filepath.Clean(formulaPath))
		if err != nil || pathutil.IsOutsideDir(rel) {
			continue
		}
		add(rel)
		add(filepath.Join(filepath.Dir(rel), "assets"))
		if filepath.Base(rel) == "formulas" {
			add(filepath.Dir(rel))
		}
	}
	return roots
}

func isFullGitObjectID(value string) bool {
	return isLowerHex(value, 40) || isLowerHex(value, 64)
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
