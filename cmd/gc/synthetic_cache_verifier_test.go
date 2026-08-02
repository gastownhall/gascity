package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/packman"
)

// TestReadinessHelpersValidateTheSharedCacheOncePerPass pins the contract the
// verifier exists for, and pins its scope in the same test.
//
// Every bundled source of a repository resolves to ONE synthetic cache
// directory, and ValidateSyntheticRepo checks every pack layout in that
// directory regardless of which source asked. requiredBuiltinSourcesUsable and
// lockedBundledImportsUsable therefore asked the identical question, and that
// question re-reads every cached pack file to compare it against the embedded
// copy.
//
// The two assertions are deliberately opposed: with no memo the first fails,
// with a memo that outlives the pass the second fails. Neither can pass
// vacuously, and the second is the self-healing contract in miniature.
func TestReadinessHelpersValidateTheSharedCacheOncePerPass(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME so the corruption never touches the shared test cache
	city := t.TempDir()

	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	writeBundledSourceImportLock(t, city, source, commit)

	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	pass := newSyntheticCacheVerifier()
	if !requiredBuiltinSourcesUsable(city, pass) {
		t.Fatal("required builtin sources unusable against a freshly materialized cache")
	}
	if !lockedBundledImportsUsable(city, newSyntheticCacheVerifier()) {
		t.Fatal("locked bundled imports unusable against a freshly materialized cache")
	}

	corruptCachedPackFileForTest(t, cacheDir)

	// The required-sources helper already validated this exact directory in
	// this pass. The locked-imports helper must not walk it again — if it
	// does, it sees the corruption and reports unusable.
	if !lockedBundledImportsUsable(city, pass) {
		t.Error("the second readiness helper re-validated a directory the first already validated in the same pass; the duplicate walk is back")
	}

	// A new pass must see the corruption, or nothing would ever self-heal.
	if lockedBundledImportsUsable(city, newSyntheticCacheVerifier()) {
		t.Error("a new pass reused a verdict from a previous one; a corrupted cache would never be repaired")
	}
}

// TestSyntheticCacheVerifierDoesNotMemoizeNegativeVerdicts pins that a failed
// verdict is never remembered. A negative means the caller is about to repair
// the cache, so memoizing it would make the post-repair re-check answer about
// the pre-repair state.
func TestSyntheticCacheVerifierDoesNotMemoizeNegativeVerdicts(t *testing.T) {
	clearGCEnv(t)
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	pass := newSyntheticCacheVerifier()
	corruptCachedPackFileForTest(t, cacheDir)
	if pass.Valid(cacheDir, commit) {
		t.Fatal("corrupted cache reported valid")
	}

	// Stand in for the repair every caller performs after a negative verdict.
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("re-materializing the cache: %v", err)
	}

	if !pass.Valid(cacheDir, commit) {
		t.Error("the verifier memoized a negative verdict; a repaired cache still reads as broken")
	}
}

// TestEnsureRequiredBuiltinSourcesCachedRevalidatesAfterItsOwnRepair walks the
// production repair sequence rather than the verifier in isolation: a helper
// reaches a negative verdict, repairs the cache, and something later in the
// same pass asks about the same directory again. It must see the repaired tree.
//
// This is the sequence that would break if negative verdicts were ever
// memoized, and it holds regardless of the order the required-sources loop
// happens to visit its map in: every entry resolves to the same
// (cacheDir, commit) memo key, so whichever entry takes the repair branch, the
// rest re-derive their verdict from the repaired tree.
func TestEnsureRequiredBuiltinSourcesCachedRevalidatesAfterItsOwnRepair(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	// The order-independence claim above holds only while every required
	// source really does resolve to this one directory. Pin that rather than
	// leave the coverage incidental to the current pack registry.
	for name, required := range requiredBuiltinSources(city) {
		path, err := packman.RepoCachePath(required, commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%q): %v", required, err)
		}
		if path != cacheDir {
			t.Fatalf("required source %q resolves to %s, not the single cache dir %s; this test no longer covers every map ordering", name, path, cacheDir)
		}
	}

	corruptCachedPackFileForTest(t, cacheDir)

	pass := newSyntheticCacheVerifier()
	if requiredBuiltinSourcesUsable(city, pass) {
		t.Fatal("corrupted cache reported usable")
	}
	if err := ensureRequiredBuiltinSourcesCached(city, pass); err != nil {
		t.Fatalf("ensureRequiredBuiltinSourcesCached: %v", err)
	}

	if !requiredBuiltinSourcesUsable(city, pass) {
		t.Error("the pass carried a verdict from before its own repair; a repaired cache still reads as broken")
	}
}

// materializeSyntheticCacheForTest writes the running binary's bundled pack
// tree to the cache directory source+commit resolves to, and returns it.
func materializeSyntheticCacheForTest(t *testing.T, source, commit string) string {
	t.Helper()
	cacheDir, err := packman.RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath(%q): %v", source, err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	return cacheDir
}

// corruptCachedPackFileForTest rewrites one cached pack file so the cache no
// longer matches the content embedded in the running binary. Only the
// whole-tree walk detects this — the cache marker still reads clean.
func corruptCachedPackFileForTest(t *testing.T, cacheDir string) {
	t.Helper()
	pack, ok := builtinpacks.ByName("core")
	if !ok {
		t.Fatal(`builtinpacks.ByName("core") is not registered`)
	}
	target := filepath.Join(cacheDir, filepath.FromSlash(pack.Subpath), "pack.toml")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("cached core pack.toml is missing: %v", err)
	}
	if err := os.WriteFile(target, []byte("[pack]\nname = \"tampered\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("corrupting cached pack file: %v", err)
	}
}

// writeBundledSourceImportLock pins one bundled source in the city's packs.lock
// so lockedBundledCanonicalImports reports it. Unlike writePreflightImportLock
// it takes the source, because these tests need a lock entry that resolves to
// the SAME cache directory as the required builtin sources.
func writeBundledSourceImportLock(t *testing.T, cityPath, source, commit string) {
	t.Helper()
	lockToml := fmt.Sprintf(`schema = 1

[packs.%q]
version = "1.0.0"
commit = %q
fetched = "2026-01-01T00:00:00Z"
`, source, commit)
	if err := os.WriteFile(filepath.Join(cityPath, packman.LockfileName), []byte(lockToml), 0o644); err != nil {
		t.Fatalf("writing packs.lock: %v", err)
	}
}
