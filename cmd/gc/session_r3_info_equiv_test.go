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
// patch reads.
type healOracleCase struct {
	name     string
	status   string
	created  time.Duration // relative to clk.Now(); 0 = zero time
	meta     map[string]string
	alive    bool
	timeout  time.Duration
	rollback bool
}

// TestHealStatePatchWithRollbackInfoEquivalence proves healStatePatchWithRollbackInfo
// is byte-identical to the raw healStatePatchWithRollback across every heal
// branch (drained fast path, start-request, failed-create preserve/clear,
// stale-creating rollback, reset-continuation clears, named-session mode guard,
// deferred-rollback). The Info form feeds the reconciler's coherent snapshot; a
// mutation of any non-trivial branch diverges here.
//
// WI-6 R3: the raw sibling is deleted in Commit B; this equivalence row is then
// re-pinned against explicit expected batches captured from this run.
func TestHealStatePatchWithRollbackInfoEquivalence(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	rfc := func(d time.Duration) string { return clk.Now().Add(d).UTC().Format(time.RFC3339) }

	cases := []healOracleCase{
		{name: "asleep-alive", meta: map[string]string{"state": "asleep"}, alive: true, rollback: true},
		{name: "active-dead-drains", meta: map[string]string{"state": "active"}, alive: false, rollback: true},
		{name: "asleep-dead-noop", meta: map[string]string{"state": "asleep", "sleep_reason": "idle"}, alive: false, rollback: true},
		{
			name:     "creating-inflight-preserves",
			created:  -30 * time.Second,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-30 * time.Second)},
			alive:    false,
			rollback: true,
		},
		{
			name:     "stale-creating-clears-lease",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-2 * time.Minute)},
			alive:    false,
			rollback: true,
		},
		{
			name:     "stale-creating-rollback-deferred",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "last_woke_at": rfc(-2 * time.Minute)},
			alive:    false,
			rollback: false,
		},
		{
			name:     "never-started-inflight",
			created:  -2 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "pending_create_started_at": rfc(-2 * time.Minute)},
			alive:    false,
			timeout:  90 * time.Second,
			rollback: true,
		},
		{
			name:     "never-started-expired",
			created:  -20 * time.Minute,
			meta:     map[string]string{"state": "creating", "pending_create_claim": "true", "pending_create_started_at": rfc(-20 * time.Minute)},
			alive:    false,
			rollback: true,
		},
		{
			name:     "failed-create-active-lease-preserves",
			created:  -30 * time.Second,
			meta:     map[string]string{"state": "failed-create", "pending_create_claim": "true", "last_woke_at": rfc(-30 * time.Second)},
			alive:    false,
			timeout:  90 * time.Second,
			rollback: true,
		},
		{
			name:     "failed-create-no-claim-heals-asleep",
			meta:     map[string]string{"state": "failed-create"},
			alive:    false,
			rollback: true,
		},
		{
			name:     "failed-create-expired-lease-clears",
			created:  -20 * time.Minute,
			meta:     map[string]string{"state": "failed-create", "pending_create_claim": "true", "pending_create_started_at": rfc(-20 * time.Minute)},
			alive:    false,
			rollback: true,
		},
		{
			name:     "always-named-preserves-session-key",
			meta:     map[string]string{"state": "active", "configured_named_session": "true", "configured_named_identity": "mayor", "configured_named_mode": "always", "session_name": "mayor", "session_key": "sk", "started_config_hash": "h"},
			alive:    false,
			rollback: true,
		},
		{
			name:     "singleton-named-resets-continuation",
			meta:     map[string]string{"state": "active", "configured_named_session": "true", "configured_named_identity": "mayor", "configured_named_mode": "singleton", "session_name": "mayor", "session_key": "sk", "started_config_hash": "h"},
			alive:    false,
			rollback: true,
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
			info := sessionpkg.InfoFromPersistedBead(b)
			got := healStatePatchWithRollbackInfo(info, tc.alive, clk, tc.timeout, tc.rollback)
			want := healStatePatchWithRollback(b, tc.alive, clk, tc.timeout, tc.rollback)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("healStatePatchWithRollbackInfo = %#v, want (raw) %#v", got, want)
			}
			// Commit B bakes this into an explicit expected-batch table.
			t.Logf("BAKED %s => %#v", tc.name, got)
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
	if batch := healStateWithRollbackInfo(sessionpkg.InfoFromPersistedBead(closed), false, sessionFrontDoor(store), clk, 0, true); batch != nil {
		t.Fatalf("closed bead heal batch = %#v, want nil (terminal beads must not move)", batch)
	}

	live, err := store.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	batch := healStateWithRollbackInfo(sessionpkg.InfoFromPersistedBead(live), false, sessionFrontDoor(store), clk, 0, true)
	if batch["state"] != "asleep" {
		t.Fatalf("heal batch = %#v, want state=asleep", batch)
	}
	got, _ := store.Get(live.ID)
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("persisted state = %q, want asleep (front-door write must land)", got.Metadata["state"])
	}
}

// TestPersistSleepPolicyMetadataInfoEquivalence proves the Info form writes the
// same store metadata and folds the same snapshot as the raw form, across the
// change / no-change / fingerprint-preservation shapes.
func TestPersistSleepPolicyMetadataInfoEquivalence(t *testing.T) {
	cfg := &config.City{SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"}, Agents: []config.Agent{{Name: "worker"}}}
	sp := routedSleepProvider{Provider: runtime.NewFake(), capabilities: runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true}, sleep: runtime.SessionSleepCapabilityFull}

	shapes := map[string]map[string]string{
		"fresh":                  {"template": "worker", "session_name": "worker-a", "state": "active"},
		"idle-drain-inflight":    {"template": "worker", "session_name": "worker-b", "state": "asleep", "sleep_reason": "idle", "sleep_policy_fingerprint": "pinned-fp"},
		"intent-pending":         {"template": "worker", "session_name": "worker-c", "sleep_intent": "idle-stop-pending", "sleep_policy_fingerprint": "pinned-fp"},
		"already-persisted-noop": {"template": "worker", "session_name": "worker-d", "state": "active"},
	}

	for name, meta := range shapes {
		for _, suppressed := range []bool{false, true} {
			name, meta, suppressed := name, meta, suppressed
			t.Run(name, func(t *testing.T) {
				storeA := beads.NewMemStore()
				storeB := beads.NewMemStore()
				beadA, err := storeA.Create(beads.Bead{Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
				if err != nil {
					t.Fatal(err)
				}
				beadB, err := storeB.Create(beads.Bead{Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
				if err != nil {
					t.Fatal(err)
				}
				policy := resolveSessionSleepPolicy(beadA, cfg, sp)

				persistSleepPolicyMetadata(&beadA, sessionFrontDoor(storeA), policy, suppressed)
				gotInfo := persistSleepPolicyMetadataInfo(sessionpkg.InfoFromPersistedBead(beadB), sessionFrontDoor(storeB), policy, suppressed)

				rawStored, _ := storeA.Get(beadA.ID)
				infoStored, _ := storeB.Get(beadB.ID)
				if !reflect.DeepEqual(rawStored.Metadata, infoStored.Metadata) {
					t.Fatalf("stored metadata diverged:\n raw = %#v\ninfo = %#v", rawStored.Metadata, infoStored.Metadata)
				}
				// Write-returns-Info: the local fold must equal re-projecting the
				// persisted bead (CreatedAt is store-stamped, so compare against
				// beadB's own persisted projection — the raw form's beadA carries a
				// different Create timestamp).
				if want := sessionpkg.InfoFromPersistedBead(infoStored); !reflect.DeepEqual(gotInfo, want) {
					t.Fatalf("folded Info diverged from re-projection:\n got = %#v\nwant = %#v", gotInfo, want)
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
	policy := resolveSessionSleepPolicy(bead, cfg, sp)
	in := sessionpkg.InfoFromPersistedBead(bead)
	// A change IS pending (the seven policy keys are absent), so only the write
	// error prevents the fold.
	front := sessionFrontDoor(setMetadataBatchFailStore{Store: base})
	got := persistSleepPolicyMetadataInfo(in, front, policy, true)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("on write error, returned Info must equal input unchanged:\n got = %#v\n in = %#v", got, in)
	}
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

	info := sessionpkg.InfoFromPersistedBead(bead)
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

// TestMarkIdleSleepPendingInfoEquivalence + recover + detach equivalence.
func TestSleepWriteTwinsInfoEquivalence(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	sp := routedSleepProvider{Provider: runtime.NewFake(), capabilities: runtime.ProviderCapabilities{CanReportActivity: true, CanReportAttachment: true}, sleep: runtime.SessionSleepCapabilityFull}

	newPair := func(t *testing.T, meta map[string]string) (beads.Store, beads.Bead, beads.Store, beads.Bead) {
		t.Helper()
		sa := beads.NewMemStore()
		sb := beads.NewMemStore()
		ba, err := sa.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
		if err != nil {
			t.Fatal(err)
		}
		bb, err := sb.Create(beads.Bead{Title: "w", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloneStringMap(meta)})
		if err != nil {
			t.Fatal(err)
		}
		return sa, ba, sb, bb
	}
	assertStoresEqual := func(t *testing.T, sa beads.Store, ida string, sb beads.Store, idb string) {
		t.Helper()
		ra, _ := sa.Get(ida)
		rb, _ := sb.Get(idb)
		if !reflect.DeepEqual(ra.Metadata, rb.Metadata) {
			t.Fatalf("stored metadata diverged:\n raw = %#v\ninfo = %#v", ra.Metadata, rb.Metadata)
		}
	}

	t.Run("markIdleSleepPending", func(t *testing.T) {
		for _, meta := range []map[string]string{
			{"session_name": "worker", "state": "active"},
			{"session_name": "worker", "sleep_intent": "idle-stop-pending"}, // no-op
		} {
			sa, ba, sb, bb := newPair(t, meta)
			gotRaw := markIdleSleepPending(&ba, sessionFrontDoor(sa))
			gotInfo := markIdleSleepPendingInfo(sessionpkg.InfoFromPersistedBead(bb), sessionFrontDoor(sb))
			if !reflect.DeepEqual(gotRaw, gotInfo) {
				t.Fatalf("patch diverged: raw=%#v info=%#v", gotRaw, gotInfo)
			}
			assertStoresEqual(t, sa, ba.ID, sb, bb.ID)
		}
	})

	t.Run("recoverPendingIdleSleep", func(t *testing.T) {
		for _, meta := range []map[string]string{
			{"session_name": "worker", "state": "active", "sleep_intent": "idle-stop-pending", "sleep_policy_fingerprint": "fp"},
			{"session_name": "worker", "state": "active"}, // no intent -> false
		} {
			sa, ba, sb, bb := newPair(t, meta)
			gotRaw := recoverPendingIdleSleep(&ba, sessionFrontDoor(sa), false, clk)
			gotInfo := recoverPendingIdleSleepInfo(sessionpkg.InfoFromPersistedBead(bb), sessionFrontDoor(sb), false, clk)
			if gotRaw != gotInfo {
				t.Fatalf("bool diverged: raw=%v info=%v", gotRaw, gotInfo)
			}
			assertStoresEqual(t, sa, ba.ID, sb, bb.ID)
		}
	})

	t.Run("reconcileDetachedAt", func(t *testing.T) {
		for _, meta := range []map[string]string{
			{"template": "worker", "session_name": "worker", "state": "active", "detached_at": clk.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
			{"template": "worker", "session_name": "worker", "state": "active"},
		} {
			sa, ba, sb, bb := newPair(t, meta)
			policy := resolveSessionSleepPolicy(ba, cfg, sp)
			gotRaw := reconcileDetachedAt(&ba, sa, policy, true, sp, clk)
			gotInfo := reconcileDetachedAtInfo(sessionpkg.InfoFromPersistedBead(bb), sb, policy, true, sp, clk)
			if !reflect.DeepEqual(gotRaw, gotInfo) {
				t.Fatalf("detach batch diverged: raw=%#v info=%#v", gotRaw, gotInfo)
			}
			assertStoresEqual(t, sa, ba.ID, sb, bb.ID)
		}
	})
}
