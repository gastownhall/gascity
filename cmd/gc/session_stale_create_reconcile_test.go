package main

import (
	"context"
	"strconv"
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

// seedStalePendingCreate writes the schema-v59 shape the legacy anchor test
// seeds (session_reconciler_test.go:7764): a never-started pending create whose
// lease anchor sits past the never-started window, with no runtime. leaseAge
// picks which side of that window the row lands on, so the same fixture serves
// the rollback arm and the preserved negative.
func seedStalePendingCreate(t *testing.T, env *reconcilerTestEnv, name string, leaseAge time.Duration) beads.Bead {
	t.Helper()
	bead := env.createSessionBead(name, name)
	env.setSessionMetadata(&bead, map[string]string{
		"session_name_explicit":     "true",
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now().Add(-leaseAge)),
		"continuation_epoch":        "1",
		// last_woke_at deliberately absent: preWakeCommit never fired.
	})
	return bead
}

func staleCreateSweepInput(
	env *reconcilerTestEnv,
	provider runtime.Provider,
	info sessionpkg.Info,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
) detectorSweepInput {
	name := info.SessionNameMetadata
	return detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: provider,
		Rows:     []sessionpkg.ReconcileSession{{Info: info}},
		// Desired, so family precedence does not route the row to D-ORPHAN
		// before D-STALE-CREATE is consulted.
		Desired: map[string]TemplateParams{name: {SessionName: name, TemplateName: info.Template}},
		Clock:   &clock.Fake{Time: now},
		Trigger: "patrol",
		Admit:   admit,
	}
}

// TestExactStaleCreateRollsBackOnceByKey is WD.7's primary RED: a seeded v59
// pending-create row whose lease has expired with no runtime is handed to the
// session-start controller under the D-STALE-CREATE admission source, rolled
// back exactly once by exact key with its claim cleared, and left terminal so a
// second admission on the same key is a zero-effect no-op. It is the keyed
// re-point of
// TestReconcileSessionBeads_RollsBackPendingCreateWhenLeaseExpiredAndNoRuntime.
func TestExactStaleCreateRollsBackOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedStalePendingCreate(t, env, "helper", pendingCreateNeverStartedTimeout+time.Second)

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	admit := cr.detectorAdmitFunc()
	if admit == nil {
		t.Fatal("detectorAdmitFunc() = nil under keyed ownership; the sweep has no enqueue seam")
	}

	// The sweep is the producer of this key: it must classify the row into
	// D-STALE-CREATE and route it under the family's own admission source.
	admitter := &recordingDetectorAdmitter{}
	in := staleCreateSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.keys[0] != bead.ID {
		t.Fatalf("sweep enqueued %v, want exactly the stale key %q", admitter.keys, bead.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionStaleCreate {
		t.Fatalf("sweep enqueued under source %q, want %q", admitter.sources[0], sessionStartAdmissionStaleCreate)
	}

	if outcome, err := admit(bead.ID, sessionStartAdmissionStaleCreate); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting stale-create key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool {
		stored, err := env.store.Get(bead.ID)
		return err == nil && stored.Status == "closed"
	}, "keyed stale pending-create rollback")

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read rolled-back row: %v", err)
	}
	if stored.Metadata["pending_create_claim"] != "" || stored.Metadata["pending_create_started_at"] != "" {
		t.Fatalf("rollback left the pending-create claim behind: %#v", stored.Metadata)
	}
	if stored.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
		t.Fatalf("durable state = %q, want %q", stored.Metadata["state"], sessionpkg.StateFailedCreate)
	}
	if stored.Metadata["last_woke_at"] != "" || stored.Metadata["session_name"] != "" {
		t.Fatalf("rollback did not clear the in-flight lease markers: %#v", stored.Metadata)
	}
	closedAt := stored.Metadata["closed_at"]

	// Exactly once by key: the level-triggered condition no longer holds, so a
	// second admission on the same key changes nothing.
	if outcome, err := admit(bead.ID, sessionStartAdmissionStaleCreate); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("re-admitting rolled-back key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool { return !cr.sessionStartController.ownsStaleCreateRollback(bead.ID) }, "stale-create admission drain")
	again, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read rolled-back row: %v", err)
	}
	if again.Metadata["closed_at"] != closedAt {
		t.Fatalf("second admission rolled the row back again: closed_at %q -> %q", closedAt, again.Metadata["closed_at"])
	}
}

// TestExactStaleCreateRetiresThePerTickRollbackBudget is the R6 proof: legacy
// caps itself at five rollbacks per tick and defers the rest, because each one
// costs three bd subprocess calls on the tick critical path. Keyed rollbacks run
// on the controller's bounded worker pool, so ten stale rows detected in ONE
// sweep are all enqueued and all rolled back in that cycle, with nothing
// deferred to the next.
func TestExactStaleCreateRetiresThePerTickRollbackBudget(t *testing.T) {
	const stranded = 10
	if stranded <= 5 {
		t.Fatal("the fixture must exceed legacy's maxRollbacksPerTick of five to prove anything")
	}
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}

	rows := make([]sessionpkg.ReconcileSession, 0, stranded)
	ids := make([]string, 0, stranded)
	desired := map[string]TemplateParams{}
	for i := range stranded {
		name := "helper-" + strconv.Itoa(i)
		bead := seedStalePendingCreate(t, env, name, pendingCreateNeverStartedTimeout+time.Second)
		ids = append(ids, bead.ID)
		rows = append(rows, sessionpkg.ReconcileSession{Info: env.sessionInfo(bead.ID)})
		desired[name] = TemplateParams{SessionName: name, TemplateName: "helper"}
	}

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	in := detectorSweepInput{
		CityPath: "test-city", CityName: "test-city",
		Cfg: env.cfg, Provider: provider, Rows: rows, Desired: desired,
		Clock: &clock.Fake{Time: env.clk.Now()}, Trigger: "patrol",
		Admit: cr.detectorAdmitFunc(),
	}
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	routed := 0
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyStaleCreate && cond.AdmissionSource == sessionStartAdmissionStaleCreate {
			routed++
		}
	}
	if routed != stranded {
		t.Fatalf("one sweep routed %d stale creates, want all %d — no per-tick budget survives", routed, stranded)
	}
	for _, id := range ids {
		awaitCond(t, func() bool {
			stored, err := env.store.Get(id)
			return err == nil && stored.Status == "closed" && stored.Metadata["pending_create_claim"] == ""
		}, "rollback of "+id+" in the same cycle")
	}
}

// TestDetectorStaleCreateFreshLeaseNeitherEnqueuesNorRollsBack is the negative:
// a never-started pending create still inside its lease window raises the
// PendingCreatePreserved arm, never an enqueue, and the handler refuses with
// zero writes even when some other admission carries the key into the seam. It
// is the keyed re-point of
// TestReconcileSessionBeads_PreservesNeverStartedPendingCreateBeforeLeaseExpires.
func TestDetectorStaleCreateFreshLeaseNeitherEnqueuesNorRollsBack(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedStalePendingCreate(t, env, "helper", pendingCreateNeverStartedTimeout-time.Minute)

	admitter := &recordingDetectorAdmitter{}
	in := staleCreateSweepInput(env, provider, env.sessionInfo(bead.ID), env.clk.Now(), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	if len(admitter.keys) != 0 {
		t.Fatalf("fresh pending create enqueued %v; want zero enqueues", admitter.keys)
	}
	preserved := false
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyStaleCreate {
			continue
		}
		if cond.Outcome == TraceOutcomeRollback {
			t.Fatalf("fresh pending create predicted a rollback: %#v", cond)
		}
		if cond.Reason == detectorReasonPendingCreatePreserved {
			preserved = true
		}
	}
	if !preserved {
		t.Fatalf("no PendingCreatePreserved condition recorded; conditions=%#v", result.Conditions)
	}

	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		Recorder: events.Discard, RolloutMode: rollout.Require,
	}
	// The seam never dispatches this row: its guard is the durable lease, and
	// the lease has not expired.
	if exactSessionStaleCreateRollbackCandidate(params, info, response, env.clk) {
		t.Fatal("a fresh pending create satisfied the stale-create seam guard")
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStaleCreate},
		params, info, response, env.clk,
	)
	if handled || err != nil || owner != exactSessionStartUnowned {
		t.Fatalf("seam claimed a fresh pending create: handled=%v owner=%v err=%v", handled, owner, err)
	}
	// And the handler itself refuses with zero effect if some other admission
	// carries the key into it anyway.
	if owner, err := reconcileExactSessionStaleCreateRollback(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStaleCreate},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read seeded row: %v", err)
	}
	if after.Status != before.Status || after.Metadata["pending_create_claim"] != "true" ||
		after.Metadata["state"] != before.Metadata["state"] || after.Metadata["closed_at"] != "" {
		t.Fatalf("refused stale-create handler mutated the durable row: %#v", after.Metadata)
	}
}

// TestExactStaleCreateRollbackRefusesWhenLeaseIsRenewedBeforeTheWrite pins the
// seam's second rule on this family: the handler re-derives its own condition
// from the authoritative row and refuses with zero effect the moment it no
// longer holds. A create that re-leased between detection and dispatch is a
// PendingCreatePreserved no-op, not a rollback.
func TestExactStaleCreateRollbackRefusesWhenLeaseIsRenewedBeforeTheWrite(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper", StartCommand: "true"}}}
	provider := &unattendedStopProvider{Fake: env.sp}
	bead := seedStalePendingCreate(t, env, "helper", pendingCreateNeverStartedTimeout+time.Second)

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if !exactSessionStaleCreateRollbackCandidate(exactSessionStartParams{Config: env.cfg}, info, response, env.clk) {
		t.Fatal("seeded row is not a stale-create candidate; the fixture no longer reproduces the condition")
	}
	// Renew the lease behind the handler's back, exactly as a fresh create
	// attempt would.
	env.setSessionMetadata(&bead, map[string]string{
		"pending_create_started_at": pendingCreateStartedAtNow(env.clk.Now()),
	})

	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		Recorder: events.Discard, RolloutMode: rollout.Require,
	}
	owner, err := reconcileExactSessionStaleCreateRollback(
		t.Context(),
		sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionStaleCreate},
		params, info, response, env.clk,
	)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read re-leased row: %v", err)
	}
	if stored.Status == "closed" || stored.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("handler rolled back a re-leased pending create: status=%q metadata=%#v", stored.Status, stored.Metadata)
	}
}

// TestLegacyPendingCreateArmsYieldToKeyedOwnedRow is the coexistence-doctrine
// RED. Legacy's rollback arms and the keyed handler read the SAME durable
// predicate on the same tick, so an acting D-STALE-CREATE beside a non-yielding
// legacy double-rolls-back by construction: two Tx closes racing on one bead,
// two retired-session cleanups, and legacy's per-tick budget spent on a key it
// does not own. The exclusion is a NEW narrow one rather than a reuse of
// sessionStartLegacyExclusionPredicate — see the WD.7 §3 delta.
func TestLegacyPendingCreateArmsYieldToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper"}}}
	env.addDesired("helper", "helper", false)
	bead := seedStalePendingCreate(t, env, "helper", pendingCreateNeverStartedTimeout+time.Second)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyStaleCreateRollbackExclusion(func(info sessionpkg.Info) bool { return info.ID == bead.ID }),
	)

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read yielded row: %v", err)
	}
	if stored.Status == "closed" {
		t.Fatalf("legacy rolled back a row the keyed D-STALE-CREATE handler owns: %#v", stored.Metadata)
	}
	if stored.Metadata["pending_create_claim"] != "true" || stored.Metadata["last_woke_at"] != "" && stored.Metadata["session_name"] == "" {
		t.Fatalf("legacy applied part of the keyed handler's rollback: %#v", stored.Metadata)
	}
}

// TestLegacyPendingCreateArmsStillRollBackUnownedRows is the other half of the
// doctrine: the exclusion is narrow. A row the keyed controller does NOT own
// still rolls back through legacy for the whole WD wave, so installing the
// bridge cannot silently disable fleet rollback the way reusing the start-family
// predicate would have.
func TestLegacyPendingCreateArmsStillRollBackUnownedRows(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "helper"}}}
	env.addDesired("helper", "helper", false)
	bead := seedStalePendingCreate(t, env, "helper", pendingCreateNeverStartedTimeout+time.Second)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyStaleCreateRollbackExclusion(func(sessionpkg.Info) bool { return false }),
	)

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read unowned row: %v", err)
	}
	if stored.Status != "closed" {
		t.Fatalf("legacy left an unowned stale pending create open: status=%q metadata=%#v", stored.Status, stored.Metadata)
	}
}
