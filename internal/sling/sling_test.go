package sling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// --- Test helpers ---

type fakeRunnerRule struct {
	prefix string
	out    string
	err    error
}

type fakeRunner struct {
	calls []string
	dirs  []string
	envs  []map[string]string
	rules []fakeRunnerRule
}

type getErrStore struct {
	beads.Store
	err error
}

func (s *getErrStore) Get(_ string) (beads.Bead, error) {
	return beads.Bead{}, s.err
}

type closeAllFailMemStore struct {
	*beads.MemStore
	failCloseAllCalls   int
	failSetMetadataID   string
	failSetMetadataKey  string
	failSetMetadataCall int
}

func (s *closeAllFailMemStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.failCloseAllCalls > 0 {
		s.failCloseAllCalls--
		if len(ids) > 0 {
			return 0, fmt.Errorf("forced close failure for %s", ids[0])
		}
		return 0, fmt.Errorf("forced close failure")
	}
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *closeAllFailMemStore) SetMetadata(id, key, value string) error {
	if s.failSetMetadataCall > 0 && id == s.failSetMetadataID && key == s.failSetMetadataKey {
		s.failSetMetadataCall--
		return fmt.Errorf("forced metadata failure for %s %s", id, key)
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

func (r *fakeRunner) on(prefix string, err error) {
	r.rules = append(r.rules, fakeRunnerRule{prefix: prefix, err: err})
}

func (r *fakeRunner) run(dir, command string, env map[string]string) (string, error) {
	r.calls = append(r.calls, command)
	r.dirs = append(r.dirs, dir)
	r.envs = append(r.envs, env)
	for _, rule := range r.rules {
		if strings.Contains(command, rule.prefix) {
			return rule.out, rule.err
		}
	}
	return "", nil
}

func intPtr(v int) *int          { return &v }
func stringPtr(v string) *string { return &v }

func seededStore(ids ...string) *beads.MemStore {
	seed := make([]beads.Bead, 0, len(ids))
	for _, id := range ids {
		seed = append(seed, beads.Bead{
			ID:       id,
			Title:    id,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{},
		})
	}
	return beads.NewMemStoreFrom(0, seed, nil)
}

// testResolver implements AgentResolver for tests using exact match.
type testResolver struct{}

func (testResolver) ResolveAgent(cfg *config.City, name, _ string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if a.QualifiedName() == name || a.Name == name {
			return a, true
		}
	}
	return config.Agent{}, false
}

// testNotifier implements Notifier as a no-op.
type testNotifier struct{}

func (testNotifier) PokeController(_ string)      {}
func (testNotifier) PokeControlDispatch(_ string) {}

func testDeps(cfg *config.City, sp runtime.Provider, runner SlingRunner) SlingDeps {
	if cfg != nil && len(cfg.FormulaLayers.City) == 0 {
		cfg.FormulaLayers.City = []string{sharedTestFormulaDir}
	}
	return SlingDeps{
		CityName: "test-city",
		CityPath: sharedTestCityDir,
		Cfg:      cfg,
		SP:       sp,
		Runner:   runner,
		Store:    beads.NewMemStore(),
		StoreRef: "city:test-city",
		Resolver: testResolver{},
		Notify:   testNotifier{},
	}
}

func testOpts(a config.Agent, beadOrFormula string) SlingOpts {
	return SlingOpts{Target: a, BeadOrFormula: beadOrFormula}
}

var (
	sharedTestFormulaDir string
	sharedTestCityDir    string
)

func init() {
	dir, err := os.MkdirTemp("", "gc-sling-test-formulas-*")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{
		"code-review", "mol-feature", "mol-polecat-work", "mol-do-work",
		"mol-refinery-patrol", "review", "build", "test-formula",
		"bad-formula", "mol-polecat-pr", "custom-formula",
		"mol-digest", "mol-cleanup", "mol-db-health", "mol-health-check",
		"my-formula", "convoy-formula",
	} {
		content := fmt.Sprintf("formula = %q\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n", name)
		_ = os.WriteFile(filepath.Join(dir, name+".formula.toml"), []byte(content), 0o644)
	}
	sharedTestFormulaDir = dir

	cityDir, err := os.MkdirTemp("", "gc-sling-test-city-*")
	if err != nil {
		panic(err)
	}
	sharedTestCityDir = cityDir
}

// --- Pure helper tests ---

func TestBuildSlingCommandSling(t *testing.T) {
	tests := []struct {
		template string
		beadID   string
		want     string
	}{
		{"bd update {} --set-metadata gc.routed_to=mayor", "BL-42", "bd update 'BL-42' --set-metadata gc.routed_to=mayor"},
		{"bd update {} --add-label=pool:hw/polecat", "XY-7", "bd update 'XY-7' --add-label=pool:hw/polecat"},
		{"custom {} script {}", "ID-1", "custom 'ID-1' script 'ID-1'"},
	}
	for _, tt := range tests {
		got := BuildSlingCommand(tt.template, tt.beadID)
		if got != tt.want {
			t.Errorf("BuildSlingCommand(%q, %q) = %q, want %q", tt.template, tt.beadID, got, tt.want)
		}
	}
}

func TestBuildSlingCommandForAgentParseErrorRedactsTemplate(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	a := config.Agent{Name: "worker"}
	template := "custom {} --route={{.Rig"

	got, warning := BuildSlingCommandForAgent("sling_query", template, "BL-42", cityPath, "", a, nil)

	if got != "custom 'BL-42' --route={{.Rig" {
		t.Fatalf("BuildSlingCommandForAgent() = %q, want %q", got, "custom 'BL-42' --route={{.Rig")
	}
	if !strings.Contains(warning, "sling_query") {
		t.Fatalf("warning missing field name: %q", warning)
	}
	if strings.Contains(warning, template) {
		t.Fatalf("warning should redact raw template, got %q", warning)
	}
}

func TestBuildSlingCommandForAgentExpandsPathContextPlaceholders(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigPath := filepath.Join(cityPath, "frontend")
	a := config.Agent{Name: "worker", Dir: "frontend"}
	rigs := []config.Rig{{Name: "frontend", Path: rigPath}}

	got, _ := BuildSlingCommandForAgent(
		"sling_query",
		"custom {} --route={{.CityName}}/{{.Rig}}/{{.AgentBase}}",
		"BL-42",
		cityPath,
		"",
		a,
		rigs,
	)

	if want := "custom 'BL-42' --route=demo-city/frontend/worker"; got != want {
		t.Fatalf("BuildSlingCommandForAgent() = %q, want %q", got, want)
	}
}

func TestCheckBeadStateCustomBDQueryNoIdempotency(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{
		Name:       "mayor",
		SlingQuery: "bd update {} --set-metadata gc.routed_to=mayor --set-metadata owner=ops",
	}, SlingDeps{})

	if result.Idempotent {
		t.Fatal("expected Idempotent=false for user-defined bd sling_query")
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(result.Warnings), result.Warnings)
	}
	if !strings.Contains(result.Warnings[0], "already routed") {
		t.Fatalf("expected routing warning, got %q", result.Warnings[0])
	}
}

func TestCheckBeadStatePinnedDefaultBDQueryRemainsIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{
		Name:       "mayor",
		SlingQuery: "bd   update {}   --set-metadata gc.routed_to=mayor",
	}, SlingDeps{})

	if !result.Idempotent {
		t.Fatalf("expected Idempotent=true for pinned default sling_query, got %+v", result)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected no warnings for pinned default sling_query, got %v", result.Warnings)
	}
}

func TestBeadPrefixSling(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"BL-42", "bl"},
		{"HW-1", "hw"},
		{"FE-123", "fe"},
		{"DEMO--42", "demo"},
		{"projectwrenunity-abc", "projectwrenunity"},
		{"A-B-C", "a"},
		{"A-", "a"},
		{"", ""},
		{"nohyphen", ""},
		{"-1", ""},
	}
	for _, tt := range tests {
		got := BeadPrefix(tt.id)
		if got != tt.want {
			t.Errorf("BeadPrefix(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestBeadPrefixForCityLongestMatch(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "agent", Path: "/agent", Prefix: "agent"},
			{Name: "agent-diagnostics", Path: "/ad", Prefix: "agent-diagnostics"},
			{Name: "fe", Path: "/fe", Prefix: "fe"},
		},
	}
	tests := []struct {
		id   string
		want string
	}{
		{"agent-diagnostics-hnn", "agent-diagnostics"},
		{"agent-diagnostics-spawn-storm", "agent-diagnostics"},
		{"agent-x1", "agent"},
		{"fe-42", "fe"},
		{"unknown-7", "unknown"}, // falls back to BeadPrefix.
		{"", ""},
	}
	for _, tt := range tests {
		got := BeadPrefixForCity(cfg, tt.id)
		if got != tt.want {
			t.Errorf("BeadPrefixForCity(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestBeadPrefixForCityFallsBackToBeadPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "fe", Path: "/fe", Prefix: "fe"}},
	}
	// Unknown prefix → fall back to BeadPrefix's first-dash split.
	if got := BeadPrefixForCity(cfg, "unknown-7"); got != "unknown" {
		t.Errorf("BeadPrefixForCity(unknown-7) = %q, want unknown", got)
	}
	// Nil cfg → fall back to BeadPrefix.
	if got := BeadPrefixForCity(nil, "fe-42"); got != "fe" {
		t.Errorf("BeadPrefixForCity(nil, fe-42) = %q, want fe", got)
	}
}

func TestLooksLikeConfiguredBeadIDAcceptsHyphenatedPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "agent-diagnostics", Path: "/ad", Prefix: "agent-diagnostics"},
		},
	}
	tests := []struct {
		id   string
		want bool
	}{
		{"agent-diagnostics-hnn", true},
		{"agent-diagnostics-h1", true},
		{"agent-diagnostics-12345678", true},   // 8-char numeric suffix.
		{"agent-diagnostics-123456789", false}, // 9-char suffix exceeds cap.
		{"agent-diagnostics-", false},          // empty suffix.
		{"agent-diagnostics-h.1", true},        // hierarchical .child.
		{"agent-diagnostics-h.x", true},
		{"agent-diagnostics-h.", true}, // trailing dot accepted (matches BeadIDParts).
		{"agent-diagnostics", false},   // no suffix dash.
	}
	for _, tt := range tests {
		got := LooksLikeConfiguredBeadID(cfg, tt.id)
		if got != tt.want {
			t.Errorf("LooksLikeConfiguredBeadID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestLooksLikeConfiguredBeadIDPrefersLongestPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "agent", Path: "/agent", Prefix: "agent"},
			{Name: "agent-diagnostics", Path: "/ad", Prefix: "agent-diagnostics"},
		},
	}
	// Both prefixes can match "agent-diagnostics-h1" via the prefix-then-validate
	// rule, but matchConfiguredBeadPrefix must pick the longest.
	if !LooksLikeConfiguredBeadID(cfg, "agent-diagnostics-h1") {
		t.Fatal("LooksLikeConfiguredBeadID(agent-diagnostics-h1) = false, want true")
	}
	// "agent-x1" only matches the shorter "agent" prefix.
	if !LooksLikeConfiguredBeadID(cfg, "agent-x1") {
		t.Fatal("LooksLikeConfiguredBeadID(agent-x1) = false, want true")
	}
}

func TestLooksLikeConfiguredBeadIDRejectsUnknownPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "fe", Path: "/fe", Prefix: "fe"}},
	}
	cases := []string{
		"unknown-42",
		"code-review-please", // no rig "code" or "code-review" configured.
		"hello-world",
		"",
		"   ",
		"fe foo",  // whitespace.
		"fe-foo!", // non-alphanumeric suffix char.
	}
	for _, c := range cases {
		if LooksLikeConfiguredBeadID(cfg, c) {
			t.Errorf("LooksLikeConfiguredBeadID(%q) = true, want false", c)
		}
	}
}

func TestLooksLikeConfiguredBeadIDAcceptsHQPrefix(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "HQ"},
	}
	if !LooksLikeConfiguredBeadID(cfg, "HQ-42") {
		t.Fatal("HQ-42 should be a configured bead ID")
	}
	if !LooksLikeConfiguredBeadID(cfg, "hq-abc") {
		t.Fatal("hq-abc should match HQ prefix case-insensitively")
	}
}

// Underscored rig prefixes (e.g. "live_docs") are common in real cities
// but were rejected by BeadIDParts' alpha-only prefix charset. The
// config-aware path matches against cfg.Rigs literally, so the broken
// charset gate is bypassed for any prefix the city has actually
// declared. Coverage parallels the bug-report cases: live_docs,
// migration_evals, scix_experiments, EnterpriseBench.
func TestLooksLikeConfiguredBeadIDAcceptsUnderscoredPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "live_docs", Path: "/ld", Prefix: "live_docs"},
			{Name: "migration_evals", Path: "/me", Prefix: "migration_evals"},
			{Name: "scix_experiments", Path: "/sx", Prefix: "scix_experiments"},
			{Name: "EnterpriseBench", Path: "/eb", Prefix: "EnterpriseBench"},
		},
	}
	tests := []struct {
		id   string
		want bool
	}{
		{"live_docs-5du", true},
		{"migration_evals-cns", true},
		{"scix_experiments-wqr.9.3", true}, // hierarchical .child suffix.
		{"EnterpriseBench-0rv.18", true},
		{"EnterpriseBench-0rv", true},
		{"live_docs-", false},    // empty suffix.
		{"live_docs", false},     // no suffix dash.
		{"unknown_rig-7", false}, // not in config.
	}
	for _, tt := range tests {
		got := LooksLikeConfiguredBeadID(cfg, tt.id)
		if got != tt.want {
			t.Errorf("LooksLikeConfiguredBeadID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestBeadPrefixForCityHandlesUnderscoredPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "live_docs", Path: "/ld", Prefix: "live_docs"},
			{Name: "migration_evals", Path: "/me", Prefix: "migration_evals"},
		},
	}
	tests := []struct {
		id   string
		want string
	}{
		{"live_docs-5du", "live_docs"},
		{"migration_evals-cns", "migration_evals"},
		{"migration_evals-cns.1", "migration_evals"},
	}
	for _, tt := range tests {
		got := BeadPrefixForCity(cfg, tt.id)
		if got != tt.want {
			t.Errorf("BeadPrefixForCity(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestRigDirForBeadHonorsUnderscoredPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "live_docs", Path: "/live-docs-rig", Prefix: "live_docs"},
		},
	}
	if got := RigDirForBead(cfg, "live_docs-5du"); got != "/live-docs-rig" {
		t.Errorf("RigDirForBead(live_docs-5du) = %q, want /live-docs-rig", got)
	}
}

// RigDirForBead returns "" in two distinct ways: the prefix doesn't
// parse at all (BeadPrefixForCity returns "") and the prefix parses
// but doesn't match any configured rig (BeadPrefix falls back to
// first-dash split for unknown prefixes). Cover both so a regression
// that conflates the branches is caught.
func TestRigDirForBeadEmptyPrefixAndUnknownRig(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "fe", Path: "/fe", Prefix: "fe"}},
	}
	// Empty input → BeadPrefixForCity returns "", short-circuits.
	if got := RigDirForBead(cfg, ""); got != "" {
		t.Errorf("RigDirForBead(\"\") = %q, want \"\"", got)
	}
	// Unknown prefix that BeadPrefix's fallback parses ("unknown")
	// but is not a configured rig: hits the FindRigByPrefix=false
	// branch.
	if got := RigDirForBead(cfg, "unknown-7"); got != "" {
		t.Errorf("RigDirForBead(unknown-7) = %q, want \"\" (no matching rig)", got)
	}
}

// configuredBeadPrefixes skips rigs whose effective prefix is empty.
// Reaching that branch requires both an empty Name and Prefix —
// validated configs reject this, but the guard exists so a malformed
// or partially-applied config can't produce an "" entry that confuses
// equal-length tiebreaks in matchConfiguredBeadPrefix.
func TestConfiguredBeadPrefixesSkipsEmptyRigPrefix(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "HQ"},
		Rigs: []config.Rig{
			{Name: "fe", Path: "/fe", Prefix: "fe"},
			{Name: "", Path: "/empty", Prefix: ""},
		},
	}
	got := configuredBeadPrefixes(cfg)
	want := []string{"HQ", "fe"}
	if len(got) != len(want) {
		t.Fatalf("configuredBeadPrefixes = %v, want %v (empty-prefix rig must be skipped)", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("configuredBeadPrefixes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRigDirForBeadHonorsHyphenatedPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "agent", Path: "/agent", Prefix: "agent"},
			{Name: "agent-diagnostics", Path: "/agent-diag", Prefix: "agent-diagnostics"},
		},
	}
	if got := RigDirForBead(cfg, "agent-diagnostics-hnn"); got != "/agent-diag" {
		t.Errorf("RigDirForBead = %q, want /agent-diag (longest configured prefix)", got)
	}
	if got := RigDirForBead(cfg, "agent-x1"); got != "/agent" {
		t.Errorf("RigDirForBead = %q, want /agent", got)
	}
}

func TestCheckCrossRigDetectsHyphenatedPrefixMismatch(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "agent", Path: "/agent", Prefix: "agent"},
			{Name: "agent-diagnostics", Path: "/ad", Prefix: "agent-diagnostics"},
		},
	}
	// First-dash BeadPrefix yields "agent" for "agent-diagnostics-hnn",
	// which falsely matches a worker in rig "agent" and lets cross-rig
	// routing through silently. The longest-prefix resolver returns
	// "agent-diagnostics", so the guard fires correctly.
	a := config.Agent{Name: "worker", Dir: "agent"}
	if msg := CheckCrossRig("agent-diagnostics-hnn", a, cfg); msg == "" {
		t.Error("expected cross-rig warning: bead in rig 'agent-diagnostics' routed to worker in rig 'agent' must not be silently permitted")
	}
}

func TestCheckCrossRigSling(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "BL"},
			{Name: "other", Path: "/other", Prefix: "OT"},
		},
	}

	t.Run("same rig allowed", func(t *testing.T) {
		a := config.Agent{Name: "worker", Dir: "myrig"}
		if msg := CheckCrossRig("BL-42", a, cfg); msg != "" {
			t.Errorf("expected no warning, got %q", msg)
		}
	})

	t.Run("different rig blocked", func(t *testing.T) {
		a := config.Agent{Name: "worker", Dir: "other"}
		if msg := CheckCrossRig("BL-42", a, cfg); msg == "" {
			t.Error("expected cross-rig warning")
		}
	})

	t.Run("city agent no block", func(t *testing.T) {
		a := config.Agent{Name: "mayor"}
		if msg := CheckCrossRig("BL-42", a, cfg); msg != "" {
			t.Errorf("expected no warning, got %q", msg)
		}
	})
}

// --- DoSling integration tests (structured result) ---

func TestDoSlingBeadToFixedAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("BL-42")
	result, err := DoSling(testOpts(a, "BL-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.BeadID != "BL-42" {
		t.Errorf("BeadID = %q, want BL-42", result.BeadID)
	}
	if result.Target != "mayor" {
		t.Errorf("Target = %q, want mayor", result.Target)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("got %d runner calls, want 1", len(runner.calls))
	}
}

func TestDoSlingSuspendedAgentWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), Suspended: true}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("BL-42")
	result, err := DoSling(testOpts(a, "BL-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.AgentSuspended {
		t.Error("expected AgentSuspended=true")
	}
}

func TestDoSlingRunnerError(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("BL-42")
	_, err := DoSling(testOpts(a, "BL-42"), deps, nil)

	if err == nil {
		t.Fatal("expected error from runner failure")
	}
}

func TestDoSlingFormulaToAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "code-review",
		IsFormula:     true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.Method != "formula" {
		t.Errorf("Method = %q, want formula", result.Method)
	}
	if result.BeadID == "" {
		t.Error("expected non-empty BeadID (wisp root)")
	}
}

func TestBuildSlingFormulaVarsSeedsRoutingNamespace(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)

	vars := BuildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{
		Name:        "polecat",
		Dir:         "hw",
		BindingName: "gastown",
	}, deps)

	if got := vars["rig_name"]; got != "hw" {
		t.Fatalf("rig_name var = %q, want hw", got)
	}
	if got := vars["binding_name"]; got != "gastown" {
		t.Fatalf("binding_name var = %q, want gastown", got)
	}
	if got := vars["binding_prefix"]; got != "gastown." {
		t.Fatalf("binding_prefix var = %q, want gastown.", got)
	}
}

func TestBuildSlingFormulaVarsPreservesExplicitRoutingNamespace(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)

	vars := BuildSlingFormulaVars("mol-polecat-work", "HW-42", []string{
		"rig_name=override-rig",
		"binding_name=override-binding",
		"binding_prefix=override.",
	}, config.Agent{
		Name:        "polecat",
		Dir:         "hw",
		BindingName: "gastown",
	}, deps)

	if got := vars["rig_name"]; got != "override-rig" {
		t.Fatalf("rig_name var = %q, want override-rig", got)
	}
	if got := vars["binding_name"]; got != "override-binding" {
		t.Fatalf("binding_name var = %q, want override-binding", got)
	}
	if got := vars["binding_prefix"]; got != "override." {
		t.Fatalf("binding_prefix var = %q, want override.", got)
	}
}

func TestBuildSlingFormulaVarsSeedsEmptyRoutingNamespaceForUnboundAgent(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)

	vars := BuildSlingFormulaVars("mol-deacon-patrol", "CITY-42", nil, config.Agent{
		Name: "deacon",
	}, deps)

	for _, key := range []string{"rig_name", "binding_name", "binding_prefix"} {
		got, ok := vars[key]
		if !ok || got != "" {
			t.Fatalf("%s var = %q, %v; want empty string, true", key, got, ok)
		}
	}
}

func TestDoSlingCrossRigBlocks(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "BL"},
			{Name: "other", Path: "/other", Prefix: "OT"},
		},
	}
	a := config.Agent{Name: "worker", Dir: "other", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("BL-42")
	_, err := DoSling(testOpts(a, "BL-42"), deps, nil)

	if err == nil {
		t.Fatal("expected cross-rig error")
	}
	if len(runner.calls) != 0 {
		t.Error("runner should not have been called")
	}
}

func TestDoSlingIdempotent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Title:    "test",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = store
	result, err := DoSling(testOpts(a, b.ID), deps, store)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if !result.Idempotent {
		t.Error("expected Idempotent=true")
	}
	if len(runner.calls) != 0 {
		t.Error("runner should not have been called")
	}
}

func TestCheckBatchBurnOutputsWarn(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-2", Type: "task", Status: "open"},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-2"},
	}, nil)
	child := beads.Bead{ID: "BL-2", Status: "open", Assignee: ""}
	var result SlingResult
	// Pass store as both the store and querier (MemStore implements BeadChildQuerier)
	err := CheckBatchNoMoleculeChildren(store, []beads.Bead{child}, store, &result)
	t.Logf("err=%v autoburned=%d", err, len(result.AutoBurned))
	if len(result.AutoBurned) == 0 {
		t.Error("expected auto-burn")
	}
	if result.AutoBurned[0] != "MOL-1" {
		t.Errorf("AutoBurned[0] = %q, want MOL-1", result.AutoBurned[0])
	}
}

func TestCheckNoMoleculeChildrenRejectsLiveWorkflowWithoutForce(t *testing.T) {
	store := beads.NewMemStore()
	source, err := store.Create(beads.Bead{Title: "source", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{
		Title:  "workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": source.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "child",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	var result SlingResult
	err = CheckNoMoleculeChildren(store, source.ID, store, &result)
	if err == nil {
		t.Fatal("CheckNoMoleculeChildren error = nil, want workflow conflict")
	}
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("CheckNoMoleculeChildren error = %v, want ConflictError", err)
	}
	if conflictErr.SourceBeadID != source.ID || len(conflictErr.WorkflowIDs) != 1 || conflictErr.WorkflowIDs[0] != root.ID {
		t.Fatalf("ConflictError = %#v, want source=%s workflow=%s", conflictErr, source.ID, root.ID)
	}
	if len(result.AutoBurned) != 0 {
		t.Fatalf("AutoBurned = %#v, want empty", result.AutoBurned)
	}
	updatedRoot, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedRoot.Status != root.Status {
		t.Fatalf("root status = %q, want %q", updatedRoot.Status, root.Status)
	}
	updatedChild, err := store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedChild.Status != child.Status {
		t.Fatalf("child status = %q, want %q", updatedChild.Status, child.Status)
	}
	updatedSource, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != root.ID {
		t.Fatalf("source workflow_id = %q, want %q", got, root.ID)
	}
}

func TestDoSlingValidatesRequiredDeps(t *testing.T) {
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	opts := testOpts(a, "BL-42")

	t.Run("nil Cfg", func(t *testing.T) {
		deps := testDeps(nil, nil, nil)
		deps.Cfg = nil
		_, err := DoSling(opts, deps, nil)
		if err == nil || !strings.Contains(err.Error(), "Cfg") {
			t.Errorf("expected Cfg validation error, got %v", err)
		}
	})

	t.Run("nil Store", func(t *testing.T) {
		deps := testDeps(&config.City{}, nil, nil)
		deps.Store = nil
		_, err := DoSling(opts, deps, nil)
		if err == nil || !strings.Contains(err.Error(), "Store") {
			t.Errorf("expected Store validation error, got %v", err)
		}
	})

	t.Run("nil Runner", func(t *testing.T) {
		deps := testDeps(&config.City{}, nil, nil)
		deps.Runner = nil
		_, err := DoSling(opts, deps, nil)
		if err == nil || !strings.Contains(err.Error(), "Runner") {
			t.Errorf("expected Runner validation error, got %v", err)
		}
	})
}

func TestDoSlingCustomSlingQueryExpandsTemplateContext(t *testing.T) {
	runner := newFakeRunner()
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigPath := filepath.Join(cityPath, "frontend")
	a := config.Agent{
		Name:       "worker",
		Dir:        "frontend",
		SlingQuery: "custom-dispatch {} --route={{.Rig}}/{{.AgentBase}} --city={{.CityName}}",
	}
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "frontend", Path: rigPath}},
	}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.CityPath = cityPath
	deps.CityName = ""
	deps.Store = seededStore("FR-99")
	opts := testOpts(a, "FR-99")
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.BeadID != "FR-99" {
		t.Fatalf("result.BeadID = %q, want %q", result.BeadID, "FR-99")
	}
	want := "custom-dispatch 'FR-99' --route=frontend/worker --city=demo-city"
	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("runner calls = %#v, want %#v", runner.calls, []string{want})
	}
}

// --- Intent-based API tests ---

func TestNewSlingValidates(t *testing.T) {
	_, err := New(SlingDeps{})
	if err == nil {
		t.Error("expected validation error for empty deps")
	}
}

func TestNewSlingValid(t *testing.T) {
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), newFakeRunner().run)
	s, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Sling")
	}
}

func TestSlingRouteBead(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-42")
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := s.RouteBead(context.Background(), "BL-42", a, RouteOpts{})
	if err != nil {
		t.Fatalf("RouteBead: %v", err)
	}
	if result.BeadID != "BL-42" {
		t.Errorf("BeadID = %q, want BL-42", result.BeadID)
	}
	if result.Target != "mayor" {
		t.Errorf("Target = %q, want mayor", result.Target)
	}
	if result.Method != "bead" {
		t.Errorf("Method = %q, want bead", result.Method)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("got %d runner calls, want 1", len(runner.calls))
	}
}

func TestSlingRouteBeadRejectsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.RouteBead(context.Background(), "BL-404", a, RouteOpts{})
	if err == nil {
		t.Fatal("RouteBead error = nil, want missing bead error")
	}
	if !strings.Contains(err.Error(), "BL-404") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("RouteBead error = %q, want missing bead diagnostic", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestProbeBeadInStoreTreatsBackendNotFoundAsMissing(t *testing.T) {
	fileStore, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	bdStore := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	})
	execScript := filepath.Join(t.TempDir(), "beads-provider")
	if err := os.WriteFile(execScript, []byte("#!/bin/sh\ncase \"$1\" in\n  get) echo 'not found' >&2; exit 1 ;;\n  *) exit 2 ;;\nesac\n"), 0o755); err != nil {
		t.Fatalf("write exec provider: %v", err)
	}

	tests := []struct {
		name  string
		store beads.Store
	}{
		{name: "mem", store: beads.NewMemStore()},
		{name: "file", store: fileStore},
		{name: "caching", store: beads.NewCachingStoreForTest(beads.NewMemStore(), nil)},
		{name: "bd", store: bdStore},
		{name: "exec", store: beadsexec.NewStore(execScript)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exists, err := ProbeBeadInStore(tt.store, "NOPE-1")
			if err != nil {
				t.Fatalf("ProbeBeadInStore error = %v, want nil", err)
			}
			if exists {
				t.Fatal("ProbeBeadInStore exists = true, want false")
			}
		})
	}
}

func TestSlingRouteBeadDryRunRejectsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.RouteBead(context.Background(), "BL-404", a, RouteOpts{DryRun: true})
	if err == nil {
		t.Fatal("RouteBead dry-run error = nil, want missing bead error")
	}
	if !strings.Contains(err.Error(), "BL-404") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("RouteBead dry-run error = %q, want missing bead diagnostic", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestDoSlingDryRunInlineTextSkipsMissingBeadValidation(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "write docs",
		DryRun:        true,
		InlineText:    true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling dry-run inline text: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("DryRun = false, want true")
	}
	if result.BeadID != "write docs" {
		t.Fatalf("BeadID = %q, want inline text", result.BeadID)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestDoSlingBatchValidatesContainerInQuerierStore(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	querier := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "BL-2", Title: "child", Type: "task", Status: "open", ParentID: "BL-1", Metadata: map[string]string{}},
	}, nil)

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, querier)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Method != "batch" {
		t.Fatalf("Method = %q, want batch", result.Method)
	}
	if result.Routed != 1 {
		t.Fatalf("Routed = %d, want 1", result.Routed)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want one", runner.calls)
	}
}

func TestDoSlingBatchFallsBackToSelectedStoreForContainerExpansion(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if _, err := deps.Store.Create(beads.Bead{Title: "child", Type: "task", Status: "open", ParentID: convoy.ID}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	wrongQuerier := beads.NewMemStore()

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: convoy.ID}, deps, wrongQuerier)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Method != "batch" {
		t.Fatalf("Method = %q, want batch", result.Method)
	}
	if result.Routed != 1 {
		t.Fatalf("Routed = %d, want 1", result.Routed)
	}
}

func TestDoSlingBatchUsesCallerQuerierChildrenWhenContainerExistsThere(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "BL-store-only", Title: "store child", Type: "task", Status: "open", ParentID: "BL-1", Metadata: map[string]string{}},
	}, nil)
	querier := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Title: "convoy", Type: "convoy", Status: "open", Metadata: map[string]string{}},
		{ID: "BL-query-only", Title: "query child", Type: "task", Status: "open", ParentID: "BL-1", Metadata: map[string]string{}},
	}, nil)

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, querier)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Routed != 1 {
		t.Fatalf("Routed = %d, want 1", result.Routed)
	}
	if len(result.Children) != 1 || result.Children[0].BeadID != "BL-query-only" {
		t.Fatalf("children = %#v, want caller querier child", result.Children)
	}
}

func TestDoSlingBatchRoutesNonContainerFoundInQuerierStore(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	querier := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Title: "task", Type: "task", Status: "open", Metadata: map[string]string{}},
	}, nil)

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, querier)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Method != "bead" {
		t.Fatalf("Method = %q, want bead", result.Method)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want one", runner.calls)
	}
	if result.ConvoyID != "" {
		t.Fatalf("ConvoyID = %q, want no local auto-convoy", result.ConvoyID)
	}
	if len(result.MetadataErrors) != 1 || !strings.Contains(result.MetadataErrors[0], "skipping auto-convoy") {
		t.Fatalf("MetadataErrors = %#v, want auto-convoy skip warning", result.MetadataErrors)
	}
}

func TestDoSlingBatchDoesNotFallbackOnQuerierLookupError(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.StoreRef = "rig:selected"
	convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if _, err := deps.Store.Create(beads.Bead{Title: "child", Type: "task", Status: "open", ParentID: convoy.ID}); err != nil {
		t.Fatalf("create child: %v", err)
	}
	querier := &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("backend unavailable")}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: convoy.ID}, deps, querier)
	if err == nil {
		t.Fatal("DoSlingBatch error = nil, want lookup error")
	}
	var lookup *BeadLookupError
	if !errors.As(err, &lookup) {
		t.Fatalf("DoSlingBatch error = %T %[1]v, want BeadLookupError", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestSlingRouteBeadForceAllowsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := s.RouteBead(context.Background(), "BL-404", a, RouteOpts{Force: true})
	if err != nil {
		t.Fatalf("RouteBead force: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %#v, want one call", runner.calls)
	}
	if len(result.MetadataErrors) != 1 || !strings.Contains(result.MetadataErrors[0], "forced dispatch skipped missing-bead validation") {
		t.Fatalf("MetadataErrors = %#v, want forced missing-bead warning", result.MetadataErrors)
	}
	all, err := deps.Store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("stored beads = %#v, want no orphan auto-convoy", all)
	}
}

func TestSlingRouteDefaultFormulaForceStillRejectsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", DefaultSlingFormula: stringPtr("code-review"), MaxActiveSessions: intPtr(1)}
	_, err = s.RouteBead(context.Background(), "BL-404", a, RouteOpts{Force: true})
	if err == nil {
		t.Fatal("RouteBead force with default formula error = nil, want missing bead error")
	}
	var missing *MissingBeadError
	if !errors.As(err, &missing) {
		t.Fatalf("RouteBead force with default formula error = %T %[1]v, want MissingBeadError", err)
	}
	all, listErr := deps.Store.List(beads.ListQuery{AllowScan: true})
	if listErr != nil {
		t.Fatalf("list beads: %v", listErr)
	}
	if len(all) != 0 {
		t.Fatalf("stored beads = %#v, want no orphan formula state", all)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestValidateExistingBeadInQuerierNilIsLookupError(t *testing.T) {
	err := validateExistingBeadInQuerier("BL-42", "rig:missing", nil)
	var lookup *BeadLookupError
	if !errors.As(err, &lookup) {
		t.Fatalf("error = %T %[1]v, want BeadLookupError", err)
	}
	var missing *MissingBeadError
	if errors.As(err, &missing) {
		t.Fatalf("error = %T %[1]v, should not report missing bead for nil store", err)
	}
}

func TestSlingRouteBeadSurfacesStoreLookupError(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	deps.Store = &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("backend unavailable")}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.RouteBead(context.Background(), "BL-42", a, RouteOpts{})
	if err == nil {
		t.Fatal("RouteBead error = nil, want store lookup failure")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("RouteBead error = %q, want backend failure", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("RouteBead error = %q, want store failure, not not-found", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestSlingLaunchFormula(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := s.LaunchFormula(context.Background(), "code-review", a, FormulaOpts{})
	if err != nil {
		t.Fatalf("LaunchFormula: %v", err)
	}
	if result.Method != "formula" {
		t.Errorf("Method = %q, want formula", result.Method)
	}
	if result.FormulaName != "code-review" {
		t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
	}
	if result.BeadID == "" {
		t.Error("expected non-empty BeadID")
	}
}

// --- Typed router tests ---

type fakeBeadRouter struct {
	routed []RouteRequest
}

func (r *fakeBeadRouter) Route(_ context.Context, req RouteRequest) error {
	r.routed = append(r.routed, req)
	return nil
}

func TestSlingRouteBeadWithTypedRouter(t *testing.T) {
	router := &fakeBeadRouter{}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
	deps.Store = seededStore("BL-42")

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.RouteBead(context.Background(), "BL-42", a, RouteOpts{})
	if err != nil {
		t.Fatalf("RouteBead: %v", err)
	}

	if len(router.routed) != 1 {
		t.Fatalf("got %d route calls, want 1", len(router.routed))
	}
	if router.routed[0].BeadID != "BL-42" {
		t.Errorf("BeadID = %q, want BL-42", router.routed[0].BeadID)
	}
	if router.routed[0].Target != "mayor" {
		t.Errorf("Target = %q, want mayor", router.routed[0].Target)
	}
}

// --- Missing coverage tests ---

func TestSlingAttachFormula(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	// Create the bead in the store so attachment can find it.
	b, _ := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := s.AttachFormula(context.Background(), "code-review", b.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("AttachFormula: %v", err)
	}
	if result.Method != "on-formula" {
		t.Errorf("Method = %q, want on-formula", result.Method)
	}
	if result.WispRootID == "" {
		t.Error("expected non-empty WispRootID")
	}
	if result.FormulaName != "code-review" {
		t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
	}
}

func TestSlingAttachFormulaRejectsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.AttachFormula(context.Background(), "code-review", "BL-404", a, FormulaOpts{})
	if err == nil {
		t.Fatal("AttachFormula error = nil, want missing bead error")
	}
	var missing *MissingBeadError
	if !errors.As(err, &missing) {
		t.Fatalf("AttachFormula error = %T %[1]v, want MissingBeadError", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestSlingAttachFormulaForceStillRejectsMissingBead(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = beads.NewMemStore()

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	_, err = s.AttachFormula(context.Background(), "code-review", "BL-404", a, FormulaOpts{Force: true})
	if err == nil {
		t.Fatal("AttachFormula force error = nil, want missing bead error")
	}
	var missing *MissingBeadError
	if !errors.As(err, &missing) {
		t.Fatalf("AttachFormula force error = %T %[1]v, want MissingBeadError", err)
	}
	all, listErr := deps.Store.List(beads.ListQuery{AllowScan: true})
	if listErr != nil {
		t.Fatalf("list beads: %v", listErr)
	}
	if len(all) != 0 {
		t.Fatalf("stored beads = %#v, want no orphan formula state", all)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none", runner.calls)
	}
}

func TestSlingAttachGraphFormulaRejectsExistingLiveRoot(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err == nil {
		t.Fatal("AttachFormula error = nil, want conflict")
	}
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("AttachFormula error = %v, want ConflictError", err)
	}
	if conflictErr.SourceBeadID != source.ID {
		t.Fatalf("SourceBeadID = %q, want %q", conflictErr.SourceBeadID, source.ID)
	}
}

func TestSlingAttachGraphFormulaRejectsExistingLiveRootAcrossStores(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	otherStore := beads.NewMemStore()
	root, err := otherStore.Create(beads.Bead{
		ID:     "wf-other",
		Title:  "cross-store workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.formula_contract":                    "graph.v2",
			"gc.source_bead_id":                      source.ID,
			sourceworkflow.SourceStoreRefMetadataKey: deps.StoreRef,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{
			{Store: deps.Store, StoreRef: deps.StoreRef},
			{Store: otherStore, StoreRef: "rig:alpha"},
		}, nil
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err == nil {
		t.Fatal("AttachFormula error = nil, want conflict")
	}
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("AttachFormula error = %v, want ConflictError", err)
	}
	if conflictErr.SourceBeadID != source.ID {
		t.Fatalf("SourceBeadID = %q, want %q", conflictErr.SourceBeadID, source.ID)
	}
	if len(conflictErr.WorkflowIDs) != 1 || conflictErr.WorkflowIDs[0] != root.ID {
		t.Fatalf("WorkflowIDs = %#v, want [%s]", conflictErr.WorkflowIDs, root.ID)
	}
}

func TestSlingAttachGraphFormulaForceReplacesExistingLiveRootAcrossStores(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	otherStore := beads.NewMemStore()
	root, err := otherStore.Create(beads.Bead{
		ID:     "wf-other",
		Title:  "cross-store workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.formula_contract":                    "graph.v2",
			"gc.source_bead_id":                      source.ID,
			sourceworkflow.SourceStoreRefMetadataKey: deps.StoreRef,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{
			{Store: deps.Store, StoreRef: deps.StoreRef},
			{Store: otherStore, StoreRef: "rig:alpha"},
		}, nil
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err != nil {
		t.Fatalf("AttachFormula force: %v", err)
	}
	if result.WorkflowID == "" {
		t.Fatal("WorkflowID = empty, want new workflow root")
	}

	roots, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatalf("ListLiveRoots(city): %v", err)
	}
	if len(roots) != 1 || roots[0].ID != result.WorkflowID {
		t.Fatalf("live roots in city = %#v, want [%s]", roots, result.WorkflowID)
	}

	updatedOtherRoot, err := otherStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(other root): %v", err)
	}
	if updatedOtherRoot.Status != "closed" {
		t.Fatalf("other root status = %q, want closed", updatedOtherRoot.Status)
	}

	updatedSource, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != result.WorkflowID {
		t.Fatalf("source workflow_id = %q, want %q", got, result.WorkflowID)
	}
}

func TestSlingAttachGraphFormulaForceRestoresCrossStoreRootWhenFinalizeFails(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	otherStore := beads.NewMemStore()
	root, err := otherStore.Create(beads.Bead{
		ID:     "wf-other",
		Title:  "cross-store workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.formula_contract":                    "graph.v2",
			"gc.source_bead_id":                      source.ID,
			sourceworkflow.SourceStoreRefMetadataKey: deps.StoreRef,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(source.ID, "workflow_id", root.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	baseStore, ok := deps.Store.(*beads.MemStore)
	if !ok {
		t.Fatalf("deps.Store type = %T, want *beads.MemStore", deps.Store)
	}
	failStore := &closeAllFailMemStore{
		MemStore:            baseStore,
		failSetMetadataID:   source.ID,
		failSetMetadataKey:  "workflow_id",
		failSetMetadataCall: 1,
	}
	deps.Store = failStore
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{
			{Store: deps.Store, StoreRef: deps.StoreRef},
			{Store: otherStore, StoreRef: "rig:alpha"},
		}, nil
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err == nil {
		t.Fatal("AttachFormula force error = nil, want finalize failure")
	}
	if !strings.Contains(err.Error(), "workflow_id") {
		t.Fatalf("AttachFormula force error = %v, want workflow_id context", err)
	}

	updatedOtherRoot, err := otherStore.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(other root): %v", err)
	}
	if updatedOtherRoot.Status != root.Status {
		t.Fatalf("other root status = %q, want restored %q", updatedOtherRoot.Status, root.Status)
	}

	updatedSource, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != root.ID {
		t.Fatalf("source workflow_id = %q, want restored %q", got, root.ID)
	}
}

func TestSlingAttachGraphFormulaForceAllowsExistingLiveRoot(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open", Assignee: "mayor"})
	if err != nil {
		t.Fatal(err)
	}
	existingRoot, err := deps.Store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(source.ID, "workflow_id", existingRoot.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err != nil {
		t.Fatalf("AttachFormula force: %v", err)
	}
	if result.WorkflowID == "" {
		t.Fatal("WorkflowID = empty, want new workflow root")
	}
	roots, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("live roots = %d, want 1", len(roots))
	}
	if roots[0].ID != result.WorkflowID {
		t.Fatalf("live root = %q, want %q", roots[0].ID, result.WorkflowID)
	}
	existing, err := deps.Store.Get(existingRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if existing.Status != "closed" {
		t.Fatalf("existing root status = %q, want closed", existing.Status)
	}
	updatedSource, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != result.WorkflowID {
		t.Fatalf("source workflow_id = %q, want %q", got, result.WorkflowID)
	}
}

func TestSlingAttachGraphFormulaForceRollsBackNewRootWhenSupersededCloseFails(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open", Assignee: "mayor"})
	if err != nil {
		t.Fatal(err)
	}
	existingRoot, err := deps.Store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(source.ID, "workflow_id", existingRoot.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	baseStore, ok := deps.Store.(*beads.MemStore)
	if !ok {
		t.Fatalf("deps.Store type = %T, want *beads.MemStore", deps.Store)
	}
	failStore := &closeAllFailMemStore{MemStore: baseStore}
	deps.Store = failStore
	rootsBefore, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatalf("ListLiveRoots(before): %v", err)
	}
	if len(rootsBefore) != 1 || rootsBefore[0].ID != existingRoot.ID {
		t.Fatalf("roots before force = %#v, want [%s]", rootsBefore, existingRoot.ID)
	}
	failStore.failCloseAllCalls = 1

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err == nil {
		rootsAfter, listErr := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
		if listErr != nil {
			t.Fatalf("AttachFormula force error = nil and ListLiveRoots(after) failed: %v", listErr)
		}
		t.Fatalf(
			"AttachFormula force error = nil, want close failure (remaining failCalls=%d workflow=%s existing=%s roots_after=%#v)",
			failStore.failCloseAllCalls,
			result.WorkflowID,
			existingRoot.ID,
			rootsAfter,
		)
	}
	if !strings.Contains(err.Error(), "close superseded workflow") {
		t.Fatalf("AttachFormula force error = %v, want close superseded workflow", err)
	}

	roots, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("live roots = %d, want 1", len(roots))
	}
	if roots[0].ID != existingRoot.ID {
		t.Fatalf("live root = %q, want %q", roots[0].ID, existingRoot.ID)
	}
	updatedSource, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != existingRoot.ID {
		t.Fatalf("source workflow_id = %q, want restored %q", got, existingRoot.ID)
	}
}

func TestSlingAttachGraphFormulaForceRestoresSupersededRootWhenFinalizeFails(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open", Assignee: "mayor"})
	if err != nil {
		t.Fatal(err)
	}
	existingRoot, err := deps.Store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(source.ID, "workflow_id", existingRoot.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}
	baseStore, ok := deps.Store.(*beads.MemStore)
	if !ok {
		t.Fatalf("deps.Store type = %T, want *beads.MemStore", deps.Store)
	}
	failStore := &closeAllFailMemStore{
		MemStore:            baseStore,
		failSetMetadataID:   source.ID,
		failSetMetadataKey:  "workflow_id",
		failSetMetadataCall: 1,
	}
	deps.Store = failStore

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{Force: true})
	if err == nil {
		t.Fatal("AttachFormula force error = nil, want finalize failure")
	}
	if !strings.Contains(err.Error(), "setting workflow_id") {
		t.Fatalf("AttachFormula force error = %v, want setting workflow_id", err)
	}

	roots, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("live roots = %d, want 1", len(roots))
	}
	if roots[0].ID != existingRoot.ID {
		t.Fatalf("live root = %q, want %q", roots[0].ID, existingRoot.ID)
	}
	existing, err := deps.Store.Get(existingRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if existing.Status != existingRoot.Status {
		t.Fatalf("existing root status = %q, want restored %q", existing.Status, existingRoot.Status)
	}
	updatedSource, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedSource.Metadata["workflow_id"]; got != existingRoot.ID {
		t.Fatalf("source workflow_id = %q, want restored %q", got, existingRoot.ID)
	}
}

func TestSlingAttachGraphFormulaConcurrentLaunchCreatesSingleRoot(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	agent := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	start := make(chan struct{})
	type attempt struct {
		result SlingResult
		err    error
	}
	results := make(chan attempt, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.AttachFormula(context.Background(), "graph-work", source.ID, agent, FormulaOpts{})
			results <- attempt{result: res, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for attempt := range results {
		if attempt.err == nil {
			successes++
			continue
		}
		var conflictErr *sourceworkflow.ConflictError
		if errors.As(attempt.err, &conflictErr) {
			conflicts++
			continue
		}
		t.Fatalf("unexpected error: %v", attempt.err)
	}
	if successes == 0 {
		t.Fatal("successes=0, want at least one successful launch")
	}
	roots, err := sourceworkflow.ListLiveRoots(deps.Store, source.ID, deps.StoreRef, deps.StoreRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("successes=%d conflicts=%d live roots=%d, want singleton live root", successes, conflicts, len(roots))
	}
}

func TestDoSlingBatchGraphFormulaForceAllowsAttachedWorkflow(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := deps.Store
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "work",
		Type:     "task",
		ParentID: convoy.ID,
		Status:   "open",
		Assignee: "mayor",
	})
	if err != nil {
		t.Fatal(err)
	}
	existingRoot, err := store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": child.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child.ID, "workflow_id", existingRoot.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	result, err := DoSlingBatch(SlingOpts{
		Target:        config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		BeadOrFormula: convoy.ID,
		OnFormula:     "graph-work",
		Force:         true,
	}, deps, store)
	if err != nil {
		t.Fatalf("DoSlingBatch force: %v", err)
	}
	if result.Routed != 1 {
		t.Fatalf("Routed = %d, want 1", result.Routed)
	}
	if len(result.Children) != 1 {
		t.Fatalf("Children = %d, want 1", len(result.Children))
	}
	if result.Children[0].WorkflowID == "" {
		t.Fatal("child workflow id = empty, want replacement workflow")
	}
	updatedRoot, err := store.Get(existingRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedRoot.Status != "closed" {
		t.Fatalf("existing root status = %q, want closed", updatedRoot.Status)
	}
	updatedChild, err := store.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updatedChild.Metadata["workflow_id"]; got != result.Children[0].WorkflowID {
		t.Fatalf("child workflow_id = %q, want %q", got, result.Children[0].WorkflowID)
	}
}

func TestDoSlingBatchPropagatesConflictErrorToCaller(t *testing.T) {
	// Regression: DoSlingBatch captured per-child errors only as strings in
	// SlingChildResult.FailReason and returned a generic "%d/%d children
	// failed" at the end. That broke the top-level errors.As check in
	// cmdSling, so batch users with live-workflow conflicts got exit 1
	// instead of exit 3 and never saw the "gc workflow delete-source"
	// cleanup hint — the whole user-facing point of the fix.
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := deps.Store
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "work",
		Type:     "task",
		ParentID: convoy.ID,
		Status:   "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Orphan live root: exists with gc.source_bead_id=child.ID, but the
	// child's workflow_id pointer was never set (or was cleared by a
	// previous recovery). The pre-check via CollectAttachedBeads reads
	// workflow_id/molecule_id on the child, so it passes; the inner
	// attachBatchFormula then acquires the source-workflow launch lock
	// and discovers the orphan via ListLiveRoots — that's where the
	// typed ConflictError originates.
	existingRoot, err := store.Create(beads.Bead{
		Title:  "orphan live workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": child.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// No --force: child hits the live-workflow singleton and attachBatchFormula
	// returns *sourceworkflow.ConflictError. The batch wrapper must preserve
	// the typed error so errors.As at the CLI boundary finds it.
	_, err = DoSlingBatch(SlingOpts{
		Target:        config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		BeadOrFormula: convoy.ID,
		OnFormula:     "graph-work",
	}, deps, store)
	if err == nil {
		t.Fatal("DoSlingBatch error = nil, want conflict from child")
	}
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("errors.As(ConflictError) = false; err = %v\n(regression: batch loses typed error → exit 1 instead of exit 3)", err)
	}
	if conflictErr.SourceBeadID != child.ID {
		t.Fatalf("ConflictError.SourceBeadID = %q, want %q", conflictErr.SourceBeadID, child.ID)
	}
	if len(conflictErr.WorkflowIDs) != 1 || conflictErr.WorkflowIDs[0] != existingRoot.ID {
		t.Fatalf("ConflictError.WorkflowIDs = %#v, want [%s]", conflictErr.WorkflowIDs, existingRoot.ID)
	}
}

func TestDoSlingBatchPreflightEmitsConflictErrorForWorkflowAttachment(t *testing.T) {
	// Regression: non-force batch with a graph formula whose child already
	// has workflow_id pointing at a live workflow hit
	// checkBatchNoMoleculeChildren, which returned a plain string error
	// ("cannot use --on: beads already have attached molecules...") — so
	// cmdSling's errors.As(&ConflictError) missed, returning exit 1 and
	// dropping the `gc workflow delete-source` cleanup hint. Users saw
	// a generic error and didn't know the recovery command existed. The
	// pre-check now emits a typed ConflictError alongside the legacy
	// summary so errors.As succeeds at the CLI boundary.
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := deps.Store
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "work",
		Type:     "task",
		ParentID: convoy.ID,
		Status:   "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Live workflow attachment: child.workflow_id set, which is the
	// regular "user already launched this" case the pre-check catches.
	existingRoot, err := store.Create(beads.Bead{
		Title:  "existing live workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": child.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child.ID, "workflow_id", existingRoot.ID); err != nil {
		t.Fatalf("SetMetadata(workflow_id): %v", err)
	}

	// No --force: pre-check rejects via checkBatchNoMoleculeChildren.
	// The returned error must expose *ConflictError via errors.As so
	// cmdSling can return exit 3 and print the cleanup hint.
	_, err = DoSlingBatch(SlingOpts{
		Target:        config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		BeadOrFormula: convoy.ID,
		OnFormula:     "graph-work",
	}, deps, store)
	if err == nil {
		t.Fatal("DoSlingBatch error = nil, want pre-check rejection")
	}
	var conflictErr *sourceworkflow.ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("errors.As(ConflictError) = false; err = %v\n(regression: batch preflight still returns plain string)", err)
	}
	if conflictErr.SourceBeadID != child.ID {
		t.Fatalf("ConflictError.SourceBeadID = %q, want %q", conflictErr.SourceBeadID, child.ID)
	}
	if len(conflictErr.WorkflowIDs) != 1 || conflictErr.WorkflowIDs[0] != existingRoot.ID {
		t.Fatalf("ConflictError.WorkflowIDs = %#v, want [%s]", conflictErr.WorkflowIDs, existingRoot.ID)
	}
}

func TestDoSlingBatchPreflightEmitsPerChildConflictErrors(t *testing.T) {
	// Regression: iter-3's batch preflight fix collapsed N conflicting
	// children into a single ConflictError keyed to the first child,
	// which misattributed every other child's blocking workflow IDs.
	// The cleanup hint then only addressed the first child; users
	// running it saw unrelated workflow IDs and failed to clean up the
	// rest of the batch. The preflight now emits one ConflictError per
	// conflicted child via errors.Join so each child's blocking IDs
	// stay correctly attributed.
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}
	formulatest.EnableV2ForTest(t)
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := deps.Store

	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}

	// Two children, each with their own live workflow attachment.
	child1, err := store.Create(beads.Bead{Title: "work-1", Type: "task", ParentID: convoy.ID, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	child2, err := store.Create(beads.Bead{Title: "work-2", Type: "task", ParentID: convoy.ID, Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	root1, err := store.Create(beads.Bead{
		Title:  "workflow-1",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": child1.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root2, err := store.Create(beads.Bead{
		Title:  "workflow-2",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": child2.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child1.ID, "workflow_id", root1.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child2.ID, "workflow_id", root2.ID); err != nil {
		t.Fatal(err)
	}

	_, err = DoSlingBatch(SlingOpts{
		Target:        config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		BeadOrFormula: convoy.ID,
		OnFormula:     "graph-work",
	}, deps, store)
	if err == nil {
		t.Fatal("DoSlingBatch error = nil, want preflight rejection")
	}

	// Walk the error tree and collect every typed ConflictError. Both
	// children should appear with their own root IDs — the critical
	// invariant is that root2.ID is NOT attributed to child1.ID.
	var collected []*sourceworkflow.ConflictError
	{
		var walk func(error)
		walk = func(e error) {
			if e == nil {
				return
			}
			// Test walker intentionally uses direct type assertion to
			// collect every ConflictError in the tree (errors.As collapses
			// to the first match). See collectConflictErrors in cmd/gc.
			if c, ok := e.(*sourceworkflow.ConflictError); ok { //nolint:errorlint
				collected = append(collected, c)
			}
			type mu interface{ Unwrap() []error }
			if m, ok := e.(mu); ok { //nolint:errorlint
				for _, child := range m.Unwrap() {
					walk(child)
				}
				return
			}
			if inner := errors.Unwrap(e); inner != nil {
				walk(inner)
			}
		}
		walk(err)
	}

	if len(collected) != 2 {
		t.Fatalf("ConflictError count = %d, want 2 (one per conflicted child)", len(collected))
	}

	byChild := map[string][]string{}
	for _, c := range collected {
		byChild[c.SourceBeadID] = c.WorkflowIDs
	}
	if got := byChild[child1.ID]; len(got) != 1 || got[0] != root1.ID {
		t.Fatalf("child1 ConflictError.WorkflowIDs = %#v, want [%s]", got, root1.ID)
	}
	if got := byChild[child2.ID]; len(got) != 1 || got[0] != root2.ID {
		t.Fatalf("child2 ConflictError.WorkflowIDs = %#v, want [%s]", got, root2.ID)
	}
}

func TestSlingAttachNonGraphFormulaAllowsExistingLiveWorkflow(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		FormulaLayers: config.FormulaLayers{
			City: []string{sharedTestFormulaDir},
		},
	}
	config.InjectImplicitAgents(cfg)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{ID: "BL-42", Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "code-review", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err != nil {
		t.Fatalf("AttachFormula non-graph: %v", err)
	}
	if result.WispRootID == "" {
		t.Fatal("WispRootID = empty, want attached wisp")
	}
}

func TestSourceWorkflowLockScopeUsesStorePath(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "rigs/alpha"},
		},
	}
	if got := sourceWorkflowLockScope(SlingDeps{
		CityPath: "/city",
		StoreRef: "city:test-city",
		Cfg:      cfg,
	}); got != filepath.Clean("/city") {
		t.Fatalf("city scope = %q, want /city", got)
	}
	if got := sourceWorkflowLockScope(SlingDeps{
		CityPath: "/city",
		StoreRef: "rig:alpha",
		Cfg:      cfg,
	}); got != filepath.Join("/city", "rigs", "alpha") {
		t.Fatalf("rig scope = %q, want %q", got, filepath.Join("/city", "rigs", "alpha"))
	}
	wantShared := sourceworkflow.LockScopeForStoreRef("/city", "", "rig:alpha", func(rigName string) (string, bool) {
		if rigName != "alpha" {
			return "", false
		}
		return "rigs/alpha", true
	})
	if got := sourceWorkflowLockScope(SlingDeps{
		CityPath: "/city",
		StoreRef: "rig:alpha",
		Cfg:      cfg,
	}); got != wantShared {
		t.Fatalf("rig scope = %q, want shared helper scope %q", got, wantShared)
	}
}

func TestSlingExpandConvoy(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if _, err := store.Create(beads.Bead{Title: "task1", Type: "task", ParentID: convoy.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "task2", Type: "task", ParentID: convoy.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := s.ExpandConvoy(context.Background(), convoy.ID, a, RouteOpts{}, store)
	if err != nil {
		t.Fatalf("ExpandConvoy: %v", err)
	}
	if result.Routed != 2 {
		t.Errorf("Routed = %d, want 2", result.Routed)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	if len(result.Children) != 2 {
		t.Fatalf("Children = %d, want 2", len(result.Children))
	}
}

func TestDoSlingPoolEmptyWarns(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "pool", MaxActiveSessions: intPtr(0)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")
	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.PoolEmpty {
		t.Error("expected PoolEmpty=true for max=0")
	}
}

func TestFinalizeAutoConvoy(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	b, _ := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})

	result, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: b.ID,
	}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.ConvoyID == "" {
		t.Error("expected auto-convoy creation")
	}
	// Verify convoy bead exists in store.
	if _, err := deps.Store.Get(result.ConvoyID); err != nil {
		t.Errorf("convoy %s not found in store: %v", result.ConvoyID, err)
	}
}

func TestFinalizeNoConvoyWhenSuppressed(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")

	result, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: "BL-1", NoConvoy: true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.ConvoyID != "" {
		t.Errorf("expected no convoy, got %q", result.ConvoyID)
	}
}

func TestDoSlingBatchPartialFailure(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if _, err := store.Create(beads.Bead{Title: "t1", Type: "task", ParentID: convoy.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	b2, _ := store.Create(beads.Bead{Title: "t2", Type: "task", ParentID: convoy.ID, Status: "open"})
	if _, err := store.Create(beads.Bead{Title: "t3", Type: "task", ParentID: convoy.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	// Fail the runner for the second child's actual bead ID.
	runner.on(b2.ID, fmt.Errorf("runner failed"))

	result, err := DoSlingBatch(SlingOpts{
		Target: a, BeadOrFormula: convoy.ID,
	}, deps, store)
	// Partial failure returns error but result has per-child data.
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if result.Routed != 2 {
		t.Errorf("Routed = %d, want 2", result.Routed)
	}
	if result.Failed != 1 {
		t.Errorf("Failed = %d, want 1", result.Failed)
	}
	// Find the failed child.
	for _, c := range result.Children {
		if c.BeadID == b2.ID && !c.Failed {
			t.Errorf("expected child %s to be failed", b2.ID)
		}
	}
}

func TestFindBlockingMolecule(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open"},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-1"},
	}, nil)
	label, id := FindBlockingMolecule(store, "BL-1", store)
	if label != "molecule" {
		t.Errorf("label = %q, want molecule", label)
	}
	if id != "MOL-1" {
		t.Errorf("id = %q, want MOL-1", id)
	}
}

func TestFindBlockingMoleculeNone(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open"},
	}, nil)
	label, id := FindBlockingMolecule(store, "BL-1", store)
	if label != "" || id != "" {
		t.Errorf("expected no blocking molecule, got %q %q", label, id)
	}
}

func TestHasMoleculeChildren(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open"},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-1"},
	}, nil)
	if !HasMoleculeChildren(store, "BL-1", store) {
		t.Error("expected true")
	}
}

func TestDoSlingDryRun(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")

	result, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: "BL-1", DryRun: true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.DryRun {
		t.Error("expected DryRun=true")
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner should not be called during dry-run, got %d calls", len(runner.calls))
	}
}

func TestDoSlingNudgeSignal(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")

	result, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: "BL-1", Nudge: true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.NudgeAgent == nil {
		t.Error("expected NudgeAgent to be set")
	}
}

func TestDoSlingSuspendedAgentWarnsEvenOnFailure(t *testing.T) {
	// Matches gastown-sling tutorial: sling to suspended agent, runner fails,
	// but AgentSuspended should still be set so CLI prints the warning.
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), Suspended: true}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")
	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)

	if err == nil {
		t.Fatal("expected runner error")
	}
	// Even on failure, the warning flags must be set so callers can display them.
	if !result.AgentSuspended {
		t.Error("expected AgentSuspended=true even when runner fails")
	}
}

// --- Tests matching tutorial scenarios (gastown-sling.txtar) ---

func TestDoSlingNonexistentTargetFails(_ *testing.T) {
	// Matches gastown-sling scenario 2: sling to nonexistent target.
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}
	nonexistent := config.Agent{Name: "nonexistent", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")
	// Cross-rig and routing should still work even if agent doesn't exist in config.
	// The runner will fail, but the domain doesn't validate agent existence.
	result, err := DoSling(testOpts(nonexistent, "BL-1"), deps, nil)
	if err != nil {
		// Runner fails because bd can't find the agent, which is expected.
		_ = result
		return
	}
	// If no error, the bead was routed to the nonexistent agent -- also valid at domain level.
}

func TestDoSlingPoolEmptyWarnsOnFailure(t *testing.T) {
	// Matches gastown-sling scenario 4: sling to empty pool warns.
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "empty-pool", MaxActiveSessions: intPtr(0)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("BL-1")

	result, err := DoSling(testOpts(a, "BL-1"), deps, nil)
	if err == nil {
		t.Fatal("expected runner error for max=0 pool")
	}
	if !result.PoolEmpty {
		t.Error("expected PoolEmpty=true even when runner fails")
	}
}

func TestDoSlingFormulaInstantiationError(t *testing.T) {
	// Matches gastown-sling scenario 5: formula not found.
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	_, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: "nonexistent-formula", IsFormula: true,
	}, deps, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent formula")
	}
	if !strings.Contains(err.Error(), "nonexistent-formula") {
		t.Errorf("error = %q, want formula name in message", err.Error())
	}
}

func TestDoSlingBatchSkipsClosedChildren(t *testing.T) {
	// Matches gastown-convoy: convoy with mixed open/closed children.
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if _, err := store.Create(beads.Bead{Title: "open", Type: "task", ParentID: convoy.ID, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	cb, _ := store.Create(beads.Bead{Title: "closed", Type: "task", ParentID: convoy.ID})
	if err := store.Close(cb.ID); err != nil {
		t.Fatal(err)
	}

	result, err := DoSlingBatch(SlingOpts{
		Target: a, BeadOrFormula: convoy.ID,
	}, deps, store)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Routed != 1 {
		t.Errorf("Routed = %d, want 1 (only open child)", result.Routed)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (closed child)", result.Skipped)
	}
}

func TestDoSlingBatchEmptyConvoyErrors(t *testing.T) {
	// Convoy with no open children should error.
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	cb, _ := store.Create(beads.Bead{Title: "closed", Type: "task", ParentID: convoy.ID})
	if err := store.Close(cb.ID); err != nil {
		t.Fatal(err)
	}

	_, err := DoSlingBatch(SlingOpts{
		Target: a, BeadOrFormula: convoy.ID,
	}, deps, store)
	if err == nil {
		t.Fatal("expected error for convoy with no open children")
	}
}

func TestDoSlingForceSkipsCrossRig(t *testing.T) {
	// --force should allow cross-rig routing.
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "BL"},
			{Name: "other", Path: "/other", Prefix: "OT"},
		},
	}
	a := config.Agent{Name: "worker", Dir: "other", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	_, err := DoSling(SlingOpts{
		Target: a, BeadOrFormula: "BL-42", Force: true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling with --force should not error on cross-rig: %v", err)
	}
}

// TestSlingFormulaSearchPaths_RigNameKey: agent.Dir = rig name should
// resolve to the rig-specific FormulaLayers entry. This is the legacy
// shape and was already working pre-#1801.
func TestSlingFormulaSearchPaths_RigNameKey(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "gascity", Path: "/home/ds/gascity"},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"gascity": {"/rig/formulas", "/pack/formulas"},
			},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "gascity"}
	deps := SlingDeps{Cfg: cfg}

	got := SlingFormulaSearchPaths(deps, a)
	want := []string{"/rig/formulas", "/pack/formulas"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("SlingFormulaSearchPaths(rig-name agent) = %v, want %v", got, want)
	}
}

// TestSlingFormulaSearchPaths_RigPathKey: agent.Dir = filesystem path
// should ALSO resolve to the rig-specific FormulaLayers entry by mapping
// the path to the rig name. Prior to #1801 this fell through to
// fl.City silently, which made every pack-imported formula appear
// "not found in search paths" when sling tried to instantiate it.
func TestSlingFormulaSearchPaths_RigPathKey(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "gascity", Path: "/home/ds/gascity"},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"gascity": {"/rig/formulas", "/pack/formulas"},
			},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "/home/ds/gascity"}
	deps := SlingDeps{Cfg: cfg}

	got := SlingFormulaSearchPaths(deps, a)
	want := []string{"/rig/formulas", "/pack/formulas"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("SlingFormulaSearchPaths(rig-path agent) = %v, want %v (regression of #1801)", got, want)
	}
}

// TestSlingFormulaSearchPaths_CityScoped: agent with empty Dir should
// fall back to fl.City layers. Verifies the city-scoped path remains
// untouched by the #1801 fix.
func TestSlingFormulaSearchPaths_CityScoped(t *testing.T) {
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"gascity": {"/rig/formulas"},
			},
		},
	}
	a := config.Agent{Name: "mayor", Dir: ""}
	deps := SlingDeps{Cfg: cfg}

	got := SlingFormulaSearchPaths(deps, a)
	want := []string{"/city/formulas"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("SlingFormulaSearchPaths(city-scoped) = %v, want %v", got, want)
	}
}

// TestSlingFormulaSearchPaths_RigPathKey_TrailingSlash: agent.Dir with a
// trailing slash should match the rig path after normalization. Strict
// string equality (which the first version of this fix used) re-introduces
// the #1801 fall-through whenever the operator writes `dir =
// "/home/ds/gascity/"` in agent.toml.
func TestSlingFormulaSearchPaths_RigPathKey_TrailingSlash(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "gascity", Path: "/home/ds/gascity"},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"gascity": {"/rig/formulas"},
			},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "/home/ds/gascity/"}
	deps := SlingDeps{Cfg: cfg}

	got := SlingFormulaSearchPaths(deps, a)
	want := []string{"/rig/formulas"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("SlingFormulaSearchPaths(trailing-slash dir) = %v, want %v", got, want)
	}
}

// TestSlingFormulaSearchPaths_UnknownDir: agent.Dir matching neither a
// rig name nor a rig path should fall back to fl.City (the existing
// SearchPaths fallback when the rig key is absent).
func TestSlingFormulaSearchPaths_UnknownDir(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "gascity", Path: "/home/ds/gascity"},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"gascity": {"/rig/formulas"},
			},
		},
	}
	a := config.Agent{Name: "mystery", Dir: "/some/other/place"}
	deps := SlingDeps{Cfg: cfg}

	got := SlingFormulaSearchPaths(deps, a)
	want := []string{"/city/formulas"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("SlingFormulaSearchPaths(unknown dir) = %v, want %v", got, want)
	}
}

// fixedBranchResolver returns a constant branch regardless of dir.
type fixedBranchResolver struct{ branch string }

func (r fixedBranchResolver) DefaultBranch(string) string { return r.branch }

func TestSlingFormulaTargetBranch_PrefersBeadMetadata(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC", DefaultBranch: "master"},
		},
	}
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Metadata: map[string]string{"target": "release/v2"}})
	if err != nil {
		t.Fatalf("seeding bead: %v", err)
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    store,
		Branches: fixedBranchResolver{branch: "main"},
	}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	got := SlingFormulaTargetBranch(bead.ID, deps, a)
	if got != "release/v2" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (bead metadata wins)", got, "release/v2")
	}
}

func TestSlingFormulaTargetBranch_UsesRigDefaultBranchByBead(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC", DefaultBranch: "master"},
		},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: "main"},
	}
	a := config.Agent{Name: "polecat"} // no Dir — bead-prefix lookup must win

	got := SlingFormulaTargetBranch("SC-1", deps, a)
	if got != "master" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (rig stored default by bead prefix)", got, "master")
	}
}

func TestSlingFormulaTargetBranch_UsesRigDefaultBranchByHyphenatedBeadPrefix(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "agent-diagnostics", Path: "/agent-diagnostics", Prefix: "agent-diagnostics", DefaultBranch: "master"},
		},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: "main"},
	}
	a := config.Agent{Name: "polecat"} // no Dir - bead-prefix lookup must handle hyphenated prefixes

	got := SlingFormulaTargetBranch("agent-diagnostics-hnn", deps, a)
	if got != "master" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (hyphenated rig prefix stored default)", got, "master")
	}
}

func TestSlingFormulaTargetBranch_UsesRigDefaultBranchByAgent(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC", DefaultBranch: "master"},
		},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: "main"},
	}
	a := config.Agent{Name: "refinery", Dir: "scamper"}

	// No bead ID — agent.Dir lookup must find the rig.
	got := SlingFormulaTargetBranch("", deps, a)
	if got != "master" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (rig stored default by agent.Dir)", got, "master")
	}
}

func TestSlingFormulaTargetBranch_UsesRigDefaultBranchByAgentPath(t *testing.T) {
	rigPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: rigPath, Prefix: "SC", DefaultBranch: "master"},
		},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: "main"},
	}
	a := config.Agent{Name: "refinery", Dir: rigPath}

	got := SlingFormulaTargetBranch("", deps, a)
	if got != "master" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (rig stored default by agent path)", got, "master")
	}
}

func TestSlingFormulaTargetBranch_FallsBackToProbeWhenUnset(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC"}, // no DefaultBranch
		},
	}
	deps := SlingDeps{
		Cfg:      cfg,
		Store:    beads.NewMemStore(),
		Branches: fixedBranchResolver{branch: "trunk"},
	}
	a := config.Agent{Name: "refinery", Dir: "scamper"}

	got := SlingFormulaTargetBranch("SC-1", deps, a)
	if got != "trunk" {
		t.Errorf("SlingFormulaTargetBranch = %q, want %q (fallback to live probe)", got, "trunk")
	}
}
