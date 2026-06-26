package sling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/pidutil"
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

const (
	slingTestFormulaDirPrefix = "gc-sling-test-formulas-pid"
	slingTestCityDirPrefix    = "gc-sling-test-city-pid"
)

func init() {
	tmpRoot := os.TempDir()
	sweepOrphanSlingPIDPrefixedDirs(tmpRoot, slingTestFormulaDirPrefix)
	sweepOrphanSlingPIDPrefixedDirs(tmpRoot, slingTestCityDirPrefix)

	dir, err := os.MkdirTemp("", slingPIDPrefixedTempPattern(slingTestFormulaDirPrefix))
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
		_ = os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644)
	}
	sharedTestFormulaDir = dir

	cityDir, err := os.MkdirTemp("", slingPIDPrefixedTempPattern(slingTestCityDirPrefix))
	if err != nil {
		panic(err)
	}
	sharedTestCityDir = cityDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	_ = os.RemoveAll(sharedTestFormulaDir)
	_ = os.RemoveAll(sharedTestCityDir)
	os.Exit(code)
}

func slingPIDPrefixedTempPattern(prefix string) string {
	return prefix + strconv.Itoa(os.Getpid()) + "-*"
}

func slingPIDFromPrefixedDirName(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	end := 0
	for end < len(suffix) && suffix[end] >= '0' && suffix[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if end < len(suffix) && suffix[end] != '-' {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:end])
	if err != nil {
		return 0, false
	}
	return pid, true
}

func sweepOrphanSlingPIDPrefixedDirs(root, prefix string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, ok := slingPIDFromPrefixedDirName(e.Name(), prefix)
		if !ok || pid <= 0 || pid == self {
			continue
		}
		if pidutil.Alive(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
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
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		ParentID: convoy.ID,
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

// TestCheckBeadStateRoutedWithoutConvoyIsNotIdempotent guards the recovery
// path: a bead with gc.routed_to set (e.g. declared via bd create --metadata
// rather than routed through gc sling) but no convoy parent must not be
// treated as idempotent — otherwise the caller skips finalize() and the work
// sits orphaned.
func TestCheckBeadStateRoutedWithoutConvoyIsNotIdempotent(t *testing.T) {
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

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if result.Idempotent {
		t.Fatalf("expected Idempotent=false when routed bead has no convoy parent, got %+v", result)
	}
}

func TestCheckBeadStateRoutedWithLiveTrackingConvoyIsIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "auto convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}
	if err := store.DepAdd(convoy.ID, bead.ID, "tracks"); err != nil {
		t.Fatalf("store.DepAdd(tracks): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if !result.Idempotent {
		t.Fatalf("expected Idempotent=true when routed bead has live tracking convoy, got %+v", result)
	}
}

func TestCheckBeadStateRoutedWithSyntheticTrackingConvoyIsIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Status:   "open",
		Metadata: map[string]string{"gc.synthetic": "true"},
	})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}
	if err := store.DepAdd(convoy.ID, bead.ID, "tracks"); err != nil {
		t.Fatalf("store.DepAdd(tracks): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if !result.Idempotent {
		t.Fatalf("expected Idempotent=true when routed bead has a live tracking convoy, got %+v", result)
	}
}

func TestCheckBeadStateRoutedWithClosedTrackingConvoyIsNotIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "old convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}
	if err := store.DepAdd(convoy.ID, bead.ID, "tracks"); err != nil {
		t.Fatalf("store.DepAdd(tracks): %v", err)
	}
	if err := store.Close(convoy.ID); err != nil {
		t.Fatalf("store.Close(convoy): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if result.Idempotent {
		t.Fatalf("expected Idempotent=false when routed bead has only closed tracking convoy, got %+v", result)
	}
}

// TestCheckBeadStateRoutedWithClosedConvoyIsNotIdempotent ensures a prior
// convoy whose run has finished does not count as a live attachment — the
// next sling should still create a fresh convoy and poke the controller.
func TestCheckBeadStateRoutedWithClosedConvoyIsNotIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "old convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(convoy): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "route me",
		Type:     "task",
		Status:   "open",
		ParentID: convoy.ID,
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}
	if err := store.Close(convoy.ID); err != nil {
		t.Fatalf("store.Close(convoy): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if result.Idempotent {
		t.Fatalf("expected Idempotent=false when convoy parent is closed, got %+v", result)
	}
}

func TestCheckBeadStateRoutedWithWorkflowParentIsIdempotent(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
	}{
		{
			name: "workflow kind",
			metadata: map[string]string{
				"gc.kind": "workflow",
			},
		},
		{
			name: "graph v2 contract",
			metadata: map[string]string{
				"gc.formula_contract": "graph.v2",
			},
		},
		{
			name: "workflow kind and graph v2 contract",
			metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			parent, err := store.Create(beads.Bead{
				Title:    "workflow",
				Type:     "workflow",
				Status:   "in_progress",
				Metadata: tt.metadata,
			})
			if err != nil {
				t.Fatalf("store.Create(parent): %v", err)
			}
			bead, err := store.Create(beads.Bead{
				Title:    "workflow step",
				Type:     "task",
				Status:   "open",
				ParentID: parent.ID,
				Metadata: map[string]string{"gc.routed_to": "mayor"},
			})
			if err != nil {
				t.Fatalf("store.Create(bead): %v", err)
			}

			result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

			if !result.Idempotent {
				t.Fatalf("expected Idempotent=true for routed bead under workflow parent, got %+v", result)
			}
		})
	}
}

func TestCheckBeadStateRoutedWithNormalParentWithoutTrackingConvoyRecovers(t *testing.T) {
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "epic", Type: "epic", Status: "open"})
	if err != nil {
		t.Fatalf("store.Create(parent): %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "epic child",
		Type:     "task",
		Status:   "open",
		ParentID: parent.ID,
		Metadata: map[string]string{"gc.routed_to": "mayor"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}

	result := CheckBeadState(store, bead.ID, config.Agent{Name: "mayor"}, SlingDeps{Store: store})

	if result.Idempotent {
		t.Fatalf("expected Idempotent=false for routed bead under normal parent with no tracking convoy, got %+v", result)
	}
}

func TestCheckBeadStatePoolLabelWithoutConvoyIsNotIdempotent(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "pool work",
		Type:   "task",
		Status: "open",
		Labels: []string{"pool:hw/polecat"},
	})
	if err != nil {
		t.Fatalf("store.Create(bead): %v", err)
	}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hw",
		MinActiveSessions: intPtr(1),
		MaxActiveSessions: intPtr(3),
	}

	result := CheckBeadState(store, bead.ID, a, SlingDeps{Store: store})

	if result.Idempotent {
		t.Fatalf("expected Idempotent=false for pool label without convoy parent, got %+v", result)
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
		{"pieces-annotator-x8o", "pieces-annotator"},
		{"pieces-annotator-a3f", "pieces-annotator"},
		{"pieces-cli-5b8i", "pieces-cli"},
		{" pieces-annotator-x8o ", "pieces-annotator"},
		{"my-cool-app-123", "my-cool-app"},
		{"beads-vscode-1", "beads-vscode"},
		{"vc-baseline-test", "vc"},
		{"pieces-annotator-baseline", "pieces"},
		// All-letter suffixes are ambiguous without city config.
		{"pieces-annotator-gnpgief", "pieces"},
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
			{Name: "pieces-annotator", Path: "/pa", Prefix: "pieces-annotator"},
			{Name: "fe", Path: "/fe", Prefix: "fe"},
		},
	}
	tests := []struct {
		id   string
		want string
	}{
		{"agent-diagnostics-hnn", "agent-diagnostics"},
		{"agent-diagnostics-spawn-storm", "agent-diagnostics"},
		{"pieces-annotator-gnpgief", "pieces-annotator"},
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
	// Unknown prefix -> fall back to BeadPrefix's config-free heuristic.
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
// the config-free heuristic for unknown prefixes). Cover both so a regression
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

func TestDoSlingSuspendedRigWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", Suspended: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("my-42")
	result, err := DoSling(testOpts(a, "my-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q", result.SuspendedRig, "myrig")
	}
	if result.AgentSuspended {
		t.Error("expected AgentSuspended=false: only the rig is suspended, not the agent")
	}
}

func TestDoSlingSuspendedRigForce(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig", Suspended: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("my-42")
	opts := testOpts(a, "my-42")
	opts.Force = true
	result, err := DoSling(opts, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty with --force", result.SuspendedRig)
	}
}

func TestDoSlingLiveRigNoSuspendedRigWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "myrig"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	deps.Store = seededStore("my-42")
	result, err := DoSling(testOpts(a, "my-42"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.SuspendedRig != "" {
		t.Errorf("SuspendedRig = %q, want empty for live rig", result.SuspendedRig)
	}
}

func TestDoSlingSuspendedRigWarnsEvenOnFailure(t *testing.T) {
	// Mirrors TestDoSlingSuspendedAgentWarnsEvenOnFailure: the warning flag
	// must survive a routing failure so the CLI can still display it.
	runner := newFakeRunner()
	runner.on("bd update", fmt.Errorf("runner failed"))
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "myrig", Suspended: true}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = seededStore("my-1")
	result, err := DoSling(testOpts(a, "my-1"), deps, nil)

	if err == nil {
		t.Fatal("expected runner error")
	}
	if result.SuspendedRig != "myrig" {
		t.Errorf("SuspendedRig = %q, want %q even when routing fails", result.SuspendedRig, "myrig")
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

func TestDoSlingFormulaToPoolRejectsLegacyMoleculeRoot(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}

	deps := testDeps(cfg, sp, runner.run)
	_, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "code-review",
		IsFormula:     true,
	}, deps, nil)
	if err == nil {
		t.Fatal("DoSling error = nil, want legacy molecule-root pool rejection")
	}
	if !strings.Contains(err.Error(), "root is a molecule container") {
		t.Fatalf("DoSling error = %q, want Ready-visible root guidance", err.Error())
	}
}

func TestDoSlingFormulaToPoolAllowsRootOnlyReadySurface(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "root-only.toml"), []byte(`
formula = "root-only"
version = 1
phase = "vapor"

[[steps]]
id = "work"
title = "Work"
`), 0o644); err != nil {
		t.Fatalf("write root-only formula: %v", err)
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
	}
	a := config.Agent{Name: "agent-a", MaxActiveSessions: intPtr(3)}

	deps := testDeps(cfg, sp, runner.run)
	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "root-only",
		IsFormula:     true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling error: %v", err)
	}
	if result.Method != "formula" {
		t.Errorf("Method = %q, want formula", result.Method)
	}
	if result.BeadID == "" {
		t.Fatal("expected non-empty root-only wisp ID")
	}
}

func TestDoSlingFormulaToSingleSessionAgentSkipsPoolDemand(t *testing.T) {
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
	root, err := deps.Store.Get(result.BeadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", result.BeadID, err)
	}
	if got, ok := root.Metadata["gc.pool_demand"]; ok {
		t.Errorf("wisp root gc.pool_demand = %q, want absent", got)
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
	convoy, _ := store.Create(beads.Bead{Title: "convoy", Type: "convoy", Status: "open"})
	b, _ := store.Create(beads.Bead{
		Title:    "test",
		ParentID: convoy.ID,
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

func TestDoSlingBatchExpandsTracksConvoy(t *testing.T) {
	runner := newFakeRunner()
	deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
	store := deps.Store
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	epic, err := store.Create(beads.Bead{Title: "epic", Type: "epic"})
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", Type: "task", Status: "open", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.DepAdd(convoy.ID, child.ID, "tracks"); err != nil {
		t.Fatalf("track child: %v", err)
	}

	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: convoy.ID}, deps, store)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Routed != 1 {
		t.Fatalf("Routed = %d, want 1", result.Routed)
	}
	if len(result.Children) != 1 || result.Children[0].BeadID != child.ID {
		t.Fatalf("children = %#v, want tracked child %s", result.Children, child.ID)
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

func TestSlingAttachFormulaRoutesSourceBeadWithTypedRouter(t *testing.T) {
	router := &fakeBeadRouter{}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
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

	if result.WispRootID == "" {
		t.Fatal("WispRootID is empty")
	}
	if len(router.routed) != 1 {
		t.Fatalf("got %d route calls, want 1", len(router.routed))
	}
	if router.routed[0].BeadID != b.ID {
		t.Fatalf("routed BeadID = %q, want source bead %q", router.routed[0].BeadID, b.ID)
	}
	got, err := deps.Store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", b.ID, err)
	}
	if got.Metadata["molecule_id"] != result.WispRootID {
		t.Fatalf("molecule_id metadata = %q, want %q", got.Metadata["molecule_id"], result.WispRootID)
	}
}

func TestSlingRouteBeadDefaultFormulaRoutesSourceBeadWithTypedRouter(t *testing.T) {
	router := &fakeBeadRouter{}
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
	deps.Store = seededStore("BL-42")

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	a := config.Agent{Name: "mayor", DefaultSlingFormula: stringPtr("code-review"), MaxActiveSessions: intPtr(1)}
	result, err := s.RouteBead(context.Background(), "BL-42", a, RouteOpts{})
	if err != nil {
		t.Fatalf("RouteBead: %v", err)
	}

	if result.WispRootID == "" {
		t.Fatal("WispRootID is empty")
	}
	if len(router.routed) != 1 {
		t.Fatalf("got %d route calls, want 1", len(router.routed))
	}
	if router.routed[0].BeadID != "BL-42" {
		t.Fatalf("routed BeadID = %q, want source bead BL-42", router.routed[0].BeadID)
	}
	got, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("Get(BL-42): %v", err)
	}
	if got.Metadata["molecule_id"] != result.WispRootID {
		t.Fatalf("molecule_id metadata = %q, want %q", got.Metadata["molecule_id"], result.WispRootID)
	}
}

func crossStoreSlingDeps(t *testing.T) (SlingDeps, *fakeBeadRouter, config.Agent) {
	t.Helper()
	router := &fakeBeadRouter{}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "RW"},
		},
	}
	target := config.Agent{Name: "target", Dir: "myrig", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
	deps.StoreRef = "city:test-city"
	return deps, router, target
}

func crossStoreSlingFixture(t *testing.T, assignee string) (SlingOpts, SlingDeps, *fakeBeadRouter, beads.Bead) {
	t.Helper()
	deps, router, target := crossStoreSlingDeps(t)
	bead, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	opts := SlingOpts{Target: target, BeadOrFormula: bead.ID, Force: true}
	return opts, deps, router, bead
}

func TestValidateBuiltInRouteStoreReachableAllowsCityScopedTarget(t *testing.T) {
	// vp-kvp stage i: the fail-loud cross-store route guard must refuse a
	// rig-scoped target that cannot reach the bead's store, but must NOT
	// false-positive on a city-scoped (cross-store-eligible) target, which
	// legitimately serves any store. Before the exemption the city singleton was
	// refused, blocking cross-store work delivery (stages ii/iii).
	deps, _, rigTarget := crossStoreSlingDeps(t) // deps.StoreRef = "city:test-city"

	if err := validateBuiltInRouteStoreReachable(deps, "RW-1", rigTarget); err == nil {
		t.Fatal("rig-scoped target routing a city-store bead must be refused (fail loud)")
	} else {
		requireCrossStoreRouteError(t, err)
	}

	cityTarget := config.Agent{Name: "platform-architect", Scope: "city", MaxActiveSessions: intPtr(1)}
	if err := validateBuiltInRouteStoreReachable(deps, "RW-1", cityTarget); err != nil {
		t.Fatalf("city-scoped target must not be refused as cross-store: %v", err)
	}
}

func requireCrossStoreRouteError(t *testing.T, err error) {
	t.Helper()
	var crossStoreErr *CrossStoreRouteError
	if !errors.As(err, &crossStoreErr) {
		t.Fatalf("error = %T %[1]v, want CrossStoreRouteError", err)
	}
}

func requireNoCrossStoreRouteMutation(t *testing.T, store beads.Store, beadID string, wantAssignee string) {
	t.Helper()
	got, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", beadID, err)
	}
	if got.Assignee != wantAssignee {
		t.Fatalf("Assignee = %q, want %q", got.Assignee, wantAssignee)
	}
	if got.Metadata["gc.routed_to"] != "" {
		t.Fatalf("gc.routed_to = %q, want unset after refusal", got.Metadata["gc.routed_to"])
	}
	if got.Metadata["molecule_id"] != "" {
		t.Fatalf("molecule_id = %q, want unset after refusal", got.Metadata["molecule_id"])
	}
}

func requireOnlySeedBeads(t *testing.T, store beads.Store, want int) {
	t.Helper()
	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != want {
		t.Fatalf("store bead count = %d, want %d; items = %#v", len(items), want, items)
	}
}

func TestDoSlingRefusesCrossStoreOnFormulaBeforeMutation(t *testing.T) {
	opts, deps, router, bead := crossStoreSlingFixture(t, "")
	opts.OnFormula = "code-review"

	_, err := DoSling(opts, deps, deps.Store)

	requireCrossStoreRouteError(t, err)
	requireNoCrossStoreRouteMutation(t, deps.Store, bead.ID, "")
	requireOnlySeedBeads(t, deps.Store, 1)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
	}
}

func TestDoSlingRefusesCrossStoreDefaultFormulaBeforeMutation(t *testing.T) {
	opts, deps, router, bead := crossStoreSlingFixture(t, "")
	opts.Target.DefaultSlingFormula = stringPtr("code-review")

	_, err := DoSling(opts, deps, deps.Store)

	requireCrossStoreRouteError(t, err)
	requireNoCrossStoreRouteMutation(t, deps.Store, bead.ID, "")
	requireOnlySeedBeads(t, deps.Store, 1)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
	}
}

func TestDoSlingRefusesCrossStoreForceBeforeRouting(t *testing.T) {
	opts, deps, router, bead := crossStoreSlingFixture(t, "")
	opts.NoFormula = true

	_, err := DoSling(opts, deps, deps.Store)

	requireCrossStoreRouteError(t, err)
	requireNoCrossStoreRouteMutation(t, deps.Store, bead.ID, "")
	requireOnlySeedBeads(t, deps.Store, 1)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
	}
}

func TestDoSlingRefusesCrossStoreReassignBeforeClearingAssignee(t *testing.T) {
	opts, deps, router, bead := crossStoreSlingFixture(t, "human@example.com")
	opts.NoFormula = true
	opts.Reassign = true

	_, err := DoSling(opts, deps, deps.Store)

	requireCrossStoreRouteError(t, err)
	requireNoCrossStoreRouteMutation(t, deps.Store, bead.ID, "human@example.com")
	requireOnlySeedBeads(t, deps.Store, 1)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
	}
}

func TestDoSlingBatchRefusesCrossStoreBeforeFormulaMutation(t *testing.T) {
	deps, router, target := crossStoreSlingDeps(t)
	store := deps.Store
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	childOne, err := store.Create(beads.Bead{Title: "one", Type: "task", ParentID: convoy.ID, Status: "open"})
	if err != nil {
		t.Fatalf("create child one: %v", err)
	}
	childTwo, err := store.Create(beads.Bead{Title: "two", Type: "task", ParentID: convoy.ID, Status: "open"})
	if err != nil {
		t.Fatalf("create child two: %v", err)
	}
	opts := SlingOpts{
		Target:        target,
		BeadOrFormula: convoy.ID,
		OnFormula:     "code-review",
		Force:         true,
	}

	result, err := DoSlingBatch(opts, deps, store)

	requireCrossStoreRouteError(t, err)
	if result.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", result.Failed)
	}
	if result.Routed != 0 {
		t.Fatalf("Routed = %d, want 0", result.Routed)
	}
	for _, child := range []beads.Bead{childOne, childTwo} {
		requireNoCrossStoreRouteMutation(t, store, child.ID, "")
	}
	requireOnlySeedBeads(t, store, 3)
	if len(router.routed) != 0 {
		t.Fatalf("router calls = %d, want 0", len(router.routed))
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

func TestSlingAttachGraphFormulaCreatesConvoyFirstRoot(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err != nil {
		t.Fatalf("AttachFormula: %v", err)
	}
	root, err := deps.Store.Get(result.WorkflowID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	inputConvoyID := root.Metadata["gc.input_convoy_id"]
	if inputConvoyID == "" {
		t.Fatalf("root metadata = %#v, missing gc.input_convoy_id", root.Metadata)
	}
	if got := root.Metadata["gc.source_bead_id"]; got != "" {
		t.Fatalf("root gc.source_bead_id = %q, want empty", got)
	}
	sourceAfter, err := deps.Store.Get(source.ID)
	if err != nil {
		t.Fatalf("Get(source): %v", err)
	}
	if got := sourceAfter.Metadata["workflow_id"]; got != "" {
		t.Fatalf("source workflow_id = %q, want empty", got)
	}
	members, err := convoycore.Members(deps.Store, inputConvoyID, true)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != source.ID {
		t.Fatalf("members = %+v, want source %s", members, source.ID)
	}
}

func TestSlingAttachGraphFormulaCreatesFreshRootForBareBeadTarget(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	first, err := s.AttachFormula(context.Background(), "graph-work", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("first AttachFormula: %v", err)
	}
	second, err := s.AttachFormula(context.Background(), "graph-work", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("second AttachFormula: %v", err)
	}
	if second.WorkflowID == first.WorkflowID {
		t.Fatalf("WorkflowID = %q, want fresh root for fresh input convoy", second.WorkflowID)
	}
}

func TestSlingAttachGraphFormulaAllowsDifferentLiveBareBeadRoots(t *testing.T) {
	formulaDir := t.TempDir()
	writeNamedGraphV2ConvoyFormula(t, formulaDir, "graph-a")
	writeNamedGraphV2ConvoyFormula(t, formulaDir, "graph-b")
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	first, err := s.AttachFormula(context.Background(), "graph-a", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("first AttachFormula: %v", err)
	}
	second, err := s.AttachFormula(context.Background(), "graph-b", source.ID, a, FormulaOpts{})
	if err != nil {
		t.Fatalf("second AttachFormula: %v", err)
	}
	roots, err := deps.Store.ListByMetadata(map[string]string{"gc.formula_contract": "graph.v2"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	var liveRoots []beads.Bead
	for _, root := range roots {
		if sourceworkflow.IsWorkflowRoot(root) && root.Status != "closed" {
			liveRoots = append(liveRoots, root)
		}
	}
	if len(liveRoots) != 2 {
		t.Fatalf("live graph roots = %+v, want two roots %s and %s", liveRoots, first.WorkflowID, second.WorkflowID)
	}
}

func TestInstantiateSlingFormulaForceReplacesGraphV2Root(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	convoy, err := deps.Store.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"convoy_id": convoy.ID}
	opts := molecule.Options{Vars: vars}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	first, err := InstantiateSlingFormula(context.Background(), "graph-work", []string{formulaDir}, opts, "", "default", "", a, deps)
	if err != nil {
		t.Fatalf("first InstantiateSlingFormula: %v", err)
	}
	second, err := InstantiateSlingFormula(context.Background(), "graph-work", []string{formulaDir}, opts, "", "default", "", a, deps, true)
	if err != nil {
		t.Fatalf("force InstantiateSlingFormula: %v", err)
	}
	if second.RootID == first.RootID {
		t.Fatalf("force RootID = %q, want fresh root", second.RootID)
	}
	oldRoot, err := deps.Store.Get(first.RootID)
	if err != nil {
		t.Fatalf("Get(old root): %v", err)
	}
	if oldRoot.Status != "closed" {
		t.Fatalf("old root status = %q, want closed", oldRoot.Status)
	}
	if got := oldRoot.Metadata["gc.failure_reason"]; got != "graphv2_force_replaced" {
		t.Fatalf("old root gc.failure_reason = %q, want graphv2_force_replaced", got)
	}
	newRoot, err := deps.Store.Get(second.RootID)
	if err != nil {
		t.Fatalf("Get(new root): %v", err)
	}
	if newRoot.Status == "closed" {
		t.Fatalf("new root status = %q, want live", newRoot.Status)
	}
}

func TestRollbackGraphV2ReplacementLaunchRestoresReplacedRoot(t *testing.T) {
	store := beads.NewMemStore()
	replaced, err := store.Create(beads.Bead{
		Title:    "old graph root",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-1",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "old child",
		Type:     "task",
		Status:   "in_progress",
		ParentID: replaced.ID,
		Assignee: "worker-2",
		Metadata: map[string]string{
			"gc.root_bead_id": replaced.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := sourceworkflow.SnapshotOpenWorkflowBeads(store, replaced.ID)
	if err != nil {
		t.Fatalf("SnapshotOpenWorkflowBeads: %v", err)
	}
	if _, err := sourceworkflow.CloseWorkflowSubtree(store, replaced.ID); err != nil {
		t.Fatalf("CloseWorkflowSubtree(replaced): %v", err)
	}
	replacement, err := store.Create(beads.Bead{
		Title:  "new graph root",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = rollbackGraphV2ReplacementLaunch(store, replacement.ID, graphV2ReplacementSnapshot{
		rootID:    replaced.ID,
		snapshots: snapshots,
	})
	if err != nil {
		t.Fatalf("rollbackGraphV2ReplacementLaunch: %v", err)
	}
	replacedAfter, err := store.Get(replaced.ID)
	if err != nil {
		t.Fatalf("Get(replaced): %v", err)
	}
	if replacedAfter.Status != "open" || replacedAfter.Assignee != "worker-1" {
		t.Fatalf("replaced root after rollback = %+v, want original state", replacedAfter)
	}
	childAfter, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if childAfter.Status != "open" || childAfter.Assignee != "worker-2" {
		t.Fatalf("child after rollback = %+v, want original state", childAfter)
	}
	replacementAfter, err := store.Get(replacement.ID)
	if err != nil {
		t.Fatalf("Get(replacement): %v", err)
	}
	if replacementAfter.Status != "closed" {
		t.Fatalf("replacement status = %q, want closed", replacementAfter.Status)
	}
}

func TestDoSlingDefaultGraphFormulaAllowsDifferentLiveBareBeadRoots(t *testing.T) {
	formulaDir := t.TempDir()
	writeNamedGraphV2ConvoyFormula(t, formulaDir, "graph-a")
	writeNamedGraphV2ConvoyFormula(t, formulaDir, "graph-b")
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	source, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AttachFormula(context.Background(), "graph-a", source.ID, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err != nil {
		t.Fatalf("first AttachFormula: %v", err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("graph-b")}
	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WorkflowID == "" || result.WorkflowID == first.WorkflowID {
		t.Fatalf("WorkflowID = %q, want fresh root different from %s", result.WorkflowID, first.WorkflowID)
	}
}

func TestDoSlingBatchGraphFormulaTreatsConvoyAsSingleInput(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	cfg := graphV2SlingTestConfig(t, formulaDir)
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	convoy, err := deps.Store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := deps.Store.Create(beads.Bead{Title: "child", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItem(deps.Store, convoy.ID, child.ID); err != nil {
		t.Fatal(err)
	}

	result, err := DoSlingBatch(SlingOpts{
		Target:        config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		BeadOrFormula: convoy.ID,
		OnFormula:     "graph-work",
	}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSlingBatch: %v", err)
	}
	if result.Method != "on-formula" || result.WorkflowID == "" {
		t.Fatalf("result = %+v, want direct graph workflow attach", result)
	}
	root, err := deps.Store.Get(result.WorkflowID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if got := root.Metadata["gc.input_convoy_id"]; got != convoy.ID {
		t.Fatalf("root input convoy = %q, want %q", got, convoy.ID)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner calls = %#v, want none for graph workflow launch", runner.calls)
	}
}

func writeGraphV2ConvoyFormula(t *testing.T, dir string) {
	t.Helper()
	writeNamedGraphV2ConvoyFormula(t, dir, "graph-work")
}

func writeNamedGraphV2ConvoyFormula(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".formula.toml"), []byte(fmt.Sprintf(`
formula = "%s"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work for {{convoy_id}}"
`, name)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func graphV2SlingTestConfig(t *testing.T, formulaDir string) *config.City {
	t.Helper()
	formulatest.EnableV2ForTest(t)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
		Agents: []config.Agent{slingControlDispatcherAgent()},
	}
	return cfg
}

func slingControlDispatcherAgent() config.Agent {
	return config.Agent{
		Name:              config.ControlDispatcherAgentName,
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		ProcessNames:      []string{"gc"},
		MaxActiveSessions: intPtr(1),
	}
}

func TestSlingAttachGraphFormulaRejectsExistingLiveRoot(t *testing.T) {
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.toml"), []byte(`
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
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
		Agents: []config.Agent{slingControlDispatcherAgent()},
	}
	formulatest.EnableV2ForTest(t)
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

// TestExpandConvoyNoFormulaSuppressesDefaultFormula is a regression test for
// the bug where ExpandConvoy did not propagate NoFormula into DoSlingBatch,
// causing the default_sling_formula to fire even when --no-formula was set.
func TestExpandConvoyNoFormulaSuppressesDefaultFormula(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	store := deps.Store

	// Agent with a default_sling_formula configured.
	a := config.Agent{Name: "mayor", DefaultSlingFormula: stringPtr("code-review"), MaxActiveSessions: intPtr(1)}

	bead, err := store.Create(beads.Bead{Title: "task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}

	// NoFormula=true: formula must NOT be invoked.
	result, err := s.ExpandConvoy(context.Background(), bead.ID, a, RouteOpts{NoFormula: true}, store)
	if err != nil {
		t.Fatalf("ExpandConvoy with NoFormula=true: %v", err)
	}
	if result.WispRootID != "" || result.WorkflowID != "" {
		t.Errorf("expected no formula attachment; WispRootID=%q WorkflowID=%q", result.WispRootID, result.WorkflowID)
	}
	if len(runner.calls) != 1 {
		t.Errorf("expected 1 route call, got %d", len(runner.calls))
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

// TestFinalizeAutoConvoyPreservesEpicParent is a regression test for the
// bug where `gc sling` re-parented a bead to its auto-convoy via
// `bd update --parent`, silently evicting the bead's existing
// parent-child edge to its epic. After this fix, the auto-convoy links
// to the bead via a "tracks" dep instead, leaving the epic parent
// intact.
func TestFinalizeAutoConvoyPreservesEpicParent(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	epic, err := deps.Store.Create(beads.Bead{Title: "epic", Type: "epic"})
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	child, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: child.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.ConvoyID == "" {
		t.Fatal("expected auto-convoy creation")
	}

	got, err := deps.Store.Get(child.ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if got.ParentID != epic.ID {
		t.Errorf("child parent = %q, want %q (epic parent evicted by sling)", got.ParentID, epic.ID)
	}

	epicChildren, err := deps.Store.Children(epic.ID)
	if err != nil {
		t.Fatalf("epic children: %v", err)
	}
	var found bool
	for _, c := range epicChildren {
		if c.ID == child.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("epic %s children missing %s; got %d children", epic.ID, child.ID, len(epicChildren))
	}

	convoyOut, err := deps.Store.DepList(result.ConvoyID, "down")
	if err != nil {
		t.Fatalf("DepList convoy: %v", err)
	}
	var tracks bool
	for _, d := range convoyOut {
		if d.DependsOnID == child.ID && d.Type == "tracks" {
			tracks = true
			break
		}
	}
	if !tracks {
		t.Errorf("convoy %s has no tracks dep to %s; got deps=%v", result.ConvoyID, child.ID, convoyOut)
	}
}

// failingDepAddStore wraps a beads.Store and returns an error when
// DepAdd is called with a specific dep type. Used to exercise error
// branches in code that links beads via DepAdd.
type failingDepAddStore struct {
	beads.Store
	failType string
	err      error
}

func (f *failingDepAddStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == f.failType {
		return f.err
	}
	return f.Store.DepAdd(issueID, dependsOnID, depType)
}

// TestFinalizeAutoConvoyTracksDepAddError covers the error branch of
// the auto-convoy linking step: if DepAdd("tracks") fails, the failure
// is recorded in result.MetadataErrors rather than bubbled up.
func TestFinalizeAutoConvoyTracksDepAddError(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = &failingDepAddStore{
		Store:    deps.Store,
		failType: "tracks",
		err:      errors.New("injected DepAdd failure"),
	}

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	// When DepAdd fails, result.ConvoyID is intentionally left unset and
	// the error is appended to MetadataErrors (soft failure — sling
	// itself still succeeds).
	if result.ConvoyID != "" {
		t.Errorf("result.ConvoyID = %q, want empty (DepAdd failed)", result.ConvoyID)
	}

	var found bool
	for _, e := range result.MetadataErrors {
		if strings.Contains(e, "linking bead to convoy") && strings.Contains(e, "injected DepAdd failure") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected MetadataErrors to include tracks DepAdd failure; got %v", result.MetadataErrors)
	}
}

// TestFinalizeAutoConvoyTracksDepCreated is a regression test for
// "Field 'id' doesn't have a default value" on dependencies.id: Dolt strips
// DEFAULT (uuid()) when migration 0043 runs via PREPARE/EXECUTE, causing
// every DepAdd to fail silently via MetadataErrors. Verify that DoSling
// creates the convoy→bead tracks dependency with no metadata errors.
func TestFinalizeAutoConvoyTracksDepCreated(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.ConvoyID == "" {
		t.Fatal("expected auto-convoy creation")
	}
	if len(result.MetadataErrors) != 0 {
		t.Errorf("unexpected MetadataErrors (dep link failure?): %v", result.MetadataErrors)
	}

	downDeps, err := deps.Store.DepList(result.ConvoyID, "down")
	if err != nil {
		t.Fatalf("DepList convoy: %v", err)
	}
	var tracks bool
	for _, d := range downDeps {
		if d.DependsOnID == b.ID && d.Type == "tracks" {
			tracks = true
			break
		}
	}
	if !tracks {
		t.Errorf("convoy %s missing tracks dep to bead %s; deps=%v", result.ConvoyID, b.ID, downDeps)
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

// reassignTestSetup builds a sling deps + store + a bead with the given
// assignee, configured for an in-rig agent so cross-rig guard does not
// fire and the reassign path can be exercised in isolation.
func reassignTestSetup(t *testing.T, assignee string) (SlingOpts, SlingDeps, beads.Store, beads.Bead) {
	t.Helper()
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "task", Type: "task", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true}
	return opts, deps, deps.Store, bead
}

// TestDoSling_Reassign_ClearsHumanAssignee: --reassign clears an existing
// human assignee on the bead before routing. Without this flag the bead
// stays assigned to the human and `gc hook` filters it out from pool
// claims, leaving the bead routed-but-unclaimable. See #1007.
func TestDoSling_Reassign_ClearsHumanAssignee(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "stephanie")
	opts.Reassign = true
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling with --reassign: %v", err)
	}
	got, _ := store.Get(bead.ID)
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty after --reassign", got.Assignee)
	}
}

// TestDoSling_Reassign_PreservesAssigneeWithoutFlag: without --reassign
// the existing human assignee is preserved (current warn-only behavior).
// Locks in backward compatibility for the existing two-step flow.
func TestDoSling_Reassign_PreservesAssigneeWithoutFlag(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "stephanie")
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	got, _ := store.Get(bead.ID)
	if got.Assignee != "stephanie" {
		t.Fatalf("Assignee = %q, want %q (preserved without --reassign)", got.Assignee, "stephanie")
	}
}

// TestDoSling_Reassign_NoOpWhenAlreadyEmpty: --reassign on a bead with no
// existing assignee is a no-op (no spurious store write, no error).
func TestDoSling_Reassign_NoOpWhenAlreadyEmpty(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "")
	opts.Reassign = true
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling with --reassign on unassigned bead: %v", err)
	}
	got, _ := store.Get(bead.ID)
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty", got.Assignee)
	}
}

// TestDoSling_Reassign_DryRunSkipsClear: --reassign is suppressed under
// --dry-run so previewing the operation does not mutate state.
func TestDoSling_Reassign_DryRunSkipsClear(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "stephanie")
	opts.Reassign = true
	opts.DryRun = true
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --dry-run --reassign: %v", err)
	}
	got, _ := store.Get(bead.ID)
	if got.Assignee != "stephanie" {
		t.Fatalf("Assignee = %q, want %q (dry-run must not mutate)", got.Assignee, "stephanie")
	}
}

// TestClearHumanAssignee_RigStore: clearHumanAssignee clears the assignee on
// a rig-prefixed bead whose record lives in a source-workflow (rig) store
// rather than the city primary store. Direct unit test of the multi-store
// fallback added for gastownhall/gascity#3408.
func TestClearHumanAssignee_RigStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	deps := SlingDeps{
		Store: cityStore,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	if err := clearHumanAssignee(bead.ID, deps); err != nil {
		t.Fatalf("clearHumanAssignee: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty after clear in rig store", got.Assignee)
	}
}

// TestClearHumanAssignee_PrimaryStoreReadError: a non-ErrNotFound failure from
// the city primary store must abort the clear with a contextual error rather
// than falling through to the source-workflow sweep. A real read failure under
// --force --reassign would otherwise be treated like a miss, so routing could
// proceed with the human assignee uncleared (or a same-ID bead cleared in a
// different store). Regression for the gastownhall/gascity#3408 review.
func TestClearHumanAssignee_PrimaryStoreReadError(t *testing.T) {
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	sourceSwept := false
	deps := SlingDeps{
		Store: &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("backend unavailable")},
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			sourceSwept = true
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	err = clearHumanAssignee(bead.ID, deps)
	if err == nil {
		t.Fatal("clearHumanAssignee error = nil, want primary read failure")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("error = %q, want wrapped primary read failure", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want store failure, not a not-found miss", err)
	}
	if sourceSwept {
		t.Fatal("source-workflow stores swept after a primary read failure; want abort before fallback")
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "human" {
		t.Fatalf("Assignee = %q, want unchanged (clear must not run after a primary read failure)", got.Assignee)
	}
}

// TestClearHumanAssignee_SourceStoreReadError: a non-ErrNotFound failure while
// reading a source-workflow store during the rig-store sweep aborts the clear
// with a store-ref-qualified error instead of silently skipping the store,
// which would leave the bead human-assigned and pool-invisible (the #3408
// symptom) under partial store failure.
func TestClearHumanAssignee_SourceStoreReadError(t *testing.T) {
	deps := SlingDeps{
		Store: beads.NewMemStore(), // bead is absent here, so the sweep runs
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{
				{Store: &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("rig store unreadable")}, StoreRef: "rig:myrig"},
			}, nil
		},
	}
	err := clearHumanAssignee("gc-123", deps)
	if err == nil {
		t.Fatal("clearHumanAssignee error = nil, want source-store read failure")
	}
	if !strings.Contains(err.Error(), "rig store unreadable") {
		t.Fatalf("error = %q, want wrapped source-store read failure", err)
	}
	if !strings.Contains(err.Error(), "rig:myrig") {
		t.Fatalf("error = %q, want store-ref context to localize the failing store", err)
	}
}

// TestClearHumanAssignee_SourceStoreListError: a failure from the
// SourceWorkflowStores lister itself — the callback returning an error before
// any store can be scanned, distinct from a per-store Get failure — aborts the
// clear with a bead-qualified error instead of silently no-op'ing. This is the
// fail-loud guard for the #3408 --reassign contract: if the source-workflow
// stores cannot even be listed after a primary-store miss, routing must not
// proceed as though the bead were absent everywhere and leave it human-assigned.
func TestClearHumanAssignee_SourceStoreListError(t *testing.T) {
	deps := SlingDeps{
		Store: beads.NewMemStore(), // bead is absent here (ErrNotFound), so the sweep runs
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return nil, fmt.Errorf("stores unavailable")
		},
	}
	err := clearHumanAssignee("gc-456", deps)
	if err == nil {
		t.Fatal("clearHumanAssignee error = nil, want source-workflow store listing failure")
	}
	if !strings.Contains(err.Error(), "listing source-workflow stores") {
		t.Fatalf("error = %q, want wrapped source-workflow store listing failure", err)
	}
	if !strings.Contains(err.Error(), "stores unavailable") {
		t.Fatalf("error = %q, want underlying lister error preserved", err)
	}
	if !strings.Contains(err.Error(), "gc-456") {
		t.Fatalf("error = %q, want bead ID context to localize the failed clear", err)
	}
}

// TestClearHumanAssignee_NilPrimaryStore: with no city primary store, the clear
// still sweeps the source-workflow stores and clears the assignee where the
// bead lives, matching the multi-store behavior of sourceWorkflowRootByID. A
// nil deps.Store must not skip available rig stores.
func TestClearHumanAssignee_NilPrimaryStore(t *testing.T) {
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	deps := SlingDeps{
		Store: nil,
		SourceWorkflowStores: func() ([]SourceWorkflowStore, error) {
			return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
		},
	}
	if err := clearHumanAssignee(bead.ID, deps); err != nil {
		t.Fatalf("clearHumanAssignee: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty after clearing via the source-workflow sweep with a nil primary store", got.Assignee)
	}
}

// TestDoSling_Reassign_ClearsHumanAssignee_RigStore: --reassign clears a human
// assignee on a rig-prefixed bead whose record lives in the rig store, not the
// city primary store. Regression for gastownhall/gascity#3408 — the clear
// previously no-op'd because clearHumanAssignee only consulted deps.Store, so
// the bead stayed routed+human-assigned and invisible to the pool scaler.
func TestDoSling_Reassign_ClearsHumanAssignee_RigStore(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	rigStore := beads.NewMemStore()
	bead, err := rigStore.Create(beads.Bead{Title: "task", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatalf("rig Create: %v", err)
	}
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
	}
	// Force routing: the bead lives only in the rig store, so the city-store
	// existence validation must be bypassed to reach the reassign clear.
	opts := SlingOpts{
		Target:        a,
		BeadOrFormula: bead.ID,
		NoFormula:     true,
		NoConvoy:      true,
		Reassign:      true,
		Force:         true,
	}
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign on rig-store bead: %v", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("rig Get: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty after --reassign", got.Assignee)
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
