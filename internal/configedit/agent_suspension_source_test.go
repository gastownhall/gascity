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

// cityImportCity writes a schema-2 city importing pack binding "base", whose
// city-scoped agent "worker" (qualified "base.worker") defaults to
// suspended = true. cityTail is appended after the import table.
func cityImportCity(t *testing.T, cityTail string) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "base")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(`[pack]
name = "base"
schema = 2

[[agent]]
name = "worker"
scope = "city"
provider = "claude"
suspended = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return writeTOML(t, dir, `# City rationale: keep this comment.
[workspace]
name = "test-city"

# Shared base pack.
[imports.base]
source = "./packs/base"
`+cityTail)
}

func TestResumeAgent_SetsExistingAgentPatchInPlace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		patch       string
		wantRemoved []string
	}{
		{
			name: "patch without suspended key",
			patch: `
# Operator tuning for the worker.
[[patches.agent]]
name = "base.worker"
nudge = "check the queue" # tuned 10-30
`,
		},
		{
			name: "patch with suspended true",
			patch: `
# Operator tuning for the worker.
[[patches.agent]]
name = "base.worker"
nudge = "check the queue" # tuned 10-30
suspended = true # held for the migration
`,
			wantRemoved: []string{"suspended = true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := cityImportCity(t, tc.patch)
			if !effectiveSuspended(t, path, "base.worker") {
				t.Fatal("fixture: worker should start suspended")
			}
			before := string(mustReadFile(t, path))

			ed := configedit.NewEditor(fsys.OSFS{}, path)
			if err := ed.ResumeAgent("base.worker"); err != nil {
				t.Fatalf("ResumeAgent: %v", err)
			}

			if effectiveSuspended(t, path, "base.worker") {
				t.Fatal("worker still suspended after resume")
			}
			after := string(mustReadFile(t, path))
			for _, comment := range []string{"# City rationale: keep this comment.", "# Shared base pack.", "# Operator tuning for the worker.", "# tuned 10-30"} {
				if !strings.Contains(after, comment) {
					t.Errorf("resume dropped comment %q; city.toml now:\n%s", comment, after)
				}
			}
			if tc.wantRemoved != nil && !strings.Contains(after, "suspended = false # held for the migration") {
				t.Errorf("resume did not keep the suspended line's trailing comment; city.toml now:\n%s", after)
			}
			if strings.Count(after, "[[patches.agent]]") != 1 {
				t.Errorf("resume must update the existing patch, not add one; city.toml now:\n%s", after)
			}
			assertOnlyLinesChanged(t, before, after, tc.wantRemoved, []string{"suspended = false"})
		})
	}
}

func TestSuspendAgent_RigOverrideDecidesPackAgent(t *testing.T) {
	path := rigImportCity(t, false, `
[[rigs.overrides]]
agent = "worker"
nudge = "keep me"
suspended = false
`, "")
	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("proj/base.worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	if !effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("worker not suspended after suspend: the rig override decides and was not edited")
	}
	after := string(mustReadFile(t, path))
	assertOnlyLinesChanged(t, before, after, []string{"suspended = false"}, []string{"suspended = true"})
}

// TestSuspendAgent_RigPatchWinsOverOverride pins composition's last-wins
// order within a rig: overrides apply before patches, so when both a
// [[rigs.overrides]] and a [[rigs.patches]] entry set suspended for the
// agent, the patches entry decides and is the one suspend edits.
func TestSuspendAgent_RigPatchWinsOverOverride(t *testing.T) {
	path := rigImportCity(t, false, `
[[rigs.overrides]]
agent = "worker"
suspended = false # override entry stays false

[[rigs.patches]]
agent = "worker"
suspended = false # deciding patch entry
`, "")
	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("proj/base.worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	if !effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("worker not suspended after suspend: the deciding rig patch was not edited")
	}
	after := string(mustReadFile(t, path))
	if !strings.Contains(after, "suspended = false # override entry stays false") {
		t.Errorf("the shadowed [[rigs.overrides]] entry must stay false; city.toml now:\n%s", after)
	}
	if !strings.Contains(after, "suspended = true # deciding patch entry") {
		t.Errorf("the deciding [[rigs.patches]] entry must flip to true; city.toml now:\n%s", after)
	}
	assertOnlyLinesChanged(t, before, after, []string{"suspended = false"}, []string{"suspended = true"})
}
