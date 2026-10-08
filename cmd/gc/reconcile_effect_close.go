package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The close effect (CONTRACT v5 C3, arm A21). Under the runtime name lock,
// and then one session mutation section (closeLocked), it first confirms
// the stop (C8.8, v5 F3): a complete, error-free fresh read
// through the composite provider, begun after the effect began, that shows
// no live pane for the row's name. A corpse counts as stopped. When the
// attach leg (L3) passes, the effect also removes the corpse under the
// corpse exception (v5 F2), by exact session object, and then reads again:
// the kill re-checks server-side, and a refused kill leaves the corpse in
// place (a multi-pane corpse is never auto-removed), so the second read alone
// decides, and only a live pane refuses. Then the premise close: the row the
// pass decided on is the premise, so a row that changed since (a wake that
// landed after the read) refuses with cause superseded and is decided again,
// never closed at its new revision. Right before the close, in the same
// section, it reads the work guard (L5) live (guardWork). A landed close then
// runs legacy's post-close cascade, best-effort as legacy does (cascade),
// outside the section, as legacy's closeBead does.
//
// The section is process-local. Another process is not held off: its row
// writes still lose the close's premise CAS, but a `gc session attach` that
// re-creates the runtime after the last fresh read and before the CAS is
// not seen (the cross-process attach residual, as for every C8.8 read). The
// row then closes over a live runtime that no open row owns, as legacy's
// close can.

// Close refusal causes; a fresh read's are freshLiveness's.
const (
	causeLivePane   = "live-pane"  // the fresh read shows a live pane: not stopped
	causeSuperseded = "superseded" // the row changed since the pass decided
	causeHasWork    = "has-work"   // L5: work is assigned, or the read failed
	causeStranded   = "stranded"   // L5 found work on a pool slot: the marker is stamped
	// causeUnstamped: L5 found work on an unmarked pool slot, and nothing was
	// stamped (its work is detached and alive, or the fresh row moved).
	causeUnstamped     = "stranded-unstamped"
	causeReleaseFailed = "release-failed" // the stranded repair's unclaim failed on a leg
)

// errNoReadStore refuses L5 when the pass has no read-only city store: the
// work guard fails closed.
var errNoReadStore = errors.New("v2 close: no read-only city store for the work guard (L5)")

// closeEffect is an admitted close. Its intent's Patch is the terminal patch
// (session.ClosePatch and any clears the close kind adds) and its Event what
// a landed close records.
func closeEffect(p *effectPass, it intent) func(context.Context) settlement {
	return closeRun{pass: p, it: it, now: time.Now, prune: pruneAgentHomeWorktreeIfSafeInfo}.run
}

// closeRun is one close. now is the runtime's clock: a fresh read accepts
// only a refresh begun at or after the time it gives. prune is the worker
// worktree prune.
type closeRun struct {
	pass  *effectPass
	it    intent
	now   func() time.Time
	prune func(info session.Info, cityPath string, cfg *config.City, stderr io.Writer)
}

func (e closeRun) run(ctx context.Context) settlement {
	row, ok := e.pass.World.Census.Rows[e.it.Key]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeRedecided}
	}
	writer, ok := e.pass.Writers[e.it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	name, unlock, ok := lockRuntimeName(e.pass.World, row.Info)
	switch {
	case !ok && name == "":
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	case !ok:
		return settlement{Outcome: settledRefused, Cause: causeNameBusy}
	}
	defer unlock()
	var s settlement
	var snapshot beads.Bead // the open row, whose identities the release reads
	_ = session.WithSessionMutationLock(row.Info.ID, func() error {
		s, snapshot = e.closeLocked(ctx, writer, row.Info, name)
		return nil
	})
	if s.Outcome == settledLanded {
		e.cascade(e.pass.Releasers.Legs[e.it.Key.Leg], row.Info, snapshot, snapshot.ID != "")
	}
	return s
}

// closeLocked is the close inside one session mutation section, so no
// in-process writer of the row lands between its reads and its CAS: the
// fresh reads (their since stamp included), the work guard (L5) with the
// stranded marker's CAS, the open row the release reads, and the premise
// close.
func (e closeRun) closeLocked(ctx context.Context, writer fencedWriter, row session.Info, name string) (settlement, beads.Bead) {
	if cause := e.noLivePane(ctx, name); cause != "" {
		return settlement{Outcome: settledRefused, Cause: cause}, beads.Bead{}
	}
	if err := ctx.Err(); err != nil {
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: err}, beads.Bead{}
	}
	patch := e.it.Patch
	var evs []events.Event
	if !e.it.Closing.Phantom {
		var cause string
		if patch, cause, evs = e.guardWork(ctx, writer, row); cause != "" {
			return settlement{Outcome: settledRefused, Cause: cause, Events: evs}, beads.Bead{}
		}
	}
	if err := ctx.Err(); err != nil { // the work guard may have run past the deadline
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: err, Events: evs}, beads.Bead{}
	}
	var snapshot beads.Bead
	if releaser := e.pass.Releasers.Legs[e.it.Key.Leg]; releaser != nil {
		if b, err := releaser.Get(row.ID); err == nil {
			snapshot = b
		}
	}
	closed, err := writer.closeWithTerminalPatch(row, patch, "gc: close session "+row.ID, e.pass.World.Now)
	switch {
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err, Events: evs}, snapshot
	case errors.Is(err, session.ErrSessionCloseSuperseded), errors.Is(err, session.ErrSessionKillPending):
		return settlement{Outcome: settledRefused, Cause: causeSuperseded, Err: err, Events: evs}, snapshot
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err, Events: evs}, snapshot
	case !closed: // another writer closed it first, and runs its own cascade
		return settlement{Outcome: settledNoop, Events: evs}, snapshot
	}
	return settlement{Outcome: settledLanded, Events: append(evs, eventsOf(e.it.Event)...)}, snapshot
}

// guardWork is L5, read live right before the close through the read-only
// city and rig stores, over every store the row's agent can reach
// (sessionHasOpenAssignedWorkForReachableStore); a failed read counts as
// work. With no work the close proceeds with patch. With work:
//   - a confirmed orphan releases its held claims (releaseConfirmedOrphanSessionWork,
//     one bead.dead_assignee_reopened each) and reads L5 again (SESS-082);
//   - a pool slot whose stranded marker has aged unclaims its work on every
//     leg, and closes stranded-repair only if every release landed (SESS-625);
//   - a pool slot with no marker stamps it and records session.stranded
//     (SESS-624), and holds until it ages;
//   - anything else refuses has-work.
func (e closeRun) guardWork(ctx context.Context, writer fencedWriter, row session.Info) (session.MetadataPatch, string, []events.Event) {
	has, err := e.hasWork(row)
	spec := e.it.Closing
	switch {
	case err == nil && !has:
		return e.it.Patch, "", nil
	case err != nil:
		return nil, causeHasWork, nil
	case spec.Orphaned:
		var evs eventSlice
		released := releaseConfirmedOrphanSessionWork(e.cfg(), e.pass.Releasers.City, e.pass.Releasers.Rigs, e.pass.World.Demand.AssignedWork, e.pass.Releasers.Assigned, row)
		emitDeadAssigneeReopenedEvents(&evs, e.pass.World.Demand.AssignedWork, released, e.pass.World.Now)
		if has, err = e.hasWork(row); len(released) > 0 && err == nil && !has {
			return e.it.Patch, "", evs
		}
		return nil, causeHasWork, evs
	case spec.Kind == closePoolSlot && spec.Repair:
		res := unclaimWorkAssignedToRetiredSessionInfo(e.pass.World.CityPath, e.cfg(), e.pass.Releasers.City, e.pass.Releasers.Rigs, row, retiredSessionFallbackRouteInfo(row), e.pass.Stderr)
		if res.Failed > 0 {
			return nil, causeReleaseFailed, nil
		}
		return session.ClosePatch(e.pass.World.Now, strandedRepairCloseReason), "", nil
	case spec.Kind == closePoolSlot && strings.TrimSpace(row.StrandedEventEmittedAt) == "":
		if evs := e.markStranded(ctx, writer, row); len(evs) > 0 {
			return nil, causeStranded, evs
		}
		return nil, causeUnstamped, nil
	}
	return nil, causeHasWork, nil
}

// hasWork is L5's live read; a missing store fails closed.
func (e closeRun) hasWork(row session.Info) (bool, error) {
	if e.pass.Reads.City == nil {
		return true, errNoReadStore
	}
	return sessionHasOpenAssignedWorkForReachableStore(e.pass.World.CityPath, e.cfg(), e.pass.Reads.City, e.pass.Reads.Rigs, row)
}

// markStranded stamps the stranded marker by CAS, and returns
// session.stranded with legacy's payload (emitSessionStrandedDiagnostic)
// when it landed. The CAS stamps only while the fresh row's marker is
// empty, it is still a freeable pool slot, and its lifecycle facts and
// incarnation are the ones the pass decided on (v5.6 C3). Work that every
// detached probe still reports alive is not stranded: nothing is stamped
// or recorded.
func (e closeRun) markStranded(ctx context.Context, writer fencedWriter, row session.Info) []events.Event {
	work, err := collectSessionAssignedWorkInfo(e.pass.World.CityPath, e.cfg(), e.pass.Reads.City, e.pass.Reads.Rigs, row)
	diagnostic := strandedDiagnosticWork(ctx, work)
	if err == nil && len(work) > 0 && len(diagnostic) == 0 {
		return nil
	}
	now := e.pass.World.Now.UTC()
	wrote, _ := writer.updateMetadataFenced(row.ID, 1, func(fresh session.Info, _ session.PersistedResponse) session.MetadataPatch {
		if strings.TrimSpace(fresh.StrandedEventEmittedAt) != "" || !isPoolSessionSlotFreeableInfo(fresh) ||
			!reflect.DeepEqual(session.LifecycleInputFromInfo(fresh), session.LifecycleInputFromInfo(row)) ||
			fresh.Generation != row.Generation || fresh.InstanceToken != row.InstanceToken {
			return nil
		}
		return session.MetadataPatch{strandedEventEmittedKey: now.Format(time.RFC3339)}
	})
	if !wrote {
		return nil
	}
	ids := strandedAssignedWorkIDs(diagnostic)
	return []events.Event{{
		Type: events.SessionStranded, Ts: now, Actor: "gc", Subject: row.ID, SessionID: row.ID,
		Message: formatStrandedMessage(e.it.Closing.Template, row.SessionNameMetadata, ids),
		Payload: api.SessionStrandedPayloadJSON(row.ID, row.SessionNameMetadata, e.it.Closing.Template, ids),
	}}
}

// strandedDiagnosticWork is legacy's detached-probe filter
// (filterDetachedStrandedDiagnosticWork) without its metadata clear on a
// dead probe: the effect reads through refusing stores, and the release
// clears it. Work whose probe reports alive is not stranded. Each probe runs
// under ctx, the effect's deadline, since it runs inside the close's locks.
func strandedDiagnosticWork(ctx context.Context, work []strandedAssignedWork) []strandedAssignedWork {
	out := make([]strandedAssignedWork, 0, len(work))
	for _, item := range work {
		spec := strings.TrimSpace(item.bead.Metadata[detachedProbeMetadataKey])
		if spec == "" || probeDetachedWork(ctx, spec).Status != detachedProbeAlive {
			out = append(out, item)
		}
	}
	return out
}

// cfg is the pass's config.
func (e closeRun) cfg() *config.City {
	if e.pass.World.Env == nil {
		return nil
	}
	return e.pass.World.Env.Cfg
}

// eventSlice is an events.Recorder that keeps what it records, for a
// settlement's Events.
type eventSlice []events.Event

func (s *eventSlice) Record(ev events.Event) { *s = append(*s, ev) }

// cascade is legacy's post-close cascade (closeBeadPreservingAssignees,
// closeFailedCreateBead), on the row's own leg store: cancel the row's waits
// and close its external-message bindings; then, except for a failed
// create, release its work (every identity it carried, alias history
// included, but the kept assignees), from the open row read before the
// close, as legacy skips it when that read failed; then, for a pool slot,
// prune its worker worktree when safe. Each step is best-effort: errors go
// to the diagnostics, and C8's orphan release is the fallback.
func (e closeRun) cascade(store beads.Store, row session.Info, snapshot beads.Bead, haveSnapshot bool) {
	now := e.pass.World.Now
	cancelStateAssignedToRetiredSessionBead(store, row.ID, now, e.pass.Stderr)
	if e.it.Closing.Kind != closeFailedCreate && haveSnapshot {
		releaseWorkFromClosedSessionBeadExcept(store, snapshot, assigneePreserveSet(e.it.Closing.Preserve), e.pass.Stderr)
	}
	if e.it.Closing.Kind == closePoolSlot && e.cfg() != nil {
		e.prune(row, e.pass.World.CityPath, e.cfg(), e.pass.Stderr)
	}
}

// noLivePane is C8.8 for the close, removing a detached corpse on the way.
// It returns the refusal cause, or "" once no live pane is confirmed.
func (e closeRun) noLivePane(ctx context.Context, name string) string {
	live, cause := freshLiveness(ctx, e.pass.Runtime, name, e.now())
	switch {
	case cause != "":
		return cause
	case live.Running:
		return causeLivePane
	case !live.Corpse || !e.removeCorpse(ctx, name, live):
		return ""
	}
	if live, cause = freshLiveness(ctx, e.pass.Runtime, name, e.now()); cause == "" && live.Running {
		cause = causeLivePane // the name now holds a session the first read did not (R54)
	}
	return cause
}

// removeCorpse kills the corpse live read, under the corpse exception:
// identity waived, L3 kept. The routed leaf both answers L3 and kills, so the
// two cannot split across backends; a leaf that cannot kill by session
// object leaves the corpse. It reports whether a kill was sent.
func (e closeRun) removeCorpse(ctx context.Context, name string, live runtime.Liveness) bool {
	leaf, _, known := runtime.ResolveBackend(e.pass.Runtime, name)
	killer, kills := leaf.(runtime.SessionObjectKiller)
	if !known || !kills || ctx.Err() != nil {
		return false
	}
	if reason, _ := attachLeg(ctx, leaf, name, false, e.pass.World.Now); reason != "" {
		return false // an attached corpse stays (v5 D3)
	}
	_, _ = killer.KillCorpseObject(name, live.ObjectID, live.ObjectCreated)
	return true
}
