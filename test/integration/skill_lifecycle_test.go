//go:build integration

// Skill materialization lifecycle integration tests (Phase 4B).
//
// These tests exercise the materializer at the api boundary the
// supervisor tick uses — materialize.Run and the
// end-to-end catalog discovery wiring — against a real filesystem.
// Fast (no runtime.Provider spawned) but with real os.Symlink,
// os.Readlink, and filepath.EvalSymlinks behaviour.
//
// The spec-called-out "full add/edit/delete lifecycle with drain/
// restart observation" is covered in two layers:
//
//   - Symlink lifecycle (here): add, edit the source, delete, rename —
//     assert sink converges each pass.
//   - Drain observation (unit): cmd/gc/skill_supervisor_test.go covers
//     the per-agent materialization call that feeds FingerprintExtra.
package integration

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSkillLifecycle_AddEditDeleteRename walks a city-pack skill
// catalog through its lifecycle and asserts the materializer
// converges the agent's sink at each step. This is the
// spec-requested integration test (engdocs/proposals/skill-
// materialization.md § "Testing") with the drain observation folded
// in via runtime.HashPathContent hash comparison — the same hash the
// Phase 3B fingerprint machinery uses.
func TestSkillLifecycle_AddEditDeleteRename(t *testing.T) {
	cityPath := t.TempDir()
	sinkDir := filepath.Join(cityPath, ".claude", "skills")
	packSkills := filepath.Join(cityPath, "skills")

	// Isolate bootstrap discovery so the test doesn't pick up the
	// host's ~/.gc implicit-import state.
	t.Setenv("GC_HOME", t.TempDir())

	// Step 1: Add "plan". Materialise, assert symlink points at the
	// source, capture the initial content hash.
	writeLifecycleSkill(t, packSkills, "plan", "initial body\n")
	firstHash := materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"plan"})
	if firstHash["plan"] == "" {
		t.Fatalf("plan hash empty at add")
	}

	// Step 2: Edit "plan" content. Materialise (idempotent — symlink
	// unchanged). The hash must drift — this is what drives the
	// Phase 3B FingerprintExtra drain.
	writeLifecycleSkill(t, packSkills, "plan", "edited body with different content\n")
	secondHash := materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"plan"})
	if secondHash["plan"] == firstHash["plan"] {
		t.Errorf("hash did not drift after content edit: %q", firstHash["plan"])
	}

	// Step 3: Add a second skill "code-review". Materialise, assert
	// both symlinks present, existing "plan" hash stable (idempotent).
	writeLifecycleSkill(t, packSkills, "code-review", "review body\n")
	thirdHash := materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"code-review", "plan"})
	if thirdHash["plan"] != secondHash["plan"] {
		t.Errorf("plan hash drifted while adding code-review; want %q got %q",
			secondHash["plan"], thirdHash["plan"])
	}
	if thirdHash["code-review"] == "" {
		t.Errorf("code-review hash empty")
	}

	// Step 4: Delete "plan" from the catalog. Next materialise should
	// remove the plan symlink and preserve code-review.
	if err := os.RemoveAll(filepath.Join(packSkills, "plan")); err != nil {
		t.Fatal(err)
	}
	fourthHash := materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"code-review"})
	if _, ok := fourthHash["plan"]; ok {
		t.Errorf("plan hash present after delete: %v", fourthHash)
	}

	// Step 5: Rename "code-review" -> "review". The materialiser
	// should delete the old symlink and create the new one in a
	// single pass.
	if err := os.RemoveAll(filepath.Join(packSkills, "code-review")); err != nil {
		t.Fatal(err)
	}
	writeLifecycleSkill(t, packSkills, "review", "review body\n")
	fifthHash := materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"review"})
	if _, err := os.Lstat(filepath.Join(sinkDir, "code-review")); !os.IsNotExist(err) {
		t.Errorf("code-review symlink should be removed after rename, err=%v", err)
	}
	if fifthHash["review"] == "" {
		t.Errorf("review hash empty after rename")
	}
}

// TestSkillLifecycle_UserContentPreserved exercises the user-owned
// content safety matrix rows: a regular file and a regular directory
// at sink paths must survive every materialisation pass. Matches the
// acceptance-test row "user-placed `.claude/skills/my-skill/`
// directory is preserved."
func TestSkillLifecycle_UserContentPreserved(t *testing.T) {
	cityPath := t.TempDir()
	sinkDir := filepath.Join(cityPath, ".claude", "skills")
	packSkills := filepath.Join(cityPath, "skills")
	t.Setenv("GC_HOME", t.TempDir())

	if err := os.MkdirAll(sinkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// User-placed regular directory at a sink path.
	userDir := filepath.Join(sinkDir, "my-skill")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "note.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// User-placed regular file at a sink path.
	userFile := filepath.Join(sinkDir, "notes.txt")
	if err := os.WriteFile(userFile, []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeLifecycleSkill(t, packSkills, "plan", "body\n")
	materialiseAndAssertSkills(t, cityPath, sinkDir, []string{"plan"})

	// Both user artefacts must survive.
	body, err := os.ReadFile(filepath.Join(userDir, "note.md"))
	if err != nil || string(body) != "mine" {
		t.Errorf("user dir contents corrupted: body=%q err=%v", string(body), err)
	}
	body, err = os.ReadFile(userFile)
	if err != nil || string(body) != "user" {
		t.Errorf("user file contents corrupted: body=%q err=%v", string(body), err)
	}
}

// materialiseAndAssertSkills runs one materialisation pass and
// returns the hash for each materialised skill — the same hash the
// Phase 3B FingerprintExtra["skills:<name>"] entry would carry into
// the agent's config fingerprint.
func materialiseAndAssertSkills(t *testing.T, cityPath, sinkDir string, wantNames []string) map[string]string {
	t.Helper()

	cat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
	if err != nil {
		t.Fatalf("LoadCityCatalog: %v", err)
	}
	desired := materialize.EffectiveSet(cat, materialize.AgentCatalog{})
	owned := append([]string{}, cat.OwnedRoots...)

	res, err := materialize.Run(materialize.Request{
		SinkDir:     sinkDir,
		Desired:     desired,
		OwnedRoots:  owned,
		LegacyNames: materialize.LegacyStubNames(),
	})
	if err != nil {
		t.Fatalf("MaterializeAgent: %v", err)
	}
	if len(res.Warnings) > 0 {
		t.Logf("materialize warnings: %v", res.Warnings)
	}

	haveNames := append([]string{}, res.Materialized...)
	if !reflect.DeepEqual(haveNames, wantNames) {
		t.Errorf("materialized = %v, want %v", haveNames, wantNames)
	}

	hashes := make(map[string]string, len(desired))
	for _, e := range desired {
		hashes[e.Name] = runtime.HashPathContent(e.Source)
		// Assert the symlink actually exists and points at the expected source.
		link := filepath.Join(sinkDir, e.Name)
		info, err := os.Lstat(link)
		if err != nil {
			t.Errorf("lstat %q: %v", link, err)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%q not a symlink", link)
			continue
		}
		tgt, _ := os.Readlink(link)
		if !strings.HasSuffix(tgt, filepath.Join("skills", e.Name)) {
			t.Errorf("symlink target suffix mismatch: %q", tgt)
		}
	}
	return hashes
}

func writeLifecycleSkill(t *testing.T, skillsRoot, name, body string) {
	t.Helper()
	dir := filepath.Join(skillsRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	full := "---\nname: " + name + "\ndescription: test\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
}
