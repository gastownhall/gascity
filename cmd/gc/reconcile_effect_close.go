package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The close effect (CONTRACT v5.7 C3, arm A21), a transaction (runTx) under
// the runtime lease with the row's record (§12.5: the atomic close clears
// it). Each attempt reads the runtime fresh (readRuntime) and L5 live, and
// decides A21 again on them and the fresh row (redecideClose), so a row that
// woke, or whose runtime came back, is never closed.
//
//  1. C8.8: a corpse counts as stopped. When the read finds one, the Call
//     removes it under the corpse exception (identity waived, L3 kept), by
//     exact session object; a refused kill (a multi-pane corpse) leaves it,
//     and the next section's read alone decides: only a live pane refuses.
//  2. The work branches, before the close, through ReleaseClaims:
//     - a killed pool seat gives back its unexecuted claims, and keeps them
//     all when it started any (B1); it never takes the stranded branch;
//     - with work found (L5), an orphaned orphan releases its claims
//     (SESS-082), and its close reads L5 again;
//     - with work found on a pool slot, a stranded marker that has aged
//     unclaims its work on every leg (SESS-625), and the close lands only if
//     no release failed; with no marker, the effect stamps one and records
//     session.stranded (SESS-624), unless every claim's detached probe still
//     reports alive.
//  3. The premise close: L5 must read Free, except for a SESS-080 phantom,
//     whose work is the configured identity's; a repair, whose release
//     decided; and an orphaned orphan whose remaining claims no lane serves
//     (O5, revised). The terminal patch is the re-decided intent's.
//  4. After a landed close only, outside every lock (around): legacy's
//     post-close cascade, best-effort (closeReleases.cascade). A claim no
//     lane serves stays on the closed row (O5, revised) and raises
//     session.stranded_claims.
//
// Residuals: a `gc session attach` in another process that re-creates the
// runtime after the last fresh read and before the close is not seen (the
// cross-process attach residual of every C8.8 read); the row then closes
// over a runtime no open row owns, as legacy's close can. The orphan
// release's bead.dead_assignee_reopened events ride the close section's
// step, so a premise refusal there drops them (the releases stand).

// Close refusal causes; a fresh read's are readRuntime's.
const (
	causeLivePane      = "live-pane"          // the fresh read shows a live pane: not stopped
	causeStranded      = "stranded-unstamped" // work on an unmarked pool slot whose detached probes all report alive
	causeReleaseFailed = "release-failed"     // the stranded repair's release failed on a claim or a leg
)

// closeSections are C8.8 with its corpse kill, the work branches with their
// release, then the premise close.
var closeSections = []section{
	calledWhen(callStop, section{Decide: closeCorpseStep}, corpseKill.found, killCorpse),
	calledWhen(callWork, probed(strandedProbe, closeWorkStep), closeRelease.wanted, releaseBeforeClose),
	asClose(probed(strandedOnlyProbe, closeStep)),
}

// asClose is sec as a close section.
func asClose(sec section) section {
	sec.Premise = premiseClose
	return sec
}

// strandedOnlyProbe reads, for an orphaned orphan whose L5 still finds work
// after its release, whether every claim left is one no lane serves under
// the cascade's rule: the close then lands over them, and the cascade keeps,
// stamps and alerts on them (O5, revised).
func strandedOnlyProbe(_ context.Context, r effectReads, v txView) (bool, error) {
	c := v.It.Closing
	if c.Kind != closeOrphan || !c.Orphaned || v.Fence.Work == nil || v.Fence.Work.Free || v.World.Env == nil {
		return false, nil
	}
	scope := releaseScope(v.Row, v.World.Env.Cfg)
	return onlyStrandedWork(seatWorkFor(r.legs, scope), v.Row), nil
}

// redecideClose is A21 decided again on the fresh row and runtime read:
// the close it decides, or the cause that refuses it.
func redecideClose(v txView) (intent, string) {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	fresh, _ := decideRow(&w, v.Alloc, v.It.Key)
	switch {
	case fresh.Kind == intentClose:
		return fresh, ""
	case v.RT.Class == rtUnsupported:
		return fresh, causeLivenessUnsupported
	case v.RT.Class == rtUnknown:
		return fresh, causeLivenessUnknown
	case v.RT.Class == rtAlive, v.RT.Class == rtZombie:
		return fresh, causeLivePane
	}
	return fresh, causeRedecided
}

// corpseKill is the corpse the first section's Call removes.
type corpseKill struct {
	leaf runtime.Provider
	name string
	live runtime.Liveness // the read's latest: its object
	now  time.Time
}

func (k corpseKill) found() bool { return k.leaf != nil }

// closeCorpseStep refuses a close A21 no longer decides, and hands the Call
// a corpse read on one session object across the read.
func closeCorpseStep(v txView) txStep {
	if _, cause := redecideClose(v); cause != "" {
		return txStep{Refuse: cause}
	}
	if v.RT.Class != rtCorpse || !v.RT.Same {
		return txStep{Pass: corpseKill{}}
	}
	return txStep{Pass: corpseKill{leaf: v.RT.leaf, name: strings.TrimSpace(v.Row.SessionName), live: v.RT.Again, now: v.Now}}
}

// killCorpse removes k's corpse when L3 passes, on a leaf that kills by
// session object; the kill re-checks the object server-side.
func killCorpse(ctx context.Context, _ txCaps, k corpseKill) (struct{}, error) {
	killer, ok := k.leaf.(runtime.SessionObjectKiller)
	if !ok {
		return struct{}{}, nil
	}
	if reason, _ := attachLeg(ctx, k.leaf, k.name, false, k.now); reason != "" {
		return struct{}{}, nil // an attached corpse stays (v5 D3)
	}
	_, err := killer.KillCorpseObject(k.name, k.live.ObjectID, k.live.ObjectCreated)
	return struct{}{}, err
}

// strandedWork is the stranded branch's probe: the unmarked pool slot's
// assigned work through the read-only legs, filtered by legacy's detached
// probes (filterDetachedStrandedDiagnosticWork, without its metadata clear:
// the legs refuse writes, and the release clears it). Each probe runs under
// the probe's bounded context.
type strandedWork struct {
	ids      []string
	alive    bool // every claim read reports a detached probe alive
	searched bool
}

// strandedProbe reads the stranded branch's work, only when it applies: L5
// found work on a pool slot that is not killed and carries no marker.
func strandedProbe(ctx context.Context, r effectReads, v txView) (strandedWork, error) {
	c := v.It.Closing
	if c.Kind != closePoolSlot || c.Killed || v.Fence.Work == nil || v.Fence.Work.Free ||
		strings.TrimSpace(v.Row.StrandedEventEmittedAt) != "" || v.World.Env == nil {
		return strandedWork{}, nil
	}
	work, err := collectSessionAssignedWorkInfo(seatWorkFor(r.legs, releaseScope(v.Row, v.World.Env.Cfg)), v.Row)
	out := strandedWork{searched: true}
	for _, item := range work {
		spec := strings.TrimSpace(item.bead.Metadata[detachedProbeMetadataKey])
		if spec == "" || probeDetachedWork(ctx, spec).Status != detachedProbeAlive {
			out.ids = append(out.ids, item.bead.ID)
		}
	}
	out.alive = err == nil && len(work) > 0 && len(out.ids) == 0
	return out, nil
}

// closeRelease is a release the work branch hands its Call.
type closeRelease struct {
	why   releaseWhy
	scope ReleaseScope
	seat  releaseSeat
}

func (r closeRelease) wanted() bool { return r.seat.id != "" }

// closeWorkStep is the work branch's Decide: a release for the Call, the
// stranded marker's stamp, or neither.
func closeWorkStep(v txView, stranded strandedWork, _ error) txStep {
	fresh, cause := redecideClose(v)
	if cause != "" {
		return txStep{Refuse: cause}
	}
	c := fresh.Closing
	var cfg *config.City
	if v.World.Env != nil {
		cfg = v.World.Env.Cfg
	}
	release := func(why releaseWhy) txStep {
		return txStep{Pass: closeRelease{why: why, scope: releaseScope(v.Row, cfg), seat: releaseSeatOfInfo(v.Row)}}
	}
	work := v.Fence.Work != nil && !v.Fence.Work.Free
	switch {
	case c.Killed:
		return release(releaseKilledSeat) // before L5, which the close reads after it
	case !work || c.Phantom:
	case c.Kind == closeOrphan && c.Orphaned:
		return release(releaseOrphan)
	case c.Kind == closePoolSlot && c.Repair:
		return release(releaseStrandedRepair)
	case c.Kind == closePoolSlot && strings.TrimSpace(v.Row.StrandedEventEmittedAt) == "":
		if !stranded.searched || stranded.alive {
			return txStep{Refuse: causeStranded}
		}
		at := v.World.Now.UTC()
		ids := stranded.ids
		name := v.Row.SessionNameMetadata
		return txStep{
			Write: session.MetadataPatch{strandedEventEmittedKey: at.Format(time.RFC3339)}, Done: true,
			Facts: effectFacts{Events: []events.Event{{
				Type: events.SessionStranded, Ts: at, Actor: "gc", Subject: v.Row.ID, SessionID: v.Row.ID,
				Message: formatStrandedMessage(c.Template, name, ids),
				Payload: api.SessionStrandedPayloadJSON(v.Row.ID, name, c.Template, ids),
			}}},
		}
	}
	return txStep{Pass: closeRelease{}}
}

// closeReleased is the work branch's release, as the close reads it.
type closeReleased struct {
	why    releaseWhy
	out    ReleaseOutcome
	events []events.Event
}

// releaseBeforeClose makes r's release through the close's release handles.
func releaseBeforeClose(_ context.Context, c txCaps, r closeRelease) (closeReleased, error) {
	if c.release == nil {
		return closeReleased{why: r.why}, errNoReadStore
	}
	return c.release.release(r)
}

// closeStep is the premise close's Decide.
func closeStep(v txView, strandedOnly bool, _ error) txStep {
	rel, relErr := callResult[closeReleased](v)
	step := txStep{Facts: effectFacts{Events: rel.events}}
	fresh, cause := redecideClose(v)
	work := v.Fence.Work == nil || !v.Fence.Work.Free
	switch c := fresh.Closing; {
	case cause != "":
		step.Refuse = cause
	case c.Repair && rel.why == releaseStrandedRepair && (relErr != nil || len(rel.out.Failed) > 0):
		step.Refuse, step.Err = causeReleaseFailed, relErr
	case c.Repair && rel.why == releaseStrandedRepair:
		step.Terminal = session.ClosePatch(v.World.Now, strandedRepairCloseReason)
	case work && !c.Phantom && (!c.Orphaned || !strandedOnly):
		step.Refuse = causeHasWork
		if v.Fence.Work != nil {
			step.Err = v.Fence.Work.Err
		}
	default:
		step.Terminal = fresh.Patch
	}
	return step
}

// closeAround runs the close, then its cascade once it landed. It runs
// nothing when its precheck finds work the close's branches cannot act on:
// the close would only refuse has-work, after taking the row's lease record.
func closeAround(_ context.Context, a aroundCaps, run func() settlement) settlement {
	if !closeWorthRunning(a) {
		return refused(causeHasWork)
	}
	s := run()
	if s.Closed && a.release != nil {
		a.release.cascade(a.it)
	}
	return s
}

// closeWorthRunning is the close's work precheck, outside every lock, over
// the read-only legs and the pass's row: false only when L5 finds work that
// no branch of the close acts on, so the effect would refuse has-work. A
// phantom skips L5; a killed seat runs while its release would give a claim
// back; an orphaned orphan while a claim is releasable or every claim is
// stranded; a pool slot while its marker is aged (the repair) or empty (the
// stamp). With no legs to read L5 through it runs nothing; a failed read
// runs, and the effect decides.
func closeWorthRunning(a aroundCaps) bool {
	c := a.it.Closing
	switch {
	case c.Phantom:
		return true
	case a.release == nil || a.reads.legs.unusable() != nil:
		return false // L5 cannot read: the effect could only refuse
	}
	cfg := a.release.cfg
	scope := releaseScope(c.Row, cfg)
	hits, err := seatWorkFor(a.reads.legs, scope).match(seatWorkQuery{scope: scope, statuses: seatWorkStatuses}, false)
	if err != nil || len(hits) == 0 {
		return true
	}
	seat := releaseSeatOfInfo(c.Row)
	direct := func(b beads.Bead) bool { _, ok := releaseRoute(cfg, b, seat.fallback, laneDirect); return ok }
	switch {
	case c.Killed:
		started := slices.ContainsFunc(hits, func(h seatWorkHit) bool {
			return h.bead.Status == "in_progress" || startedBy(c.Row, h.bead.ID)
		})
		return !started && slices.ContainsFunc(hits, func(h seatWorkHit) bool { return direct(h.bead) })
	case c.Kind == closeOrphan && c.Orphaned:
		return slices.ContainsFunc(hits, func(h seatWorkHit) bool { return direct(h.bead) }) ||
			onlyStrandedWork(seatWorkFor(a.reads.legs, scope), c.Row)
	case c.Kind == closePoolSlot:
		return c.Repair || strings.TrimSpace(c.Row.StrandedEventEmittedAt) == ""
	}
	return false
}

// startedBy reports that info names id as the work it executes
// (currently_processing_bead_id, current_claim_bead_id): execution evidence.
func startedBy(info session.Info, id string) bool {
	return id != "" && (strings.TrimSpace(info.CurrentlyProcessingBeadID) == id || strings.TrimSpace(info.CurrentClaimBeadID) == id)
}

// closeReleases are what a close's releases write through: the pass's raw
// work legs and leg stores, which only the close holds (capRelease), for its
// Call's release before the close and its around's cascade after it.
type closeReleases struct {
	legs     WorkLegs
	stores   map[string]beads.Store // by census leg
	cityPath string
	cfg      *config.City
	now      time.Time
	alerts   *strandedClaimAlerts
	stderr   io.Writer
}

// release is a work branch's release over the seat's legs, read live.
func (c *closeReleases) release(r closeRelease) (closeReleased, error) {
	out, err := ReleaseClaims(seatWorkFor(c.legs, r.scope), r.scope, r.seat, r.why, ReleaseOpts{})
	logRelease(c.stderr, "closing", r.seat.id, out, err)
	done := closeReleased{why: r.why, out: out}
	if r.why == releaseOrphan {
		for _, hit := range out.Released {
			b := hit.bead
			assignee, route := strings.TrimSpace(b.Assignee), strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
			done.events = append(done.events, events.Event{
				Type: events.BeadDeadAssigneeReopened, Ts: c.now.UTC(), Actor: "gc", Subject: b.ID,
				Message: formatDeadAssigneeReopenedMessage(b.ID, assignee, route),
				Payload: api.BeadDeadAssigneeReopenedPayloadJSON(b.ID, assignee, route),
			})
		}
	}
	return done, err
}

// cascade is legacy's post-close cascade (closeBeadPreservingAssignees,
// closeFailedCreateBead) for it's closed row: cancel its waits and close its
// external-message bindings; then, except for a failed create, release its
// work (every identity the closed row carries, but the kept assignees)
// through ReleaseClaims, alerting on what no lane serves; then, for a pool
// slot, prune its worker worktree when safe. Each step is best-effort, as
// legacy's: errors go to stderr, and the orphan release is the fallback.
func (c *closeReleases) cascade(it intent) {
	store := c.stores[it.Key.Leg]
	if store == nil {
		return
	}
	cancelStateAssignedToRetiredSessionBead(store, it.Key.ID, c.now, c.stderr)
	b, err := store.Get(it.Key.ID)
	if err != nil {
		fmt.Fprintf(c.stderr, "v2 close: reading closed session %s: %v\n", it.Key.ID, err) //nolint:errcheck // best-effort stderr
		return
	}
	if it.Closing.Kind != closeFailedCreate {
		preserve := assigneePreserveSet(it.Closing.Preserve)
		sw := seatWorkFor(c.legs, releaseScopeOfBead(b, c.cfg).except(preserve))
		out := releaseWorkFromClosedSessionBeadExcept(store, sw, b, preserve, c.stderr)
		strand(c.alerts, releaseSeatOfBead(b), strings.TrimSpace(b.Metadata["state"]), out, c.now, c.stderr)
	}
	if it.Closing.Kind == closePoolSlot && c.cfg != nil {
		pruneAgentHomeWorktreeIfSafeInfo(sessionInfoFromBead(b), c.cityPath, c.cfg, c.stderr)
	}
}

// newCloseReleases is w's close release handles, alerting through alerts.
func newCloseReleases(w *World, alerts *strandedClaimAlerts, stderr io.Writer) *closeReleases {
	c := &closeReleases{legs: w.WorkLegs, stores: w.LegStores, cityPath: w.CityPath, now: w.Now, alerts: alerts, stderr: stderr}
	if w.Env != nil {
		c.cfg = w.Env.Cfg
	}
	if c.stderr == nil {
		c.stderr = io.Discard
	}
	return c
}
