package builtinpacks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic cache directory is keyed on the normalized clone URL with the
// subpath stripped, and every import resolves to <cache>/<subpath> — never the
// cache root. So a gascity.git cache directory can only ever serve gascity.git
// subpaths. It used to be materialized with EVERY repository's layouts anyway,
// and ValidateSyntheticRepo then byte-compared all of them on every readiness
// pass.
//
// These tests pin that each cache holds and validates only its own
// repository's layouts, and — the part that matters — that the layouts which
// stopped being materialized are REJECTED if present rather than ignored.

// TestMaterializeSyntheticRepoWritesOnlyItsRepositorysLayouts pins the scoping
// in both directions, so a filter that silently matched everything (or nothing)
// fails.
func TestMaterializeSyntheticRepoWritesOnlyItsRepositorysLayouts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repository string
	}{
		{"gascity.git", Repository},
		{"public packs", PublicRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := MaterializeSyntheticRepo(dir, tc.repository, "deadbeef"); err != nil {
				t.Fatalf("MaterializeSyntheticRepo: %v", err)
			}
			for _, layout := range syntheticPackLayouts() {
				path := filepath.Join(dir, filepath.FromSlash(layout.Subpath))
				_, err := os.Stat(path)
				present := err == nil
				want := layout.Repository == tc.repository
				if present != want {
					t.Errorf("subpath %s of %s present=%v, want %v", layout.Subpath, layout.Repository, present, want)
				}
			}
		})
	}
}

// TestMaterializeSyntheticRepoScopingActuallyDropsLayouts guards against the
// filter being a no-op because both repositories happen to hold every layout.
// If this fails, every other test in this file is vacuous.
func TestMaterializeSyntheticRepoScopingActuallyDropsLayouts(t *testing.T) {
	all := len(syntheticPackLayouts())
	own := len(layoutsForRepository(Repository))
	public := len(layoutsForRepository(PublicRepository))
	if own == 0 || public == 0 {
		t.Fatalf("a repository has no layouts (own=%d public=%d); scoping cannot be exercised", own, public)
	}
	if own+public != all {
		t.Fatalf("layouts do not partition by repository: own=%d + public=%d != all=%d", own, public, all)
	}
	if own == all || public == all {
		t.Fatalf("scoping drops nothing: own=%d public=%d all=%d", own, public, all)
	}
}

// TestValidateSyntheticRepoRejectsCrossRepositoryStray is the load-bearing one:
// it distinguishes "correctly scoped" from "silently stopped checking".
//
// A public-repository layout dropped into a gascity.git cache is no longer part
// of that cache's expected file set — so it must be reported as an unexpected
// file, not ignored. Without this assertion, an implementation that narrowed
// the allowed set AND narrowed the walk would look identical to a correct one.
func TestValidateSyntheticRepoRejectsCrossRepositoryStray(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := ValidateSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		t.Fatalf("freshly materialized cache is invalid: %v", err)
	}

	// A subpath that belongs to the OTHER repository.
	var foreign string
	for _, layout := range syntheticPackLayouts() {
		if layout.Repository == PublicRepository {
			foreign = layout.Subpath
			break
		}
	}
	if foreign == "" {
		t.Skip("no public-repository layout registered")
	}
	stray := filepath.Join(dir, filepath.FromSlash(foreign), "pack.toml")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(stray, []byte("[pack]\nname = \"foreign\"\n"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	err := ValidateSyntheticRepo(dir, Repository, "deadbeef")
	if err == nil {
		t.Fatal("a cross-repository layout was accepted; the file-set check no longer covers it")
	}
	if !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("error = %v, want an unexpected-file/directory rejection", err)
	}
}

// TestValidateSyntheticRepoRejectsRepositoryMismatch pins that the caller's
// repository is cross-checked against the marker, exactly as the commit already
// is. Trusting the marker's own value would let a cache materialized for the
// wrong repository validate cleanly and then fail later at import resolution.
func TestValidateSyntheticRepoRejectsRepositoryMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := ValidateSyntheticRepo(dir, PublicRepository, "deadbeef"); err == nil {
		t.Fatal("ValidateSyntheticRepo accepted a mismatched repository")
	}
	if err := ValidateSyntheticRepoFast(dir, PublicRepository, "deadbeef"); err == nil {
		t.Fatal("ValidateSyntheticRepoFast accepted a mismatched repository")
	}
}

// TestValidateSyntheticRepoRejectsSupersededMarkerSchema pins that a cache
// written by an older binary — one whose marker recorded a hardcoded repository
// and whose tree held every repository's layouts — is rejected so it
// re-materializes scoped. That is the ordinary self-heal path, not a migration.
func TestValidateSyntheticRepoRejectsSupersededMarkerSchema(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeSyntheticRepo(dir, Repository, "deadbeef"); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	markerPath := filepath.Join(dir, syntheticMarkerFile)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	downgraded := strings.Replace(string(data), "schema = 2", "schema = 1", 1)
	if downgraded == string(data) {
		t.Fatalf("marker does not declare schema = 2:\n%s", data)
	}
	if err := os.WriteFile(markerPath, []byte(downgraded), 0o644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}

	if err := ValidateSyntheticRepo(dir, Repository, "deadbeef"); err == nil {
		t.Error("ValidateSyntheticRepo accepted a schema-1 marker")
	}
	if err := ValidateSyntheticRepoFast(dir, Repository, "deadbeef"); err == nil {
		t.Error("ValidateSyntheticRepoFast accepted a schema-1 marker")
	}
}

// TestMaterializeSyntheticRepoRejectsUnknownRepository pins that the repository
// is validated rather than used to silently produce an empty cache — a filter
// that matched nothing would otherwise write a marker and no packs, and that
// cache would then validate.
func TestMaterializeSyntheticRepoRejectsUnknownRepository(t *testing.T) {
	dir := t.TempDir()
	err := MaterializeSyntheticRepo(dir, "https://github.com/example/other.git", "deadbeef")
	if err == nil {
		t.Fatal("MaterializeSyntheticRepo accepted an unknown repository")
	}
	if !strings.Contains(err.Error(), "unknown bundled pack repository") {
		t.Errorf("error = %v, want an unknown-repository rejection", err)
	}
}

// TestRepositoryForSourceMapsBothRepositories pins the helper every caller uses
// to derive the repository from the source it already holds, including the fact
// that it keys on the clone URL alone: a subpath that names no bundled pack
// still maps to its repository, because it still maps to that repository's cache
// directory.
func TestRepositoryForSourceMapsBothRepositories(t *testing.T) {
	for _, layout := range syntheticPackLayouts() {
		source := layout.Repository + "//" + layout.Subpath
		got, ok := RepositoryForSource(source)
		if !ok {
			t.Errorf("RepositoryForSource(%q) not recognized", source)
			continue
		}
		if got != layout.Repository {
			t.Errorf("RepositoryForSource(%q) = %q, want %q", source, got, layout.Repository)
		}
	}
	if _, ok := RepositoryForSource("https://github.com/example/other.git//pack"); ok {
		t.Error("RepositoryForSource accepted a non-bundled source")
	}
	// A subpath under a known repository that addresses no bundled pack still
	// resolves: it still lands in that repository's cache directory. Callers
	// that need "is this a bundled pack" gate on IsSource as well.
	nonPack := Repository + "//docs"
	if got, ok := RepositoryForSource(nonPack); !ok || got != Repository {
		t.Errorf("RepositoryForSource(%q) = %q, %v; want %q, true", nonPack, got, ok, Repository)
	}
	if IsSource(nonPack) {
		t.Errorf("IsSource(%q) = true; the two helpers are expected to disagree here", nonPack)
	}
}

// TestSyntheticCacheKeyComponentBindsToMarkerSchema pins the property that
// keeps a rollout from thrashing.
//
// The cache is machine-global and shared by every gc on the box. A marker
// schema change alters the on-disk layout a binary expects WITHOUT altering the
// embedded content that produced it, so two generations would resolve to one
// directory and each reject the other's marker — re-materializing the entire
// tree on every invocation, in both directions, for the length of the rollout.
// The cost is not just the wasted rewrite: each re-materialization takes the
// EXCLUSIVE repo-cache lock, which covers the whole machine-global cache root,
// so every other gc on the box waits behind it.
//
// The component already binds to embedded content for the same reason (see its
// doc comment, which names the side-by-side-deploy wedge). This asserts the
// schema is in there too, so the two generations simply use different
// directories.
func TestSyntheticCacheKeyComponentBindsToMarkerSchema(t *testing.T) {
	component := SyntheticCacheKeyComponent()
	if component == "" {
		t.Fatal("SyntheticCacheKeyComponent is empty; embedded packs cannot be hashed")
	}
	want := fmt.Sprintf("schema%d", syntheticMarkerSchema)
	if !strings.Contains(component, want) {
		t.Errorf("cache key component %q does not encode %q; a schema bump would make two binary generations share one cache directory and re-materialize it on every invocation", component, want)
	}
	hash, err := SyntheticContentHash()
	if err != nil {
		t.Fatalf("SyntheticContentHash: %v", err)
	}
	if !strings.Contains(component, hash) {
		t.Errorf("cache key component %q no longer encodes the content hash %q", component, hash)
	}
}

// TestValidateSyntheticRepoRejectsUnknownRepository pins the read-side guard.
//
// An unknown repository collapses the layout set to empty, so the allowed-path
// set becomes just the marker and the per-pack byte-compare loop never runs — a
// directory holding nothing but a well-formed marker validates clean. That is
// the same hazard MaterializeSyntheticRepo has always rejected on the write
// side; leaving the read side unguarded made a tamper-detection function
// asymmetric.
//
// No production caller can reach it today: all of them derive the repository
// from RepositoryForSource, which only ever returns the two known values. This
// is defense in depth, one future call site away from being live — which is
// exactly the kind of gap that is cheap now and expensive later.
func TestValidateSyntheticRepoRejectsUnknownRepository(t *testing.T) {
	const unknown = "https://github.com/example/other.git"

	// A directory holding only a well-formed marker for an unknown repository:
	// correct content hash, matching commit, current schema.
	dir := t.TempDir()
	hash, err := SyntheticContentHash()
	if err != nil {
		t.Fatalf("SyntheticContentHash: %v", err)
	}
	marker := fmt.Sprintf("schema = %d\nrepository = %q\ncommit = %q\ncontent_hash = %q\n",
		syntheticMarkerSchema, unknown, "deadbeef", hash)
	if err := os.WriteFile(filepath.Join(dir, syntheticMarkerFile), []byte(marker), 0o644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}

	for _, tc := range []struct {
		name     string
		validate func() error
	}{
		{"ValidateSyntheticRepo", func() error { return ValidateSyntheticRepo(dir, unknown, "deadbeef") }},
		{"ValidateSyntheticRepoFast", func() error { return ValidateSyntheticRepoFast(dir, unknown, "deadbeef") }},
	} {
		err := tc.validate()
		if err == nil {
			t.Errorf("%s accepted a pack-less cache for an unknown repository", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "unknown bundled pack repository") {
			t.Errorf("%s error = %v, want an unknown-repository rejection", tc.name, err)
		}
	}
}
