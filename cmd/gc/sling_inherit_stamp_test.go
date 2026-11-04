package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
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
// want without a warning.
func inheritInSession(t *testing.T, opts *slingOpts, deps slingDeps, want string) {
	t.Helper()
	var stderr bytes.Buffer
	env := slingInheritEnv(map[string]string{"GC_SESSION_NAME": inheritStampCaller})
	claimStores := func() ([]sling.SourceWorkflowStore, []sourceWorkflowStoreSkip, error) {
		stores, err := deps.SourceWorkflowStores()
		return stores, nil, err
	}
	inheritSlingInitiator(opts, claimStores, env, &stderr)
	if opts.Initiator != want || stderr.Len() != 0 {
		t.Fatalf("inherited Initiator = %q, stderr = %q; want %s and no warning", opts.Initiator, stderr.String(), want)
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

	inheritInSession(t, &opts, deps, "alice")
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling = %d; stderr: %s", code, stderr.String())
	}
	requireStoredInitiator(t, store, workID, "alice")
}

// inSessionSlingAfterReslingOfClosedRun drives the re-sling flow: a plain sling
// launched by reslinger ("" for an unattributed one) routes a bead whose
// workflow_id still names alice's closed run, the calling session claims it,
// and the follow-up that session slings must be stamped want, with no
// inheritance warning.
func inSessionSlingAfterReslingOfClosedRun(t *testing.T, reslinger, want string) {
	t.Helper()
	store := beads.NewMemStore()
	closedRun := inheritClosedRootWithInitiator(t, store, "alice")
	reslung := mustCreateInheritBead(t, store, beads.Bead{
		Title: "re-slung work", Type: "task",
		Metadata: map[string]string{"workflow_id": closedRun.ID},
	})
	followUp := mustCreateInheritBead(t, store, beads.Bead{Title: "follow-up", Type: "task"})
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	deps, stdout, stderr := inheritStampDeps(cfg, store)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	resling := testOpts(worker, reslung.ID)
	resling.Initiator = reslinger
	if code := doSling(resling, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("re-sling doSling = %d; stderr: %s", code, stderr.String())
	}
	requireStoredInitiator(t, store, reslung.ID, reslinger)
	claimForInheritStampCaller(t, store, reslung.ID)

	slingFollowUpInSession(t, store, deps, followUp.ID, want)
}

// TestInSessionSlingPrefersTheClaimsOwnStampOverAStaleRun: bob re-slings the
// bead, so the follow-up is stamped bob (the stamp bob's sling left on the
// claim), not alice.
func TestInSessionSlingPrefersTheClaimsOwnStampOverAStaleRun(t *testing.T) {
	inSessionSlingAfterReslingOfClosedRun(t, "bob", "bob")
}

// TestInSessionSlingInheritsNothingFromAClosedRun: the re-sling carries no
// initiator, so it leaves the claim unstamped and its workflow_id on alice's
// closed run. That run no longer holds the claim, so the follow-up is stamped
// nothing, not alice.
func TestInSessionSlingInheritsNothingFromAClosedRun(t *testing.T) {
	inSessionSlingAfterReslingOfClosedRun(t, "", "")
}

// inheritStepFormula is the one-step graph.v2 formula the graph tests sling.
const inheritStepFormula = "inherit-stamp"

// inheritStepTitle is the title of inheritStepFormula's one step.
const inheritStepTitle = "Do work"

// inheritGraphCity is a city whose formula layer holds formulas (file name
// without .toml -> TOML body) beside inheritStepFormula, with the implicit
// agents and a control dispatcher, so a graph.v2 formula can be slung --on a
// bead.
func inheritGraphCity(t *testing.T, formulas map[string]string) *config.City {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{testFormulaDir(t)}
	files := map[string]string{
		inheritStepFormula: "formula = \"" + inheritStepFormula + "\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"step\"\ntitle = \"" + inheritStepTitle + "\"\n",
	}
	for name, body := range formulas {
		files[name] = body
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cfg.FormulaLayers.City[0], name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	return cfg
}

// inheritOnFormulaOpts is a city-scoped sling of formula --on beadID to the
// worker agent.
func inheritOnFormulaOpts(beadID, formula string) slingOpts {
	opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, beadID)
	opts.OnFormula = formula
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	return opts
}

// inheritOneBead returns the one bead in store carrying every key in meta.
func inheritOneBead(t *testing.T, store beads.Store, meta map[string]string) beads.Bead {
	t.Helper()
	found, err := store.ListByMetadata(meta, 0)
	if err != nil || len(found) != 1 {
		t.Fatalf("beads with %v = %v, %v; want exactly one", meta, found, err)
	}
	return found[0]
}

// inheritRunStep returns the step of the run rooted at rootID titled title.
func inheritRunStep(t *testing.T, store beads.Store, rootID, title string) beads.Bead {
	t.Helper()
	members, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}, 0)
	if err != nil {
		t.Fatalf("listing the beads of run %s: %v", rootID, err)
	}
	for _, b := range members {
		if b.Title == title {
			return b
		}
	}
	t.Fatalf("run %s has no step titled %q among %v", rootID, title, members)
	return beads.Bead{}
}

// claimForInheritStampCaller claims each bead in_progress for the calling
// session, the way `gc hook --claim` claims a routed step and core
// mol-do-work's `gc bd update --claim` claims the work bead.
func claimForInheritStampCaller(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	status, owner := "in_progress", inheritStampCaller
	for _, id := range ids {
		if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
			t.Fatalf("claiming %s: %v", id, err)
		}
	}
}

// slingFollowUpInSession slings followUpID from inside the calling session and
// requires both the inherited initiator and the stored stamp to be want, with
// no inheritance warning.
func slingFollowUpInSession(t *testing.T, store beads.Store, deps slingDeps, followUpID, want string) {
	t.Helper()
	opts := testOpts(config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, followUpID)
	inheritInSession(t, &opts, deps, want)
	var stdout, stderr bytes.Buffer
	if code := doSling(opts, deps, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("follow-up doSling = %d; stderr: %s", code, stderr.String())
	}
	requireStoredInitiator(t, store, followUpID, want)
}

func TestInSessionSlingStampsInheritedInitiatorOnWorkflowRoot(t *testing.T) {
	store, workID := inheritStampStore(t)
	cfg := inheritGraphCity(t, nil)
	deps, stdout, stderr := inheritStampDeps(cfg, store)
	opts := inheritOnFormulaOpts(workID, inheritStepFormula)

	inheritInSession(t, &opts, deps, "alice")
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling = %d; stderr: %s", code, stderr.String())
	}
	root := inheritOneBead(t, store, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaNameMetadataKey: inheritStepFormula})
	// graph.v2 --on routes the workflow root, not the source bead, so the
	// root is where the inherited stamp must land.
	requireStoredInitiator(t, store, root.ID, "alice")
}

// TestInSessionSlingInheritsTheRunOfItsClaimedInputBead drives core
// mol-do-work's flow: alice launches a graph.v2 formula --on a work bead,
// which stamps the workflow root but not the work bead, and the calling
// session claims both the step routed to it and the work bead. The work bead
// carries no run chain; the run's input convoy tracks it, so it counts as
// alice's run, even when it carries its creator's stamp, and the follow-up
// the session slings is stamped alice with no warning.
func TestInSessionSlingInheritsTheRunOfItsClaimedInputBead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workStamp string
	}{
		{name: "unstamped work bead"},
		{name: "work bead stamped by its creator", workStamp: "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			meta := map[string]string{}
			if tc.workStamp != "" {
				meta[beadmeta.InitiatorMetadataKey] = tc.workStamp
			}
			work := mustCreateInheritBead(t, store, beads.Bead{Title: "work", Type: "task", Metadata: meta})
			followUp := mustCreateInheritBead(t, store, beads.Bead{Title: "follow-up", Type: "task"})
			deps, stdout, stderr := inheritStampDeps(inheritGraphCity(t, nil), store)

			launch := inheritOnFormulaOpts(work.ID, inheritStepFormula)
			launch.Initiator = "alice"
			if code := doSling(launch, deps, nil, stdout, stderr); code != 0 {
				t.Fatalf("launch doSling = %d; stderr: %s", code, stderr.String())
			}
			requireStoredInitiator(t, store, work.ID, tc.workStamp)
			root := inheritOneBead(t, store, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaNameMetadataKey: inheritStepFormula})
			claimForInheritStampCaller(t, store, inheritRunStep(t, store, root.ID, inheritStepTitle).ID, work.ID)

			slingFollowUpInSession(t, store, deps, followUp.ID, "alice")
		})
	}
}

// TestInSessionSlingInheritsTheRunOfItsClaimedDrainItem is the drain-item
// variant: alice's run drains its input, the control dispatcher instantiates
// an item workflow for the member and copies alice onto the item root, and the
// calling session claims the item's step and the member bead, as an item
// formula such as core mol-do-work tells it to. The item root's input convoy
// tracks the member, so the follow-up is stamped alice with no warning.
func TestInSessionSlingInheritsTheRunOfItsClaimedDrainItem(t *testing.T) {
	store := beads.NewMemStore()
	member := mustCreateInheritBead(t, store, beads.Bead{Title: "drained member", Type: "task"})
	followUp := mustCreateInheritBead(t, store, beads.Bead{Title: "follow-up", Type: "task"})
	const drainFormula = "inherit-drain"
	cfg := inheritGraphCity(t, map[string]string{
		drainFormula: "formula = \"" + drainFormula + "\"\nversion = 1\ncontract = \"graph.v2\"\ntype = \"workflow\"\n\n" +
			"[[steps]]\nid = \"drain\"\ntitle = \"Drain the input\"\n\n" +
			"[steps.drain]\ncontext = \"separate\"\nformula = \"" + inheritStepFormula + "\"\n",
	})
	deps, stdout, stderr := inheritStampDeps(cfg, store)

	launch := inheritOnFormulaOpts(member.ID, drainFormula)
	launch.Initiator = "alice"
	if code := doSling(launch, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("launch doSling = %d; stderr: %s", code, stderr.String())
	}
	drain := inheritOneBead(t, store, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindDrain})
	if _, err := dispatch.ProcessControl(beadstest.Graph(store), drain, dispatch.ProcessOptions{
		Work:               beadstest.Work(store),
		FormulaSearchPaths: cfg.FormulaLayers.City,
	}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	itemRoot := inheritOneBead(t, store, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.DrainMemberIDMetadataKey: member.ID})
	requireStoredInitiator(t, store, itemRoot.ID, "alice")
	requireStoredInitiator(t, store, member.ID, "")
	claimForInheritStampCaller(t, store, inheritRunStep(t, store, itemRoot.ID, inheritStepTitle).ID, member.ID)

	slingFollowUpInSession(t, store, deps, followUp.ID, "alice")
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
