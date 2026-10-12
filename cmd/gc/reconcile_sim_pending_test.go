package main

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The simulator's pending creates (mc-cjtkj): manual, named and pool rows
// holding pending_create_claim, inside and past their lease, never started
// and started, with no runtime, their own, a StaleSelf one (same epoch and
// older), another row's or an unstamped one under their name; failed creates
// still holding the claim (SESS-058); and the manual create's two writes
// (the row, then its session_name: staging J45) as outside moves. So the
// corpus reaches A10's rollback and its S7 exit, the start's lost commit,
// AL1's lease keep and A21's leased failed-create hold, and the invariants
// below, each with a seeded mutant, hold them:
//   - PC1 I-lease: v2 never rolls back or closes a row inside its pending
//     create's lease (legacy's pendingCreateSessionStillLeasedInfo, its
//     anchor not past the skew bound) unless its name holds another row's
//     runtime or the row's own older incarnation (Foreign, StaleSelf);
//   - PC2 I-S7: at quiescence no pending create rests with a StaleSelf
//     runtime under its name that S7's exit could take;
//   - PC3 I-lost-commit: a start whose row moved under it (closed, or
//     retokened) leaves no live runtime it created under the moved token.

func init() {
	simEffectChecks = append(simEffectChecks, (*sim).markEffect, (*sim).checkLeaseRollback, (*sim).checkLostCommit)
	simQuietChecks = append(simQuietChecks, (*sim).checkStaleCreates)
	simStepChecks = append(simStepChecks, (*sim).noteStopAdmissions)
	simMutants = append(simMutants, []struct {
		name, inv string
		opts      simOpts
	}{
		{"A10 rolls back a pending create the lease keep holds", "PC1 ", simOpts{arms: mutateArm("A10", reflect.ValueOf(armRollback).Pointer(), func(r *rowFacts) (intent, bool) {
			if e := r.entry; e != nil && r.row.Info.PendingCreateClaim && e.Desired == desireKeep && e.Reason == reasonPendingCreate && e.Liveness.startCandidate() {
				r.rests |= restRuntime
				return intent{Kind: intentRollback, Reason: decideRollback, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}}, true
			}
			return armRollback(r)
		})}},
		{"A21 closes a failed create whose claim is still leased", "PC1 ", simOpts{arms: mutateArm("A21", reflect.ValueOf(armClose).Pointer(), func(r *rowFacts) (intent, bool) {
			it, ok := armClose(r)
			if it.Reason != decideFailedCreateLeased {
				return it, ok
			}
			info, now := r.row.Info, r.w.Now
			it = intent{Kind: intentClose, Reason: decideCloseFailedCreate, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Patch: failedCreateCloseLife(now)}
			it.Class = session.CellInfo(info, session.ActorController, session.DispEvent{Kind: session.EvRetire}, now)
			it.Closing = closeSpec{Kind: closeFailedCreate, Row: info}
			return it, true
		})}},
		{"A10 skips S7's exit", "PC2 ", simOpts{arms: mutateArm("A10", reflect.ValueOf(armRollback).Pointer(), func(r *rowFacts) (intent, bool) {
			it, ok := armRollback(r)
			if it.Reason == decideStaleCreate {
				return intent{}, false
			}
			return it, ok
		})}},
		{"a lost commit leaves its runtime", "PC3 ", simOpts{specs: func(specs map[string]effectSpec) {
			spec := specs[intentStart]
			spec.sections = slices.Clone(spec.sections)
			for i := range spec.sections {
				spec.sections[i].Moved = nil
			}
			specs[intentStart] = spec
		}}},
	}...)
}

// mutateArm replaces the arm named name whose decide is fn's pointer with
// decide.
func mutateArm(name string, fn uintptr, decide func(r *rowFacts) (intent, bool)) func([]rowArm) []rowArm {
	return func(arms []rowArm) []rowArm {
		for i := range arms {
			if arms[i].name == name && reflect.ValueOf(arms[i].decide).Pointer() == fn {
				arms[i].decide = decide
			}
		}
		return arms
	}
}

// pendingCreateRows are the seed's pending creates on the city leg: pool,
// manual (some read between their two writes, with no name yet) and, when
// the seed configures the named session "boss", a named one; and failed
// creates still holding the claim.
func (s *sim) pendingCreateRows() []beads.Bead {
	var rows []beads.Bead
	if s.rng.IntN(3) == 0 {
		s.cfg.NamedSessions = append(s.cfg.NamedSessions, config.NamedSession{Name: "boss", Template: "worker", Mode: []string{"on_demand", "always"}[s.rng.IntN(2)]})
		rows = append(rows, s.pendingCreate("gc-n1", "named", 0, true))
	}
	for i := 1; i <= s.rng.IntN(3); i++ {
		rows = append(rows, s.pendingCreate(fmt.Sprintf("gc-p%d", i), "pool", 20+i, true))
	}
	for i := 1; i <= s.rng.IntN(3); i++ {
		rows = append(rows, s.pendingCreate(fmt.Sprintf("gc-m%d", i), "manual", 0, s.rng.IntN(2) == 0))
	}
	if s.rng.IntN(2) == 0 {
		b := s.pendingCreate("gc-f1", []string{"pool", "manual"}[s.rng.IntN(2)], 30, true)
		b.Metadata["state"] = "failed-create"
		rows = append(rows, b)
	}
	return rows
}

// pendingCreate is pending create id of kind (pool at slot, manual or named),
// created up to 15 minutes ago, started (last_woke_at) or not, named or
// still nameless, and puts a runtime under its name: none, its own, a
// StaleSelf one of the same or an older epoch, another row's, or an
// unstamped one.
func (s *sim) pendingCreate(id, kind string, slot int, named bool) beads.Bead {
	gen := 1 + s.rng.IntN(3)
	token := fmt.Sprintf("tok-%s-%d", id, gen)
	meta := []string{
		"template", "worker", "agent_name", "worker", "state", []string{"creating", "start-pending"}[s.rng.IntN(2)],
		"generation", strconv.Itoa(gen), "instance_token", token, "pending_create_claim", "true",
		"pending_create_started_at", s.rel(-time.Duration(s.rng.IntN(900)) * time.Second),
	}
	if s.rng.IntN(2) == 0 {
		meta = append(meta, "last_woke_at", s.rel(-time.Duration(s.rng.IntN(180))*time.Second))
	}
	name := "s-" + id
	switch kind {
	case "pool":
		meta = append(meta, "pool_managed", "true", "pool_slot", strconv.Itoa(slot), "session_origin", "ephemeral")
	case "manual":
		meta = append(meta, "session_origin", "manual")
	case "named":
		meta = append(meta, "session_origin", "named", "configured_named_session", "true", "configured_named_identity", "boss", "alias", "boss")
	}
	if named {
		meta = append(meta, "session_name", name)
		s.pendingRuntime(name, id, gen, token)
	}
	return sessionRow(id, meta...)
}

// pendingRuntime puts one of the runtimes a pending create can find under
// its name.
func (s *sim) pendingRuntime(name, id string, gen int, token string) {
	switch s.rng.IntN(7) {
	case 0: // its own, Current
		s.sp.put(name, id, strconv.Itoa(gen), token)
	case 1: // its own, StaleSelf at its epoch (S7: the own-runtime exit)
		s.sp.put(name, id, strconv.Itoa(gen), token+"-old")
	case 2: // its own older incarnation (S7: Foreign by attribution)
		if gen > 1 {
			s.sp.put(name, id, strconv.Itoa(gen-1), fmt.Sprintf("tok-%s-%d", id, gen-1))
		}
	case 3: // another row's
		s.sp.put(name, "gc-other", "1", "tok-other")
	case 4: // unstamped
		s.sp.put(name, "", "", "")
	}
}

// pendingRuntimeOps are the pending creates' runtime moves: a pending
// create's own earlier incarnation, at its generation (S7).
var pendingRuntimeOps = []struct {
	name string
	fn   func(p *simProvider, name string, b beads.Bead)
}{
	{"stale-incarnation", func(p *simProvider, name string, b beads.Bead) {
		if b.Metadata["pending_create_claim"] == "true" && b.Status != "closed" {
			p.put(name, b.ID, b.Metadata["generation"], b.Metadata["instance_token"]+"-stale")
		}
	}},
}

// pickExternalOp is a step's outside move: externalOps, plus the pending
// creates' when the sim seeds them. A sim that seeds none (the floored
// corpus) draws from externalOps alone, so its RNG stream, and the
// scenarios it reaches, are main's.
func (s *sim) pickExternalOp() struct {
	name string
	fn   func(s *sim, l *simLeg, b beads.Bead)
} {
	ops := externalOps
	if !s.noPendingCreates {
		ops = slices.Concat(externalOps, pendingCreateOps)
	}
	return ops[s.rng.IntN(len(ops))]
}

// pickRuntimeOp is pickExternalOp for the runtime moves.
func (s *sim) pickRuntimeOp() struct {
	name string
	fn   func(p *simProvider, name string, b beads.Bead)
} {
	ops := runtimeOps
	if !s.noPendingCreates {
		ops = slices.Concat(runtimeOps, pendingRuntimeOps)
	}
	return ops[s.rng.IntN(len(ops))]
}

// pendingCreateOps are the manual create's outside moves: Manager.CreateSession's
// first write (the row with its claim, no name yet) and its second (the
// name), which may land passes apart (staging J45).
var pendingCreateOps = []struct {
	name string
	fn   func(s *sim, l *simLeg, b beads.Bead)
}{
	{"manual-create", func(s *sim, l *simLeg, _ beads.Bead) {
		if l.name != simCityLeg {
			return // the legs' stores mint IDs on one prefix: a rig-leg create could reuse a city row's ID and name
		}
		id := fmt.Sprintf("gc-mc%d", s.rng.IntN(1_000_000))
		b := sessionRow(id, "template", "worker", "agent_name", "worker", "state", "start-pending", "session_origin", "manual",
			"generation", "1", "instance_token", "tok-"+id+"-1", "pending_create_claim", "true", "pending_create_started_at", s.rel(0))
		if _, err := l.backing.Create(b); err != nil {
			s.t.Fatalf("seed %d: manual create: %v", s.seed, err)
		}
	}},
	{"name-lands", func(s *sim, l *simLeg, _ beads.Bead) {
		rows, _ := l.backing.List(beads.ListQuery{AllowScan: true})
		rows = slices.DeleteFunc(rows, func(b beads.Bead) bool {
			return b.Metadata["session_name"] != "" || b.Metadata["pending_create_claim"] != "true"
		})
		if len(rows) > 0 {
			b := rows[s.rng.IntN(len(rows))]
			s.setMeta(l, b.ID, "session_name", "s-"+b.ID)
		}
	}},
}

// pendingLeased is the lease I-lease holds v2 to on b at now: legacy's
// pendingCreateSessionStillLeasedInfo on a row holding the claim, with no
// lease anchor past the skew bound.
func (s *sim) pendingLeased(b beads.Bead, now time.Time) bool {
	info := sessionInfoFromBead(b)
	return info.PendingCreateClaim && !pendingCreateAnchorAhead(info, now) &&
		pendingCreateSessionStillLeasedInfo(info, s.cfg, &clock.Fake{Time: now})
}

// otherOccupant reports whether b's name holds a live runtime that is not
// b's current incarnation's: another row's (Foreign), or b's own under
// another token at an epoch at or below its generation (StaleSelf), the
// runtimes A10 may roll back over inside the lease.
func (s *sim) otherOccupant(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	if rt == nil || rt.corpse {
		return false
	}
	switch {
	case rt.id != "" && rt.id != b.ID:
		return true
	case rt.id == b.ID && rt.token != b.Metadata["instance_token"]:
		epoch, err1 := strconv.Atoi(rt.epoch)
		gen, err2 := strconv.Atoi(b.Metadata["generation"])
		return err1 == nil && err2 == nil && epoch <= gen
	}
	return false
}

// checkLeaseRollback is PC1 I-lease: a rollback (A10) or a close (A21)
// that lands closed on a row whose pending create was leased at its release
// needs another row's runtime or the row's StaleSelf one under its name. A
// start's own abandon (S3) is the start's to roll back, and is not judged.
func (s *sim) checkLeaseRollback(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if (e.it.Kind != intentRollback && e.it.Kind != intentClose) || l == nil {
		return func(settlement) {}
	}
	before, err := l.backing.Get(e.it.Key.ID)
	if err != nil || before.Status == "closed" || !s.pendingLeased(before, s.clk.Now()) || s.otherOccupant(before) {
		return func(settlement) {}
	}
	return func(res settlement) {
		if res.Closed { // the effect's own close landed (an outside close is not v2's)
			s.failf("PC1 I-lease", "%s %s (%s) closed %v inside its pending create's lease, with no other runtime under its name", e.it.Kind, e.it.Key.ID, e.it.Reason, before.Metadata)
		}
	}
}

// checkLostCommit is PC3 I-lost-commit: when a start settles with its row
// closed or retokened under it, no runtime it created under the token it
// set stays alive under the row's name. An effect the executor abandoned
// (ambiguous or late) is not judged: its Start may still be running.
func (s *sim) checkLostCommit(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if e.it.Kind != intentStart || l == nil {
		return func(settlement) {}
	}
	s.sp.mu.Lock()
	from := len(s.sp.starts)
	s.sp.mu.Unlock()
	return func(res settlement) {
		if res.Late || res.Outcome == settledAmbiguous {
			return
		}
		after, err := l.backing.Get(e.it.Key.ID)
		if err != nil {
			return
		}
		s.sp.mu.Lock()
		defer s.sp.mu.Unlock()
		for _, st := range s.sp.starts[min(from, len(s.sp.starts)):] {
			if st.External || !st.Created || st.ID != e.it.Key.ID {
				continue
			}
			if after.Status != "closed" && after.Metadata["instance_token"] == st.Token {
				continue // its commit stands
			}
			if rt := s.sp.rts[st.Name]; rt != nil && !rt.corpse && rt.token == st.Token {
				s.failf("PC3 I-lost-commit", "start %s settled (%d %s) with its row %s %s under token %q, and left the runtime it created under %q alive", e.it.Key.ID, res.Outcome, res.Cause, after.Status, after.Metadata["instance_token"], st.Token, st.Token)
			}
		}
	}
}

// checkStaleCreates is PC2 I-S7 at quiescence: no open pending create on
// the sessions leg rests with a StaleSelf runtime under its name (its ID,
// another token, an epoch at or below its generation) that S7's exit can
// take: one the census reads alive, not attached, whose probe answers, on
// a row no kill fence or unknown state holds.
func (s *sim) checkStaleCreates() {
	_, obs := s.observed()
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		b := s.prev[k]
		leg, _, _ := strings.Cut(k, "/")
		if leg != simCityLeg || b.Status == "closed" || b.Metadata["pending_create_claim"] != "true" || b.Metadata["state"] == "archived-v9" {
			continue
		}
		if session.IsKillPendingInfo(sessionInfoFromBead(b), s.clk.Now()) {
			continue
		}
		name := b.Metadata["session_name"]
		if name == "" || s.sp.IsAttached(name) || s.probeErring(b) || obs[rowKey{Leg: rowLeg, ID: b.ID}].Liveness != livenessAlive {
			continue // what the census cannot read alive (a listing that stays partial or failed), S7 cannot see
		}
		s.sp.mu.Lock()
		rt := s.sp.rts[name]
		stale := rt != nil && !rt.corpse && !rt.zombie && rt.id == b.ID && rt.token != b.Metadata["instance_token"]
		if stale {
			epoch, err1 := strconv.Atoi(rt.epoch)
			gen, err2 := strconv.Atoi(b.Metadata["generation"])
			stale = err1 == nil && err2 == nil && epoch <= gen
		}
		s.sp.mu.Unlock()
		if stale {
			s.failf("PC2 I-S7", "%s rests at quiescence holding its claim with a StaleSelf runtime under %s that S7's exit would take", k, name)
		}
	}
}

// pendingCreateRollback reports a v2 close of a row holding its pending
// create's claim as failed-create: a rollback (A10, S3) or A21's failed
// create close, which end the row and its holds with the Retire cell, as
// legacy's rollbackPendingCreate does whatever holds the row
// (failedCreateClosePatch). I15 leaves the holds to it; PC1 judges when it
// may land.
func pendingCreateRollback(w simWrite) bool {
	return w.Before.Metadata["pending_create_claim"] == "true" && w.After.Metadata["state"] == string(session.StateFailedCreate)
}

// pendingCreateTimers are the lease edges of each open row holding its
// pending create's claim, which change what A10, A21 and AL1's keep decide
// when a patrol passes them: the never-started deadline (S6), the attempt
// window and a started create's in-flight window.
func (s *sim) pendingCreateTimers() []time.Time {
	var out []time.Time
	for _, b := range s.snapshot() {
		if b.Status == "closed" || b.Metadata["pending_create_claim"] != "true" {
			continue
		}
		if at, err := time.Parse(time.RFC3339, b.Metadata["pending_create_started_at"]); err == nil {
			out = append(out, at.Add(pendingCreateNeverStartedTimeout), at.Add(staleCreatingStateTimeout))
		}
		if woke, err := time.Parse(time.RFC3339, b.Metadata["last_woke_at"]); err == nil {
			out = append(out, woke.Add(s.cfg.Session.StartupTimeoutDuration()+staleKeyDetectDelay+5*time.Second))
		}
	}
	return out
}

// markEffect records, for the provider's kills, the kind of the v2 effect
// now running (simKill.Effect), until it settles.
func (s *sim) markEffect(e *simEffect) func(settlement) {
	s.sp.mu.Lock()
	s.sp.effect = e.it.Kind
	s.sp.mu.Unlock()
	return func(settlement) {
		s.sp.mu.Lock()
		s.sp.effect = ""
		s.sp.mu.Unlock()
	}
}

// ownRuntimeException is CONTRACT v5's own-runtime exception to I5
// (I-attached), with the effects it covers: a rollback's teardown and S7's
// exit (A10, C3) and the start's lost-commit cleanup (S2) stop the row's own
// runtime (own: O2 Current, or StaleSelf at its generation) or, for a
// pending create, its bead-scoped name (C8.2(a)), attached or not.
func (s *sim) ownRuntimeException(k simKill, row beads.Bead, own bool) bool {
	if !ownRuntimeEffect(k) {
		return false
	}
	return own || row.Metadata["pending_create_claim"] == "true" && ownsName(sessionInfoFromBead(row), k.Name)
}

// ownRuntimeEffect reports a Stop made by an effect the own-runtime
// exception covers: a rollback (its teardown, S7's exit) or a start (its
// lost commit, its abandon's teardown).
func ownRuntimeEffect(k simKill) bool {
	return k.Method == "Stop" && (k.Effect == intentRollback || k.Effect == intentStart)
}

// simStopAdmissions records, per sim, for each stop a pass admitted:
// whether the cache held its row with another drain basis than its backing
// (an outside write whose event the cache had not applied), and how many
// outside drain-basis moves had happened then. A stop's after-stop premise
// refuses a row an outside writer moved after the pass read it, by design,
// whether the move landed before the pass (unseen), between the pass and
// the release (the effect parked), or inside the effect (I16's v5.9n
// excuse); the next pass finalizes it.
var simStopAdmissions = struct {
	sync.Mutex
	by   map[*sim]map[rowKey]stopAdmission
	seen map[*sim]*passTrace // the pass record last noted
}{by: map[*sim]map[rowKey]stopAdmission{}, seen: map[*sim]*passTrace{}}

type stopAdmission struct {
	lagged bool
	moves  int
}

// noteStopAdmissions is a step check: after a pass, it records each stop
// the pass admitted (simStopAdmissions).
func (s *sim) noteStopAdmissions() {
	rec := s.p.out.record.Load()
	simStopAdmissions.Lock()
	defer simStopAdmissions.Unlock()
	if rec == nil || simStopAdmissions.seen[s] == rec {
		return // each pass's admissions are noted once, at that pass's step
	}
	simStopAdmissions.seen[s] = rec
	for _, it := range rec.Admitted {
		l := s.legOf(it.Key.Leg)
		if it.Kind != intentStop || l == nil {
			continue
		}
		cached, err1 := l.cache.Get(it.Key.ID)
		backing, err2 := l.backing.Get(it.Key.ID)
		lagged := err1 == nil && err2 == nil && len(session.FactsDrainBasis.Moved(session.FactsOf(backing.Metadata), session.FactsOf(cached.Metadata))) > 0
		if simStopAdmissions.by[s] == nil {
			simStopAdmissions.by[s] = map[rowKey]stopAdmission{}
		}
		simStopAdmissions.by[s][it.Key] = stopAdmission{lagged: lagged, moves: len(s.basisMoves)}
	}
}

// stopBasisMovedSinceRead reports whether the stop on k was admitted from a
// lagged drain basis, or an outside writer moved the row's drain basis on
// leg since its admission (noteStopAdmissions).
func (s *sim) stopBasisMovedSinceRead(leg string, k rowKey) bool {
	simStopAdmissions.Lock()
	a, ok := simStopAdmissions.by[s][k]
	simStopAdmissions.Unlock()
	return ok && (a.lagged || s.basisMovedOutside(leg, k.ID, a.moves))
}

// unstampedPane reports whether b's name holds a live runtime with no
// session ID (Ownerless, or a token alone).
func (s *sim) unstampedPane(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	return rt != nil && !rt.corpse && rt.id == ""
}

// Kills each v5.9t harness relaxation (mc-cjtkj) that excuses too much: a
// planted bug in the path each relaxation covers still fails its invariant.
func TestSimPendingRelaxationsBite(t *testing.T) {
	row := func(id string, kv ...string) beads.Bead {
		b := sessionRow(id, append([]string{"session_name", "s-" + id, "generation", "2", "instance_token", "tok-2"}, kv...)...)
		b.Status = "open"
		return b
	}
	held := []string{"held_until", plannerT0.Add(time.Hour).Format(time.RFC3339), "sleep_reason", "user-hold", "sleep_intent", "user-hold"}
	closedAs := func(b beads.Bead, state string) beads.Bead {
		c := b
		c.Status = "closed"
		c.Metadata = maps.Clone(b.Metadata)
		maps.Copy(c.Metadata, session.ClosePatch(plannerT0, state))
		c.Metadata["held_until"], c.Metadata["sleep_intent"], c.Metadata["sleep_reason"] = "", "", ""
		return c
	}
	signaled := []string{"state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason}
	for _, c := range []struct {
		name, inv string
		arrange   func(s *sim)
	}{
		// I15: the rollback close may clear a held pending create's holds;
		// a close of a held row that is not a pending create's rollback may
		// not.
		{"I15: a held row closed failed-create without its claim", "I15 ", func(s *sim) {
			b := row("x", held...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: closedAs(b, string(session.StateFailedCreate))})
		}},
		{"I15: a held pending create closed orphaned, not failed-create", "I15 ", func(s *sim) {
			b := row("x", append(held, "pending_create_claim", "true")...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: closedAs(b, "orphaned")})
		}},
		// I5: the own-runtime exception covers a rollback's or a start's
		// stop of the row's own runtime; not a stop verb's, nor another
		// row's runtime under a name the row does not own.
		{"I5: the stop verb kills an attached own runtime", "I5 ", func(s *sim) {
			s.prev = map[string]beads.Bead{"city/x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentStop, Hit: &simRuntime{id: "x", epoch: "2", token: "tok-2", attached: true}})
		}},
		{"I5: a rollback kills another row's attached runtime under a named row's name", "I5 ", func(s *sim) {
			s.prev = map[string]beads.Bead{"city/x": row("x", "session_name", "chat", "pending_create_claim", "true")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "chat", Effect: intentRollback, Hit: &simRuntime{id: "y", epoch: "1", token: "tok-y", attached: true}})
		}},
		// I4: on a row closed within the step, a StaleSelf runtime at its
		// generation is its own only for the own-runtime effects; a closed
		// pending create's bead-scoped name stays its own only for its
		// rollback.
		{"I4: the stop verb kills a just-closed row's StaleSelf runtime at its generation", "I4 ", func(s *sim) {
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentStop, Hit: &simRuntime{id: "x", epoch: "2", token: "tok-old"}})
		}},
		{"I4: a rollback kills an older incarnation under a named row's name", "I4 ", func(s *sim) {
			s.prev = map[string]beads.Bead{"city/x": row("x", "session_name", "chat", "pending_create_claim", "true")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "chat", Effect: intentRollback, Hit: &simRuntime{id: "x", epoch: "1", token: "tok-1"}})
		}},
		{"I4: a stop verb kills another row's runtime under a just-closed pending create's name", "I4 ", func(s *sim) {
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x", "pending_create_claim", "true", "pool_managed", "true")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentStop, Hit: &simRuntime{id: "y", epoch: "2", token: "tok-9"}})
		}},
		// I16: a premise refusal is excused only by a drain-basis move the
		// store shows after the pass read the row; a stop admitted on a
		// current basis that nothing moved still fails, and checkStranded
		// still catches a row the stop never re-issues for.
		{"I16: a premise refusal of a stop admitted on a current, unmoved basis", "I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			k := rowKey{Leg: rowLeg, ID: "gc-1"}
			simStopAdmissions.Lock()
			simStopAdmissions.by[s] = map[rowKey]stopAdmission{k: {moves: len(s.basisMoves)}}
			simStopAdmissions.Unlock()
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: k}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16: a move before the stop's admission excuses nothing", "I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.basisMoves = append(s.basisMoves, s.legs[0].name+"/gc-1")
			k := rowKey{Leg: rowLeg, ID: "gc-1"}
			simStopAdmissions.Lock()
			simStopAdmissions.by[s] = map[rowKey]stopAdmission{k: {moves: len(s.basisMoves)}}
			simStopAdmissions.Unlock()
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: k}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16: signaled at quiescence with no pane, its probe answering", "I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.prev = s.snapshot()
			s.checkStranded()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSim(t, 3, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
				s.sp.put("s-gc-1", "gc-1", "1", "tok-gc-1-1")
				return []beads.Bead{poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1-1")}, nil
			}})
			s.advance(time.Second)
			s.inventory()
			s.advance(time.Second)
			c.arrange(s)
			s.check()
			if !slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, c.inv) }) {
				t.Errorf("the check reported %q, want an %sviolation", s.failures, c.inv)
			}
		})
	}
}

// Kills I10's paced excuses for a start held or attached, and a stop held by
// an unstamped runtime, excusing a retry loop: each is quiescent only at or
// past its row's backoff expiry with no change but the runtime lease record,
// and a stop refused token_absent under a stamped runtime is not paced.
func TestSimPendingPacedRetries(t *testing.T) {
	k := rowKey{Leg: rowLeg, ID: "gc-1"}
	at := plannerT0.Add(time.Hour)
	row := func(kv ...string) map[string]beads.Bead {
		return map[string]beads.Bead{"city/gc-1": sessionRow("gc-1", append([]string{"session_name", "s-gc-1"}, kv...)...)}
	}
	for _, c := range []struct {
		name          string
		kind, cause   string
		until         time.Duration
		stamped       bool
		before, after map[string]beads.Bead
		paced         bool
	}{
		{"a held start at the expiry", intentStart, causeHeld, -time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"a held start before the expiry", intentStart, causeHeld, time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"an attached start at the expiry", intentStart, causeAttached, -time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"a held start that moved another key", intentStart, causeHeld, -time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4", "state", "asleep"), false},
		{"a start refused for another cause", intentStart, causePrepare, -time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"a stop held by an unstamped runtime", intentStop, fenceTokenAbsent, -time.Second, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"a stop refused token_absent under a stamped runtime", intentStop, fenceTokenAbsent, -time.Second, true, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
	} {
		s := &sim{round: []simResult{{it: intent{Kind: c.kind, Key: k}, res: settlement{Outcome: settledRefused, Cause: c.cause}}}}
		rt := &simRuntime{}
		if c.stamped {
			rt.id = "gc-1"
		}
		s.sp = &simProvider{rts: map[string]*simRuntime{"s-gc-1": rt}}
		backoff := map[string]backoffRecord{rowBackoffKey(k): {Consecutive: 2, Until: at.Add(c.until), Cause: c.cause}}
		if got := s.pacedRetries(backoff, at, c.before, c.after); got != c.paced {
			t.Errorf("%s: paced %t, want %t", c.name, got, c.paced)
		}
	}
}

// Kills I10's pending-create timers dropped or misplaced: an open row holding
// its claim arms the never-started deadline, the attempt window and, once
// started, its in-flight window; a closed or claimless row arms none.
func TestSimPendingCreateTimers(t *testing.T) {
	s := newSim(t, 3, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) {
		started := plannerT0.Format(time.RFC3339)
		return []beads.Bead{
			sessionRow("gc-1", "pending_create_claim", "true", "pending_create_started_at", started, "last_woke_at", started),
			sessionRow("gc-2", "pending_create_started_at", started),
		}, nil
	}})
	want := []time.Time{
		plannerT0.Add(pendingCreateNeverStartedTimeout), plannerT0.Add(staleCreatingStateTimeout),
		plannerT0.Add(s.cfg.Session.StartupTimeoutDuration() + staleKeyDetectDelay + 5*time.Second),
	}
	got := s.pendingCreateTimers()
	slices.SortFunc(got, func(a, b time.Time) int { return a.Compare(b) })
	slices.SortFunc(want, func(a, b time.Time) int { return a.Compare(b) })
	if !slices.Equal(got, want) {
		t.Fatalf("timers %v, want %v", got, want)
	}
}
