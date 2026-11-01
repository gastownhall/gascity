package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/usage"
	"github.com/gastownhall/gascity/internal/worker"
)

// Formula attribution end to end (ga-ihdclm3): the fact a session's model usage
// mints carries the formula its acting bead's run root was compiled from. The
// runs here are launched by the real sling entry point and the pointers are
// written by the real reconciler/binder code, never hand-stamped.

// attributionReadStore counts the reads a store serves.
type attributionReadStore struct {
	beads.Store
	reads atomic.Int64
}

func (s *attributionReadStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.Get(id)
}

func (s *attributionReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.List(q)
}

func (s *attributionReadStore) ListByMetadata(f map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.Store.ListByMetadata(f, limit, opts...)
}

// slingGraphRun launches formula through doSling into store and returns one
// step bead that belongs to the workflow run it created.
func slingGraphRun(t *testing.T, store beads.Store, formula, sourceID string) beads.Bead {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	body := "formula = \"" + formula + "\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"step\"\ntitle = \"Do work\"\n"
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], formula+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	opts := testOpts(a, sourceID)
	opts.OnFormula = formula
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling(%s) = %d; stderr: %s", formula, code, stderr.String())
	}
	roots, err := store.ListByMetadata(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaNameMetadataKey: formula}, 0)
	if err != nil || len(roots) != 1 {
		t.Fatalf("workflow roots for %s = %v, %v; want exactly one", formula, roots, err)
	}
	root := roots[0]
	all, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.ID != root.ID && b.Title == "Do work" && beadmeta.ResolveRunID(b.Metadata, b.ID, "") == root.ID {
			return b
		}
	}
	t.Fatalf("no step bead of %s reachable from root %s in %+v", formula, root.ID, all)
	return beads.Bead{}
}

// rigStoreFixture is a rig store seeded high so its ids never collide with the
// city store's.
func rigStoreFixture(sourceID string) *beads.MemStore {
	return beads.NewMemStoreFrom(5000, []beads.Bead{{ID: sourceID, Title: "Work", Type: "task", Status: "open"}}, nil)
}

func modelFactFormulaNames(t *testing.T, sinkPath string) []string {
	t.Helper()
	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	var out []string
	for _, f := range facts {
		if f.Kind == usage.KindModel {
			out = append(out, f.FormulaName)
		}
	}
	return out
}

func wantAllNamed(t *testing.T, got []string, want string, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("model facts = %d (%v), want %d", len(got), got, n)
	}
	for _, g := range got {
		if g != want {
			t.Fatalf("formula names = %v, want every one %q", got, want)
		}
	}
}

// liveSweepWithRig builds the live-sweep harness over a city store plus the
// given rig stores, serving or suspended per the rigs slice.
func liveSweepWithRigs(t *testing.T, rigs []config.Rig, stores map[string]beads.Store) liveSweepHarness {
	t.Helper()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{{150, 100, 50}, {450, 200, 100}})
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))
	h.cr.cfg.Rigs = rigs
	h.cr.cs.beadStores = stores
	return h
}

// T1 (sweep path, controller): a rig-resident run launched by doSling, the
// session pointed at its step by the reconciler's wake stamp.
func TestLiveSweepFormulaAttributionFromRealSlingAndReconcilerPointer(t *testing.T) {
	rig := rigStoreFixture("BL-42")
	step := slingGraphRun(t, rig, "mol-attribution-sling", "BL-42")
	h := liveSweepWithRigs(t, []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "gc"}}, map[string]beads.Store{"alpha": rig})

	front := sessionFrontDoor(h.store)
	info, err := front.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	if patch := recordCurrentBeadIDOnWake(info, front, step.ID, io.Discard); patch == nil {
		t.Fatalf("reconciler wake stamp wrote nothing for %s", step.ID)
	}

	h.tick()
	wantAllNamed(t, modelFactFormulaNames(t, h.sinkPath), "mol-attribution-sling", 2)
}

// The fallback pointer: a session that carries only the trigger, written by the
// real pool binder's patch.
func TestLiveSweepFormulaAttributionFromRealBinderTriggerOnly(t *testing.T) {
	rig := rigStoreFixture("BL-42")
	step := slingGraphRun(t, rig, "mol-attribution-trigger", "BL-42")
	h := liveSweepWithRigs(t, []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "gc"}}, map[string]beads.Store{"alpha": rig})

	front := sessionFrontDoor(h.store)
	info, err := front.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	patch := computePoolTriggerBindingPatch(info, SessionRequest{WorkBeadID: step.ID}, "")
	if patch[beadmeta.TriggerBeadIDMetadataKey] != step.ID {
		t.Fatalf("binder patch = %v, want the trigger set to %s", patch, step.ID)
	}
	if _, err := front.UpdateMetadataInfo(info, patch); err != nil {
		t.Fatal(err)
	}

	h.tick()
	wantAllNamed(t, modelFactFormulaNames(t, h.sinkPath), "mol-attribution-trigger", 2)
}

// T1 (prompt-op path): the one-shot CLI factory, whose store is the city store.
// The run and the session bead live there, so the factory-store fallback
// resolves it.
func TestPromptOpFormulaAttributionFromRealSlingAndReconcilerPointer(t *testing.T) {
	cityPath := t.TempDir()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "BL-42", Title: "Work", Type: "task", Status: "open"}}, nil)
	step := slingGraphRun(t, store, "mol-attribution-promptop", "BL-42")

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{searchBase}}}
	factory, err := workerFactoryWithConfig(cityPath, store, runtime.NewFake(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h, err := factory.Session(worker.SessionSpec{
		Profile:  worker.ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	actor := testActorIn(t, session.ActorAgent, cityPath)
	if err := h.Start(context.Background(), actor); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sessions, err := store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("session beads = %v, %v; want one", sessions, err)
	}
	sess := sessions[0]
	front := sessionFrontDoor(store)
	info, err := front.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if patch := recordCurrentBeadIDOnWake(info, front, step.ID, io.Discard); patch == nil {
		t.Fatalf("reconciler wake stamp wrote nothing for %s", step.ID)
	}

	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(map[string]any{
		"type": "assistant",
		"uuid": "u1",
		"message": map[string]any{
			"id": "msg-1", "role": "assistant", "model": "claude-opus-4-7",
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 50},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slugDir, sess.Metadata["session_key"]+".jsonl"), append(entry, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Message(context.Background(), actor, worker.MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	wantAllNamed(t, modelFactFormulaNames(t, filepath.Join(cityPath, ".gc", "usage.jsonl")), "mol-attribution-promptop", 1)
}

// T10: a pointer into a SUSPENDED rig attributes to "" and the suspended
// store is never read; a pointer into a serving rig resolves.
func TestFormulaBeadResolverSkipsSuspendedRigs(t *testing.T) {
	alpha := &attributionReadStore{Store: rigStoreFixture("AL-1")}
	bravo := &attributionReadStore{Store: beads.NewMemStoreFrom(9000, []beads.Bead{{ID: "BR-1", Title: "Work", Type: "task", Status: "open"}}, nil)}
	alphaStep := slingGraphRun(t, alpha, "mol-in-suspended", "AL-1")
	bravoStep := slingGraphRun(t, bravo, "mol-in-serving", "BR-1")
	alpha.reads.Store(0)

	cityPath := t.TempDir()
	cfg := residencyTestConfig()
	// The mem stores mint "gc-" ids, so both rigs carry that prefix: the
	// suspended rig is one a by-id probe would otherwise reach, which is what
	// makes its zero reads mean something.
	cfg.Rigs[0].Prefix = "gc"
	cfg.Rigs[1].Prefix = "gc"
	cfg.Rigs[0].Suspended = true
	cr := &CityRuntime{
		cfg:                 cfg,
		cityPath:            cityPath,
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"alpha": alpha, "bravo": bravo},
		storageRoutes:       splitRoutes(beads.NewMemStore()),
	}
	resolve := cr.formulaBeadResolver(cfg, nil)

	if _, err := resolve(alphaStep.ID); err == nil {
		t.Fatalf("resolving %s in the suspended rig succeeded, want beads.ErrNotFound", alphaStep.ID)
	}
	if got := alpha.reads.Load(); got != 0 {
		t.Fatalf("suspended rig store served %d reads, want 0", got)
	}
	got, err := resolve(bravoStep.ID)
	if err != nil || got.ID != bravoStep.ID {
		t.Fatalf("resolving %s in the serving rig = %+v, %v", bravoStep.ID, got, err)
	}
}

// T10 end to end: the swept batch from a session pointed into a suspended rig
// carries an empty name and the suspended store serves no reads.
func TestLiveSweepPointerIntoSuspendedRigIsUnattributedWithZeroReads(t *testing.T) {
	alpha := &attributionReadStore{Store: rigStoreFixture("AL-1")}
	step := slingGraphRun(t, alpha, "mol-in-suspended", "AL-1")
	alpha.reads.Store(0)
	h := liveSweepWithRigs(t, []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "gc", Suspended: true}}, map[string]beads.Store{"alpha": alpha})

	front := sessionFrontDoor(h.store)
	info, err := front.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	recordCurrentBeadIDOnWake(info, front, step.ID, io.Discard)

	h.tick()
	wantAllNamed(t, modelFactFormulaNames(t, h.sinkPath), "", 2)
	if got := alpha.reads.Load(); got != 0 {
		t.Fatalf("suspended rig store served %d reads, want 0", got)
	}
}

// T11: the serving frame is built lazily, once, and only when something is
// actually looked up.
func TestFormulaBeadResolverBuildsTopologyLazilyAndOnce(t *testing.T) {
	cityStore := beads.NewMemStore()
	b, err := cityStore.Create(beads.Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cr := &CityRuntime{cfg: residencyTestConfig(), standaloneCityStore: cityStore}
	var builds int
	resolve := cr.formulaBeadResolver(cr.cfg, func() storeref.Topology {
		builds++
		return cr.residencyTopology(cr.rigBeadStores())
	})
	if builds != 0 {
		t.Fatalf("topology built %d times before any lookup, want 0", builds)
	}
	for i := 0; i < 3; i++ {
		if got, err := resolve(b.ID); err != nil || got.ID != b.ID {
			t.Fatalf("lookup %d = %+v, %v", i, got, err)
		}
	}
	if _, err := resolve("missing-1"); err == nil {
		t.Fatalf("lookup of a missing id succeeded, want an error")
	}
	if builds != 1 {
		t.Fatalf("topology built %d times across four lookups, want exactly 1", builds)
	}
}

// A pointer into a rig that is suspended at lookup time is unattributed, and
// that miss is not remembered: once the rig resumes, the next batch through the
// same runtime's memo is attributed.
func TestLiveSweepPointerIntoResumedRigRecoversAttribution(t *testing.T) {
	alpha := rigStoreFixture("AL-1")
	step := slingGraphRun(t, alpha, "mol-in-resumed", "AL-1")

	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{{150, 100, 50}})
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))
	h.cr.cfg.Rigs = []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "gc", Suspended: true}}
	h.cr.cs.beadStores = map[string]beads.Store{"alpha": alpha}

	front := sessionFrontDoor(h.store)
	info, err := front.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	if patch := recordCurrentBeadIDOnWake(info, front, step.ID, io.Discard); patch == nil {
		t.Fatalf("reconciler wake stamp wrote nothing for %s", step.ID)
	}

	h.tick()
	wantAllNamed(t, modelFactFormulaNames(t, h.sinkPath), "", 1)

	h.cr.cfg.Rigs[0].Suspended = false
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{{150, 100, 50}, {450, 200, 100}})
	h.expireSweepThrottle()
	h.tick()
	got := modelFactFormulaNames(t, h.sinkPath)
	if len(got) != 2 || got[0] != "" || got[1] != "mol-in-resumed" {
		t.Fatalf("formula names across the resume = %q, want [\"\" \"mol-in-resumed\"]", got)
	}
}
