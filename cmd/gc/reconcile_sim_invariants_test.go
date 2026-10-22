package main

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The simulator's invariant checks (D1a; CONTRACT v5 §10) beyond I8 and I10,
// against ground truth: I1, I7, I15, I23 and AL1's census-only rows, which
// merged code can break, each with a seeded mutant; and I2, I4, I5, I9, I14,
// I16, I17 and I24, whose writers have not merged, each shown to report by
// TestSimChecksBite until its PR adds a mutant.

func init() {
	simStepChecks = append(simStepChecks, (*sim).checkCaps, (*sim).checkStarts, (*sim).checkDestructive, (*sim).checkTokens, (*sim).checkDead, (*sim).checkQuarantinedCommits)
	simWriteChecks = append(simWriteChecks, (*sim).checkWrite)
	simEffectChecks = append(simEffectChecks, (*sim).checkFinalized, (*sim).checkCensusOnly, (*sim).checkPremiseRefusal)
	simQuietChecks = append(simQuietChecks, (*sim).checkDwell, (*sim).checkStranded)
	simMutants = append(simMutants, []struct {
		name, inv string
		opts      simOpts
	}{
		{"admit ignores the effects in flight", "I1 ", simOpts{inflight: func(m *inflightMap) plannerInflight { return hidingInflight{m} }}},
		{"a heal writes a state legacy does not know", "I7 ", simOpts{arms: mutateHeal(func(r *rowFacts) (intent, bool) {
			it, ok := armTimerHeals(r)
			if it.Kind != "" {
				it.Patch["state"] = "healing"
			}
			return it, ok
		})}},
		{"a heal misreads its timer and clears an operator hold", "I15 ", simOpts{arms: mutateHeal(func(r *rowFacts) (intent, bool) {
			patch, class, _ := timerHealPatch(r.row.Info, r.w.Now.Add(time.Hour))
			if len(patch) == 0 && class.Len() == 0 {
				return intent{}, false
			}
			return intent{Kind: intentRowHeal, Reason: decideTimerHeal, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Patch: patch, Class: class}, true
		})}},
		// The I15 carve-out for A6's stability clear (v5.8) holds only for a
		// committed row whose own runtime the deciding pass read alive 30s
		// past its wake (TestSimI15StabilityCarveOut pins the rest).
		{"a stability clear inside 30s of the wake ends a live quarantine", "I15 ", simOpts{arms: mutateHeal(quarantineClear(func(r *rowFacts, stable bool) bool {
			return committed(r.row.Info) && r.aliveProbed() && !stable
		}))}},
		{"an arm acts on a census-only row", "AL1 ", simOpts{arms: func(arms []rowArm) []rowArm {
			return slices.DeleteFunc(arms, func(a rowArm) bool {
				return reflect.ValueOf(a.decide).Pointer() == reflect.ValueOf(armCensusOnly).Pointer()
			})
		}}},
	}...)
}

// quarantineClear is an A6 that clears a set quarantined_until where when
// says, given whether the row woke 30s or more before the pass.
func quarantineClear(when func(r *rowFacts, stable bool) bool) func(r *rowFacts) (intent, bool) {
	return func(r *rowFacts) (intent, bool) {
		woke, err := time.Parse(time.RFC3339, r.row.Info.LastWokeAt)
		if r.row.Info.QuarantinedUntil == "" || err != nil || !when(r, r.w.Now.Sub(woke) >= stabilityThreshold) {
			return intent{}, false
		}
		return r.healClass(intentRowHeal, decideStabilityClear, session.MetadataPatch{}, session.CellInfo(r.row.Info, session.ActorController, session.DispEvent{Kind: session.EvQuarantine}, r.w.Now))
	}
}

// hidingInflight is an admission that ignores the effects in flight: the pass
// sees an empty map.
type hidingInflight struct{ *inflightMap }

func (hidingInflight) view() inflightView { return inflightView{} }

// mutateHeal replaces A6's timer heal with heal.
func mutateHeal(heal func(r *rowFacts) (intent, bool)) func([]rowArm) []rowArm {
	return func(arms []rowArm) []rowArm {
		for i := range arms {
			if arms[i].name == "A6" {
				arms[i].decide = heal
			}
		}
		return arms
	}
}

// checkCaps is I1 (I-cap, P4): the effects in flight never exceed their
// class's cap, counted on the planner's own map, whatever admission saw.
func (s *sim) checkCaps() {
	probe := s.cfg.Daemon.ProbeConcurrencyOrDefault()
	caps := map[capClass]int{capStarts: s.cfg.Daemon.MaxWakesPerTickOrDefault(), capCreates: createsInFlightCap, capProbing: probe, capRowWrites: probe}
	count := make(map[capClass]int)
	for _, e := range s.inflight.view().Entries {
		if spec, ok := effectSpecs[e.Kind]; ok && !e.Ambiguous {
			count[spec.class]++
		}
	}
	for c, n := range count {
		if n > caps[c] {
			s.failf("I1 I-cap", "%d effects of cap class %d in flight, cap %d", n, c, caps[c])
		}
	}
}

// checkStarts is I2 (I-start: at most one runtime v2 creates per row and
// intent token) and I24 (I-fresh-start: every v2 Start is FreshOnly).
func (s *sim) checkStarts() {
	created := make(map[[2]string]int)
	for _, st := range s.sp.starts {
		if st.External {
			continue
		}
		if !st.FreshOnly {
			s.failf("I24 I-fresh-start", "v2 started %s without FreshOnly", st.Name)
		}
		if k := [2]string{st.ID, st.Token}; st.Created {
			if created[k]++; created[k] > 1 {
				s.failf("I2 I-start", "v2 created a second runtime for row %s token %s", st.ID, st.Token)
			}
		}
	}
}

// checkDestructive is I4 (I-fence) and I5 (I-attached) over v2's provider
// kills, each checked at its step: each hits its row's own runtime (O2
// Current, or the own-runtime exception's StaleSelf with epoch =
// generation), any corpse (the corpse exception) or the row's own zombie
// (the start recycle's and the close's), or a pending create's bead-scoped
// name (C8.2(a)), and never an attached one. The own-runtime exception to
// I5 is D1b's to model with the effects it covers.
func (s *sim) checkDestructive() {
	kills := s.sp.kills[min(s.killsChecked, len(s.sp.kills)):] // each judged once, on the rows before its step
	s.killsChecked = len(s.sp.kills)
	for _, k := range kills {
		if k.External || k.Hit == nil {
			continue
		}
		row, closed := s.rowNamed(k.Name)
		own := k.Hit.id == row.ID && k.Hit.token == row.Metadata["instance_token"]
		// The own-runtime exception takes O2's own-runtime test: Current, or
		// StaleSelf at the row's generation (S7's exit stops that one;
		// seeds 23, 38, 153, 519, 1007).
		ownForException := own || k.Hit.id == row.ID && k.Hit.epoch == row.Metadata["generation"]
		if k.Hit.attached && !s.ownRuntimeException(k, row, ownForException) {
			s.failf("I5 I-attached", "v2 %s on %s while attached", k.Method, k.Name)
		}
		switch {
		case k.Method == "KillCorpseObject" && k.Hit.corpse: // the corpse exception, whoever it was
		case k.Method == "KillZombieObject" && k.Hit.zombie && k.Hit.id != "" && k.Hit.id == s.rowNamedEver(k.Name).ID:
			// The row's own zombie: the start recycle's and the close's, the
			// row closed meanwhile included (its zombie stays its own).
		case k.Method == "Stop" && row.Metadata["pending_create_claim"] == "true" && ownsName(sessionInfoFromBead(row), k.Name) && (!closed || own || k.Effect == intentRollback):
			// C8.2(a): a pending create's rollback stops its bead-scoped name
			// whoever runs under it (C3, S1's Foreign row), as legacy does;
			// the name embeds the row's bead ID, so it stays the row's when
			// an outside writer closes the row inside the rollback (v5.9q).
		case row.ID == "" || k.Hit.id != row.ID:
			s.failf("I4 I-fence", "v2 %s on %s hit row %q's runtime, not its own", k.Method, k.Name, k.Hit.id)
		case closed && !own && (k.Effect != intentRollback || !ownForException):
			// A pending create's rollback stops the row's own runtime, Current
			// or StaleSelf at its generation, as PC4 requires of it
			// (checkRollbackTeardown), the row closed meanwhile included.
			s.failf("I4 I-fence", "v2 %s on %s hit token %s, not the just-closed row %s's", k.Method, k.Name, k.Hit.token, row.ID)
		case k.Hit.token != row.Metadata["instance_token"] && k.Hit.epoch != row.Metadata["generation"]:
			s.failf("I4 I-fence", "v2 %s on %s hit token %s, not row %s's", k.Method, k.Name, k.Hit.token, row.ID)
		}
	}
}

// rowNamedEver is the row that carried name before this step, open or
// closed, preferring an open one.
func (s *sim) rowNamedEver(name string) beads.Bead {
	if b, _ := s.rowNamed(name); b.ID != "" {
		return b
	}
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; b.Metadata["session_name"] == name {
			return b
		}
	}
	return beads.Bead{}
}

// rowNamed is the open row that carried name before this step, or (closed)
// the row an outside writer closed within this step, as it stood before its
// close (v5.9n): stopping a just-closed row's own runtime is no fence breach,
// and unleased closes are real (bd close on a session row). A close in an
// earlier step excuses nothing (beginStep).
func (s *sim) rowNamed(name string) (row beads.Bead, closed bool) {
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; b.Status != "closed" && b.Metadata["session_name"] == name {
			return b, false
		}
	}
	b, ok := s.closedInStep[name]
	return b, ok
}

// checkTokens is I9 (I-tokens): in any window, admitted starts ≤ capacity
// plus the refill over the window, with no refund.
func (s *sim) checkTokens() {
	capacity := s.cfg.Daemon.MaxWakesPerTickOrDefault()
	period := simPatrol / time.Duration(capacity)
	for i := range s.admitted {
		for j := i; j < len(s.admitted); j++ {
			if n := j - i + 1; n > capacity+int(s.admitted[j].Sub(s.admitted[i])/period) {
				s.failf("I9 I-tokens", "%d starts admitted within %s, capacity %d", n, s.admitted[j].Sub(s.admitted[i]), capacity)
			}
		}
	}
}

// stopKeys are the stop request's five keys (v5 D1, R1).
var stopKeys = []string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey, session.DrainAckIncarnationKey, session.DrainAckAtKey}

// checkWrite checks one row change v2 made:
//   - I7 (I-legacy): it writes no session row state outside
//     knownSessionStates, but the signaled projection (draining +
//     stop-pending) and a close's state, which is its close reason as legacy
//     writes it (killed, quarantine, ...); a wait or nudge bead's state is
//     its own;
//   - I14 (I-STOP-1/2): a stop request names the row's current incarnation;
//   - I15 (I-STOP-3/4): no request survives a v2 PreWake, and no write
//     rewrites what an operator holds dormant (dormantChange);
//   - I4 (I-fence): no terminal write lands while the row's own runtime has a
//     live pane, which C8.8 would have read;
//   - AL1 (census-only): it writes no row off the sessions leg, which legacy
//     never reconciles and a shared rig store fills with other cities' rows.
func (s *sim) checkWrite(w simWrite) {
	b, a := w.Before.Metadata, w.After.Metadata
	id := w.Leg + "/" + w.After.ID
	if w.Leg != simCityLeg {
		s.failf("AL1 census-only", "v2 wrote %s, a census-only row", id)
	}
	closes := w.After.Status == "closed" && w.Before.Status != "closed"
	if st := a["state"]; w.After.Type == sessionBeadType && st != b["state"] && !closes && !knownSessionStates[st] && (st != string(session.StateDraining) || a["state_reason"] != session.DrainAckStopPendingReason) {
		s.failf("I7 I-legacy", "v2 wrote state %q on %s", st, id)
	}
	if inc := a[session.DrainIntentIncarnationKey]; inc != "" && inc != b[session.DrainIntentIncarnationKey] && inc != a["generation"] {
		s.failf("I14 I-STOP-1/2", "v2 bound %s's stop request to incarnation %s at generation %s", id, inc, a["generation"])
	}
	if w.Before.ID == "" {
		return
	}
	if a["generation"] != b["generation"] {
		if s.preWakeQuarantine == nil {
			s.preWakeQuarantine = map[string]string{}
		}
		s.preWakeQuarantine[id] = a["quarantined_until"]
		for _, k := range stopKeys {
			if a[k] != "" {
				s.failf("I15 I-STOP-3/4", "%s survived v2's PreWake of %s", k, id)
			}
		}
	}
	if k := dormantChange(w.Before, a, w.At, s.stable[w.After.ID]); k != "" && (!closes || !closeEndsSuspend(w.Before, k, w.At)) && (!closes || !pendingCreateRollback(w)) && !pendingCreateScreenPark(w, s.stable[w.After.ID]) && !pendingCreateAdopt(w, k) && !s.quarantinedCommit(id, w, k) && !namedReopen(w) {
		s.failf("I15 I-STOP-3/4", "v2 rewrote %s %q -> %q on %s, which an operator holds dormant at %s", k, b[k], a[k], id, w.At.Format(time.RFC3339))
	}
	terminal := (w.After.Status == "closed" && w.Before.Status != "closed") ||
		(a["state"] != b["state"] && slices.Contains([]string{"asleep", "drained", "stopped", "closed", "orphaned", string(session.StateFailedCreate)}, a["state"]))
	// A zombie's agent is dead: S5's screens park or quarantine it in place
	// (a terminal provider error or a rate-limit screen, with no stop), and
	// A6 heals one A12 classified asleep, as legacy does (CONTRACT v5.9m).
	screened := w.After.Status != "closed" && s.zombiePane(w.Before) &&
		(a["state"] == string(session.StateAsleep) && s.classifiedZombie(w.Before) || slices.Contains([]string{string(session.SleepReasonProviderTerminalError), string(session.SleepReasonRateLimit)}, a["sleep_reason"]))
	if terminal && s.livePane(w.Before) && !screened {
		s.failf("I4 I-fence", "v2 wrote %s terminal (state %q) while its runtime has a live pane", id, a["state"])
	}
}

// quarantineDrainBound is how long a row whose commit kept a late
// quarantine may stay up undrained: A6's stability clear ends the
// quarantine of a row alive 30s past its wake, and an undesired row's A20
// begin waits out INC-003's grace.
const quarantineDrainBound = 6 * simPatrol

// quarantinedCommit is I15's exception for §12.2 #57: S2's commit of a row
// whose quarantine landed after its PreWake, its only hold, moves state
// alone, and the row is then held to checkQuarantinedCommits.
func (s *sim) quarantinedCommit(id string, w simWrite, k string) bool {
	b, a := w.Before.Metadata, w.After.Metadata
	if k != "state" || b["state"] != string(session.StateCreating) || !slices.Contains([]string{"awake", "active"}, a["state"]) ||
		!quarantineAlone(b, w.At) || a["quarantined_until"] != b["quarantined_until"] || a["sleep_reason"] != b["sleep_reason"] {
		return false
	}
	if at, ok := s.preWakeQuarantine[id]; ok && at == b["quarantined_until"] {
		return false // the quarantine was there at the PreWake, which must have refused
	}
	if s.quarantinedCommits == nil {
		s.quarantinedCommits = map[string]*quarantinedClock{}
	}
	s.quarantinedCommits[id] = &quarantinedClock{start: w.At, checked: w.At, moves: len(s.basisMoves), refusals: len(s.premiseRefusals)}
	return true
}

// quarantineAlone reports meta's quarantine in force at now as its only
// hold: no operator suspend or hold, no wait hold, no held_until timer.
func quarantineAlone(meta map[string]string, now time.Time) bool {
	held, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["held_until"]))
	return session.DecodeDisposition(meta).Quarantine.After(now) &&
		session.State(strings.TrimSpace(meta["state"])) != session.StateSuspended && strings.TrimSpace(meta["wait_hold"]) == "" &&
		strings.TrimSpace(meta["sleep_intent"]) != string(session.SleepReasonUserHold) && (err != nil || !held.After(now))
}

// quarantinedClock is a quarantined commit's bound clock (CONTRACT v5.9x):
// start moves on while v2 cannot act on the row, from the last check
// (checked), and restarts at an outside write that moved its drain basis
// past basisMoves[moves].
type quarantinedClock struct {
	start, checked  time.Time
	moves, refusals int
}

// noteWriter records actor as the last writer of every key the write from
// before to after changed on row k (the status as "status").
func (s *sim) noteWriter(k, actor string, before, after beads.Bead) {
	if s.lastWriter == nil {
		s.lastWriter = map[string]map[string]string{}
	}
	keys := s.lastWriter[k]
	if keys == nil {
		keys = map[string]string{}
		s.lastWriter[k] = keys
	}
	if before.Status != after.Status {
		keys["status"] = actor
	}
	for key := range metaKeys(before.Metadata, after.Metadata) {
		if before.Metadata[key] != after.Metadata[key] {
			keys[key] = actor
		}
	}
}

// metaKeys is the union of a's and b's keys.
func metaKeys(a, b map[string]string) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// checkPremiseRefusal records, for I15's #57 clock (v5.9x, v5.9z-1), a row
// whose own effect an outside write refused on its premise: it settled
// refused premise, and a premise key the refusal names as moved
// (errPremiseMoved's list) differs between the row the cache held at its
// release (what the pass read) and the store at its settlement, last
// changed by an outside writer. v2 could not act on the row then.
func (s *sim) checkPremiseRefusal(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if l == nil {
		return func(settlement) {}
	}
	cached, err := l.cache.Get(e.it.Key.ID)
	if err != nil {
		return func(settlement) {}
	}
	cached.Metadata = maps.Clone(cached.Metadata)
	return func(res settlement) {
		if res.Cause != causePremise || res.Err == nil {
			return
		}
		fresh, err := l.backing.Get(e.it.Key.ID)
		if err != nil {
			return
		}
		k := l.name + "/" + e.it.Key.ID
		writers := s.lastWriter[k]
		for _, key := range premiseMovedKeys(res.Err) {
			before, after, writer := cached.Metadata[key], fresh.Metadata[key], writers[key]
			if key == "closed" {
				before, after, writer = cached.Status, fresh.Status, writers["status"]
			}
			if before != after && writer != "" && writer != "v2" {
				s.premiseRefusals = append(s.premiseRefusals, k)
				return
			}
		}
	}
}

// premiseMovedKeys are the premise keys a premise refusal's error names as
// moved (errPremiseMoved: "...: a, b, c").
func premiseMovedKeys(err error) []string {
	_, list, ok := strings.Cut(err.Error(), errPremiseMoved.Error()+": ")
	if !ok {
		return nil
	}
	return strings.Split(list, ", ")
}

// unknownState reports b in a state A5 holds (the census's UnknownState:
// one main does not know, but drain-ack stop-pending), on which no arm may
// drain it (CONTRACT v5.10i; seed 437 of the floored-pending corpus).
func (s *sim) unknownState(b beads.Bead) bool {
	info := sessionInfoFromBead(b)
	return !isKnownStateInfo(info) && !isDrainAckStopPendingInfo(info)
}

// checkQuarantinedCommits is I15's bound on §12.2 #57: a row whose commit
// kept a late quarantine is, within quarantineDrainBound of v2's own time,
// closed, stopped, asked to stop (A20's begin; A4 stops it), out of its
// quarantine, or kept by its assigned work, as legacy's assigned-work lens
// keeps it (DRAIN-046). The bound pauses while the row's liveness reads
// unknown (A9 holds every arm, GUAR-053) or its state is one A5 holds
// (v5.10i), and restarts at an outside write
// that moves its incarnation or drain basis, or that refuses the row's own
// pending effect on its premise (checkPremiseRefusal), whose refusal and
// backoff are not v2's delay (CONTRACT v5.9x).
func (s *sim) checkQuarantinedCommits() {
	if len(s.quarantinedCommits) == 0 {
		return
	}
	now := s.clk.Now()
	rows := s.snapshot()
	_, obs := s.observed()
	for id, c := range s.quarantinedCommits {
		b := rows[id]
		leg, _, _ := strings.Cut(id, "/")
		switch {
		case s.basisMovedOutside(leg, b.ID, c.moves), slices.Contains(s.premiseRefusals[min(c.refusals, len(s.premiseRefusals)):], leg+"/"+b.ID):
			c.start, c.moves, c.refusals = now, len(s.basisMoves), len(s.premiseRefusals)
		case obs == nil || obs[rowKey{Leg: rowLeg, ID: b.ID}].Liveness == livenessUnknown, s.unknownState(b):
			c.start = c.start.Add(now.Sub(c.checked))
		}
		c.checked = now
		at := c.start
		worked := slices.ContainsFunc(slices.Collect(maps.Values(rows)), func(w beads.Bead) bool {
			return w.Assignee == b.ID && (w.Status == "open" || w.Status == "in_progress")
		})
		switch {
		case b.Status == "closed", !s.livePane(b), b.Metadata[session.DrainIntentReasonKey] != "",
			!session.DecodeDisposition(b.Metadata).Quarantine.After(now), worked:
			delete(s.quarantinedCommits, id)
		case now.Sub(at) > quarantineDrainBound:
			s.failf("I15 I-STOP-3/4", "%s, committed under a quarantine that landed after its PreWake, is still up undrained %s later", id, now.Sub(at))
			delete(s.quarantinedCommits, id)
		}
	}
}

// classifiedZombie reports b's runtime classified by the planner (A12), by
// its dead token.
func (s *sim) classifiedZombie(b beads.Bead) bool {
	s.sp.mu.Lock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	s.sp.mu.Unlock()
	if rt == nil {
		return false
	}
	tok, ok := s.p.classified[rowKey{Leg: rowLeg, ID: b.ID}]
	return ok && !tok.Recycle && tok.classifies(runtimeIdentity{SessionID: rt.id, Token: rt.token})
}

// zombiePane reports whether b's own runtime is a zombie: its pane up, its
// agent dead.
func (s *sim) zombiePane(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	return rt != nil && rt.zombie && !rt.corpse && rt.id == b.ID
}

// suspendKey are a suspend's keys, which an A21 close may end: CONTRACT
// v5.7 A10 frees a wisp-tier row in a suspended state (MAINT-051's sweep), and
// A21's orphan close ends a suspended row whose agent is gone. Its deliberate
// holds (a kill fence, a hold timer, a quarantine) it never ends.
var suspendKey = map[string]bool{"state": true, "suspended_at": true}

// closeEndsSuspend reports whether a close may change k on b at t: k is a
// suspend's key and no deliberate hold is in force or unreadable (§12.2 #27).
// A held suspended row is never closed.
// namedReopen reports w a configured named session's reopen of its closed
// row (v5 C2, AM-N2): a fresh spawn on the row, which retires the closed
// row's holds (its suspend, hold, quarantine and their reasons) as legacy's
// reopenNamedSessionBatch does (the disposition's Retire cell and
// ClearWakeBlockersPatch), so no dormant key it moves is an operator's
// (ruling on the creates generator's seeds 250, 783 and 1001, mc-i7efm).
func namedReopen(w simWrite) bool {
	return w.Before.Status == "closed" && w.After.Status != "closed" && w.Before.Metadata["configured_named_session"] == "true"
}

func closeEndsSuspend(b beads.Bead, k string, t time.Time) bool {
	return suspendKey[k] && !session.DecodeDisposition(b.Metadata).HoldsDeliberately(t)
}

// dormantChange returns a key a v2 write changed on b that an operator owns
// at t, or "": an honored kill fence owns the row's lifecycle and its sleep
// reason, a suspend the row's state (but for the dormant stop's drain
// completion, asleep or drained), and an unexpired hold or quarantine its
// timer and sleep reason, though A6's stability clear may clear the
// quarantine's timer, and with it its own quarantine or rate_limit reason
// (§12.2 #57, mc-bwvzv), on a row its pass read stable (v5.8, SESS-539;
// stable, sim.stable), and a crash, churn or rate-limit quarantine may
// extend the timer, never shorten it (quarantineExtension). No such row is
// woken. Only an operator-dormant row (D1 rule 2: killed, suspended under a
// hold, or sleep_intent=user-hold) is never given a stop
// request, though one it holds may be cleared; a hold or quarantine alone
// is not dormant, and legacy drains under one (owner ruling at C6a2).
func dormantChange(b beads.Bead, a map[string]string, t time.Time, stable bool) string {
	if ks := dormantChanges(b, a, t, stable); len(ks) > 0 {
		return ks[0]
	}
	return ""
}

// dormantChanges is every key dormantChange would report, in its order.
func dormantChanges(b beads.Bead, a map[string]string, t time.Time, stable bool) (out []string) {
	add := func(k string) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	m := b.Metadata
	until := func(k string) bool { at, err := time.Parse(time.RFC3339, m[k]); return err == nil && at.After(t) }
	reason := session.SleepReason(m["sleep_reason"])
	var owned []string
	dormant := strings.TrimSpace(m["sleep_intent"]) == string(session.SleepReasonUserHold)
	if session.KillPendingMetadata(m["state"], m["state_reason"], m["sleep_reason"], m["slept_at"], t) {
		owned, dormant = append(owned, "state", "state_reason", "sleep_reason", "slept_at"), true
	}
	// A soft suspend, under no hold, is no operator's (CONTRACT v5.9u):
	// legacy's state heal and its work's wake take it. A city stop's
	// sleep_reason beside it is no hold, as softSuspended reads it.
	suspended := m["state"] == string(session.StateSuspended) && !softSuspended(session.Info{
		MetadataState: m["state"], HeldUntil: m["held_until"], QuarantinedUntil: m["quarantined_until"], SleepIntent: m["sleep_intent"],
		WaitHold: m["wait_hold"], SuspendedAt: m["suspended_at"], SleepReason: m["sleep_reason"],
	}, t)
	if suspended {
		owned, dormant = append(owned, "suspended_at"), true
	}
	if until("held_until") {
		owned = append(owned, "held_until")
		if reason == session.SleepReasonUserHold {
			owned = append(owned, "sleep_reason")
		}
	}
	quarantined := until("quarantined_until")
	if quarantined {
		ended := stable && a["quarantined_until"] == "" // A6's stability clear ends it (SESS-539)
		if !ended && !quarantineExtension(m, a, t) {
			owned = append(owned, "quarantined_until")
		}
		// The clear ends a quarantine's own reason with it (§12.2 #57,
		// mc-bwvzv); a churn sleep's stays the quarantine's to keep.
		ownReason := (reason == session.SleepReasonQuarantine || reason == session.SleepReasonRateLimit) && a["sleep_reason"] == "" // cleared, never rewritten
		if slices.Contains([]session.SleepReason{session.SleepReasonQuarantine, session.SleepReasonContextChurn, session.SleepReasonRateLimit}, reason) && (!ended || !ownReason) {
			owned = append(owned, "sleep_reason")
		}
	}
	for _, k := range owned {
		if a[k] != m[k] {
			add(k)
		}
	}
	// A suspend owns the row's state, but for the drain completion the
	// operator-dormant stop writes, as legacy's does (asleep or drained,
	// the hold kept; C6b2 ruling).
	if suspended && a["state"] != m["state"] && a["state"] != string(session.StateAsleep) && a["state"] != string(session.StateDrained) {
		add("state")
	}
	for _, k := range stopKeys { // the residue void clears them (v5 D1, R1 as amended at C6a)
		if dormant && a[k] != m[k] && a[k] != "" {
			add(k)
		}
	}
	// An agent heartbeat's hold (held_until with no intent) does not hold
	// A17's respawn (v5.9i): waking a row it alone holds is no violation.
	heartbeat := len(owned) == 1 && owned[0] == "held_until" && !quarantined && strings.TrimSpace(m["sleep_intent"]) == ""
	if (dormant || quarantined || len(owned) > 0) && !heartbeat && a["state"] != m["state"] && slices.Contains([]string{"awake", "active", "creating", string(session.StateStartPending)}, a["state"]) {
		add("state")
	}
	return out
}

// quarantineExtension reports a write's extension of b's live quarantine
// to a that I15 accepts (the #7706 and #7726 reviews): a later deadline, no
// later than the write's time t plus the longest quarantine the controller
// writes, on a write carrying an accrual's or a rate-limit quarantine's
// signature: its counter advanced, a quarantine reason the write itself
// sets, or last_woke_at cleared. A quarantine reason the row already
// carries signs nothing, and nothing shortens one.
func quarantineExtension(b, a map[string]string, t time.Time) bool {
	was, _ := time.Parse(time.RFC3339, b["quarantined_until"])
	later, err := time.Parse(time.RFC3339, a["quarantined_until"])
	if err != nil || !later.After(was) || later.After(t.Add(max(defaultQuarantineDuration, defaultRateLimitQuarantineDuration))) {
		return false
	}
	advanced := func(k string) bool {
		n, _ := strconv.Atoi(b[k])
		m, err := strconv.Atoi(a[k])
		return err == nil && m > n
	}
	reason := session.SleepReason(a["sleep_reason"])
	setsReason := a["sleep_reason"] != b["sleep_reason"] &&
		(reason == session.SleepReasonQuarantine || reason == session.SleepReasonContextChurn || reason == session.SleepReasonRateLimit)
	clearsWake := b["last_woke_at"] != "" && a["last_woke_at"] == ""
	return advanced("wake_attempts") || advanced("churn_count") || setsReason || clearsWake
}

// checkStranded is I16 (I-STOP-5) at quiescence: no row is still signaled
// with no live pane under its name, which the next pass's A4 confirms and
// finalizes; so a premise refusal I16 excused (an outside drain-basis write
// inside the stop) is finalized by then (v5.9n). I17 sees only live panes.
func (s *sim) checkStranded() {
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; signaledRow(b) && !s.namedPane(b) {
			s.failf("I16 I-STOP-5", "%s is still signaled at quiescence with no live pane: the next pass never finalized it", k)
		}
	}
}

// probeErring reports whether b's name holds a runtime whose process probe
// errors.
func (s *sim) probeErring(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	return rt != nil && rt.probeErr
}

// livePane reports whether b's own runtime (its ID and token) has a live
// pane: what a C8.8 confirmation reads as not stopped.
func (s *sim) livePane(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	return rt != nil && !rt.corpse && rt.id == b.ID && rt.token == b.Metadata["instance_token"]
}

// namedPane reports whether b's name holds a live pane the stop verb must
// not finalize over: b's own runtime, whatever its token, an ownerless one
// (no identity: C8.3 defers and escalates on it, as livePane counts it), or,
// under b's bead-scoped pool name, any runtime (never Foreign there, v5.2
// D3; L2 refuses it). Another row's runtime under any other name is the
// Foreign verdict, which proves b's gone.
func (s *sim) namedPane(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	name := b.Metadata["session_name"]
	rt := s.sp.rts[name]
	beadScoped := b.Metadata["pool_managed"] == "true" && strings.HasSuffix(name, "-"+b.ID)
	return rt != nil && !rt.corpse && (rt.id == b.ID || rt.id == "" || beadScoped)
}

// basisMovedOutside reports an outside write since basisMoves[from] that
// moved row id's drain basis on leg (session.FactsDrainBasis).
func (s *sim) basisMovedOutside(leg, id string, from int) bool {
	return slices.Contains(s.basisMoves[min(from, len(s.basisMoves)):], leg+"/"+id)
}

func signaledRow(b beads.Bead) bool {
	return b.Status != "closed" && b.Metadata["state"] == string(session.StateDraining) && b.Metadata["state_reason"] == session.DrainAckStopPendingReason
}

// checkFinalized is I16 (I-STOP-5): a stop effect that ran while its row's
// name held no live pane, the absence C8.8 confirms, leaves the row's
// request finalized. A live pane of another token under the name is no
// absence: the verb refuses it (L2), and finalizes nothing. A premise
// refusal is excused when, and only when, an outside writer moved a
// drain-basis key of the row inside the effect's window (v5.9n): the
// after-stop premise refuses it by design, and the next pass finalizes,
// which I17's bound holds it to.
func (s *sim) checkFinalized(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if e.it.Kind != intentStop || l == nil {
		return func(settlement) {}
	}
	before, _ := l.backing.Get(e.it.Key.ID)
	paneBefore := s.namedPane(before)
	window := len(s.basisMoves)
	return func(res settlement) {
		b, err := l.backing.Get(e.it.Key.ID)
		if res.Cause == causePremise && (s.basisMovedOutside(l.name, e.it.Key.ID, window) || s.stopBasisMovedSinceRead(l.name, e.it.Key)) {
			return
		}
		if res.Cause == causeDeadline {
			return // abandoned at its deadline, the stop confirmed nothing; I17's bound holds the row (ruling on seed 222 of the floored-pending corpus, mc-i7efm)
		}
		if res.Cause == fenceLivenessUnknown && s.probeErring(b) {
			return // an erroring probe proves no absence (O3), as I17 rules it not stoppable (v5.9n; seed 606 of the floored-pending corpus)
		}
		if !paneBefore && res.Cause != "name-busy" && err == nil && signaledRow(b) && !s.namedPane(b) {
			s.failf("I16 I-STOP-5", "the stop effect on %v confirmed no live pane and left the request signaled", e.it.Key)
		}
	}
}

// checkCensusOnly is AL1 (census-only) on what the planner admits: no
// effect runs for a row off the sessions leg, which gather gives no writer,
// so checkWrite alone would not see the arm that proposed it.
func (s *sim) checkCensusOnly(e *simEffect) func(settlement) {
	if e.it.Kind != intentCreate && s.legOf(e.it.Key.Leg) != s.legs[0] {
		s.failf("AL1 census-only", "v2 admitted %s for %v, a census-only row", e.it.Kind, e.it.Key)
	}
	return func(settlement) {}
}

// legOf is the leg whose cache the census reads as ref.
func (s *sim) legOf(ref string) *simLeg {
	legs, err := sessionCensusStoreCandidates(s.env.CityPath, s.cfg, s.legs[0].cache, s.env.RigStores(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, c := range legs {
		for _, l := range s.legs {
			if c.ref == ref && c.store == beads.Store(l.cache) {
				return l
			}
		}
	}
	return nil
}

// checkDwell is I17 (I-STOP-7), at quiescence: past the bound, no row is
// still signaled whose runtime would pass the fence (its own, live,
// unattached).
// A runtime whose process probe errors is not stoppable (v5.9n: D3 waives
// liveness only on identity Current, as legacy's SESS-056 defers). No
// runtime fault changes during quiescence, so the bound, from quiescence's
// start, runs from the first pass whose probe succeeds.
func (s *sim) checkDwell() {
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; signaledRow(b) && s.livePane(b) && !s.sp.IsAttached(b.Metadata["session_name"]) && !s.probeErring(b) {
			s.failf("I17 I-STOP-7", "%s is still signaled past the bound with a stoppable runtime", k)
		}
	}
}

// observed is the census a pass would read now and C2d's classes for it.
func (s *sim) observed() (*sessionCensus, map[rowKey]rowObservation) {
	legs, err := sessionCensusStoreCandidates(s.env.CityPath, s.cfg, s.legs[0].cache, s.env.RigStores(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	c, err := readSessionCensus(s.clk.Now(), legs)
	if err != nil {
		return nil, nil
	}
	return c, observeCensus(s.lane.cache.Snapshot(), c, s.clk.Now(), 2*simPatrol)
}

// checkDead is I23 (I-dead): where the latest fresh inventory pass listed a
// row's runtime after its last change (by the provider's version), the census and a fresh read agree on
// a corpse or a zombie: the census reads it dead (or unknown, its identity
// unread), never alive, gone or occupied; and reads nothing else dead, an
// agent whose probe errored included.
func (s *sim) checkDead() {
	pass := s.lane.cache.Snapshot().Inventory
	if pass.FinishedAt.IsZero() || s.clk.Now().Sub(pass.FinishedAt) > 2*simPatrol {
		return
	}
	c, obs := s.observed()
	if c == nil {
		return
	}
	listed := pass.listedOn()
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	for _, row := range c.Canonical() {
		name := row.Info.SessionName
		rt := s.sp.rts[name]
		if _, ok := listed[name]; !ok || rt == nil || rt.id != row.Key.ID || s.sp.changed[name] > s.listed {
			continue
		}
		fresh := runtimeLiveness(rt)
		dead := fresh.Present() && !fresh.Alive
		switch got := obs[row.Key].Liveness; {
		case dead && !rt.probeErr && got != livenessDead && got != livenessUnknown: // an erroring probe proves nothing (O3)
			s.failf("I23 I-dead", "the census reads %s %s; a fresh read finds it dead (corpse %t)", name, got, fresh.Corpse)
		case !dead && got == livenessDead:
			s.failf("I23 I-dead", "the census reads %s dead; a fresh read finds it alive", name)
		}
	}
}

// KillCorpseObject and KillZombieObject are LL2's exact-object kills: each
// kills only the object a read observed, still in the shape it requires, as
// the if-shell re-check does (v5 F2).
func (p *simProvider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	return p.killObject("KillCorpseObject", name, objectID, created, func(rt *simRuntime) bool { return rt.corpse })
}

func (p *simProvider) KillZombieObject(name, objectID, created, _ string) (runtime.SessionObjectKillResult, error) {
	return p.killObject("KillZombieObject", name, objectID, created, func(rt *simRuntime) bool { return !rt.corpse && rt.zombie })
}

func (p *simProvider) killObject(method, name, objectID, created string, shape func(*simRuntime) bool) (runtime.SessionObjectKillResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch rt := p.rts[name]; {
	case rt == nil || fmt.Sprintf("$%d", rt.object) != objectID || strconv.FormatInt(rt.created, 10) != created:
		return runtime.SessionObjectGone, nil
	case !shape(rt):
		return runtime.SessionObjectLive, nil
	}
	p.killLocked(method, name)
	return runtime.SessionObjectKilled, nil
}

// Kills a check that cannot fail: each invariant whose writers have not
// merged reports a violation fed straight to it.
func TestSimChecksBite(t *testing.T) {
	row := func(id string, kv ...string) beads.Bead {
		b := sessionRow(id, append([]string{"session_name", "s-" + id, "generation", "2", "instance_token", "tok-2"}, kv...)...)
		b.Status = "open"
		return b
	}
	signaled := []string{"state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason}
	for _, c := range []struct {
		inv     string
		arrange func(s *sim)
	}{
		{"I2 ", func(s *sim) {
			for range 2 {
				s.sp.starts = append(s.sp.starts, simStart{Name: "s-x", ID: "x", Token: "tok-2", FreshOnly: true, Created: true})
			}
		}},
		{"I24 ", func(s *sim) { s.sp.starts = append(s.sp.starts, simStart{Name: "s-x", ID: "x", Token: "tok-2"}) }},
		{"I4 ", func(s *sim) { // a kill of another incarnation's runtime
			s.prev = map[string]beads.Bead{"city/x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Hit: &simRuntime{id: "x", epoch: "1", token: "tok-1"}})
		}},
		{"I4 ", func(s *sim) { // a zombie kill on another row's zombie (B33)
			s.prev = map[string]beads.Bead{"city/x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "KillZombieObject", Name: "s-x", Hit: &simRuntime{id: "other", epoch: "2", token: "tok-9", zombie: true}})
		}},
		{"I4 ", func(s *sim) { // an asleep write over a zombie classified under another token (E08)
			s.sp.put("s-x", "x", "2", "tok-2")
			s.sp.rts["s-x"].zombie = true
			s.p.classified = map[rowKey]classifiedRuntime{{Leg: rowLeg, ID: "x"}: {Token: "x/tok-old"}}
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "state", "active"), After: row("x", "state", "asleep")})
		}},
		{"I4 ", func(s *sim) { // an asleep write over an unclassified zombie (T1)
			s.sp.put("s-x", "x", "2", "tok-2")
			s.sp.rts["s-x"].zombie = true
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "state", "active"), After: row("x", "state", "asleep")})
		}},
		{"I4 ", func(s *sim) { // a terminal write over a live pane
			s.sp.put("s-x", "x", "2", "tok-2")
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "state", "active"), After: row("x", "state", "asleep")})
		}},
		{"I4 ", func(s *sim) { // a just-closed row's runtime of another incarnation (v5.9n)
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Hit: &simRuntime{id: "x", epoch: "2", token: "tok-1"}})
		}},
		{"I4 ", func(s *sim) { // a just-closed pending create's own name, another row's runtime (v5.9n)
			x := poolRow("x", "worker", 1, "creating", "session_name", "s-x", "instance_token", "tok-2", "generation", "2", "pending_create_claim", "true")
			x.Status = "open"
			if !ownsName(sessionInfoFromBead(x), "s-x") {
				s.t.Fatal("the row does not own s-x: the case would not reach the C8.2(a) exception")
			}
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": x}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Hit: &simRuntime{id: "y", epoch: "2", token: "tok-9"}})
		}},
		{"I4 ", func(s *sim) { // a just-closed row's rollback kill of its runtime at another generation (v5.10e (3) excuses only its own generation; the #7799 review's s5)
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentRollback, Hit: &simRuntime{id: "x", epoch: "1", token: "tok-1-stale"}})
		}},
		{"I4 ", func(s *sim) { // a row closed in an earlier step excuses nothing (v5.9n)
			s.prev = map[string]beads.Bead{}
			s.closedInStep = map[string]beads.Bead{"s-x": row("x")}
			s.beginStep()
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Hit: &simRuntime{id: "x", epoch: "2", token: "tok-2"}})
		}},
		{"I5 ", func(s *sim) {
			s.sp.kills = append(s.sp.kills, simKill{Method: "KillCorpseObject", Name: "s-x", Hit: &simRuntime{corpse: true, attached: true}})
		}},
		{"I9 ", func(s *sim) {
			for range s.cfg.Daemon.MaxWakesPerTickOrDefault() + 1 {
				s.admitted = append(s.admitted, s.clk.Now())
			}
		}},
		{"I14 ", func(s *sim) {
			s.checkWrite(simWrite{Leg: "city", Before: row("x"), After: row("x", session.DrainIntentIncarnationKey, "1")})
		}},
		{"I15 ", func(s *sim) { // a request that survives a v2 PreWake
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "generation", "1"), After: row("x", session.DrainAckIncarnationKey, "1")})
		}},
		{"I15 ", func(s *sim) { // a commit over a quarantine its PreWake already saw (no #57 exception)
			q := s.rel(time.Hour)
			s.preWakeQuarantine = map[string]string{"city/x": q}
			s.checkWrite(simWrite{
				Leg: "city", At: s.clk.Now(), Before: row("x", "state", "creating", "quarantined_until", q, "sleep_reason", "quarantine"),
				After: row("x", "state", "awake", "quarantined_until", q, "sleep_reason", "quarantine"),
			})
		}},
		{"I15 ", func(s *sim) { // §12.2 #57: a late-quarantined commit left up undrained past the bound
			s.setMeta(s.legs[0], "gc-1", "quarantined_until", s.rel(time.Hour), "sleep_reason", "quarantine", "instance_token", "tok-gc-1-1", "generation", "1")
			s.quarantinedCommits = map[string]*quarantinedClock{"city/gc-1": {start: s.clk.Now().Add(-quarantineDrainBound - time.Second), checked: s.clk.Now(), moves: len(s.basisMoves)}}
		}},
		{"I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})(settlement{Outcome: settledLanded})
		}},
		{"I16 ", func(s *sim) { // a premise refusal no outside basis write excuses (v5.9n)
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16 ", func(s *sim) { // a basis write before the effect's window excuses nothing (v5.9n)
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.basisMoves = append(s.basisMoves, s.legs[0].name+"/gc-1")
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16 ", func(s *sim) { // another row's basis write excuses nothing (v5.9n)
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			settle := s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})
			s.basisMoves = append(s.basisMoves, s.legs[0].name+"/gc-2")
			settle(settlement{Outcome: settledRefused, Cause: causePremise})
		}},
		{"I16 ", func(s *sim) { // signaled at quiescence with no pane: never finalized (v5.9n)
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.prev = s.snapshot()
			s.checkStranded()
		}},
		{"I17 ", func(s *sim) {
			s.prev = map[string]beads.Bead{"city/x": row("x", signaled...)}
			s.sp.put("s-x", "x", "2", "tok-2")
			s.checkDwell()
		}},
		{"I23 ", func(s *sim) { // the census still reads alive what is now a corpse
			s.sp.rts["s-gc-1"].corpse = true // its version stays the one listed
		}},
	} {
		t.Run(c.inv, func(t *testing.T) {
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

// Kills an I15 checker whose dormant set strays from D1 rule 2: a stop
// request on a held or quarantined row is no violation; on a user-hold,
// suspended or killed row it is. Waking any of them is (the C6a2 review's
// must-fix: any operator-owned row woken is a violation).
func TestDormantChangeFollowsRule2(t *testing.T) {
	at := plannerT0
	soon := at.Add(time.Minute).Format(time.RFC3339)
	for _, c := range []struct {
		name    string
		meta    map[string]string
		dormant bool
	}{
		{"held", map[string]string{"held_until": soon, "sleep_reason": "user-hold"}, false},
		{"quarantined", map[string]string{"quarantined_until": soon, "sleep_reason": "quarantine"}, false},
		{"user-hold", map[string]string{"sleep_intent": "user-hold"}, true},
		{"suspended under a hold timer", map[string]string{"state": "suspended", "held_until": soon}, true},
		{"killed", map[string]string{"state": "asleep", "state_reason": session.KillPendingReason, "sleep_reason": "killed", "slept_at": at.Format(time.RFC3339)}, true},
	} {
		after := maps.Clone(c.meta)
		after[session.DrainIntentReasonKey] = "orphaned"
		if got := dormantChange(beads.Bead{Metadata: c.meta}, after, at, false); (got != "") != c.dormant {
			t.Errorf("%s: dormantChange = %q, want a violation %v", c.name, got, c.dormant)
		}
		woken := maps.Clone(c.meta)
		woken["state"] = "active"
		if got := dormantChange(beads.Bead{Metadata: c.meta}, woken, at, false); got == "" {
			t.Errorf("%s: a wake is no violation", c.name)
		}
	}
	// A soft suspend is no operator's (v5.9u): its stop request, its wake
	// and its heal by the runtime are free.
	soft := map[string]string{"state": "suspended", "suspended_at": at.Add(-time.Hour).Format(time.RFC3339)}
	cityStop := maps.Clone(soft)
	cityStop["sleep_reason"] = "city-stop"
	for _, m := range []map[string]string{soft, cityStop} {
		for _, st := range []string{"active", "awake", "asleep"} {
			after := maps.Clone(m)
			after["state"], after[session.DrainIntentReasonKey] = st, "orphaned"
			if got := dormantChange(beads.Bead{Metadata: m}, after, at, false); got != "" {
				t.Errorf("soft suspend %v to %s: dormantChange = %q, want none", m, st, got)
			}
		}
	}
}

// I15's close relaxation ends a bare suspend and nothing an operator holds
// (§12.2 #27): a held suspended row is never closed. A bare suspend, and one
// beside an unreadable timer, which holds no PreWake, are soft (v5.9u): no
// operator owns them, so I15 has nothing to relax.
func TestCloseEndsSuspend(t *testing.T) {
	now := gatherNow
	later, bad := now.Add(time.Hour).Format(time.RFC3339), "soon"
	for _, tc := range []struct {
		name string
		meta []string
		want bool
	}{
		{"a bare suspend", nil, true},
		{"a user hold", []string{"sleep_intent", "user-hold"}, false},
		{"a hold timer", []string{"held_until", later}, false},
		{"an unreadable hold timer", []string{"held_until", bad}, true},
		{"a quarantine", []string{"quarantined_until", later}, false},
		{"a wait hold", []string{"wait_hold", "true"}, false},
		{"an expired hold timer", []string{"held_until", now.Add(-time.Hour).Format(time.RFC3339)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{"state": string(session.StateSuspended), "suspended_at": now.Add(-time.Hour).Format(time.RFC3339)}
			for i := 0; i+1 < len(tc.meta); i += 2 {
				m[tc.meta[i]] = tc.meta[i+1]
			}
			after := maps.Clone(m)
			maps.Copy(after, session.ClosePatch(now, "orphaned"))
			b := beads.Bead{ID: "gc-1", Status: "open", Metadata: m}
			k := dormantChange(b, after, now, false)
			if got := k == "" || closeEndsSuspend(b, k, now); got != tc.want {
				t.Fatalf("change %q exempted = %t, want %t", k, got, tc.want)
			}
		})
	}
}

// TestSimPacedRetries (v5.9n, I10). Kills a pacing rule that excuses a retry
// loop: a refused re-issue held by an attach is quiescent only at or past
// its row's backoff expiry and with no change but the runtime lease record;
// one before the expiry, one with any other change, or one refused for
// another cause is not. A fresh heal refused liveness-unknown over an
// erroring probe is paced as the stop is (v5.10l).
func TestSimPacedRetries(t *testing.T) {
	k := rowKey{Leg: rowLeg, ID: "gc-1"}
	at := plannerT0.Add(time.Hour)
	row := func(kv ...string) map[string]beads.Bead {
		return map[string]beads.Bead{"city/gc-1": sessionRow("gc-1", kv...)}
	}
	for _, c := range []struct {
		name          string
		kind          string
		until         time.Duration
		cause         string
		before, after map[string]beads.Bead
		paced         bool
	}{
		{"at the expiry, the lease alone", intentStop, -time.Second, fenceAttached, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), true},
		{"before the expiry", intentStop, time.Second, fenceAttached, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"another key moved", intentStop, -time.Second, fenceAttached, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4", "state", "asleep"), false},
		{"another cause", intentStop, -time.Second, fencePending, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"no backoff record", intentStop, 0, fenceAttached, row("runtime_lease_epoch", "3"), row("runtime_lease_epoch", "4"), false},
		{"liveness unknown, the probe erroring, at the expiry", intentStop, -time.Second, fenceLivenessUnknown, row("session_name", "s-gc-1", "runtime_lease_epoch", "3"), row("session_name", "s-gc-1", "runtime_lease_epoch", "4"), true},
		{"liveness unknown, the probe erroring, before the expiry", intentStop, time.Second, fenceLivenessUnknown, row("session_name", "s-gc-1", "runtime_lease_epoch", "3"), row("session_name", "s-gc-1", "runtime_lease_epoch", "4"), false},
		{"liveness unknown, the probe reading", intentStop, -time.Second, fenceLivenessUnknown, row("session_name", "s-gc-2", "runtime_lease_epoch", "3"), row("session_name", "s-gc-2", "runtime_lease_epoch", "4"), false},
		{"fresh heal, liveness unknown, the probe erroring", intentRowHealFresh, -time.Second, causeLivenessUnknown, row("session_name", "s-gc-1"), row("session_name", "s-gc-1"), true},
		{"fresh heal, liveness unknown, the probe reading", intentRowHealFresh, -time.Second, causeLivenessUnknown, row("session_name", "s-gc-2"), row("session_name", "s-gc-2"), false},
		{"fresh heal, another cause", intentRowHealFresh, -time.Second, causeRuntimePresent, row("session_name", "s-gc-1"), row("session_name", "s-gc-1"), false},
	} {
		s := &sim{round: []simResult{{it: intent{Kind: c.kind, Key: k}, res: settlement{Outcome: settledRefused, Cause: c.cause}}}}
		s.sp = &simProvider{rts: map[string]*simRuntime{"s-gc-1": {id: "gc-1", probeErr: true}, "s-gc-2": {id: "gc-1"}}}
		backoff := map[string]backoffRecord{rowBackoffKey(k): {Consecutive: 2, Until: at.Add(c.until), Cause: c.cause}}
		if c.until == 0 {
			backoff = nil
		}
		if got := s.pacedRetries(backoff, at, c.before, c.after); got != c.paced {
			t.Errorf("%s: paced %t, want %t", c.name, got, c.paced)
		}
	}
}

// signaledSim is a sim with one signaled row, gc-1, whose own runtime is
// live (the #7648 review's directed scenarios).
func signaledSim(t *testing.T) *sim {
	return newSim(t, 3, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		s.sp.put("s-gc-1", "gc-1", "1", "tok-gc-1-1")
		return []beads.Bead{poolRow("gc-1", "worker", 1, string(session.StateDraining), "instance_token", "tok-gc-1-1", "state_reason", session.DrainAckStopPendingReason)}, nil
	}})
}

func (s *sim) setProbeErr(name string, v bool) {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	s.sp.rts[name].probeErr = v
	s.sp.touch(name)
}

// TestSimProbeErrorHoldsTheStopUntilItRecovers (v5.9n, I17 and I10).
// Kills a stop verb that never stops a runtime whose probe once errored: the
// probe errors through a stop attempt, then recovers, and by quiescence the
// row is stopped and finalized. A probe still erroring through quiescence
// holds the stop, and I17 excuses it; so does I10 its paced liveness-unknown
// re-issues, whether quiescence starts at once or after a stop attempt and a
// patrol (the shape whose late re-issue v5.9n's amendment rules paced).
func TestSimProbeErrorHoldsTheStopUntilItRecovers(t *testing.T) {
	for _, c := range []struct {
		name              string
		attempt, recovers bool
	}{
		{"recovers after a stop attempt", true, true},
		{"still erroring", false, false},
		{"still erroring after a stop attempt", true, false},
	} {
		s := signaledSim(t)
		s.setProbeErr("s-gc-1", true)
		s.inventory()
		if c.attempt {
			s.pass()
			for len(s.parked) > 0 {
				s.release(0)
			}
			s.audit("v2")
			s.advance(simPatrol)
		}
		if c.recovers {
			s.setProbeErr("s-gc-1", false)
		}
		s.quiesce()
		b, _ := s.legs[0].backing.Get("gc-1")
		switch {
		case len(s.failures) > 0:
			t.Errorf("%s: violated %q", c.name, s.failures)
		case c.recovers && signaledRow(b):
			t.Errorf("%s: gc-1 still signaled at quiescence: %v", c.name, b.Metadata)
		case !c.recovers && !signaledRow(b):
			t.Errorf("%s: gc-1 finalized over a runtime no read proved stopped: %v", c.name, b.Metadata)
		}
	}
}

// TestSimExcusedRefusalIsFinalized (v5.9n, I16). Kills an excused premise
// refusal left signaled: an outside quarantine after the stop's row read
// refuses its after-stop finalize (excused), and by quiescence the next pass
// has finalized the row, which checkStranded holds it to.
func TestSimExcusedRefusalIsFinalized(t *testing.T) {
	s := signaledSim(t)
	s.inventory()
	s.pass()
	if len(s.parked) != 1 || s.parked[0].it.Kind != intentStop {
		t.Fatalf("parked %v, want one stop", s.parked)
	}
	l, e := s.legs[0], s.parked[0]
	e.seam = seamAfterRowRead
	e.outside = func() {
		b, _ := l.backing.Get("gc-1")
		s.outside("quarantine inside stop", l, b, func(l *simLeg, b beads.Bead) {
			s.setMeta(l, b.ID, "quarantined_until", s.rel(30*time.Second), "sleep_reason", "quarantine")
		})
	}
	s.release(0)
	s.audit("v2")
	s.quiesce()
	if b, _ := l.backing.Get("gc-1"); len(s.failures) > 0 || signaledRow(b) {
		t.Fatalf("violated %q; gc-1 %v: want it finalized by quiescence", s.failures, b.Metadata)
	}
}

// TestSimCloseBeforeQuiescenceExcusesNoKill (v5.9n; #7648 review round 2).
// Kills a quiescence that inherits the last step's outside closes: a stop
// parked before a close in the step before quiescence is released by
// quiescence, a step of its own, where the close excuses no kill; the stop,
// re-reading the closed row, kills nothing, and nothing is violated.
func TestSimCloseBeforeQuiescenceExcusesNoKill(t *testing.T) {
	s := signaledSim(t)
	s.inventory()
	s.pass()
	s.audit("v2")
	if len(s.parked) != 1 || s.parked[0].it.Kind != intentStop {
		t.Fatalf("parked %v, want one stop", s.parked)
	}
	l := s.legs[0]
	s.beginStep()
	b, _ := l.backing.Get("gc-1")
	s.outside("close", l, b, func(l *simLeg, b beads.Bead) { _ = l.backing.Close(b.ID) })
	s.quiesce()
	if len(s.closedInStep) != 0 || len(s.failures) > 0 {
		t.Fatalf("closedInStep %v, violated %q: want quiescence its own step, clean", s.closedInStep, s.failures)
	}
	for _, k := range s.sp.kills {
		if !k.External && k.Hit != nil {
			t.Fatalf("v2 %s on %s: a stop of a closed row", k.Method, k.Name)
		}
	}
}

// TestSimStepClearsItsCloses (v5.9n, I4). Kills a step that keeps an
// earlier step's outside closes: whatever the step does, a close recorded
// before it is gone.
func TestSimStepClearsItsCloses(t *testing.T) {
	s := signaledSim(t)
	s.closedInStep = map[string]beads.Bead{"s-unknown": {ID: "unknown"}}
	s.step()
	if _, ok := s.closedInStep["s-unknown"]; ok {
		t.Fatal("a step kept an earlier step's close")
	}
}

// TestSimQuiesceRoundsCountPerRow (v5.9n amended, I10). Kills a round
// counter that misses a loop or adds correct rows up: nine paced rounds of
// one row fail, quiet rounds between them or not; a paced round then two
// quiet ones ends quiescence; two rows paced eight times each never fail.
func TestSimQuiesceRoundsCountPerRow(t *testing.T) {
	a, b := rowKey{Leg: rowLeg, ID: "gc-1"}, rowKey{Leg: rowLeg, ID: "gc-2"}
	run := func(steps string) (done, loop bool, at int) {
		var q quiesceRounds
		for i, c := range steps {
			switch c {
			case 'a', 'b':
				k := map[rune]rowKey{'a': a, 'b': b}[c]
				if _, l := q.paced([]rowKey{k}); l {
					return false, true, i
				}
			case 'q':
				if q.quiet() {
					return true, false, i
				}
			}
		}
		return false, false, len(steps)
	}
	for _, c := range []struct {
		name, steps string
		done, loop  bool
		at          int
	}{
		{"nine paced rounds", "aaaaaaaaa", false, true, 8},
		{"paced and quiet alternating", "aqaqaqaqaqaqaqaqa", false, true, 16},
		{"paced, then two quiet", "aqq", true, false, 2},
		{"two rows, eight each", "abqabqabqabqabqabqabqab", false, false, 23},
	} {
		done, loop, at := run(c.steps)
		if done != c.done || loop != c.loop || at != c.at {
			t.Errorf("%s: done %t loop %t at %d, want done %t loop %t at %d", c.name, done, loop, at, c.done, c.loop, c.at)
		}
	}
}

// Kills I4's closed-row model judging a lost commit's stop on the
// step-start snapshot, or excusing a real wrong-token stop (v5.9n amended):
// a start mints its PreWake token, an outside close lands in the same step,
// and the stop of the runtime carrying the minted token passes I4, while a
// stop of one carrying the row's step-start token still fails it.
func TestSimI4ClosedRowAsTheCloseFoundIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		token string
		fails bool
	}{
		{"the runtime carrying the minted token", "tok-new", false},
		{"a runtime carrying the step-start token", "tok-gc-1-1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSim(t, 3, simOpts{rows: func(*sim) ([]beads.Bead, []beads.Bead) {
				return []beads.Bead{poolRow("gc-1", "worker", 1, "creating", "instance_token", "tok-gc-1-1")}, nil
			}})
			s.advance(time.Second)
			s.beginStep()
			l := s.legs[0]
			if err := l.backing.SetMetadataBatch("gc-1", map[string]string{"instance_token": "tok-new", "generation": "2"}); err != nil { // the PreWake, not yet audited
				t.Fatal(err)
			}
			b, err := l.backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			s.outside("close", l, b, func(l *simLeg, b beads.Bead) { _ = l.backing.Close(b.ID) })
			s.failures = nil
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: b.Metadata["session_name"], Hit: &simRuntime{id: "gc-1", epoch: "2", token: c.token}})
			s.check()
			if got := slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "I4 ") }); got != c.fails {
				t.Fatalf("failures %q, want an I4 violation %v", s.failures, c.fails)
			}
		})
	}
}

// Kills I16's live-pane test missing an ownerless runtime (shifted seed 824):
// under a name the row does not own, its own runtime and an ownerless one
// (no identity, which C8.3 defers on, as livePane counts it) are live panes
// the stop may not finalize over; another row's (Foreign) is not.
func TestNamedPaneCountsAnOwnerlessRuntime(t *testing.T) {
	b := poolRow("gc-1", "worker", 1, "draining", "session_name", "s-worker", "pool_managed", "")
	for _, c := range []struct {
		name string
		id   string
		want bool
	}{
		{"its own runtime", "gc-1", true},
		{"an ownerless runtime", "", true},
		{"another row's runtime", "gc-9", false},
	} {
		s := scripted(t, nil, b)
		s.sp.mu.Lock()
		s.sp.put("s-worker", c.id, "1", "tok")
		s.sp.mu.Unlock()
		if got := s.namedPane(b); got != c.want {
			t.Errorf("%s: namedPane %t, want %t", c.name, got, c.want)
		}
	}
}

// TestSimI23CatchesAWrongGoneNote (the #7799 review's P1). Kills an I23 that
// excuses any effect's note: a gone note over a zombie the latest pass
// listed, unchanged since, makes the census read it gone, which I23
// reports. (Ties of a note and a pass go to the pass in Note itself, so I23
// needs no exemption for them.)
func TestSimI23CatchesAWrongGoneNote(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1", "last_woke_at", rowAt(-time.Hour),
		"quarantined_until", rowAt(time.Hour), "sleep_reason", "quarantine"))
	s.sp.mu.Lock()
	s.sp.rts["s-gc-1"].zombie = true
	s.sp.touch("s-gc-1")
	s.sp.mu.Unlock()
	s.inventory()
	s.advance(time.Second)
	now := s.clk.Now()
	s.p.applyFacts(effectFacts{Noted: &notedRuntime{Name: "s-gc-1", At: now, Gone: true}}, now)
	s.failures = nil
	s.check()
	if !slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, "I23 ") }) {
		t.Errorf("a gone note over a listed, unchanged zombie: violations %q, want I23", s.failures)
	}
}

// TestSimI4JustClosedRollbackVerdict (v5.10e (3); the #7799 review's s5):
// a just-closed row's rollback kill of its runtime at another generation is
// judged by the just-closed rule, not passed on to the generic token rule:
// only the row's own generation is excused. Kills the excuse widened to
// every rollback (which the generic rule happens to catch for this kill).
func TestSimI4JustClosedRollbackVerdict(t *testing.T) {
	x := sessionRow("x", "session_name", "s-x", "generation", "2", "instance_token", "tok-2")
	x.Status = "open"
	for _, c := range []struct {
		epoch string
		want  bool // a just-closed I4 verdict
	}{{"1", true}, {"2", false}} {
		s := newSim(t, 3, simOpts{})
		s.prev, s.closedInStep = map[string]beads.Bead{}, map[string]beads.Bead{"s-x": x}
		s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Effect: intentRollback, Hit: &simRuntime{id: "x", epoch: c.epoch, token: "tok-stale"}})
		s.failures = nil
		s.checkDestructive()
		got := slices.ContainsFunc(s.failures, func(f string) bool { return strings.Contains(f, "not the just-closed row x's") })
		if got != c.want {
			t.Errorf("rollback kill at generation %s: failures %q, want a just-closed verdict %t", c.epoch, s.failures, c.want)
		}
	}
}
