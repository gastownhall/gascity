package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sling"
)

// In-session inheritance (sling_inherit.go) and sling's initiator stamping
// (internal/sling) together: a `gc sling` run from inside a session that holds
// a claim on alice's run must leave gc.initiator = alice on the work it
// creates, not just on slingOpts. These tests drive the same two calls
// cmdSlingWithJSON makes — inheritSlingInitiator, then the sling entry point —
// over one store, and read the stamp back from the store.

const inheritStampCaller = "worker-1"

// inheritStampStore seeds a store holding alice's run root, a step of that run
// claimed by the calling session, and an unrelated open bead to sling.
func inheritStampStore(t *testing.T) (store *beads.MemStore, workID string) {
	t.Helper()
	store = beads.NewMemStore()
	root := inheritRootWithInitiator(t, store, "alice")
	inheritClaimFor(t, store, inheritStampCaller, beadmeta.RootBeadIDMetadataKey, root.ID, nil)
	work := mustCreateInheritBead(t, store, beads.Bead{Title: "follow-up", Type: "task"})
	return store, work.ID
}

// inheritStampDeps wires deps whose claim stores are store itself, the way
// cmdSlingWithJSON wires deps.SourceWorkflowStores to the sling's own stores.
func inheritStampDeps(cfg *config.City, store beads.Store) (slingDeps, *bytes.Buffer, *bytes.Buffer) {
	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store
	deps.SourceWorkflowStores = func() ([]sling.SourceWorkflowStore, error) {
		return []sling.SourceWorkflowStore{{Store: store, StoreRef: deps.StoreRef, Strict: true}}, nil
	}
	return deps, stdout, stderr
}

// inheritInSession runs inheritSlingInitiator as the calling session and
// requires it to pick up alice without a warning.
func inheritInSession(t *testing.T, opts *slingOpts, deps slingDeps) {
	t.Helper()
	var stderr bytes.Buffer
	env := slingInheritEnv(map[string]string{"GC_SESSION_NAME": inheritStampCaller})
	inheritSlingInitiator(opts, deps, env, &stderr)
	if opts.Initiator != "alice" || stderr.Len() != 0 {
		t.Fatalf("inherited Initiator = %q, stderr = %q; want alice and no warning", opts.Initiator, stderr.String())
	}
}

func requireStoredInitiator(t *testing.T, store beads.Store, id, want string) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := b.Metadata[beadmeta.InitiatorMetadataKey]; got != want {
		t.Fatalf("%s %s = %q, want %q", id, beadmeta.InitiatorMetadataKey, got, want)
	}
}

func TestInSessionSlingStampsInheritedInitiatorOnRoutedBead(t *testing.T) {
	store, workID := inheritStampStore(t)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	deps, stdout, stderr := inheritStampDeps(cfg, store)
	opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, workID)

	inheritInSession(t, &opts, deps)
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling = %d; stderr: %s", code, stderr.String())
	}
	requireStoredInitiator(t, store, workID, "alice")
}

func TestInSessionSlingStampsInheritedInitiatorOnWorkflowRoot(t *testing.T) {
	store, workID := inheritStampStore(t)
	const formula = "inherit-stamp"
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	body := "formula = \"" + formula + "\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"step\"\ntitle = \"Do work\"\n"
	if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], formula+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	deps, stdout, stderr := inheritStampDeps(cfg, store)
	opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, workID)
	opts.OnFormula = formula
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"

	inheritInSession(t, &opts, deps)
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling = %d; stderr: %s", code, stderr.String())
	}
	roots, err := store.ListByMetadata(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaNameMetadataKey: formula}, 0)
	if err != nil || len(roots) != 1 {
		t.Fatalf("workflow roots = %v, %v; want exactly one", roots, err)
	}
	// graph.v2 --on routes the workflow root, not the source bead, so the
	// root is where the inherited stamp must land.
	requireStoredInitiator(t, store, roots[0].ID, "alice")
}
