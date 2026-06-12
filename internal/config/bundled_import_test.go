package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveLockedRemoteImportAcceptsBundledSyntheticCache(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	got, ok, err := resolveLockedRemoteImport(source, cityDir)
	if err != nil {
		t.Fatalf("resolveLockedRemoteImport: %v", err)
	}
	if !ok {
		t.Fatal("resolveLockedRemoteImport ok = false, want true")
	}
	if got != cacheDir {
		t.Fatalf("cacheDir = %q, want %q", got, cacheDir)
	}
}

func TestResolveLockedRemoteImportRejectsBundledSyntheticContentDrift(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}
	writeTestFile(t, cacheDir, "internal/bootstrap/packs/core/pack.toml", `
[pack]
name = "tampered"
schema = 1
`)

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected synthetic cache content drift error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want synthetic cache content drift", err)
	}
}

func TestResolveLockedRemoteImportRejectsBundledSyntheticExtraFile(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}
	writeTestFile(t, cacheDir, "internal/bootstrap/packs/core/agents/injected/prompt.md", "malicious")

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected synthetic cache extra-file error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("error = %v, want synthetic cache unexpected-file rejection", err)
	}
}

func TestResolveInstalledRemoteImportAcceptsBundledSyntheticCache(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	got, err := resolveInstalledRemoteImport(source, "", cityDir)
	if err != nil {
		t.Fatalf("resolveInstalledRemoteImport: %v", err)
	}
	if got != cacheDir {
		t.Fatalf("cacheDir = %q, want %q", got, cacheDir)
	}
}

func TestResolveImportPackRefAcceptsPublicGastownSyntheticCache(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := PublicGastownPackSource
	commit := strings.TrimPrefix(PublicGastownPackVersion, "sha:")
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	got, err := resolveImportPackRef(source, "", cityDir, cityDir)
	if err != nil {
		t.Fatalf("resolveImportPackRef: %v", err)
	}
	want := filepath.Join(cacheDir, "gastown")
	if got != want {
		t.Fatalf("import path = %q, want %q", got, want)
	}
}

// TestLoadWithIncludes_RigBundledImportSelfHealsOfflineWithoutLock pins the
// rig-scope analog of the city-scope no-lock bundled fallback: a rig-scoped
// [rigs.*.imports.*] entry naming a bundled source at its canonical pin must
// compose from the binary's embedded content when packs.lock is absent,
// exactly like the byte-identical source declared at city scope. It guards
// against rig imports resolving through the legacy resolvePackRef (which only
// consults packs.lock and the retired include cache, then hard-errors) instead
// of the bundled-aware resolveImportPackRef.
func TestLoadWithIncludes_RigBundledImportSelfHealsOfflineWithoutLock(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := PublicGastownPackSource
	commit := strings.TrimPrefix(PublicGastownPackVersion, "sha:")
	cacheDir := bundledRepoCacheDir(home, source, commit)
	if err := builtinpacks.MaterializeSyntheticRepo(cacheDir, commit); err != nil {
		t.Fatalf("materialize synthetic repo: %v", err)
	}

	writeTestFile(t, cityDir, "city.toml", fmt.Sprintf(`
[workspace]
name = "test"

[[rigs]]
name = "proj"
path = "/tmp/proj"

[rigs.imports.gs]
source = %q
version = %q
`, source, PublicGastownPackVersion))

	// Intentionally no packs.lock: the rig-scoped bundled import must
	// self-heal from embedded content offline, just like a city import does
	// before the first `gc import install` writes the lock.
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes with no packs.lock: %v", err)
	}

	var fromBinding int
	for _, a := range explicitAgents(cfg.Agents) {
		if strings.Contains(a.QualifiedName(), "gs.") {
			fromBinding++
		}
	}
	if fromBinding == 0 {
		t.Fatalf("rig bundled import composed no agents under the gs binding offline; agents=%+v", explicitAgents(cfg.Agents))
	}
}

func TestPublicGastownPackSourceMapsToBundledGastownPack(t *testing.T) {
	name, ok := builtinpacks.NameForSource(PublicGastownPackSource)
	if !ok {
		t.Fatalf("PublicGastownPackSource %q is not recognized as a bundled pack source", PublicGastownPackSource)
	}
	if name != "gastown" {
		t.Fatalf("PublicGastownPackSource maps to bundled pack %q, want gastown", name)
	}
}

func TestResolvePackRefServesLockedImportEvenWithGitRef(t *testing.T) {
	// Regression test: a workspace.includes entry with an explicit "#ref"
	// (e.g., "#main") previously bypassed the import lock and required the
	// city-local .gc/cache/includes/ directory to exist. After the fix,
	// resolvePackRef checks the lock first regardless of the gitRef.
	home, cityDir := setupBundledImportTest(t)
	const source = "https://github.com/example/mypack.git"
	const commit = "abc123def456abc123def456abc123def456abc123de"

	// Write packs.lock so the source is treated as installed.
	writeTestFile(t, cityDir, "packs.lock", fmt.Sprintf(`
schema = 1

[packs.%q]
version = "1.0.0"
commit = %q
fetched = "2026-01-01T00:00:00Z"
`, source, commit))

	// Populate the shared repo cache with a fake git checkout.
	cacheRoot := filepath.Join(home, ".gc", "cache", "repos")
	cacheDir := filepath.Join(cacheRoot, RepoCacheKey(source, commit))
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	writeTestFile(t, cacheDir, ".git/index", "idx")
	writeTestFile(t, cacheDir, "pack.toml", "[pack]\nname = \"mypack\"\nschema = 1\n")

	// Stub the git rev-parse / status calls used by validateInstalledRemoteCache.
	orig := runRepoCacheGit
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return commit + "\n", nil
		}
		return "", nil // status --porcelain: clean
	}
	t.Cleanup(func() { runRepoCacheGit = orig })

	// With a "#main" gitRef the old code skipped the lock entirely and
	// went to fetchRemoteInclude (which would fail with "not cached at ...").
	refWithGitRef := source + "#main"

	got, err := resolvePackRef(refWithGitRef, cityDir, cityDir)
	if err != nil {
		t.Fatalf("resolvePackRef(%q): %v", refWithGitRef, err)
	}
	if got != cacheDir {
		t.Fatalf("resolvePackRef = %q, want %q", got, cacheDir)
	}
}

func TestResolveLockedRemoteImportSurfacesInvalidBundledMarker(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", `
schema = 99
repository = "https://github.com/gastownhall/gascity.git"
commit = "abc123def456abc123def456abc123def456abc123de"
content_hash = "sha256:deadbeef"
`)

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected invalid marker error")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "unsupported bundled pack cache marker schema 99") {
		t.Fatalf("error = %v, want invalid marker detail", err)
	}
}

func TestResolveLockedRemoteImportRejectsSyntheticMarkerForNonBundledSource(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := "https://github.com/example/other.git//pack"
	commit := "abc123def456"
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", fmt.Sprintf(`
schema = 1
repository = %q
commit = %q
content_hash = "sha256:deadbeef"
`, source, commit))

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected non-bundled synthetic cache to be rejected")
	}
	if !strings.Contains(err.Error(), "locked but not cached") {
		t.Fatalf("error = %v, want ordinary missing-cache error", err)
	}
}

func TestResolveLockedRemoteImportPrefersGitCacheOverInvalidBundledMarker(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := canonicalBundledCommit(source)
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)
	mustMkdirAll(t, filepath.Join(cacheDir, ".git"), 0o755)
	writeTestFile(t, cacheDir, ".gc-bundled-pack-cache.toml", `
schema = 99
repository = "https://github.com/gastownhall/gascity.git"
commit = "different"
content_hash = "sha256:deadbeef"
`)
	oldRunRepoCacheGit := runRepoCacheGit
	t.Cleanup(func() { runRepoCacheGit = oldRunRepoCacheGit })
	runRepoCacheGit = func(dir string, args ...string) (string, error) {
		if dir != cacheDir {
			t.Fatalf("git dir = %q, want %q", dir, cacheDir)
		}
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			return commit, nil
		case "status --porcelain":
			return "", nil
		default:
			t.Fatalf("unexpected git args %q", strings.Join(args, " "))
			return "", nil
		}
	}

	_, ok, err := resolveLockedRemoteImport(source, cityDir)
	if err != nil {
		t.Fatalf("resolveLockedRemoteImport: %v", err)
	}
	if !ok {
		t.Fatal("resolveLockedRemoteImport ok = false, want true")
	}
}

func TestValidateInstalledRemoteCacheTreatsBundledGitENOTDIRAsNonCheckout(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(cacheDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", cacheDir, err)
	}

	source := bundledPackSource()
	err := validateInstalledRemoteCache(source, cacheDir, canonicalBundledCommit(source))
	if err == nil {
		t.Fatal("validateInstalledRemoteCache accepted file cache")
	}
	if !strings.Contains(err.Error(), "synthetic cache is invalid") || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v, want synthetic validation context", err)
	}
	if strings.Contains(err.Error(), "checking cached import") {
		t.Fatalf("error = %v, want ENOTDIR classified as non-checkout", err)
	}
}

func setupBundledImportTest(t *testing.T) (home, cityDir string) {
	t.Helper()
	dir := t.TempDir()
	home = filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityDir = filepath.Join(dir, "city")
	mustMkdirAll(t, cityDir, 0o755)
	return home, cityDir
}

func bundledPackSource() string {
	source, ok := builtinpacks.Source("core")
	if !ok {
		panic("missing core bundled pack source")
	}
	return source
}

func writeBundledImportLock(t *testing.T, cityDir, source, commit string) {
	t.Helper()
	writeTestFile(t, cityDir, "packs.lock", fmt.Sprintf(`
schema = 1

[packs.%q]
version = "1.2.3"
commit = %q
fetched = "2026-04-10T00:00:00Z"
`, source, commit))
}

func bundledRepoCacheDir(home, source, commit string) string {
	return filepath.Join(home, ".gc", "cache", "repos", RepoCacheKey(source, commit))
}

// canonicalBundledCommit returns the only commit the running binary
// pre-seeds from embedded content for a bundled source. Any other commit
// on a bundled source behaves like an ordinary remote import.
func canonicalBundledCommit(source string) string {
	return strings.TrimPrefix(BundledSourcePinnedVersion(source), "sha:")
}

// TestIsBundledSourceAtCanonicalPin pins the gate that decides whether a
// bundled source is served from the binary's embedded content: only the
// source's canonical pin qualifies. Spellings are normalized per
// builtinpacks.SourceLayout — public gascity-packs sources carry the
// public registry pins, while gascity.git sources (including the legacy
// gascity.git gastown spelling) carry the bundled gascity.git pin.
func TestIsBundledSourceAtCanonicalPin(t *testing.T) {
	coreSource := bundledPackSource()
	gascityGitCommit := strings.TrimPrefix(BundledPackImportVersion, "sha:")
	publicGastownCommit := strings.TrimPrefix(PublicGastownPackVersion, "sha:")
	legacyGastownSource := builtinpacks.MustSource("gastown")

	tests := []struct {
		name   string
		source string
		commit string
		want   bool
	}{
		{"core at canonical gascity.git pin", coreSource, gascityGitCommit, true},
		{"core at non-canonical commit", coreSource, "abc123def456abc123def456abc123def456abc123de", false},
		{"public gastown tree URL at public pin", PublicGastownPackSource, publicGastownCommit, true},
		{"public gastown git subpath spelling at public pin", builtinpacks.PublicRepository + "//gastown", publicGastownCommit, true},
		{"public gastown at gascity.git pin", PublicGastownPackSource, gascityGitCommit, false},
		{"legacy gascity.git gastown at gascity.git pin", legacyGastownSource, gascityGitCommit, true},
		{"legacy gascity.git gastown at public pin", legacyGastownSource, publicGastownCommit, false},
		{"non-bundled URL", "https://github.com/example/other.git//pack", gascityGitCommit, false},
		{"empty commit", coreSource, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBundledSourceAtCanonicalPin(tt.source, tt.commit); got != tt.want {
				t.Fatalf("IsBundledSourceAtCanonicalPin(%q, %q) = %v, want %v", tt.source, tt.commit, got, tt.want)
			}
		})
	}
}

// TestBundledSourcePinnedVersionNormalizesSpellings pins the canonical-pin
// lookup across source spellings: every spelling addressing a pack through
// the public gascity-packs repository resolves to that pack's public
// registry pin, while gascity.git spellings resolve to the bundled
// gascity.git pin.
func TestBundledSourcePinnedVersionNormalizesSpellings(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{"public gastown tree URL", PublicGastownPackSource, PublicGastownPackVersion},
		{"public gastown git subpath spelling", builtinpacks.PublicRepository + "//gastown", PublicGastownPackVersion},
		{"public gascity tree URL", PublicGascityPackSource, PublicGascityPackVersion},
		{"public gascity canonical source", builtinpacks.MustSource("gascity"), PublicGascityPackVersion},
		{"core canonical source", bundledPackSource(), BundledPackImportVersion},
		{"legacy gascity.git gastown spelling", builtinpacks.MustSource("gastown"), BundledPackImportVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BundledSourcePinnedVersion(tt.source); got != tt.want {
				t.Fatalf("BundledSourcePinnedVersion(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}

// TestValidateInstalledRemoteCacheRequiresGitForNonCanonicalBundledPin pins
// the new semantics for bundled sources locked at a non-canonical commit:
// they behave exactly like ordinary remote imports. With no cache present
// the load fails with the regular not-cached error — no synthetic-cache
// language — and the loader must not auto-materialize embedded content.
func TestValidateInstalledRemoteCacheRequiresGitForNonCanonicalBundledPin(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := "abc123def456abc123def456abc123def456abc123de"
	if IsBundledSourceAtCanonicalPin(source, commit) {
		t.Fatalf("test commit %q unexpectedly matches the canonical pin for %q", commit, source)
	}
	writeBundledImportLock(t, cityDir, source, commit)
	cacheDir := bundledRepoCacheDir(home, source, commit)

	_, _, err := resolveLockedRemoteImport(source, cityDir)
	if err == nil {
		t.Fatal("expected non-canonical bundled pin without cache to fail")
	}
	if !strings.Contains(err.Error(), "locked but not cached") || !strings.Contains(err.Error(), `run "gc import install"`) {
		t.Fatalf("error = %v, want regular not-cached error", err)
	}
	if strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("error = %v, must not mention the synthetic cache for a non-canonical pin", err)
	}
	if _, statErr := os.Stat(cacheDir); !os.IsNotExist(statErr) {
		t.Fatalf("cache dir %q exists (stat err = %v); non-canonical bundled pin must not auto-materialize embedded content", cacheDir, statErr)
	}
}

// TestResolveInstalledRemoteImportBundledFallbackWithoutLock pins the
// no-lock self-heal: a bundled source with no packs.lock resolves to the
// binary's canonical pin, hydrating the synthetic cache on demand.
func TestResolveInstalledRemoteImportBundledFallbackWithoutLock(t *testing.T) {
	home, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()
	commit := strings.TrimPrefix(BundledSourcePinnedVersion(source), "sha:")

	got, err := resolveInstalledRemoteImport(source, "", cityDir)
	if err != nil {
		t.Fatalf("resolveInstalledRemoteImport without lock: %v", err)
	}
	want := bundledRepoCacheDir(home, source, commit)
	if got != want {
		t.Fatalf("cacheDir = %q, want %q", got, want)
	}
	if err := builtinpacks.ValidateSyntheticRepo(got, commit); err != nil {
		t.Fatalf("fallback did not hydrate a valid synthetic cache: %v", err)
	}
}

// TestResolveInstalledRemoteImportNonBundledStillRequiresLock pins that the
// no-lock fallback stays scoped to bundled sources.
func TestResolveInstalledRemoteImportNonBundledStillRequiresLock(t *testing.T) {
	_, cityDir := setupBundledImportTest(t)

	_, err := resolveInstalledRemoteImport("https://github.com/example/other.git", "", cityDir)
	if err == nil || !strings.Contains(err.Error(), "missing packs.lock") {
		t.Fatalf("err = %v, want missing packs.lock error", err)
	}
}

// TestResolveInstalledRemoteImportRejectsDeclaredNonCanonicalPinWithoutLock
// pins the declared-version gate: a bundled source declared at a
// non-canonical pin with no lock entry must NOT silently fall back to the
// binary's embedded canonical content — it errors like any other
// uninstalled remote import.
func TestResolveInstalledRemoteImportRejectsDeclaredNonCanonicalPinWithoutLock(t *testing.T) {
	_, cityDir := setupBundledImportTest(t)
	source := bundledPackSource()

	_, err := resolveInstalledRemoteImport(source, "sha:0123456789abcdef0123456789abcdef01234567", cityDir)
	if err == nil || !strings.Contains(err.Error(), "missing packs.lock") {
		t.Fatalf("err = %v, want missing packs.lock error for declared non-canonical pin", err)
	}

	// The canonical declared pin (and an empty declaration) still falls back.
	if _, err := resolveInstalledRemoteImport(source, BundledSourcePinnedVersion(source), cityDir); err != nil {
		t.Fatalf("canonical declared pin should fall back to embedded content: %v", err)
	}
}

// TestSupersededBundledPinErrorsRecommendDoctorFix pins the upgrade
// remediation UX: a city pinned at a SUPERSEDED canonical public pin (one an
// older gc release wrote as canonical) hard-fails config load when nothing
// is cached for that commit. The error must lead with the offline repair
// purpose-built for this case — "gc doctor --fix" re-pins to the current
// canonical and serves embedded content — and keep "gc import install" as
// the exact-commit network fallback. Without the doctor pointer, every
// public-pin bump strands such cities on a network-only resolution path,
// the exact outcome the superseded-pin machinery exists to prevent.
func TestSupersededBundledPinErrorsRecommendDoctorFix(t *testing.T) {
	if len(SupersededPublicGastownPackVersions) == 0 {
		t.Skip("no superseded public gastown pins in this release")
	}
	superseded := SupersededPublicGastownPackVersions[0]
	source, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}

	assertDoctorFirstRemediation := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected superseded pin without cache to fail")
		}
		if !strings.Contains(err.Error(), "superseded canonical") {
			t.Fatalf("error = %v, want the superseded-pin diagnosis", err)
		}
		if !strings.Contains(err.Error(), `run "gc doctor --fix"`) {
			t.Fatalf("error = %v, want gc doctor --fix as the primary (offline) remediation", err)
		}
		if !strings.Contains(err.Error(), `"gc import install"`) {
			t.Fatalf("error = %v, want gc import install as the exact-commit fallback", err)
		}
	}

	t.Run("locked but not cached", func(t *testing.T) {
		_, cityDir := setupBundledImportTest(t)
		writeBundledImportLock(t, cityDir, source, strings.TrimPrefix(superseded, "sha:"))
		_, err := resolveInstalledRemoteImport(source, superseded, cityDir)
		assertDoctorFirstRemediation(t, err)
	})

	t.Run("missing packs.lock", func(t *testing.T) {
		_, cityDir := setupBundledImportTest(t)
		_, err := resolveInstalledRemoteImport(source, superseded, cityDir)
		assertDoctorFirstRemediation(t, err)
	})

	t.Run("missing packs.lock entry", func(t *testing.T) {
		_, cityDir := setupBundledImportTest(t)
		writeBundledImportLock(t, cityDir, "https://github.com/example/other.git", "0123456789abcdef0123456789abcdef01234567")
		_, err := resolveInstalledRemoteImport(source, superseded, cityDir)
		assertDoctorFirstRemediation(t, err)
	})

	// A deliberate user pin at a commit that was never canonical keeps the
	// plain install remediation — the doctor re-pin would not honor it.
	t.Run("non-superseded pin stays plain", func(t *testing.T) {
		_, cityDir := setupBundledImportTest(t)
		_, err := resolveInstalledRemoteImport(source, "sha:0123456789abcdef0123456789abcdef01234567", cityDir)
		if err == nil || !strings.Contains(err.Error(), `run "gc import install"`) {
			t.Fatalf("err = %v, want the plain gc import install remediation", err)
		}
		if strings.Contains(err.Error(), "gc doctor --fix") {
			t.Fatalf("err = %v, must not recommend the doctor re-pin for a deliberate non-canonical pin", err)
		}
	})
}
