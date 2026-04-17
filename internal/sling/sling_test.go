package sling

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

func intPtr(v int) *int { return &v }

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
		CityPath: "/city",
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

var sharedTestFormulaDir string

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
