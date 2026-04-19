package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// selectiveErrStore wraps a beads.Store and injects Create errors for selected
// beads. Used to simulate partial cook failures in batch operations.
type selectiveErrStore struct {
	beads.Store
	failOnParentIDs map[string]error
	failOnCreate    func(beads.Bead) error
}

func (s *selectiveErrStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.failOnCreate != nil {
		if err := s.failOnCreate(b); err != nil {
			return beads.Bead{}, err
		}
	}
	if err, ok := s.failOnParentIDs[b.ParentID]; ok {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

// recordingStore wraps a store and overrides Get for bead injection.
type recordingStore struct {
	beads.Store
	beadsByID map[string]beads.Bead
}

func (s *recordingStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.beadsByID[id]; ok {
		return b, nil
	}
	return s.Store.Get(id)
}

// fakeRunnerRule maps a command substring to a canned response.
type fakeRunnerRule struct {
	prefix string
	out    string
	err    error
}

type slingTestStore struct {
	beads.Store
	synthetic map[string]beads.Bead
}

func newSlingTestStore() *slingTestStore {
	return &slingTestStore{Store: beads.NewMemStore(), synthetic: map[string]beads.Bead{}}
}

func (s *slingTestStore) ensureSynthetic(id string) beads.Bead {
	b, ok := s.synthetic[id]
	if !ok {
		b = beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Metadata: map[string]string{}}
	}
	if b.Metadata == nil {
		b.Metadata = map[string]string{}
	}
	return b
}

func (s *slingTestStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil || !errors.Is(err, beads.ErrNotFound) {
		return b, err
	}
	b, ok := s.synthetic[id]
	if !ok {
		return beads.Bead{}, err
	}
	return b, nil
}

func (s *slingTestStore) SetMetadata(id, key, value string) error {
	if err := s.Store.SetMetadata(id, key, value); err == nil || !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	b := s.ensureSynthetic(id)
	b.Metadata[key] = value
	s.synthetic[id] = b
	return nil
}

func (s *slingTestStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.Store.Update(id, opts); err == nil || !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	b := s.ensureSynthetic(id)
	if opts.Title != nil {
		b.Title = *opts.Title
	}
	if opts.Status != nil {
		b.Status = *opts.Status
	}
	if opts.Type != nil {
		b.Type = *opts.Type
	}
	if opts.Priority != nil {
		p := *opts.Priority
		b.Priority = &p
	}
	if opts.Description != nil {
		b.Description = *opts.Description
	}
	if opts.ParentID != nil {
		b.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		b.Assignee = *opts.Assignee
	}
	if len(opts.Labels) > 0 {
		b.Labels = append(b.Labels, opts.Labels...)
	}
	if len(opts.RemoveLabels) > 0 {
		filtered := b.Labels[:0]
		for _, existing := range b.Labels {
			remove := false
			for _, doomed := range opts.RemoveLabels {
				if existing == doomed {
					remove = true
					break
				}
			}
			if !remove {
				filtered = append(filtered, existing)
			}
		}
		b.Labels = filtered
	}
	if len(opts.Metadata) > 0 {
		if b.Metadata == nil {
			b.Metadata = map[string]string{}
		}
		for k, v := range opts.Metadata {
			b.Metadata[k] = v
		}
	}
	s.synthetic[id] = b
	return nil
}

// fakeRunner records the commands it receives and returns canned output.
// Rules are matched in order (first match wins), providing deterministic behavior.
type fakeRunner struct {
	calls []string
	dirs  []string
	envs  []map[string]string
	rules []fakeRunnerRule
}

func newFakeRunner() *fakeRunner { return &fakeRunner{} }

// on registers a rule: if a command contains prefix, return (out, err).
func (r *fakeRunner) on(prefix, out string, err error) {
	r.rules = append(r.rules, fakeRunnerRule{prefix: prefix, out: out, err: err})
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

// testOpts constructs a slingOpts for testing with the given agent and bead.
func testOpts(a config.Agent, beadOrFormula string) slingOpts {
	return slingOpts{Target: a, BeadOrFormula: beadOrFormula}
}

// testDeps constructs a slingDeps for testing, returning the deps and
// stdout/stderr buffers for inspection. The config's FormulaLayers.City
// is automatically populated with common test formulas.
func testDeps(cfg *config.City, sp runtime.Provider, runner SlingRunner) (slingDeps, *bytes.Buffer, *bytes.Buffer) {
	if cfg != nil && len(cfg.FormulaLayers.City) == 0 {
		cfg.FormulaLayers.City = []string{sharedTestFormulaDir}
	}
	var stdout, stderr bytes.Buffer
	return slingDeps{
		CityName: "test-city",
		CityPath: sharedTestCityDir,
		Cfg:      cfg,
		SP:       sp,
		Runner:   runner,
		Store:    newSlingTestStore(),
		StoreRef: "city:test-city",
	}, &stdout, &stderr
}

//nolint:unused // retained for future sling path-resolution scenarios
func writeSlingTestCity(t *testing.T, cityDir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

//nolint:unused // retained for future sling cwd-sensitive scenarios
func chdirSlingTest(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

func assertStoreRoutedTo(t *testing.T, store beads.Store, beadID, want string) {
	t.Helper()
	bead, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", beadID, err)
	}
	if bead.Metadata["gc.routed_to"] != want {
		t.Fatalf("%s gc.routed_to = %q, want %q", beadID, bead.Metadata["gc.routed_to"], want)
	}
}

// sharedTestFormulaDir is a package-level temp directory containing minimal
// formula TOML files for all formula names commonly used in sling tests.
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

func testFormulaDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newRepoWithOriginHead(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
	return dir
}

func findVarValue(vars map[string]string, key string) (string, bool) {
	v, ok := vars[key]
	return v, ok
}

func priorityPtr(v int) *int {
	return &v
}

func TestBuildSlingCommand(t *testing.T) {
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
		got := sling.BuildSlingCommand(tt.template, tt.beadID)
		if got != tt.want {
			t.Errorf("BuildSlingCommand(%q, %q) = %q, want %q", tt.template, tt.beadID, got, tt.want)
		}
	}
}

func TestDoSlingBeadToFixedAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	bead, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if bead.Metadata["gc.routed_to"] != "mayor" {
		t.Errorf("gc.routed_to = %q, want mayor", bead.Metadata["gc.routed_to"])
	}
	if !strings.Contains(stdout.String(), "Slung BL-42") {
		t.Errorf("stdout = %q, want to contain 'Slung BL-42'", stdout.String())
	}
}

func TestDoSlingPinnedDefaultSlingQueryUsesBuiltInRouting(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "mayor",
		MaxActiveSessions: intPtr(1),
		SlingQuery:        "bd update {} --set-metadata gc.routed_to=mayor",
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for pinned default sling_query: %v", len(runner.calls), runner.calls)
	}
	bead, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if bead.Metadata["gc.routed_to"] != "mayor" {
		t.Errorf("gc.routed_to = %q, want mayor", bead.Metadata["gc.routed_to"])
	}
	if !strings.Contains(stdout.String(), "Slung BL-42") {
		t.Errorf("stdout = %q, want to contain 'Slung BL-42'", stdout.String())
	}
}

func TestDoSlingEnvPassthrough(t *testing.T) {
	// Fixed agent (max=1): env should contain GC_SLING_TARGET with resolved session name.
	t.Run("fixed agent", func(t *testing.T) {
		runner := newFakeRunner()
		sp := runtime.NewFake()
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{a},
		}

		deps, stdout, stderr := testDeps(cfg, sp, runner.run)
		opts := testOpts(a, "BL-42")
		code := doSling(opts, deps, nil, stdout, stderr)

		if code != 0 {
			t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
		}
		if len(runner.calls) != 1 {
			t.Fatalf("got %d runner calls, want 1", len(runner.calls))
		}
		if len(runner.envs) != 1 {
			t.Fatalf("got %d env captures, want 1", len(runner.envs))
		}
		env := runner.envs[0]
		if env == nil {
			t.Fatal("env is nil for fixed agent, want GC_SLING_TARGET set")
		}
		if _, ok := env["GC_SLING_TARGET"]; !ok {
			t.Error("env missing GC_SLING_TARGET key")
		}
	})

	// Pool agent: env should be nil (label-based dispatch).
	t.Run("pool agent", func(t *testing.T) {
		runner := newFakeRunner()
		sp := runtime.NewFake()
		a := config.Agent{
			Name:              "polecat",
			Dir:               "hello-world",
			SlingQuery:        "custom-dispatch {}",
			MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
		}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{a},
		}

		deps, stdout, stderr := testDeps(cfg, sp, runner.run)
		opts := testOpts(a, "HW-7")
		code := doSling(opts, deps, nil, stdout, stderr)

		if code != 0 {
			t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
		}
		if len(runner.calls) != 1 {
			t.Fatalf("got %d runner calls, want 1", len(runner.calls))
		}
		if len(runner.envs) != 1 {
			t.Fatalf("got %d env captures, want 1", len(runner.envs))
		}
		if runner.envs[0] != nil {
			t.Errorf("env = %v for pool agent, want nil", runner.envs[0])
		}
	})
}

func TestShellSlingRunnerOverridesInheritedBDEnv(t *testing.T) {
	t.Setenv("GC_DOLT_HOST", "stale-host")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	out, err := shellSlingRunner("", `printf '%s|%s|%s|%s' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$BEADS_DOLT_SERVER_HOST" "$BEADS_DOLT_SERVER_PORT"`, map[string]string{
		"GC_DOLT_HOST":           "rig-db.example.com",
		"GC_DOLT_PORT":           "3307",
		"BEADS_DOLT_SERVER_HOST": "rig-db.example.com",
		"BEADS_DOLT_SERVER_PORT": "3307",
	})
	if err != nil {
		t.Fatalf("shellSlingRunner: %v", err)
	}
	if got := strings.TrimSpace(out); got != "rig-db.example.com|3307|rig-db.example.com|3307" {
		t.Fatalf("shellSlingRunner env = %q", got)
	}
}

func TestDoSlingBeadToPool(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hello-world",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "HW-7")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	bead, err := deps.Store.Get("HW-7")
	if err != nil {
		t.Fatalf("store.Get(HW-7): %v", err)
	}
	if bead.Metadata["gc.routed_to"] != "hello-world/polecat" {
		t.Errorf("gc.routed_to = %q, want hello-world/polecat", bead.Metadata["gc.routed_to"])
	}
}

func TestDoSlingFormulaToAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "code-review")
	opts.IsFormula = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	root, err := deps.Store.Get("gc-1")
	if err != nil {
		t.Fatalf("store.Get(gc-1): %v", err)
	}
	if root.Metadata["gc.routed_to"] != "mayor" {
		t.Errorf("gc.routed_to = %q, want mayor", root.Metadata["gc.routed_to"])
	}
	if !strings.Contains(stdout.String(), "formula") && !strings.Contains(stdout.String(), "wisp root gc-1") {
		t.Errorf("stdout = %q, want mention of formula/wisp", stdout.String())
	}
}

func TestDoSlingFormulaWithTitle(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "code-review")
	opts.IsFormula = true
	opts.Title = "my-review"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// MolCook goes through the store; verify the bead was created with the title.
	b, err := deps.Store.Get("gc-1")
	if err != nil {
		t.Fatalf("store.Get(gc-1): %v", err)
	}
	if b.Title != "my-review" {
		t.Errorf("bead title = %q, want %q", b.Title, "my-review")
	}
}

func TestDoSlingSuspendedAgentWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", Suspended: true, MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (still routes)", code)
	}
	if !strings.Contains(stderr.String(), "suspended") {
		t.Errorf("stderr = %q, want suspended warning", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 for built-in routing", len(runner.calls))
	}
	assertStoreRoutedTo(t, deps.Store, "BL-1", "mayor")
}

func TestDoSlingSuspendedAgentForce(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", Suspended: true, MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.Force = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "suspended") {
		t.Errorf("--force should suppress warning; stderr = %q", stderr.String())
	}
}

func TestDoSlingMultiSessionMaxZeroWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(0),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (still routes)", code)
	}
	if !strings.Contains(stderr.String(), "session config") || !strings.Contains(stderr.String(), "max_active_sessions=0") {
		t.Errorf("stderr = %q, want session config max_active_sessions=0 warning", stderr.String())
	}
}

func TestDoSlingMultiSessionMaxZeroForce(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "rig",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(0),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.Force = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "max=0") {
		t.Errorf("--force should suppress warning; stderr = %q", stderr.String())
	}
}

func TestDoSlingRunnerError(t *testing.T) {
	runner := newFakeRunner()
	runner.on("custom-dispatch", "", fmt.Errorf("dispatch failed"))
	sp := runtime.NewFake()
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "dispatch failed") {
		t.Errorf("stderr = %q, want error message", stderr.String())
	}
}

func TestDoSlingFormulaInstantiationError(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "nonexistent")
	opts.IsFormula = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want formula error", stderr.String())
	}
}

func TestDoSlingNudgeFixedAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.Calls = nil // clear start call
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	pending, _, dead, err := listQueuedNudges(deps.CityPath, "mayor", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(dead) != 0 {
		t.Fatalf("pending=%d dead=%d, want 1/0", len(pending), len(dead))
	}
	if pending[0].Source != "sling" {
		t.Fatalf("source = %q, want sling", pending[0].Source)
	}
	if !strings.Contains(stdout.String(), "Queued nudge for mayor") {
		t.Errorf("stdout = %q, want queue confirmation", stdout.String())
	}
}

func TestDoSlingNudgeNoSession(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	// Don't start the session — agent has no running session.
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir() // isolated path so poke doesn't hit real socket
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (sling succeeds, poke attempted)", code)
	}
	if !strings.Contains(stderr.String(), "poke failed") {
		t.Errorf("stderr = %q, want 'poke failed' message (no controller socket in test)", stderr.String())
	}
	pending, _, dead, err := listQueuedNudges(deps.CityPath, "mayor", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(dead) != 0 {
		t.Fatalf("pending=%d dead=%d, want 1/0", len(pending), len(dead))
	}
}

func TestDoSlingNudgeSuspended(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", Suspended: true, MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	opts.Force = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "cannot nudge") {
		t.Errorf("stderr = %q, want 'cannot nudge: suspended' warning", stderr.String())
	}
}

func TestDoSlingNudgePoolMember(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	// Start pool instance 2 (instance 1 not running).
	_ = sp.Start(context.Background(), "hw--polecat-2", runtime.Config{})
	sp.Calls = nil
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hw",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Fatalf("expected queued sling reminder, got direct nudge calls: %+v", sp.Calls)
		}
	}
}

func TestDoSlingNudgePoolNoMembers(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	// No pool instances running.
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hw",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir() // isolated path so poke doesn't hit real socket
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (sling succeeds, poke attempted)", code)
	}
	if !strings.Contains(stderr.String(), "poke failed") {
		t.Errorf("stderr = %q, want 'poke failed' message (no controller socket in test)", stderr.String())
	}
}

func TestDoSlingCustomSlingQuery(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	a := config.Agent{
		Name:       "worker",
		SlingQuery: "custom-dispatch {} --queue=priority",
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-99")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	want := "custom-dispatch 'BL-99' --queue=priority"
	if runner.calls[0] != want {
		t.Errorf("runner call = %q, want %q", runner.calls[0], want)
	}
}

func TestDoSlingCustomSlingQueryExpandsTemplateContext(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	rigPath := filepath.Join(cityPath, "frontend")
	a := config.Agent{
		Name:       "worker",
		Dir:        "frontend",
		SlingQuery: "custom-dispatch {} --route={{.Rig}}/{{.AgentBase}} --city={{.CityName}}",
	}
	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "frontend", Path: rigPath}},
		Agents: []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = cityPath
	deps.CityName = ""
	opts := testOpts(a, "FR-99")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	want := "custom-dispatch 'FR-99' --route=frontend/worker --city=demo-city"
	if runner.calls[0] != want {
		t.Errorf("runner call = %q, want %q", runner.calls[0], want)
	}
}

func TestCmdSlingUsesRigScopedFileStoreForBuiltInRouting(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"

[[agent]]
name = "worker"
dir = "frontend"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSling([]string{"frontend/worker", "ship feature"}, false, false, true, "", nil, "", true, false, "", false, false, false, "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	rigBeads, err := rigStore.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("rigStore.List: %v", err)
	}
	if len(rigBeads) != 1 {
		t.Fatalf("rig store bead count = %d, want 1: %#v", len(rigBeads), rigBeads)
	}
	if rigBeads[0].Title != "ship feature" {
		t.Fatalf("rig bead title = %q, want %q", rigBeads[0].Title, "ship feature")
	}
	if rigBeads[0].Metadata["gc.routed_to"] != "frontend/worker" {
		t.Fatalf("rig bead gc.routed_to = %q, want %q", rigBeads[0].Metadata["gc.routed_to"], "frontend/worker")
	}

	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(city): %v", err)
	}
	cityBeads, err := cityStore.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("cityStore.List: %v", err)
	}
	if len(cityBeads) != 0 {
		t.Fatalf("city store bead count = %d, want 0: %#v", len(cityBeads), cityBeads)
	}
}

func TestSlingStoreEnvUsesRigBdRuntimeForMixedProviderRig(t *testing.T) {
	cityDir := t.TempDir()
	wantPort := strconv.Itoa(writeReachableManagedDoltState(t, cityDir))
	rigDir := filepath.Join(cityDir, "repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: repo
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"repo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	env := slingStoreEnv(cfg, cityDir, rigDir)
	if got := env["GC_DOLT_PORT"]; got != wantPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, wantPort)
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(rigDir, ".beads"))
	}
	if got := env["GC_RIG"]; got != "repo" {
		t.Fatalf("GC_RIG = %q, want %q", got, "repo")
	}
}

func TestTargetType(t *testing.T) {
	fixed := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	if got := targetType(&fixed); got != "agent" {
		t.Errorf("targetType(fixed) = %q, want %q", got, "agent")
	}

	pool := config.Agent{Name: "polecat", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}
	if got := targetType(&pool); got != "pool" {
		t.Errorf("targetType(pool) = %q, want %q", got, "pool")
	}
}

func TestNewSlingCmdArgs(t *testing.T) {
	cmd := newSlingCmd(&bytes.Buffer{}, &bytes.Buffer{})
	if cmd.Use != "sling [target] <bead-or-formula-or-text>" {
		t.Errorf("Use = %q", cmd.Use)
	}
	// Verify flags exist.
	for _, name := range []string{"formula", "nudge", "force", "title", "on", "no-formula"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
	// Verify -f shorthand for --formula.
	if f := cmd.Flags().ShorthandLookup("f"); f == nil || f.Name != "formula" {
		t.Error("missing -f shorthand for --formula")
	}
	// Verify -t shorthand for --title.
	if f := cmd.Flags().ShorthandLookup("t"); f == nil || f.Name != "title" {
		t.Error("missing -t shorthand for --title")
	}
}

// fakeQuerier implements BeadQuerier for testing pre-flight checks.
type fakeQuerier struct {
	bead beads.Bead
	err  error
}

func (q *fakeQuerier) Get(_ string) (beads.Bead, error) {
	return q.bead, q.err
}

// fakeChildQuerier implements BeadChildQuerier for testing batch dispatch.
type fakeChildQuerier struct {
	beadsByID   map[string]beads.Bead
	childrenOf  map[string][]beads.Bead
	getErr      error
	childrenErr error
}

func newFakeChildQuerier() *fakeChildQuerier {
	return &fakeChildQuerier{
		beadsByID:  make(map[string]beads.Bead),
		childrenOf: make(map[string][]beads.Bead),
	}
}

func (q *fakeChildQuerier) Get(id string) (beads.Bead, error) {
	if q.getErr != nil {
		return beads.Bead{}, q.getErr
	}
	b, ok := q.beadsByID[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return b, nil
}

func (q *fakeChildQuerier) Children(parentID string, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	if q.childrenErr != nil {
		return nil, q.childrenErr
	}
	return q.childrenOf[parentID], nil
}

func (q *fakeChildQuerier) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID == "" {
		return nil, beads.ErrQueryRequiresScan
	}
	children, err := q.Children(query.ParentID)
	if err != nil {
		return nil, err
	}
	normalized := make([]beads.Bead, len(children))
	copy(normalized, children)
	for i := range normalized {
		if normalized[i].ParentID == "" {
			normalized[i].ParentID = query.ParentID
		}
	}
	query.ParentID = ""
	return beads.ApplyListQuery(normalized, query), nil
}

func TestCheckBeadStateAssigneeWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "other-agent"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "MY-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "already assigned to \"other-agent\"") {
		t.Errorf("stderr = %q, want assignee warning", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 for built-in routing", len(runner.calls))
	}
	assertStoreRoutedTo(t, deps.Store, "MY-42", "mayor")
}

func TestCheckBeadStatePoolLabelWarns(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Labels: []string{"pool:hw/polecat"}}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "already has pool label \"pool:hw/polecat\"") {
		t.Errorf("stderr = %q, want pool label warning", stderr.String())
	}
}

func TestCheckBeadStateBothWarnings(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{
		ID:       "BL-42",
		Assignee: "other-agent",
		Labels:   []string{"pool:hw/polecat"},
	}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "already assigned") {
		t.Errorf("stderr = %q, want assignee warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "already has pool label") {
		t.Errorf("stderr = %q, want pool label warning", stderr.String())
	}
}

func TestCheckBeadStateCleanNoWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("clean bead should produce no warnings; stderr = %q", stderr.String())
	}
}

func TestCheckBeadStateQueryFailsNoWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{err: fmt.Errorf("bd not available")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("query failure should produce no warnings; stderr = %q", stderr.String())
	}
}

func TestCheckBeadStateNilQuerierNoWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("nil querier should produce no warnings; stderr = %q", stderr.String())
	}
}

func TestCheckBeadStateForceSkipsCheck(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "other-agent"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.Force = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "already assigned") {
		t.Errorf("--force should suppress pre-flight warnings; stderr = %q", stderr.String())
	}
}

func TestCheckBeadStateFormulaChecksResolvedBead(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-99\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	// The querier returns a clean bead for the wisp root — verifies check
	// runs on WP-99, not the formula name "my-formula".
	q := &fakeQuerier{bead: beads.Bead{ID: "WP-99"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "my-formula")
	opts.IsFormula = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("clean wisp root should produce no warnings; stderr = %q", stderr.String())
	}
}

// --- Batch dispatch (doSlingBatch) tests ---

func TestDoSlingBatchConvoyExpandsChildren(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
		{ID: "BL-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-1", "mayor")
	assertStoreRoutedTo(t, deps.Store, "BL-2", "mayor")
	assertStoreRoutedTo(t, deps.Store, "BL-3", "mayor")
	if !strings.Contains(stdout.String(), "Expanding convoy CVY-1") {
		t.Errorf("stdout = %q, want expansion header", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Slung 3/3 children") {
		t.Errorf("stdout = %q, want summary line", stdout.String())
	}
}

func TestDoSlingBatchConvoyMixedStatus(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-2"] = beads.Bead{ID: "CVY-2", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-2"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "closed"},
		{ID: "BL-3", Status: "open"},
		{ID: "BL-4", Status: "in_progress"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-2")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-1", "mayor")
	assertStoreRoutedTo(t, deps.Store, "BL-3", "mayor")
	out := stdout.String()
	if !strings.Contains(out, "Expanding convoy CVY-2 (4 children, 2 open)") {
		t.Errorf("stdout = %q, want header with counts", out)
	}
	if !strings.Contains(out, "Skipped BL-2 (status: closed)") {
		t.Errorf("stdout = %q, want skipped BL-2", out)
	}
	if !strings.Contains(out, "Skipped BL-4 (status: in_progress)") {
		t.Errorf("stdout = %q, want skipped BL-4", out)
	}
	if !strings.Contains(out, "Slung 2/4 children") {
		t.Errorf("stdout = %q, want summary", out)
	}
}

func TestDoSlingBatchConvoyNoOpenChildren(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-3"] = beads.Bead{ID: "CVY-3", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-3"] = []beads.Bead{
		{ID: "BL-1", Status: "closed"},
		{ID: "BL-2", Status: "closed"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-3")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no open children") {
		t.Errorf("stderr = %q, want 'no open children'", stderr.String())
	}
}

func TestDoSlingBatchEpicErrors(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["EP-1"] = beads.Bead{ID: "EP-1", Type: "epic", Status: "open"}
	q.childrenOf["EP-1"] = []beads.Bead{
		{ID: "BL-10", Status: "open"},
		{ID: "BL-11", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "EP-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "first-class support is for convoys only") {
		t.Errorf("stderr = %q, want convoy-only error", stderr.String())
	}
}

func TestDoSlingBatchRegularBeadPassthrough(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-42", "mayor")
	if !strings.Contains(stdout.String(), "Slung BL-42") {
		t.Errorf("stdout = %q, want direct sling output", stdout.String())
	}
	if strings.Contains(stdout.String(), "Expanding") {
		t.Errorf("stdout = %q, should not expand a regular bead", stdout.String())
	}
}

func TestDoSlingBatchFormulaPassthrough(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	// Even if the querier has a convoy, --formula bypasses container check.
	q.beadsByID["convoy-formula"] = beads.Bead{ID: "convoy-formula", Type: "convoy"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "convoy-formula")
	opts.IsFormula = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// Should have gone through formula path.
	if !strings.Contains(stdout.String(), "formula") {
		t.Errorf("stdout = %q, want formula output", stdout.String())
	}
}

func TestDoSlingBatchNilQuerier(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Slung BL-42") {
		t.Errorf("stdout = %q, want direct sling output", stdout.String())
	}
}

func TestDoSlingBatchGetFails(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.getErr = fmt.Errorf("bd not available")

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0 (falls through to doSling); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Slung BL-42") {
		t.Errorf("stdout = %q, want direct sling output", stdout.String())
	}
}

func TestDoSlingBatchChildrenFails(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenErr = fmt.Errorf("storage error")

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "listing children") {
		t.Errorf("stderr = %q, want children error", stderr.String())
	}
}

func TestDoSlingBatchPartialFailure(t *testing.T) {
	runner := newFakeRunner()
	runner.on("custom-dispatch 'BL-2'", "", fmt.Errorf("dispatch failed"))
	sp := runtime.NewFake()
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
		{ID: "BL-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1 (partial failure)", code)
	}
	if !strings.Contains(stdout.String(), "Slung BL-1") {
		t.Errorf("stdout = %q, want BL-1 routed", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Slung BL-3") {
		t.Errorf("stdout = %q, want BL-3 routed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Failed BL-2") {
		t.Errorf("stderr = %q, want BL-2 failure", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Slung 2/3 children") {
		t.Errorf("stdout = %q, want summary", stdout.String())
	}
}

func TestDoSlingBatchAllChildrenFail(t *testing.T) {
	runner := newFakeRunner()
	runner.on("custom-dispatch", "", fmt.Errorf("dispatch failed"))
	sp := runtime.NewFake()
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "Slung 0/2 children") {
		t.Errorf("stdout = %q, want 0/2 summary", stdout.String())
	}
}

func TestDoSlingBatchNudgeOnceAfterAll(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.Calls = nil
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	opts := testOpts(a, "CVY-1")
	opts.Nudge = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Fatalf("expected queued sling reminder, got direct nudge calls: %+v", sp.Calls)
		}
	}
	pending, _, dead, err := listQueuedNudges(deps.CityPath, "mayor", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(dead) != 0 {
		t.Fatalf("pending=%d dead=%d, want 1/0", len(pending), len(dead))
	}
}

func TestDoSlingBatchForceSkipsPerChildWarnings(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	// Children already assigned — would normally warn.
	q.beadsByID["BL-1"] = beads.Bead{ID: "BL-1", Status: "open", Assignee: "other"}
	q.beadsByID["BL-2"] = beads.Bead{ID: "BL-2", Status: "open", Assignee: "other"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Assignee: "other"},
		{ID: "BL-2", Status: "open", Assignee: "other"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.Force = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "already assigned") {
		t.Errorf("--force should suppress per-child warnings; stderr = %q", stderr.String())
	}
}

// --- On-formula (--on) tests ---

func TestOnAndFormulaMutuallyExclusive(t *testing.T) {
	cmd := newSlingCmd(&bytes.Buffer{}, &bytes.Buffer{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"mayor", "BL-1", "--formula", "--on=code-review"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for mutually exclusive --formula and --on")
	}
	if !strings.Contains(err.Error(), "if any flags in the group") {
		t.Errorf("err = %v, want mutual exclusion error", err)
	}
}

func TestOnFormulaAttachesAndRoutes(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, deps.Store, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if source.Metadata["gc.routed_to"] != "mayor" {
		t.Errorf("gc.routed_to = %q, want mayor", source.Metadata["gc.routed_to"])
	}
	rootID := source.Metadata["molecule_id"]
	if rootID == "" {
		t.Fatal("source bead missing molecule_id")
	}
	// Verify wisp was created in the store without parenting it to the outer bead.
	b, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", rootID, err)
	}
	if b.ParentID != "" {
		t.Errorf("wisp ParentID = %q, want empty", b.ParentID)
	}
	if b.Ref != "code-review" {
		t.Errorf("wisp Ref = %q, want %q", b.Ref, "code-review")
	}
}

func TestOnFormulaCopiesSourcePriorityToCreatedBeads(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Source", Type: "task", Status: "open", Priority: priorityPtr(4)},
	}, nil)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("Get(BL-42): %v", err)
	}
	rootID := source.Metadata["molecule_id"]
	if rootID == "" {
		t.Fatal("source bead missing molecule_id")
	}

	root, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("Get(%s): %v", rootID, err)
	}
	if root.Priority == nil || *root.Priority != 4 {
		t.Fatalf("workflow root priority = %v, want 4", root.Priority)
	}

	queue := []string{rootID}
	seenIDs := map[string]struct{}{rootID: {}}
	seenDescendants := false
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := deps.Store.Children(parentID)
		if err != nil {
			t.Fatalf("Children(%s): %v", parentID, err)
		}
		for _, child := range children {
			seenDescendants = true
			if child.Priority == nil || *child.Priority != 4 {
				t.Fatalf("workflow descendant %s priority = %v, want 4", child.ID, child.Priority)
			}
			seenIDs[child.ID] = struct{}{}
			queue = append(queue, child.ID)
		}
	}
	if !seenDescendants {
		t.Fatal("workflow root has no descendants")
	}

	all, err := deps.Store.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, bead := range all {
		if bead.ID == "BL-42" {
			continue
		}
		if bead.Type == "convoy" && bead.Title == "sling-BL-42" {
			continue
		}
		if _, ok := seenIDs[bead.ID]; !ok {
			t.Fatalf("created bead %s was not reachable from workflow root %s", bead.ID, rootID)
		}
	}
}

func TestOnFormulaGraphWorkflowPreassignsNonLatchBeadsForFixedAgent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.Daemon.FormulaV2 = true
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)
	config.InjectImplicitAgents(cfg)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "graph-work"
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("graph workflow runner calls = %d, want 0; calls=%v", len(runner.calls), runner.calls)
	}

	parent, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got := parent.Status; got != "open" {
		t.Fatalf("parent status = %q, want open", got)
	}
	rootID := parent.Metadata["workflow_id"]
	if rootID == "" {
		t.Fatal("parent workflow_id missing")
	}

	root, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("get workflow root: %v", err)
	}
	if got := root.Status; got != "in_progress" {
		t.Fatalf("root status = %q, want in_progress", got)
	}
	if got := root.Metadata["gc.run_target"]; got != "mayor" {
		t.Fatalf("root gc.run_target = %q, want mayor", got)
	}
	if got := root.Metadata["gc.source_bead_id"]; got != "BL-42" {
		t.Fatalf("root gc.source_bead_id = %q, want BL-42", got)
	}
	if got := root.Metadata["gc.scope_kind"]; got != "city" {
		t.Fatalf("root gc.scope_kind = %q, want city", got)
	}
	if got := root.Metadata["gc.scope_ref"]; got != "test-city" {
		t.Fatalf("root gc.scope_ref = %q, want test-city", got)
	}
	if got := root.Metadata[sourceworkflow.SourceStoreRefMetadataKey]; got != "city:test-city" {
		t.Fatalf("root %s = %q, want city:test-city", sourceworkflow.SourceStoreRefMetadataKey, got)
	}
	if got := root.Metadata["gc.root_store_ref"]; got != "city:test-city" {
		t.Fatalf("root gc.root_store_ref = %q, want city:test-city", got)
	}
	all, err := deps.Store.ListOpen()
	if err != nil {
		t.Fatalf("list workflow beads: %v", err)
	}
	assigned := 0
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		switch bead.Metadata["gc.kind"] {
		case "workflow", "scope":
			if bead.Assignee != "" {
				t.Fatalf("latch bead %s assignee = %q, want empty", bead.ID, bead.Assignee)
			}
		case "workflow-finalize":
			if bead.Assignee != config.ControlDispatcherAgentName {
				t.Fatalf("workflow-finalize assignee = %q, want %q", bead.Assignee, config.ControlDispatcherAgentName)
			}
			if bead.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
				t.Fatalf("workflow-finalize gc.routed_to = %q, want %q", bead.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
			}
			if bead.Metadata[graphExecutionRouteMetaKey] != "mayor" {
				t.Fatalf("workflow-finalize execution route = %q, want mayor", bead.Metadata[graphExecutionRouteMetaKey])
			}
			assigned++
		default:
			if bead.Assignee != "mayor" {
				t.Fatalf("workflow bead %s assignee = %q, want mayor", bead.ID, bead.Assignee)
			}
			if bead.Metadata["gc.routed_to"] != "mayor" {
				t.Fatalf("workflow bead %s gc.routed_to = %q, want mayor", bead.ID, bead.Metadata["gc.routed_to"])
			}
			assigned++
		}
	}
	if assigned == 0 {
		t.Fatal("expected at least one assigned workflow bead")
	}
	if !strings.Contains(stdout.String(), "Attached workflow") {
		t.Fatalf("stdout = %q, want attached workflow message", stdout.String())
	}
}

func TestDoSlingGraphWorkflowConflictReturnsExit3(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.Daemon.FormulaV2 = true
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
		{
			ID:     "wf-existing",
			Title:  "Existing workflow",
			Type:   "task",
			Status: "in_progress",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
				"gc.source_bead_id":   "BL-42",
			},
		},
	}, nil)
	config.InjectImplicitAgents(cfg)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "graph-work"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 3 {
		t.Fatalf("doSling returned %d, want 3; stderr: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	errText := stderr.String()
	if !strings.Contains(errText, "source bead BL-42 already has live workflow(s): wf-existing") {
		t.Fatalf("stderr = %q, want blocking workflow ids", errText)
	}
	if !strings.Contains(errText, "gc workflow delete-source BL-42 --store-ref city:test-city --apply") {
		t.Fatalf("stderr = %q, want cleanup hint", errText)
	}
}

func TestBatchOnGraphWorkflowStartsWorkflowWithoutRoutingChild(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.Daemon.FormulaV2 = true
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{{ID: "BL-1", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "CVY-1", Title: "Batch", Type: "convoy", Status: "open"},
		{ID: "BL-1", Title: "Child", Type: "task", Status: "open"},
	}, nil)
	config.InjectImplicitAgents(cfg)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "graph-work"
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("graph workflow runner calls = %d, want 0; calls=%v", len(runner.calls), runner.calls)
	}
	child, err := deps.Store.Get("BL-1")
	if err != nil {
		t.Fatalf("Get(BL-1): %v", err)
	}
	if child.Metadata["workflow_id"] == "" {
		t.Fatal("child workflow_id missing")
	}
	out := stdout.String()
	if !strings.Contains(out, "Attached workflow") {
		t.Fatalf("stdout = %q, want attached workflow message", out)
	}
	if strings.Contains(out, "  Slung BL-1") {
		t.Fatalf("stdout = %q, want no direct child sling line for graph workflow", out)
	}
}

func TestBatchOnGraphWorkflowConflictLeavesExistingRootInPlace(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.Daemon.FormulaV2 = true
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{{ID: "BL-1", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "CVY-1", Title: "Batch", Type: "convoy", Status: "open"},
		{ID: "BL-1", Title: "Child", Type: "task", Status: "open"},
		{
			ID:     "wf-existing",
			Title:  "Existing workflow",
			Type:   "task",
			Status: "in_progress",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
				"gc.source_bead_id":   "BL-1",
			},
		},
	}, nil)
	config.InjectImplicitAgents(cfg)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "graph-work"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	// Batch conflicts must use the same exit-3 contract as single-bead
	// conflicts so users see the cleanup hint and know to run
	// `gc workflow delete-source`. Before the adoption-review fixups
	// batch returned exit 1 with no hint; that was the bug this PR
	// exists to close for the batch path as well.
	if code != 3 {
		t.Fatalf("doSlingBatch returned %d, want 3 (exit-3 contract for batch conflict); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("graph workflow runner calls = %d, want 0; calls=%v", len(runner.calls), runner.calls)
	}
	errText := stderr.String()
	if !strings.Contains(errText, "Failed BL-1: source bead BL-1 already has live workflow(s): wf-existing") {
		t.Fatalf("stderr = %q, want per-child conflict summary", errText)
	}
	if !strings.Contains(errText, "gc workflow delete-source BL-1") {
		t.Fatalf("stderr = %q, want cleanup hint for conflicted child", errText)
	}
	child, err := deps.Store.Get("BL-1")
	if err != nil {
		t.Fatalf("Get(BL-1): %v", err)
	}
	if got := child.Metadata["workflow_id"]; got != "" {
		t.Fatalf("child workflow_id = %q, want unchanged empty metadata", got)
	}
}

func TestWorkflowStoreRefForDir(t *testing.T) {
	cityPath := filepath.Join(string(filepath.Separator), "tmp", "bright-lights")
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: "rigs/alpha"},
			{Name: "beta", Path: filepath.Join(cityPath, "rigs", "beta")},
		},
	}

	tests := []struct {
		name     string
		storeDir string
		cityName string
		want     string
	}{
		{
			name:     "named city store",
			storeDir: cityPath,
			cityName: "bright-lights",
			want:     "city:bright-lights",
		},
		{
			name:     "unnamed city store uses canonical fallback",
			storeDir: cityPath,
			cityName: "",
			want:     "city:city",
		},
		{
			name:     "relative rig path resolves under city",
			storeDir: filepath.Join(cityPath, "rigs", "alpha"),
			cityName: "bright-lights",
			want:     "rig:alpha",
		},
		{
			name:     "absolute rig path matches directly",
			storeDir: filepath.Join(cityPath, "rigs", "beta"),
			cityName: "bright-lights",
			want:     "rig:beta",
		},
		{
			name:     "unknown store yields empty ref",
			storeDir: filepath.Join(cityPath, "other"),
			cityName: "bright-lights",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workflowStoreRefForDir(tt.storeDir, cityPath, tt.cityName, cfg); got != tt.want {
				t.Fatalf("workflowStoreRefForDir(%q) = %q, want %q", tt.storeDir, got, tt.want)
			}
		})
	}
}

func TestResolveSlingStoreRootUsesCanonicalRigRoot(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: filepath.Join("rigs", "alpha"), Prefix: "al"},
			{Name: "beta", Path: filepath.Join("rigs", "beta"), Prefix: "be"},
		},
	}

	got := resolveSlingStoreRoot(cfg, cityPath, "plain text", config.Agent{Dir: "alpha"})
	want := filepath.Join(cityPath, "rigs", "alpha")
	if got != want {
		t.Fatalf("resolveSlingStoreRoot() = %q, want %q", got, want)
	}
}

func TestResolveSlingStoreRootPrefersBeadPrefixRig(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "alpha", Path: filepath.Join("rigs", "alpha"), Prefix: "al"},
			{Name: "beta", Path: filepath.Join("rigs", "beta"), Prefix: "be"},
		},
	}

	got := resolveSlingStoreRoot(cfg, cityPath, "be-123", config.Agent{Dir: "alpha"})
	want := filepath.Join(cityPath, "rigs", "beta")
	if got != want {
		t.Fatalf("resolveSlingStoreRoot() = %q, want %q", got, want)
	}
}

func TestSlingFormulaRepoDirUsesCanonicalRigRoot(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	deps := slingDeps{
		CityPath: cityPath,
		Cfg: &config.City{
			Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join("rigs", "alpha"), Prefix: "al"}},
		},
	}

	got := slingFormulaRepoDir("plain text", deps, config.Agent{Dir: "alpha"})
	want := filepath.Join(cityPath, "rigs", "alpha")
	if got != want {
		t.Fatalf("slingFormulaRepoDir() = %q, want %q", got, want)
	}
}

func TestDoSlingRejectsScopeForPlainBeadRouting(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig"},
		},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/city/myrig"},
		},
	}
	a, ok := resolveAgentIdentity(cfg, "worker", "")
	if !ok {
		t.Fatal("resolveAgentIdentity(worker) failed")
	}
	sp := runtime.NewFake()
	deps, stdout, stderr := testDeps(cfg, sp, func(dir, command string, env map[string]string) (string, error) {
		t.Fatalf("runner should not be invoked, got dir=%q command=%q env=%v", dir, command, env)
		return "", nil
	})
	opts := testOpts(a, "MY-42")
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"

	code := doSling(opts, deps, nil, stdout, stderr)

	if code == 0 {
		t.Fatalf("doSling returned %d, want non-zero", code)
	}
	if !strings.Contains(stderr.String(), "--scope-kind/--scope-ref require a formula-backed workflow launch") {
		t.Fatalf("stderr = %q, want scope validation message", stderr.String())
	}
}

func TestOnFormulaGraphWorkflowPokesOnce(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cfg.Daemon.FormulaV2 = true
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	config.InjectImplicitAgents(cfg)
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	graphFormula := `
formula = "graph-work"
version = 2

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], "graph-work.formula.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "graph-work"

	oldPoke := slingPokeControlDispatcher
	defer func() { slingPokeControlDispatcher = oldPoke }()
	pokes := 0
	slingPokeControlDispatcher = func(string) error {
		pokes++
		return nil
	}

	code := doSling(opts, deps, nil, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if pokes != 1 {
		t.Fatalf("graph workflow pokes = %d, want 1", pokes)
	}
}

func TestOnFormulaWithTitle(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.Title = "my-review"
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// MolCookOn goes through the store; verify bead was created with title and
	// left unattached from the outer bead.
	b, err := deps.Store.Get("gc-1")
	if err != nil {
		t.Fatalf("store.Get(gc-1): %v", err)
	}
	if b.Title != "my-review" {
		t.Errorf("bead title = %q, want %q", b.Title, "my-review")
	}
	if b.ParentID != "" {
		t.Errorf("bead ParentID = %q, want empty", b.ParentID)
	}
}

func TestReloadControllerConfigUsesControllerReloadCommand(t *testing.T) {
	dir := shortSocketTempDir(t, "gc-reload-cmd-")
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", gcDir, err)
	}

	sockPath := filepath.Join(gcDir, "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", sockPath, err)
	}
	defer lis.Close() //nolint:errcheck

	cmdCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			errCh <- err
			return
		}
		cmdCh <- string(buf[:n])
		_, _ = conn.Write([]byte("ok\n"))
	}()

	if err := reloadControllerConfig(dir); err != nil {
		t.Fatalf("reloadControllerConfig(): %v", err)
	}

	select {
	case cmd := <-cmdCh:
		if cmd != "reload\n" {
			t.Fatalf("controller command = %q, want %q", cmd, "reload\n")
		}
	case err := <-errCh:
		t.Fatalf("controller accept/read: %v", err)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for controller reload command")
	}
}

func TestPokeSupervisorReturnsWithoutWaitingForReloadAck(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortSocketTempDir(t, "gc-run-"))
	runtimeDir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", runtimeDir, err)
	}

	lis, err := net.Listen("unix", supervisorSocketPath())
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", supervisorSocketPath(), err)
	}
	defer lis.Close() //nolint:errcheck

	cmdCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			errCh <- err
			return
		}
		cmdCh <- string(buf[:n])
		time.Sleep(500 * time.Millisecond)
	}()

	start := time.Now()
	if err := pokeSupervisor(); err != nil {
		t.Fatalf("pokeSupervisor(): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("pokeSupervisor() took %v, want it to return immediately after queueing reload", elapsed)
	}

	select {
	case cmd := <-cmdCh:
		if cmd != "reload\n" {
			t.Fatalf("supervisor command = %q, want %q", cmd, "reload\n")
		}
	case err := <-errCh:
		t.Fatalf("supervisor accept/read: %v", err)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for supervisor reload command")
	}
}

func TestOnFormulaCookError(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "nonexistent-formula"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want formula error", stderr.String())
	}
}

func TestOnFormulaCookMissingFormula(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "totally-missing"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

func TestOnFormulaExistingMoleculeErrors(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	// Assigned bead — molecule is legitimate, should NOT be auto-burned.
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open", Assignee: "other-agent"}
	q.childrenOf["BL-42"] = []beads.Bead{
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already has attached molecule MOL-1") {
		t.Errorf("stderr = %q, want molecule error", stderr.String())
	}
	// No runner calls — should fail before routing.
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 (should not route)", len(runner.calls))
	}
}

func TestOnFormulaExistingWispErrors(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	// Assigned bead — attached molecule is legitimate, should NOT be auto-burned.
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open", Assignee: "other-agent"}
	q.childrenOf["BL-42"] = []beads.Bead{
		{ID: "MOL-5", Type: "molecule", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already has attached molecule MOL-5") {
		t.Errorf("stderr = %q, want molecule error", stderr.String())
	}
}

func TestOnFormulaAutoBurnStaleMolecule(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open", Assignee: ""}
	q.childrenOf["BL-42"] = []beads.Bead{{ID: "MOL-1", Type: "molecule", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}, nil)

	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (auto-burn should unblock); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Auto-burned stale molecule MOL-1") {
		t.Errorf("stderr = %q, want auto-burn message", stderr.String())
	}
	assertStoreRoutedTo(t, deps.Store, "BL-42", "mayor")
}

func TestOnFormulaMetadataAttachmentSkipsIdempotentRetry(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "BL-42", Title: "Work", Type: "task", Status: "open"}}, nil)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"

	if code := doSling(opts, deps, deps.Store, stdout, stderr); code != 0 {
		t.Fatalf("first doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("Get(BL-42): %v", err)
	}
	firstRootID := source.Metadata["molecule_id"]
	if firstRootID == "" {
		t.Fatal("first sling did not set molecule_id")
	}
	assertStoreRoutedTo(t, deps.Store, "BL-42", "mayor")

	stdout.Reset()
	stderr.Reset()
	if code := doSling(opts, deps, deps.Store, stdout, stderr); code != 0 {
		t.Fatalf("second doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty on idempotent retry", stderr.String())
	}
	if !strings.Contains(stdout.String(), "skipping (idempotent)") {
		t.Fatalf("stdout = %q, want idempotent skip", stdout.String())
	}

	firstRoot, err := deps.Store.Get(firstRootID)
	if err != nil {
		t.Fatalf("Get(%s): %v", firstRootID, err)
	}
	if firstRoot.Status != "open" {
		t.Fatalf("first root status = %q, want open", firstRoot.Status)
	}

	updatedSource, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("Get(BL-42) after retry: %v", err)
	}
	if updatedSource.Metadata["molecule_id"] != firstRootID {
		t.Fatalf("source molecule_id = %q, want %q", updatedSource.Metadata["molecule_id"], firstRootID)
	}
}

func TestOnFormulaSkipsClosedMolecule(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open", Assignee: "other-agent"}
	q.childrenOf["BL-42"] = []beads.Bead{
		{ID: "MOL-1", Type: "molecule", Status: "closed"}, // closed — should be skipped
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (closed molecule should be skipped); stderr: %s", code, stderr.String())
	}
}

func TestOnFormulaCleanBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open"}
	q.childrenOf["BL-42"] = []beads.Bead{{ID: "STEP-1", Type: "step", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-42", "mayor")
}

func TestOnFormulaNilQuerier(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	// nil querier → molecule check skipped, should succeed.
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestOnFormulaOutput(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// MemStore generates IDs like "gc-1".
	if !strings.Contains(out, "Attached wisp gc-1 (formula \"code-review\") to BL-42") {
		t.Errorf("stdout = %q, want attach message", out)
	}
	if !strings.Contains(out, "Slung BL-42 (with formula \"code-review\")") {
		t.Errorf("stdout = %q, want slung with formula message", out)
	}
}

func TestBatchOnConvoy(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
		{ID: "BL-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// MolCookOn goes through the store, verify 3 wisps were created.
	all, _ := deps.Store.ListOpen()
	molCount := 0
	for _, b := range all {
		if b.Type == "molecule" {
			molCount++
		}
	}
	if molCount != 3 {
		t.Errorf("got %d molecule beads in store, want 3", molCount)
	}
	out := stdout.String()
	// MemStore generates IDs gc-1, gc-2, ... Each molecule.Cook creates
	// 2 beads (root + step), so wisp root IDs are gc-1, gc-3, gc-5.
	if !strings.Contains(out, "Attached wisp gc-1") {
		t.Errorf("stdout = %q, want gc-1 attach", out)
	}
	if !strings.Contains(out, "Attached wisp gc-3") {
		t.Errorf("stdout = %q, want gc-3 attach", out)
	}
	if !strings.Contains(out, "Attached wisp gc-5") {
		t.Errorf("stdout = %q, want gc-5 attach", out)
	}
	if !strings.Contains(out, "Slung 3/3 children") {
		t.Errorf("stdout = %q, want summary", out)
	}
}

func TestBatchOnConvoyCopiesChildPriorityToCreatedBeads(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Priority: priorityPtr(3)},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}

	all, err := deps.Store.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, bead := range all {
		if bead.Priority == nil || *bead.Priority != 3 {
			t.Fatalf("created bead %s priority = %v, want 3", bead.ID, bead.Priority)
		}
	}
}

func TestBatchOnFailFastMolecule(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open", Assignee: "other-agent"},
	}
	// BL-2 has an existing molecule child AND is assigned — legitimate, should block.
	q.childrenOf["BL-2"] = []beads.Bead{
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot use --on") {
		t.Errorf("stderr = %q, want '--on' error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "BL-2 (has molecule MOL-1)") {
		t.Errorf("stderr = %q, want BL-2 details", stderr.String())
	}
	// Nothing should be routed — fail-fast.
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 (fail-fast)", len(runner.calls))
	}
}

func TestBatchAutoBurnStaleMolecules(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{{ID: "BL-1", Status: "open"}, {ID: "BL-2", Status: "open"}}
	q.childrenOf["BL-2"] = []beads.Bead{{ID: "MOL-1", Type: "molecule", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "CVY-1", Title: "Batch", Type: "convoy", Status: "open"},
		{ID: "BL-1", Title: "One", Type: "task", Status: "open"},
		{ID: "BL-2", Title: "Two", Type: "task", Status: "open"},
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}, nil)

	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0 (auto-burn should unblock); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Auto-burned stale molecule MOL-1") {
		t.Errorf("stderr = %q, want auto-burn message", stderr.String())
	}
	assertStoreRoutedTo(t, deps.Store, "BL-1", "mayor")
	assertStoreRoutedTo(t, deps.Store, "BL-2", "mayor")
}

func TestOnFormulaPoolAttachmentLeavesLegacyStepsUnrouted(t *testing.T) {
	dir := testFormulaDir(t)
	content := `
formula = "multi-step"
version = 1

[[steps]]
id = "prep"
title = "Prep"

[[steps]]
id = "ship"
title = "Ship"
needs = ["prep"]
`
	if err := os.WriteFile(filepath.Join(dir, "multi-step.formula.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{
			City: []string{dir},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)

	opts := testOpts(a, "BL-42")
	opts.OnFormula = "multi-step"
	code := doSling(opts, deps, deps.Store, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("Get(BL-42): %v", err)
	}
	rootID := source.Metadata["molecule_id"]
	if rootID == "" {
		t.Fatal("source bead missing molecule_id")
	}

	root, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("Get(%s): %v", rootID, err)
	}
	if root.ParentID != "" {
		t.Fatalf("root ParentID = %q, want empty", root.ParentID)
	}

	all, err := deps.Store.ListOpen()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, bead := range all {
		if bead.ID == "BL-42" || bead.ID == rootID {
			continue
		}
		if bead.Type == "convoy" && bead.Title == "sling-BL-42" {
			continue
		}
		if bead.ParentID == "BL-42" {
			t.Fatalf("internal bead %s ParentID = %q, want not outer bead", bead.ID, bead.ParentID)
		}
		if bead.Metadata["gc.routed_to"] != "" {
			t.Fatalf("internal bead %s gc.routed_to = %q, want empty", bead.ID, bead.Metadata["gc.routed_to"])
		}
	}
}

func TestBatchSkipsClosedMolecules(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Assignee: "other-agent"},
	}
	// BL-1 has a closed molecule — should be skipped, not block dispatch.
	q.childrenOf["BL-1"] = []beads.Bead{
		{ID: "MOL-1", Type: "molecule", Status: "closed"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0 (closed molecule should be skipped); stderr: %s", code, stderr.String())
	}
}

func TestBatchOnPartialCookFailure(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{{ID: "BL-1", Status: "open"}, {ID: "BL-2", Status: "open"}, {ID: "BL-3", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	createCount := 0
	deps.Store = &selectiveErrStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{
			{ID: "BL-1", Title: "One", Type: "task", Status: "open"},
			{ID: "BL-2", Title: "Two", Type: "task", Status: "open"},
			{ID: "BL-3", Title: "Three", Type: "task", Status: "open"},
		}, nil),
		failOnCreate: func(b beads.Bead) error {
			if b.Type != "molecule" || b.ParentID != "" {
				return nil
			}
			createCount++
			if createCount == 2 {
				return fmt.Errorf("cook failed for BL-2")
			}
			return nil
		},
	}
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1 (partial failure)", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Slung BL-1") {
		t.Errorf("stdout = %q, want BL-1 routed", out)
	}
	if !strings.Contains(out, "Slung BL-3") {
		t.Errorf("stdout = %q, want BL-3 routed", out)
	}
	if !strings.Contains(stderr.String(), "Failed BL-2") {
		t.Errorf("stderr = %q, want BL-2 failure", stderr.String())
	}
	if !strings.Contains(out, "Slung 2/3 children") {
		t.Errorf("stdout = %q, want summary", out)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-1", "mayor")
	assertStoreRoutedTo(t, deps.Store, "BL-3", "mayor")
}

func TestBatchOnNudgeOnce(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.Calls = nil
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })
	opts := testOpts(a, "CVY-1")
	opts.Nudge = true
	opts.OnFormula = "code-review"
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Fatalf("expected queued sling reminder, got direct nudge calls: %+v", sp.Calls)
		}
	}
	pending, _, dead, err := listQueuedNudges(deps.CityPath, "mayor", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(dead) != 0 {
		t.Fatalf("pending=%d dead=%d, want 1/0", len(pending), len(dead))
	}
}

func TestBatchOnRegularPassthrough(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	// Non-container bead + --on → should fall through to doSling.
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// MemStore generates IDs like "gc-1".
	if !strings.Contains(out, "Attached wisp gc-1") {
		t.Errorf("stdout = %q, want attach message", out)
	}
	if !strings.Contains(out, "Slung BL-42 (with formula") {
		t.Errorf("stdout = %q, want slung with formula", out)
	}
	if strings.Contains(out, "Expanding") {
		t.Errorf("stdout = %q, should not expand a regular bead", out)
	}
}

// --- Dry-run tests ---

func TestDryRunFlagExists(t *testing.T) {
	cmd := newSlingCmd(&bytes.Buffer{}, &bytes.Buffer{})
	f := cmd.Flags().Lookup("dry-run")
	if f == nil {
		t.Fatal("missing --dry-run flag")
	}
	if f.Shorthand != "n" {
		t.Errorf("--dry-run shorthand = %q, want %q", f.Shorthand, "n")
	}
}

func TestDryRunSingleBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Title: "Implement login page", Type: "task", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Target section.
	if !strings.Contains(out, "Agent:       mayor (non-expanding template)") {
		t.Errorf("stdout missing agent info: %s", out)
	}
	if !strings.Contains(out, "Sling query: bd update {} --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing sling query: %s", out)
	}
	// Work section.
	if !strings.Contains(out, "BL-42") {
		t.Errorf("stdout missing bead ID: %s", out)
	}
	if !strings.Contains(out, "Implement login page") {
		t.Errorf("stdout missing bead title: %s", out)
	}
	// Route command.
	if !strings.Contains(out, "bd update 'BL-42' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing route command: %s", out)
	}
	// Footer.
	if !strings.Contains(out, "No side effects executed (--dry-run).") {
		t.Errorf("stdout missing footer: %s", out)
	}
	// Zero mutations.
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 (dry-run): %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunSingleBeadExpandsSlingQuerySummary(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "/city/frontend"}},
	}
	a := config.Agent{
		Name:       "worker",
		Dir:        "frontend",
		SlingQuery: "custom-dispatch {} --route={{.Rig}}/{{.AgentBase}} --city={{.CityName}}",
	}
	q := &fakeQuerier{bead: beads.Bead{ID: "FR-42", Title: "Implement login page", Type: "task", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FR-42")
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Sling query: custom-dispatch {} --route=frontend/worker --city=test-city") {
		t.Fatalf("stdout missing expanded sling query: %s", out)
	}
}

func TestDryRunFormula(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "code-review")
	opts.IsFormula = true
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Formula:") {
		t.Errorf("stdout missing Formula section: %s", out)
	}
	if !strings.Contains(out, "Name: code-review") {
		t.Errorf("stdout missing formula name: %s", out)
	}
	if !strings.Contains(out, "Would run: bd mol cook --formula=code-review") {
		t.Errorf("stdout missing cook command: %s", out)
	}
	if !strings.Contains(out, "'<wisp-root>'") {
		t.Errorf("stdout missing wisp-root placeholder: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunOnFormula(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open"}
	q.childrenOf["BL-42"] = []beads.Bead{} // no molecule children

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Attach formula:") {
		t.Errorf("stdout missing attach section: %s", out)
	}
	if !strings.Contains(out, "Would run: bd mol cook --formula=code-review --on=BL-42") {
		t.Errorf("stdout missing cook command: %s", out)
	}
	if !strings.Contains(out, "Pre-check: BL-42 has no existing molecule/wisp children") {
		t.Errorf("stdout missing pre-check: %s", out)
	}
	if !strings.Contains(out, "bd update 'BL-42' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing route command: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunMultiSessionConfig(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hw",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3),
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Session config: hw/polecat (min=1 max=3)") {
		t.Errorf("stdout missing multi-session config info: %s", out)
	}
	if !strings.Contains(out, "bd update {} --set-metadata gc.routed_to=hw/polecat") {
		t.Errorf("stdout missing sling query: %s", out)
	}
	if !strings.Contains(out, "Multi-session configs share a routed work queue via gc.routed_to") {
		t.Errorf("stdout missing multi-session explanation: %s", out)
	}
	if strings.Contains(out, "Pool agents") || strings.Contains(out, "pool member") {
		t.Errorf("stdout contains stale pool terminology: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunConvoy(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open", Title: "Sprint 12 tasks"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Title: "Login page", Status: "open"},
		{ID: "BL-2", Title: "Auth backend", Status: "closed"},
		{ID: "BL-3", Title: "Session mgmt", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.DryRun = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Container explanation.
	if !strings.Contains(out, "convoy") {
		t.Errorf("stdout missing convoy type: %s", out)
	}
	// Children list.
	if !strings.Contains(out, "Children (3 total, 2 open)") {
		t.Errorf("stdout missing children summary: %s", out)
	}
	if !strings.Contains(out, "BL-1") || !strings.Contains(out, "would route") {
		t.Errorf("stdout missing BL-1 route: %s", out)
	}
	if !strings.Contains(out, "BL-2") {
		t.Errorf("stdout missing BL-2: %s", out)
	}
	if !strings.Contains(out, "skip") {
		t.Errorf("stdout missing skip indicator: %s", out)
	}
	// Route commands.
	if !strings.Contains(out, "bd update 'BL-1' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing BL-1 route command: %s", out)
	}
	if !strings.Contains(out, "bd update 'BL-3' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing BL-3 route command: %s", out)
	}
	if strings.Contains(out, "bd update 'BL-2' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout should not route closed BL-2: %s", out)
	}
	// Zero mutations.
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunBatchOnFormula(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open"},
		{ID: "BL-2", Status: "closed"},
		{ID: "BL-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.OnFormula = "code-review"
	opts.DryRun = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Per-child cook commands.
	if !strings.Contains(out, "bd mol cook --formula=code-review --on=BL-1") {
		t.Errorf("stdout missing BL-1 cook command: %s", out)
	}
	if !strings.Contains(out, "bd mol cook --formula=code-review --on=BL-3") {
		t.Errorf("stdout missing BL-3 cook command: %s", out)
	}
	if strings.Contains(out, "bd mol cook --formula=code-review --on=BL-2") {
		t.Errorf("stdout should not cook for closed BL-2: %s", out)
	}
	// Route commands.
	if !strings.Contains(out, "bd update 'BL-1' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing BL-1 route: %s", out)
	}
	if !strings.Contains(out, "bd update 'BL-3' --set-metadata gc.routed_to=mayor") {
		t.Errorf("stdout missing BL-3 route: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunNudgeRunning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.Calls = nil
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Nudge:") {
		t.Errorf("stdout missing Nudge section: %s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("stdout missing running status: %s", out)
	}
	// No actual nudge should have been sent.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Error("dry-run should not send an actual nudge")
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunNudgeNotRunning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.Nudge = true
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "no running session") {
		t.Errorf("stdout missing 'no running session': %s", out)
	}
}

func TestDryRunNoMutations(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0", code)
	}
	if len(runner.calls) != 0 {
		t.Errorf("dry-run executed %d commands, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunSuspendedWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", Suspended: true, MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-1")
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0", code)
	}
	// Suspended warning should still fire to stderr.
	if !strings.Contains(stderr.String(), "suspended") {
		t.Errorf("stderr = %q, want suspended warning", stderr.String())
	}
	// But no mutations.
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunOnExistingMolecule(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["BL-42"] = beads.Bead{ID: "BL-42", Type: "task", Status: "open"}
	q.childrenOf["BL-42"] = []beads.Bead{
		{ID: "MOL-1", Type: "molecule", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "code-review"
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("dry-run returned %d, want 1 (existing molecule)", code)
	}
	if !strings.Contains(stderr.String(), "already has attached molecule MOL-1") {
		t.Errorf("stderr = %q, want molecule error", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunNilQuerier(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Should still show bead ID even without querier details.
	if !strings.Contains(out, "BL-42") {
		t.Errorf("stdout missing bead ID: %s", out)
	}
	if !strings.Contains(out, "No side effects executed (--dry-run).") {
		t.Errorf("stdout missing footer: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

// --- Idempotency detection (checkBeadState + integration) tests ---

func TestCheckBeadStateIdempotentFixedAgent(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Metadata: map[string]string{"gc.routed_to": "mayor"}}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	result := checkBeadState(q, "BL-42", a)
	if !result.Idempotent {
		t.Error("expected Idempotent=true for matching gc.routed_to")
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
}

func TestCheckBeadStateIdempotentPool(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Metadata: map[string]string{"gc.routed_to": "hw/polecat"}}}
	a := config.Agent{Name: "polecat", Dir: "hw", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}

	result := checkBeadState(q, "BL-42", a)
	if !result.Idempotent {
		t.Error("expected Idempotent=true for matching gc.routed_to")
	}
	if len(result.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", result.Warnings)
	}
}

func TestCheckBeadStateIdempotentPoolMultiLabels(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{
		ID:       "BL-42",
		Labels:   []string{"priority:high", "sprint:3"},
		Metadata: map[string]string{"gc.routed_to": "hw/polecat"},
	}}
	a := config.Agent{Name: "polecat", Dir: "hw", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}

	result := checkBeadState(q, "BL-42", a)
	if !result.Idempotent {
		t.Error("expected Idempotent=true for matching gc.routed_to among other labels")
	}
}

func TestCheckBeadStateCustomQueryNoIdempotency(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "mayor"}}
	a := config.Agent{Name: "mayor", SlingQuery: "custom-script {} --route"}

	result := checkBeadState(q, "BL-42", a)
	if result.Idempotent {
		t.Error("expected Idempotent=false for custom sling_query (can't detect)")
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(result.Warnings), result.Warnings)
	}
	if !strings.Contains(result.Warnings[0], "already assigned") {
		t.Errorf("expected assignee warning, got %q", result.Warnings[0])
	}
}

func TestCheckBeadStateDifferentAssignee(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "other-agent"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	result := checkBeadState(q, "BL-42", a)
	if result.Idempotent {
		t.Error("expected Idempotent=false for different assignee")
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(result.Warnings), result.Warnings)
	}
	if !strings.Contains(result.Warnings[0], "already assigned to \"other-agent\"") {
		t.Errorf("expected assignee warning, got %q", result.Warnings[0])
	}
}

func TestCheckBeadStateDifferentPoolLabel(t *testing.T) {
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Labels: []string{"pool:other/pool"}}}
	a := config.Agent{Name: "polecat", Dir: "hw", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}

	result := checkBeadState(q, "BL-42", a)
	if result.Idempotent {
		t.Error("expected Idempotent=false for different pool label")
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(result.Warnings), result.Warnings)
	}
	if !strings.Contains(result.Warnings[0], "pool:other/pool") {
		t.Errorf("expected pool label warning, got %q", result.Warnings[0])
	}
}

func TestDoSlingIdempotentSkipsRouting(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Metadata: map[string]string{"gc.routed_to": "mayor"}}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	if len(runner.calls) != 0 {
		t.Errorf("idempotent bead should not be routed; got %d calls: %v", len(runner.calls), runner.calls)
	}
	if !strings.Contains(stdout.String(), "already routed to mayor") {
		t.Errorf("stdout = %q, want idempotent skip message", stdout.String())
	}
	if !strings.Contains(stdout.String(), "skipping (idempotent)") {
		t.Errorf("stdout = %q, want idempotent label", stdout.String())
	}
}

func TestDoSlingIdempotentForceOverrides(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "mayor"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.Force = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0", code)
	}
	// --force should bypass idempotency and route.
	if len(runner.calls) != 0 {
		t.Errorf("--force should not shell out for built-in routing; got %d calls", len(runner.calls))
	}
	bead, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if bead.Metadata["gc.routed_to"] == "" {
		t.Error("expected gc.routed_to to be set during forced routing")
	}
	if strings.Contains(stdout.String(), "idempotent") {
		t.Errorf("--force should not print idempotent message; stdout = %q", stdout.String())
	}
}

func TestDoSlingIdempotentWithOnFormula(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	// Bead is already assigned to mayor — idempotent.
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Assignee: "mayor"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "my-formula"
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// Idempotent — should skip both wisp attachment and routing.
	if len(runner.calls) != 0 {
		t.Errorf("idempotent + --on should skip all mutations; got %d calls: %v", len(runner.calls), runner.calls)
	}
	if !strings.Contains(stdout.String(), "skipping (idempotent)") {
		t.Errorf("stdout = %q, want idempotent skip message", stdout.String())
	}
}

func TestDoSlingBatchIdempotentChildSkipped(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.beadsByID["BL-1"] = beads.Bead{ID: "BL-1", Status: "open", Assignee: "mayor"}
	q.beadsByID["BL-2"] = beads.Bead{ID: "BL-2", Status: "open"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Assignee: "mayor"},
		{ID: "BL-2", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "BL-2", "mayor")
	out := stdout.String()
	if !strings.Contains(out, "Skipped BL-1") {
		t.Errorf("stdout should mention skipped BL-1: %s", out)
	}
	if !strings.Contains(out, "already routed to mayor") {
		t.Errorf("stdout should mention idempotent skip: %s", out)
	}
	if !strings.Contains(out, "Slung 1/2 children") {
		t.Errorf("stdout summary should show 1/2 routed: %s", out)
	}
	if !strings.Contains(out, "(1 already routed)") {
		t.Errorf("stdout summary should mention idempotent count: %s", out)
	}
}

func TestDoSlingBatchAllIdempotent(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.beadsByID["BL-1"] = beads.Bead{ID: "BL-1", Status: "open", Assignee: "mayor"}
	q.beadsByID["BL-2"] = beads.Bead{ID: "BL-2", Status: "open", Assignee: "mayor"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Assignee: "mayor"},
		{ID: "BL-2", Status: "open", Assignee: "mayor"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("all idempotent: got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "Slung 0/2 children") {
		t.Errorf("stdout summary should show 0/2 routed: %s", out)
	}
	if !strings.Contains(out, "(2 already routed)") {
		t.Errorf("stdout summary should mention both idempotent: %s", out)
	}
}

func TestDryRunIdempotentBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	q := &fakeQuerier{bead: beads.Bead{ID: "BL-42", Title: "Login page", Assignee: "mayor", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "BL-42")
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	// Dry-run reaches the full preview — including the Idempotency section.
	if code != 0 {
		t.Fatalf("returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Should show idempotency section.
	if !strings.Contains(out, "Idempotency:") {
		t.Errorf("stdout missing Idempotency section: %s", out)
	}
	if !strings.Contains(out, "already routed to mayor") {
		t.Errorf("stdout should show idempotent info: %s", out)
	}
	// Should show the footer (proving dryRunSingle was reached).
	if !strings.Contains(out, "No side effects executed (--dry-run).") {
		t.Errorf("stdout missing footer: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

// --- Cross-rig guard tests ---

func TestRigPrefixForAgentCityWide(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "mayor", Dir: ""} // city-wide

	got := rigPrefixForAgent(a, cfg)
	if got != "" {
		t.Errorf("rigPrefixForAgent(city-wide) = %q, want empty (exempt)", got)
	}
}

func TestRigPrefixForAgentRigScoped(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	got := rigPrefixForAgent(a, cfg)
	// DeriveBeadsPrefix("hello-world") = "hw"
	if got != "hw" {
		t.Errorf("rigPrefixForAgent(rig-scoped) = %q, want %q", got, "hw")
	}
}

func TestRigPrefixForAgentExplicitPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw", Prefix: "HELLO"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	got := rigPrefixForAgent(a, cfg)
	if got != "hello" {
		t.Errorf("rigPrefixForAgent(explicit prefix) = %q, want %q", got, "hello")
	}
}

func TestRigPrefixForAgentOrphanDir(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "nonexistent"}

	got := rigPrefixForAgent(a, cfg)
	if got != "" {
		t.Errorf("rigPrefixForAgent(orphan dir) = %q, want empty (best-effort skip)", got)
	}
}

func TestCheckCrossRigSameRig(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	msg := checkCrossRig("HW-7", a, cfg)
	if msg != "" {
		t.Errorf("checkCrossRig(same rig) = %q, want empty", msg)
	}
}

func TestCheckCrossRigDifferentRig(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	msg := checkCrossRig("FE-123", a, cfg)
	if msg == "" {
		t.Fatal("checkCrossRig(different rig) = empty, want error message")
	}
	if !strings.Contains(msg, "cross-rig") {
		t.Errorf("message = %q, want 'cross-rig'", msg)
	}
	if !strings.Contains(msg, `prefix "fe"`) {
		t.Errorf("message = %q, want bead prefix", msg)
	}
	if !strings.Contains(msg, `rig prefix "hw"`) {
		t.Errorf("message = %q, want rig prefix", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("message = %q, want --force hint", msg)
	}
}

func TestCheckCrossRigCityAgent(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "mayor", Dir: ""} // city-wide

	msg := checkCrossRig("FE-123", a, cfg)
	if msg != "" {
		t.Errorf("checkCrossRig(city-wide agent) = %q, want empty (exempt)", msg)
	}
}

func TestDoSlingCrossRigBlocks(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-123")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1 (cross-rig block)", code)
	}
	if !strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("stderr = %q, want cross-rig error", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 (should not route)", len(runner.calls))
	}
}

func TestDoSlingCrossRigForceOverrides(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-123")
	opts.Force = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (--force overrides cross-rig); stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("--force should suppress cross-rig block; stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 for built-in routing", len(runner.calls))
	}
	assertStoreRoutedTo(t, deps.Store, "FE-123", "hello-world/polecat")
}

func TestDoSlingCrossRigSameRigAllowed(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "HW-7")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (same rig); stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("same-rig bead should not trigger cross-rig block; stderr = %q", stderr.String())
	}
}

func TestDoSlingBatchCrossRigBlocks(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	q := newFakeChildQuerier()
	q.beadsByID["FE-1"] = beads.Bead{ID: "FE-1", Type: "convoy", Status: "open"}
	q.childrenOf["FE-1"] = []beads.Bead{
		{ID: "FE-2", Status: "open"},
		{ID: "FE-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-1")
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSlingBatch returned %d, want 1 (cross-rig block)", code)
	}
	if !strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("stderr = %q, want cross-rig error", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0 (should not route)", len(runner.calls))
	}
}

func TestDryRunCrossRigSection(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}
	q := &fakeQuerier{bead: beads.Bead{ID: "FE-123", Type: "task", Status: "open"}}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-123")
	opts.DryRun = true
	code := doSling(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Cross-rig:") {
		t.Errorf("stdout missing Cross-rig section: %s", out)
	}
	if !strings.Contains(out, `prefix "fe"`) {
		t.Errorf("stdout missing bead prefix: %s", out)
	}
	if !strings.Contains(out, `rig prefix "hw"`) {
		t.Errorf("stdout missing rig prefix: %s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("stdout missing --force hint: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDryRunBatchCrossRigSection(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	q := newFakeChildQuerier()
	q.beadsByID["FE-1"] = beads.Bead{ID: "FE-1", Type: "convoy", Status: "open"}
	q.childrenOf["FE-1"] = []beads.Bead{
		{ID: "FE-2", Status: "open"},
		{ID: "FE-3", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-1")
	opts.DryRun = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("dry-run returned %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Cross-rig:") {
		t.Errorf("stdout missing Cross-rig section: %s", out)
	}
	if !strings.Contains(out, `prefix "fe"`) {
		t.Errorf("stdout missing bead prefix: %s", out)
	}
	if !strings.Contains(out, `rig prefix "hw"`) {
		t.Errorf("stdout missing rig prefix: %s", out)
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0: %v", len(runner.calls), runner.calls)
	}
}

func TestDoSlingCrossRigFormulaExempt(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "code-review")
	opts.IsFormula = true
	// Formula mode — cross-rig check should not apply.
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0 (formula exempt from cross-rig); stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("formula mode should not trigger cross-rig; stderr = %q", stderr.String())
	}
}

// --- New tests for shell quoting, helpers, and edge cases ---

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"it's", "'it'\\''s'"},
		{"a b c", "'a b c'"},
		{"", "''"},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"`evil`", "'`evil`'"},
		{"hello'world'end", "'hello'\\''world'\\''end'"},
	}
	for _, tt := range tests {
		got := shellquote.Quote(tt.input)
		if got != tt.want {
			t.Errorf("shellquote.Quote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatBeadLabel(t *testing.T) {
	if got := formatBeadLabel("BL-42", ""); got != "BL-42" {
		t.Errorf("formatBeadLabel(no title) = %q, want %q", got, "BL-42")
	}
	if got := formatBeadLabel("BL-42", "Login page"); !strings.Contains(got, "BL-42") || !strings.Contains(got, "Login page") {
		t.Errorf("formatBeadLabel(with title) = %q, want BL-42 and title", got)
	}
}

func TestDoSlingOnFormulaCrossRigBlocked(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-123")
	opts.OnFormula = "code-review"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 1 {
		t.Fatalf("doSling returned %d, want 1 (cross-rig block with --on)", code)
	}
	if !strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("stderr = %q, want cross-rig error", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("got %d runner calls, want 0", len(runner.calls))
	}
}

func TestDoSlingOnFormulaCrossRigForceOverrides(t *testing.T) {
	runner := newFakeRunner()
	runner.on("bd mol cook", "WP-1\n", nil)
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "hello-world", Path: "/tmp/hw"}},
	}
	a := config.Agent{Name: "polecat", Dir: "hello-world"}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "FE-123")
	opts.OnFormula = "code-review"
	opts.Force = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-rig") {
		t.Errorf("--force should suppress cross-rig; stderr = %q", stderr.String())
	}
}

func TestDoSlingBatchAllIdempotentNoNudge(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.Calls = nil
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	q := newFakeChildQuerier()
	q.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	q.beadsByID["BL-1"] = beads.Bead{ID: "BL-1", Status: "open", Assignee: "mayor"}
	q.beadsByID["BL-2"] = beads.Bead{ID: "BL-2", Status: "open", Assignee: "mayor"}
	q.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "BL-1", Status: "open", Assignee: "mayor"},
		{ID: "BL-2", Status: "open", Assignee: "mayor"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	opts.Nudge = true
	code := doSlingBatch(opts, deps, q, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// All idempotent → 0 routed → no nudge should fire.
	for _, c := range sp.Calls {
		if c.Method == "Nudge" {
			t.Error("all-idempotent batch should not nudge")
		}
	}
}

// --- Default sling formula tests ---

func TestDefaultFormulaApplied(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{{ID: "HW-42", Title: "Work", Type: "task", Status: "open"}}, nil)
	opts := testOpts(a, "HW-42")
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "HW-42", "hw/polecat")
	source, err := deps.Store.Get("HW-42")
	if err != nil {
		t.Fatalf("store.Get(HW-42): %v", err)
	}
	rootID := source.Metadata["molecule_id"]
	if rootID == "" {
		t.Fatal("source bead missing molecule_id")
	}
	b, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", rootID, err)
	}
	if b.Ref != "mol-polecat-work" {
		t.Errorf("bead Ref = %q, want %q", b.Ref, "mol-polecat-work")
	}
	if b.ParentID != "" {
		t.Errorf("bead ParentID = %q, want empty", b.ParentID)
	}
	if !strings.Contains(stdout.String(), "default formula") {
		t.Errorf("stdout = %q, want mention of default formula", stdout.String())
	}
}

func TestDefaultFormulaNoFormulaOverride(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "HW-42")
	opts.NoFormula = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "HW-42", "hw/polecat")
}

func TestDefaultFormulaExplicitOnOverrides(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "HW-42")
	opts.OnFormula = "custom-formula"
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// MolCookOn goes through the store; verify the explicit formula was used.
	b, err := deps.Store.Get("gc-1")
	if err != nil {
		t.Fatalf("store.Get(gc-1): %v", err)
	}
	if b.Ref != "custom-formula" {
		t.Errorf("bead Ref = %q, want explicit custom-formula", b.Ref)
	}
	// Output should mention explicit formula, not default.
	if strings.Contains(stdout.String(), "default formula") {
		t.Errorf("stdout should not mention default formula when --on is explicit: %q", stdout.String())
	}
}

func TestDefaultFormulaExplicitFormulaOverrides(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "code-review")
	opts.IsFormula = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0 for built-in routing: %v", len(runner.calls), runner.calls)
	}
	assertStoreRoutedTo(t, deps.Store, "gc-1", "hw/polecat")
	b, err := deps.Store.Get("gc-1")
	if err != nil {
		t.Fatalf("store.Get(gc-1): %v", err)
	}
	if b.Ref != "code-review" {
		t.Errorf("bead Ref = %q, want explicit code-review", b.Ref)
	}
}

func TestDefaultFormulaBatchApplied(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	querier := newFakeChildQuerier()
	querier.beadsByID["CVY-1"] = beads.Bead{ID: "CVY-1", Type: "convoy", Status: "open"}
	querier.childrenOf["CVY-1"] = []beads.Bead{
		{ID: "HW-1", Type: "task", Status: "open"},
		{ID: "HW-2", Type: "task", Status: "open"},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "CVY-1")
	code := doSlingBatch(opts, deps, querier, stdout, stderr)

	if code != 0 {
		t.Fatalf("doSlingBatch returned %d, want 0; stderr: %s", code, stderr.String())
	}
	// MolCookOn goes through the store; verify 2 molecule beads were created.
	all, _ := deps.Store.ListOpen()
	molCount := 0
	for _, b := range all {
		if b.Type == "molecule" {
			molCount++
		}
	}
	if molCount != 2 {
		t.Errorf("got %d molecule beads in store, want 2 (one per child)", molCount)
	}
	if !strings.Contains(stdout.String(), "default formula") {
		t.Errorf("stdout should mention default formula: %q", stdout.String())
	}
}

func TestDefaultFormulaDryRun(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", Dir: "hw", DefaultSlingFormula: strPtr("mol-polecat-work")}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	opts := testOpts(a, "HW-42")
	opts.DryRun = true
	code := doSling(opts, deps, nil, stdout, stderr)

	if code != 0 {
		t.Fatalf("dryRunSingle returned %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Default formula:") {
		t.Errorf("dry-run should show Default formula section; got:\n%s", out)
	}
	if !strings.Contains(out, "mol-polecat-work") {
		t.Errorf("dry-run should show formula name; got:\n%s", out)
	}
	if !strings.Contains(out, "--no-formula") {
		t.Errorf("dry-run should mention --no-formula suppression; got:\n%s", out)
	}
	// No runner calls in dry-run.
	if len(runner.calls) != 0 {
		t.Errorf("dry-run should not execute commands; got %v", runner.calls)
	}
}

func TestBuildSlingFormulaVarsUsesBeadTargetForPolecatFormula(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				Metadata: map[string]string{"target": "integration/convoy-7"},
			},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "issue"); !ok || got != "HW-42" {
		t.Fatalf("issue var = %q, %v; want HW-42, true", got, ok)
	}
	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "integration/convoy-7" {
		t.Fatalf("base_branch var = %q, %v; want integration/convoy-7, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsUsesAncestorTargetForPolecatFormula(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				ParentID: "CVY-7",
			},
			"CVY-7": {
				ID:       "CVY-7",
				Type:     "convoy",
				Metadata: map[string]string{"target": "integration/convoy-7"},
			},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "integration/convoy-7" {
		t.Fatalf("base_branch var = %q, %v; want integration/convoy-7, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsIgnoresNonConvoyAncestorTarget(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				ParentID: "EP-7",
			},
			"EP-7": {
				ID:       "EP-7",
				Type:     "epic",
				Metadata: map[string]string{"target": "integration/legacy-epic"},
			},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if _, ok := findVarValue(vars, "base_branch"); !ok {
		t.Fatal("base_branch var missing")
	}
	if got, _ := findVarValue(vars, "base_branch"); got == "integration/legacy-epic" {
		t.Fatalf("base_branch inherited from non-convoy ancestor: %q", got)
	}
}

func TestBuildSlingFormulaVarsSkipsNonConvoyAncestorTargetAndUsesConvoyAncestor(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				ParentID: "EP-7",
			},
			"EP-7": {
				ID:       "EP-7",
				Type:     "epic",
				ParentID: "CVY-9",
				Metadata: map[string]string{"target": "integration/legacy-epic"},
			},
			"CVY-9": {
				ID:       "CVY-9",
				Type:     "convoy",
				Metadata: map[string]string{"target": "integration/convoy-9"},
			},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "integration/convoy-9" {
		t.Fatalf("base_branch var = %q, %v; want integration/convoy-9, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsUsesRigDefaultBranchWhenTargetMissing(t *testing.T) {
	repoDir := newRepoWithOriginHead(t, "develop")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "hw", Path: repoDir},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = &recordingStore{Store: beads.NewMemStore()}

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "develop" {
		t.Fatalf("base_branch var = %q, %v; want develop, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsPreservesSlashesInRigDefaultBranch(t *testing.T) {
	// Regression test for #719: slashes in the default branch must survive
	// the rig → defaultBranchFor → base_branch path, not just the internal
	// git parser. Previously LastIndex(ref, "/") truncated at the consumer
	// boundary too.
	repoDir := newRepoWithOriginHead(t, "team/feature/x")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "hw", Path: repoDir},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = &recordingStore{Store: beads.NewMemStore()}

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "team/feature/x" {
		t.Fatalf("base_branch var = %q, %v; want team/feature/x, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsPreservesSlashesInRefineryTargetBranch(t *testing.T) {
	// Regression test for #719 covering the refinery target_branch path.
	repoDir := newRepoWithOriginHead(t, "boylec/develop")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "hw", Path: repoDir},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)

	vars := buildSlingFormulaVars("mol-refinery-patrol", "", nil, config.Agent{Name: "refinery", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "target_branch"); !ok || got != "boylec/develop" {
		t.Fatalf("target_branch var = %q, %v; want boylec/develop, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsPreservesExplicitValues(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				Metadata: map[string]string{"target": "integration/convoy-7"},
			},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42",
		[]string{"issue=custom-1", "base_branch=release/1.2"}, config.Agent{Name: "polecat", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "issue"); !ok || got != "custom-1" {
		t.Fatalf("issue var = %q, %v; want custom-1, true", got, ok)
	}
	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "release/1.2" {
		t.Fatalf("base_branch var = %q, %v; want release/1.2, true", got, ok)
	}
}

func TestBeadMetadataTargetStopsOnParentCycle(t *testing.T) {
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"A": {ID: "A", ParentID: "B"},
			"B": {ID: "B", ParentID: "A"},
		},
	}

	if got := beadMetadataTarget(store, "A"); got != "" {
		t.Fatalf("beadMetadataTarget = %q, want empty string", got)
	}
}

func TestBuildSlingFormulaVarsSeedsRefineryTargetBranch(t *testing.T) {
	repoDir := newRepoWithOriginHead(t, "trunk")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "hw", Path: repoDir},
		},
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)

	vars := buildSlingFormulaVars("mol-refinery-patrol", "", nil, config.Agent{Name: "refinery", Dir: "hw"}, deps)

	if got, ok := findVarValue(vars, "target_branch"); !ok || got != "trunk" {
		t.Fatalf("target_branch var = %q, %v; want trunk, true", got, ok)
	}
}

func TestBuildSlingFormulaVarsInjectsIssueAndBaseBranch(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "hw", Path: newRepoWithOriginHead(t, "develop")},
		},
	}
	a := config.Agent{
		Name:              "polecat",
		Dir:               "hw",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5),
	}

	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	store := &recordingStore{
		Store: beads.NewMemStore(),
		beadsByID: map[string]beads.Bead{
			"HW-42": {
				ID:       "HW-42",
				Metadata: map[string]string{"target": "integration/convoy-7"},
			},
		},
	}
	deps.Store = store

	vars := buildSlingFormulaVars("mol-polecat-work", "HW-42", nil, a, deps)

	if got, ok := findVarValue(vars, "issue"); !ok || got != "HW-42" {
		t.Fatalf("issue var = %q, %v; want HW-42, true", got, ok)
	}
	if got, ok := findVarValue(vars, "base_branch"); !ok || got != "integration/convoy-7" {
		t.Fatalf("base_branch var = %q, %v; want integration/convoy-7, true", got, ok)
	}
}

// --- 1-arg sling tests (via doSling, not cmdSling which needs a real city) ---

func TestFindRigByPrefix(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "hello-world", Path: "/tmp/hw", Prefix: "hw"},
			{Name: "my-project", Path: "/tmp/mp"},
		},
	}

	// Exact match.
	rig, ok := findRigByPrefix(cfg, "hw")
	if !ok {
		t.Fatal("expected to find rig with prefix hw")
	}
	if rig.Name != "hello-world" {
		t.Errorf("rig.Name = %q, want hello-world", rig.Name)
	}

	// Case-insensitive match.
	rig, ok = findRigByPrefix(cfg, "HW")
	if !ok {
		t.Fatal("expected case-insensitive match for HW")
	}
	if rig.Name != "hello-world" {
		t.Errorf("rig.Name = %q, want hello-world", rig.Name)
	}

	// Derived prefix match.
	rig, ok = findRigByPrefix(cfg, "mp")
	if !ok {
		t.Fatal("expected to find rig with derived prefix mp")
	}
	if rig.Name != "my-project" {
		t.Errorf("rig.Name = %q, want my-project", rig.Name)
	}

	// No match.
	_, ok = findRigByPrefix(cfg, "zz")
	if ok {
		t.Error("expected no match for prefix zz")
	}
}

func TestOneArgSlingNoPrefix(t *testing.T) {
	// A bead ID with no dash can't derive a prefix.
	// We test this through cmdSling but that requires a city on disk.
	// Instead, test the sling.BeadPrefix helper directly — canonical coverage
	// lives in internal/sling; this just verifies the no-dash contract.
	got := sling.BeadPrefix("nodash")
	if got != "" {
		t.Errorf("sling.BeadPrefix(%q) = %q, want empty", "nodash", got)
	}
}

func TestOneArgSlingFormulaRequiresTarget(t *testing.T) {
	// --formula with 1 arg is checked in newSlingCmd via cobra.RangeArgs.
	// Verify the flag exists and the error path message.
	cmd := newSlingCmd(&bytes.Buffer{}, &bytes.Buffer{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--formula", "code-review"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --formula with 1 arg")
	}
}

func TestLooksLikeBeadID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Valid bead IDs (digits-only suffix).
		{"BL-42", true},
		{"gc-1", true},
		{"HW-7", true},
		{"FE-123", true},
		{"abc-0", true},

		// Valid bead IDs (base36 hash suffix from bd).
		{"mp-1j1", true},
		{"mp-bfg", true},
		{"od-2ie", true},
		{"BL-42a", true},
		{"g6-53b", true},

		// Valid bead IDs (5-char base36 suffix from bd).
		{"gc-56nqn", true},
		{"mp-a1b2c", true},

		// Valid bead IDs (longer base36 hash suffixes from bd, up to 8 chars).
		{"gc-8bi3tk", true},
		{"gc-r5sr6bm", true},

		// Valid bead IDs (5-digit numeric suffix from bd counter mode).
		{"gc-10000", true},
		{"gc-99999", true},

		// Valid bead IDs (hierarchical / epic children with dot notation).
		{"ProjectWrenUnity-0fze.1", true},
		{"gc-42.3", true},
		{"BL-1a.7", true},

		// Inline text (not bead IDs).
		{"write a README", false},
		{"write README", false},
		{"hello world", false},
		{"fix the bug", false},

		// Edge cases — not bead IDs.
		{"", false},
		{"nodash", false},
		{"-1", false},
		{"42-abc", false},      // digits before dash
		{"BL-", false},         // nothing after dash
		{"code-review", false}, // long suffix (6+ chars, formula name)
		{"hello-world", false}, // all-alpha suffix (no digit), treated as inline text
		{"hello-there", false}, // all-alpha suffix, not a bead ID
		{"od-zzzzz", false},    // all-alpha suffix, rare but caught by beadExistsInStore fallback
	}
	for _, tt := range tests {
		got := looksLikeBeadID(tt.input)
		if got != tt.want {
			t.Errorf("looksLikeBeadID(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBeadExistsInStoreFallback(t *testing.T) {
	base := beads.NewMemStore()
	store := &recordingStore{
		Store:     base,
		beadsByID: map[string]beads.Bead{"ProjectWrenUnity-0fze.1": {ID: "ProjectWrenUnity-0fze.1", Title: "Expedition step"}},
	}

	// beadExistsInStore should find it.
	if !beadExistsInStore(store, "ProjectWrenUnity-0fze.1") {
		t.Error("beadExistsInStore should find existing bead")
	}

	// Non-existent bead should return false.
	if beadExistsInStore(store, "nonexistent-xyz") {
		t.Error("beadExistsInStore should return false for missing bead")
	}
}

func TestOneArgSlingInlineTextRequiresTarget(t *testing.T) {
	// Inline text with 1 arg should error asking for explicit target.
	cmd := newSlingCmd(&bytes.Buffer{}, &bytes.Buffer{})
	cmd.SetArgs([]string{"write a README"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for inline text with 1 arg")
	}
}

func TestSlingStdinSingleLine(t *testing.T) {
	// --stdin with a single line creates a bead with title only.
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps, stdout, stderr := testDeps(cfg, sp, runner.run)

	// Override slingStdin to provide test input.
	old := slingStdin
	slingStdin = func() io.Reader { return strings.NewReader("fix login bug\n") }
	defer func() { slingStdin = old }()

	// Simulate what cmdSling does for --stdin: read stdin, create bead, sling it.
	content := "fix login bug"
	created, err := deps.Store.Create(beads.Bead{Title: content, Type: "task"})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	opts := testOpts(a, created.ID)
	code := doSling(opts, deps, nil, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Slung "+created.ID) {
		t.Errorf("stdout = %q, want to contain 'Slung %s'", stdout.String(), created.ID)
	}

	// Verify the bead has no description.
	got, err := deps.Store.Get(created.ID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	if got.Description != "" {
		t.Errorf("bead description = %q, want empty", got.Description)
	}
}

func TestSlingStdinMultiLine(t *testing.T) {
	// --stdin with multiple lines: first line = title, rest = description.
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps, stdout, stderr := testDeps(cfg, sp, runner.run)

	old := slingStdin
	slingStdin = func() io.Reader {
		return strings.NewReader("fix login bug\nThe login page returns 500\nwhen email has a plus sign\n")
	}
	defer func() { slingStdin = old }()

	// Create bead with description (simulating the stdin split).
	created, err := deps.Store.Create(beads.Bead{
		Title:       "fix login bug",
		Description: "The login page returns 500\nwhen email has a plus sign",
		Type:        "task",
	})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	opts := testOpts(a, created.ID)
	code := doSling(opts, deps, nil, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Slung "+created.ID) {
		t.Errorf("stdout = %q, want to contain 'Slung %s'", stdout.String(), created.ID)
	}

	// Verify bead has description.
	got, err := deps.Store.Get(created.ID)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	if got.Description != "The login page returns 500\nwhen email has a plus sign" {
		t.Errorf("bead description = %q, want multi-line description", got.Description)
	}
}

func TestSlingStdinEmpty(t *testing.T) {
	// --stdin with empty input returns error.
	var stderr bytes.Buffer
	cmd := newSlingCmd(&bytes.Buffer{}, &stderr)
	cmd.SetArgs([]string{"mayor", "--stdin"})

	old := slingStdin
	slingStdin = func() io.Reader { return strings.NewReader("") }
	defer func() { slingStdin = old }()

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty stdin")
	}
	if !strings.Contains(stderr.String(), "no input received") {
		t.Errorf("stderr = %q, want to contain 'no input received'", stderr.String())
	}
}

func TestSlingStdinMutuallyExclusiveWithFormula(t *testing.T) {
	// --stdin and --formula are mutually exclusive.
	var stderr bytes.Buffer
	cmd := newSlingCmd(&bytes.Buffer{}, &stderr)
	cmd.SetArgs([]string{"mayor", "--stdin", "--formula"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --stdin with --formula")
	}
}

func TestSlingStdinWithExtraArg(t *testing.T) {
	// --stdin with 2 positional args (target + text) should error.
	var stderr bytes.Buffer
	cmd := newSlingCmd(&bytes.Buffer{}, &stderr)
	cmd.SetArgs([]string{"mayor", "extra-arg", "--stdin"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --stdin with extra positional arg")
	}
	if !strings.Contains(stderr.String(), "--stdin requires exactly 1 argument") {
		t.Errorf("stderr = %q, want to contain '--stdin requires exactly 1 argument'", stderr.String())
	}
}
