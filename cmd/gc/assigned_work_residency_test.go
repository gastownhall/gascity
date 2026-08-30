package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// S2 pins: the assigned-work spine reads Plan(AssignedWork).
//
// The §3.3 discipline — every migrated site lands with a before/after pin that
// asserts the resolver's leg list is IDENTICAL to the hand-rolled one where the
// site was already right, and states the DOCUMENTED delta where it was not.
// The pre-S2 list is written out literally in each row rather than computed, so
// a future change to either side has to change a row.

// seedSplitRoutes gives cityPath a served whole-split binding without opening
// one. It seeds the one-shot funnel's memo, which is the same seam the by-id and
// order-dispatch class tests already use: the topology constructors read OPENED
// ROUTES, so a test that only sets cfg.Storage is describing a city whose config
// says "split" and whose routes say nothing.
func seedSplitRoutes(t *testing.T, cityPath string, binding beads.Store) {
	t.Helper()
	resetCLIStorageRoutes(t)
	resetCLIResidencyBindings()
	t.Cleanup(resetCLIResidencyBindings)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = splitRoutes(binding) })
}

// seedNoRoutes is the single-store control: a city that relocates nothing.
func seedNoRoutes(t *testing.T, cityPath string) {
	t.Helper()
	resetCLIStorageRoutes(t)
	resetCLIResidencyBindings()
	t.Cleanup(resetCLIResidencyBindings)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = nil })
}

// planStores walks a plan through the executor and reports the stores it reads,
// in order — the list the migrated site used to build by hand.
func planStores(t *testing.T, plan storeref.ResolvedPlan) []beads.Store {
	t.Helper()
	var out []beads.Store
	if _, err := storeref.Walk(plan, func(l storeref.Leg) (bool, error) {
		out = append(out, l.Store)
		return false, nil
	}); err != nil {
		t.Fatalf("walking the plan: %v", err)
	}
	return out
}

func sameStores(got []beads.Store, want ...beads.Store) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Byte-identity pins for the sweep leg set.
// ---------------------------------------------------------------------------

// T0: a city that relocates nothing reads exactly the legs the pre-S2
// workAssignmentStores(store, rigStores) built — the leading store, then the rig
// stores by name. No probe, no binding, nothing added.
func TestAssignedWorkSweepPlanIsByteIdenticalOnASingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath)
	work, alpha, bravo := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	cfg := residencyTestConfig()

	plan, err := assignedWorkSweepPlan(cityPath, cfg, work, map[string]beads.Store{"bravo": bravo, "alpha": alpha}, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work, alpha, bravo) {
		t.Fatalf("sweep legs = %v, want [work, rig:alpha, rig:bravo] — the pre-S2 list verbatim", got)
	}
}

// T1: the controller's split city, where the leading store IS the binding (the
// reconciler leads with the sessions-class store and the whole split serves all
// five classes from one engine). The binding leg collapses onto the leading one,
// so the list is the pre-S2 list and the city pays nothing for the seam.
func TestAssignedWorkSweepPlanCollapsesALeadingBindingLeg(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	alpha := beads.NewMemStore()

	plan, err := assignedWorkSweepPlan(cityPath, residencyTestConfig(), binding, map[string]beads.Store{"alpha": alpha}, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, binding, alpha) {
		t.Fatalf("sweep legs = %v, want [binding(leading), rig:alpha]: a leg that resolved back to the leading store must not be read twice", got)
	}
}

// THE DOCUMENTED DELTA (ga-j4ob9). A scan that leads with the WORK store on a
// split city used to add `classBindingLegForSessionScan(cfg, store)` — which
// returns the store it was handed, so the "binding leg" was the work store
// again and deduped to nothing. The release path was therefore structurally
// blind to every claim claim-time routing had written into the binding. The
// resolver adds the REAL binding, last.
func TestAssignedWorkSweepPlanAddsTheRealBindingToAWorkLedScan(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work, alpha := beads.NewMemStore(), beads.NewMemStore()

	plan, err := assignedWorkSweepPlan(cityPath, residencyTestConfig(), work, map[string]beads.Store{"alpha": alpha}, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work, alpha, binding) {
		t.Fatalf("sweep legs = %v, want [work, rig:alpha, binding LAST] — a work-led release scan that cannot see the binding releases a routed claim by nothing", got)
	}
}

// Un-shape-gated: the binding comes from the OPENED ROUTES, so a city whose
// [storage] section was deleted after it had served a split fails LOUD instead
// of silently answering from the work ledger the classes were moved off.
func TestAssignedWorkSweepPlanFailsLoudOnARefusedCity(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	resetCLIResidencyBindings()
	t.Cleanup(resetCLIResidencyBindings)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() {
		entry.routes = refusingStorageRoutes("infra", errStorageRefusedForTest{})
	})

	if _, err := assignedWorkSweepPlan(cityPath, residencyTestConfig(), beads.NewMemStore(), nil, nil); err == nil {
		t.Fatal("a refused city planned an assigned-work sweep; a work-only sweep on a refused city is the answer that looks like success and reads the wrong ledger")
	}
}

// ---------------------------------------------------------------------------
// The session-scoped plan: what reachableStoresForSessionInfo used to answer.
// ---------------------------------------------------------------------------

// A rig-bound session reads its own rig store and the binding, in that order —
// byte-identical to the pre-S2 workAssignmentStores(rigStore, nil, binding). The
// city work store must NOT appear: reachability is the agent's SCOPE, and the
// resolver is told which work legs the scope has rather than deciding it.
func TestSessionAssignedWorkPlanStaysRigScopedAndAddsTheBinding(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	rigStore, cityStore := beads.NewMemStore(), beads.NewMemStore()

	plan, err := assignedWorkPlanForSessionInfo(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("assignedWorkPlanForSessionInfo: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, rigStore, binding) {
		t.Fatalf("session legs = %v, want [rig store, binding]", got)
	}
}

// Control: the same session on a city that relocates nothing reads one leg. The
// extra leg is a property of the SPLIT, not a general widening.
func TestSessionAssignedWorkPlanStaysSingleLeggedOnASingleStoreCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	seedNoRoutes(t, cityPath)
	rigStore, cityStore := beads.NewMemStore(), beads.NewMemStore()

	plan, err := assignedWorkPlanForSessionInfo(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("assignedWorkPlanForSessionInfo: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, rigStore) {
		t.Fatalf("session legs = %v, want only the rig store", got)
	}
}

// ---------------------------------------------------------------------------
// The release path: it must SEE a graph-resident claim, and a LIVE holder's
// claim must survive every leg that newly became visible. The two rows fail
// differently on purpose — the first as a strand, the second as claim LOSS.
// ---------------------------------------------------------------------------

// ga-j4ob9: a session died without drain-ack while holding a claim claim-time
// routing had written into the binding. The retired-session sweep leads with the
// WORK store here (the `gc session close` / stranded-repair shape), so before S2
// no leg of it could see the claim and the babysitter hand-released them.
func TestRetiredSessionSweepReleasesABindingResidentClaim(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := beads.NewMemStore()
	cfg := residencyTestConfig()

	sessionBead := beads.Bead{ID: "gcs-dead-1", Metadata: map[string]string{"session_name": "worker-1"}}
	step, err := binding.Create(beads.Bead{
		ID:       "gcg-step-1",
		Title:    "graph step claimed by a session that then died",
		Type:     "task",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("seed the binding-resident claim: %v", err)
	}
	inProgress := "in_progress"
	if err := binding.Update(step.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claim the step: %v", err)
	}

	unclaimWorkAssignedToRetiredSessionBead(cityPath, cfg, work, nil, sessionBead, "", io.Discard)

	got, err := binding.Get(step.ID)
	if err != nil {
		t.Fatalf("re-read the step: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("the retired-session sweep left %s as status=%q assignee=%q; a dead session's binding-resident claim has no other automatic reopen lane (ga-j4ob9)", step.ID, got.Status, got.Assignee)
	}
}

// THE CONTROL THAT MUST FAIL DIFFERENTLY. The same newly visible leg, but the
// holder is ALIVE: an open session bead whose identities include the assignee.
// Release-side false positives are claim LOSS, so widening what the release path
// can SEE may never widen what it releases. This row fails as a lost claim under
// a live worker, not as a strand.
// SEAM PIN — DO NOT FOLD INTO AN END-TO-END TEST.
//
// Three independent defenses keep a live holder's claim: this slice's close gate
// (the session is never retired), the widened ownership index (the release scan
// owns the leg), and #5242's owner-store liveness probe. Defense in depth is
// what makes the system safe and is also what makes an end-to-end test blind:
// remove any ONE of the three and a whole-tick test still passes, because the
// other two cover for it. Only a seam-level pin can fail on a single removal.
//
// This test and TestOpenSessionStoreRefIndexOwnsTheLeadingArmForARigBoundHolder
// are the only guards on the ownership-index defense. Deleting either, or
// rewriting it against the reconciler tick, silently retires the guard.
//
// The fixture is arranged so that ONLY the widened ownership index can save the
// claim: the session bead is graph-resident (in the binding), while the claim
// and both liveness probes' stores are the WORK ledger, so the #5242 primary and
// owner-store probes both miss. Pre-S2 the index answered "this rig-scoped
// holder owns only rig:riga", the leading-arm claim looked orphaned, and a live
// worker had its work reopened underneath it.
func TestOrphanReleaseSparesALiveHoldersBindingResidentClaim(t *testing.T) {
	cfg, cityPath, _ := rigScopedWakeFixture(t)
	cfg.Agents[0].MaxActiveSessions = intPtr(3)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := beads.NewMemStore()

	sess, err := binding.Create(beads.Bead{
		Title:    "live pool session, graph-resident",
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"template": "riga/worker", "session_name": "test-city--worker-1", "pool_managed": "true"},
	})
	if err != nil {
		t.Fatalf("create the live session bead: %v", err)
	}
	claim, err := work.Create(beads.Bead{
		Title:    "claim the live worker is executing",
		Type:     "task",
		Assignee: "test-city--worker-1",
		Metadata: map[string]string{"gc.routed_to": "riga/worker"},
	})
	if err != nil {
		t.Fatalf("create the claimed work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := work.Update(claim.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark the claim in_progress: %v", err)
	}
	if claim, err = work.Get(claim.ID); err != nil {
		t.Fatalf("reload the claimed work bead: %v", err)
	}

	infos := sessionInfosFromBeads([]beads.Bead{sess})
	released := releaseOrphanedPoolAssignments(
		work, beads.SessionStore{Store: work}, cfg, cityPath, infos,
		[]beads.Bead{claim}, []beads.Store{work}, []string{""}, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released %v — a LIVE holder's claim was taken back on a leg the release path now reads; that is claim loss, not a strand", released)
	}
	got, err := work.Get(claim.ID)
	if err != nil {
		t.Fatalf("re-read the claim: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "test-city--worker-1" {
		t.Fatalf("the live worker's claim is status=%q assignee=%q, want in_progress/test-city--worker-1", got.Status, got.Assignee)
	}
}

// The FIRST counterweight, and the one that fires earliest: a live rig-scoped
// holder with a binding-resident claim must read as "still has assigned work",
// so the drain/close lane never retires it and the retired-session sweep — the
// scan this slice taught to see the binding — is never pointed at it at all.
//
// This row fails as a session closed out from under a live claim; the strand row
// above fails as a claim nobody reopens. Two signatures, one leg set.
func TestCloseGateSeesALiveHoldersBindingResidentClaim(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	rigStore := beads.NewMemStore()

	claim, err := binding.Create(beads.Bead{
		Title:    "graph step the live worker is executing",
		Type:     "task",
		Assignee: "test-city--worker-1",
	})
	if err != nil {
		t.Fatalf("seed the binding-resident claim: %v", err)
	}
	inProgress := "in_progress"
	if err := binding.Update(claim.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claim it: %v", err)
	}

	// The work-led plane: the scan's leading store is NOT the binding.
	has, err := sessionHasOpenAssignedWorkForReachableStore(
		cityPath, cfg, beads.NewMemStore(), map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if !has {
		t.Fatal("the close gate reports a live claim-holder as holding nothing; its session bead is retired and the retired-session sweep then releases the claim it is executing")
	}
}

// SEAM PIN — DO NOT FOLD INTO AN END-TO-END TEST. See the note on
// TestOrphanReleaseSparesALiveHoldersBindingResidentClaim: these two are the
// only guards on the ownership-index defense, and defense in depth means a
// whole-tick test cannot see the index being removed.
//
// The ownership index is the counterweight the row above rests on: a rig-scoped
// holder now OWNS the leading arm for its own exact identities, so the release
// path's widened view cannot reap it. This is the I10 assertion flip its own
// comment asked for.
func TestOpenSessionStoreRefIndexOwnsTheLeadingArmForARigBoundHolder(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)

	index := makeOpenSessionStoreRefIndex(cityPath, cfg, binding, infos, true)
	identity := "test-city--worker-1"
	if !openSessionOwnsWork(nil, index, identity, "riga", true) {
		t.Error("the index dropped the holder's own rig leg")
	}
	if !openSessionOwnsWork(nil, index, identity, "", true) {
		t.Error("the index does not own the leading arm for its own rig-bound holder; a claim claim-time routing wrote there is released under a live worker (ga-j4ob9's counterweight)")
	}
	if openSessionOwnsWork(nil, index, "test-city--someone-else", "", true) {
		t.Error("the index owns the leading arm for a FOREIGN identity; the widening is per-identity ownership, not a blanket keep")
	}
}

// The refs the wake filter and the ownership index match against come from the
// resolver, not from a constant. On a work-led plane the binding's own ref is
// in the set too, which is what lets a mixed-version city — old census rows
// under "" during a rollout, new ones under "class:*" — be read by one filter.
func TestAssignedWorkClaimRefsCarryTheBindingRef(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := beads.NewMemStore()

	refs := assignedWorkClaimRefs(cityPath, residencyTestConfig(), work)
	if len(refs) != 2 || refs[0] != "" || refs[1] != "class:gmnos" {
		t.Fatalf("claim refs = %#v, want the leading work arm and the binding", refs)
	}

	// The controller's shape: the leading store IS the binding, so the census
	// records ONE ref and the filter must match exactly that one.
	if got := assignedWorkClaimRefs(cityPath, residencyTestConfig(), binding); len(got) != 1 || got[0] != "" {
		t.Fatalf("claim refs on a leading-binding city = %#v, want just the leading arm", got)
	}
}

// A refused city still has claims recorded under its binding's ref, so the ref
// answer has to exist where a PLAN correctly refuses. Losing it would drain
// every claim-holder on the city the refusal is about.
func TestAssignedWorkClaimRefsSurviveARefusedCity(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	resetCLIResidencyBindings()
	t.Cleanup(resetCLIResidencyBindings)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() {
		entry.routes = refusingStorageRoutes("infra", errStorageRefusedForTest{})
	})

	refs := assignedWorkClaimRefs(cityPath, residencyTestConfig(), beads.NewMemStore())
	if len(refs) != 2 || refs[0] != "" {
		t.Fatalf("claim refs on a refused city = %#v, want the work arm and the refusing binding's ref", refs)
	}
}

// The controller must not open a SECOND binding for a city it already serves:
// a duplicate managed-Dolt server or a second sqlite writer is a worse bug than
// the blindness this slice closes. A runtime registers the routes it opened at
// boot, and the spine answers from those.
func TestRegisteredControllerRoutesAnswerResidencyWithoutASecondOpen(t *testing.T) {
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath) // the one-shot funnel says "no split"
	binding := beads.NewMemStore()
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	work := beads.NewMemStore()
	plan, err := assignedWorkSweepPlan(cityPath, residencyTestConfig(), work, nil, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work, binding) {
		t.Fatalf("sweep legs = %v, want the REGISTERED binding behind the work leg", got)
	}

	unregisterResidencyRoutes(cityPath, routes)
	plan, err = assignedWorkSweepPlan(cityPath, residencyTestConfig(), work, nil, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan after unregister: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work) {
		t.Fatalf("sweep legs after unregister = %v, want the one-shot funnel's answer back", got)
	}
}

// OWNERSHIP-SAFE UNREGISTRATION. Registration is keyed by city path, and two
// runtimes for one city overlap in production: the supervisor builds a
// replacement runtime BEFORE it learns whether it can take the controller lock,
// and a hung predecessor still holds both. A delete-by-key unregister lets the
// loser's shutdown remove a registration that is no longer its own, and the
// still-live winner's release sweeps then fall back to the one-shot funnel —
// either a second handle on the same binding root or, on a city the funnel
// cannot resolve, a binding-blind release sweep with ga-j4ob9 back and silent.
//
// Same shape as cmd_supervisor.go's unlink-only-if-ours socket removal.
func TestUnregisterResidencyRoutesOnlyDropsItsOwnRegistration(t *testing.T) {
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath) // the funnel would answer "no split" — the fallback is visible
	loser := splitRoutes(beads.NewMemStore())
	winnerBinding := beads.NewMemStore()
	winner := splitRoutes(winnerBinding)

	registerResidencyRoutes(cityPath, loser, nil)
	registerResidencyRoutes(cityPath, winner, nil)
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, winner) })

	// The loser's shutdown defer runs after the winner has registered.
	unregisterResidencyRoutes(cityPath, loser)

	work := beads.NewMemStore()
	plan, err := assignedWorkSweepPlan(cityPath, residencyTestConfig(), work, nil, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work, winnerBinding) {
		t.Fatalf("sweep legs = %v, want [work, the WINNER's binding] — a losing runtime's shutdown dropped a registration that was not its own, and the live one's release sweep is now binding-blind", got)
	}

	// And the winner's own unregister still works: ownership-safety must not
	// turn into a registration that can never be released.
	unregisterResidencyRoutes(cityPath, winner)
	plan, err = assignedWorkSweepPlan(cityPath, residencyTestConfig(), work, nil, nil)
	if err != nil {
		t.Fatalf("assignedWorkSweepPlan after the owner unregistered: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, work) {
		t.Fatalf("sweep legs = %v, want the funnel's answer back after the owner released its registration", got)
	}
}

// The other half of the blocker: registration must not happen before the
// controller lock is taken, or a replacement that loses the lock has already
// pointed the live predecessor's sweeps at a handle it is about to close.
// Asserted at the source, because the ordering is the invariant and no unit
// test can stage two supervisors.
func TestResidencyRegistrationHappensUnderTheControllerLock(t *testing.T) {
	root := repoRootForResidency(t)
	for _, tt := range []struct {
		file      string
		mustHave  bool
		rationale string
	}{
		{"cmd/gc/city_runtime.go", false, "newCityRuntime runs BEFORE the supervisor takes the controller lock; registering there hands a losing replacement the live city's spine"},
		{"cmd/gc/cmd_supervisor.go", true, "the supervisor registers only after acquireControllerLock succeeds"},
		{"cmd/gc/controller.go", true, "the standalone controller already holds the lock before it builds the runtime"},
	} {
		body, err := os.ReadFile(filepath.Join(root, tt.file))
		if err != nil {
			t.Fatalf("reading %s: %v", tt.file, err)
		}
		// A word boundary, so unregisterResidencyRoutes( is not a hit.
		if got := regexp.MustCompile(`(^|[^A-Za-z0-9_])registerResidencyRoutes\(`).Match(body); got != tt.mustHave {
			t.Errorf("%s calls registerResidencyRoutes = %v, want %v: %s", tt.file, got, tt.mustHave, tt.rationale)
		}
	}
}

func repoRootForResidency(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// FOLD-IN 4: the cross-store close gates (the cleanup-of-record lane) sit on the
// same close-then-release path this slice widened, and they were binding-blind
// on a work-led plane — invisible to the boundary census too, because they
// range a rig map rather than calling a named enumerator. A session whose
// binding-resident claim they cannot see is closed, and the retired-session
// sweep this slice taught to read the binding then releases the claim it is
// executing.
func TestCrossStoreCloseGateSeesABindingResidentClaim(t *testing.T) {
	cityPath := t.TempDir()
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	cfg := residencyTestConfig()

	sessionBead := beads.Bead{
		ID:       "gcs-1",
		Type:     sessionBeadType,
		Status:   "open",
		Metadata: map[string]string{"session_name": "test-city--worker-1"},
	}
	claim, err := binding.Create(beads.Bead{
		Title:    "graph step the live worker is executing",
		Type:     "task",
		Assignee: "test-city--worker-1",
	})
	if err != nil {
		t.Fatalf("seed the binding-resident claim: %v", err)
	}
	inProgress := "in_progress"
	if err := binding.Update(claim.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claim it: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForConfig(cityPath, cfg, beads.NewMemStore(), nil, sessionBead)
	if err != nil {
		t.Fatalf("cross-store close gate: %v", err)
	}
	if !has {
		t.Fatal("the cross-store close gate reports a live claim-holder as holding nothing; it closes the session bead and the retired-session sweep releases the claim underneath the worker")
	}
}

// A gate that asks "does this session still hold work" must fail CLOSED when a
// leg went dark: "one ledger could not be read" reported as "holds nothing" is
// how a live claim-holder gets drained.
func TestAssignedWorkGateFailsClosedOnADegradedLeg(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: "/tmp/riga"}},
		Agents:    []config.Agent{{Name: "worker", Scope: "city"}},
	}
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath)
	info := sessionInfosFromBeads([]beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "test-city--worker-1",
			"state":        "active",
		},
	}})[0]

	has, err := sessionHasOpenAssignedWorkForReachableStore(
		cityPath, cfg, beads.NewMemStore(),
		map[string]beads.Store{"riga": failingListStore{}}, info)
	if err == nil {
		t.Fatal("a rig store that could not be read reported \"no assigned work\" with no error; the drain arm reads that as a session holding nothing")
	}
	if has {
		t.Fatal("a failed scan reported assigned work")
	}
}

// failingListStore is a work leg that has gone dark: every assignee query fails.
type failingListStore struct{ beads.Store }

func (failingListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errDarkLeg{}
}

func (failingListStore) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, errDarkLeg{}
}

type errDarkLeg struct{}

func (errDarkLeg) Error() string { return "leg is dark" }

var _ sessionpkg.Info // keep the typed session import honest across edits
