package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// deadRuntimeProvider proves ABSENCE the way a D2-capable provider does: the
// observation completes and reports the runtime neither running nor alive. It
// is the fixture the whole family turns on — a close behind an unproven absence
// is the #3872 orphaning bug.
//
// unlicensableAlive models the busy-host asymmetry instead (ga-bxa8r): a LIVE
// target holds a pane, a live pane withholds the tmux-absence license the /proc
// sweep needs, and so its observation is positive-but-incomplete forever, while
// the same probe completes the moment the pane is gone.
type deadRuntimeProvider struct {
	*runtime.Fake
	incomplete        bool
	unlicensableAlive bool
	observed          int
}

func (p *deadRuntimeProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	p.observed++
	if p.incomplete {
		return runtime.Liveness{Complete: false}
	}
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: !running || !p.unlicensableAlive}
}

func (p *deadRuntimeProvider) StopUnattendedSession(name, _ string) error { return p.Stop(name) }

// newExactOrphanCloseParams builds the handler's params for one seeded row,
// with the fleet's desired view supplied exactly as the tick publishes it.
func newExactOrphanCloseParams(env *reconcilerTestEnv, provider runtime.Provider, desired map[string]bool) exactSessionStartParams {
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	return exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stderr:              &bytes.Buffer{},
		DesiredSessionNames: func() map[string]bool { return desired },
	}
}

func orphanCloseAdmission(id string) sessionStartAdmission {
	return sessionStartAdmission{SessionID: id, Source: sessionStartAdmissionOrphanClose}
}

// orphanSweepInput builds the minimum sweep input that reaches D-ORPHAN for one
// row, with admit as the routing seam's enqueue hook.
func orphanSweepInput(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	info sessionpkg.Info,
	desired map[string]TemplateParams,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
) detectorSweepInput {
	return detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: provider,
		Rows:     []sessionpkg.ReconcileSession{{Info: info}},
		Desired:  desired,
		Clock:    &clock.Fake{Time: now},
		Trigger:  "patrol",
		Admit:    admit,
	}
}

// TestExactOrphanDeadRowClosesOnceByKey is WD.3's primary RED: an undesired row
// whose runtime is PROVABLY absent is closed exactly once by exact key, with
// legacy's close_reason and terminal state recorded, and its pool slot freed
// (the row stops counting as open for its template). It is the keyed re-point
// of TestReconcileSessionBeads_OrphanNotRunningClosed.
func TestExactOrphanDeadRowClosesOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "other"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")
	env.setSessionMetadata(&bead, map[string]string{"pool_slot": "1", poolManagedMetadataKey: boolMetadata(true)})

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the D-ORPHAN close seam did not claim an undesired dead row")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read closed row: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("row status = %q, want closed", stored.Status)
	}
	if want := sessionpkg.CanonicalCloseReason("orphaned"); stored.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", stored.Metadata["close_reason"], want)
	}
	if stored.Metadata["state"] != "orphaned" {
		t.Fatalf("state = %q, want orphaned", stored.Metadata["state"])
	}
	if count := openPoolSessionCountForTemplate(
		map[string]sessionpkg.Info{bead.ID: env.sessionInfo(bead.ID)}, env.cfg, "orphan"); count != 0 {
		t.Fatalf("open pool session count for the closed slot = %d, want 0 (slot not freed)", count)
	}

	// Exactly once: a second admission on the same key finds a closed row, makes
	// no further write, and the close arm does not re-enter.
	observedAfterFirst := provider.observed
	handledAgain, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("second admission returned owner=%v err=%v, want a quiet keyed refusal", owner, err)
	}
	if handledAgain && provider.observed != observedAfterFirst {
		t.Fatal("second admission re-observed liveness on an already-closed row; the close is not idempotent by key")
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata["closed_at"] != stored.Metadata["closed_at"] {
		t.Fatalf("second admission re-stamped the terminal metadata: %q then %q",
			stored.Metadata["closed_at"], after.Metadata["closed_at"])
	}
}

// TestExactOrphanFailedCreateClosesByKeyAndFreesSlot is the merged
// CloseFailedCreate arm: an expired failed-create row is closed by exact key
// with the failed-create close reason and its pending-create claim cleared, so
// the pool slot is free for the successor. Keyed re-point of
// TestReconcileSessionBeads_ClosesOrphanedFailedCreateAndFreesSlot.
func TestExactOrphanFailedCreateClosesByKeyAndFreesSlot(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":                     string(sessionpkg.StateFailedCreate),
		"pool_slot":                 "1",
		poolManagedMetadataKey:      boolMetadata(true),
		"pending_create_claim":      boolMetadata(true),
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
	})

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{"worker-2": true})

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("failed-create close: handled=%v owner=%v err=%v", handled, owner, err)
	}

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read closed row: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("failed-create row status = %q, want closed", stored.Status)
	}
	if want := sessionpkg.CanonicalCloseReason(string(sessionpkg.StateFailedCreate)); stored.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", stored.Metadata["close_reason"], want)
	}
	if stored.Metadata["pending_create_claim"] != "" || stored.Metadata["pending_create_started_at"] != "" {
		t.Fatalf("pending-create lease survived the close: claim=%q started_at=%q",
			stored.Metadata["pending_create_claim"], stored.Metadata["pending_create_started_at"])
	}
	if count := openPoolSessionCountForTemplate(
		map[string]sessionpkg.Info{bead.ID: env.sessionInfo(bead.ID)}, env.cfg, "worker"); count != 0 {
		t.Fatalf("open pool session count after the failed-create close = %d, want 0", count)
	}
}

// TestExactOrphanStillLeasedFailedCreateStaysOpen is the PendingCreatePreserved
// negative: a failed-create row whose create lease has NOT expired belongs to
// D-STALE-CREATE, not to this family, so the seam never claims it.
func TestExactOrphanStillLeasedFailedCreateStaysOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"state":                     string(sessionpkg.StateFailedCreate),
		"pending_create_claim":      boolMetadata(true),
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
	})

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})

	handled, _, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if handled || err != nil {
		t.Fatalf("still-leased failed-create claimed by the close seam: handled=%v err=%v", handled, err)
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("still-leased failed-create row was closed; PendingCreatePreserved is D-STALE-CREATE's arm")
	}
}

// TestDetectorOrphanDesiredRowNeitherEnqueuesNorCloses is the first negative: a
// DESIRED row raises no orphan condition, enqueues nothing, and the handler
// refuses with zero effect even if some other admission carries the key into
// the seam.
func TestDetectorOrphanDesiredRowNeitherEnqueuesNorCloses(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("worker", "worker")

	desired := map[string]TemplateParams{"worker": {SessionName: "worker", TemplateName: "worker"}}
	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), desired, env.clk.Now().UTC(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan {
			t.Fatalf("sweep raised an orphan condition for a desired row: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("sweep enqueued %v for a desired row; want zero enqueues", admitter.keys)
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{"worker": true})
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if handled || err != nil || owner != exactSessionStartUnowned {
		t.Fatalf("desired row entered the close seam: handled=%v owner=%v err=%v", handled, owner, err)
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("desired row was closed by the keyed orphan handler")
	}
}

// TestExactOrphanUnprovenAbsenceRefusesWithZeroEffect is the load-bearing
// negative: when the fresh-liveness observation does NOT complete, absence is
// not proven, so the handler refuses with a typed error, zero store writes and
// zero provider mutations — never an optimistic close. This is legacy's
// fail-closed liveness-error arm, kept.
func TestExactOrphanUnprovenAbsenceRefusesWithZeroEffect(t *testing.T) {
	for _, tc := range []struct {
		name       string
		incomplete bool
		running    bool
	}{
		{name: "incomplete observation", incomplete: true},
		{name: "possibly alive runtime", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
			provider := &deadRuntimeProvider{Fake: env.sp, incomplete: tc.incomplete}
			bead := env.createSessionBead("orphan", "orphan")
			env.markSessionActive(&bead)
			if tc.running {
				if err := provider.Start(t.Context(), "orphan", runtime.Config{}); err != nil {
					t.Fatalf("start runtime: %v", err)
				}
			}
			before, err := env.store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}

			info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
			if err != nil {
				t.Fatalf("authoritative read: %v", err)
			}
			params := newExactOrphanCloseParams(env, provider, map[string]bool{})
			// WD.4: the possibly-alive case is handed to the drain arm from inside
			// this observation, and the drain arm needs somewhere to record intent.
			params.DrainTracker = env.dt
			handled, owner, err := reconcileExactSessionDetectorFamily(
				t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
			if !handled {
				t.Fatal("the seam did not claim an undesired row")
			}
			if owner != exactSessionStartKeyedOwner {
				t.Fatalf("owner = %v, want keyed ownership (require mode never hands an unproven close to legacy)", owner)
			}
			if tc.incomplete && err == nil {
				t.Fatal("incomplete liveness observation produced no typed refusal")
			}
			if tc.incomplete && env.dt.get(bead.ID) != nil {
				t.Fatal("an incomplete observation began a drain; an unproven row belongs to neither arm")
			}
			if tc.running {
				if err != nil {
					t.Fatalf("a possibly-alive row is the WD.4 drain arm's, not an error: %v", err)
				}
				if env.dt.get(bead.ID) == nil {
					t.Fatal("a possibly-alive row was released by the close arm and no drain arm picked it up")
				}
			}

			after, err := env.store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status == "closed" {
				t.Fatal("a row whose absence was never proven was closed")
			}
			if len(after.Metadata) != len(before.Metadata) || after.Metadata["close_reason"] != "" || after.Metadata["closed_at"] != "" {
				t.Fatalf("refused close mutated the durable row: before=%#v after=%#v", before.Metadata, after.Metadata)
			}
			if tc.running && !provider.IsRunning("orphan") {
				t.Fatal("refused close stopped the runtime; the close arm never touches the provider")
			}
		})
	}
}

// TestExactOrphanLiveRowReachesTheDrainArmOnIncompleteScan is ga-bxa8r's
// D-ORPHAN specimen. The POSITIVE arm here is not a no-op: a live undesired row
// is handed to the WD.4 drain arm from inside this very observation, and that
// hand-off is the family's whole answer for a session that must go away
// gracefully. Because a live pane withholds the tmux-absence license, the
// unconditional Complete gate parked the row above the hand-off on any busy
// host, so a live undesired session was never drained and never closed — it just
// re-parked every sweep.
//
// The destructive close below keeps its proof obligation untouched; it is only
// reachable from the negative arm, which still demands a COMPLETE observation.
func TestExactOrphanLiveRowReachesTheDrainArmOnIncompleteScan(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp, unlicensableAlive: true}
	bead := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&bead)
	if err := provider.Start(t.Context(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})
	params.DrainTracker = env.dt

	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if !handled {
		t.Fatal("the seam did not claim an undesired row")
	}
	if err != nil {
		t.Fatalf("a live row's incomplete scan parked the orphan hand-off: %v", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("the live undesired row never reached the drain arm; a positive observation must license the hand-off")
	}
	if after, _ := env.store.Get(bead.ID); after.Status == "closed" {
		t.Fatal("a live row was closed; the close arm is the negative arm's alone")
	}
	if !provider.IsRunning("orphan") {
		t.Fatal("the close arm touched the provider")
	}
}

// TestDetectorOrphanAssignedWorkNeverEnqueues ports legacy's kept-open
// suppressor (session_reconciler.go:2193-2204) to the sweep: an undesired row
// still holding open assigned work is recorded but never enqueued, so no close
// can strand its work.
func TestDetectorOrphanAssignedWorkNeverEnqueues(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")

	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.AssignedWorkBeads = []beads.Bead{{ID: "ga-work-1", Status: "in_progress", Assignee: bead.ID}}
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("a row with open assigned work enqueued %v; want zero enqueues", admitter.keys)
	}
	keptOpen := false
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyOrphan {
			continue
		}
		if cond.Outcome == TraceOutcomeClosed {
			t.Fatalf("a row with open assigned work predicted a close: %#v", cond)
		}
		if cond.Reason == detectorReasonOrphanAssignedWork {
			keptOpen = true
		}
	}
	if !keptOpen {
		t.Fatalf("no kept-open record for the assigned-work suppressor; conditions=%#v", result.Conditions)
	}
}

// TestDetectorOrphanStoreQueryPartialEmitsNoCloseRecord preserves the gc-hz0nu
// guard on the keyed path. Suppression happens BEFORE the condition exists, so
// a partial store view cannot produce a close record at all — legacy's
// record-Closed-then-decline-to-close trace lie is impossible by construction,
// not fixed after the fact.
func TestDetectorOrphanStoreQueryPartialEmitsNoCloseRecord(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")

	admitter := &recordingDetectorAdmitter{}
	in := orphanSweepInput(env, provider, env.sessionInfo(bead.ID), map[string]TemplateParams{}, env.clk.Now().UTC(), admitter.admit)
	in.StoreQueryPartial = true
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyOrphan {
			t.Fatalf("partial store view produced an orphan record: %#v", cond)
		}
	}
	if result.SuppressedByPartialStore != 1 {
		t.Fatalf("SuppressedByPartialStore = %d, want 1", result.SuppressedByPartialStore)
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("partial store view enqueued %v; want zero enqueues (gc-hz0nu)", admitter.keys)
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("partial store view closed a row")
	}
}

// TestLegacyOrphanCloseArmYieldsToKeyedOwnedRow is the coexistence-doctrine RED
// for the CloseOrphan arm. Both writers read the same durable row on the same
// tick, so an acting D-ORPHAN beside a non-yielding legacy double-closes by
// construction; legacy must stand down while the keyed controller owns the key.
func TestLegacyOrphanCloseArmYieldsToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	session := env.createSessionBead("orphan", "orphan")

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyOrphanCloseExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == "closed" {
		t.Fatal("legacy CloseOrphan arm closed a row the keyed D-ORPHAN handler owns")
	}
	if stored.Metadata["close_reason"] != "" {
		t.Fatalf("legacy stamped the keyed handler's close patch: %#v", stored.Metadata)
	}
}

// TestLegacyFailedCreateCloseArmYieldsToKeyedOwnedRow is the same doctrine on
// the merged CloseFailedCreate arm.
func TestLegacyFailedCreateCloseArmYieldsToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	session := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                     string(sessionpkg.StateFailedCreate),
		"pool_slot":                 "1",
		poolManagedMetadataKey:      boolMetadata(true),
		"pending_create_claim":      boolMetadata(true),
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now().Add(-(pendingCreateNeverStartedTimeout + time.Second))),
	})

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyOrphanCloseExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == "closed" {
		t.Fatal("legacy CloseFailedCreate arm closed a row the keyed D-ORPHAN handler owns")
	}
	if stored.Metadata["pending_create_claim"] == "" {
		t.Fatal("legacy cleared the keyed handler's pending-create lease")
	}
}

// TestExactOrphanCloseRequiresPublishedDesiredView pins the fail-closed rule for
// the one fleet-shaped input the handler cannot re-derive itself: with no
// desired view published yet — the window before the first patrol tick — the
// family claims nothing and closes nothing.
func TestExactOrphanCloseRequiresPublishedDesiredView(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}

	for _, tc := range []struct {
		name   string
		params exactSessionStartParams
	}{
		{name: "no accessor", params: newExactOrphanCloseParams(env, provider, nil)},
		{name: "unpublished view", params: newExactOrphanCloseParams(env, provider, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := tc.params
			if tc.name == "no accessor" {
				params.DesiredSessionNames = nil
			}
			handled, owner, err := reconcileExactSessionDetectorFamily(
				t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
			if handled || err != nil || owner != exactSessionStartUnowned {
				t.Fatalf("close seam claimed a key without a desired view: handled=%v owner=%v err=%v", handled, owner, err)
			}
			if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
				t.Fatal("row closed without a published desired view")
			}
		})
	}
}

// TestCityRuntimePublishesDesiredSessionNames pins the one piece of fleet state
// the handler reads: the patrol tick's desired view reaches the handler through
// the runtime accessor, and a later tick replaces it wholesale rather than
// accumulating stale names.
func TestCityRuntimePublishesDesiredSessionNames(t *testing.T) {
	cr := &CityRuntime{}
	if view := cr.desiredSessionNamesView(); view != nil {
		t.Fatalf("desiredSessionNamesView() = %#v before any tick, want nil", view)
	}
	cr.publishDesiredSessionNames(map[string]TemplateParams{"worker-1": {SessionName: "worker-1"}})
	if view := cr.desiredSessionNamesView(); !view["worker-1"] {
		t.Fatalf("desiredSessionNamesView() = %#v, want worker-1 desired", view)
	}
	cr.publishDesiredSessionNames(map[string]TemplateParams{"worker-2": {SessionName: "worker-2"}})
	view := cr.desiredSessionNamesView()
	if view["worker-1"] || !view["worker-2"] {
		t.Fatalf("desiredSessionNamesView() = %#v, want only worker-2 (a republish replaces, never merges)", view)
	}
}

// TestExactOrphanCloseAdmissionIsOwnedAndYielded closes the ownership loop: an
// orphan-close admission is reported by ownsOrphanClose and NOT by
// ownsDeadlineStop, so each family's legacy arm yields only to its own keyed
// owner. A single shared predicate would silently disable the other arm.
func TestExactOrphanCloseAdmissionIsOwnedAndYielded(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")

	cs := coherentSessionStartControllerStateForTest(env.cfg, provider, env.store, rollout.Require)
	cr := &CityRuntime{
		cityPath: "test-city", cityName: "test-city", cfg: env.cfg, sp: provider, cs: cs,
		rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	t.Cleanup(cr.stopSessionStartController)

	controller := cr.sessionStartController
	if _, err := controller.Admit(bead.ID, sessionStartAdmissionOrphanClose); err != nil {
		t.Fatalf("admitting orphan-close key: %v", err)
	}
	if controller.ownsDeadlineStop(bead.ID) {
		t.Fatal("ownsDeadlineStop() answered true for an orphan-close admission; legacy's idle kill would be silently disabled")
	}
	awaitCond(t, func() bool { return !controller.ownsOrphanClose(bead.ID) }, "orphan-close admission drain")
}

// TestExactOrphanCloseYieldsToLegacyDrainInFlight pins the one hand-back: a row
// with an active legacy drain is D-DRAIN's, so the close handler refuses rather
// than racing a drain it does not own.
func TestExactOrphanCloseYieldsToLegacyDrainInFlight(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	bead := env.createSessionBead("orphan", "orphan")
	env.markSessionActive(&bead)
	if !beginSessionDrainInfo(env.sessionInfo(bead.ID), provider, env.dt, "orphaned", env.clk, defaultDrainTimeout) {
		t.Fatal("could not begin the legacy drain the negative depends on")
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := newExactOrphanCloseParams(env, provider, map[string]bool{})
	params.DrainTracker = env.dt

	_, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(), orphanCloseAdmission(bead.ID), params, info, response, env.clk)
	if err == nil {
		t.Fatal("close handler entered a row with an active legacy drain")
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership in require mode", owner)
	}
	if errors.Is(err, errSessionStartLegacyFallbackRequired) {
		t.Fatal("require mode must park, never request a legacy fallback")
	}
	if stored, _ := env.store.Get(bead.ID); stored.Status == "closed" {
		t.Fatal("a row with an active legacy drain was closed")
	}
}
