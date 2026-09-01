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
	if pass.Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Fatal("corrupted cache reported valid")
	}

	// Stand in for the repair every caller performs after a negative verdict.
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, builtinpacks.Repository, commit); err != nil {
		t.Fatalf("re-materializing the cache: %v", err)
	}

	if !pass.Valid(cacheDir, builtinpacks.Repository, commit) {
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
	repository, known := builtinpacks.RepositoryForSource(source)
	if !known {
		t.Fatalf("RepositoryForSource(%q): not a bundled source", source)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, repository, commit); err != nil {
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

// TestWarmSyntheticCacheVerifierReusesPositiveVerdictsAcrossPasses pins the
// ready fast path's two halves, and like the pass-scoped test above the
// assertions are deliberately opposed: with no cross-pass memo the first
// fails, and with a memo that never re-validates the second fails.
//
// Half one is the performance contract. Re-reading every cached pack file on
// every config load — and a config load happens on every gc command — is what
// made the ready path cost O(bundled pack files) per command. The only
// corruption the stat fingerprint cannot see is one that preserves BOTH size
// and mtime, so that is what proves the memo was consulted rather than the
// tree re-read. It is also the contract's documented blind spot, stated here
// as a test rather than left to a comment.
//
// packContentHashCache used to make the same trade, but #5367 closed it there
// by adding ctime to that fingerprint — precisely because mtime-preserving
// tooling does exist (cp -p, rsync --checksum --times). So this assertion now
// pins a gap the sibling no longer has, and it should be inverted into a
// self-healing assertion when the ctime follow-up lands here too.
//
// Half two is the self-healing contract that blind spot must not swallow. Any
// ordinary write moves size or mtime, so a later pass still re-runs the full
// validator and reports the corruption for repair.
func TestWarmSyntheticCacheVerifierReusesPositiveVerdictsAcrossPasses(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME, so this cache path is unique to this test
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	pack, ok := builtinpacks.ByName("core")
	if !ok {
		t.Fatal(`builtinpacks.ByName("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	if !newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Fatal("freshly materialized cache reported invalid by the warm verifier")
	}

	target := filepath.Join(cacheDir, filepath.FromSlash(pack.Subpath), "pack.toml")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading cached core pack.toml: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat cached core pack.toml: %v", err)
	}

	// Same length as the original, so only the bytes differ. Flip a bit rather
	// than splice in a chosen byte: the first byte of this file is already "#",
	// so an "overwrite byte 0 with #" corruption reproduces the original
	// exactly and asserts nothing.
	invisible := make([]byte, len(original))
	copy(invisible, original)
	invisible[len(invisible)-1] ^= 0x20
	if len(invisible) != len(original) || string(invisible) == string(original) {
		t.Fatalf("invisible corruption must preserve length (%d vs %d) and change content", len(invisible), len(original))
	}
	if err := os.WriteFile(target, invisible, 0o644); err != nil {
		t.Fatalf("writing size-preserving corruption: %v", err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restoring mtime: %v", err)
	}

	if !newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Error("a later pass re-read the cached tree instead of reusing the memoized verdict; the per-command re-read of every pack file is back")
	}

	// Now an ordinary corruption. Keep it a different length from the original
	// so the fingerprint moves at any timestamp resolution — the same
	// discipline the revision tests apply, and the reason this assertion
	// cannot flake on a coarse-granularity runner.
	visible := append([]byte("[pack]\nname = \"tampered\"\n"), original...)
	if len(visible) == len(original) {
		t.Fatalf("visible corruption must differ in length from the %d-byte original", len(original))
	}
	if err := os.WriteFile(target, visible, 0o644); err != nil {
		t.Fatalf("writing size-changing corruption: %v", err)
	}

	if newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Error("a corrupted cache reported valid; the ready path would never repair it")
	}
}

// firstExecutableCachedFile returns a cached file materialized 0o755, plus its
// pre-modification stat. builtinpacks.MaterializedFileMode gives .sh/.py/.bash
// files that mode, and the bundled bd shim (gc-beads-bd.sh) is exec'd through
// it — so a cache whose executable bit was cleared is a real breakage, not a
// hypothetical one. Located by walk rather than by name so renaming a bundled
// script cannot silently turn this regression test into a no-op.
func firstExecutableCachedFile(t *testing.T, cacheDir string) (string, os.FileInfo) {
	t.Helper()
	var found string
	var info os.FileInfo
	err := filepath.WalkDir(cacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() {
			return err
		}
		fi, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		if fi.Mode().Perm() == 0o755 {
			found, info = path, fi
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking cache for an executable file: %v", err)
	}
	if found == "" {
		t.Fatal("no 0o755 file in the materialized cache; MaterializedFileMode should give .sh/.py/.bash files that mode")
	}
	return found, info
}

// TestWarmSyntheticCacheVerifierNoticesAModeChange pins the first of the two
// gaps between the stat fingerprint and what ValidateSyntheticRepo actually
// rejects.
//
// validatePackFiles fails a cached file whose permission bits differ from the
// embedded copy, but chmod moves ctime — NOT mtime — and leaves size untouched.
// A fingerprint over path+size+mtime alone therefore cannot see it, so a
// stale positive verdict would let the ready path keep serving a cache whose
// bd shim is no longer executable. The memo must not outlive a chmod.
func TestWarmSyntheticCacheVerifierNoticesAModeChange(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME, so this cache path is unique to this test
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	if !newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Fatal("freshly materialized cache reported invalid by the warm verifier")
	}

	target, before := firstExecutableCachedFile(t, cacheDir)
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod cached executable: %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after chmod: %v", err)
	}
	// Guard the premise: if chmod ever moved size or mtime, the existing
	// fingerprint would catch this for the wrong reason and the assertion
	// below would pass vacuously.
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("chmod changed size/mtime (%d/%v -> %d/%v); this test no longer isolates the mode gap",
			before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}

	if newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Errorf("a cache whose %s lost its executable bit reported valid; the stat fingerprint ignores mode, so the ready path would never repair it", filepath.Base(target))
	}
}

// TestWarmSyntheticCacheVerifierNoticesAnUnexpectedDirectory pins the second
// gap. validateSyntheticRepoFileSet rejects any directory outside the layout's
// allowed set, but the fingerprint walk skips directory entries entirely, so an
// added directory changes no hashed entry — it contains no files, and a parent
// directory's own mtime is never hashed. The memo must not outlive it.
func TestWarmSyntheticCacheVerifierNoticesAnUnexpectedDirectory(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME, so this cache path is unique to this test
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal(`builtinpacks.Source("core") is not registered`)
	}
	commit := bundledPackImportCommit()
	cacheDir := materializeSyntheticCacheForTest(t, source, commit)

	if !newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Fatal("freshly materialized cache reported invalid by the warm verifier")
	}

	// Empty on purpose: a directory holding a file would move that file's
	// entry into the hash and pass for the wrong reason.
	stray := filepath.Join(cacheDir, "unexpected-dir")
	if err := os.Mkdir(stray, 0o755); err != nil {
		t.Fatalf("creating unexpected directory: %v", err)
	}

	if newWarmSyntheticCacheVerifier().Valid(cacheDir, builtinpacks.Repository, commit) {
		t.Error("a cache containing an unexpected directory reported valid; the stat fingerprint skips directories, so the ready path would never repair it")
	}
}
