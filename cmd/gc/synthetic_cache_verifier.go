package main

import "github.com/gastownhall/gascity/internal/builtinpacks"

// syntheticCacheVerifier deduplicates builtinpacks.ValidateSyntheticRepo
// within ONE builtin readiness pass.
//
// Every bundled source of a repository resolves to a single synthetic cache
// directory — the cache key strips the subpath — and ValidateSyntheticRepo
// checks every pack layout in that directory regardless of which source asked.
// requiredBuiltinSourcesUsable and lockedBundledImportsUsable therefore ask the
// identical question about the identical directory, and that question is not
// cheap: it os.ReadFile's every cached pack file and compares it against the
// copy embedded in the running binary.
//
// Only POSITIVE verdicts are memoized, and that is what makes the repair paths
// safe: no verdict about a broken cache is ever recorded, so there is nothing
// stale for a caller to observe after it repairs one. A caller that repaired a
// cache necessarily got its negative verdict from a full validation, and the
// next question about that directory runs a full validation too.
//
// A verifier is scoped to one readiness pass and is not safe for concurrent
// use. A verdict therefore never outlives the pass that produced it: every
// pass still runs the full validator at least once, which is what detects and
// repairs a corrupted cache.
type syntheticCacheVerifier struct {
	valid map[string]struct{}
}

// newSyntheticCacheVerifier returns a verifier scoped to a single readiness
// pass.
func newSyntheticCacheVerifier() *syntheticCacheVerifier {
	return &syntheticCacheVerifier{valid: make(map[string]struct{})}
}

// Valid reports whether the synthetic pack cache at cachePath validates for
// commit, reusing a positive verdict already reached in this pass.
func (v *syntheticCacheVerifier) Valid(cachePath, commit string) bool {
	key := cachePath + "\x00" + commit
	if _, ok := v.valid[key]; ok {
		return true
	}
	if builtinpacks.ValidateSyntheticRepo(cachePath, commit) != nil {
		return false
	}
	v.valid[key] = struct{}{}
	return true
}
