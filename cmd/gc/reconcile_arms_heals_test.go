package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A6's heals and markers (CONTRACT v5 §4 A6; C5d).

// freshObserver is a provider whose fresh read answers l and err, after
// calling read when set, and whose identity env is env.
type freshObserver struct {
	*runtime.Fake
	l    runtime.Liveness
	err  error
	read func()
	env  map[string]string
}

func (f *freshObserver) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	if f.read != nil {
		f.read()
	}
	return f.l, f.err
}

func (f *freshObserver) GetAllEnvironment(name string) (map[string]string, error) {
	if f.env == nil {
		return nil, fmt.Errorf("no environment for %q", name)
	}
	return f.env, nil
}

// healCase is one row on a fenced MemStore, its entry reading liveness and
// desire, its fresh read answering sp.
type healCase struct {
	store *beads.MemStore
	w     *World
	a     *allocDecision
	k     rowKey
	// before runs between the pass's decision and its effect.
	before func()
}

func newHealCase(t *testing.T, liveness rowLiveness, desired desire, meta ...string) *healCase {
	t.Helper()
	store, _ := stampedMem(t, gate.Require)
	base := []string{"template", "worker", "session_name", "s-heal", "generation", "3", "instance_token", "tok-3"}
	b, err := store.Create(sessionRow("heal", append(base, meta...)...))
	if err != nil {
		t.Fatal(err)
	}
	c := &healCase{store: store, k: rowKeyOf(b.ID)}
	c.w = &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), Mislabelled: map[rowKey]bool{}, CityPath: t.Name()}
	c.w.LegStores = map[string]beads.Store{rowLeg: store}
	c.a = &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{
		c.k: {Key: c.k, Liveness: liveness, Desired: desired},
	}}}
	return c
}

func (c *healCase) decide() intent {
	it, _ := decideRow(c.w, c.a, c.k)
	return it
}

// run decides the row and runs its registered effect with sp as the
// env's provider, writing through writer (the store when nil).
func (c *healCase) run(t *testing.T, sp runtime.Provider, writer beads.Store) (intent, settlement) {
	t.Helper()
	p, it := c.pass(t, sp, writer)
	if c.before != nil {
		c.before()
	}
	return it, effectRegistry[it.Kind](p, it)(context.Background())
}

// pass decides the row and returns the pass that admitted its registered
// heal, with sp as the env's provider, writing through writer (the store
// when nil).
func (c *healCase) pass(t *testing.T, sp runtime.Provider, writer beads.Store) (*effectPass, intent) {
	t.Helper()
	it := c.decide()
	if effectRegistry[it.Kind] == nil {
		t.Fatalf("decideRow = %+v, want a registered heal", it)
	}
	w := *c.w
	w.Env = &reconcileEnv{SP: sp}
	if writer != nil {
		w.LegStores = map[string]beads.Store{rowLeg: writer}
	}
	return newEffectPass(&w, c.a), it
}

func (c *healCase) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := c.store.Get(c.k.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

func gone() *freshObserver { return &freshObserver{Fake: runtime.NewFake()} }

// legacyHeal is legacy's heal patch for info with its runtime observed not
// alive, past the stale-creating window, with rollback available.
func legacyHeal(info session.Info) session.MetadataPatch {
	info.CreatedAt = gatherNow.Add(-time.Hour)
	return session.MetadataPatch(healStatePatchWithRollbackInfo(info, false, true, &clock.Fake{Time: gatherNow}, 0, true))
}

// Kills a creating row stuck forever, and a heal (or rollback) of a row
// that is a pending create (v5 B5, C3; scenario R53): a creating row with
// no claim, its runtime gone and not wanted, is healed to asleep with
// legacy's patch, by the fresh heal; the same row holding a claim is not.
func TestCreatingRowWithoutClaimHealedToAsleep(t *testing.T) {
	c := newHealCase(t, livenessGone, desireNone, "state", "creating", "session_key", "k-1")
	it, s := c.run(t, gone(), nil)
	if it.Kind != intentRowHealFresh || it.Reason != decideCreatingHeal {
		t.Fatalf("decideRow = (%q, %q), want the creating heal", it.Kind, it.Reason)
	}
	if want := legacyHeal(c.w.Census.Rows[c.k].Info); !maps.Equal(it.Patch, want) {
		t.Fatalf("patch %v, want legacy's %v", it.Patch, want)
	}
	if s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if m := c.meta(t); m["state"] != "asleep" || m["session_key"] != "" || m["instance_token"] != "tok-3" {
		t.Fatalf("row %v, want asleep with its continuation reset and its token kept", m)
	}

	claimed := newHealCase(t, livenessGone, desireNone, "state", "creating", "pending_create_claim", "true")
	if it := claimed.decide(); it.Kind != "" {
		t.Fatalf("pending create: decideRow = (%q, %q), want no A6 write: A10's rollback owns it", it.Kind, it.Reason)
	}
}

// Kills a heal that overwrites a newer incarnation: a rekey (the token
// alone) or a PreWake (token and generation) landing after the pass read the
// row refuses the heal on the re-decided basis, and one landing between the
// effect's read and its CAS refuses on the CAS. The row keeps the new
// incarnation, still creating.
func TestCreatingHealFencedOnToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		patch   map[string]string
		between bool
		cause   string
	}{
		{"rekey after the pass", map[string]string{"instance_token": "tok-rekeyed"}, false, causeRedecided},
		{"prewake after the pass", map[string]string{"instance_token": "tok-4", "generation": "4"}, false, causeRedecided},
		{"prewake before the CAS", map[string]string{"instance_token": "tok-4", "generation": "4"}, true, causeCAS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireNone, "state", "creating")
			write := func() {
				if err := c.store.SetMetadataBatch(c.k.ID, tc.patch); err != nil {
					t.Errorf("external write: %v", err)
				}
			}
			var writer beads.Store
			if tc.between {
				writer = &interleavedStore{Store: c.store, id: c.k.ID, between: write}
			} else {
				c.before = write
			}
			_, s := c.run(t, gone(), writer)
			if s.Outcome != settledRefused || s.Cause != tc.cause {
				t.Fatalf("settlement %+v, want refused with cause %q", s, tc.cause)
			}
			if m := c.meta(t); m["state"] != "creating" || m["instance_token"] != tc.patch["instance_token"] {
				t.Fatalf("row %v, want the new incarnation creating, unhealed", m)
			}
		})
	}
}

// Kills each of the creating heal's conditions dropped: a pending create,
// a Wake row, a row whose runtime is alive, occupied, unknown or dead (A12
// classifies a dead one first), and a row in another state are not healed
// by it.
func TestCreatingHealSkipsPendingCreateAndWakeRows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		meta     []string
		want     string
	}{
		{"pending create", livenessGone, desireNone, []string{"pending_create_claim", "true"}, decideNoAction},
		{"wake", livenessGone, desireWake, nil, decideNoAction},
		{"alive", livenessAlive, desireNone, nil, decideNoAction},
		{"occupied", livenessOccupied, desireNone, nil, decideNoAction},
		{"unknown", livenessUnknown, desireNone, nil, decideLivenessUnknown},
		{"start-pending", livenessGone, desireNone, []string{"state", "start-pending"}, decideNoAction},
		{"dead, before A12 classifies it", livenessDead, desireSleep, nil, decideNoAction},
		{"gone, asleep desired", livenessGone, desireSleep, nil, decideCreatingHeal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, append([]string{"state", "creating"}, tc.meta...)...)
			if it := c.decide(); it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want reason %q", it.Kind, it.Reason, tc.want)
			}
		})
	}
}

// Kills a dead unwanted named row left active forever, a heal of a pool
// row (A21 closes those) or of a runtime A12 has not classified, and a patch
// that drifts from legacy's (v5.2 A6). A Wake named row is the crash heal's.
func TestDeadUnwantedActiveNamedRowHealedToAsleep(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		meta     []string
		want     string
	}{
		{"named active", livenessGone, desireNone, append([]string{"state", "active", "session_key", "k-1"}, named...), decideDeadNamedHeal},
		{"named awake", livenessGone, desireSleep, append([]string{"state", "awake"}, named...), decideDeadNamedHeal},
		{"named wake", livenessGone, desireWake, append([]string{"state", "active"}, named...), decideCrashHeal},
		{"named dead", livenessDead, desireNone, append([]string{"state", "active"}, named...), decideNoAction},
		{"pool active", livenessGone, desireNone, []string{"state", "active"}, decideNoAction},
		{"named asleep", livenessGone, desireNone, append([]string{"state", "asleep"}, named...), decideNoAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, tc.meta...)
			it := c.decide()
			if it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want reason %q", it.Kind, it.Reason, tc.want)
			}
			if tc.want != decideDeadNamedHeal {
				return
			}
			if want := legacyHeal(c.w.Census.Rows[c.k].Info); !maps.Equal(it.Patch, want) {
				t.Fatalf("patch %v, want legacy's %v", it.Patch, want)
			}
			if _, s := c.run(t, gone(), nil); s.Outcome != settledLanded || c.meta(t)["state"] != "asleep" {
				t.Fatalf("settlement %+v, want the row healed asleep", s)
			}
		})
	}
}

// Kills asleepHealPatch drifting from legacy's healStatePatchWithRollbackInfo
// for every row it heals: a claimless creating row past the stale window and
// a committed row, each sleep reason, with and without a continuation, named
// mode always or not.
func TestAsleepHealPatchMatchesLegacy(t *testing.T) {
	reasons := []session.SleepReason{
		"", "crashed", session.SleepReasonKilled, session.SleepReasonIdle, session.SleepReasonIdleTimeout, session.SleepReasonNoWakeReason,
		session.SleepReasonConfigDrift, session.SleepReasonDrained, session.SleepReasonCityStop, session.SleepReasonUserHold,
		session.SleepReasonWaitHold, session.SleepReasonRateLimit, session.SleepReasonFailedCreate,
		session.SleepReasonProviderTerminalError, session.SleepReasonRuntimeMissing, session.SleepReasonQuarantine,
		session.SleepReasonContextChurn, session.SleepReasonMaxSessionAge, session.SleepReasonAssignedWorkExhausted,
	}
	for _, state := range []string{"creating", "active", "awake"} {
		for _, reason := range reasons {
			for _, cont := range []session.Info{{}, {SessionKey: "k"}, {StartedConfigHash: " h "}} {
				for _, mode := range []string{"", "on_demand", "always"} {
					info := cont
					info.ID, info.MetadataState, info.SleepReason = "gc-1", state, string(reason)
					info.CreatedAt = gatherNow.Add(-time.Hour)
					info.ConfiguredNamedSession, info.ConfiguredNamedMode = mode != "", mode
					if got, want := asleepHealPatch(info), legacyHeal(info); !maps.Equal(got, want) {
						t.Errorf("%s/%q/%+v/%q: patch %v, legacy %v", state, reason, cont, mode, got, want)
					}
				}
			}
		}
	}
	// Both sides read session.SleepReasonKeepsContinuation, so pin its list
	// against a frozen copy of legacy's shouldResetContinuation list.
	keeps := map[session.SleepReason]bool{
		session.SleepReasonIdle: true, session.SleepReasonIdleTimeout: true, session.SleepReasonNoWakeReason: true,
		session.SleepReasonConfigDrift: true, session.SleepReasonDrained: true, session.SleepReasonCityStop: true,
		session.SleepReasonUserHold: true, session.SleepReasonWaitHold: true, session.SleepReasonRateLimit: true,
		session.SleepReasonRuntimeMissing: true,
	}
	for _, reason := range reasons {
		if got := session.SleepReasonKeepsContinuation(string(reason)); got != keeps[reason] {
			t.Errorf("SleepReasonKeepsContinuation(%q) = %v, want %v", reason, got, keeps[reason])
		}
	}
}

// Kills a leftover claim left on a committed row (v5 P4, the C1b ruling), a
// claim cleared on an uncommitted row (A10's), and the claim clear ordered
// after the dead named heal, which legacy would project start-pending.
func TestLeftoverClaimClearedOnCommittedRow(t *testing.T) {
	want := session.MetadataPatch{"pending_create_claim": "", "pending_create_started_at": ""}
	for _, tc := range []struct {
		name string
		meta []string
		want string
	}{
		{"active", []string{"state", "active"}, decideClaimClear},
		{"awake", []string{"state", "awake"}, decideClaimClear},
		{"dead named", []string{"state", "active", "configured_named_session", "true"}, decideClaimClear},
		{"creating", []string{"state", "creating"}, decideNoAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]string{"pending_create_claim", "true", "pending_create_started_at", rowAt(-time.Hour)}, tc.meta...)
			c := newHealCase(t, livenessGone, desireNone, meta...)
			it := c.decide()
			if it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want reason %q", it.Kind, it.Reason, tc.want)
			}
			if tc.want != decideClaimClear {
				return
			}
			if it.Kind != intentRowHeal || !maps.Equal(it.Patch, want) {
				t.Fatalf("intent (%q, %v), want a row heal of %v", it.Kind, it.Patch, want)
			}
			if _, s := c.run(t, nil, nil); s.Outcome != settledLanded || c.meta(t)["pending_create_claim"] != "" {
				t.Fatalf("settlement %+v, want the claim cleared", s)
			}
		})
	}
}

// Kills SESS-603 lost or widened: an alive row clears the stranded marker
// with legacy's patch (clearStrandedEventMarker); a row not alive keeps it.
func TestStrandedMarkerClearedOnAliveRow(t *testing.T) {
	marker := []string{"state", "active", strandedEventEmittedKey, rowAt(-time.Hour)}
	c := newHealCase(t, livenessAlive, desireKeep, marker...)
	it := c.decide()
	legacy := clearStrandedEventMarker(beads.NewMemStoreFrom(0, []beads.Bead{sessionRow(c.k.ID, marker...)}, nil), c.w.Census.Rows[c.k].Info, nil, io.Discard)
	if it.Kind != intentRowHeal || it.Reason != decideStrandedClear || !maps.Equal(it.Patch, legacy) {
		t.Fatalf("decideRow = (%q, %q, %v), want the stranded clear of legacy's %v", it.Kind, it.Reason, it.Patch, legacy)
	}
	if _, s := c.run(t, nil, nil); s.Outcome != settledLanded || c.meta(t)[strandedEventEmittedKey] != "" {
		t.Fatalf("settlement %+v, want the marker cleared", s)
	}
	for _, l := range []rowLiveness{livenessGone, livenessDead, livenessOccupied} {
		if it := newHealCase(t, l, desireKeep, marker...).decide(); it.Reason == decideStrandedClear {
			t.Fatalf("%s row: the stranded marker cleared off a runtime not alive", l)
		}
	}
}

// Kills SESS-613 lost or widened: an alive Wake row records its assigned
// work with legacy's patch (recordCurrentBeadIDOnWake); a row already on it,
// not Wake or not alive does not, and neither does a fresh-mode row due a
// fresh cycle that has not claimed the work itself, which is A13's; one that
// has claimed it is stamped, as legacy's self-claimed branch does.
func TestCurrentBeadStampedOnAliveWakeRow(t *testing.T) {
	work := &assignedWorkView{BeadID: "ga-7"}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		work     *assignedWorkView
		meta     []string
		stamp    bool
	}{
		{"alive wake", livenessAlive, desireWake, work, nil, true},
		{"reassigned", livenessAlive, desireWake, work, []string{session.CurrentBeadIDKey, "ga-6"}, true},
		{"fresh cycle in resume mode", livenessAlive, desireWake, &assignedWorkView{BeadID: "ga-7", RequiresFreshCycle: true}, []string{"wake_mode", "resume"}, true},
		{"already stamped", livenessAlive, desireWake, work, []string{session.CurrentBeadIDKey, "ga-7"}, false},
		{"no work", livenessAlive, desireWake, &assignedWorkView{BeadID: " "}, nil, false},
		{"keep", livenessAlive, desireKeep, work, nil, false},
		{"dead", livenessDead, desireWake, work, nil, false},
		{"fresh cycle", livenessAlive, desireWake, &assignedWorkView{BeadID: "ga-7", RequiresFreshCycle: true}, []string{"wake_mode", "fresh"}, false},
		{"fresh cycle, claimed by another bead", livenessAlive, desireWake, &assignedWorkView{BeadID: "ga-7", RequiresFreshCycle: true}, []string{"wake_mode", "fresh", "current_claim_bead_id", "ga-6"}, false},
		{"fresh cycle, self-claimed", livenessAlive, desireWake, &assignedWorkView{BeadID: "ga-7", RequiresFreshCycle: true}, []string{"wake_mode", "fresh", "current_claim_bead_id", "ga-7"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]string{"state", "active"}, tc.meta...)
			c := newHealCase(t, tc.liveness, tc.desired, meta...)
			c.a.Snapshot.Entries[c.k].AssignedWork = tc.work
			it := c.decide()
			if got := it.Reason == decideCurrentBead; got != tc.stamp {
				t.Fatalf("decideRow = (%q, %q), want stamp %v", it.Kind, it.Reason, tc.stamp)
			}
			if !tc.stamp {
				return
			}
			legacy := recordCurrentBeadIDOnWake(c.w.Census.Rows[c.k].Info, sessionFrontDoor(beads.NewMemStoreFrom(0, []beads.Bead{sessionRow(c.k.ID, meta...)}, nil)), tc.work.BeadID, io.Discard)
			if it.Kind != intentRowHeal || !maps.Equal(it.Patch, legacy) {
				t.Fatalf("intent (%q, %v), want a row heal of legacy's %v", it.Kind, it.Patch, legacy)
			}
			if _, s := c.run(t, nil, nil); s.Outcome != settledLanded || c.meta(t)[session.CurrentBeadIDKey] != "ga-7" {
				t.Fatalf("settlement %+v, want the bead recorded", s)
			}
		})
	}
}

// Kills SESS-531 lost or widened: a committed Wake row whose runtime is gone
// heals asleep with legacy's desired-path patch, its continuation reset so
// the relaunch does not resume the crashed conversation, by the fresh heal.
// A creating Wake row (S1's), a committed pool row not Wake (A21's) and a
// dead Wake row (A12's, then S5's recycle) do not.
func TestCrashHealResetsAWakeRowsContinuation(t *testing.T) {
	c := newHealCase(t, livenessGone, desireWake, "state", "active", "session_key", "k-1", "started_config_hash", "h")
	it, s := c.run(t, gone(), nil)
	if it.Kind != intentRowHealFresh || it.Reason != decideCrashHeal {
		t.Fatalf("decideRow = (%q, %q), want the crash heal", it.Kind, it.Reason)
	}
	if want := legacyHeal(c.w.Census.Rows[c.k].Info); !maps.Equal(it.Patch, want) {
		t.Fatalf("patch %v, want legacy's %v", it.Patch, want)
	}
	if m := c.meta(t); s.Outcome != settledLanded || m["state"] != "asleep" || m["session_key"] != "" || m["continuation_reset_pending"] != "true" {
		t.Fatalf("settlement %+v, row %v, want asleep with the continuation reset", s, m)
	}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		state    string
	}{
		{"creating wake", livenessGone, desireWake, "creating"},
		{"active keep", livenessGone, desireKeep, "active"},
		{"dead wake", livenessDead, desireWake, "active"},
	} {
		if it := newHealCase(t, tc.liveness, tc.desired, "state", tc.state, "session_key", "k-1").decide(); it.Reason == decideCrashHeal {
			t.Errorf("%s: the crash heal fired", tc.name)
		}
	}
}

// Kills an orphan left forever (an asleep row whose own runtime came up
// outside the controller): an asleep row whose runtime the inventory reads
// alive with the row's token heals awake with legacy's patch, once the
// effect reads it alive and Current again fresh. Another token, in the
// inventory or in the fresh read, or a runtime no longer alive, does not.
func TestAwakeHealRestoresAnAsleepRowsOwnRuntime(t *testing.T) {
	for _, tc := range []struct {
		name            string
		inventory, read string // the runtime's token in the inventory and in the fresh read
		alive           bool
		want            string // the decided reason
		cause           string // the effect's refusal, "" for landed
	}{
		{"own runtime", "tok-3", "tok-3", true, decideAwakeHeal, ""},
		{"another token in the inventory", "tok-9", "tok-9", true, decideNoAction, ""},
		{"rekeyed since the pass", "tok-3", "tok-9", true, decideAwakeHeal, causeRuntimeNotOwn},
		{"died since the pass", "tok-3", "tok-3", false, decideAwakeHeal, causeRuntimeNotOwn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessAlive, desireNone, "state", "asleep", "sleep_reason", "idle")
			c.w.Now, c.w.ObsMaxAge = censusNow, observeMaxAge
			ident := runtimeIdentity{Known: true, SessionID: c.k.ID, Token: tc.inventory}
			c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": {Identity: ident}}, completeBackend("tmux", "s-heal"))
			it := c.decide()
			if it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want reason %q", it.Kind, it.Reason, tc.want)
			}
			if tc.want != decideAwakeHeal {
				return
			}
			legacy := healStatePatchWithRollbackInfo(c.w.Census.Rows[c.k].Info, true, true, &clock.Fake{Time: gatherNow}, 0, true)
			if !maps.Equal(it.Patch, session.MetadataPatch(legacy)) {
				t.Fatalf("patch %v, want legacy's %v", it.Patch, legacy)
			}
			sp := &freshObserver{
				Fake: runtime.NewFake(), l: runtime.Liveness{Running: tc.alive, Alive: tc.alive},
				env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": tc.read},
			}
			_, s := c.run(t, sp, nil)
			if tc.cause != "" {
				if s.Outcome != settledRefused || s.Cause != tc.cause || c.meta(t)["state"] != "asleep" {
					t.Fatalf("settlement %+v, state %q, want refused %q and the row asleep", s, c.meta(t)["state"], tc.cause)
				}
				return
			}
			if s.Outcome != settledLanded || c.meta(t)["state"] != "awake" {
				t.Fatalf("settlement %+v, state %q, want the row healed awake", s, c.meta(t)["state"])
			}
		})
	}
}
