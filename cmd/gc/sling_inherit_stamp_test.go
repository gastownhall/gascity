package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

// inheritStampDeps wires deps whose source-workflow stores are store itself,
// the stores cmdSlingWithJSON also opens as the claim stores.
func inheritStampDeps(cfg *config.City, store beads.Store) (slingDeps, *bytes.Buffer, *bytes.Buffer) {
	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Store = store
	deps.SourceWorkflowStores = func() ([]sling.SourceWorkflowStore, error) {
		return []sling.SourceWorkflowStore{{Store: store, StoreRef: deps.StoreRef, Strict: true}}, nil
	}
	return deps, stdout, stderr
}

// inheritInSession runs inheritSlingInitiator as the calling session, over
// deps' source-workflow stores as its claim stores, and requires it to pick up
// alice without a warning.
func inheritInSession(t *testing.T, opts *slingOpts, deps slingDeps) {
	t.Helper()
	var stderr bytes.Buffer
	env := slingInheritEnv(map[string]string{"GC_SESSION_NAME": inheritStampCaller})
	claimStores := func() ([]sling.SourceWorkflowStore, []sourceWorkflowStoreSkip, error) {
		stores, err := deps.SourceWorkflowStores()
		return stores, nil, err
	}
	inheritSlingInitiator(opts, claimStores, env, &stderr)
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

// inheritCmdSlingRigBeads is the rig ledger the cmdSling tests start from: a
// poured molecule launched by initiator, its source bead claimed by claimant,
// and the open bead the tests sling.
func inheritCmdSlingRigBeads(initiator, claimant string) []beads.Bead {
	return []beads.Bead{
		{ID: "fo-multi-work", Title: "multi-target work bead", Type: "task", Status: "open", Metadata: map[string]string{}},
		{ID: "fo-run", Title: "run root", Type: "molecule", Status: "open", Metadata: map[string]string{beadmeta.InitiatorMetadataKey: initiator}},
		{ID: "fo-claim", Title: "claimed source", Type: "task", Status: "in_progress", Assignee: claimant, Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: "fo-run"}},
	}
}

// cmdSlingFromSession runs `gc sling <beadID>` through cmdSling with env set
// as the calling session's, and requires it to route without an inheritance
// warning.
func cmdSlingFromSession(t *testing.T, env map[string]string, beadID string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{beadID},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		false, false, false,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("cmdSling returned %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "not inheriting") {
		t.Fatalf("stderr = %q, want no inheritance warning", stderr.String())
	}
}

// TestCmdSlingFromSessionStampsInheritedInitiator drives `gc sling` itself
// from inside a session that has claimed carol's run under its name: the
// local path runs inheritance, so the bead it routes is stamped carol.
func TestCmdSlingFromSessionStampsInheritedInitiator(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, []string{"foundations/worker-a"})
	writeTestFileStoreBeads(t, rigDir, inheritCmdSlingRigBeads("carol", "foundations/worker-b"))

	cmdSlingFromSession(t, map[string]string{"GC_SESSION_NAME": "foundations/worker-b"}, "fo-multi-work")
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	requireStoredInitiator(t, rigStore, "fo-multi-work", "carol")
}

// TestCmdSlingFromSessionInheritsThroughCurrentClaim drives `gc sling` from a
// session whose claim is assigned to an identity the session does not carry,
// so only the session bead's current_claim_bead_id, read through the session
// front door, leads to dave's run.
func TestCmdSlingFromSessionInheritsThroughCurrentClaim(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, []string{"foundations/worker-a"})
	writeTestFileStoreBeads(t, rigDir, inheritCmdSlingRigBeads("dave", "pool-seat-7"))
	writeTestFileStoreBeads(t, cityDir, []beads.Bead{{
		ID:       "gc-sess-1",
		Title:    "worker session",
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: "fo-claim"},
	}})

	cmdSlingFromSession(t, map[string]string{"GC_SESSION_ID": "gc-sess-1"}, "fo-multi-work")
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	requireStoredInitiator(t, rigStore, "fo-multi-work", "dave")
}
