package materialize

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap"
	"github.com/gastownhall/gascity/internal/config"
)

// requireBootstrapNames asserts every name in want exists in the
// production bootstrap pack list. The materialize tests build fake
// implicit-import.toml entries for these names; if a pack is renamed
// or removed in production code, the test helper would silently drop
// the entry from discovery and the assertions would pass vacuously.
// This guard fails loudly instead.
func requireBootstrapNames(t *testing.T, want ...string) {
	t.Helper()
	have := make(map[string]struct{})
	for _, name := range bootstrap.PackNames() {
		have[name] = struct{}{}
	}
	for _, name := range want {
		if _, ok := have[name]; !ok {
			t.Fatalf("test fixture references bootstrap pack %q which is no longer in bootstrap.PackNames() = %v", name, bootstrap.PackNames())
		}
	}
}

func TestReadSkillDescription(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", "---\nname: x\ndescription: A plan\n---\nbody\n", "A plan"},
		{"quoted double", "---\nname: x\ndescription: \"A plan\"\n---\n", "A plan"},
		{"quoted single", "---\nname: x\ndescription: 'A plan'\n---\n", "A plan"},
		{"no frontmatter", "no dashes here\ndescription: ignored\n", ""},
		{"missing description", "---\nname: x\n---\nbody\n", ""},
		{"frontmatter trailing CRLF", "---\r\nname: x\r\ndescription: win\r\n---\r\n", "win"},
		{"description after closing", "---\nname: x\n---\ndescription: outside\n", ""},
		{"empty value", "---\ndescription:\n---\n", ""},
		// Regression for pass-1 Claude review: UTF-8 BOM emitted by
		// Windows editors / some export pipelines must not blind
		// the frontmatter detector.
		{"utf-8 bom", "\ufeff---\nname: x\ndescription: survived bom\n---\n", "survived bom"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got := readSkillDescription(path)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := readSkillDescription(filepath.Join(t.TempDir(), "missing.md")); got != "" {
		t.Errorf("missing file: got %q", got)
	}
}

func TestReadSkillDirPopulatesDescription(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	skillDir := filepath.Join(root, "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: plan\ndescription: Plan the work\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := readSkillDir(root, "city")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Description != "Plan the work" {
		t.Errorf("Description = %q, want %q", entries[0].Description, "Plan the work")
	}
}

func TestVendorSink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider string
		wantSink string
		wantOK   bool
	}{
		{"claude", ".claude/skills", true},
		{"codex", ".codex/skills", true},
		{"gemini", ".gemini/skills", true},
		{"opencode", ".opencode/skills", true},
		{"copilot", "", false},
		{"cursor", "", false},
		{"pi", "", false},
		{"omp", "", false},
		{"unknown", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.provider, func(t *testing.T) {
			t.Parallel()
			sink, ok := VendorSink(c.provider)
			if sink != c.wantSink || ok != c.wantOK {
				t.Fatalf("VendorSink(%q) = (%q, %v), want (%q, %v)", c.provider, sink, ok, c.wantSink, c.wantOK)
			}
		})
	}
}

func TestSupportedVendorsStable(t *testing.T) {
	t.Parallel()
	got := SupportedVendors()
	want := []string{"claude", "codex", "gemini", "opencode"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedVendors() = %v, want %v", got, want)
	}
}

func TestReadSkillDirEnumerates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkSkill(t, root, "alpha")
	mkSkill(t, root, "beta")
	// Non-skill: no SKILL.md.
	if err := os.MkdirAll(filepath.Join(root, "no-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Non-skill: regular file at the root.
	if err := os.WriteFile(filepath.Join(root, "loose.md"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := readSkillDir(root, "city")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "alpha" || entries[1].Name != "beta" {
		t.Fatalf("want [alpha beta], got %+v", entries)
	}
	for _, e := range entries {
		if e.Origin != "city" {
			t.Errorf("Origin = %q, want %q", e.Origin, "city")
		}
		if !filepath.IsAbs(e.Source) {
			t.Errorf("Source = %q, want absolute path", e.Source)
		}
	}
}

func TestReadSkillDirMissingReturnsNil(t *testing.T) {
	t.Parallel()
	entries, err := readSkillDir(filepath.Join(t.TempDir(), "missing"), "x")
	if err != nil || entries != nil {
		t.Fatalf("missing dir: got (%v, %v), want (nil, nil)", entries, err)
	}
}

func TestLoadCityCatalogEmptyAndIsolated(t *testing.T) {
	// Hermetic: clear GC_HOME so bootstrap discovery is a no-op.
	t.Setenv("GC_HOME", "")
	cat, err := LoadCityCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 0 || len(cat.OwnedRoots) != 0 || len(cat.Shadowed) != 0 {
		t.Fatalf("empty catalog: got %+v", cat)
	}
}

func TestLoadCityCatalogCityOnly(t *testing.T) {
	t.Setenv("GC_HOME", "")
	pack := t.TempDir()
	skillsDir := filepath.Join(pack, "skills")
	mkSkill(t, skillsDir, "alpha")
	mkSkill(t, skillsDir, "beta")

	cat, err := LoadCityCatalog(skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(cat.Entries))
	}
	for _, e := range cat.Entries {
		if e.Origin != "city" {
			t.Errorf("Origin = %q, want city", e.Origin)
		}
	}
	if len(cat.OwnedRoots) != 1 {
		t.Fatalf("want 1 owned root, got %v", cat.OwnedRoots)
	}
}

func TestLoadCityCatalogBootstrapMerge(t *testing.T) {
	requireBootstrapNames(t, "core", "registry")
	gcHome := setupBootstrapHome(t, map[string][]string{
		"core":     {"alpha", "shared"},
		"registry": {"reg-only"},
	})
	t.Setenv("GC_HOME", gcHome)

	pack := t.TempDir()
	cityDir := filepath.Join(pack, "skills")
	mkSkill(t, cityDir, "city-only")
	mkSkill(t, cityDir, "shared") // collides with core/shared — city must win

	cat, err := LoadCityCatalog(cityDir)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]string, len(cat.Entries))
	for _, e := range cat.Entries {
		got[e.Name] = e.Origin
	}
	want := map[string]string{
		"alpha":     "core",
		"shared":    "city",
		"reg-only":  "registry",
		"city-only": "city",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries: got %v, want %v", got, want)
	}

	if len(cat.Shadowed) != 1 || cat.Shadowed[0].Name != "shared" || cat.Shadowed[0].Winner != "city" || cat.Shadowed[0].Loser != "core" {
		t.Fatalf("shadowed: %+v", cat.Shadowed)
	}

	// City root + every bootstrap pack root that contributed.
	if len(cat.OwnedRoots) < 3 {
		t.Fatalf("want >=3 owned roots (city + 2 bootstrap), got %v", cat.OwnedRoots)
	}
}

func TestLoadCityCatalogImportedPackSkills(t *testing.T) {
	t.Setenv("GC_HOME", "")
	cityPack := t.TempDir()
	importedPack := t.TempDir()

	cityDir := filepath.Join(cityPack, "skills")
	importedDir := filepath.Join(importedPack, "skills")
	mkSkill(t, cityDir, "city-only")
	mkSkill(t, importedDir, "plan")

	cat, err := LoadCityCatalog(cityDir, config.DiscoveredSkillCatalog{
		SourceDir:   importedDir,
		PackDir:     importedPack,
		PackName:    "tools",
		BindingName: "ops",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]string, len(cat.Entries))
	for _, e := range cat.Entries {
		got[e.Name] = e.Origin
	}
	want := map[string]string{
		"city-only": "city",
		"ops.plan":  "ops",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries: got %v, want %v", got, want)
	}

	if len(cat.OwnedRoots) != 2 {
		t.Fatalf("owned roots = %v, want 2 roots", cat.OwnedRoots)
	}
}

func TestLoadCityCatalogPreservesOwnedRootsOnReadError(t *testing.T) {
	t.Setenv("GC_HOME", "")
	pack := t.TempDir()
	skillsDir := filepath.Join(pack, "skills")
	mkSkill(t, skillsDir, "alpha")

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	cat, err := LoadCityCatalog(skillsDir)
	if err == nil {
		t.Fatal("LoadCityCatalog should fail when skills dir is unreadable")
	}
	if len(cat.OwnedRoots) != 1 {
		t.Fatalf("owned roots = %v, want 1 root preserved on error", cat.OwnedRoots)
	}
	wantRoot, absErr := filepath.Abs(skillsDir)
	if absErr != nil {
		t.Fatal(absErr)
	}
	if cat.OwnedRoots[0] != wantRoot {
		t.Fatalf("owned root = %q, want %q", cat.OwnedRoots[0], wantRoot)
	}
}

func TestLoadCityCatalogPreservesLaterImportedOwnedRootsOnEarlyReadError(t *testing.T) {
	t.Setenv("GC_HOME", "")
	laterDir := filepath.Join(t.TempDir(), "later", "skills")
	mkSkill(t, laterDir, "beta")

	tooLongDir := filepath.Join(t.TempDir(), strings.Repeat("x", 5000))
	cat, err := LoadCityCatalog("",
		config.DiscoveredSkillCatalog{
			SourceDir:   tooLongDir,
			BindingName: "broken",
		},
		config.DiscoveredSkillCatalog{
			SourceDir:   laterDir,
			BindingName: "later",
		},
	)
	if err == nil {
		t.Fatal("LoadCityCatalog should fail when an imported skills dir cannot be stated")
	}

	wantLater, absErr := filepath.Abs(laterDir)
	if absErr != nil {
		t.Fatal(absErr)
	}
	if !slices.Contains(cat.OwnedRoots, wantLater) {
		t.Fatalf("owned roots = %v, want later imported root %q preserved", cat.OwnedRoots, wantLater)
	}
}

func TestLoadCityCatalogIgnoresUnknownImplicitImport(t *testing.T) {
	requireBootstrapNames(t, "core")
	gcHome := setupBootstrapHome(t, map[string][]string{
		"core": {"alpha"},
	})
	// Add a non-bootstrap import — must not contribute, per spec.
	implicit := filepath.Join(gcHome, "implicit-import.toml")
	data, err := os.ReadFile(implicit)
	if err != nil {
		t.Fatal(err)
	}
	extraSkills := filepath.Join(gcHome, "cache", "repos", "stranger", "skills")
	mkSkill(t, extraSkills, "stranger-skill")
	appended := string(data) + "\n[imports.\"stranger\"]\nsource = \"stranger-source\"\nversion = \"0.1.0\"\ncommit = \"deadbeef\"\n"
	if err := os.WriteFile(implicit, []byte(appended), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_HOME", gcHome)
	cat, err := LoadCityCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cat.Entries {
		if e.Name == "stranger-skill" {
			t.Fatalf("non-bootstrap import contributed entry: %+v", e)
		}
	}
}

func TestLoadAgentCatalogEmpty(t *testing.T) {
	t.Parallel()
	cat, err := LoadAgentCatalog("")
	if err != nil || cat.OwnedRoot != "" || len(cat.Entries) != 0 {
		t.Fatalf("empty agent catalog: got %+v err %v", cat, err)
	}
}

func TestLoadAgentCatalogLists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mkSkill(t, dir, "private")
	cat, err := LoadAgentCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].Name != "private" || cat.Entries[0].Origin != "agent" {
		t.Fatalf("agent catalog: %+v", cat)
	}
	abs, _ := filepath.Abs(dir)
	if cat.OwnedRoot != abs {
		t.Fatalf("OwnedRoot = %q, want %q", cat.OwnedRoot, abs)
	}
}

func TestEffectiveSetAgentLocalWins(t *testing.T) {
	t.Parallel()
	city := CityCatalog{
		Entries: []SkillEntry{
			{Name: "alpha", Source: "/city/skills/alpha", Origin: "city"},
			{Name: "shared", Source: "/city/skills/shared", Origin: "city"},
		},
	}
	agent := AgentCatalog{
		Entries: []SkillEntry{
			{Name: "shared", Source: "/agent/skills/shared", Origin: "agent"},
			{Name: "private", Source: "/agent/skills/private", Origin: "agent"},
		},
	}
	got := EffectiveSet(city, agent)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d (%+v)", len(got), got)
	}
	byName := make(map[string]SkillEntry, len(got))
	for _, e := range got {
		byName[e.Name] = e
	}
	if byName["shared"].Origin != "agent" || byName["shared"].Source != "/agent/skills/shared" {
		t.Errorf("agent did not win on shared: %+v", byName["shared"])
	}
	if byName["alpha"].Origin != "city" || byName["private"].Origin != "agent" {
		t.Errorf("non-collision wrong: alpha=%+v private=%+v", byName["alpha"], byName["private"])
	}
}

func TestMaterializeAgentCreatesSink(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "alpha")
	sink := filepath.Join(t.TempDir(), "deeply", "nested", "skills")

	res, err := Run(Request{
		SinkDir:    sink,
		Desired:    []SkillEntry{{Name: "alpha", Source: filepath.Join(src, "alpha"), Origin: "city"}},
		OwnedRoots: []string{src},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Materialized, []string{"alpha"}) {
		t.Fatalf("materialized = %v", res.Materialized)
	}
	target, err := os.Readlink(filepath.Join(sink, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(src, "alpha") {
		t.Fatalf("target = %q, want %q", target, filepath.Join(src, "alpha"))
	}
}

func TestMaterializeAgentIdempotent(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "alpha")
	sink := filepath.Join(t.TempDir(), "skills")
	desired := []SkillEntry{{Name: "alpha", Source: filepath.Join(src, "alpha"), Origin: "city"}}

	for i := 0; i < 3; i++ {
		res, err := Run(Request{
			SinkDir:    sink,
			Desired:    desired,
			OwnedRoots: []string{src},
		})
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if !reflect.DeepEqual(res.Materialized, []string{"alpha"}) {
			t.Fatalf("pass %d materialized = %v", i, res.Materialized)
		}
		if len(res.Skipped) != 0 || len(res.LegacyMigrated) != 0 || len(res.Warnings) != 0 {
			t.Fatalf("pass %d non-empty side channels: %+v", i, res)
		}
	}
}

// TestMaterializeAgentDecisionMatrix exercises the seven-row safety
// matrix from engdocs/proposals/skill-materialization.md.
func TestMaterializeAgentDecisionMatrix(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "keep")
	mkSkill(t, src, "drift-to")
	mkSkill(t, src, "drift-from")
	mkSkill(t, src, "external-name")
	mkSkill(t, src, "user-file-name")
	mkSkill(t, src, "user-dir-name")

	external := t.TempDir()
	mkSkill(t, external, "external-target")

	sink := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing entries representing each row of the matrix.

	// Row 1: symlink, gc-managed, desired, target matches → Keep.
	mustSymlink(t, filepath.Join(src, "keep"), filepath.Join(sink, "keep"))

	// Row 2: symlink, gc-managed, desired, target drifted → atomic replace.
	mustSymlink(t, filepath.Join(src, "drift-from"), filepath.Join(sink, "drift"))

	// Row 3: symlink, gc-managed, NOT desired → delete.
	mustSymlink(t, filepath.Join(src, "drift-from"), filepath.Join(sink, "orphan"))

	// Row 4: symlink, gc-managed, dangling → delete. Use a path that
	// IS under the owned root but does not exist on disk.
	mustSymlink(t, filepath.Join(src, "this-skill-was-removed"), filepath.Join(sink, "dangling"))

	// Row 5: symlink, target external → leave alone.
	mustSymlink(t, filepath.Join(external, "external-target"), filepath.Join(sink, "external-name"))

	// Row 6: regular file → leave alone (also blocks any matching desired entry).
	if err := os.WriteFile(filepath.Join(sink, "user-file-name"), []byte("user content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Row 7: regular directory → leave alone.
	if err := os.MkdirAll(filepath.Join(sink, "user-dir-name"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sink, "user-dir-name", "note.md"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}

	desired := []SkillEntry{
		{Name: "keep", Source: filepath.Join(src, "keep"), Origin: "city"},
		{Name: "drift", Source: filepath.Join(src, "drift-to"), Origin: "city"},
		{Name: "external-name", Source: filepath.Join(src, "external-name"), Origin: "city"},
		{Name: "user-file-name", Source: filepath.Join(src, "user-file-name"), Origin: "city"},
		{Name: "user-dir-name", Source: filepath.Join(src, "user-dir-name"), Origin: "city"},
		// Note: 'orphan' and 'dangling' are intentionally NOT desired.
	}

	res, err := Run(Request{
		SinkDir:    sink,
		Desired:    desired,
		OwnedRoots: []string{src},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Row 1: keep present, untouched.
	checkSymlink(t, filepath.Join(sink, "keep"), filepath.Join(src, "keep"))

	// Row 2: drift atomically replaced.
	checkSymlink(t, filepath.Join(sink, "drift"), filepath.Join(src, "drift-to"))

	// Row 3: orphan deleted.
	if _, err := os.Lstat(filepath.Join(sink, "orphan")); !os.IsNotExist(err) {
		t.Errorf("orphan symlink still present (err=%v)", err)
	}

	// Row 4: dangling deleted.
	if _, err := os.Lstat(filepath.Join(sink, "dangling")); !os.IsNotExist(err) {
		t.Errorf("dangling symlink still present (err=%v)", err)
	}

	// Row 5: external-target symlink preserved.
	tgt, err := os.Readlink(filepath.Join(sink, "external-name"))
	if err != nil {
		t.Errorf("external symlink read: %v", err)
	}
	if tgt != filepath.Join(external, "external-target") {
		t.Errorf("external symlink overwritten: target=%q", tgt)
	}

	// Row 6: regular file preserved, desired entry recorded as Skipped.
	body, err := os.ReadFile(filepath.Join(sink, "user-file-name"))
	if err != nil || string(body) != "user content" {
		t.Errorf("user file modified: body=%q err=%v", string(body), err)
	}

	// Row 7: regular dir preserved.
	body, err = os.ReadFile(filepath.Join(sink, "user-dir-name", "note.md"))
	if err != nil || string(body) != "user" {
		t.Errorf("user dir modified: body=%q err=%v", string(body), err)
	}

	// Sanity: results contain exactly the right names. Materialized
	// includes keep + drift; external-name was preserved as a
	// user-owned symlink and is therefore Skipped.
	wantMaterialized := []string{"drift", "keep"}
	if !reflect.DeepEqual(res.Materialized, wantMaterialized) {
		t.Errorf("materialized = %v, want %v", res.Materialized, wantMaterialized)
	}
	skippedNames := skippedToNames(res.Skipped)
	wantSkipped := []string{"external-name", "user-dir-name", "user-file-name"}
	if !reflect.DeepEqual(skippedNames, wantSkipped) {
		t.Errorf("skipped = %v, want %v", skippedNames, wantSkipped)
	}
}

func TestMaterializeAgentLegacyStubMigratedThenSymlinked(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "gc-work")
	sink := filepath.Join(t.TempDir(), "claude", "skills")
	if err := os.MkdirAll(filepath.Join(sink, "gc-work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sink, "gc-work", "SKILL.md"),
		[]byte(legacyStubBodies["gc-work"]), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Request{
		SinkDir:     sink,
		Desired:     []SkillEntry{{Name: "gc-work", Source: filepath.Join(src, "gc-work"), Origin: "core"}},
		OwnedRoots:  []string{src},
		LegacyNames: LegacyStubNames(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.LegacyMigrated, []string{"gc-work"}) {
		t.Fatalf("LegacyMigrated = %v", res.LegacyMigrated)
	}
	if !reflect.DeepEqual(res.Materialized, []string{"gc-work"}) {
		t.Fatalf("Materialized = %v", res.Materialized)
	}
	checkSymlink(t, filepath.Join(sink, "gc-work"), filepath.Join(src, "gc-work"))
}

func TestMaterializeAgentLegacyStubPreservesUserContent(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "gc-work")
	sink := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(sink, "gc-work"), 0o755); err != nil {
		t.Fatal(err)
	}
	customBody := "---\nname: gc-work\ndescription: my custom skill\n---\n\nUser-defined content.\n"
	if err := os.WriteFile(
		filepath.Join(sink, "gc-work", "SKILL.md"),
		[]byte(customBody), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Request{
		SinkDir:     sink,
		Desired:     []SkillEntry{{Name: "gc-work", Source: filepath.Join(src, "gc-work"), Origin: "core"}},
		OwnedRoots:  []string{src},
		LegacyNames: LegacyStubNames(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.LegacyMigrated) != 0 {
		t.Fatalf("LegacyMigrated should be empty; got %v", res.LegacyMigrated)
	}
	if len(res.Materialized) != 0 {
		t.Fatalf("Materialized should be empty; got %v", res.Materialized)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Name != "gc-work" {
		t.Fatalf("Skipped = %+v", res.Skipped)
	}
	body, err := os.ReadFile(filepath.Join(sink, "gc-work", "SKILL.md"))
	if err != nil || string(body) != customBody {
		t.Errorf("user content modified: body=%q err=%v", string(body), err)
	}
}

func TestMaterializeAgentLegacyStubExtraFilesPreserved(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "gc-work")
	sink := filepath.Join(t.TempDir(), "skills")
	stubDir := filepath.Join(sink, "gc-work")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "SKILL.md"), []byte(legacyStubBodies["gc-work"]), 0o644); err != nil {
		t.Fatal(err)
	}
	// User added a sibling file — directory no longer matches stub shape.
	if err := os.WriteFile(filepath.Join(stubDir, "notes.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Request{
		SinkDir:     sink,
		Desired:     []SkillEntry{{Name: "gc-work", Source: filepath.Join(src, "gc-work"), Origin: "core"}},
		OwnedRoots:  []string{src},
		LegacyNames: LegacyStubNames(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.LegacyMigrated) != 0 {
		t.Fatalf("LegacyMigrated = %v (must be empty when sibling files exist)", res.LegacyMigrated)
	}
	if _, err := os.Stat(filepath.Join(stubDir, "notes.md")); err != nil {
		t.Errorf("user sibling file removed: %v", err)
	}
}

func TestMaterializeAgentSinkDirRequired(t *testing.T) {
	t.Parallel()
	if _, err := Run(Request{}); err == nil {
		t.Fatal("expected error for empty SinkDir")
	}
}

func TestMaterializeAgentRemovesAllOwnedWhenDesiredEmpty(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "alpha")
	mkSkill(t, src, "beta")
	sink := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(src, "alpha"), filepath.Join(sink, "alpha"))
	mustSymlink(t, filepath.Join(src, "beta"), filepath.Join(sink, "beta"))
	// User content survives.
	if err := os.WriteFile(filepath.Join(sink, "user.md"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(Request{
		SinkDir:    sink,
		OwnedRoots: []string{src},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Materialized) != 0 {
		t.Fatalf("nothing should be materialized; got %v", res.Materialized)
	}
	if _, err := os.Lstat(filepath.Join(sink, "alpha")); !os.IsNotExist(err) {
		t.Errorf("alpha not removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sink, "beta")); !os.IsNotExist(err) {
		t.Errorf("beta not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sink, "user.md")); err != nil {
		t.Errorf("user file removed: %v", err)
	}
}

// TestMaterializeAgentAliasedOwnedRoot exercises the path-alias case
// from the Phase 2 review: when the owned root is supplied as a path
// that traverses a symlink (e.g., /tmp/proj/skills where /tmp →
// /private/tmp on macOS), the materializer must still recognize
// previously-written symlinks pointing at the resolved form as its
// own. Without canonicalisation the symlink would be reclassified as
// external and never updated.
func TestMaterializeAgentAliasedOwnedRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Real source dir: <root>/realDir/skills
	realSkills := filepath.Join(root, "realDir", "skills")
	mkSkill(t, realSkills, "alpha")
	// Symlinked alias: <root>/alias -> <root>/realDir
	if err := os.Symlink(filepath.Join(root, "realDir"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	aliasedRoot := filepath.Join(root, "alias", "skills")

	sink := filepath.Join(t.TempDir(), "skills")

	// First pass: materialize via the aliased owned-root path. The link
	// target is also written using the aliased path.
	res, err := Run(Request{
		SinkDir:    sink,
		Desired:    []SkillEntry{{Name: "alpha", Source: filepath.Join(aliasedRoot, "alpha"), Origin: "city"}},
		OwnedRoots: []string{aliasedRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Materialized, []string{"alpha"}) {
		t.Fatalf("first pass materialized = %v", res.Materialized)
	}

	// Second pass: same desired set, but supply the owned root via the
	// canonical (resolved) path. Without canonicalisation the symlink
	// written above would be classified as external; cleanup would skip
	// it and the create loop would Skip the desired entry as a
	// "user-owned symlink at sink path" — not what we want.
	res2, err := Run(Request{
		SinkDir:    sink,
		Desired:    []SkillEntry{{Name: "alpha", Source: filepath.Join(realSkills, "alpha"), Origin: "city"}},
		OwnedRoots: []string{realSkills},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res2.Materialized, []string{"alpha"}) {
		t.Fatalf("second pass materialized = %v (want [alpha]); skipped=%+v warnings=%v",
			res2.Materialized, res2.Skipped, res2.Warnings)
	}
	if len(res2.Skipped) != 0 {
		t.Fatalf("aliased root reclassified as user-owned: %+v", res2.Skipped)
	}
}

// TestMaterializeAgentRelativeSymlinkLeftAlone is the regression for
// the pass-2 Codex finding: a sink entry that is a relative-target
// symlink (which the materializer never writes — it always uses
// absolute targets) must be treated as user-placed and left alone.
// Without the IsAbs short-circuit, filepath.Abs would resolve the
// relative path against the process cwd and may falsely classify
// the link as gc-owned, leading to incorrect cleanup.
func TestMaterializeAgentRelativeSymlinkLeftAlone(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	mkSkill(t, src, "alpha")
	sink := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	// User-placed relative symlink at the sink, pointing somewhere
	// arbitrary by relative path.
	if err := os.Symlink("../../elsewhere", filepath.Join(sink, "user-rel")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	res, err := Run(Request{
		SinkDir:    sink,
		Desired:    []SkillEntry{{Name: "alpha", Source: filepath.Join(src, "alpha"), Origin: "city"}},
		OwnedRoots: []string{src},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The relative symlink must remain untouched.
	tgt, err := os.Readlink(filepath.Join(sink, "user-rel"))
	if err != nil {
		t.Fatalf("user-rel removed: %v", err)
	}
	if tgt != "../../elsewhere" {
		t.Errorf("user-rel target rewritten: %q", tgt)
	}
	if !reflect.DeepEqual(res.Materialized, []string{"alpha"}) {
		t.Errorf("alpha not materialized alongside user-rel: %v", res.Materialized)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "user-rel") || strings.Contains(w, "elsewhere") {
			t.Errorf("relative symlink produced warning: %q", w)
		}
	}
}

func TestCanonicalizePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDir := filepath.Join(root, "realDir")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Existing directory: alias resolves to realDir.
	got, err := canonicalizePath(alias)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := filepath.EvalSymlinks(alias)
	if got != expected {
		t.Errorf("alias dir: got %q, want %q", got, expected)
	}

	// Missing tail under an aliased ancestor: walk-up + suffix re-append.
	missing := filepath.Join(alias, "not-yet-created", "leaf")
	got, err = canonicalizePath(missing)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix, _ := filepath.EvalSymlinks(alias)
	wantMissing := filepath.Join(wantPrefix, "not-yet-created", "leaf")
	if got != wantMissing {
		t.Errorf("missing tail: got %q, want %q", got, wantMissing)
	}

	// Empty input.
	if got, err := canonicalizePath(""); err != nil || got != "" {
		t.Errorf("empty: got (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestTargetUnderOwnedRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
		roots  []string
		want   bool
	}{
		{"under root", "/srv/skills/foo", []string{"/srv/skills"}, true},
		{"exact root", "/srv/skills", []string{"/srv/skills"}, true},
		{"sibling not match", "/srv/skills2/foo", []string{"/srv/skills"}, false},
		{"unrelated", "/elsewhere/foo", []string{"/srv/skills"}, false},
		{"relative not owned", "../skills/foo", []string{"/srv/skills"}, false},
		{"second root matches", "/other/here", []string{"/srv/skills", "/other"}, true},
		{"empty roots", "/anywhere", nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := targetUnderOwnedRoot(c.target, c.roots); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestAtomicSymlinkReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target1 := filepath.Join(dir, "t1")
	target2 := filepath.Join(dir, "t2")
	if err := os.WriteFile(target1, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target2, []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")

	if err := atomicSymlink(target1, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != target1 {
		t.Fatalf("first link target = %q", got)
	}
	if err := atomicSymlink(target2, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != target2 {
		t.Fatalf("second link target = %q", got)
	}
	// No leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".link.tmp.") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLegacyStubNamesCoverSevenTopics(t *testing.T) {
	t.Parallel()
	got := LegacyStubNames()
	want := []string{
		"gc-agents", "gc-city", "gc-dashboard", "gc-dispatch",
		"gc-mail", "gc-rigs", "gc-work",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// -- helpers ----------------------------------------------------------

func mkSkill(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: test skill\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func checkSymlink(t *testing.T, link, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %q: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q is not a symlink (mode %v)", link, info.Mode())
	}
	tgt, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %q: %v", link, err)
	}
	if tgt != wantTarget {
		t.Fatalf("%q -> %q, want %q", link, tgt, wantTarget)
	}
}

func skippedToNames(skipped []SkippedConflict) []string {
	out := make([]string, len(skipped))
	for i, s := range skipped {
		out[i] = s.Name
	}
	sort.Strings(out)
	return out
}

// setupBootstrapHome creates a fake GC_HOME with bootstrap pack caches
// and an implicit-import.toml that points each named pack at its cache.
// Each pack receives a skills/ directory with the listed skill names.
//
// The returned path can be set as GC_HOME via t.Setenv.
func setupBootstrapHome(t *testing.T, packs map[string][]string) string {
	t.Helper()
	gcHome := t.TempDir()
	cacheRoot := filepath.Join(gcHome, "cache", "repos")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("schema = 1\n")
	for name, skills := range packs {
		// One cache dir per pack; the dir name matches the pack name for
		// determinism. The materializer doesn't care what the dir name
		// is — only the source+commit pair, and we synthesize a unique
		// commit per pack so config.GlobalRepoCachePath returns the
		// path we created.
		commit := name + "-commit"
		source := "github.com/example/" + name
		// Pre-create the cache dir matching what GlobalRepoCachePath
		// will compute. We invoke the package function via the same
		// bootstrap helpers used by production code.
		// Use config.GlobalRepoCachePath to compute the canonical path.
		cacheDir := globalRepoCachePathHelper(gcHome, source, commit)
		skillsDir := filepath.Join(cacheDir, "skills")
		for _, skill := range skills {
			mkSkill(t, skillsDir, skill)
		}
		sb.WriteString("\n[imports.\"" + name + "\"]\n")
		sb.WriteString("source = \"" + source + "\"\n")
		sb.WriteString("version = \"0.1.0\"\n")
		sb.WriteString("commit = \"" + commit + "\"\n")
	}
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return gcHome
}

// globalRepoCachePathHelper centralizes the import of config.GlobalRepoCachePath
// so the rest of the helpers don't need to know about that surface.
func globalRepoCachePathHelper(gcHome, source, commit string) string {
	return config.GlobalRepoCachePath(gcHome, source, commit)
}
