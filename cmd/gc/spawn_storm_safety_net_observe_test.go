package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Acceptance scenarios for the wiring layer that sits between the
// session reconciler and the safety net. These tests run with a real
// in-memory store so the "did this worker ever claim?" probe is
// exercised end-to-end.

func TestObserveSessionStopped_NoSafetyNet_Noop(t *testing.T) {
	// With no safety net registered, the observation must be a clean
	// no-op even with a store full of session-relevant beads.
	store := beads.NewMemStore()
	session := buildSessionBeadForSafetyNet(t, store, "worker-pool-1", true)
	now := time.Now().UTC()
	var buf bytes.Buffer
	observeSessionStoppedForSafetyNet("", session, "worker-pool-1", "foundations/worker", store, nil, now, &buf)
	if buf.Len() != 0 {
		t.Fatalf("stderr non-empty with no safety net registered: %s", buf.String())
	}
}

func TestObserveSessionStopped_NonPoolSession_Skipped(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1, // trigger on a single drain to make detection easy
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	// Non-pool session: no pool_managed metadata, no pool_slot, not ephemeral.
	session := buildSessionBeadForSafetyNet(t, store, "mayor-fixed", false)
	now := time.Now().UTC()
	observeSessionStoppedForSafetyNet("", session, "mayor-fixed", "foundations/mayor", store, nil, now, io.Discard)
	if sn.IsThrottled("foundations/mayor", now.Add(1*time.Minute)) {
		t.Fatal("non-pool session caused throttle: want non-pool drain to be ignored")
	}
}

func TestObserveSessionStopped_PoolWorkerWithoutClaim_DrivesDetection(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	// Three pool workers that drained without claiming any bead.
	for i := 0; i < 3; i++ {
		name := workerNameForIndex(i)
		session := buildSessionBeadForSafetyNet(t, store, name, true)
		observeSessionStoppedForSafetyNet("", session, name, "foundations/worker", store, nil, now.Add(time.Duration(i)*30*time.Second), io.Discard)
	}
	if !sn.IsThrottled("foundations/worker", now.Add(5*time.Minute)) {
		t.Fatal("safety net not throttled after threshold of drain-without-claim")
	}
}

func TestObserveSessionStopped_PoolWorkerWithClaim_DoesNotTrigger(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	// Three pool workers that each claimed and closed a bead.
	for i := 0; i < 3; i++ {
		name := workerNameForIndex(i)
		session := buildSessionBeadForSafetyNet(t, store, name, true)
		// Simulate the worker having claimed and closed a routed bead.
		workBead, err := store.Create(beads.Bead{
			Title:    "claimed routed work",
			Type:     "task",
			Status:   "closed",
			Assignee: name,
			Metadata: map[string]string{"gc.routed_to": "foundations/worker"},
		})
		if err != nil {
			t.Fatalf("create work bead: %v", err)
		}
		_ = workBead
		observeSessionStoppedForSafetyNet("", session, name, "foundations/worker", store, nil, now.Add(time.Duration(i)*30*time.Second), io.Discard)
	}
	if sn.IsThrottled("foundations/worker", now.Add(5*time.Minute)) {
		t.Fatal("healthy churn (workers claimed+closed) caused throttle")
	}
}

func TestSessionEverClaimed_DetectsClosedBeads(t *testing.T) {
	// The signal must survive the worker closing its bead. Without
	// IncludeClosed, this would return false for every successful
	// worker — false positive avalanche.
	store := beads.NewMemStore()
	session := buildSessionBeadForSafetyNet(t, store, "worker-a", true)
	if _, err := store.Create(beads.Bead{
		Title:    "claimed and closed",
		Type:     "task",
		Status:   "closed",
		Assignee: "worker-a",
		Metadata: map[string]string{"gc.routed_to": "foundations/worker"},
	}); err != nil {
		t.Fatalf("create closed bead: %v", err)
	}
	claimed, err := sessionEverClaimedAnyWork(store, nil, session)
	if err != nil {
		t.Fatalf("sessionEverClaimedAnyWork: %v", err)
	}
	if !claimed {
		t.Fatal("sessionEverClaimedAnyWork = false, want true (closed bead with matching assignee)")
	}
}

func TestSessionEverClaimed_NoMatchingBeads_ReturnsFalse(t *testing.T) {
	store := beads.NewMemStore()
	session := buildSessionBeadForSafetyNet(t, store, "worker-ghost", true)
	// Bead with a DIFFERENT assignee — must not count.
	if _, err := store.Create(beads.Bead{
		Title:    "claimed by someone else",
		Type:     "task",
		Status:   "closed",
		Assignee: "worker-real",
		Metadata: map[string]string{"gc.routed_to": "foundations/worker"},
	}); err != nil {
		t.Fatalf("create unrelated bead: %v", err)
	}
	claimed, err := sessionEverClaimedAnyWork(store, nil, session)
	if err != nil {
		t.Fatalf("sessionEverClaimedAnyWork: %v", err)
	}
	if claimed {
		t.Fatal("sessionEverClaimedAnyWork = true, want false (no beads with this session as assignee)")
	}
}

func TestObserveSessionStopped_NewStorm_SendsMail(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	session := buildSessionBeadForSafetyNet(t, store, "worker-mailtest", true)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

	observeSessionStoppedForSafetyNet("", session, "worker-mailtest", "foundations/worker", store, nil, now, io.Discard)

	// Mailer must have produced a message bead for mayor.
	mailBeads, err := store.List(beads.ListQuery{Type: "message", Assignee: spawnStormMayorRecipient, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list mail beads: %v", err)
	}
	if len(mailBeads) != 1 {
		t.Fatalf("mayor inbox = %d messages, want 1", len(mailBeads))
	}
	if !strings.Contains(mailBeads[0].Title, "Spawn-storm") {
		t.Fatalf("mail title = %q, want spawn-storm subject", mailBeads[0].Title)
	}
	if !strings.Contains(mailBeads[0].Description, "foundations/worker") {
		t.Fatalf("mail body missing template name: %q", mailBeads[0].Description)
	}
}

func TestObserveSessionStopped_NewStorm_MailListsAllContributors(t *testing.T) {
	// When the threshold-crossing drain triggers the mail, the body must
	// enumerate every drained-without-claim session in the current
	// window — not just the one that crossed the threshold. Operators
	// then have a complete contributor list for forensics.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 3,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	const template = "foundations/worker"
	names := []string{"worker-pool-alpha", "worker-pool-beta", "worker-pool-gamma"}
	for i, name := range names {
		session := buildSessionBeadForSafetyNet(t, store, name, true)
		observeSessionStoppedForSafetyNet("", session, name, template, store, nil, now.Add(time.Duration(i)*30*time.Second), io.Discard)
	}

	mailBeads, err := store.List(beads.ListQuery{Type: "message", Assignee: spawnStormMayorRecipient, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list mail: %v", err)
	}
	if len(mailBeads) != 1 {
		t.Fatalf("mayor inbox = %d, want 1", len(mailBeads))
	}
	body := mailBeads[0].Description
	for _, name := range names {
		if !strings.Contains(body, name) {
			t.Fatalf("mail body missing contributor %q\nbody:\n%s", name, body)
		}
	}
}

func TestObserveSessionStopped_OneMailPerStormEpisode(t *testing.T) {
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1, // trigger on first drain
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	// Five subsequent drain-without-claim outcomes inside the same
	// throttle window — only the first should produce a mail.
	for i := 0; i < 5; i++ {
		name := workerNameForIndex(i)
		session := buildSessionBeadForSafetyNet(t, store, name, true)
		observeSessionStoppedForSafetyNet("", session, name, "foundations/worker", store, nil, now.Add(time.Duration(i)*30*time.Second), io.Discard)
	}
	mailBeads, err := store.List(beads.ListQuery{Type: "message", Assignee: spawnStormMayorRecipient, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list mail beads: %v", err)
	}
	if len(mailBeads) != 1 {
		t.Fatalf("mayor inbox = %d messages across one storm episode, want 1", len(mailBeads))
	}
}

func TestBuildDesiredState_GateSuppressesDemandForThrottledTemplate(t *testing.T) {
	// End-to-end: a registered safety net in storm state must cause
	// buildDesiredStateWithSessionBeads to report 0 demand for the
	// throttled template, and consequently no pool sessions in the
	// desired-state map. This is the production effect — workers stop
	// being spawned for the affected pool.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "foundations/worker"

	// Seed real demand: 3 routed beads ready to be claimed.
	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{
			Title:    "queued routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": template},
		}); err != nil {
			t.Fatalf("create routed work: %v", err)
		}
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	cfg := buildPoolCfgForGateTest(template)

	// Register a safety net that's already in a storm episode for this
	// template. We achieve this by directly driving RecordDrainOutcome
	// past the threshold; the gate then sees IsThrottled=true at build
	// time.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 2,
		InitialBackoff: 30 * time.Minute,
		MaxBackoff:     60 * time.Minute,
	})
	now := time.Now().UTC()
	sn.RecordDrainOutcome(template, "w1", false, now)
	sn.RecordDrainOutcome(template, "w2", false, now.Add(10*time.Second))
	if !sn.IsThrottled(template, now.Add(20*time.Second)) {
		t.Fatal("test scaffold: safety net failed to enter storm before build")
	}
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	var stderr strings.Builder
	dsResult := callBuildDesiredStateWithSessionBeads(t, cityPath, cfg, store, snapshot, &stderr)

	if got := dsResult.ScaleCheckCounts[template]; got != 0 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 0 with safety net throttling", template, got)
	}
	if !strings.Contains(stderr.String(), "spawn-storm safety net") {
		t.Fatalf("expected throttle diagnostic in stderr, got:\n%s", stderr.String())
	}
	for sessionName, tp := range dsResult.State {
		if tp.TemplateName == template {
			t.Fatalf("desired state materialized pool session %q while throttled; want empty pool", sessionName)
		}
	}
}

func TestBuildDesiredState_NoGateWhenSafetyNetUnregistered(t *testing.T) {
	// Positive control: without a registered safety net, the same
	// fixture must produce real demand. Ensures the gate is opt-in
	// (process-wide registration) and doesn't accidentally gate when
	// no controller installed one.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	const template = "foundations/worker"

	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{
			Title:    "queued routed work",
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": template},
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	cfg := buildPoolCfgForGateTest(template)

	var stderr strings.Builder
	dsResult := callBuildDesiredStateWithSessionBeads(t, cityPath, cfg, store, snapshot, &stderr)

	if got := dsResult.ScaleCheckCounts[template]; got != 3 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 3 (unsuppressed demand)", template, got)
	}
}

func TestObserveSessionStopped_RateConditionGuard_SuppressesWhenLiveClaim(t *testing.T) {
	// A live in-progress claim for the same template indicates the pool is
	// actively making progress: a coincident drain-without-claim is most
	// likely race-loser noise, not the spawn-storm pathology. The guard
	// must skip registration so the safety net's window is not poisoned
	// by transient races.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1, // would trigger on a single drain absent the guard
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	const template = "foundations/worker"
	// Seed an in-progress claim by another live worker for the same template.
	// MemStore.Create forces status=open, so promote it via Update.
	live, err := store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Assignee: "worker-pool-live",
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		t.Fatalf("seed in-progress claim: %v", err)
	}
	inProg := "in_progress"
	if err := store.Update(live.ID, beads.UpdateOpts{Status: &inProg}); err != nil {
		t.Fatalf("promote to in_progress: %v", err)
	}

	session := buildSessionBeadForSafetyNet(t, store, "worker-pool-drained", true)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	observeSessionStoppedForSafetyNet("", session, "worker-pool-drained", template, store, nil, now, io.Discard)

	if sn.IsThrottled(template, now.Add(1*time.Minute)) {
		t.Fatal("rate-condition guard failed: drain registered while another worker is in_progress")
	}
}

func TestObserveSessionStopped_RateConditionGuard_NoLiveClaim_ProceedsNormally(t *testing.T) {
	// With NO live in-progress claim for the template, the guard must not
	// fire — observation proceeds as before.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	store := beads.NewMemStore()
	const template = "foundations/worker"
	session := buildSessionBeadForSafetyNet(t, store, "worker-pool-drained", true)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	observeSessionStoppedForSafetyNet("", session, "worker-pool-drained", template, store, nil, now, io.Discard)

	if !sn.IsThrottled(template, now.Add(1*time.Minute)) {
		t.Fatal("guard suppressed drain registration with no live claim present")
	}
}

func TestObserveSessionStopped_RateConditionGuard_StoreError_FailsSafe(t *testing.T) {
	// A transient store failure on the live-claim probe must NOT silently
	// disable the safety net. Fail-safe = treat as no-live-claim and
	// proceed with registration.
	sn := NewSpawnStormSafetyNet(SpawnStormConfig{
		Window:         5 * time.Minute,
		DrainThreshold: 1,
		InitialBackoff: 10 * time.Minute,
	})
	restore := RegisterSpawnStormSafetyNetForCurrentController(sn)
	defer restore()

	const template = "foundations/worker"
	store := newLiveClaimErroringStore(template)
	session := buildSessionBeadForSafetyNet(t, store.MemStore, "worker-pool-drained", true)
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	observeSessionStoppedForSafetyNet("", session, "worker-pool-drained", template, store, nil, now, io.Discard)

	if !sn.IsThrottled(template, now.Add(1*time.Minute)) {
		t.Fatal("fail-safe broken: probe error caused safety net to skip registration")
	}
}

// liveClaimErroringStore wraps an in-memory store and injects an error
// only on the rate-condition probe (Status=in_progress + gc.routed_to
// metadata). All other queries pass through untouched so the
// already-claimed lookup behaves normally.
type liveClaimErroringStore struct {
	*beads.MemStore
	template string
}

func newLiveClaimErroringStore(template string) *liveClaimErroringStore {
	return &liveClaimErroringStore{MemStore: beads.NewMemStore(), template: template}
}

func (s *liveClaimErroringStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "in_progress" && q.Metadata["gc.routed_to"] == s.template {
		return nil, errProbeUnavailable
	}
	return s.MemStore.List(q)
}

var errProbeUnavailable = probeErr("probe transiently unavailable")

type probeErr string

func (e probeErr) Error() string { return string(e) }

func buildSessionBeadForSafetyNet(t *testing.T, store beads.Store, sessionName string, poolManaged bool) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		"session_name": sessionName,
		"template":     "foundations/worker",
	}
	if poolManaged {
		metadata["pool_managed"] = "true"
		metadata["pool_slot"] = "1"
	}
	bead, err := store.Create(beads.Bead{
		Title:    "session " + sessionName,
		Type:     "session",
		Status:   "closed",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead %q: %v", sessionName, err)
	}
	return bead
}

func workerNameForIndex(i int) string {
	suffixes := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	if i < len(suffixes) {
		return "worker-pool-" + suffixes[i]
	}
	return "worker-pool-extra"
}

func buildPoolCfgForGateTest(template string) *config.City {
	zero := 0
	ten := 10
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              template,
			StartCommand:      "true",
			MinActiveSessions: &zero,
			MaxActiveSessions: &ten,
		}},
	}
}

func callBuildDesiredStateWithSessionBeads(
	t *testing.T,
	cityPath string,
	cfg *config.City,
	store beads.Store,
	snapshot *sessionBeadSnapshot,
	stderr *strings.Builder,
) DesiredStateResult {
	t.Helper()
	return buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, snapshot, nil, stderr,
	)
}
