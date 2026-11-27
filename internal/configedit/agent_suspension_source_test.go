package configedit_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// rigImportCity writes a schema-2 city whose rig "proj" imports a pack
// binding "base" that stamps one rig-scoped agent "worker" (qualified
// "proj/base.worker"). rigTail is appended inside the [[rigs]] entry after
// its import table, so callers can add [[rigs.overrides]] / [[rigs.patches]]
// entries. It returns the city.toml path.
func rigImportCity(t *testing.T, rigTail string) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "base")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packTOML := "[pack]\nname = \"base\"\nschema = 2\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\nprovider = \"claude\"\n"
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
` + rigTail
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
	path := rigImportCity(t, `
# Held while the rig migrates.
[[rigs.patches]]
agent = "worker"
suspended = true # paused by operator
`)
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
	path := rigImportCity(t, `
[[rigs.overrides]]
agent = "worker"
nudge = "keep me"
suspended = false
`)
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
	path := rigImportCity(t, `
[[rigs.overrides]]
agent = "worker"
suspended = false # override entry stays false

[[rigs.patches]]
agent = "worker"
suspended = false # deciding patch entry
`)
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

// treeSnapshot maps every regular file under root to its content, so a
// rollback assertion covers city.toml, .gc/site.toml and any file an edit
// could create.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return files
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeSnapshot(t, root)
	for path, content := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s was removed by the failed edit", path)
			continue
		}
		if got != content {
			t.Errorf("%s changed by the failed edit:\n--- before\n%s\n--- after\n%s", path, content, got)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was created by the failed edit", path)
		}
	}
}

// fragmentShadowCity is cityImportCity with the base pack default not
// suspended and a fragment whose [[patches.agent]] (applied after every
// city.toml patch) holds the worker suspended.
func fragmentShadowCity(t *testing.T) string {
	t.Helper()
	path := cityImportCity(t, "")
	dir := filepath.Dir(path)
	city := string(mustReadFile(t, path))
	city = "include = [\"fragment.toml\"]\n" + city
	if err := os.WriteFile(path, []byte(city), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fragment.toml"), []byte(`[[patches.agent]]
name = "base.worker"
suspended = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentSuspension_ShadowedLayerRollsBack(t *testing.T) {
	path := fragmentShadowCity(t)
	dir := filepath.Dir(path)
	if !effectiveSuspended(t, path, "base.worker") {
		t.Fatal("fixture: fragment patch should hold worker suspended")
	}
	before := treeSnapshot(t, dir)

	err := configedit.NewEditor(fsys.OSFS{}, path).ResumeAgent("base.worker")
	if !errors.Is(err, configedit.ErrShadowed) {
		t.Fatalf("ResumeAgent error = %v, want ErrShadowed", err)
	}
	if !strings.Contains(err.Error(), "base.worker") {
		t.Errorf("error %q does not name the agent", err)
	}
	assertTreeUnchanged(t, dir, before)
	if !effectiveSuspended(t, path, "base.worker") {
		t.Fatal("worker resumed despite the refused edit")
	}
}

func TestAgentSuspension_AmbiguousRigSettersRefused(t *testing.T) {
	path := rigImportCity(t, `
[[rigs.patches]]
agent = "worker"
suspended = true

[[rigs.patches]]
agent = "worker"
suspended = true
`)
	dir := filepath.Dir(path)
	before := treeSnapshot(t, dir)

	err := configedit.NewEditor(fsys.OSFS{}, path).ResumeAgent("proj/base.worker")
	if !errors.Is(err, configedit.ErrShadowed) {
		t.Fatalf("ResumeAgent error = %v, want ErrShadowed", err)
	}
	if !strings.Contains(err.Error(), "[[rigs.patches]]") {
		t.Errorf("error %q does not name the ambiguous table", err)
	}
	assertTreeUnchanged(t, dir, before)
}

// renameFailFS fails every rename onto target after the first, so the
// forward write succeeds and the rollback write fails. Paths are compared
// canonically: the writers rename onto the symlink-resolved path, which
// differs from t.TempDir()'s on macOS (/var -> /private/var).
type renameFailFS struct {
	fsys.OSFS
	target  string
	renames int
}

func (f *renameFailFS) Rename(oldpath, newpath string) error {
	if pathutil.SamePath(newpath, f.target) {
		f.renames++
		if f.renames > 1 {
			return errors.New("injected rename failure")
		}
	}
	return f.OSFS.Rename(oldpath, newpath)
}

func TestAgentSuspension_RollbackFailureSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		viaLink bool
	}{
		{name: "direct path"},
		// The macOS shape: t.TempDir() is under /var, a link to /private/var,
		// so the writers rename onto a path other than the one the editor got.
		{name: "city reached through a symlinked dir", viaLink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fragmentShadowCity(t)
			if tc.viaLink {
				link := filepath.Join(t.TempDir(), "city-link")
				if err := os.Symlink(filepath.Dir(path), link); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				path = filepath.Join(link, filepath.Base(path))
			}
			fs := &renameFailFS{target: path}

			err := configedit.NewEditor(fs, path).ResumeAgent("base.worker")
			if !errors.Is(err, configedit.ErrShadowed) {
				t.Fatalf("ResumeAgent error = %v, want ErrShadowed", err)
			}
			if !strings.Contains(err.Error(), "injected rename failure") || !strings.Contains(err.Error(), "restor") {
				t.Fatalf("error %q does not surface the failed restoration", err)
			}
			if strings.Contains(err.Error(), "rolled back") {
				t.Fatalf("error %q claims a rollback that failed", err)
			}
		})
	}
}

// fragmentRigPatchCity is rigImportCity with the whole rig, including its
// deciding [[rigs.patches]] entry holding the worker suspended, moved into an
// included fragment the editor does not own. withSiteBinding pre-creates
// .gc/site.toml binding the rig's path.
func fragmentRigPatchCity(t *testing.T, withSiteBinding bool) string {
	t.Helper()
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "base")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte("[pack]\nname = \"base\"\nschema = 2\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\nprovider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(dir, "proj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fragment.toml"), []byte(`[[rigs]]
name = "proj"
path = "`+rigPath+`"

[rigs.imports.base]
source = "./packs/base"

# This entry keeps the worker down.
[[rigs.patches]]
agent = "worker"
suspended = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if withSiteBinding {
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte("[[rig]]\nname = \"proj\"\npath = \""+rigPath+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return writeTOML(t, dir, `# City rationale: keep this comment.
include = ["fragment.toml"]

[workspace]
name = "test-city"
`)
}

// TestResumeAgent_FragmentOwnedRigPatchRefusedAndRolledBack covers the
// #8708 shape: the deciding [[rigs.patches]] entry lives in an included
// fragment, so the local city patch the editor writes is shadowed. Resume
// must refuse with ErrShadowed naming that rig entry and leave every file
// as it was.
func TestResumeAgent_FragmentOwnedRigPatchRefusedAndRolledBack(t *testing.T) {
	path := fragmentRigPatchCity(t, true)
	dir := filepath.Dir(path)
	if !effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("fixture: the fragment's rig patch should hold worker suspended")
	}
	before := treeSnapshot(t, dir)

	err := configedit.NewEditor(fsys.OSFS{}, path).ResumeAgent("proj/base.worker")
	if !errors.Is(err, configedit.ErrShadowed) {
		t.Fatalf("ResumeAgent error = %v, want ErrShadowed", err)
	}
	for _, want := range []string{
		`agent "proj/base.worker"`,
		"the edit set suspended = false, but the composed config still reports suspended = true",
		`decided by rig "proj"'s [[rigs.patches]] entry for agent "worker"`,
		"(the attempted edit was rolled back)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is not actionable: missing %q in error: %v", want, err)
		}
	}
	assertTreeUnchanged(t, dir, before)
	if !effectiveSuspended(t, path, "proj/base.worker") {
		t.Fatal("worker resumed despite the refused edit")
	}
}

// TestResumeAgent_RestoreRemovesCreatedSiteBinding checks that rollback
// restores file existence: a .gc/site.toml the refused edit created (the
// city patch append persists rig site bindings) is removed again.
func TestResumeAgent_RestoreRemovesCreatedSiteBinding(t *testing.T) {
	path := fragmentRigPatchCity(t, false)
	dir := filepath.Dir(path)
	sitePath := filepath.Join(dir, ".gc", "site.toml")
	if _, err := os.Stat(sitePath); !os.IsNotExist(err) {
		t.Fatalf("fixture must start without a site binding: %v", err)
	}
	before := treeSnapshot(t, dir)

	err := configedit.NewEditor(fsys.OSFS{}, path).ResumeAgent("proj/base.worker")
	if !errors.Is(err, configedit.ErrShadowed) {
		t.Fatalf("ResumeAgent error = %v, want ErrShadowed", err)
	}
	if _, err := os.Stat(sitePath); !os.IsNotExist(err) {
		t.Errorf("a site binding created by the refused edit must be removed; stat error: %v", err)
	}
	assertTreeUnchanged(t, dir, before)
}

// setAgentSuspended runs the editor's suspend or resume for target.
func setAgentSuspended(t *testing.T, path, target string, suspend bool) error {
	t.Helper()
	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if suspend {
		return ed.SuspendAgent(target)
	}
	return ed.ResumeAgent(target)
}

// TestAgentSuspension_RigKeyedExactPatchAppendsDirKeyedBlock covers an exact
// [[patches.agent]] keyed by rig = "<rig>", the key AgentPatch recommends.
// The in-place setter only locates dir-keyed blocks, so the editor keeps the
// rig-keyed block as it is and appends a dir-keyed block after it, which
// composition applies later. The edit must succeed as a pure append.
func TestAgentSuspension_RigKeyedExactPatchAppendsDirKeyedBlock(t *testing.T) {
	for _, tc := range []struct {
		name         string
		suspendedKey string
		suspend      bool
	}{
		{name: "resume over suspended true", suspendedKey: "suspended = true # held by the operator\n", suspend: false},
		{name: "resume without a suspended key", suspend: false},
		{name: "suspend over suspended false", suspendedKey: "suspended = false\n", suspend: true},
		{name: "suspend without a suspended key", suspend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := rigImportCity(t, `
# Operator tuning keyed by rig.
[[patches.agent]]
rig = "proj"
name = "base.worker"
nudge = "check the queue" # tuned 10-30
`+tc.suspendedKey)
			before := string(mustReadFile(t, path))

			if err := setAgentSuspended(t, path, "proj/base.worker", tc.suspend); err != nil {
				t.Fatalf("setting suspended = %t: %v", tc.suspend, err)
			}

			if got := effectiveSuspended(t, path, "proj/base.worker"); got != tc.suspend {
				t.Fatalf("composed suspended = %t, want %t", got, tc.suspend)
			}
			after := string(mustReadFile(t, path))
			if !strings.HasPrefix(after, before) {
				t.Fatalf("the edit must keep every existing byte, the rig-keyed patch included, and only append; city.toml now:\n%s", after)
			}
			assertOnlyLinesChanged(t, before, after, nil, []string{
				"[[patches.agent]]", `dir = "proj"`, `name = "base.worker"`, fmt.Sprintf("suspended = %t", tc.suspend),
			})
		})
	}
}

// twoBindingRigCity writes a schema-2 city whose rig "proj" imports pack
// bindings "aa" and "bb", each stamping a rig-scoped agent with the bare name
// "worker" ("proj/aa.worker" and "proj/bb.worker"). tail is appended inside
// the [[rigs]] entry after its import tables.
func twoBindingRigCity(t *testing.T, tail string) string {
	t.Helper()
	dir := t.TempDir()
	for _, binding := range []string{"aa", "bb"} {
		packDir := filepath.Join(dir, "packs", binding)
		if err := os.MkdirAll(packDir, 0o755); err != nil {
			t.Fatal(err)
		}
		packTOML := "[pack]\nname = \"" + binding + "\"\nschema = 2\n\n[[agent]]\nname = \"worker\"\nscope = \"rig\"\nprovider = \"claude\"\n"
		if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(packTOML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rigPath := filepath.Join(dir, "proj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return writeTOML(t, dir, `# City rationale: keep this comment.
[workspace]
name = "test-city"

[[rigs]]
name = "proj"
path = "`+rigPath+`"

[rigs.imports.aa]
source = "./packs/aa"

[rigs.imports.bb]
source = "./packs/bb"
`+tail)
}

// TestAgentSuspension_RigSetterDecidesOnlyTheFirstBareNameMatch pins that a
// rig setter decides only the agent composition applies it to: the first
// agent the rig stamped with the setter's bare name. Editing a second binding
// that shares the bare name takes the exact city-patch path and leaves the
// rig setter and the first binding alone.
func TestAgentSuspension_RigSetterDecidesOnlyTheFirstBareNameMatch(t *testing.T) {
	const first, second = "proj/aa.worker", "proj/bb.worker"
	probe := twoBindingRigCity(t, "\n[[rigs.patches]]\nagent = \"worker\"\nsuspended = true\n")
	if !effectiveSuspended(t, probe, first) || effectiveSuspended(t, probe, second) {
		t.Fatalf("fixture: a [[rigs.patches]] entry for agent \"worker\" must reach %s only", first)
	}

	for _, tc := range []struct {
		name        string
		tail        string
		target      string
		suspend     bool
		wantRemoved []string
		wantAdded   []string
	}{
		{
			name: "resume the second binding held by its own city patch",
			tail: `
[[rigs.patches]]
agent = "worker"
suspended = true # holds the first binding

[[patches.agent]]
dir = "proj"
name = "bb.worker"
suspended = true
`,
			target:      second,
			wantRemoved: []string{"suspended = true"},
			wantAdded:   []string{"suspended = false"},
		},
		{
			name: "suspend the second binding while the rig setter says false",
			tail: `
[[rigs.patches]]
agent = "worker"
suspended = false
`,
			target:    second,
			suspend:   true,
			wantAdded: []string{"[[patches.agent]]", `dir = "proj"`, `name = "bb.worker"`, "suspended = true"},
		},
		{
			name: "resume the running second binding while the rig setter holds the first",
			tail: `
[[rigs.patches]]
agent = "worker"
suspended = true
`,
			target:    second,
			wantAdded: []string{"[[patches.agent]]", `dir = "proj"`, `name = "bb.worker"`, "suspended = false"},
		},
		{
			name: "resume the first binding edits the rig setter",
			tail: `
[[rigs.patches]]
agent = "worker"
suspended = true
`,
			target:      first,
			wantRemoved: []string{"suspended = true"},
			wantAdded:   []string{"suspended = false"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := twoBindingRigCity(t, tc.tail)
			other := first
			if tc.target == first {
				other = second
			}
			otherBefore := effectiveSuspended(t, path, other)
			before := string(mustReadFile(t, path))

			if err := setAgentSuspended(t, path, tc.target, tc.suspend); err != nil {
				t.Fatalf("setting %s suspended = %t: %v", tc.target, tc.suspend, err)
			}

			if got := effectiveSuspended(t, path, tc.target); got != tc.suspend {
				t.Fatalf("%s composed suspended = %t, want %t", tc.target, got, tc.suspend)
			}
			if got := effectiveSuspended(t, path, other); got != otherBefore {
				t.Fatalf("%s composed suspended changed %t -> %t", other, otherBefore, got)
			}
			assertOnlyLinesChanged(t, before, string(mustReadFile(t, path)), tc.wantRemoved, tc.wantAdded)
		})
	}
}

// sideEffectRenameFS appends extra to target right after the first rename
// onto it lands, so an otherwise exact suspension write also changes another
// agent's composed suspension.
type sideEffectRenameFS struct {
	fsys.OSFS
	target string
	extra  string
	fired  bool
}

func (f *sideEffectRenameFS) Rename(oldpath, newpath string) error {
	if err := f.OSFS.Rename(oldpath, newpath); err != nil {
		return err
	}
	if f.fired || !pathutil.SamePath(newpath, f.target) {
		return nil
	}
	f.fired = true
	data, err := os.ReadFile(newpath)
	if err != nil {
		return err
	}
	return os.WriteFile(newpath, append(data, f.extra...), 0o644)
}

// TestAgentSuspension_EditChangingAnotherAgentRefused pins the verify step's
// cross-agent guard: a suspension write that also changes another agent's
// composed suspension is refused with ErrShadowed (HTTP 409), and every file
// is restored.
func TestAgentSuspension_EditChangingAnotherAgentRefused(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
provider = "claude"

[[agent]]
name = "worker"
provider = "claude"
`)
	before := treeSnapshot(t, dir)
	fs := &sideEffectRenameFS{target: path, extra: "\n[[patches.agent]]\nname = \"mayor\"\nsuspended = true\n"}

	err := configedit.NewEditor(fs, path).SuspendAgent("worker")
	if !fs.fired {
		t.Fatal("fixture: the suspension write never renamed onto city.toml")
	}
	if !errors.Is(err, configedit.ErrShadowed) {
		t.Fatalf("SuspendAgent error = %v, want ErrShadowed", err)
	}
	for _, want := range []string{`also changed agent "mayor" (suspended false -> true)`, "(the attempted edit was rolled back)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	assertTreeUnchanged(t, dir, before)
}
