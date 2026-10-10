package main

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The session field registry at run time (F1c): what the census proves from
// syntax, these prove by running the code. Every key a main flow writes to a
// session row is registered, computed keys included; and every built clear
// site, run on a row that holds its keys, leaves each one "".

// sessionKeyRecorder is a MemStore that records every key written to a session
// row, through any metadata-writing method. It embeds the MemStore, so the
// conditional writers and their stamp stay visible to the code under test.
type sessionKeyRecorder struct {
	*beads.MemStore
	keys map[string]string // key -> the write that first set it
}

func newSessionKeyRecorder(m *beads.MemStore) *sessionKeyRecorder {
	return &sessionKeyRecorder{MemStore: m, keys: map[string]string{}}
}

func (s *sessionKeyRecorder) reset() { s.keys = map[string]string{} }

func recordedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *sessionKeyRecorder) record(id, how string, kvs map[string]string) {
	b, err := s.Get(id)
	if err != nil || !session.IsSessionBeadOrRepairable(b) {
		return
	}
	s.recordBead(b, how, kvs)
}

func (s *sessionKeyRecorder) recordBead(b beads.Bead, how string, kvs map[string]string) {
	if !session.IsSessionBeadOrRepairable(b) {
		return
	}
	for k := range kvs {
		if _, seen := s.keys[k]; !seen {
			s.keys[k] = how
		}
	}
}

func (s *sessionKeyRecorder) Create(b beads.Bead) (beads.Bead, error) {
	s.recordBead(b, "Create", b.Metadata)
	return s.MemStore.Create(b)
}

func (s *sessionKeyRecorder) Update(id string, opts beads.UpdateOpts) error {
	s.record(id, "Update", opts.Metadata)
	return s.MemStore.Update(id, opts)
}

func (s *sessionKeyRecorder) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.record(id, "UpdateIfMatch", opts.Metadata)
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *sessionKeyRecorder) SetMetadata(id, key, value string) error {
	s.record(id, "SetMetadata", map[string]string{key: value})
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *sessionKeyRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	s.record(id, "SetMetadataBatch", kvs)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *sessionKeyRecorder) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	s.record(id, "CompareAndSetMetadataKey", map[string]string{key: next})
	return s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
}

func (s *sessionKeyRecorder) CloseAll(ids []string, kvs map[string]string) (int, error) {
	for _, id := range ids {
		s.record(id, "CloseAll", kvs)
	}
	return s.MemStore.CloseAll(ids, kvs)
}

// TestSessionFieldsFlowsWriteTheirKeys runs the main session flows on a
// recording store and checks each writes exactly the keys it must: a flow
// that silently stops writing, or starts writing more, fails here. (Every
// key a production write sends to a session row must also be registered,
// which F1b's guard checks in every test.) Each step's error fails the test.
func TestSessionFieldsFlowsWriteTheirKeys(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	type step struct {
		name  string
		want  string   // the exact keys, sorted
		sites []string // the registry sites ("file:Func") the step runs
		run   func(t *testing.T, rec *sessionKeyRecorder)
	}
	steps := []step{
		{"legacy tick: start and wake", "awake_started_at continuation_epoch continuation_reset_pending core_hash_breakdown creation_complete_at detached_at effective_sleep_after_idle generation instance_token last_woke_at live_hash pending_create_started_at requested_sleep_after_idle reset_committed_at runtime_lease_epoch runtime_lease_expires_at runtime_lease_flock runtime_lease_holder runtime_lease_ttl sleep_capability sleep_intent sleep_policy_fingerprint sleep_policy_source sleep_reason started_config_hash started_launch_hash started_live_hash started_provision_hash state state_reason wake_request wake_requested_at", []string{"internal/session/lifecycle_transition.go:PreWakePatch", "internal/session/lifecycle_transition.go:CommitStartedPatch", "cmd/gc/session_sleep.go:persistSleepPolicyMetadataInfo", "cmd/gc/session_sleep.go:reconcileDetachedAtInfo", "internal/session/runtime_lease.go:RuntimeLease.acquireRecord"}, func(t *testing.T, rec *sessionKeyRecorder) {
			env := newReconcilerTestEnv()
			env.city = t.TempDir()
			env.store = rec
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", false)
			b := env.createSessionBead("worker", "worker")
			env.markSessionCreating(&b)
			rec.reset()
			for range 3 {
				env.reconcile([]beads.Bead{fieldBead(t, rec, b.ID)})
				env.clk.Advance(time.Minute)
			}
		}},
		{"legacy drain-ack finalize", "last_woke_at pending_create_claim pending_create_started_at slept_at state state_reason", []string{"cmd/gc/session_reconciler.go:finalizeDrainAckStoppedSession"}, func(t *testing.T, rec *sessionKeyRecorder) {
			env := newReconcilerTestEnv()
			env.city = t.TempDir()
			env.store = rec
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			b := env.createSessionBead("worker", "worker")
			must(t, rec.SetMetadataBatch(b.ID, session.DrainAckStopPendingPatch(env.clk.Now().UTC())))
			rec.reset()
			finalizeDrainAckStoppedSession(env.city, env.cfg, rec, nil, env.sessionInfo(b.ID), "worker", false,
				newFakeDrainOps(), env.dt, env.clk, env.rec, io.Discard)
		}},
		{"v2 named create", "agent_name alias canonical_instance_name command configured_named_identity configured_named_mode configured_named_session continuation_epoch generation instance_token live_hash pending_create_claim pending_create_started_at session_name session_origin state synced_at template work_dir", []string{"cmd/gc/allocator_create_named.go:createEffects.writeNamed"}, func(t *testing.T, rec *sessionKeyRecorder) {
			cfg := mayorCity()
			h := newNamedHarness(t, t.TempDir(), nil)
			h.reserve(t, "c1")
			rec.reset()
			h.runAll(t, &createPass{cfg: cfg, store: rec}, namedPlan(t, cfg, "c1", "mayor"))
		}},
		{"v2 named reopen", "churn_count close_reason closed_at creation_complete_at gc.bound_step_id held_until last_woke_at live_hash pending_create_claim pending_create_started_at primed_at priming_attempted_at prompt_hash quarantined_until runtime_lease_expires_at runtime_lease_flock runtime_lease_holder runtime_lease_ttl sleep_intent sleep_reason started_config_hash started_live_hash startup_dialog_verified startup_kickoff_attempts startup_kickoff_started_at startup_kickoff_state state state_reason synced_at wait_hold wake_attempts wake_refused_event_at", []string{"cmd/gc/allocator_create_named.go:reopenNamed"}, func(t *testing.T, rec *sessionKeyRecorder) {
			cfg := mayorCity()
			seedClosedNamedRow(t, rec, cfg, nil)
			h := newNamedHarness(t, t.TempDir(), nil)
			h.reserve(t, "c1")
			plan := namedPlan(t, cfg, "c1", "mayor")
			plan.Named.BoundStepID = "gc-step"
			rec.reset()
			h.runAll(t, &createPass{cfg: cfg, store: rec}, plan)
		}},
		{"v2 heal effect", "stranded_event_emitted_at", []string{"cmd/gc/reconcile_arms_heals.go:armStrandedClear"}, func(t *testing.T, rec *sessionKeyRecorder) {
			heal := newHealCase(t, livenessAlive, desireKeep, "state", "active", strandedEventEmittedKey, now.Format(time.RFC3339))
			healRec := newSessionKeyRecorder(heal.store)
			if _, st := heal.run(t, nil, healRec); st.Outcome != settledLanded {
				t.Fatalf("v2 heal settled %+v", st)
			}
			rec.keys = healRec.keys
		}},
		{"waits, claim markers and the circuit breaker", "continuation_claim_nudge_at continuation_claim_nudge_count continuation_claim_nudge_generation continuation_claim_nudge_root continuation_claim_nudge_store_ref continuation_claim_nudge_work execution_claim_nudge_at execution_claim_nudge_count execution_claim_nudge_root execution_claim_nudge_store_ref execution_claim_nudge_work session_circuit_last_observed session_circuit_last_progress session_circuit_last_restart session_circuit_open_restart_count session_circuit_opened_at session_circuit_progress_signature session_circuit_reset_generation session_circuit_restarts session_circuit_state sleep_intent wait_hold wait_lookup_capped_at wait_lookup_capped_label wait_lookup_capped_limit wait_lookup_capped_source", []string{"cmd/gc/reconcile_steps_waits.go:clearSessionWaitHoldFenced", "cmd/gc/cmd_wait.go:doSessionWait", "internal/session/waits.go:StampWaitLookupCapMetadata", "cmd/gc/idle_nudge.go:writeContinuationClaimMarker", "cmd/gc/execution_backstop.go:writeExecutionClaimMarker", "cmd/gc/session_circuit_breaker.go:sessionCircuitBreaker.metadataLocked"}, func(t *testing.T, rec *sessionKeyRecorder) {
			id := fieldRow(t, rec.MemStore, "state", "active", "wait_hold", "true", "sleep_intent", "wait-hold")
			front := sessionFrontDoor(rec)
			must(t, clearSessionWaitHoldFenced(front, id))
			must(t, front.SetWaitHold(id, true, "wait"))
			stampWaitLookupCapDiagnostic(front, id, beads.LookupLimitError{Limit: 1}, now, "test")
			b := fieldBead(t, rec, id)
			target := backstopTarget{ID: "w-1", RootID: "r-1", StoreRef: "city", Generation: "1"}
			writeContinuationClaimMarker(rec, &b, target, 1, now, io.Discard)
			writeExecutionClaimMarker(rec, &b, target, 1, now, io.Discard)
			cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
			cb.RecordRestart("worker", now)
			must(t, persistSessionCircuitBreakerMetadata(front, id, cb, "worker", now))
		}},
		{"v2 nudges step", "idle_claim_nudge_at idle_claim_nudge_count idle_claim_nudge_trigger", []string{"cmd/gc/reconcile_steps_nudges.go:externalReadsLane.nudges"}, func(t *testing.T, rec *sessionKeyRecorder) {
			env, _ := nudgeStepsFixture(t)
			sess := idleClaimPoolSession()
			sess.Labels = []string{sessionBeadLabel}
			_, err := rec.MemStore.Create(sess)
			must(t, err)
			env.CityStore = rec
			lane, _ := newPoolStepsTestLane(env, func() *allocSummary {
				return &allocSummary{At: stepsTestNow, AssignedWork: []beads.Bead{{ID: "work-a", Status: "open"}}}
			}, nil)
			rec.reset()
			lane.nudges(context.Background(), env)
		}},
	}
	// The operator's verbs, in an order each accepts, on one row.
	var m *session.Manager
	var id string
	opCity := t.TempDir()
	steps = append(steps, []step{
		{"operator create (bead only)", "command continuation_epoch generation instance_token pending_create_claim pending_create_started_at provider resume_command resume_flag resume_style session_id_flag session_name session_origin state template work_dir", []string{"internal/session/manager.go:Manager.createBeadOnly"}, func(t *testing.T, rec *sessionKeyRecorder) {
			m = session.NewManagerWithOptions(rec, runtime.NewFake(), session.WithCityPath(opCity))
			_, err := m.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "test-cmd", WorkDir: t.TempDir(), Provider: "fake", BeadOnly: true})
			must(t, err)
			id = fieldRow(t, rec.MemStore, "state", "asleep", "sleep_reason", "idle") // the dormant row the later verbs act on
		}},
		{"operator template overrides on the dormant row", "opt_model template_overrides", []string{"internal/session/manager.go:Manager.UpdateTemplateOverrides"}, func(t *testing.T, _ *sessionKeyRecorder) {
			_, err := m.UpdateTemplateOverrides(id, map[string]string{"model": "m"})
			must(t, err)
		}},
		{"operator session key", "session_key", []string{"internal/session/manager.go:Manager.PersistSessionKey"}, func(t *testing.T, _ *sessionKeyRecorder) { must(t, m.PersistSessionKey(id, "key-2")) }},
		{"operator wake", "churn_count held_until quarantined_until sleep_intent wait_hold wake_attempts wake_refused_event_at wake_request wake_requested_at", []string{"internal/session/wait_store.go:Store.wakeSessionFromBead"}, func(t *testing.T, rec *sessionKeyRecorder) {
			_, err := sessionFrontDoor(rec).WakeSession(id, now, session.WakeOpts{})
			must(t, err)
		}},
		{"operator fresh restart", "continuation_reset_pending restart_requested", []string{"internal/session/manager.go:Manager.RequestFreshRestart"}, func(t *testing.T, _ *sessionKeyRecorder) { must(t, m.RequestFreshRestart(id)) }},
		{"operator suspend", "held_until runtime_lease_epoch runtime_lease_expires_at runtime_lease_flock runtime_lease_holder runtime_lease_ttl sleep_intent sleep_reason slept_at state suspended_at wake_request wake_requested_at", []string{"internal/session/manager.go:Manager.suspend", "internal/session/runtime_lease.go:RuntimeLease.acquireRecord"}, func(t *testing.T, _ *sessionKeyRecorder) { must(t, m.Suspend(id)) }},
		{"operator kill fence", "held_until last_woke_at pending_create_claim pending_create_started_at sleep_intent sleep_reason slept_at state state_reason suspended_at synced_at wake_request wake_requested_at", []string{"internal/session/kill_fence.go:KillPendingPatch", "cmd/gc/cmd_session_kill_fence.go:writeSessionKillFence"}, func(t *testing.T, rec *sessionKeyRecorder) {
			_, err := writeSessionKillFence(rec, id, now)
			must(t, err)
		}},
		{"operator set state", "state state_reason", []string{"internal/session/store.go:Store.SetState"}, func(t *testing.T, rec *sessionKeyRecorder) {
			must(t, sessionFrontDoor(rec).SetState(id, session.StateAsleep, "test"))
		}},
		{"operator restart request (the legacy patch)", "continuation_reset_pending last_woke_at pending_create_claim pending_create_started_at primed_at priming_attempted_at prompt_hash reset_committed_at restart_requested session_key started_config_hash", []string{"internal/session/lifecycle_transition.go:RestartRequestPatch"}, func(t *testing.T, rec *sessionKeyRecorder) {
			must(t, sessionFrontDoor(rec).ApplyPatch(id, session.RestartRequestPatch("key-3", now)))
		}},
		{"operator config-drift reset (the legacy patch)", "continuation_reset_pending last_woke_at live_hash pending_create_claim pending_create_started_at primed_at priming_attempted_at prompt_hash restart_requested session_key started_config_hash started_live_hash startup_dialog_verified state", []string{"internal/session/lifecycle_transition.go:ConfigDriftResetPatch"}, func(t *testing.T, rec *sessionKeyRecorder) {
			must(t, sessionFrontDoor(rec).ApplyPatch(id, session.ConfigDriftResetPatch(session.StateAsleep, "key-4", now)))
		}},
	}...)
	declared := map[string]map[string]bool{} // "file:Func" -> the keys it writes or clears
	for _, f := range session.Fields() {
		for _, s := range append(append([]session.Site{}, f.Writers...), f.Clears...) {
			if s.Func != "" {
				if declared[s.File+":"+s.Func] == nil {
					declared[s.File+":"+s.Func] = map[string]bool{}
				}
				declared[s.File+":"+s.Func][f.Key] = true
			}
		}
	}
	m0, _ := stampedMem(t, gate.Require)
	opRec := newSessionKeyRecorder(m0)
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			rec := opRec
			if !strings.HasPrefix(st.name, "operator") {
				m, _ := stampedMem(t, gate.Require)
				rec = newSessionKeyRecorder(m)
			}
			rec.reset()
			st.run(t, rec)
			if got := strings.Join(recordedKeys(rec.keys), " "); got != st.want {
				t.Errorf("wrote %q, want %q", got, st.want)
			}
			for k := range rec.keys {
				if f, ok := session.LookupField(k); !ok {
					t.Errorf("wrote %s, which the session field registry does not have", k)
				} else if !slices.ContainsFunc(st.sites, func(site string) bool { return declared[site][f.Key] }) {
					t.Errorf("wrote %s, which the registry declares at none of %v", k, st.sites)
				}
			}
		})
	}
}

// TestSessionFieldsClearSitesClear runs every built clear site the registry
// names, every request and stop-request clear point among them, on a row
// holding its keys and checks each reads "" afterwards: a patch that drops a
// key instead of writing "" leaves it set on the store. A built site still
// pending (NEW-1's awake heal) must leave its keys set, so the fix that makes
// it clear must also drop its Pending.
func TestSessionFieldsClearSitesClear(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	patchSite := func(fn func(session.Info) session.MetadataPatch) func(*testing.T, []string) map[string]string {
		return func(t *testing.T, meta []string) map[string]string {
			m, _ := stampedMem(t, gate.Require)
			id := fieldRow(t, m, meta...)
			if err := m.SetMetadataBatch(id, fn(fieldInfo(t, m, id))); err != nil {
				t.Fatal(err)
			}
			return fieldBead(t, m, id).Metadata
		}
	}
	armSite := func(arm func(*rowFacts) (intent, bool)) func(*testing.T, []string) map[string]string {
		return func(t *testing.T, meta []string) map[string]string {
			c := newHealCase(t, livenessAlive, desireKeep, meta...)
			it, _ := arm(&rowFacts{w: c.w, k: c.k, row: c.w.Census.Rows[c.k], entry: c.a.Snapshot.Entries[c.k]})
			if err := c.store.SetMetadataBatch(c.k.ID, it.Patch); err != nil {
				t.Fatal(err)
			}
			return c.meta(t)
		}
	}
	sites := map[string]struct {
		meta []string // the row: each cleared key set, and what the site needs to act
		run  func(*testing.T, []string) map[string]string
	}{
		"cmd/gc/session_reconciler.go:finalizeDrainAckStoppedSession": {[]string{"restart_requested", "true"}, func(t *testing.T, meta []string) map[string]string {
			env := newReconcilerTestEnv()
			env.city = t.TempDir()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			b := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&b, pairs(meta))
			env.setSessionMetadata(&b, session.DrainAckStopPendingPatch(env.clk.Now().UTC()))
			finalizeDrainAckStoppedSession(env.city, env.cfg, env.store, nil, env.sessionInfo(b.ID), "worker", false,
				newFakeDrainOps(), env.dt, env.clk, env.rec, io.Discard)
			return fieldBead(t, env.store, b.ID).Metadata
		}},
		"internal/session/lifecycle_transition.go:PreWakePatch": {
			[]string{"wake_request", "explicit", "wake_requested_at", ago(time.Minute), "detached_at", ago(time.Hour), "sleep_intent", "idle-stop-pending"},
			patchSite(func(session.Info) session.MetadataPatch {
				return session.PreWakePatch(session.PreWakePatchInput{Generation: 4, InstanceToken: "tok-4", Now: now})
			}),
		},
		"internal/session/lifecycle_transition.go:CommitStartedPatch": {
			[]string{"continuation_reset_pending", "true", session.ResetCommittedAtKey, ago(time.Minute)},
			patchSite(func(session.Info) session.MetadataPatch {
				return session.CommitStartedPatch(session.CommitStartedPatchInput{CoreHash: "h", Now: now})
			}),
		},
		"cmd/gc/allocator_create_named.go:reopenNamed": {
			[]string{session.RuntimeLeaseHolderKey, "host/1/n", session.RuntimeLeaseExpiresKey, ago(-time.Minute), session.RuntimeLeaseTTLKey, "60", session.RuntimeLeaseFlockKey, "f"},
			func(t *testing.T, meta []string) map[string]string {
				cfg := mayorCity()
				store := newNamedCondStore(t)
				closed := seedClosedNamedRow(t, store, cfg, pairs(meta))
				h := newNamedHarness(t, t.TempDir(), nil)
				h.reserve(t, "c1")
				h.runAll(t, &createPass{cfg: cfg, store: store}, namedPlan(t, cfg, "c1", "mayor"))
				return fieldBead(t, store, closed.ID).Metadata
			},
		},
		"internal/session/lifecycle_transition.go:ConfigDriftResetPatch": {
			[]string{"restart_requested", "true"},
			patchSite(func(session.Info) session.MetadataPatch {
				return session.ConfigDriftResetPatch(session.StateAsleep, "key", now)
			}),
		},
		"internal/session/kill_fence.go:KillPendingPatch": {
			[]string{"wake_request", "explicit", "wake_requested_at", ago(time.Minute)},
			patchSite(func(session.Info) session.MetadataPatch { return session.KillPendingPatch(now) }),
		},
		"internal/session/resume_user_hold.go:Manager.consumeUserHold": {
			[]string{"state", "suspended", "sleep_intent", "user-hold", "held_until", ago(-time.Hour), "suspended_at", ago(time.Minute), "provider", "claude"},
			func(t *testing.T, meta []string) map[string]string {
				m, _ := stampedMem(t, gate.Require)
				id := fieldRow(t, m, meta...)
				if err := session.NewManagerWithOptions(m, runtime.NewFake()).Attach(context.Background(), id, "claude", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				return fieldBead(t, m, id).Metadata
			},
		},
		"cmd/gc/cmd_session_kill_fence.go:writeSessionKillFence": {
			[]string{"state", "active", "sleep_intent", "user-hold", "held_until", ago(-time.Hour)},
			func(t *testing.T, meta []string) map[string]string {
				m, _ := stampedMem(t, gate.Require)
				id := fieldRow(t, m, meta...)
				if _, err := writeSessionKillFence(m, id, now); err != nil {
					t.Fatal(err)
				}
				return fieldBead(t, m, id).Metadata
			},
		},
		"internal/session/manager.go:Manager.suspend": {
			[]string{"state", "asleep", "wake_request", "explicit", "wake_requested_at", ago(time.Minute)},
			func(t *testing.T, meta []string) map[string]string {
				m, _ := stampedMem(t, gate.Require)
				id := fieldRow(t, m, meta...)
				if err := session.NewManagerWithOptions(m, runtime.NewFake(), session.WithCityPath(t.TempDir())).Suspend(id); err != nil {
					t.Fatal(err)
				}
				return fieldBead(t, m, id).Metadata
			},
		},
		"internal/session/lifecycle_transition.go:RestartRequestPatch": {
			[]string{"restart_requested", "true"},
			patchSite(func(session.Info) session.MetadataPatch { return session.RestartRequestPatch("key", now) }),
		},
		"internal/session/lifecycle_transition.go:ClearExpiredHoldPatch": {
			[]string{"held_until", ago(time.Minute)},
			patchSite(func(i session.Info) session.MetadataPatch { return session.ClearExpiredHoldPatch(i.SleepReason) }),
		},
		"cmd/gc/reconcile_arms_heals.go:armClaimClear": {
			[]string{"state", "active", "pending_create_claim", "true", "pending_create_started_at", ago(time.Minute)},
			armSite(armClaimClear),
		},
		"cmd/gc/reconcile_arms_heals.go:armStabilityClear": {
			[]string{"state", "active", "last_woke_at", ago(time.Hour), "quarantined_until", ago(-time.Hour)},
			armSite(armStabilityClear),
		},
		"cmd/gc/reconcile_arms_heals.go:armAwakeHeal": {
			[]string{"state", "asleep", "wake_request", "explicit", "wake_requested_at", ago(time.Minute)},
			func(t *testing.T, meta []string) map[string]string {
				c := ownRuntimeCase(t, "tok-3", meta...)
				it, ok := armAwakeHeal(&rowFacts{w: c.w, k: c.k, row: c.w.Census.Rows[c.k], entry: c.a.Snapshot.Entries[c.k]})
				if !ok || it.Patch["state"] != "awake" {
					t.Fatalf("the awake heal did not fire: %+v", it)
				}
				if err := c.store.SetMetadataBatch(c.k.ID, it.Patch); err != nil {
					t.Fatal(err)
				}
				return c.meta(t)
			},
		},
		"cmd/gc/reconcile_arms_heals.go:armStrandedClear": {
			[]string{"state", "active", strandedEventEmittedKey, ago(time.Hour)},
			armSite(armStrandedClear),
		},
		"cmd/gc/reconcile_arms_heals.go:asleepHealPatch": {
			[]string{
				"state", "active", "session_key", "k", "started_config_hash", "h",
				session.PrimedAtMetadataKey, ago(time.Hour), session.PrimingAttemptedAtMetadataKey, ago(time.Hour), session.PromptHashMetadataKey, "p",
			},
			patchSite(asleepHealPatch),
		},
		"cmd/gc/reconcile_session_decide.go:timerHealPatch": {
			[]string{"state", "active", unknownStateFirstSeenKey, ago(time.Hour), unknownStateValueKey, "weird", unknownStateEscalatedKey, ago(time.Minute)},
			patchSite(func(i session.Info) session.MetadataPatch { p, _ := timerHealPatch(i, now); return p }),
		},
		"cmd/gc/reconcile_steps_waits.go:clearSessionWaitHoldFenced": {
			[]string{"wait_hold", "true", "sleep_intent", "wait-hold", "sleep_reason", "wait-hold"},
			func(t *testing.T, meta []string) map[string]string {
				m, _ := stampedMem(t, gate.Require)
				id := fieldRow(t, m, meta...)
				if err := clearSessionWaitHoldFenced(sessionFrontDoor(m), id); err != nil {
					t.Fatal(err)
				}
				return fieldBead(t, m, id).Metadata
			},
		},
		"cmd/gc/reconcile_stop_request.go:stopCancelPatch": {
			[]string{session.DrainIntentReasonKey, "idle", session.DrainIntentAtKey, ago(time.Minute), session.DrainIntentIncarnationKey, "3"},
			patchSite(func(session.Info) session.MetadataPatch { return stopCancelPatch() }),
		},
		"cmd/gc/reconcile_stop_request.go:stopVoidResiduePatch": {
			[]string{session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, ago(time.Minute)},
			patchSite(func(session.Info) session.MetadataPatch { return stopVoidResiduePatch() }),
		},
	}
	declared := map[string][]string{} // a built clear site -> the keys it must clear
	pending := map[string]bool{}      // a built site whose clear is still pending
	for _, f := range session.Fields() {
		for _, s := range f.Clears {
			if s.Func != "" {
				declared[s.File+":"+s.Func] = append(declared[s.File+":"+s.Func], f.Key)
				pending[s.File+":"+s.Func] = s.Pending != ""
			}
		}
	}
	for site := range sites {
		if declared[site] == nil {
			t.Errorf("%s: no longer a registered clear site; drop its row", site)
		}
	}
	for site, keys := range declared {
		tc, ok := sites[site]
		if !ok {
			t.Errorf("%s: a registered clear site with no row here; add one that runs it", site)
			continue
		}
		t.Run(site, func(t *testing.T) {
			fixture := pairs(tc.meta)
			for _, k := range keys {
				if fixture[k] == "" {
					t.Fatalf("the row does not hold %s, so its clear proves nothing", k)
				}
			}
			got := tc.run(t, tc.meta)
			for _, k := range keys {
				switch {
				case pending[site] && got[k] == "":
					t.Errorf("%s now clears %s; drop its Pending in internal/session/fields.go", site, k)
				case !pending[site] && got[k] != "":
					t.Errorf("%s still reads %q after the site ran", k, got[k])
				}
			}
		})
	}
}

func pairs(kv []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func fieldRow(t *testing.T, s beads.Store, meta ...string) string {
	t.Helper()
	b, err := s.Create(sessionRow("row", append([]string{"template", "worker", "session_name", "s-row", "generation", "3", "instance_token", "tok-3"}, meta...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func fieldBead(t *testing.T, s beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fieldInfo(t *testing.T, s beads.Store, id string) session.Info {
	t.Helper()
	info, err := sessionFrontDoor(s).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
