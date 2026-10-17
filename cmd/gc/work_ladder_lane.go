package main

// The work-progress ladder's v2 lane step (PHASE1-2-EPOCH-LIVENESS-LADDER §6,
// slice W3). It replaces the three K1 backstops (the idle-claim, continuation
// and execution nudges) with one ladder per pool seat, keyed by (claim,
// generation), whose pacing never reaches zero: grace, then a doubling
// backoff to an hourly nudge, forever (internal/workladder).
//
// Per seat and pass: observe (the planner's last allocation, the row, the
// runtime, provider health), decide (workladder.Decide, pure), commit the
// work_ladder cell by a CAS on the row that decides again on the fresh read
// (UpdateRowFenced), and only then act: nudge through today's delivery call,
// post a recycle to the planner's inbox (arm A16), or release an unstarted
// claim. Every write of the cell is a CAS (WL6); the old pacing keys are
// neither read nor written (§6.7), so a deploy starts every seat from a fresh
// grace and no drain wave is possible.
//
// The agent's work_started cell (W1) is the only proof a claim is started.
// Until it lands, and for claims handed out before it, the seeds stand in:
// execution_claim_started, current_claim_bead_id and
// currently_processing_bead_id, read when the ladder first sees a generation.
// A release must re-prove at its CAS that neither the cell nor a seed names
// the claim (§6.5); the agent's start write refuses a claim the release names.

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

// The work cells on a session row (§6.1): work_ladder is this step's,
// work_started the agent's.
const (
	workLadderKey  = "work_ladder"
	workStartedKey = "work_started"
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
	// ladder holds but the snapshot lacks is carried, not dropped.
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

// outstanding is each seat's work (§6.2), by session ID, from today's three
// predicates: its own trigger, open and unclaimed; exactly one ready
// continuation pinned to it; and the in_progress claims it holds under a
// current identity.
func (p *ladderPass) outstanding(s *allocSummary, raw []beads.Bead) map[string][]ladderTarget {
	out := make(map[string][]ladderTarget)
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
		if cs := conts.bySessionID[row.ID]; len(cs) == 1 && !conts.holdBySessionID[row.ID] {
			c := cs[0]
			target := backstopTarget{ID: c.WorkBeadID, RootID: c.RootBeadID, StoreRef: c.StoreRef, Assignee: c.Assignee, Store: c.Store}
			ts = append(ts, ladderTarget{
				work:    workladder.Work{Key: workladder.ClaimKey{Store: normalizeIdleClaimStoreRef(c.StoreRef), Bead: c.WorkBeadID}, Kind: workladder.KindContinuation, Assignee: c.Assignee},
				recheck: func() backstopResolution { return poolContinuationBackstop{}.revalidate(target) },
			})
		}
		for _, c := range claims.forIdentities(currentSessionAssigneeIdentities(row)) {
			target := backstopTarget{ID: c.BeadID, Assignee: c.Assignee, Store: c.Store}
			ts = append(ts, ladderTarget{
				work:    workladder.Work{Key: workladder.ClaimKey{Store: c.StoreRef, Bead: c.BeadID}, Kind: workladder.KindClaim, Assignee: c.Assignee},
				recheck: func() backstopResolution { return poolExecutionBackstop{}.revalidate(target) },
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
	if !ok || !isUnclaimedTrigger(p.cfg, w) {
		return workladder.ClaimKey{}, false
	}
	return workladder.ClaimKey{Store: key.StoreRef, Bead: id}, true
}

// triggerRecheck re-reads a trigger live through the store that owns it,
// before a recycle (the claim backstop's drain gate): still open and
// unclaimed, or the recycle is not made.
func (p *ladderPass) triggerRecheck(id string) func() backstopResolution {
	return func() backstopResolution {
		b, found, err := p.env.WorkLegs.readLive(id)
		switch {
		case err != nil:
			return backstopResolutionHold
		case !found || !isUnclaimedTrigger(p.cfg, b):
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
		targets = carryLadderWork(targets, prev)
	}
	info, live := sessionInfoFromBead(row), p.observeRuntime(row)
	var d workladder.Decision
	for range 3 { // each round drops work a live re-read proved gone
		d = decideWorkLadder(prev, p.observe(info, row.Metadata, prev, targets, live))
		gone, hold := recheckActed(d, targets)
		if hold {
			return // the read could not tell: act on nothing this pass
		}
		if len(gone) == 0 {
			break
		}
		targets = slices.DeleteFunc(targets, func(t ladderTarget) bool { return gone[t.work.Key] })
	}
	if !d.Changed && !hasEffects(d) {
		return
	}
	landed, ok := p.commit(row.ID, targets, live)
	if !ok {
		return
	}
	p.act(row, landed)
}

// carryLadderWork adds to targets, under an incomplete snapshot, every work
// the seat's ladder holds that the snapshot lacks, so a store that did not
// answer neither drops a ladder nor resets its count. Carried work has no
// live re-read here: the release and the recycle re-read it themselves, and a
// nudge about finished work is answered by the hook's no-work result.
func carryLadderWork(targets []ladderTarget, prev workladder.Record) []ladderTarget {
	for _, e := range prev.Entries {
		if !slices.ContainsFunc(targets, func(t ladderTarget) bool { return t.work.Key == e.Key }) {
			targets = append(targets, ladderTarget{work: workladder.Work{Key: e.Key, Kind: e.Kind}})
		}
	}
	return targets
}

// recheckActed re-reads live the work d nudges or recycles: the keys proved
// gone, and whether any read could not tell.
func recheckActed(d workladder.Decision, targets []ladderTarget) (gone map[workladder.ClaimKey]bool, hold bool) {
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
		if i < 0 || targets[i].recheck == nil {
			continue
		}
		switch targets[i].recheck() {
		case backstopResolutionHold:
			return nil, true
		case backstopResolutionClear:
			if gone == nil {
				gone = make(map[workladder.ClaimKey]bool)
			}
			gone[k] = true
		}
	}
	return gone, false
}

func hasEffects(d workladder.Decision) bool {
	return slices.ContainsFunc(d.Actions, func(a workladder.Action) bool { _, note := a.(workladder.Note); return !note })
}

// decodeLadderCell is meta's work_ladder cell; an unreadable one reads as
// absent (the ladder re-seeds it), with the issue.
func decodeLadderCell(meta map[string]string) (workladder.Record, error) {
	r, err := workladder.DecodeLadder(meta[workLadderKey])
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
	rt.activity = last
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
// meta), its cell prev, its work and its runtime.
func (p *ladderPass) observe(info session.Info, meta map[string]string, prev workladder.Record, targets []ladderTarget, rt ladderRuntime) workladder.Observation {
	gen := rowGeneration(meta)
	obs := workladder.Observation{
		Now: p.now, Gen: gen, Runtime: rt.running, Quiet: rt.quiet, LastActivity: rt.activity, Provider: rt.provider,
		Gate: ladderGate(info, meta, p.now),
	}
	for _, t := range targets {
		obs.Outstanding = append(obs.Outstanding, t.work)
	}
	obs.Started, obs.StartedRead = ladderStarted(meta, prev, gen, obs.Outstanding)
	return obs
}

// ladderStarted is the start evidence for each outstanding claim: the agent's
// work_started cell, and the seeds (§6.7) when the ladder cell is absent or
// names another generation. read is false when the cell is unreadable, and
// then every claim reads as started.
func ladderStarted(meta map[string]string, prev workladder.Record, gen int64, work []workladder.Work) (map[workladder.ClaimKey]workladder.StartSource, bool) {
	cell, err := workladder.DecodeStarted(meta[workStartedKey])
	seeding := prev.V == 0 || prev.Gen != gen
	started := make(map[workladder.ClaimKey]workladder.StartSource)
	for _, w := range work {
		if w.Kind != workladder.KindClaim {
			continue
		}
		switch {
		case err == nil && cell.Names(w.Key):
			started[w.Key] = workladder.SrcAgent
		case seeding:
			if src, ok := ladderSeed(meta, w.Key.Bead); ok {
				started[w.Key] = src
			}
		}
	}
	return started, err == nil
}

// ladderSeed is the legacy evidence that names bead started on this row: the
// execution backstop's stamp for the row's incarnation, the hook's
// current-claim stamp, or the currently processing bead.
func ladderSeed(meta map[string]string, bead string) (workladder.StartSource, bool) {
	bead = strings.TrimSpace(bead)
	switch {
	case bead == "":
		return 0, false
	case strings.TrimSpace(meta[executionClaimStartedKey]) == strings.TrimSpace(meta["instance_token"])+"|"+bead:
		return workladder.SrcExecutionClaimStarted, true
	case strings.TrimSpace(meta[beadmeta.CurrentClaimBeadIDMetadataKey]) == bead:
		return workladder.SrcCurrentClaim, true
	case strings.TrimSpace(meta[session.CurrentBeadIDKey]) == bead:
		return workladder.SrcCurrentlyProcessing, true
	}
	return 0, false
}

// ladderGate is open, or held by what owns the seat for now: a stop request
// (a drain, at the row's generation), a kill, a start in flight, a restart
// request, an operator's suspend or a wait hold, a quarantine, or an agent's
// heartbeat, which holds until its held_until (WL5). Every hold is one the
// row clears when it ends, so none can silence a seat for good.
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
		return workladder.Gate{Held: true, Reason: "heartbeat", Until: d.Heartbeat}
	case d.Wait || d.WaitIntent:
		return held("agent_wait")
	case d.Operator != session.OperatorNone: // a stale sleep_reason on a running row is no hold
		return held("operator_hold")
	case d.Quarantine.After(now):
		return workladder.Gate{Held: true, Reason: "quarantined", Until: d.Quarantine}
	}
	return workladder.Gate{}
}

// commit writes the seat's cell by a CAS that decides again on the fresh
// row: the agent's start write, the planner's, or another pass's may have
// landed since the read. A release it writes is re-proved there (§6.5): the
// row open, the agent cell readable and not naming the claim, and no seed
// naming it; a seed that names it makes the claim started instead. It
// returns the decision that landed (or, writing nothing, the fresh one).
func (p *ladderPass) commit(id string, targets []ladderTarget, rt ladderRuntime) (workladder.Decision, bool) {
	var landed workladder.Decision
	wrote, err := p.front.UpdateRowFenced(id, workLadderCommitAttempts, func(fresh session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
		landed = workladder.Decision{}
		if fresh.Closed {
			return session.RowPatch{}, false
		}
		prev, _ := decodeLadderCell(resp.Metadata)
		obs := p.observe(fresh, resp.Metadata, prev, targets, rt)
		d := decideWorkLadder(prev, obs)
		for range len(targets) + 1 {
			k, ok := newRelease(prev, d)
			if !ok {
				break
			}
			src, seeded := ladderSeed(resp.Metadata, k.Bead)
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
		return session.RowPatch{Metadata: session.MetadataPatch{workLadderKey: workladder.EncodeLadder(d.Next)}}, true
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

// act performs what landed: the nudge, the recycle, the release and the
// alerts.
func (p *ladderPass) act(row beads.Bead, d workladder.Decision) {
	gen := d.Next.Gen
	recycled := false
	for _, a := range d.Actions {
		switch a := a.(type) {
		case workladder.Nudge:
			p.nudge(row, a, gen)
		case workladder.Recycle:
			p.event(events.WorkRecycleRequested, row, a.Key, gen, workLadderEventFields{Reason: recycleReason(a)})
			if !recycled {
				recycled = true
				p.recycle(row, gen)
			}
		case workladder.Release:
			at := gen // a release re-emitted from an earlier generation clears at its own
			if r := d.Next.Release; r != nil && r.Key == a.Key {
				at = r.Gen
			}
			p.release(row, a.Key, at)
		case workladder.Alert:
			fmt.Fprintf(p.l.stderr, "work-ladder: %s: %s on %s\n", row.ID, a.Reason, a.Key.Bead) //nolint:errcheck // best-effort stderr
			p.event(events.WorkEscalated, row, a.Key, gen, workLadderEventFields{Reason: a.Reason})
		}
	}
}

func recycleReason(r workladder.Recycle) string {
	if r.KeepClaim {
		return "started_unanswered"
	}
	return "trigger_unclaimed"
}

// nudge delivers n (§6.6) through today's call, after its attempt landed
// (write-ahead). A delivery error counts the attempt; one that says the
// runtime is unavailable is re-counted as paused.
func (p *ladderPass) nudge(row beads.Bead, n workladder.Nudge, gen int64) {
	text := p.nudgeText(row, n)
	fields := workLadderEventFields{Kind: n.Kind.String(), Rung: n.Rung.String(), Attempt: n.Attempt, Paused: n.Paused}
	err := ladderDeliver(p.env.SP, strings.TrimSpace(row.Metadata["session_name"]), text)
	if err != nil {
		fields.Reason = err.Error()
		fmt.Fprintf(p.l.stderr, "work-ladder: nudging %s about %s failed: %v\n", row.ID, n.Key.Bead, err) //nolint:errcheck // best-effort stderr
		if errors.Is(err, runtime.ErrRuntimeUnavailable) && !n.Paused {
			p.repause(row.ID, n.Key, n.Attempt)
		}
		p.event(events.WorkNudgeFailed, row, n.Key, gen, fields)
		return
	}
	p.event(events.WorkNudged, row, n.Key, gen, fields)
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

// repause re-counts a nudge attempt the runtime could not take as paused, by
// CAS, while the cell still holds that attempt.
func (p *ladderPass) repause(id string, key workladder.ClaimKey, attempt int) {
	_, err := p.front.UpdateRowFenced(id, workLadderCommitAttempts, func(fresh session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
		r, err := workladder.DecodeLadder(resp.Metadata[workLadderKey])
		if err != nil || fresh.Closed {
			return session.RowPatch{}, false
		}
		i := slices.IndexFunc(r.Entries, func(e workladder.Entry) bool { return e.Key == key })
		if i < 0 || r.Entries[i].Attempts != attempt || !r.Entries[i].Last.Equal(p.now) {
			return session.RowPatch{}, false
		}
		r.Entries = slices.Clone(r.Entries)
		r.Entries[i].Attempts--
		r.Entries[i].Paused++
		return session.RowPatch{Metadata: session.MetadataPatch{workLadderKey: workladder.EncodeLadder(r)}}, true
	})
	if err != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: re-counting %s's attempt on %s as paused: %v\n", id, key.Bead, err) //nolint:errcheck // best-effort stderr
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
// 2 and 3): read live on the seat's legs, released while still in_progress
// and assigned to the seat (ReleaseWorkBead fences its write on both), then
// the release cleared by CAS. A read or write that fails leaves the release
// standing: the next pass re-emits it.
func (p *ladderPass) release(row beads.Bead, key workladder.ClaimKey, gen int64) {
	info := sessionInfoFromBead(row)
	scope := releaseScope(info, p.cfg)
	hits, err := seatWorkFor(p.env.WorkLegs, scope).match(seatWorkQuery{scope: scope, statuses: startedOnly}, false)
	if err != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: reading %s's claim %s: %v\n", row.ID, key.Bead, err) //nolint:errcheck // best-effort stderr
		return
	}
	hits = slices.DeleteFunc(hits, func(h seatWorkHit) bool { return h.bead.ID != key.Bead })
	if len(hits) == 1 {
		out := releaseListed(hits, releaseLadder, releaseSeatOfInfo(info), p.cfg, ReleaseOpts{})
		logRelease(p.l.stderr, "work-ladder", row.ID, out, nil)
		if len(out.Failed) > 0 {
			return
		}
		if len(out.Released) > 0 {
			p.event(events.WorkReleased, row, key, gen, workLadderEventFields{Kind: workladder.KindClaim.String(), Rung: workladder.RungClaimed.String()})
		}
	}
	_, err = p.front.UpdateRowFenced(row.ID, workLadderCommitAttempts, func(_ session.Info, resp session.PersistedResponse) (session.RowPatch, bool) {
		r, err := workladder.DecodeLadder(resp.Metadata[workLadderKey])
		if err != nil {
			return session.RowPatch{}, false
		}
		cleared, ok := workladder.ClearRelease(r, key, gen)
		if !ok {
			return session.RowPatch{}, false
		}
		return session.RowPatch{Metadata: session.MetadataPatch{workLadderKey: workladder.EncodeLadder(cleared)}}, true
	})
	if err != nil {
		fmt.Fprintf(p.l.stderr, "work-ladder: clearing %s's release of %s: %v\n", row.ID, key.Bead, err) //nolint:errcheck // best-effort stderr
	}
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
