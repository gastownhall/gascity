package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// startDeferralHoldCeiling bounds how long a start-gate deferral keeps a
// never-started create past its pending-create lease. The memo is a hint the
// reconciler cannot re-derive from the bead, so it never extends the lease
// without limit: a create still not started this long after its lease began
// is rolled back as before. Sixty minutes is six times the 10-minute
// never-started lease: a fixed multiple of it, not a measured drain time.
const startDeferralHoldCeiling = 60 * time.Minute

// startDeferralMemo remembers which never-started pending creates the last
// start-executor run deferred at a start gate, and which never-started pending
// creates that run's planning snapshot contained at all. A site is a start gate,
// and records here, only when it declines a ready candidate for a reason
// independent of that candidate's own state, refuses it nothing (it is
// re-offered next pass), and writes nothing to any bead. A gate with its own
// hold, such as endpoint capacity, does not record. The classification of
// every start-op outcome is pinned in start_gate_classification_test.go.
// A create waiting only on such a gate has not been refused anything, so the
// 10-minute never-started lease must not roll it back while the pool drains
// at max_wakes_per_tick. A create the last run's snapshot did not contain was
// never offered a start either, whether it was born after the snapshot was read
// or the snapshot read lagged it, so it gets one pass of grace too. The memo is
// process-local and in-memory by design: it holds no bead state, and a restart
// forgets it; until the next complete pass publishes, every never-started
// create is unseen and gets first-pass grace, still bounded by
// startDeferralHoldCeiling.
//
// A nil memo records and holds nothing. One memo serves one start population:
// every completed run replaces its record, so only the main reconcile tick
// records into it (see CityRuntime.ensureStartDeferralMemo).
type startDeferralMemo struct {
	mu        sync.Mutex
	committed map[string]struct{}
	seen      map[string]struct{}
}

// startDeferralPass accumulates one executor run's start-gate deferrals and
// publishes them to the memo only when the run completes uncanceled. seen is the
// never-started pending creates the run's planning snapshot contained; a nil seen
// means the run had no complete observation and publishes nothing.
type startDeferralPass struct {
	memo     *startDeferralMemo
	seen     map[string]struct{}
	deferred map[string]struct{}
}

// newStartDeferralMemo returns an empty memo.
func newStartDeferralMemo() *startDeferralMemo { return &startDeferralMemo{} }

// beginPass opens a run whose planning snapshot contained the never-started
// pending creates in seen. A nil memo returns a nil pass.
func (m *startDeferralMemo) beginPass(seen map[string]struct{}) *startDeferralPass {
	if m == nil {
		return nil
	}
	return &startDeferralPass{memo: m, seen: seen, deferred: make(map[string]struct{})}
}

// noteGateDeferred records that the run deferred session id at a start gate.
func (p *startDeferralPass) noteGateDeferred(id string) {
	if p == nil || id == "" {
		return
	}
	p.deferred[id] = struct{}{}
}

// noteStartOutcome records session id when outcome is the async start limiter
// refusing it a slot, the one start-op outcome reported at the point the slot
// is reserved. Any other outcome records nothing here.
func (p *startDeferralPass) noteStartOutcome(outcome, id string) {
	if outcome == "deferred_by_async_start_limit" {
		p.noteGateDeferred(id)
	}
}

// finish publishes the run to the memo, replacing the previous record. A run
// whose ctx is canceled by now did not offer every candidate a start, so it
// commits nothing and leaves the previous record alone; so does a run with no
// observation (nil seen). An empty, non-nil seen is an observation and
// publishes. A nil ctx is never canceled.
func (p *startDeferralPass) finish(ctx context.Context) {
	if p == nil || p.seen == nil || (ctx != nil && ctx.Err() != nil) {
		return
	}
	p.memo.mu.Lock()
	defer p.memo.mu.Unlock()
	p.memo.committed = p.deferred
	p.memo.seen = p.seen
}

// Holds reports whether info is a never-started pending create the memo keeps
// past its lease at now: the last completed run deferred it at a start gate, or
// that run's planning snapshot did not contain it, so it has not yet been
// offered a start. The hold ends startDeferralHoldCeiling after the lease's own
// anchor.
func (m *startDeferralMemo) Holds(info sessionpkg.Info, now time.Time) bool {
	if m == nil || !neverStartedPendingCreate(info) {
		return false
	}
	anchor := pendingCreateLeaseAnchor(info)
	if anchor.IsZero() || now.After(anchor.Add(startDeferralHoldCeiling)) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, deferred := m.committed[info.ID]
	_, seen := m.seen[info.ID]
	return deferred || !seen
}

// neverStartedPendingCreate reports whether info is a pending create that has
// never started: the one predicate the memo's snapshot record and its hold share.
func neverStartedPendingCreate(info sessionpkg.Info) bool {
	return info.PendingCreateClaim && strings.TrimSpace(info.LastWokeAt) == ""
}

// neverStartedPendingCreates returns the ids of the never-started pending
// creates in infos. The map is never nil: an empty one is still an observation.
func neverStartedPendingCreates(infos []sessionpkg.Info) map[string]struct{} {
	seen := make(map[string]struct{})
	for _, info := range infos {
		if neverStartedPendingCreate(info) {
			seen[info.ID] = struct{}{}
		}
	}
	return seen
}

// holdPendingCreate is the reconciler's start-gate hold on a never-started
// pending create whose lease has expired. When the memo holds info, nothing
// refused the create (see startDeferralMemo for the criterion), so the lease
// does not roll it back while the pool drains: the hold is logged to stderr and
// traced, and it returns expired=false and capacityHeld=true. Otherwise,
// including for a nil memo or a row that is not expired, it returns expired
// and capacityHeld as it was given them and writes nothing. A nil trace is
// allowed.
func (m *startDeferralMemo) holdPendingCreate(info sessionpkg.Info, expired, capacityHeld bool, clk clock.Clock, stderr io.Writer, trace *sessionReconcilerTraceCycle, templateName, sessionName string) (bool, bool) {
	if m == nil || !expired {
		return expired, capacityHeld
	}
	now := clk.Now()
	if !m.Holds(info, now) {
		return expired, capacityHeld
	}
	fmt.Fprintf(stderr, "session reconciler: keeping pending create %s past its lease: its start is waiting on a start gate\n", sessionName) //nolint:errcheck
	trace.RecordDecision(TraceSiteReconcilerPendingCreate, TraceReasonStartGateDeferred, TraceOutcomeDeferred, templateName, sessionName, traceRecordPayload{
		"hold":       "start_gate",
		"anchor_age": now.Sub(pendingCreateLeaseAnchor(info)).Truncate(time.Second).String(),
	})
	return false, true
}

// withStartDeferralMemo installs the start-deferral memo for this run. Nil (or
// the option omitted) records nothing.
func withStartDeferralMemo(m *startDeferralMemo) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.deferralMemo = m
	}
}

// withStartDeferralSeen sets the never-started pending creates this run's
// planning snapshot contained. Omitted (nil), the run publishes nothing to the
// memo.
func withStartDeferralSeen(seen map[string]struct{}) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.deferralSeen = seen
	}
}

// startOptions returns the executor options that tie a reconcile run to the
// memo: nothing for a nil memo; a memo-less run when the store read was partial,
// which did not see every pending create and so must not replace the record; and
// otherwise the never-started creates in infos, the run's planning snapshot.
func (m *startDeferralMemo) startOptions(partial bool, infos []sessionpkg.Info) []startExecutionOption {
	if m == nil {
		return nil
	}
	if partial {
		return []startExecutionOption{withStartDeferralMemo(nil)}
	}
	return []startExecutionOption{withStartDeferralSeen(neverStartedPendingCreates(infos))}
}

// pendingCreateLeaseAnchor returns the instant the never-started lease runs
// from: pending_create_started_at when it parses, else the bead's creation
// time. The zero time means no anchor, which the lease treats as expired.
func pendingCreateLeaseAnchor(i sessionpkg.Info) time.Time {
	if started, ok := parseRFC3339Metadata(i.PendingCreateStartedAt); ok {
		return started
	}
	return i.CreatedAt
}

// pendingCreateHold combines the endpoint-capacity hold with the start-deferral
// hold: a pending create is kept past its lease when either holds it. Both
// inputs may be nil.
func pendingCreateHold(endpoint func(sessionpkg.Info) bool, m *startDeferralMemo, clk clock.Clock) func(sessionpkg.Info) bool {
	return func(info sessionpkg.Info) bool {
		if endpoint != nil && endpoint(info) {
			return true
		}
		if m == nil || clk == nil {
			return false
		}
		return m.Holds(info, clk.Now())
	}
}
