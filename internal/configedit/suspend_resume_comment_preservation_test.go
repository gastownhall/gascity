package configedit_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stripTOMLComments returns s as a slice of non-blank lines with any
// trailing comment removed. Fixtures in this file never put '#' inside a
// quoted value, so a plain substring split is enough to separate content
// from commentary.
func stripTOMLComments(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// diffContentLines compares before/after with comments and blank lines
// stripped, as a multiset of lines, and reports which content lines were
// lost (removed) or introduced (added). It is order-insensitive so it
// isolates genuine content changes (a key gaining/losing a value, a table
// appearing/disappearing) from harmless reordering, and it still catches
// array reflow: a single-line array that becomes multi-line no longer
// matches its old line verbatim, so it shows up as one removed + N added.
func diffContentLines(before, after string) (removed, added []string) {
	beforeCount := map[string]int{}
	for _, l := range stripTOMLComments(before) {
		beforeCount[l]++
	}
	afterCount := map[string]int{}
	for _, l := range stripTOMLComments(after) {
		afterCount[l]++
	}
	for l, bc := range beforeCount {
		if ac := afterCount[l]; bc > ac {
			for i := 0; i < bc-ac; i++ {
				removed = append(removed, l)
			}
		}
	}
	for l, ac := range afterCount {
		if bc := beforeCount[l]; ac > bc {
			for i := 0; i < ac-bc; i++ {
				added = append(added, l)
			}
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return removed, added
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSuspendAgent_Inline_PreservesComments pins ga-gc16k3: suspending an
// inline [[agent]] must not rewrite city.toml through the lossy
// Editor.write -> ... -> City.Marshal struct round trip, which drops every
// comment and can materialize absent tables as explicit zero values.
func TestSuspendAgent_Inline_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `# City-level rationale: primary orchestration city for gascity.
[workspace]
name = "test-city"

# The mayor plans; coding agents work. Kept inline so patches never
# shadow its suspended state.
[[agent]]
name = "mayor"
provider = "claude"
pre_start = ["echo starting", "echo ready"]
`)
	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("mayor"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	after := string(mustReadFile(t, path))

	for _, want := range []string{
		"# City-level rationale: primary orchestration city for gascity.",
		"# The mayor plans; coding agents work. Kept inline so patches never",
		"# shadow its suspended state.",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("suspend dropped comment %q; city.toml now:\n%s", want, after)
		}
	}

	if strings.Contains(after, "[dolt]") {
		t.Errorf("suspend materialized an absent [dolt] table; city.toml now:\n%s", after)
	}

	if !strings.Contains(after, `pre_start = ["echo starting", "echo ready"]`) {
		t.Errorf("suspend reflowed the single-line pre_start array; city.toml now:\n%s", after)
	}

	removed, added := diffContentLines(before, after)
	if len(removed) != 0 {
		t.Errorf("suspend removed unrelated content lines %v; city.toml now:\n%s", removed, after)
	}
	if want := []string{"suspended = true"}; !stringSlicesEqual(added, want) {
		t.Errorf("suspend changed unrelated content; added lines = %v, want %v; city.toml now:\n%s", added, want, after)
	}
}

// TestResumeAgent_Inline_PreservesComments mirrors
// TestSuspendAgent_Inline_PreservesComments for the resume direction: the
// suspended = true line must disappear (Suspended is bool,omitempty) and
// nothing else should move.
func TestResumeAgent_Inline_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `# City-level rationale: primary orchestration city for gascity.
[workspace]
name = "test-city"

# The mayor plans; coding agents work. Kept inline so patches never
# shadow its suspended state.
[[agent]]
name = "mayor"
provider = "claude"
pre_start = ["echo starting", "echo ready"]
suspended = true
`)
	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("mayor"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	after := string(mustReadFile(t, path))

	for _, want := range []string{
		"# City-level rationale: primary orchestration city for gascity.",
		"# The mayor plans; coding agents work. Kept inline so patches never",
		"# shadow its suspended state.",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("resume dropped comment %q; city.toml now:\n%s", want, after)
		}
	}

	if !strings.Contains(after, `pre_start = ["echo starting", "echo ready"]`) {
		t.Errorf("resume reflowed the single-line pre_start array; city.toml now:\n%s", after)
	}

	removed, added := diffContentLines(before, after)
	if want := []string{"suspended = true"}; !stringSlicesEqual(removed, want) {
		t.Errorf("resume changed unrelated content; removed lines = %v, want %v; city.toml now:\n%s", removed, want, after)
	}
	if len(added) != 0 {
		t.Errorf("resume added unrelated content lines %v; city.toml now:\n%s", added, after)
	}
}

// TestSuspendAgent_PackDerived_PreservesComments covers the
// [[patches.agent]] branch of mutateAgentSuspended (a pack-declared agent,
// not a city-local convention agent). This fixture's fresh patch append now
// takes the surgical AppendAgentPatchSuspendedForEdit path and returns
// ErrUnmodified, so existing comments must survive and the only new content
// should be the appended patch block.
func TestSuspendAgent_PackDerived_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `# City-level rationale: primary orchestration city for gascity.
[workspace]
name = "test-city"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "worker"
provider = "claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A conventional prompt template at the discovery location must not
	// trigger the agent.toml write path when a pack [[agent]] entry exists.
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.SuspendAgent("worker"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	after := string(mustReadFile(t, path))

	if !strings.Contains(after, "# City-level rationale: primary orchestration city for gascity.") {
		t.Errorf("suspend dropped comment; city.toml now:\n%s", after)
	}

	removed, added := diffContentLines(before, after)
	if len(removed) != 0 {
		t.Errorf("suspend removed unrelated content lines %v; city.toml now:\n%s", removed, after)
	}
	wantAdded := []string{
		`[[patches.agent]]`,
		`dir = ""`,
		`name = "worker"`,
		`suspended = true`,
	}
	sort.Strings(wantAdded)
	if !stringSlicesEqual(added, wantAdded) {
		t.Errorf("suspend changed unrelated content; added lines = %v, want %v; city.toml now:\n%s", added, wantAdded, after)
	}
}

// TestResumeAgent_LocalDiscovered_StripsLegacyPatchSuspended_PreservesComments
// covers the fixture shape the round-1 review of ga-gc16k3 flagged as
// uncovered: a locally-discovered agent (agents/<name>/agent.toml) that also
// carries a legacy [[patches.agent]] suspended override. Resuming such an
// agent must strip the now-redundant patch override without falling through
// to the lossy Editor.write struct-marshal rewrite -- mutateAgentSuspended's
// OriginDerived branch returned plain nil whenever StripAgentPatchSuspended
// reported a change, and EditExpanded treats plain nil as "write the raw
// config," dropping every comment. Functional (non-comment) behavior for
// this exact fixture is already covered by
// TestResumeAgent_StripsLegacyPatchSuspended in configedit_test.go; this
// test adds the comment-preservation assertion that test predates.
func TestResumeAgent_LocalDiscovered_StripsLegacyPatchSuspended_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `# City-level rationale: primary orchestration city for gascity.
[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = true
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("suspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	if err := ed.ResumeAgent("worker"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	after := string(mustReadFile(t, path))

	if !strings.Contains(after, "# City-level rationale: primary orchestration city for gascity.") {
		t.Errorf("resume dropped comment; city.toml now:\n%s", after)
	}

	if strings.Contains(after, "[[patches.agent]]") {
		t.Errorf("resume should strip the legacy patch override; city.toml now:\n%s", after)
	}

	removed, added := diffContentLines(before, after)
	wantRemoved := []string{
		`[[patches.agent]]`,
		`dir = ""`,
		`name = "worker"`,
		`suspended = true`,
	}
	sort.Strings(wantRemoved)
	if !stringSlicesEqual(removed, wantRemoved) {
		t.Errorf("resume changed unexpected content; removed lines = %v, want %v; city.toml now:\n%s", removed, wantRemoved, after)
	}
	if len(added) != 0 {
		t.Errorf("resume added unrelated content lines %v; city.toml now:\n%s", added, after)
	}
}

// TestResumeAgent_LocalDiscovered_AmbiguousLegacyPatch_RefusesLossyRewrite
// pins ga-47m118 round 3: the sibling failure mode to
// TestResumeAgent_LocalDiscovered_StripsLegacyPatchSuspended_PreservesComments's
// single-block success case above. Here two duplicate [[patches.agent]]
// blocks share the same (dir, name) identity -- a leftover shape
// findTOMLArrayBlock refuses to guess between -- so
// StripAgentPatchSuspendedForEdit reports config.ErrSurgicalAgentEditUnsupported.
// mutateAgentSuspended's OriginDerived branch used to swallow that error and
// return plain nil, which EditExpanded treats as "write the raw config,"
// silently deleting every comment in city.toml while leaving the ambiguous
// patch blocks in place. Resuming must instead surface the refusal and
// leave city.toml byte-for-byte untouched.
func TestResumeAgent_LocalDiscovered_AmbiguousLegacyPatch_RefusesLossyRewrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `# City-level rationale: primary orchestration city for gascity.
[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = true

[[patches.agent]]
dir = ""
name = "worker"
suspended = true
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "agents", "worker")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("You are the worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("suspended = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := string(mustReadFile(t, path))

	ed := configedit.NewEditor(fsys.OSFS{}, path)
	err := ed.ResumeAgent("worker")

	if err == nil {
		t.Fatal("expected ResumeAgent to refuse the ambiguous legacy patch, got nil error")
	}
	if !errors.Is(err, config.ErrSurgicalAgentEditUnsupported) {
		t.Errorf("expected error wrapping config.ErrSurgicalAgentEditUnsupported, got: %v", err)
	}

	after := string(mustReadFile(t, path))
	if after != before {
		t.Errorf("refused resume must leave city.toml byte-for-byte unchanged; diff:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
