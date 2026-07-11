package main

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// setMetadataBatchFailStore rejects every SetMetadataBatch so the persist
// swallow contract can be exercised: on a write error the snapshot must not
// advance.
type setMetadataBatchFailStore struct {
	beads.Store
}

func (s setMetadataBatchFailStore) SetMetadataBatch(string, map[string]string) error {
	return errors.New("injected SetMetadataBatch failure")
}

// healOracleCase is a heal fixture plus the runtime/clock/lease knobs the heal
// patch reads and the exact batch the Info form must return.
type healOracleCase struct {
	name     string
	status   string
	created  time.Duration // relative to clk.Now(); 0 = zero time
	meta     map[string]string
	alive    bool
	timeout  time.Duration
	rollback bool
	want     map[string]string
}

// TestHealStatePatchWithRollbackInfo pins healStatePatchWithRollbackInfo against
// explicit expected batches across every heal branch (drained fast path,
// start-request, failed-create preserve/clear, stale-creating rollback,
// reset-continuation clears, named-session mode guard, deferred-rollback). Each
// row is load-bearing: a mutation of the corresponding non-trivial branch flips
// the batch. The expected batches were captured from the WI-6-R2 raw
// healStatePatchWithRollback before it was deleted in R3 (byte-identical oracle).
func TestHealStatePatchWithRollbackInfo(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	rfc := func(d time.Duration) string { return clk.Now().Add(d).UTC().Format(time.RFC3339) }
	// S19 stage 2: every started_config_hash clear also clears the priming keys
	// (pinned by the write-site/priming-lifetime gates), so the reset batches
	// carry the primed_at/priming_attempted_at/prompt_hash clears.
	resetBatch := map[string]string{
		"continuation_reset_pending": "true", "pending_create_claim": "", "pending_create_started_at": "",
		"primed_at": "", "priming_attempted_at": "", "prompt_hash": "",
		"session_key": "", "sleep_reason": "runtime-missing", "started_config_hash": "", "state": "asleep",
	}

	cases := []healOracleCase{
		{name: "asleep-alive", meta: map[string]string{"state": "asleep"}, alive: true, rollback: true, want: map[string]string{"state": "awake"}},
		{name: "active-dead-drains", meta: map[string]string{"state": "active"}, alive: false, rollback: true, want: map[string]string{"state": "asleep"}},
		{name: "asleep-dead-noop", meta: map[string]string{"state": "asleep", "sleep_reason": "idle"}, alive: false, rollback: true, want: nil},
		{
			name:     "creating-inflight-preserves",
			created:  -30 * time.Second,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-30 * time.Second)},
			alive:    false,
			rollback: true,
			want:     nil,
		},
		{
			name:     "stale-creating-clears-lease",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-2 * time.Minute)},
			alive:    false,
			rollback: true,
			want:     resetBatch,
		},
		{
			name:     "stale-creating-rollback-deferred",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-2 * time.Minute)},
			alive:    false,
			rollback: false,
			want:     map[string]string{"state": "asleep"},
		},
		{
			name:     "never-started-inflight",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "pending_create_started_at": rfc(-2 * time.Minute)},
			alive:    false,
			timeout:  90 * time.Second,
			rollback: true,
			want:     map[string]string{"state": "start-pending"},
		},
		{
			name:     "never-started-expired",
			created:  -20 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "pending_create_started_at": rfc(-20 * time.Minute)},
			alive:    false,
			rollback: true,
			want:     resetBatch,
		},
		{
			name:     "failed-create-active-lease-preserves",
			created:  -30 * time.Second,
			meta:     map[string]string{"state": "failed-create", "pending_create_claim": "true", "last_woke_at": rfc(-30 * time.Second)},
			alive:    false,
			timeout:  90 * time.Second,
			rollback: true,
			want:     nil,
		},
		{
			name:     "failed-create-no-claim-heals-asleep",
			meta:     map[string]string{"state": "failed-create"},
			alive:    false,
			rollback: true,
			want:     map[string]string{"sleep_reason": "failed-create", "state": "asleep"},
		},
		{
			name:     "failed-create-expired-lease-clears",
			created:  -20 * time.Minute,
			meta:     map[string]string{"state": "failed-create", "pending_create_claim": "true", "pending_create_started_at": rfc(-20 * time.Minute)},
			alive:    false,
			rollback: true,
			want:     map[string]string{"pending_create_claim": "", "pending_create_started_at": "", "sleep_reason": "failed-create", "state": "asleep"},
		},
		{
			name:     "always-named-preserves-session-key",
			meta:     map[string]string{"state": "active", "configured_named_session": "true", "configured_named_identity": "mayor", "configured_named_mode": "always", "session_name": "mayor", "session_key": "sk", "started_config_hash": "h"},
			alive:    false,
			rollback: true,
			want:     map[string]string{"sleep_reason": "runtime-missing", "state": "asleep"},
		},
		{
			name:     "singleton-named-resets-continuation",
			meta:     map[string]string{"state": "active", "configured_named_session": "true", "configured_named_identity": "mayor", "configured_named_mode": "singleton", "session_name": "mayor", "session_key": "sk", "started_config_hash": "h"},
			alive:    false,
			rollback: true,
			want:     map[string]string{"continuation_reset_pending": "true", "primed_at": "", "priming_attempted_at": "", "prompt_hash": "", "session_key": "", "sleep_reason": "runtime-missing", "started_config_hash": "", "state": "asleep"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b := makeBead("ga-"+tc.name, cloneStringMap(tc.meta))
			if tc.status != "" {
				b.Status = tc.status
			}
			if tc.created != 0 {
				b.CreatedAt = clk.Now().Add(tc.created)
			}
			got := healStatePatchWithRollbackInfo(seedSessionInfo(b), tc.alive, clk, tc.timeout, tc.rollback)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("healStatePatchWithRollbackInfo = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestHealStateWithRollbackInfoClosedGuardAndWrite pins the wrapper: closed beads
// are a no-op (matches the raw session.Status=="closed" guard via Info.Closed),
// and a healing patch is persisted through the front door.
func TestHealStateWithRollbackInfoClosedGuardAndWrite(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	closed, err := store.Create(beads.Bead{Title: "c", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(closed.ID, beads.UpdateOpts{Status: strPtr("closed")}); err != nil {
		t.Fatal(err)
	}
	closed, _ = store.Get(closed.ID)
	if batch := healStateWithRollbackInfo(sessiontest.SeedBead(t, closed), false, sessionFrontDoor(store), clk, 0, true); batch != nil {
		t.Fatalf("closed bead heal batch = %#v, want nil (terminal beads must not move)", batch)
	}

	live, err := store.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	batch := healStateWithRollbackInfo(sessiontest.SeedBead(t, live), false, sessionFrontDoor(store), clk, 0, true)
	if batch["state"] != "asleep" {
		t.Fatalf("heal batch = %#v, want state=asleep", batch)
	}
	got, _ := store.Get(live.ID)
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("persisted state = %q, want asleep (front-door write must land)", got.Metadata["state"])
	}
}

// TestPersistSleepPolicyMetadataInfo pins the seven-key persist: the folded Info
// equals the re-projection of the persisted bead (write-returns-Info), and the
// fingerprint-preservation branch keeps the in-flight idle-drain fingerprint.
func TestPersistSleepPolicyMetadataInfo(t *testing.T) {
	cfg := &config.City{SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"}, Agents: []config.Agent{{Name: "worker"}}}
	sp := routedSleepProvider{Provider: runtime.NewFake(), capabilities: runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true}, sleep: runtime.SessionSleepCapabilityFull}

	shapes := map[string]map[string]string{
		"fresh":               {"template": "worker", "session_name": "worker-a", "state": "active"},
		"idle-drain-inflight": {"template": "worker", "session_name": "worker-b", "state": "asleep", "sleep_reason": "idle", "sleep_policy_fingerprint": "pinned-fp"},
		"intent-pending":      {"template": "worker", "session_name": "worker-c", "sleep_intent": "idle-stop-pending", "sleep_policy_fingerprint": "pinned-fp"},
	}
	for name, meta := range shapes {
		for _, suppressed := range []bool{false, true} {
			name, meta, suppressed := name, meta, suppressed
			t.Run(name, func(t *testing.T) {
				store := beads.NewMemStore()
				bead, err := store.Create(beads.Bead{Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
				if err != nil {
					t.Fatal(err)
				}
				policy := resolveSessionSleepPolicyInfo(sessiontest.SeedBead(t, bead), cfg, sp)
				got := persistSleepPolicyMetadataInfo(sessiontest.SeedBead(t, bead), sessionFrontDoor(store), policy, suppressed)

				persisted, _ := store.Get(bead.ID)
				// Write-returns-Info: local fold == re-projection of the persisted bead.
				if want := sessiontest.SeedBead(t, persisted); !reflect.DeepEqual(got, want) {
					t.Fatalf("folded Info diverged from re-projection:\n got = %#v\nwant = %#v", got, want)
				}
				// The seven policy keys landed with the resolved policy values.
				if persisted.Metadata["config_wake_suppressed"] != boolMetadata(suppressed) {
					t.Errorf("config_wake_suppressed = %q, want %q", persisted.Metadata["config_wake_suppressed"], boolMetadata(suppressed))
				}
				if persisted.Metadata["effective_sleep_after_idle"] != policy.Effective {
					t.Errorf("effective_sleep_after_idle = %q, want %q", persisted.Metadata["effective_sleep_after_idle"], policy.Effective)
				}
				// Fingerprint-preservation: the in-flight idle-drain shapes keep "pinned-fp".
				if meta["sleep_policy_fingerprint"] == "pinned-fp" {
					if persisted.Metadata["sleep_policy_fingerprint"] != "pinned-fp" {
						t.Errorf("sleep_policy_fingerprint = %q, want preserved pinned-fp", persisted.Metadata["sleep_policy_fingerprint"])
					}
				} else if persisted.Metadata["sleep_policy_fingerprint"] != policy.Fingerprint {
					t.Errorf("sleep_policy_fingerprint = %q, want resolved %q", persisted.Metadata["sleep_policy_fingerprint"], policy.Fingerprint)
				}
			})
		}
	}
}

// TestPersistSleepPolicyMetadataInfoSwallowsWriteError pins §3c: on an
// ApplyPatch failure the returned Info equals the INPUT byte-for-byte and no
// partial fold leaks.
func TestPersistSleepPolicyMetadataInfoSwallowsWriteError(t *testing.T) {
	cfg := &config.City{SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"}, Agents: []config.Agent{{Name: "worker"}}}
	sp := routedSleepProvider{Provider: runtime.NewFake(), capabilities: runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true}, sleep: runtime.SessionSleepCapabilityFull}
	base := beads.NewMemStore()
	bead, err := base.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"template": "worker", "session_name": "worker-a", "state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	policy := resolveSessionSleepPolicyInfo(sessiontest.SeedBead(t, bead), cfg, sp)
	in := sessiontest.SeedBead(t, bead)
	// A change IS pending (the seven policy keys are absent), so only the write
	// error prevents the fold.
	front := sessionFrontDoor(setMetadataBatchFailStore{Store: base})
	got := persistSleepPolicyMetadataInfo(in, front, policy, true)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("on write error, returned Info must equal input unchanged:\n got = %#v\n in = %#v", got, in)
	}
}

// TestSleepWriteTwinsInfo pins the Info-form write helpers markIdleSleepPendingInfo,
// recoverPendingIdleSleepInfo, and reconcileDetachedAtInfo against explicit
// store outcomes.
func TestSleepWriteTwinsInfo(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	newBead := func(t *testing.T, meta map[string]string) (beads.Store, beads.Bead) {
		t.Helper()
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
		if err != nil {
			t.Fatal(err)
		}
		return store, b
	}

	t.Run("markIdleSleepPending-fresh", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "state": "active"})
		got := markIdleSleepPendingInfo(sessiontest.SeedBead(t, b), sessionFrontDoor(store))
		if !reflect.DeepEqual(got, sessionpkg.MetadataPatch{"sleep_intent": "idle-stop-pending"}) {
			t.Fatalf("patch = %#v, want sleep_intent=idle-stop-pending", got)
		}
		persisted, _ := store.Get(b.ID)
		if persisted.Metadata["sleep_intent"] != "idle-stop-pending" {
			t.Fatalf("persisted sleep_intent = %q, want idle-stop-pending", persisted.Metadata["sleep_intent"])
		}
	})
	t.Run("markIdleSleepPending-noop", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "sleep_intent": "idle-stop-pending"})
		if got := markIdleSleepPendingInfo(sessiontest.SeedBead(t, b), sessionFrontDoor(store)); got != nil {
			t.Fatalf("patch = %#v, want nil (already pending)", got)
		}
	})
	t.Run("recoverPendingIdleSleep-recovers", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "state": "active", "sleep_intent": "idle-stop-pending", "sleep_policy_fingerprint": "fp"})
		if !recoverPendingIdleSleepInfo(sessiontest.SeedBead(t, b), sessionFrontDoor(store), false, clk) {
			t.Fatal("recoverPendingIdleSleepInfo = false, want true")
		}
		persisted, _ := store.Get(b.ID)
		if persisted.Metadata["state"] != "asleep" || persisted.Metadata["sleep_reason"] != "idle" {
			t.Fatalf("persisted state/reason = %q/%q, want asleep/idle", persisted.Metadata["state"], persisted.Metadata["sleep_reason"])
		}
		if persisted.Metadata["sleep_policy_fingerprint"] != "fp" {
			t.Fatalf("sleep_policy_fingerprint = %q, want preserved fp", persisted.Metadata["sleep_policy_fingerprint"])
		}
	})
	t.Run("recoverPendingIdleSleep-noop", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "state": "active"})
		if recoverPendingIdleSleepInfo(sessiontest.SeedBead(t, b), sessionFrontDoor(store), false, clk) {
			t.Fatal("recoverPendingIdleSleepInfo = true, want false (no pending intent)")
		}
	})
	t.Run("reconcileDetachedAt-clears-when-disabled", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "state": "active", "detached_at": clk.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
		// A NonInteractive policy takes the early clear branch (no runtime probe).
		policy := resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive}
		got := reconcileDetachedAtInfo(sessiontest.SeedBead(t, b), store, policy, true, runtime.NewFake(), clk)
		if !reflect.DeepEqual(got, map[string]string{"detached_at": ""}) {
			t.Fatalf("detach batch = %#v, want detached_at cleared", got)
		}
		persisted, _ := store.Get(b.ID)
		if persisted.Metadata["detached_at"] != "" {
			t.Fatalf("persisted detached_at = %q, want cleared", persisted.Metadata["detached_at"])
		}
	})
	t.Run("reconcileDetachedAt-noop-when-absent", func(t *testing.T) {
		store, b := newBead(t, map[string]string{"session_name": "worker", "state": "active"})
		policy := resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive}
		if got := reconcileDetachedAtInfo(sessiontest.SeedBead(t, b), store, policy, true, runtime.NewFake(), clk); got != nil {
			t.Fatalf("detach batch = %#v, want nil (nothing to clear)", got)
		}
	})
}

// TestPendingInteractionKeepsAwakeInfoReflectsMidTickQuarantineClear is the R3
// anti-drift pin. The W6 red-team caught a SPLIT decision: a mid-tick
// clearWakeFailures cleared quarantined_until on the typed snapshot, but the
// downstream kill/drain deferral (pendingInteractionKeepsAwake) read
// quarantined_until off the STALE raw bead — so the lifecycle blocker (from the
// cleared snapshot) and the pending-interaction read (from the stale mirror)
// disagreed, and a live user interaction lost its deferral. R3 makes BOTH reads
// consult the same Info: clearWakeFailures folds the clear onto the snapshot, and
// pendingInteractionKeepsAwakeInfo reads the SAME folded snapshot, so the pending
// interaction keeps the session awake. This test would fail if a reader still
// read a stale, un-cleared quarantine (i.e. mirror #1 dropped without migrating
// its reader).
func TestPendingInteractionKeepsAwakeInfoReflectsMidTickQuarantineClear(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "witness",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":      "witness",
			"state":             "active",
			"wake_attempts":     "3",
			"quarantined_until": clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	sp.SetPendingInteraction("witness", &runtime.PendingInteraction{RequestID: "r", Kind: "question", Prompt: "approve?"})

	info := sessiontest.SeedBead(t, bead)
	// Precondition: while the still-future quarantine is present, the quarantine
	// blocker suppresses the pending-interaction deferral.
	if pendingInteractionKeepsAwakeInfo(info, sp, "witness", clk) {
		t.Fatal("with a live quarantine present, pendingInteractionKeepsAwakeInfo must return false (BlockerQuarantined) — precondition unmet")
	}

	// Mid-tick clear (clearWakeFailures folds quarantined_until="" onto the snapshot).
	cleared := clearWakeFailures(info, sessionFrontDoor(store))
	if cleared.QuarantinedUntil != "" {
		t.Fatalf("clearWakeFailures did not clear QuarantinedUntil on the snapshot: %q", cleared.QuarantinedUntil)
	}
	// Anti-drift: the SAME folded snapshot the blocker read cleared is what the
	// pending-interaction reader consults, so the deferral now engages. No split.
	if !pendingInteractionKeepsAwakeInfo(cleared, sp, "witness", clk) {
		t.Fatal("after the mid-tick quarantine clear, pendingInteractionKeepsAwakeInfo(cleared) = false; the reader must read the cleared snapshot (not a stale mirror) so the live interaction defers the kill/drain — W6 split-decision drift regressed")
	}
}
