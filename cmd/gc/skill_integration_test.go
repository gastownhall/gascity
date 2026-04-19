package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

func TestIsStage2EligibleSession(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		cityProvider string
		agentSession string
		wantEligible bool
	}{
		{"default empty → tmux (eligible)", "", "", true},
		{"tmux eligible", "tmux", "", true},
		// subprocess runtime does not execute PreStart in v0.15.1 —
		// ineligible per Phase 3 pass-1 review.
		{"subprocess ineligible (no PreStart execution)", "subprocess", "", false},
		{"k8s ineligible", "k8s", "", false},
		{"acp city ineligible", "acp", "", false},
		{"hybrid ineligible", "hybrid", "", false},
		{"exec prefix ineligible", "exec:./run.sh", "", false},
		{"fake ineligible", "fake", "", false},
		{"tmux + acp agent → ineligible", "tmux", "acp", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			agent := &config.Agent{Session: c.agentSession}
			got := isStage2EligibleSession(c.cityProvider, agent)
			if got != c.wantEligible {
				t.Fatalf("isStage2EligibleSession(%q, %q) = %v, want %v",
					c.cityProvider, c.agentSession, got, c.wantEligible)
			}
		})
	}
}

func TestAgentScopeRoot(t *testing.T) {
	t.Parallel()
	rigs := []config.Rig{
		{Name: "fe", Path: "/rigs/fe"},
		{Name: "be", Path: "/rigs/be"},
	}
	cases := []struct {
		name  string
		agent config.Agent
		want  string
	}{
		{"city-scoped returns cityPath", config.Agent{Scope: "city"}, "/city"},
		{"rig-scoped returns rig path", config.Agent{Scope: "rig", Dir: "fe"}, "/rigs/fe"},
		{"empty scope defaults to rig", config.Agent{Dir: "be"}, "/rigs/be"},
		{"unknown rig falls back to cityPath", config.Agent{Scope: "rig", Dir: "unknown"}, "/city"},
		{"empty dir rig-scope falls back", config.Agent{Scope: "rig"}, "/city"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := agentScopeRoot(&c.agent, "/city", rigs)
			if got != c.want {
				t.Fatalf("agentScopeRoot(%+v) = %q, want %q", c.agent, got, c.want)
			}
		})
	}
}

func TestAgentRigScopeName(t *testing.T) {
	t.Parallel()

	rigs := []config.Rig{
		{Name: "fe", Path: "/rigs/fe"},
		{Name: "be", Path: "/rigs/be"},
	}
	cases := []struct {
		name  string
		agent *config.Agent
		want  string
	}{
		{name: "nil agent", agent: nil, want: ""},
		{name: "city-scoped matching dir stays city", agent: &config.Agent{Scope: "city", Dir: "fe"}, want: ""},
		{name: "rig-scoped matching dir uses rig", agent: &config.Agent{Scope: "rig", Dir: "fe"}, want: "fe"},
		{name: "empty scope matching dir defaults to rig", agent: &config.Agent{Dir: "be"}, want: "be"},
		{name: "plain dir not a rig", agent: &config.Agent{Dir: "workdir"}, want: ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := agentRigScopeName(c.agent, rigs); got != c.want {
				t.Fatalf("agentRigScopeName(%+v) = %q, want %q", c.agent, got, c.want)
			}
		})
	}
}

func TestEffectiveSkillsForAgentFourBranches(t *testing.T) {
	t.Parallel()

	// Build a tiny catalog with one shared entry.
	tmp := t.TempDir()
	sharedSkill := filepath.Join(tmp, "shared", "plan")
	mustCreateSkill(t, sharedSkill)
	shared := materialize.CityCatalog{
		Entries:    []materialize.SkillEntry{{Name: "plan", Source: sharedSkill, Origin: "city"}},
		OwnedRoots: []string{filepath.Dir(sharedSkill)},
	}

	// Branch 1: eligible + shared catalog.
	t.Run("claude eligible shared catalog", func(t *testing.T) {
		t.Parallel()
		a := &config.Agent{Provider: "claude"}
		desired := effectiveSkillsForAgent(&shared, a, "", nil)
		if len(desired) != 1 || desired[0].Name != "plan" {
			t.Fatalf("desired = %+v", desired)
		}
	})

	// Branch 2: ineligible provider (copilot) returns nothing — no sink.
	t.Run("copilot provider has no sink", func(t *testing.T) {
		t.Parallel()
		a := &config.Agent{Provider: "copilot"}
		desired := effectiveSkillsForAgent(&shared, a, "", nil)
		if desired != nil {
			t.Fatalf("want nil, got %+v", desired)
		}
	})

	// Branch 3: eligible provider + per-agent local skills overlay.
	t.Run("agent-local catalog overlay", func(t *testing.T) {
		t.Parallel()
		agentDir := filepath.Join(tmp, "agents", "mayor", "skills")
		mustCreateSkill(t, filepath.Join(agentDir, "private"))
		a := &config.Agent{Provider: "codex", SkillsDir: agentDir}
		desired := effectiveSkillsForAgent(&shared, a, "", nil)
		names := namesOf(desired)
		if !reflect.DeepEqual(names, []string{"plan", "private"}) {
			t.Fatalf("names = %v", names)
		}
	})

	// Branch 4: city catalog nil (load failed) — agent-local skills still work.
	t.Run("nil city catalog + agent-local only", func(t *testing.T) {
		t.Parallel()
		agentDir := filepath.Join(tmp, "agents", "solo", "skills")
		mustCreateSkill(t, filepath.Join(agentDir, "only"))
		a := &config.Agent{Provider: "gemini", SkillsDir: agentDir}
		desired := effectiveSkillsForAgent(nil, a, "", nil)
		if len(desired) != 1 || desired[0].Name != "only" {
			t.Fatalf("desired = %+v", desired)
		}
	})

	// Empty catalog + no agent skills → nothing.
	t.Run("no skills anywhere", func(t *testing.T) {
		t.Parallel()
		a := &config.Agent{Provider: "claude"}
		empty := materialize.CityCatalog{}
		desired := effectiveSkillsForAgent(&empty, a, "", nil)
		if desired != nil {
			t.Fatalf("want nil, got %+v", desired)
		}
	})

	// Workspace-provider fallback: agent without explicit provider
	// inherits from workspace. Regression for the latent bug found
	// during Phase 4B acceptance testing.
	t.Run("agent inherits workspace provider", func(t *testing.T) {
		t.Parallel()
		a := &config.Agent{Name: "mayor"} // Provider="" — inherits.
		desired := effectiveSkillsForAgent(&shared, a, "claude", nil)
		if len(desired) != 1 || desired[0].Name != "plan" {
			t.Fatalf("desired = %+v (workspace-provider fallback broken)", desired)
		}
	})

	// Agent-catalog load error surfaces on stderr — regression for
	// Phase 3 pass-1 Claude finding #4. Use a directory with
	// no-read permissions so os.ReadDir fails with an error that
	// is NOT ErrNotExist (which readSkillDir handles specially).
	t.Run("agent catalog load error logs to stderr", func(t *testing.T) {
		t.Parallel()
		unreadable := filepath.Join(tmp, "unreadable-skills")
		if err := os.Mkdir(unreadable, 0o000); err != nil {
			t.Fatal(err)
		}
		// Restore perms at cleanup so t.TempDir can remove the tree.
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

		// Running as root would bypass the permissions check. Skip if
		// the unreadable dir is actually readable (e.g., in CI root).
		if _, err := os.ReadDir(unreadable); err == nil {
			t.Skip("environment ignores chmod 000 (likely running as root)")
		}

		a := &config.Agent{Name: "mayor", Provider: "claude", SkillsDir: unreadable}
		var buf strings.Builder
		_ = effectiveSkillsForAgent(&shared, a, "", &buf)
		if !strings.Contains(buf.String(), "LoadAgentCatalog") {
			t.Errorf("expected stderr to mention LoadAgentCatalog, got %q", buf.String())
		}
	})
}

func TestSharedSkillCatalogForAgentDoesNotFallBackWhenRigCatalogFails(t *testing.T) {
	t.Parallel()

	cityPath := t.TempDir()
	writeSkillSource(t, filepath.Join(cityPath, "skills", "city-shared"))

	badRigCatalog := filepath.Join(cityPath, "broken-rig-catalog")
	if err := os.Mkdir(badRigCatalog, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(badRigCatalog, 0o755) })
	if _, err := os.ReadDir(badRigCatalog); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Rigs:          []config.Rig{{Name: "fe", Path: filepath.Join(cityPath, "rigs", "fe")}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{
			"fe": {{
				SourceDir:   badRigCatalog,
				BindingName: "ops",
				PackName:    "helper",
			}},
		},
	}

	var stderr strings.Builder
	params := newAgentBuildParams("test-city", cityPath, cfg, nil, time.Now(), nil, &stderr)
	if params.skillCatalog == nil {
		t.Fatal("city skill catalog should still load")
	}
	if got := params.sharedSkillCatalogForAgent(&config.Agent{Name: "rig-agent", Scope: "rig", Dir: "fe"}); got != nil {
		t.Fatalf("sharedSkillCatalogForAgent() = %+v, want nil when rig catalog load fails", got)
	}
	if !strings.Contains(stderr.String(), `LoadCityCatalog rig "fe"`) {
		t.Fatalf("stderr = %q, want rig catalog load error", stderr.String())
	}
}

func TestMergeSkillFingerprintEntries(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	mustCreateSkill(t, filepath.Join(tmp, "alpha"))
	mustCreateSkill(t, filepath.Join(tmp, "beta"))
	desired := []materialize.SkillEntry{
		{Name: "alpha", Source: filepath.Join(tmp, "alpha")},
		{Name: "beta", Source: filepath.Join(tmp, "beta")},
	}

	// Nil fpExtra: allocates and populates.
	got := mergeSkillFingerprintEntries(nil, desired)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2; %+v", len(got), got)
	}
	for _, name := range []string{"skills:alpha", "skills:beta"} {
		if got[name] == "" {
			t.Errorf("missing or empty %q in %+v", name, got)
		}
	}

	// Non-nil fpExtra: preserves existing keys.
	base := map[string]string{"pool.max": "3"}
	got = mergeSkillFingerprintEntries(base, desired)
	if got["pool.max"] != "3" {
		t.Errorf("existing key dropped: %+v", got)
	}
	if got["skills:alpha"] == "" {
		t.Errorf("skills:alpha missing: %+v", got)
	}

	// Empty desired: returns input unchanged.
	orig := map[string]string{"x": "y"}
	got = mergeSkillFingerprintEntries(orig, nil)
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("empty desired modified map: got %+v, want %+v", got, orig)
	}
}

// TestMergeSkillFingerprintEntriesPrefixPartitioning asserts that the
// "skills:" prefix keeps entries from colliding with other
// fpExtra keys like "skills_dir" that might conceivably be added later.
func TestMergeSkillFingerprintEntriesPrefixPartitioning(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mustCreateSkill(t, filepath.Join(tmp, "x"))
	desired := []materialize.SkillEntry{{Name: "x", Source: filepath.Join(tmp, "x")}}

	got := mergeSkillFingerprintEntries(nil, desired)
	for k := range got {
		if !strings.HasPrefix(k, "skills:") {
			t.Errorf("non-prefix key present: %q", k)
		}
	}
}

func TestEffectiveInjectAssignedSkills(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	cases := []struct {
		name string
		ptr  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", &yes, true},
		{"explicit false", &no, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			agent := &config.Agent{InjectAssignedSkills: c.ptr}
			if got := effectiveInjectAssignedSkills(agent); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
	if effectiveInjectAssignedSkills(nil) {
		t.Error("nil agent should not inject")
	}
}

func TestBuildAssignedSkillsPromptFragmentPartitions(t *testing.T) {
	t.Parallel()
	city := &materialize.CityCatalog{
		Entries: []materialize.SkillEntry{
			{Name: "code-review", Source: "/x", Origin: "city", Description: "Review pull requests"},
			{Name: "gc-work", Source: "/y", Origin: "core", Description: "Working with beads"},
			{Name: "planning", Source: "/z", Origin: "city", Description: "Shared planning"},
		},
	}
	agentCat := materialize.AgentCatalog{
		Entries: []materialize.SkillEntry{
			{Name: "mayor-planning", Source: "/a", Origin: "agent", Description: "Mayor-only strategy"},
			// Overrides shared "planning" — should NOT appear in the shared section.
			{Name: "planning", Source: "/b", Origin: "agent", Description: "Mayor's planning override"},
		},
	}
	a := &config.Agent{Name: "mayor", Scope: "city"}
	got := buildAssignedSkillsPromptFragment(a, city, agentCat)

	mustContain := []string{
		"## Skills available to this session",
		"You are `mayor`",
		"### Assigned to you",
		"`mayor-planning` — Mayor-only strategy",
		"`planning` — Mayor's planning override",
		"### Shared in this scope",
		"`code-review` — Review pull requests *(city)*",
		"`gc-work` — Working with beads *(core)*",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("fragment missing %q:\n%s", want, got)
		}
	}

	// Agent-local "planning" must SHADOW the city "planning" from the
	// shared section — agents should see their override, not the
	// conflicting shared entry.
	if strings.Contains(got, "Shared planning") {
		t.Errorf("shared section still lists the shadowed city planning entry:\n%s", got)
	}
}

func TestBuildAssignedSkillsPromptFragmentEmptyInputs(t *testing.T) {
	t.Parallel()
	a := &config.Agent{Name: "x"}
	if got := buildAssignedSkillsPromptFragment(a, nil, materialize.AgentCatalog{}); got != "" {
		t.Errorf("empty inputs should return empty fragment, got: %q", got)
	}
	// City-only (no agent-local) still renders, just without the Assigned section.
	city := &materialize.CityCatalog{
		Entries: []materialize.SkillEntry{
			{Name: "gc-work", Source: "/x", Origin: "core", Description: "Work stuff"},
		},
	}
	got := buildAssignedSkillsPromptFragment(a, city, materialize.AgentCatalog{})
	if got == "" {
		t.Fatal("expected non-empty fragment when city catalog has entries")
	}
	if strings.Contains(got, "### Assigned to you") {
		t.Errorf("should not render Assigned section when no agent-local skills:\n%s", got)
	}
	if !strings.Contains(got, "### Shared") {
		t.Errorf("should render Shared section:\n%s", got)
	}
}

func TestBuildAssignedSkillsPromptFragmentAgentOnlyNoCity(t *testing.T) {
	t.Parallel()
	a := &config.Agent{Name: "solo"}
	agentCat := materialize.AgentCatalog{
		Entries: []materialize.SkillEntry{{Name: "only-mine", Source: "/x", Origin: "agent"}},
	}
	got := buildAssignedSkillsPromptFragment(a, nil, agentCat)
	if got == "" {
		t.Fatal("agent-local-only should still render")
	}
	if !strings.Contains(got, "### Assigned to you") {
		t.Errorf("missing Assigned section:\n%s", got)
	}
	if strings.Contains(got, "### Shared") {
		t.Errorf("Shared section should not render when no city catalog:\n%s", got)
	}
}

func TestBuildAssignedSkillsPromptFragmentOmitsDescriptionWhenMissing(t *testing.T) {
	t.Parallel()
	a := &config.Agent{Name: "x"}
	city := &materialize.CityCatalog{
		Entries: []materialize.SkillEntry{
			{Name: "bare", Source: "/x", Origin: "city"}, // no Description
		},
	}
	got := buildAssignedSkillsPromptFragment(a, city, materialize.AgentCatalog{})
	// Name present, no dash-separator.
	if !strings.Contains(got, "`bare`") {
		t.Errorf("missing bare skill name:\n%s", got)
	}
	if strings.Contains(got, "`bare` — ") {
		t.Errorf("should not render em-dash separator when description is empty:\n%s", got)
	}
}

func TestAppendMaterializeSkillsPreStart(t *testing.T) {
	t.Parallel()
	existing := []string{"mkdir -p .cache", "./setup.sh"}
	got := appendMaterializeSkillsPreStart(existing, "hello-world/polecat", "/worktrees/polecat-1")
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d (%v)", len(got), got)
	}
	// User-configured entries come first (per spec: "appended ... user
	// setup runs first, materialize-skills runs last").
	if got[0] != "mkdir -p .cache" || got[1] != "./setup.sh" {
		t.Errorf("user entries reordered: %v", got)
	}
	// Final entry is the materialize-skills command with both flags
	// properly quoted.
	last := got[2]
	if !strings.Contains(last, "internal materialize-skills") {
		t.Errorf("materialize-skills command missing: %q", last)
	}
	if !strings.Contains(last, "--agent") || !strings.Contains(last, "hello-world/polecat") {
		t.Errorf("--agent flag missing: %q", last)
	}
	if !strings.Contains(last, "--workdir") || !strings.Contains(last, "/worktrees/polecat-1") {
		t.Errorf("--workdir flag missing: %q", last)
	}
	// gc binary reference must go through ${GC_BIN:-gc} so the runtime
	// env provides the authoritative binary path.
	if !strings.Contains(last, "${GC_BIN:-gc}") {
		t.Errorf("GC_BIN reference missing: %q", last)
	}
}

// helpers

func mustCreateSkill(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + filepath.Base(dir) + "\ndescription: test\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func namesOf(entries []materialize.SkillEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
