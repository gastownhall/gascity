package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The session controller (P4 spec §3.1): ctrl.session's real implementation,
// one reconcile of one session key. It gathers the row and its inputs, runs
// the two-phase pure decide, rechecks at the commit point and performs at
// most one action, then returns requeueAfter. Provider mutations go to the
// effect executor (reconcile_session_effects.go); a write is fenced on the
// lifecycle facts the decide read. Reads are proposals: every read may be
// stale, and only the fenced write and the effect's own fence make an action
// safe.
//
// Unwired until P4.1e: v2SessionControllerReal stays false and
// defaultV2Controllers keeps the trace-only skeleton.

// v2MaxProbeRounds bounds the decide's probe rounds per reconcile (§3.3).
const v2MaxProbeRounds = 2

// Reason kinds the session controller adds.
const (
	v2ReasonEffect = "effect"
	v2ReasonNote   = "note"
)

// sessionController reconciles session keys.
type sessionController struct {
	host v2Host
	exec *effectExecutor
	// snapshot returns the latest published selection snapshot, or nil
	// before S_1. P3-7 publishes it.
	snapshot      func() *selectionSnapshot
	wakeAllocator func(kind string)
	// decide is decideSession; tests substitute it to drive the probe and
	// commit paths.
	decide func(sessionInputs, probeAnswers) (sessionDecision, probeKinds)
	// lockRow is session.WithSessionMutationLock, the process-wide row lock
	// every v2 row write holds.
	lockRow func(id string, fn func() error) error

	mu     sync.Mutex
	traced map[rowKey]string // the last reason traced per key: trace on change only
}

// newSessionController builds rt's session controller over snapshot.
func newSessionController(rt *v2Runtime, snapshot func() *selectionSnapshot) *sessionController {
	return &sessionController{
		host:          rt.host,
		exec:          rt.exec,
		snapshot:      snapshot,
		wakeAllocator: func(kind string) { rt.alloc.wake(workqueue.Reason{Kind: kind}) },
		decide:        decideSession,
		lockRow:       session.WithSessionMutationLock,
		traced:        make(map[rowKey]string),
	}
}

// reconcile is ctrl.session. An error backs the key off (C1.4); a failed read
// is never an answer (GUAR-011).
func (c *sessionController) reconcile(ctx context.Context, env *reconcileEnv, it workqueue.Item[rowKey]) (time.Duration, error) {
	in, err := c.gather(env, it)
	if err != nil {
		return 0, err
	}
	ans := unaskedAnswers()
	d, ask := c.decide(in, ans)
	for round := 0; ask&^ans.Asked != 0; round++ {
		if round == v2MaxProbeRounds {
			// A decide that keeps asking acts on nothing, but its timers
			// still come due.
			d = sessionDecision{Reason: "probe-rounds-exhausted", RequeueAfter: d.RequeueAfter}
			break
		}
		c.probe(ctx, env, in.Row, ask&^ans.Asked, &ans)
		d, ask = c.decide(in, ans)
	}
	outcome := TraceOutcomeSkipped
	if d.Action.Kind == actWrite {
		var wrote bool
		d, wrote, err = c.write(ctx, in, ans)
		switch {
		case err != nil:
			return 0, err
		case wrote:
			outcome = TraceOutcomeApplied
			if d.Event != nil && c.host.rec != nil {
				c.host.rec.Record(*d.Event)
			}
		case d.Action.Kind == actWrite:
			// Another writer won the CAS, the latest entry no longer
			// authorizes the action, or the commit read needs a probe this
			// reconcile did not run: decide again from the store.
			outcome, d.RequeueAfter = TraceOutcomeRejected, time.Nanosecond
		}
	}
	c.trace(in, d, outcome)
	return d.RequeueAfter, nil
}

// gather reads one reconcile's inputs: the row through the cache (C2.9: no
// live read, even on an operator poke; a refused write evicts the row, so the
// next read reaches the store), the latest snapshot and entry, the in-flight
// effect and the I3 reading. It never lists the session fleet (GUAR-001).
func (c *sessionController) gather(env *reconcileEnv, it workqueue.Item[rowKey]) (sessionInputs, error) {
	in := sessionInputs{Key: it.Key, Now: time.Now()}
	store := c.host.sessionsStore()
	if store == nil {
		return in, errV2NoSessionsStore
	}
	row, err := sessionFrontDoor(store).Get(it.Key.ID)
	switch {
	case errors.Is(err, beads.ErrNotFound) || errors.Is(err, session.ErrSessionNotFound):
	case err != nil:
		return in, fmt.Errorf("reading session %s: %w", it.Key.ID, err)
	default:
		in.Row, in.Found = row, true
	}
	if in.Snap = c.snapshot(); in.Snap != nil {
		in.Entry = in.Snap.Entries[it.Key]
	}
	in.InFlightDeadline, in.InFlight = c.exec.inFlight(it.Key)
	if obs := c.host.observations(); obs != nil && in.Found {
		snap, maxAge := obs.Snapshot(), 2*env.patrol()
		listed, provable := inventoryAbsence(snap, in.Now, maxAge)
		// Shared slot names are refused at boot (C4.5 item 1b), so a row's
		// runtime name is its own.
		in.Obs = observeRow(snap, row.ID, strings.TrimSpace(row.SessionName), 1, listed, provable, in.Now, maxAge)
	}
	return in, nil
}

// probe runs the asked probes on the row's routed leaf, each bounded, and
// writes their answers back to I3 (Note). A note that flips a selection input
// wakes the allocator (P4 F12).
func (c *sessionController) probe(ctx context.Context, env *reconcileEnv, row session.Info, ask probeKinds, ans *probeAnswers) {
	ans.Asked |= ask
	name := strings.TrimSpace(row.SessionName)
	leaf, _, known := runtime.ResolveBackend(env.SP, name)
	if !known || name == "" {
		return
	}
	now, obs := time.Now(), c.host.observations()
	note := func(kind FactKind, v ObsFact) {
		if obs != nil && obs.Note(name, kind, v, now, SourceProbe, "") {
			c.wakeAllocator(v2ReasonNote)
		}
	}
	if ask&probeAttach != 0 {
		_, reporter := leaf.(runtime.AttachmentObserverWithError)
		if reporter && leaf.Capabilities().CanReportAttachment {
			type answer struct{ attached, ok bool }
			a, done := boundedProbe(ctx, func() answer {
				attached, err := runtime.IsAttachedWithError(leaf, name)
				return answer{attached, err == nil}
			})
			ans.Attached, ans.AttachKnown = a.attached, done && a.ok
			if ans.AttachKnown {
				note(FactAttached, obsFactOf(ans.Attached))
			}
		}
	}
	if ask&probePending != 0 {
		switch ans.Pending = boundedPending(ctx, leaf, name); ans.Pending {
		case pendingInteractionYes:
			note(FactPending, ObsYes)
		case pendingInteractionNo:
			note(FactPending, ObsNo)
		}
	}
}

// write is the commit point of a write (§3.6, §4.3, C0.6, C2.9). Under the
// process-wide row lock, with the worker's context checked inside it (an
// abandoned worker never writes after stop), it re-reads the row through the
// cache, decides again on that read, and writes the decided patch with a CAS
// at that read's revision. So the patch written is always the one decided
// from the version it is fenced on, and a lost CAS writes nothing: the
// store evicts the row and the key re-decides from the store. A commit read
// whose decide asks for a probe this reconcile did not run writes nothing and
// re-decides at once, so an unasked answer is never acted on. A
// desire-dependent action must still hold against the latest entry. A store
// with no conditional writer is refused, never written blind (C0.7). It
// returns the decision made at the commit point and whether it was written.
func (c *sessionController) write(ctx context.Context, in sessionInputs, ans probeAnswers) (sessionDecision, bool, error) {
	store := c.host.sessionsStore()
	switch w, _, err := beads.ResolveConditionalWriter(store); {
	case err != nil:
		return sessionDecision{}, false, fmt.Errorf("writing session %s: %w", in.Row.ID, err)
	case w == nil:
		return sessionDecision{}, false, fmt.Errorf("writing session %s: the sessions store has no conditional writer, and v2 never writes blind (C0.7)", in.Row.ID)
	}
	var d sessionDecision
	wrote := false
	err := c.lockRow(in.Row.ID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		wrote, err = sessionFrontDoor(store).UpdateMetadataFenced(in.Row.ID, 1, func(row session.Info, _ session.PersistedResponse) session.MetadataPatch {
			fresh := in
			fresh.Row = row
			var ask probeKinds
			d, ask = c.decide(fresh, ans)
			switch {
			case ask&^ans.Asked != 0:
				d.RequeueAfter = time.Nanosecond
				return nil
			case d.Action.Kind != actWrite || !c.authorized(fresh, d.Action):
				return nil
			}
			return d.Action.Patch
		})
		return err
	})
	return d, wrote, err
}

// authorized applies §4.3 rule 1 to a desire-dependent action: the latest
// entry must exist at the row's incarnation and still authorize it.
func (c *sessionController) authorized(in sessionInputs, a sessionAction) bool {
	if a.Authorize == nil {
		return true
	}
	latest := c.snapshot()
	if latest == nil {
		return false
	}
	e := latest.Entries[in.Key]
	return e != nil && e.Basis.Incarnation == rowIncarnation(in.Row) && a.Authorize(e)
}

// trace records d when its reason differs from the key's last one, so a
// standing skip, a missing or closed row's included, is traced once (C0.5,
// S10).
func (c *sessionController) trace(in sessionInputs, d sessionDecision, outcome TraceOutcomeCode) {
	last := d.Reason + "/" + string(outcome)
	c.mu.Lock()
	changed := c.traced[in.Key] != last
	c.traced[in.Key] = last
	c.mu.Unlock()
	if !changed {
		return
	}
	t := c.host.beginTrace("v2-session")
	t.RecordDecision(TraceSiteV2SessionDecision, TraceReasonCode(d.Reason), outcome, in.Row.Template, in.Row.SessionNameMetadata, map[string]any{
		"key": in.Key.Leg + "/" + in.Key.ID, "requeue_after": d.RequeueAfter.String(),
	})
	t.end(TraceCompletionCompleted, traceRecordPayload{"phase": "session"})
}
