package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cutover's mode switches (C9; CONTRACT I7, Gate R R8). A switch is a
// session-preserving controller restart under the other mode: the store and
// the runtimes carry over, the in-memory state does not. Each phase runs to
// its fixed point on the same rows and runtimes the last one left, over
// every session-differential fixture.

// switched is f for a phase after the first: its rows and runtimes are the
// last phase's, so it seeds no runtime, scripted start error or in-memory
// drain of its own.
func switched(f parityFixture) parityFixture {
	f.Runtimes, f.StartErr, f.Drains = nil, nil, nil
	return f
}

// carryRuntimes replaces dst's runtimes with copies of src's, as a
// session-preserving restart keeps them, and keeps dst's object numbers
// past src's so a later start never reuses one.
func carryRuntimes(dst, src *simProvider) {
	src.mu.Lock()
	rts, objects := make(map[string]simRuntime, len(src.rts)), src.objects
	for name, rt := range src.rts {
		rts[name] = *rt
	}
	src.mu.Unlock()
	dst.mu.Lock()
	defer dst.mu.Unlock()
	for name := range dst.rts {
		dst.drop(name)
	}
	for name, rt := range rts {
		c := rt
		dst.rts[name] = &c
		dst.touch(name)
	}
	dst.objects = max(dst.objects, objects)
}

// liveRuntimes are p's runtimes whose agent runs (not a corpse or a zombie),
// by name.
func liveRuntimes(p *simProvider) map[string]simRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]simRuntime{}
	for name, rt := range p.rts {
		if !rt.corpse && !rt.zombie {
			out[name] = *rt
		}
	}
	return out
}

// assertKnownStates fails on an open session row whose state no reconciler
// knows and that a reconciler wrote (I7: v2 writes no state outside
// knownSessionStates and the signaled projection, draining with
// stop-pending). A state the seed carried is an outside writer's, which both
// modes leave alone (A5).
func assertKnownStates(t *testing.T, phase string, rows map[string]beads.Bead, seed map[string]string) {
	t.Helper()
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		b := rows[id]
		if b.Status == "closed" || b.Type != sessionBeadType {
			continue
		}
		st := b.Metadata["state"]
		if seeded, ok := seed[id]; ok && seeded == st {
			continue
		}
		if !knownSessionStates[st] && (st != string(session.StateDraining) || b.Metadata["state_reason"] != session.DrainAckStopPendingReason) {
			t.Errorf("%s: row %s reads state %q, which no reconciler knows", phase, id, st)
		}
	}
}

// assertNoDoubleStart fails on a provider Start, in the phase that acted,
// of a name whose agent was running when the phase began: the switched
// controller must adopt it, never start a second one.
func assertNoDoubleStart(t *testing.T, phase string, liveBefore map[string]simRuntime, acted map[string]int) {
	t.Helper()
	for name := range liveBefore {
		if n := acted["Start "+name]; n > 0 {
			t.Errorf("%s: started %s %d time(s) over its running agent", phase, name, n)
		}
	}
}

// assertStartsOwned fails on a runtime started during the round trip that no
// open row owns at the end (an orphan the switch left).
func assertStartsOwned(t *testing.T, before map[string]simRuntime, after map[string]simRuntime, rows map[string]beads.Bead) {
	t.Helper()
	for name, rt := range after {
		if old, ok := before[name]; ok && old.object == rt.object {
			continue // the fixture's own runtime, carried through
		}
		b, ok := rows[rt.id]
		if !ok || b.Status == "closed" || strings.TrimSpace(b.Metadata["session_name"]) != name {
			t.Errorf("runtime %s (session %s) started during the round trip is owned by no open row", name, rt.id)
		}
	}
}

// roundTrip runs f under v2, then legacy (and, unless legacyOnly, v2 again),
// each from the last phase's rows and runtimes, checking each phase.
func roundTrip(t *testing.T, f parityFixture, legacyOnly bool) {
	t.Helper()
	cityPath := t.TempDir()
	rows := f.converged(t, cityPath)
	seed := map[string]string{}
	for _, b := range rows {
		seed[b.ID] = b.Metadata["state"]
	}

	first := newV2World(t, f, cityPath, rows, nil, "", nil)
	afterV2 := first.settle(t, f.Name+" (v2)")
	assertKnownStates(t, "v2", afterV2.rows, seed)
	liveV2 := liveRuntimes(first.s.sp)

	leg := newLegacyWorld(t, switched(f), cityPath, cloneDiffBeads(v2Beads(t, first)))
	carryRuntimes(leg.sp.simProvider, first.s.sp)
	afterLegacy := leg.settle(t, f.Name+" (legacy after v2)")
	assertKnownStates(t, "legacy after v2", afterLegacy.rows, seed)
	assertNoDoubleStart(t, "legacy after v2", liveV2, afterLegacy.actions)
	if legacyOnly {
		return
	}
	liveLegacy := liveRuntimes(leg.sp.simProvider)

	again := newV2World(t, switched(f), cityPath, leg.list(t), nil, "", nil)
	carryRuntimes(again.s.sp, leg.sp.simProvider)
	afterAgain := again.settle(t, f.Name+" (v2 after legacy)")
	assertKnownStates(t, "v2 after legacy", afterAgain.rows, seed)
	assertNoDoubleStart(t, "v2 after legacy", liveLegacy, afterAgain.actions)
	assertStartsOwned(t, liveV2, liveRuntimes(again.s.sp), afterAgain.rows)
}

// v2Beads are the city leg's beads as w's store holds them, unmasked: the
// next phase seeds from them.
func v2Beads(t *testing.T, w *v2World) []beads.Bead {
	t.Helper()
	all, err := w.s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// TestV2WrittenRowsConvergeUnderLegacy (CONTRACT I7; C9 rollback rung 1).
// Kills a v2-written row legacy cannot settle: over every session-differential
// fixture, v2 runs to its fixed point, then legacy, restarted on the same
// rows and runtimes, reaches its own fixed point, writes no unknown state,
// and starts no second agent over one v2 left running.
func TestV2WrittenRowsConvergeUnderLegacy(t *testing.T) {
	for _, f := range parityFixtures() {
		t.Run(f.Name, func(t *testing.T) { roundTrip(t, f, true) })
	}
}

// TestCutoverRoundTripV2LegacyV2 (Gate R R8, the C9 seed test). Kills a
// switch that leaves an unknown state, a double start or an orphaned
// runtime: over every session-differential fixture, v2 → legacy → v2, each
// a restart on the last phase's rows and runtimes, settles in every phase
// with no unknown state, no second agent started over a running one, and no
// runtime started during the round trip that no open row owns.
func TestCutoverRoundTripV2LegacyV2(t *testing.T) {
	for _, f := range parityFixtures() {
		t.Run(f.Name, func(t *testing.T) { roundTrip(t, f, false) })
	}
}

// TestRollbackHoldsAV2SignaledStopForAnAttachedClient (CONTRACT v5.10v; the
// #7824 review probe). Kills a rollback that stops a runtime v2 holds for an
// attached client: v2's signal writes legacy's stop-pending projection, and
// legacy, restarted on the same rows and runtimes mid-flight, read it as an
// agent's ack and stopped on a token check alone. A v2-signaled row (its
// drain_intent_* half, no ack) whose runtime a client is attached to stays
// up under legacy; an agent's ack keeps legacy's stop, and so does a
// detached v2-signaled row.
func TestRollbackHoldsAV2SignaledStopForAnAttachedClient(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	signaled := func(extra ...string) parityRow {
		return poolRow("gc-1", "worker", 1, "draining", append([]string{
			"state_reason", session.DrainAckStopPendingReason, "drain_at", at(-time.Minute),
			session.DrainIntentReasonKey, "idle", session.DrainIntentAtKey, at(-2 * time.Minute), session.DrainIntentIncarnationKey, "1",
			"instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour),
		}, extra...)...)
	}
	for _, c := range []struct {
		name     string
		row      parityRow
		attached bool
		up       bool // the runtime under legacy after the rollback
	}{
		{"v2-signaled, attached: held", signaled(), true, true},
		{"v2-signaled, detached: stopped", signaled(), false, false},
		{"an agent's ack, attached: stopped as legacy", signaled(session.DrainAckIncarnationKey, "1", session.DrainAckAtKey, at(-time.Minute)), true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := parityFixture{Name: c.name, Rows: []parityRow{c.row}, Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1", attached: c.attached}}}
			cityPath := t.TempDir()
			// v2 holds the attached signaled stop (L3); legacy starts from the
			// rows and runtimes it left, as a mid-flight rollback does.
			v := newV2World(t, f, cityPath, f.converged(t, cityPath), nil, "", nil)
			v.settle(t, c.name+" (v2)")
			_, v2Up := liveRuntimes(v.s.sp)["s-gc-1"]
			if c.attached && c.row.Metadata[session.DrainAckIncarnationKey] == "" && !v2Up {
				t.Fatalf("v2 stopped the attached seat; the fixture needs v2 to hold it")
			}
			leg := newLegacyWorld(t, switched(f), cityPath, cloneDiffBeads(v2Beads(t, v)))
			carryRuntimes(leg.sp.simProvider, v.s.sp)
			if !v2Up { // nothing left to stop: the case is v2's own stop
				return
			}
			leg.settle(t, c.name+" (legacy after v2)")
			if _, up := liveRuntimes(leg.sp.simProvider)["s-gc-1"]; up != c.up {
				t.Errorf("legacy after the rollback: runtime up %t, want %t", up, c.up)
			}
		})
	}
}

// attachProbe is a fake whose attach probe answers attached, or errs; its
// IsRunning reads stale (a tmux state cache past its TTL), and its server
// may confirm dead.
type attachProbe struct {
	*runtime.Fake
	attached bool
	err      error
	dead     bool
}

func (p attachProbe) IsAttachedWithError(string) (bool, error) { return p.attached, p.err }
func (attachProbe) IsRunning(string) bool                      { return false }
func (p attachProbe) ServerConfirmedDead() bool                { return p.dead }

// TestRollbackGuardReadsPresenceFromTheAttachProbe (mc-7i9i7; the #7824 r2
// and #7858 reviews). Kills the guard holding a gone runtime, and the guard
// letting a present, attached one go. Presence and attachment come from the
// error-aware attach probe, never IsRunning, which here reads a stale cache
// (not running) throughout: a session not found is gone; an unavailable
// probe with the server confirmed dead (no tmux server left) is gone; one
// with the server not confirmed dead holds, and so does any other probe
// error (g5, as v2's L3); an attached client holds; a detached one does
// not. An ack at the row's generation keeps legacy's stop; one of another
// incarnation acks nothing of this one's.
func TestRollbackGuardReadsPresenceFromTheAttachProbe(t *testing.T) {
	unavailable := fmt.Errorf("probing attachment: tmux server unreachable or empty: %w", runtime.ErrRuntimeUnavailable)
	notFound := fmt.Errorf("probing attachment: %w", runtime.ErrSessionNotFound)
	signaled := []string{
		"state", "draining", "state_reason", session.DrainAckStopPendingReason, "generation", "2", "session_name", "s-gc-1",
		session.DrainIntentReasonKey, "idle", session.DrainIntentIncarnationKey, "2",
	}
	for _, c := range []struct {
		name     string
		attached bool
		err      error
		dead     bool
		ack      string // the ack half's incarnation, "" for none
		held     bool
	}{
		{"session not found", false, notFound, false, "", false},
		{"unavailable, the server confirmed dead", false, unavailable, true, "", false},
		{"unavailable, the server not confirmed dead", false, unavailable, false, "", true},
		{"another probe error (g5)", false, errors.New("probe failed"), false, "", true},
		{"attached, IsRunning stale (the #7858 probe)", true, nil, false, "", true},
		{"detached", false, nil, false, "", false},
		{"attached, acked at its generation", true, nil, false, "2", false},
		{"attached, acked by another incarnation", true, nil, false, "1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			meta := map[string]string{}
			for i := 0; i+1 < len(signaled); i += 2 {
				meta[signaled[i]] = signaled[i+1]
			}
			if c.ack != "" {
				meta[session.DrainAckIncarnationKey] = c.ack
			}
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Title: "gc-1", Type: sessionBeadType, Metadata: meta})
			if err != nil {
				t.Fatal(err)
			}
			sp := attachProbe{Fake: runtime.NewFake(), attached: c.attached, err: c.err, dead: c.dead}
			info := session.Info{ID: b.ID, MetadataState: "draining", StateReason: session.DrainAckStopPendingReason, SessionNameMetadata: "s-gc-1"}
			if held := v2SignaledStopHeldForAttach(store, sp, info, "s-gc-1", io.Discard); held != c.held {
				t.Errorf("held %t, want %t", held, c.held)
			}
		})
	}
}
