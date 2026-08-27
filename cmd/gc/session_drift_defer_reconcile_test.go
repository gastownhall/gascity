package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// driftDeferEnv builds the ordinary-agent city every deferral fixture below
// seeds. The deferral rungs differ by which human signal is present, never by
// agent, so one template keeps the comparison legible.
func driftDeferEnv() *reconcilerTestEnv {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	return env
}

// seedDriftedSessionNamed seeds an ALIVE schema-v59 row — named or ordinary —
// whose stored baseline was produced from the session's OWN resolved config with
// a provision-half field moved, so the ladder cannot take the warm-box relaunch
// and every rung below is reachable.
func seedDriftedSessionNamed(
	t *testing.T,
	env *reconcilerTestEnv,
	params exactSessionStartParams,
	sessionName string,
	named bool,
) beads.Bead {
	t.Helper()
	session := env.createSessionBead(sessionName, driftOrdinaryTemplate)
	env.markSessionActive(&session)
	if named {
		env.setSessionMetadata(&session, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: driftOrdinaryTemplate,
			namedSessionModeMetadata:     "always",
		})
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "current-cmd"}); err != nil {
		t.Fatalf("start fake runtime for %q: %v", sessionName, err)
	}
	agentCfg := driftAgentConfig(t, env, params, session.ID)
	stale := agentCfg
	stale.PreStart = append([]string{"echo stale-prestart"}, agentCfg.PreStart...)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(stale),
		"started_provision_hash": runtime.ProvisionFingerprint(stale),
		"started_launch_hash":    runtime.LaunchFingerprint(stale),
	})
	return session
}

// driftKeyForTest re-derives the persisted deferral key for a seeded row through
// the production resolution, so a fixture can never stamp a key the handler
// would not read back.
func driftKeyForTest(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, id string) string {
	t.Helper()
	drift, ok := resolveExactSessionConfigDrift(params, env.sessionInfo(id), env.clk)
	if !ok {
		t.Fatal("fixture row does not drift; its deferral key is undefined")
	}
	return drift.DriftKey
}

// admitDriftKey runs one keyed admission of the D-DRIFT family against the
// authoritative row, exactly as the controller's worker does.
func admitDriftKey(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, id string) {
	t.Helper()
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, id)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if !exactSessionConfigDriftCandidate(params, info, response, env.clk) {
		t.Fatal("seeded row is not a D-DRIFT candidate; the fixture no longer reproduces the condition")
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: id, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	)
	if !handled || owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("D-DRIFT admission: handled=%v owner=%v err=%v", handled, owner, err)
	}
}

// TestExactConfigDriftAttachedRowCancelsQueuedDriftDrainByKey is WD.9's primary
// RED and the keyed re-point of
// TestReconcileSessionBeads_AttachedSessionCancelsQueuedConfigDriftDrain. A
// drifted session that was drained while detached, then attached by a human
// before the drain advanced, has that queued drain CANCELED by the same exact
// key that queued it — the drain the previous cycle began is the one thing
// standing between the human and a stopped pane — and the attached window is
// stamped so a transient IsAttached false negative on the next cycle cannot
// re-queue it. The deferral is idempotent: a second admission inside the refresh
// interval writes nothing at all.
func TestExactConfigDriftAttachedRowCancelsQueuedDriftDrainByKey(t *testing.T) {
	cityPath := t.TempDir()
	env := driftDeferEnv()
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		// The provision half moves too, so the ladder cannot take the warm-box
		// relaunch and reaches the ordinary lane's drift drain.
		cfg.PreStart = append([]string{"echo stale-prestart"}, cfg.PreStart...)
		return cfg
	}, nil)

	// Cycle 1, detached: the convergence half queues the drift drain.
	admitDriftKey(t, env, params, session.ID)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "config-drift" {
		t.Fatalf("detached drift did not queue a config-drift drain: %+v", ds)
	}

	// Cycle 2, attached: a human took the pane before the drain advanced.
	env.sp.SetAttached("worker", true)
	admitDriftKey(t, env, params, session.ID)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attached row kept a queued drift drain (reason=%q); it must be canceled", ds.reason)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("attached row was stopped by the deferral arm")
	}
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
		t.Errorf("attached row was relaunched (%d calls)", got)
	}
	stamped, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read deferred row: %v", err)
	}
	if stamped.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatalf("attached deferral left no window stamp: %#v", stamped.Metadata)
	}

	// Cycle 3, still attached: the deferral is level-triggered, so it re-runs —
	// and rewrites nothing, because a durable write per tick per attached session
	// is the Dolt-commit churn the refresh interval exists to prevent.
	env.clk.Time = env.clk.Now().Add(time.Second)
	admitDriftKey(t, env, params, session.ID)
	again, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("re-read deferred row: %v", err)
	}
	if again.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] != stamped.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] {
		t.Fatalf("a second admission inside the refresh interval rewrote the stamp: %q -> %q",
			stamped.Metadata[sessionAttachedConfigDriftDeferredAtMetadata],
			again.Metadata[sessionAttachedConfigDriftDeferredAtMetadata])
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("a re-admitted attached row was drained (reason=%q)", ds.reason)
	}
}

// TestExactConfigDriftDeferralRungsAreTyped drives every shape of the ladder's
// A6 half through the classifier. The RUNG, not the free-text reason, is what
// selects the effect, and this table is why that distinction exists: a named
// session in active use and an ordinary session with a user waiting both report
// "pending_interaction", and legacy applies a window stamp to the first and a
// drain cancel to the second.
func TestExactConfigDriftDeferralRungsAreTyped(t *testing.T) {
	namedTemplate := "worker"
	cases := []struct {
		name        string
		named       bool
		arrange     func(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, session *beads.Bead, sessionName string)
		wantRung    exactSessionConfigDriftDeferralRung
		wantReason  string
		wantOutcome TraceOutcomeCode
		wantTrace   TraceReasonCode
	}{
		{
			name: "attached",
			arrange: func(_ *testing.T, env *reconcilerTestEnv, _ exactSessionStartParams, _ *beads.Bead, sessionName string) {
				env.sp.SetAttached(sessionName, true)
			},
			wantRung:    driftDeferralAttached,
			wantReason:  "attached",
			wantOutcome: TraceOutcomeDeferredAttached,
			wantTrace:   TraceReasonConfigDrift,
		},
		{
			name: "attached_recently",
			arrange: func(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, session *beads.Bead, _ string) {
				// Detached now, but this exact drift key was deferred for an
				// attached human moments ago: a single transient false negative
				// may not destroy the conversation.
				env.setSessionMetadata(session, map[string]string{
					sessionAttachedConfigDriftDeferredAtMetadata:  env.clk.Now().UTC().Format(time.RFC3339),
					sessionAttachedConfigDriftDeferredKeyMetadata: driftKeyForTest(t, env, params, session.ID),
				})
			},
			wantRung:    driftDeferralAttachedRecently,
			wantReason:  "attached_recently",
			wantOutcome: TraceOutcomeDeferredAttached,
			wantTrace:   TraceReasonConfigDrift,
		},
		{
			name:  "named_pinned",
			named: true,
			arrange: func(_ *testing.T, env *reconcilerTestEnv, _ exactSessionStartParams, session *beads.Bead, _ string) {
				env.setSessionMetadata(session, map[string]string{"pin_awake": "true"})
			},
			wantRung:    driftDeferralNamedActive,
			wantReason:  "pinned",
			wantOutcome: TraceOutcomeDeferredActive,
			wantTrace:   TraceReasonConfigDrift,
		},
		{
			name:  "named_recent_activity",
			named: true,
			arrange: func(_ *testing.T, env *reconcilerTestEnv, _ exactSessionStartParams, _ *beads.Bead, sessionName string) {
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Second))
			},
			wantRung:    driftDeferralNamedActive,
			wantReason:  "recent_activity",
			wantOutcome: TraceOutcomeDeferredActive,
			wantTrace:   TraceReasonConfigDrift,
		},
		{
			name: "pending_interaction",
			arrange: func(_ *testing.T, env *reconcilerTestEnv, _ exactSessionStartParams, _ *beads.Bead, sessionName string) {
				env.sp.SetPendingInteraction(sessionName, &runtime.PendingInteraction{RequestID: "approval-1", Kind: "question"})
			},
			wantRung:    driftDeferralPendingInteraction,
			wantReason:  "pending_interaction",
			wantOutcome: TraceOutcomeDeferredPending,
			// Legacy traces this rung under the PENDING reason, not the drift
			// reason. The keyed record must carry the same pair or the WD.15 join
			// reads one population's deferral against the other's absence.
			wantTrace: TraceReasonPending,
		},
		{
			name: "live_assigned_work",
			arrange: func(t *testing.T, env *reconcilerTestEnv, _ exactSessionStartParams, session *beads.Bead, _ string) {
				if _, err := env.store.Create(beads.Bead{
					Title:    "in-flight work",
					Type:     "task",
					Status:   "in_progress",
					Assignee: session.ID,
				}); err != nil {
					t.Fatalf("Create(work): %v", err)
				}
			},
			wantRung:    driftDeferralAssignedWork,
			wantReason:  "live_assigned_work",
			wantOutcome: TraceOutcomeDeferredActive,
			wantTrace:   TraceReasonConfigDrift,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			env := driftDeferEnv()
			sessionName := "worker"
			if tc.named {
				env.cfg.NamedSessions = []config.NamedSession{{Template: namedTemplate, Mode: "always"}}
				sessionName = config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, namedTemplate)
			}
			provider := &unattendedStopProvider{Fake: env.sp}
			params := driftParams(env, cityPath, provider)
			session := seedDriftedSessionNamed(t, env, params, sessionName, tc.named)
			tc.arrange(t, env, params, &session, sessionName)

			info := env.sessionInfo(session.ID)
			drift, ok := resolveExactSessionConfigDrift(params, info, env.clk)
			if !ok {
				t.Fatal("fixture no longer drifts")
			}
			deferral, err := exactSessionConfigDriftDeferralReason(params, info, drift, env.clk)
			if err != nil {
				t.Fatalf("deferral classifier: %v", err)
			}
			if deferral.Rung != tc.wantRung {
				t.Errorf("rung = %q, want %q", deferral.Rung, tc.wantRung)
			}
			if deferral.Reason != tc.wantReason {
				t.Errorf("active_reason = %q, want %q", deferral.Reason, tc.wantReason)
			}
			if deferral.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", deferral.Outcome, tc.wantOutcome)
			}
			if deferral.TraceReason != tc.wantTrace {
				t.Errorf("trace reason = %q, want %q", deferral.TraceReason, tc.wantTrace)
			}
		})
	}
}

// TestExactConfigDriftPendingInteractionDefersWithZeroStopAndZeroStart is the
// negative the A6 invariant is actually about: a session holding a question in
// front of a user is not merely "not drained", it is not touched. The rung
// applies no lifecycle call at all.
func TestExactConfigDriftPendingInteractionDefersWithZeroStopAndZeroStart(t *testing.T) {
	cityPath := t.TempDir()
	env := driftDeferEnv()
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.PreStart = append([]string{"echo stale-prestart"}, cfg.PreStart...)
		return cfg
	}, nil)
	env.sp.SetPendingInteraction("worker", &runtime.PendingInteraction{RequestID: "approval-1", Kind: "question"})

	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	// Baselined after the fixture's own Start, so only the admission's calls count.
	seedCalls := map[string]int{}
	for _, method := range []string{"Stop", "Start", "Relaunch", "RunLive"} {
		seedCalls[method] = env.sp.CountCalls(method, "worker")
	}
	admitDriftKey(t, env, params, session.ID)

	for _, method := range []string{"Stop", "Start", "Relaunch", "RunLive"} {
		if got := env.sp.CountCalls(method, "worker"); got != seedCalls[method] {
			t.Errorf("%s calls = %d, want %d (a pending-interaction deferral touches nothing)", method, got, seedCalls[method])
		}
	}
	if len(provider.stopCalls) != 0 {
		t.Errorf("unattended stops = %d, want 0", len(provider.stopCalls))
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("pending-interaction row was drained (reason=%q)", ds.reason)
	}
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("re-read row: %v", err)
	}
	if after.Metadata["state"] != before.Metadata["state"] ||
		after.Metadata["started_config_hash"] != before.Metadata["started_config_hash"] {
		t.Fatalf("pending-interaction deferral moved the row: %#v", after.Metadata)
	}
}

// TestExactConfigDriftDetachedRowStillConvergesWhileDeferralArmActs is the
// scope negative: the deferral is per-key, not a family-wide stand-down. With
// the A6 half acting, an unattended drifted row in the same cycle converges
// through the convergence half exactly as it did before.
func TestExactConfigDriftDetachedRowStillConvergesWhileDeferralArmActs(t *testing.T) {
	cityPath := t.TempDir()
	env := driftDeferEnv()
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.PreStart = append([]string{"echo stale-prestart"}, cfg.PreStart...)
		return cfg
	}, nil)

	admitDriftKey(t, env, params, session.ID)

	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "config-drift" {
		t.Fatalf("a detached drifted row did not converge: drain=%+v", ds)
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read converged row: %v", err)
	}
	if stored.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] != "" {
		t.Fatalf("a detached row was given an attached deferral stamp: %#v", stored.Metadata)
	}
	if stored.Metadata[namedSessionConfigDriftDeferredAtMetadata] != "" {
		t.Fatalf("a detached row was given a named deferral stamp: %#v", stored.Metadata)
	}
}

// TestExactConfigDriftDeferralRelaysToConvergenceAfterDetach is the relay: a
// deferral is a pause, not a pardon. Once the human detaches and the
// false-negative window elapses, the SAME drift — no second config edit, no new
// detection input — converges on the next admission. Without this the A6 arm
// would be an unbounded veto: a session that was attached once would never pick
// up a config change again.
func TestExactConfigDriftDeferralRelaysToConvergenceAfterDetach(t *testing.T) {
	cityPath := t.TempDir()
	env := driftDeferEnv()
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.PreStart = append([]string{"echo stale-prestart"}, cfg.PreStart...)
		return cfg
	}, nil)
	env.sp.SetAttached("worker", true)

	admitDriftKey(t, env, params, session.ID)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attached row was drained (reason=%q)", ds.reason)
	}
	deferred, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read deferred row: %v", err)
	}
	if deferred.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatal("attached rung left no stamp; the relay would be proving nothing")
	}

	// The human detaches. The stamp still holds the session for the length of the
	// false-negative window — that guard is the whole reason the stamp exists —
	// so the very next admission must still defer.
	env.sp.SetAttached("worker", false)
	admitDriftKey(t, env, params, session.ID)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("the false-negative window did not hold the session (reason=%q)", ds.reason)
	}

	// Past the window, with the config untouched: the same drift converges.
	env.clk.Time = env.clk.Now().Add(sessionAttachedConfigDriftFalseNegativeLimit + time.Second)
	admitDriftKey(t, env, params, session.ID)
	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "config-drift" {
		t.Fatalf("a detached row past its deferral window never converged: drain=%+v", ds)
	}
}

// TestLegacyConfigDriftDeferralArmsYieldToKeyedOwnedRow is the coexistence RED
// for the A6 half. Legacy's deferral arms and the keyed handler write the SAME
// two metadata keys from the same durable row on the same tick, so an
// un-yielding legacy refreshes the attached window a second time every tick for
// every attached drifted session — a durable write, and on a Dolt-backed city a
// commit, per session per tick.
func TestLegacyConfigDriftDeferralArmsYieldToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	env.sp.SetAttached("worker", true)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
		withLegacyConfigDriftDeferExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read attached row: %v", err)
	}
	if got := stored.Metadata[sessionAttachedConfigDriftDeferredAtMetadata]; got != "" {
		t.Fatalf("legacy stamped the attached window for a row the keyed deferral arm owns: %q", got)
	}
	// Yielding the deferral arm must not drop the row through the ladder into a
	// convergence effect — the failure mode this whole seam exists to prevent.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("legacy drained an attached row after yielding its deferral arm (reason=%q)", ds.reason)
	}
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
		t.Fatalf("legacy relaunched an attached row after yielding its deferral arm (%d calls)", got)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("legacy stopped an attached row after yielding its deferral arm")
	}
}

// TestLegacyConfigDriftDeferralNeverYieldsWithoutTheConvergenceYield is the
// no-lapse proof, and it is deliberately a PATHOLOGICAL wiring: the deferral
// bridge says "keyed owns this row" while the convergence bridge says it does
// not. Legacy's ladder falls THROUGH a skipped deferral arm into the
// convergence arms below it, so honoring the deferral yield alone would hand
// an attached human's session straight to a drain. The arm must refuse to stand
// down, keeping legacy's A6 protection continuous across the WD.8 -> WD.9
// handoff: on no tick is an attached session defended by neither writer.
func TestLegacyConfigDriftDeferralNeverYieldsWithoutTheConvergenceYield(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	env.sp.SetAttached("worker", true)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(sessionpkg.Info) bool { return false }),
		withLegacyConfigDriftDeferExclusion(func(sessionpkg.Info) bool { return true }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read attached row: %v", err)
	}
	if stored.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatalf("legacy stood its deferral arm down while its convergence arms were still live: %#v", stored.Metadata)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("an attached row was drained in the handoff window (reason=%q)", ds.reason)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatal("an attached row was stopped in the handoff window")
	}
}

// TestLegacyConfigDriftDeferralArmsStillRunForUnownedRows is the other half of
// the doctrine: the bridge is narrow. A row the keyed controller does not own
// keeps its legacy deferral for the whole WD wave, so installing the bridge
// cannot silently disable fleet-wide attached-user safety.
func TestLegacyConfigDriftDeferralArmsStillRunForUnownedRows(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	env.sp.SetAttached("worker", true)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(sessionpkg.Info) bool { return false }),
		withLegacyConfigDriftDeferExclusion(func(sessionpkg.Info) bool { return false }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read attached row: %v", err)
	}
	if stored.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatalf("legacy skipped the attached-deferral stamp for an unowned row: %#v", stored.Metadata)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("legacy drained an unowned attached row (reason=%q)", ds.reason)
	}
}

// TestNamedConfigDriftDeferralWindowIsReadIdenticallyByBothWriters pins the
// single source of truth the two writers share. Legacy starts the bounded
// window and the keyed handler starts the bounded window; if they disagreed on
// when a window has STARTED, one writer would re-stamp what the other wrote and
// the bounded rungs — the ones that exist so an unprovable-activity session
// eventually converges — would never expire.
func TestNamedConfigDriftDeferralWindowIsReadIdenticallyByBothWriters(t *testing.T) {
	const driftKey = "stored:current"
	stamp := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		info        sessionpkg.Info
		wantStarted bool
	}{
		{name: "no stamp", info: sessionpkg.Info{}},
		{name: "other key", info: sessionpkg.Info{
			ConfigDriftDeferredKey: "other", ConfigDriftDeferredAt: stamp.Format(time.RFC3339),
		}},
		{name: "key without timestamp", info: sessionpkg.Info{ConfigDriftDeferredKey: driftKey}},
		{name: "unparseable timestamp", info: sessionpkg.Info{
			ConfigDriftDeferredKey: driftKey, ConfigDriftDeferredAt: "not-a-time",
		}},
		{name: "started", info: sessionpkg.Info{
			ConfigDriftDeferredKey: driftKey, ConfigDriftDeferredAt: stamp.Format(time.RFC3339),
		}, wantStarted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, started := configDriftDeferralWindowStart(tc.info, driftKey)
			if started != tc.wantStarted {
				t.Fatalf("window started = %v, want %v", started, tc.wantStarted)
			}
			if started && !got.Equal(stamp) {
				t.Fatalf("window start = %v, want %v", got, stamp)
			}
			// The keyed handler stamps exactly when the window has not started —
			// the same condition legacy's bounded deferral stamps on.
			for _, reason := range []string{"activity_unknown", "recent_activity"} {
				if needs := namedSessionConfigDriftDeferralNeedsStamp(tc.info, driftKey, reason); needs == started {
					t.Errorf("needs-stamp(%s) = %v with started = %v; the two writers disagree on when the window opens", reason, needs, started)
				}
			}
			// Unconditional rungs bind forever and legacy writes nothing for them,
			// so neither may the keyed handler.
			if namedSessionConfigDriftDeferralNeedsStamp(tc.info, driftKey, "pending_interaction") {
				t.Error("an unconditional named rung stamped a bounded window it does not own")
			}
		})
	}
}
