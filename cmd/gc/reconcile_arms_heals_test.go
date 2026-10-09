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
	"github.com/gastownhall/gascity/internal/config"
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

// ObserveLivenessSince is its error-bearing read, fresh for any since.
func (f *freshObserver) ObserveLivenessSince(name string, pn []string, _ time.Time) (runtime.Liveness, error) {
	return f.ObserveLivenessWithError(name, pn)
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
	c.w = &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), Mislabelled: map[rowKey]bool{}, CityPath: t.Name(), SessionsLeg: rowLeg}
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
	return it, runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
}

// pass decides the row and returns the pass that admitted its registered
// heal, with sp as the env's provider, writing through writer (the store
// when nil).
func (c *healCase) pass(t *testing.T, sp runtime.Provider, writer beads.Store) (*effectPass, intent) {
	t.Helper()
	it := c.decide()
	if !effectSpecs[it.Kind].runs() {
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
// row refuses the heal on its premise, and one landing between the effect's
// read and its CAS loses the CAS, then the next attempt's premise. The row
// keeps the new incarnation, still creating.
func TestCreatingHealFencedOnToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		patch   map[string]string
		between bool
		cause   string
	}{
		{"rekey after the pass", map[string]string{"instance_token": "tok-rekeyed"}, false, causePremise},
		{"prewake after the pass", map[string]string{"instance_token": "tok-4", "generation": "4"}, false, causePremise},
		{"prewake before the CAS", map[string]string{"instance_token": "tok-4", "generation": "4"}, true, causePremise},
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

// Kills SESS-531 lost, narrowed back to Wake or named rows, or widened (v5.8
// A6 item 4): a committed row whose runtime is gone and that AL1 does not
// Drain heals asleep with legacy's patch, by the fresh heal, whatever else AL1
// wants of it; a pool row AL1 wants asleep so reads runtime-missing, which
// A21's pool-slot close frees. A Drain row, named or not (A21's orphan
// close), a census-only rig-leg row (another city's, on a shared rig store), a
// heartbeat-held row (A17's), a dead runtime A12 has not classified, a
// creating row (the creating heal's) and an asleep row are not healed by it.
func TestDeadRuntimeHealCoversEveryCommittedRowAL1DoesNotDrain(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}
	held := []string{"held_until", rowAt(time.Hour)}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		meta     []string
		sessions string // the pass's sessions leg, when not the row's
		heal     bool
	}{
		{"named active", livenessGone, desireNone, append([]string{"state", "active"}, named...), "", true},
		{"named awake", livenessGone, desireSleep, append([]string{"state", "awake"}, named...), "", true},
		{"wake", livenessGone, desireWake, []string{"state", "active"}, "", true},
		{"pool sleep", livenessGone, desireSleep, []string{"state", "active"}, "", true},
		{"pool keep", livenessGone, desireKeep, []string{"state", "awake"}, "", true},
		{"pool quarantined", livenessGone, desireSleep, []string{"state", "awake", "quarantined_until", rowAt(time.Hour)}, "", true},
		{"held for a suspend", livenessGone, desireSleep, append([]string{"state", "active", "sleep_intent", "user-hold"}, held...), "", true},
		{"drain", livenessGone, desireDrain, []string{"state", "active"}, "", false},
		{"named drain", livenessGone, desireDrain, append([]string{"state", "active"}, named...), "", false},
		{"census-only rig leg", livenessGone, desireNone, []string{"state", "active"}, "rig:other", false},
		{"heartbeat-held", livenessGone, desireSleep, append([]string{"state", "active"}, held...), "", false},
		{"heartbeat-held, padded", livenessGone, desireSleep, []string{"state", "active", "held_until", " " + rowAt(time.Hour) + " "}, "", false},
		{"dead, before A12 classifies it", livenessDead, desireWake, []string{"state", "active"}, "", false},
		{"alive", livenessAlive, desireNone, []string{"state", "active"}, "", false},
		{"creating", livenessGone, desireNone, []string{"state", "creating"}, "", false},
		{"asleep", livenessGone, desireNone, []string{"state", "asleep"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, append([]string{"session_key", "k-1", "started_config_hash", "h"}, tc.meta...)...)
			if tc.sessions != "" {
				c.w.SessionsLeg = tc.sessions
			}
			it := c.decide()
			if got := it.Reason == decideDeadRuntimeHeal; got != tc.heal {
				t.Fatalf("decideRow = (%q, %q), want the dead-runtime heal %v", it.Kind, it.Reason, tc.heal)
			}
			if !tc.heal {
				return
			}
			if want := legacyHeal(c.w.Census.Rows[c.k].Info); it.Kind != intentRowHealFresh || !maps.Equal(it.Patch, want) {
				t.Fatalf("intent (%q, %v), want a fresh heal of legacy's %v", it.Kind, it.Patch, want)
			}
			_, s := c.run(t, gone(), nil)
			m := c.meta(t)
			if s.Outcome != settledLanded || m["state"] != "asleep" {
				t.Fatalf("settlement %+v, row %v, want the row healed asleep", s, m)
			}
			if tc.name == "pool sleep" && !isPoolSessionSlotFreeableInfo(session.Info{MetadataState: m["state"], SleepReason: m["sleep_reason"]}, gatherNow) {
				t.Fatalf("row %v: A21's pool-slot close cannot free it", m)
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
		session.SleepReasonRuntimeMissing: true, session.SleepReasonKilled: true,
	}
	for _, reason := range reasons {
		if got := session.SleepReasonKeepsContinuation(string(reason)); got != keeps[reason] {
			t.Errorf("SleepReasonKeepsContinuation(%q) = %v, want %v", reason, got, keeps[reason])
		}
	}
}

// Kills a leftover claim left on a committed row (v5 P4, the C1b ruling), a
// claim cleared on an uncommitted row (A10's), and the claim clear ordered
// after the dead-runtime heal, which legacy would project start-pending.
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
			c := ownRuntimeCase(t, tc.inventory, "state", "asleep", "sleep_reason", "idle")
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

// ownRuntimeCase is a row whose runtime the inventory reads alive at
// censusNow, carrying token.
func ownRuntimeCase(t *testing.T, token string, meta ...string) *healCase {
	t.Helper()
	c := newHealCase(t, livenessAlive, desireNone, meta...)
	c.w.Now, c.w.ObsMaxAge = censusNow, observeMaxAge
	ident := runtimeIdentity{Known: true, SessionID: c.k.ID, Token: token}
	c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": {Identity: ident}}, completeBackend("tmux", "s-heal"))
	return c
}

// Kills the awake heal reviving a row an operator holds dormant (I15,
// I-STOP-3/4): a `gc session kill` that landed between a PreWake and its
// provider Start leaves the row asleep and killed with its own Current
// runtime up, and so does a kill whose fence is still live; neither, nor a
// user-hold, city-stop, suspend intent, wait hold, live hold or live
// quarantine, is healed awake (the simulator's attach-recreates on a
// quarantined row found the last). A kill that lands after the pass refuses
// the admitted heal.
func TestAwakeHealNeverRevivesAnOperatorDormantRow(t *testing.T) {
	later := censusNow.Add(time.Hour).Format(time.RFC3339)
	for name, meta := range map[string][]string{
		"killed, fence aged out": {"sleep_reason", "killed", "slept_at", censusNow.Add(-time.Hour).Format(time.RFC3339)},
		"kill fence live":        {"sleep_reason", "killed", "state_reason", session.KillPendingReason, "slept_at", censusNow.Add(-time.Minute).Format(time.RFC3339)},
		"user-hold":              {"sleep_reason", "user-hold"},
		"city-stop":              {"sleep_reason", "city-stop"},
		"suspend intent":         {"sleep_intent", "user-hold"},
		"wait hold":              {"wait_hold", "op"},
		"held":                   {"held_until", later},
		"quarantined":            {"sleep_reason", "quarantine", "quarantined_until", later},
		"held, padded":           {"held_until", " " + later + " "},
	} {
		c := ownRuntimeCase(t, "tok-3", append([]string{"state", "asleep"}, meta...)...)
		if it := c.decide(); it.Reason == decideAwakeHeal {
			t.Errorf("%s: an operator-dormant row was healed awake", name)
		}
	}

	c := ownRuntimeCase(t, "tok-3", "state", "asleep", "sleep_reason", "idle")
	c.before = func() {
		if err := c.store.SetMetadataBatch(c.k.ID, session.KillPendingPatch(censusNow)); err != nil {
			t.Error(err)
		}
	}
	sp := &freshObserver{
		Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true},
		env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"},
	}
	if _, s := c.run(t, sp, nil); s.Outcome != settledRefused || c.meta(t)["state"] != "asleep" {
		t.Fatalf("settlement %+v, state %q, want a kill after the pass to refuse the heal", s, c.meta(t)["state"])
	}
}

// Kills the stability clears (v5.8 A6 item 6; SESS-539/540) lost, early, or
// drifting from legacy's clearWakeFailures and clearChurn: a committed row
// alive 30s past its wake ends up as legacy's, and 5 minutes past it also
// clears churn; a row 29s past it, one already clear, one not committed or
// not alive, and one with no readable wake, are left alone.
func TestStabilityClearMatchesLegacy(t *testing.T) {
	failures := []string{"wake_attempts", "2", "churn_count", "3", "quarantined_until", rowAt(time.Hour)}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		meta     []string
		clear    bool
	}{
		{"31s", livenessAlive, append([]string{"state", "active", "last_woke_at", rowAt(-31 * time.Second)}, failures...), true},
		{"5m", livenessAlive, append([]string{"state", "awake", "last_woke_at", rowAt(-5 * time.Minute)}, failures...), true},
		{"churn only, 5m", livenessAlive, []string{"state", "active", "last_woke_at", rowAt(-5 * time.Minute), "churn_count", "1", "wake_attempts", "0"}, true},
		{"30s", livenessAlive, append([]string{"state", "active", "last_woke_at", rowAt(-30 * time.Second)}, failures...), true},
		{"29s", livenessAlive, append([]string{"state", "active", "last_woke_at", rowAt(-29 * time.Second)}, failures...), false},
		{"already clear", livenessAlive, []string{"state", "active", "last_woke_at", rowAt(-time.Hour), "wake_attempts", "0", "churn_count", "0"}, false},
		{"not committed", livenessAlive, append([]string{"state", "creating", "last_woke_at", rowAt(-time.Hour)}, failures...), false},
		{"dead", livenessDead, append([]string{"state", "active", "last_woke_at", rowAt(-time.Hour)}, failures...), false},
		{"no wake", livenessAlive, append([]string{"state", "active", "last_woke_at", "soon"}, failures...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, desireKeep, tc.meta...)
			it := c.decide()
			if got := it.Reason == decideStabilityClear; got != tc.clear {
				t.Fatalf("decideRow = (%q, %q, %v), want the stability clear %v", it.Kind, it.Reason, it.Patch, tc.clear)
			}
			if !tc.clear {
				return
			}
			legacyStore := beads.NewMemStoreFrom(0, []beads.Bead{sessionRow(c.k.ID, tc.meta...)}, nil)
			info, front, clk := c.w.Census.Rows[c.k].Info, sessionFrontDoor(legacyStore), &clock.Fake{Time: gatherNow}
			info = clearWakeFailures(info, front)
			if productiveLongEnoughInfo(info, clk) {
				clearChurn(info, front)
			}
			legacy, _ := legacyStore.Get(c.k.ID)
			if _, s := c.run(t, nil, nil); s.Outcome != settledLanded || it.Kind != intentRowHeal {
				t.Fatalf("intent %q, settlement %+v, want a landed row heal", it.Kind, s)
			}
			m := c.meta(t)
			for _, key := range []string{"wake_attempts", "churn_count", "quarantined_until"} {
				if m[key] != legacy.Metadata[key] {
					t.Errorf("%s = %q, legacy's %q", key, m[key], legacy.Metadata[key])
				}
			}
		})
	}
}

// Kills the detached-at marker (v5.8 A6 item 7; SESS-533..536) lost or
// widened: on a committed, alive row under an interactive, enabled, Full
// policy the attach fact stamps the pass time once and clears while
// attached; an uncertain attach, an unknown liveness or an unranked row
// writes nothing; on any other row a set marker clears.
func TestDetachedAtFollowsTheAttachFact(t *testing.T) {
	tracked := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "5m", Capability: runtime.SessionSleepCapabilityFull}
	stamp, set := rowAt(0), rowAt(-time.Hour)
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		state    string
		policy   resolvedSessionSleepPolicy
		obs      rowObservation
		marker   string
		want     *string // the written detached_at; nil for none
	}{
		{"detached", livenessAlive, desireKeep, "active", tracked, rowObservation{}, "", &stamp},
		{"still detached", livenessAlive, desireKeep, "active", tracked, rowObservation{}, set, nil},
		{"attached", livenessAlive, desireKeep, "awake", tracked, rowObservation{Attached: true}, set, new(string)},
		{"attached, unset", livenessAlive, desireKeep, "active", tracked, rowObservation{Attached: true}, "", nil},
		{"uncertain attach", livenessAlive, desireKeep, "active", tracked, rowObservation{Uncertain: true, Reason: observeReasonAttach + "stale"}, "", nil},
		{"uncertain attach, set", livenessAlive, desireKeep, "active", tracked, rowObservation{Uncertain: true}, set, nil},
		{"non-interactive", livenessAlive, desireKeep, "active", resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive, Effective: "5m", Capability: runtime.SessionSleepCapabilityFull}, rowObservation{}, set, new(string)},
		{"sleep off", livenessAlive, desireKeep, "active", resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: config.SessionSleepOff, Capability: runtime.SessionSleepCapabilityFull}, rowObservation{}, set, new(string)},
		{"timed only", livenessAlive, desireKeep, "active", resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "5m", Capability: runtime.SessionSleepCapabilityTimedOnly}, rowObservation{}, set, new(string)},
		{"not committed", livenessAlive, desireKeep, "creating", tracked, rowObservation{}, set, new(string)},
		{"not committed, uncertain attach", livenessAlive, desireKeep, "creating", tracked, rowObservation{Uncertain: true}, set, nil},
		{"gone, draining", livenessGone, desireDrain, "active", tracked, rowObservation{}, set, new(string)},
		{"dead", livenessDead, desireKeep, "active", tracked, rowObservation{}, set, new(string)},
		{"gone, unset", livenessGone, desireDrain, "active", tracked, rowObservation{}, "", nil},
		{"unknown", livenessUnknown, desireKeep, "active", tracked, rowObservation{}, set, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, "state", tc.state, "detached_at", tc.marker)
			c.w.SleepPolicies = map[string]resolvedSessionSleepPolicy{c.k.ID: tc.policy}
			c.w.Observed = map[rowKey]rowObservation{c.k: tc.obs}
			it := c.decide()
			got, wrote := it.Patch["detached_at"]
			if wrote != (tc.want != nil) || wrote && got != *tc.want {
				t.Fatalf("decideRow = (%q, %q, %v), want detached_at %v", it.Kind, it.Reason, it.Patch, tc.want)
			}
		})
	}
	c := newHealCase(t, livenessAlive, desireKeep, "state", "active")
	c.w.SleepPolicies = map[string]resolvedSessionSleepPolicy{c.k.ID: tracked}
	if _, s := c.run(t, nil, nil); s.Outcome != settledLanded || c.meta(t)["detached_at"] != stamp {
		t.Fatalf("settlement %+v, detached_at %q, want the pass time %q", s, c.meta(t)["detached_at"], stamp)
	}
}

// Kills A6's items pre-empting each other across passes (CONTRACT v5.8
// §12.3): the plain row writes the row's later items propose fold into the
// first item's CAS, a fresh heal's included, and the effect writes them all;
// a later fresh heal does not fold into a plain write, and the timer heal
// keeps its own patch.
func TestA6ItemsFoldIntoOneCAS(t *testing.T) {
	tracked := map[string]resolvedSessionSleepPolicy{}
	stable := []string{"state", "active", "last_woke_at", rowAt(-time.Minute), "wake_attempts", "2", strandedEventEmittedKey, rowAt(-time.Hour)}
	c := newHealCase(t, livenessAlive, desireWake, stable...)
	tracked[c.k.ID] = resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "5m", Capability: runtime.SessionSleepCapabilityFull}
	c.w.SleepPolicies = tracked
	c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "ga-7"}
	want := session.MetadataPatch{"wake_attempts": "0", "detached_at": rowAt(0), strandedEventEmittedKey: "", session.CurrentBeadIDKey: "ga-7"}
	it, s := c.run(t, nil, nil)
	if it.Kind != intentRowHeal || it.Reason != decideStabilityClear || !maps.Equal(it.Patch, want) || s.Outcome != settledLanded {
		t.Fatalf("intent (%q, %q, %v), settlement %+v, want the stability clear carrying %v", it.Kind, it.Reason, it.Patch, s, want)
	}
	for key, value := range want {
		if c.meta(t)[key] != value {
			t.Errorf("%s = %q after the CAS, want %q", key, c.meta(t)[key], value)
		}
	}

	gone := newHealCase(t, livenessGone, desireWake, "state", "active", "session_key", "k-1", "detached_at", rowAt(-time.Hour))
	if it := gone.decide(); it.Kind != intentRowHealFresh || it.Patch["state"] != "asleep" || it.Patch["detached_at"] != "" || len(it.Patch) != len(legacyHeal(gone.w.Census.Rows[gone.k].Info))+1 {
		t.Fatalf("decideRow = (%q, %v), want the dead-runtime heal carrying the detached_at clear", it.Kind, it.Patch)
	}
	held := newHealCase(t, livenessGone, desireWake, "state", "active", "held_until", rowAt(-time.Minute), "sleep_reason", "user-hold")
	if it := held.decide(); it.Reason != decideTimerHeal || it.Patch["state"] != "" {
		t.Fatalf("decideRow = (%q, %v), want the timer heal without the fresh heal's state", it.Reason, it.Patch)
	}
	quarantine := newHealCase(t, livenessAlive, desireKeep, "state", "active", "last_woke_at", rowAt(-time.Hour), "quarantined_until", rowAt(-time.Minute),
		"sleep_reason", "quarantine", "wake_attempts", "4", strandedEventEmittedKey, rowAt(-time.Hour))
	if it := quarantine.decide(); it.Reason != decideTimerHeal || it.Patch["sleep_reason"] != "" || it.Patch["wake_attempts"] != "0" || it.Patch[strandedEventEmittedKey] != "" || len(it.Patch) != 5 {
		t.Fatalf("decideRow = (%q, %v), want the timer heal's own patch carrying the stranded clear", it.Reason, it.Patch)
	}
}
