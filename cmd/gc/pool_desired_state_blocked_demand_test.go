package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockedRoutedDemandFixture builds the #6207 shape in one store: a routed step
// whose claimant session is gone, held shut by an open blocker.
//
// This is the row the westeros incident spun on. It is open, it carries
// gc.routed_to, and it still carries the dead claimant's assignee, so the
// orphan-release pass captures it (appendOpenRoutedWorkUnique) and every
// downstream demand consumer sees Bead.Status "open" — mapBdStatus collapses
// bd's blocked/deferred/review/testing onto that one value, so status alone
// cannot tell this row apart from claimable work.
func blockedRoutedDemandFixture(t *testing.T, assignee string) (store beads.Store, blockerID, stepID string) {
	t.Helper()
	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "predecessor step", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	step, err := mem.Create(beads.Bead{
		Title:    "routed step with a claimant",
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: poolWakeTemplate},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if err := mem.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block step: %v", err)
	}
	return mem, blocker.ID, step.ID
}

// poolWakeTemplate is both the configured pool agent and the identity the dead
// claimant left on the row, which is what makes the row eligible for the
// wake-known-identity tier (isKnownPoolTemplate).
const poolWakeTemplate = "worker"

func poolWakeTestCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: poolWakeTemplate, MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
}

// poolDesiredForStore drives the real production chain — the census read, the
// pool-demand filter, then the traced demand computation — with no scale-check
// counts, because `bd ready` correctly reports zero demand for a blocked row.
// That asymmetry IS the defect: the count the serve side answers and the count
// the wake tier answers must agree.
func poolDesiredForStore(t *testing.T, store beads.Store, sessions ...beads.Bead) []PoolDesiredState {
	t.Helper()
	cfg := poolWakeTestCity()
	infos := sessionInfosFromBeads(sessions)
	work, workStores, workRefs, _, partial := collectAssignedWorkBeadsWithStores("", cfg, store, nil, nil, nil)
	if partial {
		t.Fatalf("collectAssignedWorkBeadsWithStores reported partial results")
	}
	wakeReady := newPoolWakeReadiness(nil, work, workStores, workRefs, io.Discard)
	poolWork := filterAssignedWorkBeadsForPoolDemand(cfg, "", store, infos, work, workRefs, wakeReady)
	return ComputePoolDesiredStatesWithDemandTraced(cfg, poolWork, infos, nil, nil, nil)
}

func wakeKnownIdentityWorkBeads(states []PoolDesiredState) []string {
	var out []string
	for _, state := range states {
		for _, req := range state.Requests {
			if req.Tier == "wake-known-identity" {
				out = append(out, req.WorkBeadID)
			}
		}
	}
	return out
}

// TestComputePoolDesiredStatesSkipsDependencyBlockedRoutedWork is the
// regression for gastownhall/gascity#6207 (parent #4114): the pool demand scan
// and the pool claim query must answer the same question about one row.
//
// A routed step with open blockers whose claimant session no longer exists
// falls through to the wake-known-identity tier, and the only status gate
// there is Bead.Status != in_progress && != open — on the COLLAPSED mapBdStatus
// value, which a dependency-blocked row satisfies. So the controller counts
// poolDesired >= 1 forever while the seat it mints runs
// `bd ready --assignee=<identity>` (internal/config/workquery.go
// assignedReadyTierCommand) and is served nothing. Measured on a live city:
// 280 idle wakes in 7h for zero claims.
//
// The control arm closes the blocker and asserts the demand comes back, which
// is what makes this a READINESS gate and not a blanket suppression of the
// wake tier: an orphaned row a woken seat could actually claim is still demand.
func TestComputePoolDesiredStatesSkipsDependencyBlockedRoutedWork(t *testing.T) {
	store, blockerID, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)

	blocked := poolDesiredForStore(t, store)
	if got := PoolDesiredCounts(blocked)[poolWakeTemplate]; got != 0 {
		t.Errorf("poolDesired[%s] = %d for a dependency-blocked routed step, want 0 — the claim query would serve no worker this row (#6207)", poolWakeTemplate, got)
	}
	if woken := wakeKnownIdentityWorkBeads(blocked); len(woken) != 0 {
		t.Errorf("wake-known-identity requests = %v, want none: step %s is blocked by open %s", woken, stepID, blockerID)
	}

	// Control arm: the blocker closes, the step becomes ready, and the orphaned
	// row is demand again.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	unblocked := poolDesiredForStore(t, store)
	if got := PoolDesiredCounts(unblocked)[poolWakeTemplate]; got != 1 {
		t.Errorf("poolDesired[%s] = %d once the blocker closed, want 1 — the gate must be readiness, not a blanket wake-tier suppression", poolWakeTemplate, got)
	}
	if woken := wakeKnownIdentityWorkBeads(unblocked); len(woken) != 1 || woken[0] != stepID {
		t.Errorf("wake-known-identity work beads = %v, want [%s]", woken, stepID)
	}
}

// livePoolSessionBead is an awake pool session bead whose runtime name is the
// identity a work bead's Assignee carries.
func livePoolSessionBead(id, sessionName string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + poolWakeTemplate},
		Metadata: map[string]string{
			"template":             poolWakeTemplate,
			"session_name":         sessionName,
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}
}

// TestComputePoolDesiredStatesKeepsBlockedWorkForLiveClaimant pins the boundary
// of the gate above: readiness decides whether to MINT a seat for abandoned
// work, never whether to keep the seat that already holds it.
//
// A live claimant's row belongs to the resume tier, whose job is to keep that
// session alive. Dropping it because its dependencies closed shut for a moment
// would drain a worker mid-claim and strand the step — the #5731 shape, and a
// far worse failure than the idle wakes this change removes. So the gate is
// scoped to rows whose assignee resolves to no open session bead.
func TestComputePoolDesiredStatesKeepsBlockedWorkForLiveClaimant(t *testing.T) {
	const sessionName = "worker-1"
	store, blockerID, stepID := blockedRoutedDemandFixture(t, sessionName)
	live := livePoolSessionBead("sess-live", sessionName)

	states := poolDesiredForStore(t, store, live)
	if got := PoolDesiredCounts(states)[poolWakeTemplate]; got != 1 {
		t.Fatalf("poolDesired[%s] = %d while session %s still holds blocked step %s (blocker %s), want 1 — the resume tier must keep a live claimant awake", poolWakeTemplate, got, sessionName, stepID, blockerID)
	}
	for _, state := range states {
		for _, req := range state.Requests {
			if req.Tier != "resume" {
				t.Errorf("request tier = %q for a live claimant, want resume", req.Tier)
			}
		}
	}
}

// readyErrorStore fails only the Ready read, modeling a backing store that can
// still be listed but cannot answer the readiness question this tick.
type readyErrorStore struct {
	beads.Store
	err error
}

var _ beads.Store = readyErrorStore{}

func (s readyErrorStore) Ready(_ ...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, s.err
}

// TestPoolWakeReadinessFailsOpenOnReadError pins the direction the gate errs
// in. A store that answered nothing is not a store with no ready work:
// suppressing demand on a failed read is how a transient hiccup drains a pool,
// which is the same correctness-over-latency contract readyDemandCache states
// for its own cached-tier backfill. One idle seat is the acceptable cost.
func TestPoolWakeReadinessFailsOpenOnReadError(t *testing.T) {
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	failing := readyErrorStore{Store: store, err: errors.New("ready read outage")}
	cfg := poolWakeTestCity()

	// The census reports partial here, as it should — the handoff probe hit the
	// same outage. That is the caller's own retention signal and not what this
	// test is about; the subject is what the gate does with an unanswerable
	// store once the rows are in hand.
	work, workStores, workRefs, _, _ := collectAssignedWorkBeadsWithStores("", cfg, failing, nil, nil, nil)
	wakeReady := newPoolWakeReadiness(nil, work, workStores, workRefs, io.Discard)
	if !wakeReady.servesWakeCandidate("", stepID) {
		t.Fatalf("servesWakeCandidate(%s) = false after a failed ready read, want fail-open true", stepID)
	}

	poolWork := filterAssignedWorkBeadsForPoolDemand(cfg, "", failing, nil, work, workRefs, wakeReady)
	if got := PoolDesiredCounts(ComputePoolDesiredStatesWithDemandTraced(cfg, poolWork, nil, nil, nil, nil))[poolWakeTemplate]; got != 1 {
		t.Fatalf("poolDesired[%s] = %d on a failed ready read, want 1 — an unanswerable store must not suppress demand", poolWakeTemplate, got)
	}
}

// TestPoolWakeReadinessKeepsAssignedMoleculeRootReadyExcludes is the guard for
// the half of the serve predicate Ready() does not carry.
//
// Ready() excludes `molecule` and `step` by type (beads.readyExcludeTypes), on
// purpose: those are workflow containers, not generic queue work. But the census
// admits an open ASSIGNED molecule root as genuine wake demand — "an assigned
// root-only wisp is the executable turn"
// (appendOpenAssignedMoleculeWorkUnique) — and the legacy assigned-ready probe
// serves it through `bd query` with a selector that excludes epics and nothing
// else (config.ephemeralAssignedReadyProbeScript). So for such a row, absence
// from the ready frontier means "not a candidate for this query", never
// "blocked", and vetoing it would delete legitimate demand rather than gate it.
func TestPoolWakeReadinessKeepsAssignedMoleculeRootReadyExcludes(t *testing.T) {
	mem := beads.NewMemStore()
	root, err := mem.Create(beads.Bead{
		Title:    "assigned workflow root with a dead claimant",
		Type:     "molecule",
		Status:   "open",
		Assignee: poolWakeTemplate,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
			beadmeta.RoutedToMetadataKey: poolWakeTemplate,
		},
	})
	if err != nil {
		t.Fatalf("create molecule root: %v", err)
	}
	// Nothing blocks it; it is absent from Ready() purely because of its type.
	ready, err := mem.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == root.ID {
			t.Fatalf("fixture invalid: Ready() returned molecule root %s, so this test would not exercise the type exclusion", root.ID)
		}
	}

	states := poolDesiredForStore(t, mem)
	if got := PoolDesiredCounts(states)[poolWakeTemplate]; got != 1 {
		t.Fatalf("poolDesired[%s] = %d for an unblocked assigned molecule root, want 1 — Ready() excludes molecule by TYPE, so its absence is not evidence the row is blocked", poolWakeTemplate, got)
	}
}

// TestPoolWakeReadinessKeepsRowForAgentWithCustomWorkQuery covers the other
// structural escape: an agent that supplies its own work_query has replaced the
// default assigned-ready tier verbatim (config.Agent.effectiveQuery reads
// queryTable[queryAssignedReady].override = a.WorkQuery), so `bd ready` is not
// the question that seat will ask and this gate has no standing to answer for
// it.
func TestPoolWakeReadinessKeepsRowForAgentWithCustomWorkQuery(t *testing.T) {
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	cfg := poolWakeTestCity()
	cfg.Agents[0].WorkQuery = `bd list --status=open --json`

	work, workStores, workRefs, _, partial := collectAssignedWorkBeadsWithStores("", cfg, store, nil, nil, nil)
	if partial {
		t.Fatalf("collectAssignedWorkBeadsWithStores reported partial results")
	}
	wakeReady := newPoolWakeReadiness(nil, work, workStores, workRefs, io.Discard)
	poolWork := filterAssignedWorkBeadsForPoolDemand(cfg, "", store, nil, work, workRefs, wakeReady)
	if got := PoolDesiredCounts(ComputePoolDesiredStatesWithDemandTraced(cfg, poolWork, nil, nil, nil, nil))[poolWakeTemplate]; got != 1 {
		t.Fatalf("poolDesired[%s] = %d for a template with a custom work_query, want 1 — blocked step %s may well be servable by that query", poolWakeTemplate, got, stepID)
	}
}

// providerDeadPoolSessionBead is an open pool session bead the pool has already
// written off with a terminal provider error.
func providerDeadPoolSessionBead(id, sessionName string) beads.Bead {
	bead := livePoolSessionBead(id, sessionName)
	bead.Metadata[sessionProviderTerminalErrorMetadataKey] = "provider refused the session"
	return bead
}

// TestComputePoolDesiredStatesGatesBlockedWorkHeldByProviderDeadSession closes
// the eligibility hole: computePoolDesiredStatesAt skips a session carrying a
// terminal provider error when it builds its own claimant map, so such a row IS
// orphaned as far as the wake tier is concerned. The veto must use the same
// eligibility, or the row slips past the gate and becomes wake demand anyway.
func TestComputePoolDesiredStatesGatesBlockedWorkHeldByProviderDeadSession(t *testing.T) {
	// The assignee is the template's own identity so the row is genuinely
	// wake-eligible downstream (isKnownPoolTemplate); with a live claimant this
	// exact fixture produces demand, so a 0 here can only come from the veto.
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	dead := providerDeadPoolSessionBead("sess-dead", poolWakeTemplate)

	states := poolDesiredForStore(t, store, dead)
	if got := PoolDesiredCounts(states)[poolWakeTemplate]; got != 0 {
		t.Fatalf("poolDesired[%s] = %d for blocked step %s held by a provider-dead session, want 0 — the wake tier does not count that session as a claimant either", poolWakeTemplate, got, stepID)
	}
}

// TestPoolWakeReadinessIsStoreScoped proves the verdict cannot be crossed
// between stores. Two independent MemStores mint the same bead IDs, which is
// exactly the collision storeScopedBeadKey exists for: a ready row in the rig
// store must not vouch for a blocked row with the same ID in the city store.
func TestPoolWakeReadinessIsStoreScoped(t *testing.T) {
	cityStore, _, blockedID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{Title: "filler", Type: "task", Status: "open"}); err != nil {
		t.Fatalf("create filler: %v", err)
	}
	readyTwin, err := rigStore.Create(beads.Bead{
		Title:    "ready twin in another store",
		Type:     "task",
		Status:   "open",
		Assignee: poolWakeTemplate,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: poolWakeTemplate},
	})
	if err != nil {
		t.Fatalf("create ready twin: %v", err)
	}
	if readyTwin.ID != blockedID {
		t.Fatalf("fixture invalid: twin ID %s != blocked ID %s, so no collision is exercised", readyTwin.ID, blockedID)
	}

	blocked, err := cityStore.Get(blockedID)
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	wakeReady := newPoolWakeReadiness(
		nil,
		[]beads.Bead{blocked, readyTwin},
		[]beads.Store{cityStore, rigStore},
		[]string{"", "rig:fixture"},
		io.Discard,
	)
	if wakeReady.servesWakeCandidate("", blockedID) {
		t.Errorf("city row %s read as served; the rig store's ready twin must not vouch for it", blockedID)
	}
	if !wakeReady.servesWakeCandidate("rig:fixture", readyTwin.ID) {
		t.Errorf("rig row %s read as unserved; it is genuinely ready in its own store", readyTwin.ID)
	}
}

// poolWakeBuilderCity is poolWakeTestCity with the start command the pool path
// needs to actually materialize a session, so the builder-level assertions can
// be made on planned desired state rather than on an internal slice.
func poolWakeBuilderCity() *config.City {
	cfg := poolWakeTestCity()
	cfg.Agents[0].StartCommand = "true"
	return cfg
}

func poolSessionsPlannedFor(state map[string]TemplateParams, template string) []string {
	var out []string
	for name, tp := range state {
		if tp.TemplateName == template {
			out = append(out, name)
		}
	}
	return out
}

// TestBuildDesiredStateWithholdsPoolSessionForBlockedOrphanedRoutedWork is the
// builder-level arm: it drives buildDesiredStateWithSessionBeads, which is where
// the verdict is CONSTRUCTED and HANDED to the pool-demand filter. The unit
// tests above build that verdict themselves, so deleting either half of the
// production handoff would leave them green; this one goes red for both.
//
// The control arm closes the blocker and rebuilds, so the assertion is that the
// builder withholds the seat for unclaimable work specifically, not that it
// withholds seats.
func TestBuildDesiredStateWithholdsPoolSessionForBlockedOrphanedRoutedWork(t *testing.T) {
	cityPath := t.TempDir()
	store, blockerID, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	cfg := poolWakeBuilderCity()

	got := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if got.PoolWakeReadiness == nil {
		t.Fatalf("DesiredStateResult.PoolWakeReadiness = nil — the builder computed no verdict for a store holding wake candidates, so the gate is not wired")
	}
	if got.PoolWakeReadiness.servesWakeCandidate("", stepID) {
		t.Fatalf("builder verdict reports blocked step %s as served", stepID)
	}
	if planned := poolSessionsPlannedFor(got.State, poolWakeTemplate); len(planned) != 0 {
		t.Fatalf("desired state planned %v for %s while its only work (step %s) is blocked by open %s, want none", planned, poolWakeTemplate, stepID, blockerID)
	}

	// Control arm: the blocker closes, the step becomes claimable, and the
	// builder plans the seat.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	unblocked := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if planned := poolSessionsPlannedFor(unblocked.State, poolWakeTemplate); len(planned) == 0 {
		t.Fatalf("desired state planned nothing for %s once step %s became claimable, want a seat — the gate must be readiness, not a blanket withhold", poolWakeTemplate, stepID)
	}
}

// TestBuildDesiredStateFailsOpenWhenWakeReadinessIsUnanswerable is the
// builder-level partial arm: a store that cannot answer Ready must yield NO
// verdict at all, so the filter keeps every row it was handed.
func TestBuildDesiredStateFailsOpenWhenWakeReadinessIsUnanswerable(t *testing.T) {
	cityPath := t.TempDir()
	store, _, _ := blockedRoutedDemandFixture(t, poolWakeTemplate)
	failing := readyErrorStore{Store: store, err: errors.New("ready read outage")}

	got := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), poolWakeBuilderCity(), runtime.NewFake(), failing, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if got.PoolWakeReadiness != nil {
		t.Fatalf("DesiredStateResult.PoolWakeReadiness = %+v on a store that cannot answer Ready, want nil — an unanswerable store must veto nothing", got.PoolWakeReadiness)
	}
}

// deferredRoutedDemandFixture builds the state gastownhall/gascity#6207 actually
// reported: a routed bead with a dead claimant parked by `bd defer`, with NO
// blocking dependency at all. indefinite selects the reporter's first variant
// (status=deferred, defer_until absent); otherwise the row carries an ELAPSED
// defer_until, which is the variant that survives #5094's IsDeferred gate.
//
// Serve-side truth, measured against bd 1.1.1-0.20260805093327-bf97b73749ac on a
// throwaway store: `bd ready --assignee=<id> --json --limit=1` returns NEITHER
// variant — nor does `bd ready --include-deferred`, whose flag relaxes only a
// future timestamp. Both are unclaimable; the demand side must agree.
func deferredRoutedDemandFixture(t *testing.T, indefinite bool) (beads.Store, string) {
	t.Helper()
	mem := beads.NewMemStore()
	row := beads.Bead{
		Title:    "routed step parked by bd defer",
		Type:     "task",
		Status:   "open", // mapBdStatus has already collapsed bd's "deferred" to this
		Assignee: poolWakeTemplate,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: poolWakeTemplate},
	}
	if indefinite {
		row.IndefinitelyDeferred = true
	} else {
		elapsed := time.Now().UTC().Add(-24 * time.Hour)
		row.DeferUntil = &elapsed
	}
	created, err := mem.Create(row)
	if err != nil {
		t.Fatalf("create deferred row: %v", err)
	}
	got, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("get deferred row: %v", err)
	}
	if !beads.CarriesDeferral(got) {
		t.Fatalf("fixture invalid: stored row carries no deferral marker: %+v", got)
	}
	return mem, created.ID
}

// TestBuildDesiredStateWithholdsPoolSessionForDeferredRoutedWork is #6207's own
// reproduction, driven through the builder for both variants the issue names.
//
// The elapsed variant is the one that needed this change. #5094's gate drops
// only a bead IsDeferred says is hidden RIGHT NOW, and its companion test keeps
// an elapsed one on purpose; mapBdStatus spells the row "open"; and the ready
// frontier then AGREES it is ready — MemStore and the cached tier recompute it,
// NativeDoltStore.Ready deliberately resurfaces an expired time-bound deferral.
// So the frontier could not be the oracle here and the deferral is read off the
// bead instead (beads.CarriesDeferral), which is where bd's own answer lives.
func TestBuildDesiredStateWithholdsPoolSessionForDeferredRoutedWork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		indefinite bool
	}{
		{"defer_until absent", true},
		{"defer_until elapsed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, rowID := deferredRoutedDemandFixture(t, tc.indefinite)
			// The frontier's own answer, recorded so the test says WHY each
			// variant is withheld rather than just that it is.
			ready, err := store.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
			if err != nil {
				t.Fatalf("ready: %v", err)
			}
			inFrontier := false
			for _, b := range ready {
				if b.ID == rowID {
					inFrontier = true
				}
			}
			if tc.indefinite && inFrontier {
				t.Fatalf("fixture invalid: an indefinitely deferred row is in the ready frontier")
			}
			if !tc.indefinite && !inFrontier {
				t.Fatalf("fixture invalid: the elapsed-defer row is absent from the ready frontier, so this case would pass without the deferral predicate")
			}

			got := buildDesiredStateWithSessionBeads(
				"test-city", t.TempDir(), time.Now().UTC(), poolWakeBuilderCity(), runtime.NewFake(), store, nil,
				newSessionBeadSnapshot(nil), nil, io.Discard,
			)
			if planned := poolSessionsPlannedFor(got.State, poolWakeTemplate); len(planned) != 0 {
				t.Fatalf("desired state planned %v for %s while its only work (%s) is deferred, want none — `bd ready --assignee` refuses a deferred row at any timestamp (#6207)", planned, poolWakeTemplate, rowID)
			}
		})
	}
}

// TestPoolWakeReadinessDisarmsOnMisalignedSnapshot pins the early return: an
// index-aligned snapshot is the only thing that gives a row usable provenance,
// so a short or mismatched slice must yield NO verdict rather than a verdict
// keyed on the wrong store. Silently disabling the gate is the correct
// behavior here; silently MIS-applying it is not.
func TestPoolWakeReadinessDisarmsOnMisalignedSnapshot(t *testing.T) {
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	blocked, err := store.Get(stepID)
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	work := []beads.Bead{blocked}

	for _, tc := range []struct {
		name   string
		stores []beads.Store
		refs   []string
	}{
		{"stores short", nil, []string{""}},
		{"refs short", []beads.Store{store}, nil},
		{"refs longer than work", []beads.Store{store}, []string{"", "rig:extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newPoolWakeReadiness(nil, work, tc.stores, tc.refs, io.Discard); got != nil {
				t.Fatalf("newPoolWakeReadiness = %+v on a misaligned snapshot, want nil", got)
			}
		})
	}
}

// TestPoolWakeReadinessVerifiesStoreFromLaterRowWithALeg pins the nil-store
// ordering. A row with no leg must be skipped WITHOUT consuming its ref, so a
// later row from the same store that does carry one still verifies it —
// otherwise one legless row silently disarms the gate for its whole store.
func TestPoolWakeReadinessVerifiesStoreFromLaterRowWithALeg(t *testing.T) {
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	blocked, err := store.Get(stepID)
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	legless := blocked
	legless.ID = "legless-" + blocked.ID

	got := newPoolWakeReadiness(
		nil,
		[]beads.Bead{legless, blocked},
		[]beads.Store{nil, store},
		[]string{"", ""},
		io.Discard,
	)
	if got == nil {
		t.Fatalf("newPoolWakeReadiness = nil; the legless first row consumed the ref and disarmed the gate for its store")
	}
	if got.servesWakeCandidate("", stepID) {
		t.Fatalf("blocked row %s read as served after the store was verified by the later row", stepID)
	}
}

// partialReadyStore answers Ready with rows AND a PartialResultError — the shape
// controllerDemandReady hands back when a store could only half-answer.
type partialReadyStore struct {
	beads.Store
}

var _ beads.Store = partialReadyStore{}

func (s partialReadyStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.Store.Ready(query...)
	if err != nil {
		return rows, err
	}
	return rows, &beads.PartialResultError{Op: "ready", Err: errors.New("one leg did not answer")}
}

// TestPoolWakeReadinessDeclinesPartialReadyResult pins the partial branch: rows
// plus a PartialResultError is a HALF answer, and half a frontier cannot prove a
// row absent. The store stays unverified, the gate stays open there, and the
// operator is told on stderr.
func TestPoolWakeReadinessDeclinesPartialReadyResult(t *testing.T) {
	store, _, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	blocked, err := store.Get(stepID)
	if err != nil {
		t.Fatalf("get blocked: %v", err)
	}
	var stderr bytes.Buffer

	got := newPoolWakeReadiness(
		newReadyDemandCache(),
		[]beads.Bead{blocked},
		[]beads.Store{partialReadyStore{Store: store}},
		[]string{""},
		&stderr,
	)
	if got != nil {
		t.Fatalf("newPoolWakeReadiness = %+v on a partial ready result, want nil — a half-read frontier proves nothing absent", got)
	}
	if !strings.Contains(stderr.String(), "poolWakeReadiness: PARTIAL") {
		t.Fatalf("stderr = %q, want a PARTIAL line naming the store — an inert gate must be visible", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"city"`) {
		t.Fatalf("stderr = %q, want the city store named", stderr.String())
	}
}
