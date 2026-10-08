package main

import (
	"context"
	"errors"
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
// calling read when set.
type freshObserver struct {
	*runtime.Fake
	l    runtime.Liveness
	err  error
	read func()
}

func (f *freshObserver) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	if f.read != nil {
		f.read()
	}
	return f.l, f.err
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
	it := c.decide()
	if c.before != nil {
		c.before()
	}
	build := effectRegistry[it.Kind]
	if build == nil {
		t.Fatalf("decideRow = %+v, want a registered heal", it)
	}
	w := *c.w
	w.Env = &reconcileEnv{SP: sp}
	if writer != nil {
		w.LegStores = map[string]beads.Store{rowLeg: writer}
	}
	return it, build(newEffectPass(&w, c.a), it)(context.Background())
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
// a Wake row, a row whose runtime is alive, occupied or unknown, and a row
// in another state are not healed by it.
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
		{"dead", livenessDead, desireSleep, nil, decideCreatingHeal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, tc.desired, append([]string{"state", "creating"}, tc.meta...)...)
			if it := c.decide(); it.Reason != tc.want {
				t.Fatalf("decideRow = (%q, %q), want reason %q", it.Kind, it.Reason, tc.want)
			}
		})
	}
}

// Kills the heal orphaning a live runtime (C5d's open race): the inventory
// reads the name gone, but a start still holds the name, or settled
// deferred with its runtime up. The fresh heal refuses while the name is
// locked, on an incomplete or unsupported read, and on a live agent, writing
// nothing; it heals over a corpse or a zombie, and writes under the lock.
func TestFreshHealNeverOrphansALiveRuntime(t *testing.T) {
	unavailable := errors.Join(runtime.ErrRuntimeUnavailable, errors.New("probe"))
	for _, tc := range []struct {
		name   string
		sp     runtime.Provider
		locked bool
		cause  string
	}{
		{"name held by a start", gone(), true, causeNameBusy},
		{"alive", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true}}, false, causeRuntimeRunning},
		{"incomplete", &freshObserver{Fake: runtime.NewFake(), err: unavailable}, false, causeLivenessIncomplete},
		{"no error-bearing read", runtime.NewFake(), false, causeLivenessUnsupported},
		{"no provider", nil, false, causeLivenessUnsupported},
		{"corpse", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Corpse: true}}, false, ""},
		{"zombie", &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, desireNone, "state", "creating")
			if tc.locked {
				unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal")
				defer unlock()
			}
			lockedAtCAS := false
			adversary := &interleavedStore{Store: c.store, id: c.k.ID, between: func() {
				if unlock := runtimeNames.tryLock(c.w.CityPath, "s-heal"); unlock != nil {
					unlock()
				} else {
					lockedAtCAS = true
				}
			}}
			_, s := c.run(t, tc.sp, adversary)
			if tc.cause != "" {
				if s.Outcome != settledRefused || s.Cause != tc.cause {
					t.Fatalf("settlement %+v, want refused with cause %q", s, tc.cause)
				}
				if m := c.meta(t); m["state"] != "creating" {
					t.Fatalf("state %q, want creating: the heal wrote over a runtime it did not prove dead", m["state"])
				}
				return
			}
			if s.Outcome != settledLanded || c.meta(t)["state"] != "asleep" {
				t.Fatalf("settlement %+v, state %q, want the heal landed", s, c.meta(t)["state"])
			}
			if !lockedAtCAS {
				t.Fatal("the heal's CAS ran without the runtime name lock")
			}
		})
	}
}

// Kills a dead unwanted named row left active forever, a heal of a pool
// row (A21 closes those) or a Wake row (S1 starts it), and a patch that
// drifts from legacy's (v5.2 A6).
func TestDeadUnwantedActiveNamedRowHealedToAsleep(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always"}
	for _, tc := range []struct {
		name    string
		desired desire
		meta    []string
		want    string
	}{
		{"named active", desireNone, append([]string{"state", "active", "session_key", "k-1"}, named...), decideDeadNamedHeal},
		{"named awake", desireSleep, append([]string{"state", "awake"}, named...), decideDeadNamedHeal},
		{"named wake", desireWake, append([]string{"state", "active"}, named...), decideNoAction},
		{"pool active", desireNone, []string{"state", "active"}, decideNoAction},
		{"named asleep", desireNone, append([]string{"state", "asleep"}, named...), decideNoAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, livenessGone, tc.desired, tc.meta...)
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
		"", "crashed", session.SleepReasonIdle, session.SleepReasonIdleTimeout, session.SleepReasonNoWakeReason,
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
// fresh cycle, which is A13's.
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
