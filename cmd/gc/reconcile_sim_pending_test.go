package main

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
//     retokened) leaves no live runtime it created under the moved token;
//   - PC4 I-teardown: a rollback that closed its row left nothing C3
//     required it to stop: the row's own runtime (O2: Current, or StaleSelf
//     at its generation) or, for a pool row, anything under its bead-scoped
//     name (C8.2(a), v5.9q).

func init() {
	simEffectChecks = append(simEffectChecks, (*sim).markEffect, (*sim).checkLeaseRollback, (*sim).checkLostCommit, (*sim).checkRollbackTeardown)
	simQuietChecks = append(simQuietChecks, (*sim).checkStaleCreates)
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
		{"a rollback closes without its teardown's stop", "PC4 ", simOpts{specs: func(specs map[string]effectSpec) {
			// The teardown's stop skipped, and the close no longer holds
			// the row open while its own runtime stays (C3's ga-vcjr9
			// guard): P4d and P4e of the #7706 review in one.
			spec := specs[intentRollback]
			spec.sections = slices.Clone(spec.sections)
			spec.sections[0] = called(0, probed(paneScreen, rollbackStep), func(_ context.Context, _ txCaps, in teardown) (teardown, error) { return in, nil })
			spec.sections[1].Decide = func(v txView) txStep {
				td, err := callResult[teardown](v)
				if err != nil || !rollbackable(v.Row) {
					return txStep{Refuse: causeRedecided, Err: err}
				}
				patch, class := rollbackPatch(v.Row, v.Now, td.more)
				return txStep{Terminal: patch, Class: class, Fail: td.fail, Err: td.err}
			}
			specs[intentRollback] = spec
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

// pickExternalOp is a step's outside move: externalOps and the config
// drifts (reconcile_sim_drift_test.go), plus the pending creates' when the
// sim seeds them. A sim that seeds none (the floored corpus) draws no
// pending create.
func (s *sim) pickExternalOp() struct {
	name string
	fn   func(s *sim, l *simLeg, b beads.Bead)
} {
	ops := slices.Concat(externalOps, driftOps) // reconcile_sim_drift_test.go
	if !s.noPendingCreates {
		ops = slices.Concat(ops, pendingCreateOps)
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
		origin := []string{"manual", "ephemeral"}[s.rng.IntN(2)] // gc session new, or an API agent create (the pool side of J45)
		b := sessionRow(id, "template", "worker", "agent_name", "worker", "state", "start-pending", "session_origin", origin,
			"generation", "1", "instance_token", "tok-"+id+"-1", "pending_create_claim", "true", "pending_create_started_at", s.rel(0))
		if _, err := l.backing.Create(b); err != nil {
			s.t.Fatalf("seed %d: manual create: %v", s.seed, err)
		}
		s.logf("manual-create: %s/%s (%s), nameless", l.name, id, origin)
	}},
	{"name-lands", func(s *sim, l *simLeg, _ beads.Bead) {
		rows, _ := l.backing.List(beads.ListQuery{AllowScan: true})
		rows = slices.DeleteFunc(rows, func(b beads.Bead) bool {
			return b.Metadata["session_name"] != "" || b.Metadata["pending_create_claim"] != "true"
		})
		if len(rows) > 0 {
			b := rows[s.rng.IntN(len(rows))]
			s.setMeta(l, b.ID, "session_name", "s-"+b.ID)
			s.logf("name-lands: %s/%s", l.name, b.ID)
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

// pendingCreateRollback reports a v2 close as failed-create of a row
// holding its pending create's claim, or of a row already failed-create
// whose claim a partial rollback or an attach's confirm cleared: a rollback
// (A10, S3) or A21's failed-create close, which end the row and its holds
// with the Retire cell, as legacy's failedCreateClosePatch does whatever
// holds the row (SESS-061). I15 leaves the holds to it; PC1 judges when it
// may land.
func pendingCreateRollback(w simWrite) bool {
	failed := string(session.StateFailedCreate)
	return w.After.Metadata["state"] == failed && (w.Before.Metadata["pending_create_claim"] == "true" || w.Before.Metadata["state"] == failed)
}

// pendingCreateScreenPark reports A10's rollback screen parking a pending
// create instead of closing it (SESS-051/092; CONTRACT v5.9z-1 (i), "close
// or park"): a write of a row holding its claim that leaves it open and
// asleep with a provider terminal error's park or a rate-limit quarantine,
// which legacy's checkRateLimitStability writes whatever holds the row. I15
// leaves it the hold's reason alone: no other held key, no wake, and no
// quarantine shortened (an extension is no change).
func pendingCreateScreenPark(w simWrite, stable bool) bool {
	b, a := w.Before.Metadata, w.After.Metadata
	if b["pending_create_claim"] != "true" || w.After.Status == "closed" || a["state"] != string(session.StateAsleep) {
		return false
	}
	switch session.SleepReason(a["sleep_reason"]) {
	case session.SleepReasonProviderTerminalError:
		if a[sessionProviderTerminalErrorMetadataKey] == "" {
			return false
		}
	case session.SleepReasonRateLimit:
		if a["quarantined_until"] == "" {
			return false
		}
	default:
		return false
	}
	return slices.Equal(dormantChanges(w.Before, a, w.At, stable), []string{"sleep_reason"})
}

// pendingCreateAdopt reports v2's adopt (A10, S2) of a pending create whose
// own runtime is up, a write I15 lets move the row's state alone under a
// hold, as #57 lets a commit (mc-cjtkj ruling; shifted seed 382): legacy's
// recoverRunningPendingCreate adopts with no hold check. The adopt commits
// the claimed creating row awake or active and leaves every hold as it was.
func pendingCreateAdopt(w simWrite, k string) bool {
	b, a := w.Before.Metadata, w.After.Metadata
	if k != "state" || b["pending_create_claim"] != "true" || !pendingCreateQueuedOrCreatingState(b["state"]) ||
		!slices.Contains([]string{"awake", "active"}, a["state"]) {
		return false
	}
	for _, hold := range []string{"held_until", "quarantined_until", "sleep_intent", "sleep_reason", "wait_hold", "suspended_at"} {
		if a[hold] != b[hold] {
			return false
		}
	}
	return true
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

// markEffect records, for the provider's kills, the kind and reason of the
// v2 effect now running (simKill.Effect, Reason), until it settles.
func (s *sim) markEffect(e *simEffect) func(settlement) {
	s.sp.mu.Lock()
	s.sp.effect, s.sp.reason = e.it.Kind, e.it.Reason
	s.sp.mu.Unlock()
	return func(settlement) {
		s.sp.mu.Lock()
		s.sp.effect, s.sp.reason = "", ""
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
// exception covers: a rollback (its teardown, S7's exit), a start (its
// lost commit, its abandon's teardown), or A13's restart, which waives the
// attach leg (CONTRACT v5.10u).
func ownRuntimeEffect(k simKill) bool {
	return k.Method == "Stop" && (k.Effect == intentRollback || k.Effect == intentStart || k.Effect == intentStop && k.Reason == decideRestartRequested)
}

// stopAdmission is a stop a pass admitted (sim.stopAdmissions): whether
// the cache held its row with another drain basis than its backing (an
// outside write whose event the cache had not applied), and how many outside
// drain-basis moves had happened then. A stop's after-stop premise refuses a
// row an outside writer moved after the pass read it, by design, whether the
// move landed before the pass (unseen), between the pass and the release
// (the effect parked), or inside the effect (I16's v5.9n excuse); the next
// pass finalizes it.
type stopAdmission struct {
	lagged bool
	moves  int
}

// noteStopAdmissions records the stops pass record rec admitted, as the pass
// admits them (sim.pass).
func (s *sim) noteStopAdmissions(rec *passTrace) {
	if rec == nil {
		return
	}
	for _, it := range rec.Admitted {
		l := s.legOf(it.Key.Leg)
		if it.Kind != intentStop || l == nil {
			continue
		}
		cached, err1 := l.cache.Get(it.Key.ID)
		backing, err2 := l.backing.Get(it.Key.ID)
		lagged := err1 == nil && err2 == nil && len(session.FactsDrainBasis.Moved(session.FactsOf(backing.Metadata), session.FactsOf(cached.Metadata))) > 0
		if s.stopAdmissions == nil {
			s.stopAdmissions = map[rowKey]stopAdmission{}
		}
		s.stopAdmissions[it.Key] = stopAdmission{lagged: lagged, moves: len(s.basisMoves)}
	}
}

// stopBasisMovedSinceRead reports whether the stop on k was admitted from a
// lagged drain basis, or an outside writer moved the row's drain basis on
// leg since its admission (noteStopAdmissions).
func (s *sim) stopBasisMovedSinceRead(leg string, k rowKey) bool {
	a, ok := s.stopAdmissions[k]
	return ok && (a.lagged || s.basisMovedOutside(leg, k.ID, a.moves))
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
		{"I15: a held row, not failed-create and claimless, closed failed-create", "I15 ", func(s *sim) {
			b := row("x", append(held, "state", "asleep")...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: closedAs(b, string(session.StateFailedCreate))})
		}},
		{"I15: a held failed-create row closed orphaned", "I15 ", func(s *sim) {
			b := row("x", append(held, "state", string(session.StateFailedCreate))...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: closedAs(b, "orphaned")})
		}},
		// I15's adopt exception: a claimed pending create committed awake
		// keeps its holds; a claimless row woken under a hold, or an adopt
		// that clears a hold, still fails.
		{"I15: a held row without a claim moved creating to awake", "I15 ", func(s *sim) {
			b := row("x", append(held, "state", "creating")...)
			a := row("x", append(held, "state", "awake")...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: a})
		}},
		{"I15: an adopt of a held pending create that drops its user-hold intent", "I15 ", func(s *sim) {
			b := row("x", append(held, "state", "creating", "pending_create_claim", "true")...)
			a := row("x", append(held, "state", "awake", "pending_create_claim", "", "sleep_intent", "")...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: a})
		}},
		{"I15: an adopt of a held pending create that clears its hold", "I15 ", func(s *sim) {
			b := row("x", append(held, "state", "creating", "pending_create_claim", "true")...)
			a := row("x", "state", "awake", "pending_create_claim", "")
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: a})
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
		// I4's just-closed own-teardown exception (v5.10e): only a rollback's
		// or a start's stop of the row's own StaleSelf at its generation; at
		// another generation it still fails.
		{"I4: a rollback kills a just-closed row's StaleSelf runtime at another generation", "I4 ", func(s *sim) {
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentRollback, Hit: &simRuntime{id: "x", epoch: "1", token: "tok-old"}})
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
			s.stopAdmissions = map[rowKey]stopAdmission{k: {moves: len(s.basisMoves)}}
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: k}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16: a move before the stop's admission excuses nothing", "I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.basisMoves = append(s.basisMoves, s.legs[0].name+"/gc-1")
			k := rowKey{Leg: rowLeg, ID: "gc-1"}
			s.stopAdmissions = map[rowKey]stopAdmission{k: {moves: len(s.basisMoves)}}
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: k}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		// I15's named-reopen exception (mc-i7efm): only a configured named
		// session's closed row reopened; a reopen of another closed row, or
		// a named row's open write, that clears a hold still fails.
		{"I15: a reopen of a closed row no configured named session holds", "I15 ", func(s *sim) {
			b := row("x", held...)
			b.Status = "closed"
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: row("x", "state", "stopped")})
		}},
		{"I15: a named row's open write that clears its hold", "I15 ", func(s *sim) {
			b := row("x", append(held, "configured_named_session", "true")...)
			s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: row("x", "configured_named_session", "true", "state", "stopped")})
		}},
		// I16's deadline exception (mc-i7efm): a stop abandoned at its
		// deadline only; one that failed on any other cause still fails.
		{"I16: a stop failed on its write with no pane, left signaled", "I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			k := rowKey{Leg: rowLeg, ID: "gc-1"}
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: k}})(settlement{Outcome: settledFailed, Cause: causeWrite})
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

// Kills I10's paced excuse for a start refused held excusing a retry loop or
// the code's own cause: it is quiescent only at or past its row's backoff
// expiry with no change but the runtime lease record, and only when the
// sim's own hold model holds the row (simHeld). A refusal the model does not
// back (planted X1), and a start refused attached on an unattached pane
// (planted X2), are no pacing; one refused attached under an attached pane
// is. A rollback refused liveness-unknown is paced only over a
// probe that keeps erroring.
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
		attached      bool
		probeErr      bool
		before, after map[string]beads.Bead
		paced         bool
	}{
		{"a held start at the expiry", intentStart, causeHeld, -time.Second, false, false, row("runtime_lease_epoch", "3", "sleep_intent", "user-hold"), row("runtime_lease_epoch", "4", "sleep_intent", "user-hold"), true},
		{"a start refused held on a row the hold model holds not (X1)", intentStart, causeHeld, -time.Second, false, false, row("runtime_lease_epoch", "3", "sleep_reason", "idle"), row("runtime_lease_epoch", "4", "sleep_reason", "idle"), false},
		{"a held start before the expiry", intentStart, causeHeld, time.Second, false, false, row("runtime_lease_epoch", "3", "sleep_intent", "user-hold"), row("runtime_lease_epoch", "4", "sleep_intent", "user-hold"), false},
		{"an attached start at the expiry (paced: shifted seed 474)", intentStart, causeAttached, -time.Second, true, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"an attached start before the expiry", intentStart, causeAttached, time.Second, true, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"a start refused attached on an unattached pane (X2)", intentStart, causeAttached, -time.Second, false, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"a held start that moved another key", intentStart, causeHeld, -time.Second, false, false, row("runtime_lease_epoch", "3", "sleep_intent", "user-hold"), row("runtime_lease_epoch", "4", "sleep_intent", "user-hold", "state", "asleep"), false},
		{"a start refused for another cause", intentStart, causePrepare, -time.Second, false, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"a rollback refused liveness-unknown over an erroring probe", intentRollback, causeLivenessUnknown, -time.Second, false, true, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"a rollback refused liveness-unknown with its probe answering", intentRollback, causeLivenessUnknown, -time.Second, false, false, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
	} {
		s := &sim{round: []simResult{{it: intent{Kind: c.kind, Key: k}, res: settlement{Outcome: settledRefused, Cause: c.cause}}}}
		rt := &simRuntime{id: "gc-1", attached: c.attached, probeErr: c.probeErr}
		s.sp = &simProvider{rts: map[string]*simRuntime{"s-gc-1": rt}}
		backoff := map[string]backoffRecord{rowBackoffKey(k): {Consecutive: 2, Until: at.Add(c.until), Cause: c.cause}}
		if got := s.pacedRetries(backoff, at, c.before, c.after); got != c.paced {
			t.Errorf("%s: paced %t, want %t", c.name, got, c.paced)
		}
	}
}

// checkRollbackTeardown is PC4 I-teardown: a rollback whose close landed
// left no live runtime C3 required it to stop, under the name the row held:
// its own (O2: Current, or StaleSelf at its generation) or, under a pool
// row's bead-scoped name, any (C8.2(a), v5.9q).
func (s *sim) checkRollbackTeardown(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if e.it.Kind != intentRollback || l == nil {
		return func(settlement) {}
	}
	before, err := l.backing.Get(e.it.Key.ID)
	if err != nil || before.Status == "closed" {
		return func(settlement) {}
	}
	return func(res settlement) {
		name := before.Metadata["session_name"]
		if !res.Closed || name == "" {
			return
		}
		s.sp.mu.Lock()
		rt := s.sp.rts[name]
		s.sp.mu.Unlock()
		if rt == nil || rt.corpse {
			return
		}
		own := rt.id == before.ID && (rt.token == before.Metadata["instance_token"] || rt.epoch == before.Metadata["generation"])
		if own || ownsName(sessionInfoFromBead(before), name) {
			s.failf("PC4 I-teardown", "rollback %s (%s) closed the row and left %s's runtime %s/%s/%s running", e.it.Key.ID, e.it.Reason, name, rt.id, rt.epoch, rt.token)
		}
	}
}

// simHeld is the sim's own hold model for I10's paced start excuse, apart
// from the code under test's: an operator's user-hold, a hold or quarantine
// timer in force at now, a kill fence, or a wait hold. A soft suspend is no
// hold (v5.9u, #7718: A18 holds it at the arm, so no start is refused for
// it).
func simHeld(b beads.Bead, now time.Time) bool {
	m := b.Metadata
	future := func(k string) bool {
		t, err := time.Parse(time.RFC3339, m[k])
		return err == nil && t.After(now)
	}
	return m["sleep_intent"] == string(session.SleepReasonUserHold) || future("held_until") || future("quarantined_until") ||
		session.IsKillPendingInfo(sessionInfoFromBead(b), now) || strings.TrimSpace(m["wait_hold"]) != ""
}

// TestSimCorpusFlooredPending is the floored corpus with pending creates
// seeded (mc-cjtkj review): a min_active_sessions floor beside every pending
// create path. Its floor-ack coverage is TestSimCorpusFloored's; this one
// gates the invariants over floors and pending creates together. CI runs all
// 1024 seeds in ranges (BUILD.bazel).
func TestSimCorpusFlooredPending(t *testing.T) {
	t.Cleanup(func() { writeSimCoverage(t) })
	for _, seed := range simSeeds(t) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			s := newSim(t, seed, simOpts{})
			s.cfg.Agents[0].MinActiveSessions = intPtr(1 + int(seed%2))
			s.run()
			s.report(t)
		})
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

// Kills a start that launches over a row closed under it before its
// PreWake (P7a of the #7706 review, which only the corpus caught): the
// start's premise refuses a closed row, so no PreWake lands on it and its
// provider Start never runs.
func TestStartRefusesARowClosedBeforeItsPreWake(t *testing.T) {
	k, leaf := launchKit(t, "asleep", "wake_attempts", "1")
	started := false
	leaf.onStart = func(context.Context) { started = true }
	gen := k.meta("generation")
	if err := k.backing.Close("gc-1"); err != nil {
		t.Fatal(err)
	}
	s := k.runKind()
	if started || k.meta("generation") != gen || s.Outcome == settledLanded {
		t.Fatalf("settlement %+v, started %v, generation %q (was %q): want refused, no PreWake, no Start", s, started, k.meta("generation"), gen)
	}
}

// Kills I15 failing v2's adopt of a held pending create (the adopt exception,
// shifted seed 382): a claimed creating row committed awake under a user hold
// and a live quarantine, every hold kept, is no violation, as legacy's
// recoverRunningPendingCreate adopts with no hold check.
func TestSimPendingAdoptUnderHold(t *testing.T) {
	s := newSim(t, 3, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) { return nil, nil }})
	held := []string{
		"held_until", plannerT0.Add(time.Hour).Format(time.RFC3339), "sleep_intent", "user-hold", "sleep_reason", "user-hold",
		"quarantined_until", plannerT0.Add(time.Hour).Format(time.RFC3339),
	}
	b := sessionRow("x", append([]string{"session_name", "s-x", "generation", "2", "instance_token", "tok-2", "state", "creating", "pending_create_claim", "true"}, held...)...)
	a := sessionRow("x", append([]string{"session_name", "s-x", "generation", "2", "instance_token", "tok-2", "state", "awake"}, held...)...)
	b.Status, a.Status = "open", "open"
	s.checkWrite(simWrite{Leg: "city", At: s.clk.Now(), Before: b, After: a})
	if len(s.failures) > 0 {
		t.Fatalf("an adopt that keeps every hold: failures %q, want none", s.failures)
	}
}

// pc2Mutant is A10 skipping S7's exit; pc3Mutant a lost commit that leaves
// its runtime (no Moved hook). Directed scenarios drive both, not the
// seeded sweep (the #7706 review).
var (
	pc2Mutant = simOpts{arms: mutateArm("A10", reflect.ValueOf(armRollback).Pointer(), func(r *rowFacts) (intent, bool) {
		it, ok := armRollback(r)
		if it.Reason == decideStaleCreate {
			return intent{}, false
		}
		return it, ok
	})}
	pc3Mutant = simOpts{specs: func(specs map[string]effectSpec) {
		spec := specs[intentStart]
		spec.sections = slices.Clone(spec.sections)
		for i := range spec.sections {
			spec.sections[i].Moved = nil
		}
		specs[intentStart] = spec
	}}
)

// staleCreateSim is a sim over one manual pending create inside its lease
// whose name holds its own StaleSelf runtime at its generation (S7).
func staleCreateSim(t *testing.T, o simOpts) *sim {
	t.Helper()
	row := sessionRow("gc-m1", "template", "worker", "agent_name", "worker", "state", "start-pending", "session_origin", "manual",
		"generation", "2", "instance_token", "tok-gc-m1-2", "pending_create_claim", "true", "pending_create_started_at", rowAt(-time.Minute), "session_name", "s-gc-m1")
	o.rows = func(s *sim) ([]beads.Bead, []beads.Bead) {
		s.sp.put("s-gc-m1", "gc-m1", "2", "tok-gc-m1-2-old")
		return []beads.Bead{row}, nil
	}
	s := newSim(t, 1, o)
	s.lag = false
	s.advance(time.Second)
	return s
}

// Kills PC2 (I-S7) not catching a skipped S7 exit: a pending create whose
// name holds its own StaleSelf runtime is rolled back (S7) in production; with
// A10 skipping S7's exit it rests there at quiescence, which PC2 reports.
func TestSimPC2CatchesASkippedStaleCreateExit(t *testing.T) {
	for _, c := range []struct {
		name string
		o    simOpts
		want bool
	}{
		{"production", simOpts{}, false},
		{"A10 skips S7's exit", pc2Mutant, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := staleCreateSim(t, c.o)
			for range 3 {
				s.settle()
				s.advance(simPatrol)
			}
			s.quiesce()
			got := slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "PC2 ") })
			if got != c.want {
				t.Fatalf("PC2 reported %v, want %v; failures %q", got, c.want, s.failures)
			}
		})
	}
}

// Kills PC3 (I-lost-commit) not catching a lost commit left running: an
// outside close after the start's launch, with the start's Moved hook gone,
// leaves the runtime it created, which PC3 reports.
func TestSimPC3CatchesALostCommitLeftRunning(t *testing.T) {
	closeRow := func(_ *sim, l *simLeg, b beads.Bead) { _ = l.backing.Close(b.ID) }
	for seed := uint64(1); seed <= 32; seed++ {
		s, outcome, _, fired := lostCommitRunWith(t, seed, pc3Mutant, closeRow)
		if !fired || outcome == "" || strings.Contains(outcome, "outcome 1 ") {
			continue
		}
		if !slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "PC3 ") }) {
			t.Fatalf("seed %d: %q, failures %q: want PC3 to report the start's runtime left running", seed, outcome, s.failures)
		}
		return
	}
	t.Fatal("no seed reached the lost commit")
}

// Kills I15's failed-create exception narrowed to a held claim (shifted
// seeds 329, 665; SESS-061): A21's close of a row already failed-create,
// its claim cleared, retires a heartbeat's hold as legacy's
// failedCreateClosePatch does; the same close of a committed row still
// fails I15, and so does a hold rewritten without a close.
func TestFailedCreateCloseRetiresHolds(t *testing.T) {
	held := rowAt(time.Hour)
	closed := func(state string, closes bool, meta ...string) simWrite {
		before := poolRow("gc-1", "worker", 1, state, append([]string{"held_until", held}, meta...)...)
		after := poolRow("gc-1", "worker", 1, string(session.StateFailedCreate), "held_until", "")
		if closes {
			after.Status = "closed"
		}
		return simWrite{Leg: simCityLeg, Before: before, After: after, At: plannerT0}
	}
	for _, c := range []struct {
		name  string
		w     simWrite
		fails bool
	}{
		{"a held claim", closed("creating", true, "pending_create_claim", "true"), false},
		{"already failed-create, its claim cleared", closed(string(session.StateFailedCreate), true), false},
		{"a committed row", closed("awake", true), true},
		{"already failed-create, no close", closed(string(session.StateFailedCreate), false), true},
	} {
		s := scripted(t, nil, poolRow("gc-1", "worker", 1, "asleep"))
		s.checkWrite(c.w)
		failed := slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "I15 ") })
		if failed != c.fails {
			t.Errorf("%s: failures %q, want an I15 failure %t", c.name, s.failures, c.fails)
		}
	}
}

// Kills I15's failed-create exception covering a close only (CONTRACT
// v5.9z-1 (i), "close or park"): A10's rollback screen that parks a held
// pending create with a provider terminal error, or quarantines it for a
// rate limit, may rewrite the hold's sleep reason; the same park of a row
// holding no claim, or a park that also rewrites the hold's timer, fails
// I15.
func TestRollbackScreenParkRewritesHolds(t *testing.T) {
	held := []string{"sleep_intent", "user-hold", "held_until", rowAt(1000 * time.Hour), "sleep_reason", "user-hold"}
	quarantined := func(d time.Duration) []string { return append(slices.Clone(held), "quarantined_until", rowAt(d)) }
	park := func(claim bool, base []string, patch map[string]string) simWrite {
		before := poolRow("gc-1", "worker", 1, "creating", base...)
		if claim {
			before.Metadata["pending_create_claim"] = "true"
		}
		after := poolRow("gc-1", "worker", 1, "creating", base...)
		maps.Copy(after.Metadata, patch)
		return simWrite{Leg: simCityLeg, Before: before, After: after, At: plannerT0}
	}
	closed := func(w simWrite) simWrite { w.After.Status = "closed"; return w }
	// The parks are production's: A10's screen writes these patches.
	terminal := providerTerminalErrorPatch("model_not_found", plannerT0)
	rateLimit := maps.Clone(session.RateLimitQuarantinePatch())
	rateLimit["quarantined_until"] = rowAt(30 * time.Minute)
	with := func(p map[string]string, kv ...string) map[string]string {
		p = maps.Clone(p)
		for i := 0; i+1 < len(kv); i += 2 {
			p[kv[i]] = kv[i+1]
		}
		return p
	}
	without := func(p map[string]string, k string) map[string]string { p = maps.Clone(p); delete(p, k); return p }
	for _, c := range []struct {
		name  string
		w     simWrite
		fails bool
	}{
		{"a terminal-error park of a held pending create", park(true, held, terminal), false},
		{"a rate-limit quarantine of a held pending create", park(true, held, rateLimit), false},
		{"a rate-limit park extending an operator's quarantine", park(true, quarantined(10*time.Minute), rateLimit), false},
		{"the same park of a row holding no claim", park(false, held, terminal), true},
		{"a park that also clears the hold's timer", park(true, held, with(terminal, "held_until", "")), true},
		{"a park that closes the row", closed(park(true, held, terminal)), true},
		{"an asleep write with another reason", park(true, held, with(terminal, "sleep_reason", string(session.SleepReasonIdle))), true},
		{"a terminal-error park with no terminal error", park(true, held, without(terminal, sessionProviderTerminalErrorMetadataKey)), true},
		{"a rate-limit park with no quarantine", park(true, held, without(rateLimit, "quarantined_until")), true},
		{"a rate-limit park shortening an operator's quarantine", park(true, quarantined(24*time.Hour), rateLimit), true},
		{"a terminal-error write that wakes the held row", park(true, held, with(terminal, "state", "active")), true},
		{"a rate-limit write that wakes the held row", park(true, held, with(rateLimit, "state", "active")), true},
		{"a terminal-error park that leaves the row creating", park(true, held, without(terminal, "state")), true},
		{"a rate-limit park that leaves the row creating", park(true, held, without(rateLimit, "state")), true},
	} {
		s := scripted(t, nil, poolRow("gc-1", "worker", 1, "asleep"))
		s.checkWrite(c.w)
		failed := slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "I15 ") })
		if failed != c.fails {
			t.Errorf("%s: failures %q, want an I15 failure %t", c.name, s.failures, c.fails)
		}
	}
}

// Kills I10's timer table counting what holds no plan back (mc-i7efm): an
// ambiguous create's hard bound counts, a landed create's entry does not;
// a fresh pool create's post-create protection counts, a committed row
// past its creation_complete does not.
func TestSimLastTimerCounts(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(s *sim)
		want    time.Duration
	}{
		{"nothing", func(*sim) {}, 0},
		{"an ambiguous create", func(s *sim) {
			s.inflight.creates["tok-a"] = inflightEntry{Kind: inflightCreate, Token: "tok-a", Ambiguous: true, SettledAt: s.clk.Now()}
		}, inflightHardBound},
		{"a running create", func(s *sim) {
			s.inflight.creates["tok-a"] = inflightEntry{Kind: inflightCreate, Token: "tok-a", SettledAt: s.clk.Now()}
		}, 0},
		{"a fresh pool create", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", "state", "awake", "state_reason", "creation_complete", "creation_complete_at", s.rel(0))
		}, postCreateProtectionTimeout},
		{"a committed row past its creation", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", "state", "awake", "state_reason", "", "creation_complete_at", s.rel(0))
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSim(t, 3, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) {
				return []beads.Bead{poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-gc-1-1")}, nil
			}})
			c.arrange(s)
			if got := s.lastTimer().Sub(s.clk.Now()); got != c.want {
				t.Errorf("lastTimer = now+%s, want now+%s", got, c.want)
			}
		})
	}
}
