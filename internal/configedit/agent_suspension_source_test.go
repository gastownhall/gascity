package configedit_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// rigImportCity writes a schema-2 city whose rig "proj" imports a pack
// binding "base" that stamps one rig-scoped agent "worker" (qualified
// "proj/base.worker"). rigTail is appended inside the [[rigs]] entry after
// its import table, so callers can add [[rigs.overrides]] / [[rigs.patches]]
// entries; cityTail is appended after the rig. packSuspended sets the pack
// agent's own default. It returns the city.toml path.
func rigImportCity(t *testing.T, packSuspended bool, rigTail, cityTail string) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "base")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packTOML := "[pack]\nname = \"base\"\nschema = 2\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\nprovider = \"claude\"\n"
	if packSuspended {
		packTOML += "suspended = true\n"
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(dir, "proj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	city := `# City rationale: keep this comment.
[workspace]
name = "test-city"

[[rigs]]
name = "proj"
path = "` + rigPath + `"

[rigs.imports.base]
source = "./packs/base"
` + rigTail + cityTail
	return writeTOML(t, dir, city)
}

// effectiveSuspended reloads the composed config and returns the
// suspension of the agent with the given qualified name.
func effectiveSuspended(t *testing.T, path, qualified string) bool {
	t.Helper()
	cfg := readExpandedTOML(t, path)
	for _, a := range cfg.Agents {
		if a.QualifiedName() == qualified {
			return a.Suspended
		}
	}
	names := make([]string, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		names = append(names, a.QualifiedName())
	}
	t.Fatalf("agent %q not in composed config; agents: %v", qualified, names)
	return false
}

// assertOnlyLinesChanged fails unless the comment-stripped content diff
// between before and after is exactly wantRemoved / wantAdded.
func assertOnlyLinesChanged(t *testing.T, before, after string, wantRemoved, wantAdded []string) {
	t.Helper()
	removed, added := diffContentLines(before, after)
	sort.Strings(wantRemoved)
	sort.Strings(wantAdded)
	if !stringSlicesEqual(removed, wantRemoved) || !stringSlicesEqual(added, wantAdded) {
		t.Fatalf("content diff = removed %q added %q, want removed %q added %q; city.toml now:\n%s",
			removed, added, wantRemoved, wantAdded, after)
	}
}

func TestResumeAgent_RigPatchDecidesPackAgent(t *testing.T) {
	path := rigImportCity(t, false, `
# Held while the rig migrates.
[[rigs.patches]]
agent = "worker"
suspended = true # paused by operator
`, "")
	if !effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("fixture: worker should start suspended by its rig patch")
	}
	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("proj/base.worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	if effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("worker still suspended after resume: the rig patch decides and was not edited")
	}
	after := string(mustReadFile(t, path))
	for _, comment := range []string{"# City rationale: keep this comment.", "# Held while the rig migrates.", "# paused by operator"} {
		if !strings.Contains(after, comment) {
			t.Errorf("resume dropped comment %q; city.toml now:\n%s", comment, after)
		}
	}
	if strings.Contains(after, "[[patches.agent]]") {
		t.Errorf("resume appended a city patch; city.toml now:\n%s", after)
	}
	assertOnlyLinesChanged(t, before, after, []string{"suspended = true"}, []string{"suspended = false"})
}
