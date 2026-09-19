package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
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

// poolWakeSessionsPlanned lists the sessions desired state plans for the pool
// wake template.
func poolWakeSessionsPlanned(state map[string]TemplateParams) []string {
	var out []string
	for name, tp := range state {
		if tp.TemplateName == poolWakeTemplate {
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

	var stderr bytes.Buffer
	got := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, &stderr,
	)
	if got.PoolWakeReadiness == nil {
		t.Fatalf("DesiredStateResult.PoolWakeReadiness = nil — the builder computed no verdict for a store holding wake candidates, so the gate is not wired")
	}
	if got.PoolWakeReadiness.servesWakeCandidate("", stepID) {
		t.Fatalf("builder verdict reports blocked step %s as served", stepID)
	}
	if planned := poolWakeSessionsPlanned(got.State); len(planned) != 0 {
		t.Fatalf("desired state planned %v for %s while its only work (step %s) is blocked by open %s, want none", planned, poolWakeTemplate, stepID, blockerID)
	}
	if records := withheldRecords(stderr.String()); len(records) != 1 || !strings.Contains(records[0], stepID) {
		t.Fatalf("builder WITHHELD records = %q, want exactly one naming step %s", records, stepID)
	}

	// Control arm: the blocker closes, the step becomes claimable, and the
	// builder plans the seat.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	stderr.Reset()
	unblocked := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, &stderr,
	)
	if planned := poolWakeSessionsPlanned(unblocked.State); len(planned) == 0 {
		t.Fatalf("desired state planned nothing for %s once step %s became claimable, want a seat — the gate must be readiness, not a blanket withhold", poolWakeTemplate, stepID)
	}
	if records := withheldRecords(stderr.String()); len(records) != 0 {
		t.Fatalf("builder WITHHELD records = %q for a claimable step, want none", records)
	}
}

// withheldRecords returns the lines of out that report a row the wake
// readiness gate withheld.
func withheldRecords(out string) []string {
	var records []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "poolWakeReadiness: WITHHELD") {
			records = append(records, line)
		}
	}
	return records
}

// TestPoolWakeReadinessNamesEachWithheldRowOnce pins the evidence a withheld row
// leaves. The fail-open path reports on stderr, so the withhold path reports on
// the same channel: without a record, a pool that stops scaling looks the same
// as a pool with no work. Every consumer that recomputes pool demand from one
// DesiredStateResult hands the filter the same verdict, so the row is named once
// per verdict, not once per recomputation. A row the gate keeps is not named.
func TestPoolWakeReadinessNamesEachWithheldRowOnce(t *testing.T) {
	store, blockerID, stepID := blockedRoutedDemandFixture(t, poolWakeTemplate)
	cfg := poolWakeTestCity()
	var stderr bytes.Buffer

	work, workStores, workRefs, _, partial := collectAssignedWorkBeadsWithStores("", cfg, store, nil, nil, nil)
	if partial {
		t.Fatalf("collectAssignedWorkBeadsWithStores reported partial results")
	}
	wakeReady := newPoolWakeReadiness(nil, work, workStores, workRefs, &stderr)
	for pass := 1; pass <= 2; pass++ {
		if kept := filterAssignedWorkBeadsForPoolDemand(cfg, "", store, nil, work, workRefs, wakeReady); len(kept) != 0 {
			t.Fatalf("pass %d kept %v, want blocked step %s withheld", pass, kept, stepID)
		}
	}
	records := withheldRecords(stderr.String())
	if len(records) != 1 {
		t.Fatalf("WITHHELD records = %q after two passes over one verdict, want exactly one", records)
	}
	for _, want := range []string{stepID, `"city"`, poolWakeTemplate} {
		if !strings.Contains(records[0], want) {
			t.Fatalf("WITHHELD record = %q, want it to name %s", records[0], want)
		}
	}

	// Control arm: the blocker closes, a fresh verdict keeps the step, and
	// nothing is reported.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	stderr.Reset()
	work, workStores, workRefs, _, _ = collectAssignedWorkBeadsWithStores("", cfg, store, nil, nil, nil)
	wakeReady = newPoolWakeReadiness(nil, work, workStores, workRefs, &stderr)
	// The census can capture a ready orphan twice (the open-routed pass and the
	// ready handoff probe); demand dedupes per template, so only presence counts.
	kept := filterAssignedWorkBeadsForPoolDemand(cfg, "", store, nil, work, workRefs, wakeReady)
	if !slices.ContainsFunc(kept, func(b beads.Bead) bool { return b.ID == stepID }) {
		t.Fatalf("kept %v once the blocker closed, want step %s", kept, stepID)
	}
	if records := withheldRecords(stderr.String()); len(records) != 0 {
		t.Fatalf("WITHHELD records = %q for a claimable step, want none", records)
	}
}

// TestPoolWakeReadinessDoesNotNameAnUnreachableRow keeps the record to rows the
// gate removed. A row in a store its pool agent cannot reach is dropped by the
// reachability check whatever the frontier says, so naming it as withheld would
// point an operator at the wrong cause.
func TestPoolWakeReadinessDoesNotNameAnUnreachableRow(t *testing.T) {
	const template = "riga/" + poolWakeTemplate
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: filepath.Join(cityPath, "riga")}},
		Agents:    []config.Agent{{Name: poolWakeTemplate, Dir: "riga", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "predecessor step", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	step, err := mem.Create(beads.Bead{
		Title:    "blocked step in the city store, routed to a rig pool",
		Type:     "task",
		Status:   "open",
		Assignee: template,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if err := mem.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block step: %v", err)
	}
	step, err = mem.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}

	var stderr bytes.Buffer
	work := []beads.Bead{step}
	wakeReady := newPoolWakeReadiness(nil, work, []beads.Store{mem}, []string{""}, &stderr)
	agentCfg := findAgentByTemplate(cfg, template)
	if agentCfg == nil {
		t.Fatalf("fixture invalid: no agent resolves %q", template)
	}
	if !wakeReady.vetoesWakeCandidate(step, agentCfg, false, "") {
		t.Fatalf("fixture invalid: the gate would keep blocked step %s, so this test cannot tell a veto from a drop", step.ID)
	}

	if kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, mem, nil, work, []string{""}, wakeReady); len(kept) != 0 {
		t.Fatalf("kept %v, want step %s dropped: its store is not reachable from %s", kept, step.ID, template)
	}
	if records := withheldRecords(stderr.String()); len(records) != 0 {
		t.Fatalf("WITHHELD records = %q for a row the reachability check drops, want none", records)
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

// deferredRoutedDemandFixture builds a routed bead with a dead claimant, a
// deferral, and NO blocking dependency at all. indefinite selects bd's undated
// `bd defer` (status=deferred, defer_until absent); otherwise the row carries a
// defer_until that has already ELAPSED.
//
// The serve side answers the two variants differently. bd's ready read
// (beads v1.3.0-rc.2 internal/storage/sqlbuild/ready.go, BuildReadyWorkWhere)
// keeps `status IN ('open', 'in_progress')` and
// `(defer_until IS NULL OR defer_until <= UTC_TIMESTAMP())`, and GetReadyWork
// first runs wakeExpiredDefers, which returns an elapsed DATED defer to open.
// So the undated variant is never served, while the elapsed one is served
// whichever way it was deferred.
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
	if indefinite && !got.IndefinitelyDeferred {
		t.Fatalf("fixture invalid: stored row lost its undated deferral: %+v", got)
	}
	if !indefinite && (got.DeferUntil == nil || got.DeferUntil.After(time.Now())) {
		t.Fatalf("fixture invalid: stored row carries no elapsed defer_until: %+v", got)
	}
	return mem, created.ID
}

// readyFrontierHas reports whether an unfiltered Ready read of store returns id.
func readyFrontierHas(t *testing.T, store beads.Store, id string) bool {
	t.Helper()
	ready, err := store.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TestBuildDesiredStateWithholdsPoolSessionForIndefinitelyDeferredRoutedWork is
// #6207's undated `bd defer` variant, driven through the builder. bd never serves
// that row, and #5094's IsDeferred filter withholds it before the readiness gate
// runs.
func TestBuildDesiredStateWithholdsPoolSessionForIndefinitelyDeferredRoutedWork(t *testing.T) {
	store, rowID := deferredRoutedDemandFixture(t, true)
	if readyFrontierHas(t, store, rowID) {
		t.Fatalf("fixture invalid: an indefinitely deferred row is in the ready frontier")
	}

	got := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), poolWakeBuilderCity(), runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if planned := poolWakeSessionsPlanned(got.State); len(planned) != 0 {
		t.Fatalf("desired state planned %v for %s while its only work (%s) is deferred with no date, want none (#6207)", planned, poolWakeTemplate, rowID)
	}
}

// TestBuildDesiredStateKeepsPoolSessionForElapsedDeferRoutedWork is the
// builder-level anti-starvation arm. An orphaned routed row with no blocker whose
// defer_until has ELAPSED is served by the seat's own read (see
// deferredRoutedDemandFixture), so the gate must not withhold it. defer_until is
// cleared only by a reopen or a wake, so a withheld row would be withheld for
// good.
func TestBuildDesiredStateKeepsPoolSessionForElapsedDeferRoutedWork(t *testing.T) {
	store, rowID := deferredRoutedDemandFixture(t, false)
	if !readyFrontierHas(t, store, rowID) {
		t.Fatalf("fixture invalid: the elapsed-defer row is absent from the ready frontier, so the gate would withhold it for a reason other than its deferral")
	}

	got := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now().UTC(), poolWakeBuilderCity(), runtime.NewFake(), store, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if got.PoolWakeReadiness == nil {
		t.Fatalf("DesiredStateResult.PoolWakeReadiness = nil, want an armed gate — without one this test cannot fail")
	}
	if planned := poolWakeSessionsPlanned(got.State); len(planned) == 0 {
		t.Fatalf("desired state planned nothing for %s while its only work (%s) has an elapsed defer_until and no blocker, want a seat: the seat's ready read serves that row", poolWakeTemplate, rowID)
	}
}

// TestBuildDesiredStateWithholdsPoolSessionForBlockedOutcomeOrphanBdReadyWouldServe
// pins the ONE case where this gate is deliberately stricter than the query the
// seat it would mint actually runs.
//
// Every Ready() backend drops a row whose blocking dependency closed with
// gc.work_outcome=blocked, and the store conformance suite requires it
// (internal/beads/beadstest/conformance.go, ADR-0009: such a close does not
// satisfy the dependency). Raw `bd ready` does not know that key, so on a
// single-store city — where the seat's assigned-ready tier IS raw `bd ready` —
// the seat would be served this row, and gc hook would not filter it out either.
//
// Withholding is still the answer this gate gives, on purpose: it is what
// Ready() means, and the unassigned arm already answers the same way for the
// same row, so the two arms agree rather than this gate inventing a rule. See
// vetoesWakeCandidate for the full reasoning and the evidence on both sides.
//
// If this test ever flips — a seat IS planned — the frontier was made
// serve-equivalent per topology (bead gc-zkcd) and that doc comment is stale.
func TestBuildDesiredStateWithholdsPoolSessionForBlockedOutcomeOrphanBdReadyWouldServe(t *testing.T) {
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath)
	cfg := poolWakeBuilderCity()
	topo := cityQueryTopology(cityPath, cfg)
	if topo.FederatedReady {
		t.Fatalf("fixture invalid: the city reads claimable work through the federated reader")
	}
	if q := cfg.Agents[0].EffectiveAssignedReadyQueryFor(topo); !strings.Contains(q, "bd ready") {
		t.Fatalf("fixture invalid: the seat's assigned-ready tier is not raw bd ready: %s", q)
	}

	mem := beads.NewMemStore()
	blocker, err := mem.Create(beads.Bead{Title: "predecessor that closed blocked", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	step, err := mem.Create(beads.Bead{
		Title:    "routed step behind a blocked-outcome close",
		Type:     "task",
		Status:   "open",
		Assignee: poolWakeTemplate,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: poolWakeTemplate},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if err := mem.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block step: %v", err)
	}
	if err := mem.Close(blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	if err := mem.SetMetadataBatch(blocker.ID, map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}); err != nil {
		t.Fatalf("record blocked outcome: %v", err)
	}
	if readyFrontierHas(t, mem, step.ID) {
		t.Fatalf("fixture invalid: Ready() returned step %s, so the gate would not veto it", step.ID)
	}

	got := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), mem, nil,
		newSessionBeadSnapshot(nil), nil, io.Discard,
	)
	if planned := poolWakeSessionsPlanned(got.State); len(planned) != 0 {
		t.Fatalf("desired state planned %v for %s while its only work (%s) sits behind a blocked-outcome close, want none — Ready() does not serve that row, so the gate withholds it even though this city's seats read with raw `bd ready` and would be served it (gc-zkcd)", planned, poolWakeTemplate, step.ID)
	}
}

// TestPoolWakeReadinessKeepsDemandForElapsedDeferral is the anti-starvation
// direction of the deferral read, with the gate armed: an orphaned row with no
// blocker whose defer_until has ELAPSED stays wake demand. The molecule root
// case covers the ready-excluded carve-out, which Ready() cannot vouch for by
// type, so nothing in the gate may veto that row for its deferral either.
//
// A row hidden right now is withheld earlier by #5094's IsDeferred filter, so
// neither gc hook (isFutureDeferredHookCandidate) nor bd's ready read has any
// deferral left to refuse at this point.
func TestPoolWakeReadinessKeepsDemandForElapsedDeferral(t *testing.T) {
	elapsed := time.Now().UTC().Add(-24 * time.Hour)
	for _, tc := range []struct {
		name string
		row  beads.Bead
	}{
		{"routed task", beads.Bead{
			Title:      "routed step whose defer elapsed",
			Type:       "task",
			Status:     "open",
			Assignee:   poolWakeTemplate,
			Metadata:   map[string]string{beadmeta.RoutedToMetadataKey: poolWakeTemplate},
			DeferUntil: &elapsed,
		}},
		{"assigned molecule root", beads.Bead{
			Title:    "assigned workflow root whose defer elapsed",
			Type:     "molecule",
			Status:   "open",
			Assignee: poolWakeTemplate,
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
				beadmeta.RoutedToMetadataKey: poolWakeTemplate,
			},
			DeferUntil: &elapsed,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			row, err := mem.Create(tc.row)
			if err != nil {
				t.Fatalf("create row: %v", err)
			}
			cfg := poolWakeTestCity()
			work, workStores, workRefs, _, partial := collectAssignedWorkBeadsWithStores("", cfg, mem, nil, nil, nil)
			if partial {
				t.Fatalf("collectAssignedWorkBeadsWithStores reported partial results")
			}
			wakeReady := newPoolWakeReadiness(nil, work, workStores, workRefs, io.Discard)
			if wakeReady == nil || !wakeReady.verified[""] {
				t.Fatalf("newPoolWakeReadiness = %+v, want a verdict armed for the city store — without one this test cannot fail", wakeReady)
			}

			poolWork := filterAssignedWorkBeadsForPoolDemand(cfg, "", mem, nil, work, workRefs, wakeReady)
			states := ComputePoolDesiredStatesWithDemandTraced(cfg, poolWork, nil, nil, nil, nil)
			if got := PoolDesiredCounts(states)[poolWakeTemplate]; got < 1 {
				t.Fatalf("poolDesired[%s] = %d for orphaned row %s with an elapsed defer_until and no blocker, want >= 1: the seat's ready read serves it", poolWakeTemplate, got, row.ID)
			}
			if woken := wakeKnownIdentityWorkBeads(states); len(woken) != 1 || woken[0] != row.ID {
				t.Fatalf("wake-known-identity work beads = %v, want [%s]", woken, row.ID)
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
