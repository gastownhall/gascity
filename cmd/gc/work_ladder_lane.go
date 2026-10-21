package main

// The work-progress ladder's v2 lane step (PHASE1-2-EPOCH-LIVENESS-LADDER §6,
// slice W3). It replaces the three K1 backstops (the idle-claim, continuation
// and execution nudges) with one ladder per pool seat, keyed by (claim,
// generation), whose pacing never reaches zero: grace, then a doubling
// backoff to an hourly nudge, forever (internal/workladder).
//
// Per seat and pass: observe (the planner's last allocation, the row, the
// runtime, provider health), decide (workladder.Decide, pure), re-read live
// the work a nudge or a recycle would act on, commit the work_ladder cell by
// a CAS on the row that decides again on the fresh read (UpdateRowFenced),
// and only then act: nudge through today's delivery call, post a recycle to
// the planner's inbox (arm A16), or release an unstarted claim. Every write
// of the cell is a CAS (WL6); the old pacing keys are neither read nor
// written (§6.7), so a deploy starts every seat from a fresh grace and no
// drain wave is possible.
//
// The agent's work_started cell (W1, which this step requires: it is built
// on it) is the proof a claim is started. For claims handed out before W1,
// the seeds stand in, read when the ladder first sees a generation:
// execution_claim_started, current_claim_bead_id,
// currently_processing_bead_id, and the work bead's own gc.session_id, which
// the hook stamps at every hand-over. A release must re-prove at its CAS that
// neither the cell nor a seed names the claim, and its live read that the
// bead's gc.session_id does not name the seat (§6.5); the agent's start
// write refuses a claim the release names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// workLadderCommitAttempts bounds a commit's CAS retries against the agent's
// and the planner's writes to the same row.
const workLadderCommitAttempts = 4

// The nudge texts (§6.6). A started claim's is built in and never names
// --drain-ack, whatever the pack configures.
const (
	ladderClaimedNudge = "Run gc hook --claim --json now; it returns the work assigned to you. Execute it."
	ladderStartedNudge = "You hold %s and its step has started. Continue it now (run gc hook --claim --json to re-read your assignment). If you are waiting on something, say what, then stop."
)

// decideWorkLadder is one seat's ladder decision under OD7's policy. It is
// pure (WL7): the v2purity analyzer holds it, and workladder.Decide under it,
// to no clock, store or provider.
//
//gc:pure
func decideWorkLadder(prev workladder.Record, obs workladder.Observation) workladder.Decision {
	return workladder.Decide(prev, obs, workladder.DefaultPolicy())
}

// ladderTarget is one piece of a seat's outstanding work with what its live
// re-read needs.
type ladderTarget struct {
	work workladder.Work
	// claimSession: the snapshot's bead carries the seat as its
	// gc.session_id, the hook's hand-over stamp (a seed).
	claimSession bool
	// recheck re-reads the work live before the ladder acts on it: clear
	// when it is gone, hold when the read cannot tell.
	recheck func() backstopResolution
}

// ladderPass is one run of the step.
type ladderPass struct {
	l      *externalReadsLane
	env    externalReadsEnv
	cfg    *config.City
	now    time.Time
	front  *session.Store
	health *providerHealthSnapshot
	// partial: the allocation's snapshot is incomplete, so work a seat's
	// ladder holds but the snapshot lacks is carried, re-read live before
	// anything is done about it.
	partial bool
}

// runWorkLadder is the step's body over the planner's last allocation s.
func (l *externalReadsLane) runWorkLadder(ctx context.Context, env externalReadsEnv, s *allocSummary) {
	raw, err := loadSessionBeads(env.CityStore)
	if err != nil {
		fmt.Fprintf(l.stderr, "work-ladder: loading sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	p := &ladderPass{
		l: l, env: env, cfg: env.Cfg, now: l.now(), front: sessionFrontDoor(env.CityStore),
		health: loadProviderHealthSnapshot(env.CityPath), partial: s.Partial,
	}
	work := p.outstanding(s, raw)
	for _, row := range raw {
		if ctx.Err() != nil {
			return
		}
		if !ladderGoverns(row) {
			continue
		}
		p.seat(row, work[row.ID])
	}
}

// ladderGoverns is the seats the ladder paces: open pool-managed session
// rows with a runtime name, as the backstops governed.
func ladderGoverns(row beads.Bead) bool {
	return !strings.EqualFold(strings.TrimSpace(row.Status), "closed") && isSessionBead(row) &&
		strings.TrimSpace(row.Metadata["pool_managed"]) == "true" && strings.TrimSpace(row.Metadata["session_name"]) != ""
}

// clearLadderReleases drops the pending release from every governed row's
// cell, by CAS, when the ladder is turned off: nothing will finish it, and
// the hook refuses to hand out a claim a release names.
func (l *externalReadsLane) clearLadderReleases(store beads.Store, raw []beads.Bead) {
	front := sessionFrontDoor(store)
	for _, row := range raw {
		if !ladderGoverns(row) {
			continue
		}
		if r, err := workladder.DecodeLadder(row.Metadata[session.WorkLadderKey]); err != nil || r.Release == nil {
			continue
		}
		_, err := front.UpdateRowFenced(row.ID, workLadderCommitAttempts, func(_ session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
			r, err := workladder.DecodeLadder(resp.Metadata[session.WorkLadderKey])
			if err != nil || r.Release == nil {
				return session.RowPatch{}, false
			}
			r.Release = nil
			return session.RowPatch{Metadata: session.MetadataPatch{session.WorkLadderKey: workladder.EncodeLadder(r)}}, true
		})
		if err != nil {
			fmt.Fprintf(l.stderr, "work-ladder: clearing %s's pending release: %v\n", row.ID, err) //nolint:errcheck // best-effort stderr
		}
	}
}

// outstanding is each seat's work (§6.2), by session ID, from today's three
// predicates: its own trigger, open and unclaimed; exactly one ready
// continuation pinned to it; and the in_progress claims it holds under a
// current identity. Work on a dispatch hold (hold:mayor, hold:external) is
// not the ladder's (owner ruling 2026-10-18): it stays with its agent,
// unnudged, until the hold lifts, and then the ladder takes it up afresh.
func (p *ladderPass) outstanding(s *allocSummary, raw []beads.Bead) map[string][]ladderTarget {
	out := make(map[string][]ladderTarget)
	held := make(map[string]bool)
	for _, b := range slices.Concat(s.AssignedWork, s.ReadyRouted) {
		if beadmeta.HasDispatchHold(b.Labels) {
			held[b.ID] = true
		}
	}
	claimWork := slices.Concat(s.AssignedWork, s.ReadyRouted)
	refs := make([]string, len(claimWork))
	copy(refs, s.AssignedStoreRefs)
	copy(refs[len(s.AssignedWork):], s.ReadyRoutedRefs)
	triggers := newIdleClaimWorkSnapshot(claimWork, refs)
	claims := newExecutionClaimSnapshot(s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs)
	var conts poolContinuationCandidateSnapshot
	if !s.Partial {
		candidates, partial := selectReadyContinuationClaimCandidates(p.env.CityName, s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs, s.ReadyAssigned)
		if !partial {
			conts = newPoolContinuationCandidateSnapshot(raw, candidates)
		}
	}
	for _, row := range raw {
		if !ladderGoverns(row) {
			continue
		}
		var ts []ladderTarget
		if key, ok := p.trigger(triggers, row); ok {
			ts = append(ts, ladderTarget{work: workladder.Work{Key: key, Kind: workladder.KindTrigger}, recheck: p.triggerRecheck(key.Bead)})
		}
		if cs := conts.bySessionID[row.ID]; len(cs) == 1 && !conts.holdBySessionID[row.ID] && !held[cs[0].WorkBeadID] {
			c := cs[0]
			target := backstopTarget{ID: c.WorkBeadID, RootID: c.RootBeadID, StoreRef: c.StoreRef, Assignee: c.Assignee, Store: c.Store}
			ts = append(ts, ladderTarget{
				work:    workladder.Work{Key: workladder.ClaimKey{Store: claimStoreRef(c.StoreRef), Bead: c.WorkBeadID}, Kind: workladder.KindContinuation},
				recheck: func() backstopResolution { return unlessHeld(revalidateContinuation(target)) },
			})
		}
		for _, c := range claims.forIdentities(currentSessionAssigneeIdentities(row)) {
			if held[c.BeadID] {
				continue
			}
			target := backstopTarget{ID: c.BeadID, Assignee: c.Assignee, Store: c.Store}
			ts = append(ts, ladderTarget{
				// c.StoreRef is claimStoreRef's: the claim's residence, as the
				// hook's work_started entry and the backstop key it.
				work:         workladder.Work{Key: workladder.ClaimKey{Store: c.StoreRef, Bead: c.BeadID}, Kind: workladder.KindClaim},
				claimSession: c.SessionID != "" && c.SessionID == strings.TrimSpace(row.ID),
				recheck:      func() backstopResolution { return unlessHeld(revalidateExecutionClaim(target)) },
			})
		}
		if len(ts) > 0 {
			out[row.ID] = ts
		}
	}
	return out
}

// trigger is row's own trigger bead when the snapshot holds it open and
// unclaimed (isUnclaimedTrigger), keyed by the store the snapshot found it in.
func (p *ladderPass) trigger(snap idleClaimWorkSnapshot, row beads.Bead) (workladder.ClaimKey, bool) {
	id := strings.TrimSpace(row.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	ref := strings.TrimSpace(row.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey])
	if id == "" {
		return workladder.ClaimKey{}, false
	}
	var key storeScopedBeadKey
	if ref != "" {
		key = storeScopedBeadKey{StoreRef: normalizeIdleClaimStoreRef(ref), ID: id}
	} else if keys := snap.byID[id]; len(keys) == 1 {
		key = keys[0]
	} else {
		return workladder.ClaimKey{}, false
	}
	w, ok := snap.byScope[key]
	if !ok || !isUnclaimedTrigger(p.cfg, w) || beadmeta.HasDispatchHold(w.Labels) {
		return workladder.ClaimKey{}, false
	}
	return workladder.ClaimKey{Store: claimStoreRef(key.StoreRef), Bead: id}, true
}

// triggerRecheck re-reads a trigger live through the store that owns it,
// before a nudge or a recycle (the claim backstop's drain gate): still open
// and unclaimed, or nothing is done about it.
func (p *ladderPass) triggerRecheck(id string) func() backstopResolution {
	return func() backstopResolution {
		b, found, err := p.env.WorkLegs.readLive(id)
		switch {
		case err != nil:
			return backstopResolutionHold
		case !found || !isUnclaimedTrigger(p.cfg, b) || beadmeta.HasDispatchHold(b.Labels):
			return backstopResolutionClear
		}
		return backstopResolutionOutstanding
	}
}

// unlessHeld is res, a live re-read's verdict, except that work a dispatch
// hold parks (b, the bead that read found) is clear: not the ladder's while
// the hold stands.
func unlessHeld(res backstopResolution, b beads.Bead) backstopResolution {
	if res == backstopResolutionOutstanding && beadmeta.HasDispatchHold(b.Labels) {
		return backstopResolutionClear
	}
	return res
}

// carriedRecheck re-reads, live through the store that owns it, work the
// seat's ladder carries under an incomplete snapshot: still what its kind
// says (a trigger open and unclaimed; a continuation open and pinned to the
// seat; a claim in_progress under the seat), or nothing is done about it.
func (p *ladderPass) carriedRecheck(row beads.Bead, w workladder.Work) func() backstopResolution {
	if w.Kind == workladder.KindTrigger {
		return p.triggerRecheck(w.Key.Bead)
	}
	return func() backstopResolution {
		b, found, err := p.env.WorkLegs.readLive(w.Key.Bead)
		switch {
		case err != nil:
			return backstopResolutionHold
		case !found || !slices.Contains(currentSessionAssigneeIdentities(row), strings.TrimSpace(b.Assignee)) || beadmeta.HasDispatchHold(b.Labels):
			return backstopResolutionClear
		}
		status := "in_progress"
		if w.Kind == workladder.KindContinuation {
			status = "open"
		}
		if !strings.EqualFold(strings.TrimSpace(b.Status), status) {
			return backstopResolutionClear
		}
		return backstopResolutionOutstanding
	}
}

// seat runs the ladder for one seat: observe, decide, re-read the work it
// would act on, commit by CAS, then act on what landed.
func (p *ladderPass) seat(row beads.Bead, targets []ladderTarget) {
	prev, issue := decodeLadderCell(row.Metadata)
	if issue != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: %s: %v; starting its ladder afresh\n", row.ID, issue) //nolint:errcheck // best-effort stderr
	}
	if len(targets) == 0 && len(prev.Entries) == 0 && prev.Release == nil {
		return // nothing outstanding, nothing to settle: no reads, no write
	}
	if p.partial {
		targets = p.carryLadderWork(row, targets, prev)
	}
	info, live := sessionInfoFromBead(row), p.observeRuntime(row)
	hold := map[workladder.ClaimKey]bool{}
	var d workladder.Decision
	for range len(targets) + 1 { // each round settles a re-read: work gone is dropped, work unreadable waits
		d = decideWorkLadder(prev, p.observe(info, row.Metadata, prev, targets, hold, live))
		gone, held := recheckActed(d, targets, hold)
		if len(gone) == 0 && len(held) == 0 {
			break
		}
		targets = slices.DeleteFunc(targets, func(t ladderTarget) bool { return gone[t.work.Key] })
		for k := range held {
			hold[k] = true
		}
	}
	if !d.Changed && len(d.Actions) == 0 {
		return
	}
	landed, ok := p.commit(row.ID, targets, hold, live)
	if !ok {
		return
	}
	p.act(row, landed)
}

// carryLadderWork adds to targets, under an incomplete snapshot, every work
// the seat's ladder holds that the snapshot lacks, so a store that did not
// answer neither drops a ladder nor resets its count. Carried work is re-read
// live (carriedRecheck) before any nudge or recycle about it.
func (p *ladderPass) carryLadderWork(row beads.Bead, targets []ladderTarget, prev workladder.Record) []ladderTarget {
	for _, e := range prev.Entries {
		if !slices.ContainsFunc(targets, func(t ladderTarget) bool { return t.work.Key == e.Key }) {
			w := workladder.Work{Key: e.Key, Kind: e.Kind}
			targets = append(targets, ladderTarget{work: w, recheck: p.carriedRecheck(row, w)})
		}
	}
	return targets
}

// recheckActed re-reads live the work d nudges or recycles: the keys proved
// gone, and those whose read could not tell (each waits, alone, this pass).
func recheckActed(d workladder.Decision, targets []ladderTarget, hold map[workladder.ClaimKey]bool) (gone, held map[workladder.ClaimKey]bool) {
	for _, a := range d.Actions {
		var k workladder.ClaimKey
		switch a := a.(type) {
		case workladder.Nudge:
			k = a.Key
		case workladder.Recycle:
			k = a.Key
		default:
			continue
		}
		i := slices.IndexFunc(targets, func(t ladderTarget) bool { return t.work.Key == k })
		if i < 0 || targets[i].recheck == nil || hold[k] {
			continue
		}
		switch targets[i].recheck() {
		case backstopResolutionHold:
			if held == nil {
				held = make(map[workladder.ClaimKey]bool)
			}
			held[k] = true
		case backstopResolutionClear:
			if gone == nil {
				gone = make(map[workladder.ClaimKey]bool)
			}
			gone[k] = true
		}
	}
	return gone, held
}

// decodeLadderCell is meta's work_ladder cell; an unreadable one reads as
// absent (the ladder re-seeds it), with the issue.
func decodeLadderCell(meta map[string]string) (workladder.Record, error) {
	r, err := workladder.DecodeLadder(meta[session.WorkLadderKey])
	if err != nil {
		return workladder.Record{}, err
	}
	return r, nil
}

// ladderRuntime is what the pass reads of a seat's runtime, once.
type ladderRuntime struct {
	running  workladder.Tri
	quiet    workladder.Quiet
	activity time.Time
	provider workladder.ProviderState
}

// observeRuntime reads row's runtime: running, quiet (sessionQuietHold's
// rule: activity older than the grace and, for a busy-readable provider, no
// busy indicator; an unreadable clock is not quiet), and its provider's
// health. Quiet is read only for a running runtime.
func (p *ladderPass) observeRuntime(row beads.Bead) ladderRuntime {
	var rt ladderRuntime
	name := strings.TrimSpace(row.Metadata["session_name"])
	if healthy, present := p.health.check(strings.TrimSpace(row.Metadata["provider"])); present && !healthy {
		rt.provider = workladder.ProviderState{Degraded: true, Reason: "provider-health"}
	}
	sp := p.env.SP
	if sp == nil {
		return rt
	}
	if !sp.IsRunning(name) {
		rt.running = workladder.TriFalse
		return rt
	}
	rt.running = workladder.TriTrue
	last, err := sp.GetLastActivity(name)
	if err == nil {
		rt.activity = last
	}
	hold := executionHoldNotQuiet
	if err == nil && !last.IsZero() && p.now.Sub(last) >= idleClaimNudgeGrace {
		hold = ""
		if executionSnapshotVetoApplies(row.Metadata) {
			hold = snapshotHoldFor(sp, name)
		}
	}
	if hold == "" {
		rt.quiet = workladder.Quiet{State: workladder.Quiescent}
	} else {
		rt.quiet = workladder.Quiet{State: workladder.NotQuiet, Reason: hold}
	}
	return rt
}

// observe is the seat's observation from its row (info, and its metadata
// meta), its cell prev, its work, the work waiting on a live read, and its
// runtime.
func (p *ladderPass) observe(info session.Info, meta map[string]string, prev workladder.Record, targets []ladderTarget, hold map[workladder.ClaimKey]bool, rt ladderRuntime) workladder.Observation {
	gen := rowGeneration(meta)
	obs := workladder.Observation{
		Now: p.now, Gen: gen, Runtime: rt.running, Quiet: rt.quiet, LastActivity: rt.activity, Provider: rt.provider,
		Gate: ladderGate(info, meta, p.now), Hold: hold,
	}
	for _, t := range targets {
		obs.Outstanding = append(obs.Outstanding, t.work)
	}
	obs.Started, obs.StartedRead = ladderStarted(meta, prev, gen, targets)
	return obs
}

// ladderStarted is the start evidence for each outstanding claim: the agent's
// work_started cell, and the seeds (§6.7) when the ladder cell is absent or
// names another generation. read is false when the cell is unreadable, and
// then every claim reads as started.
func ladderStarted(meta map[string]string, prev workladder.Record, gen int64, targets []ladderTarget) (map[workladder.ClaimKey]workladder.StartSource, bool) {
	cell, err := workladder.DecodeStarted(meta[session.WorkStartedKey])
	seeding := prev.V == 0 || prev.Gen != gen
	started := make(map[workladder.ClaimKey]workladder.StartSource)
	for _, t := range targets {
		if t.work.Kind != workladder.KindClaim {
			continue
		}
		switch {
		case err == nil && cell.Names(t.work.Key, gen):
			started[t.work.Key] = workladder.SrcAgent
		case seeding:
			if src, ok := ladderSeed(meta, t); ok {
				started[t.work.Key] = src
			}
		}
	}
	return started, err == nil
}

// ladderSeed is the legacy evidence that names t's bead started on this row:
// the execution backstop's stamp for the row's incarnation, the hook's
// current-claim stamp, the currently processing bead, or the bead's own
// gc.session_id naming the seat.
func ladderSeed(meta map[string]string, t ladderTarget) (workladder.StartSource, bool) {
	bead := strings.TrimSpace(t.work.Key.Bead)
	switch {
	case bead == "":
		return 0, false
	case strings.TrimSpace(meta[executionClaimStartedKey]) == strings.TrimSpace(meta["instance_token"])+"|"+bead:
		return workladder.SrcExecutionClaimStarted, true
	case strings.TrimSpace(meta[beadmeta.CurrentClaimBeadIDMetadataKey]) == bead:
		return workladder.SrcCurrentClaim, true
	case strings.TrimSpace(meta[session.CurrentBeadIDKey]) == bead:
		return workladder.SrcCurrentlyProcessing, true
	case t.claimSession:
		return workladder.SrcClaimSession, true
	}
	return 0, false
}

// ladderGate is open, or held by what owns the seat for now: a stop request
// (a drain, at the row's generation), a kill, a start in flight, a restart
// request, an operator's suspend or an agent's wait, a quarantine, or an
// agent's heartbeat (WL5). Every hold is one the row clears when it ends, so
// none can silence a seat for good.
func ladderGate(info session.Info, meta map[string]string, now time.Time) workladder.Gate {
	held := func(reason string) workladder.Gate { return workladder.Gate{Held: true, Reason: reason} }
	state := session.State(strings.TrimSpace(info.MetadataState))
	switch {
	case stopRequestStands(info, meta) || state == session.StateDraining:
		return held("stop_requested")
	case session.IsKillPendingInfo(info, now):
		return held("kill_pending")
	case state == session.StateCreating || state == session.StateStartPending ||
		info.PendingCreateClaim && state != session.StateActive && state != session.StateAwake: // the census's PendingCreate
		return held("starting")
	case strings.TrimSpace(info.RestartRequested) != "":
		return held("restart_pending")
	}
	d := session.DecodeDisposition(meta)
	switch {
	case d.HeartbeatHeld(now):
		return held("heartbeat")
	case d.Wait || d.WaitIntent:
		return held("agent_wait")
	case d.Operator != session.OperatorNone: // a stale sleep_reason on a running row is no hold
		return held("operator_hold")
	case d.Quarantine.After(now):
		return held("quarantined")
	}
	return workladder.Gate{}
}

// commit writes the seat's cell by a CAS that decides again on the fresh
// row: the agent's start write, the planner's, or another pass's may have
// landed since the read. A release it writes is re-proved there (§6.5): the
// row open, the agent cell readable and not naming the claim, and no seed
// naming it; a seed that names it makes the claim started instead. It
// returns the decision that landed (or, writing nothing, the fresh one).
func (p *ladderPass) commit(id string, targets []ladderTarget, hold map[workladder.ClaimKey]bool, rt ladderRuntime) (workladder.Decision, bool) {
	var landed workladder.Decision
	wrote, err := p.front.UpdateRowFenced(id, workLadderCommitAttempts, func(fresh session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
		landed = workladder.Decision{}
		if fresh.Closed {
			return session.RowPatch{}, false
		}
		prev, _ := decodeLadderCell(resp.Metadata)
		obs := p.observe(fresh, resp.Metadata, prev, targets, hold, rt)
		d := decideWorkLadder(prev, obs)
		for range len(targets) + 1 {
			k, ok := newRelease(prev, d)
			if !ok {
				break
			}
			i := slices.IndexFunc(targets, func(t ladderTarget) bool { return t.work.Key == k })
			if i < 0 {
				break
			}
			// The seeds read off resp are this revision's, but claimSession
			// (the bead's gc.session_id naming the seat) is the planner
			// snapshot's: this CAS is on the session row and cannot re-read
			// the bead. A release that lands here on a claim the hook stamped
			// after the snapshot is caught by release(), which re-reads the
			// bead live before any write to it and keeps a bead whose
			// gc.session_id names the seat, its write fenced on that read.
			// That live read is the last guard for such a claim: keep it.
			src, seeded := ladderSeed(resp.Metadata, targets[i])
			if !seeded {
				break // the CAS proves it: unstarted on this revision
			}
			obs.Started[k] = src // started after all: decide it so
			d = decideWorkLadder(prev, obs)
		}
		landed = d
		if !d.Changed {
			return session.RowPatch{}, false
		}
		return session.RowPatch{Metadata: session.MetadataPatch{session.WorkLadderKey: workladder.EncodeLadder(d.Next)}}, true
	})
	switch {
	case err != nil:
		fmt.Fprintf(p.l.stderr, "work-ladder: committing %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
		return workladder.Decision{}, false
	case !wrote && landed.Changed:
		fmt.Fprintf(p.l.stderr, "work-ladder: committing %s: the row moved on every attempt\n", id) //nolint:errcheck // best-effort stderr
		return workladder.Decision{}, false
	}
	return landed, true
}

// newRelease is the release d writes that prev did not hold.
func newRelease(prev workladder.Record, d workladder.Decision) (workladder.ClaimKey, bool) {
	r := d.Next.Release
	if r == nil || (prev.Release != nil && *prev.Release == *r) {
		return workladder.ClaimKey{}, false
	}
	return r.Key, true
}

// act performs what landed: the releases first (a claim given back needs no
// nudge), then the nudge, the recycle and the alerts.
func (p *ladderPass) act(row beads.Bead, d workladder.Decision) {
	gen := d.Next.Gen
	released := map[workladder.ClaimKey]bool{}
	for _, a := range d.Actions {
		if r, ok := a.(workladder.Release); ok {
			at := gen // a release re-emitted from an earlier generation settles at its own
			if m := d.Next.Release; m != nil && m.Key == r.Key {
				at = m.Gen
			}
			released[r.Key] = p.release(row, r.Key, at)
		}
	}
	recycled := false
	for _, a := range d.Actions {
		switch a := a.(type) {
		case workladder.Nudge:
			if !released[a.Key] {
				p.nudge(row, a, gen, refusedOf(d.Next, a.Key))
			}
		case workladder.Recycle:
			p.event(events.WorkRecycleRequested, row, a.Key, gen, workLadderEventFields{Reason: recycleReason(a)})
			if !recycled {
				recycled = true
				p.recycle(row, gen)
			}
		case workladder.Alert:
			fmt.Fprintf(p.l.stderr, "work-ladder: %s: %s on %s\n", row.ID, a.Reason, a.Key.Bead) //nolint:errcheck // best-effort stderr
			p.event(events.WorkEscalated, row, a.Key, gen, workLadderEventFields{Reason: a.Reason})
		}
	}
}

// refusedOf is the refusals r records for key's entry.
func refusedOf(r workladder.Record, key workladder.ClaimKey) int {
	if i := slices.IndexFunc(r.Entries, func(e workladder.Entry) bool { return e.Key == key }); i >= 0 {
		return r.Entries[i].Refused
	}
	return 0
}

func recycleReason(r workladder.Recycle) string {
	if r.KeepClaim {
		return "started_unanswered"
	}
	return "trigger_unclaimed"
}

// nudge delivers n (§6.6) through today's call, after its attempt landed
// (write-ahead). A delivery error counts the attempt, but one that says the
// runtime is unavailable, or that the seat is at a prompt only a human can
// answer (a permission prompt), is re-counted as paused: neither is the
// agent ignoring its work.
func (p *ladderPass) nudge(row beads.Bead, n workladder.Nudge, gen int64, refused int) {
	text := p.nudgeText(row, n)
	fields := workLadderEventFields{Kind: n.Kind.String(), Rung: n.Rung.String(), Attempt: n.Attempt, Paused: n.Paused}
	err := ladderDeliver(p.env.SP, strings.TrimSpace(row.Metadata["session_name"]), text)
	if err != nil {
		fields.Reason = err.Error()
		fmt.Fprintf(p.l.stderr, "work-ladder: nudging %s about %s failed: %v\n", row.ID, n.Key.Bead, err) //nolint:errcheck // best-effort stderr
		p.event(events.WorkNudgeFailed, row, n.Key, gen, fields)
		if (errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrPendingInteraction)) && !n.Paused && p.refused(row.ID, n) {
			p.event(events.WorkEscalated, row, n.Key, gen, workLadderEventFields{Reason: workladder.AlertNudgeRefused})
		}
		return
	}
	p.event(events.WorkNudged, row, n.Key, gen, fields)
	if refused > 0 {
		p.settle(row.ID, func(r workladder.Record) (workladder.Record, bool) { return workladder.NudgeTaken(r, n.Key) })
	}
}

// ladderDeliver is the ladder's one delivery seam: today's nudge call
// (runNudgeBackstop's), which builds 8d4 carries. W5 swaps it for an OJ
// planner notice.
func ladderDeliver(sp runtime.Provider, name, text string) error {
	if sp == nil || name == "" {
		return runtime.ErrRuntimeUnavailable
	}
	return sp.Nudge(name, runtime.TextContent(text))
}

// nudgeText is n's text (§6.6): a trigger's is the agent's configured nudge
// or the pool default; a continuation's and an unstarted claim's the
// configured nudge or the claim line; a started claim's is built in.
func (p *ladderPass) nudgeText(row beads.Bead, n workladder.Nudge) string {
	configured := strings.TrimSpace(claimNudgeFor(p.cfg, row))
	switch {
	case n.Kind == workladder.KindClaim && n.Rung >= workladder.RungStarted:
		return fmt.Sprintf(ladderStartedNudge, n.Key.Bead)
	case configured != "":
		return configured
	case n.Kind == workladder.KindTrigger:
		return defaultPoolClaimNudge
	default:
		return ladderClaimedNudge
	}
}

// refused records, by CAS, that the seat could not take counted nudge n (a
// permission prompt, the runtime unavailable): the attempt is re-counted as
// paused and the entry backs off (workladder.NudgeRefused). It reports
// whether this refusal is the one to raise the nudge_refused alert for.
func (p *ladderPass) refused(id string, n workladder.Nudge) bool {
	var alert bool
	p.settle(id, func(r workladder.Record) (workladder.Record, bool) {
		out, a, ok := workladder.NudgeRefused(r, n.Key, n.Attempt, p.now)
		alert = a
		return out, ok
	})
	return alert
}

// settle rewrites row id's cell by CAS with change, on a fresh read; change
// reports false to write nothing.
func (p *ladderPass) settle(id string, change func(workladder.Record) (workladder.Record, bool)) {
	_, err := p.front.UpdateRowFenced(id, workLadderCommitAttempts, func(fresh session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
		r, err := workladder.DecodeLadder(resp.Metadata[session.WorkLadderKey])
		if err != nil || fresh.Closed {
			return session.RowPatch{}, false
		}
		r, ok := change(r)
		if !ok {
			return session.RowPatch{}, false
		}
		return session.RowPatch{Metadata: session.MetadataPatch{session.WorkLadderKey: workladder.EncodeLadder(r)}}, true
	})
	if err != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: settling %s's cell: %v\n", id, err) //nolint:errcheck // best-effort stderr
	}
}

// recycle posts the seat's recycle to the planner's inbox, at the generation
// the decision landed at, so A16's drain cannot land on a later incarnation.
func (p *ladderPass) recycle(row beads.Bead, gen int64) {
	if p.l.stalled == nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: recycling %s: %v\n", row.ID, errNoPlannerInbox) //nolint:errcheck // best-effort stderr
		return
	}
	p.l.stalled(executionStalledRequest{ID: row.ID, Generation: strconv.FormatInt(gen, 10), At: p.now})
}

// release gives back an unstarted claim the cell's release names (§6.5 steps
// 2 and 3) and reports whether it did. It reads the claim live on the
// seat's legs: a bead whose gc.session_id names the seat was handed to the
// seat's turn, so it is started, not released. Otherwise it releases it
// while still in_progress and assigned to the seat (ReleaseWorkBead fences
// its write on both). Then it settles the cell by CAS: released, the release
// is cleared; kept (no lane serves its route, no single live hit) or failed,
// the entry is marked so it is nudged at Cap, with an alert, and the release
// retried at Cap; found started, the entry is started.
func (p *ladderPass) release(row beads.Bead, key workladder.ClaimKey, gen int64) bool {
	info := sessionInfoFromBead(row)
	scope := releaseScope(info, p.cfg)
	hits, err := seatWorkFor(p.env.WorkLegs, scope).match(seatWorkQuery{scope: scope, statuses: startedOnly}, false)
	hits = slices.DeleteFunc(hits, func(h seatWorkHit) bool { return h.bead.ID != key.Bead })
	var outcome, why string
	switch {
	case err != nil:
		outcome, why = "failed", err.Error()
	case len(hits) == 0 && p.claimGone(row, key.Bead):
		outcome = "gone" // finished or moved since the snapshot: nothing to release, nothing to report
	case len(hits) != 1:
		outcome, why = "kept", fmt.Sprintf("%d live hits", len(hits))
	case beadmeta.HasDispatchHold(hits[0].bead.Labels):
		outcome = "gone" // parked on a hold since the snapshot: not the ladder's
	case strings.TrimSpace(hits[0].bead.Metadata[beadmeta.SessionIDMetadataKey]) == strings.TrimSpace(row.ID):
		// The live read of gc.session_id, which commit's CAS takes from the
		// snapshot: the guard for a claim the hook stamped after it was read.
		outcome = "started"
	default:
		out := releaseListed(hits, releaseLadder, releaseSeatOfInfo(info), p.cfg, ReleaseOpts{})
		logRelease(p.l.stderr, "work-ladder", row.ID, out, nil)
		switch {
		case len(out.Released) == 1:
			outcome = "released"
		case len(out.Failed) > 0:
			outcome, why = "failed", out.Why[key.Bead]
		default:
			outcome, why = "kept", out.Why[key.Bead]
		}
	}
	fields := workLadderEventFields{Kind: workladder.KindClaim.String(), Rung: workladder.RungClaimed.String(), Reason: why}
	switch outcome {
	case "released":
		p.event(events.WorkReleased, row, key, gen, fields)
	case "kept", "failed":
		fields.Reason = outcome + ": " + why
		fmt.Fprintf(p.l.stderr, "work-ladder: releasing %s's claim %s: %s\n", row.ID, key.Bead, fields.Reason) //nolint:errcheck // best-effort stderr
		p.event(events.WorkReleaseUnfinished, row, key, gen, fields)
	}
	p.settle(row.ID, func(r workladder.Record) (workladder.Record, bool) {
		switch outcome {
		case "released", "gone":
			return workladder.ClearRelease(r, key, gen)
		case "started":
			return workladder.ReleaseFoundStarted(r, key, gen, workladder.SrcClaimSession, p.now)
		default:
			return workladder.ReleaseUnfinished(r, key, gen, p.now)
		}
	})
	return outcome == "released"
}

// claimGone reports, from a live read, that bead is no longer an
// in_progress claim of row's seat (closed, released, or moved since the
// snapshot). A read that fails is not proof.
func (p *ladderPass) claimGone(row beads.Bead, bead string) bool {
	b, found, err := p.env.WorkLegs.readLive(bead)
	switch {
	case err != nil:
		return false
	case !found:
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(b.Status), "in_progress") ||
		!slices.Contains(currentSessionAssigneeIdentities(row), strings.TrimSpace(b.Assignee))
}

// workLadderEventFields are the parts of a work.* event's payload beyond the
// seat and the work.
type workLadderEventFields struct {
	Kind, Rung, Reason string
	Attempt            int
	Paused             bool
}

// event records one work.* event about key on row's seat at gen.
func (p *ladderPass) event(typ string, row beads.Bead, key workladder.ClaimKey, gen int64, f workLadderEventFields) {
	if p.l.events == nil {
		return
	}
	payload, err := json.Marshal(events.WorkLadderPayload{
		SessionID: row.ID, SessionName: strings.TrimSpace(row.Metadata["session_name"]), Template: strings.TrimSpace(row.Metadata["template"]),
		BeadID: key.Bead, StoreRef: key.Store, Generation: gen, Kind: f.Kind, Rung: f.Rung, Attempt: f.Attempt, Paused: f.Paused, Reason: f.Reason,
	})
	if err != nil {
		return
	}
	p.l.events.Record(events.Event{
		Type: typ, Actor: eventActor(), Subject: key.Bead, SessionID: row.ID, Message: f.Reason, Payload: payload,
	})
}
