package main

import (
	"context"
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
// start-executor run deferred at a start gate. A site is a start gate, and
// records here, only when it declines a ready candidate for a reason
// independent of that candidate's own state, refuses it nothing (it is
// re-offered next pass), and writes nothing to any bead. A gate with its own
// hold, such as endpoint capacity, does not record. The classification of
// every start-op outcome is pinned in start_gate_classification_test.go.
// A create waiting only on such a gate has not been refused anything, so the
// 10-minute never-started lease must not roll it back while the pool drains
// at max_wakes_per_tick. The memo is process-local and
// in-memory by design: it holds no bead state, and a restart forgets it, which
// restores the lease's ordinary rollback.
//
// A nil memo records and holds nothing. One memo serves one start population:
// every completed run replaces its record, so only the main reconcile tick
// records into it (see CityRuntime.ensureStartDeferralMemo).
type startDeferralMemo struct {
	mu            sync.Mutex
	committed     map[string]struct{}
	lastPassStart time.Time
}

// startDeferralPass accumulates one executor run's start-gate deferrals and
// publishes them to the memo only when the run completes uncanceled.
type startDeferralPass struct {
	memo     *startDeferralMemo
	start    time.Time
	deferred map[string]struct{}
}

// newStartDeferralMemo returns an empty memo.
func newStartDeferralMemo() *startDeferralMemo { return &startDeferralMemo{} }

// beginPass opens a run that starts now, by clk. A nil memo returns a nil pass
// without reading the clock.
func (m *startDeferralMemo) beginPass(clk clock.Clock) *startDeferralPass {
	if m == nil {
		return nil
	}
	return &startDeferralPass{memo: m, start: clk.Now(), deferred: make(map[string]struct{})}
}

// noteGateDeferred records that the run deferred session id at a start gate.
func (p *startDeferralPass) noteGateDeferred(id string) {
	if p == nil || id == "" {
		return
	}
	p.deferred[id] = struct{}{}
}

// noteStartOutcome records session id when outcome is the async start limiter
// refusing it a slot.
func (p *startDeferralPass) noteStartOutcome(_, _ string) {}

// finish publishes the run to the memo, replacing the previous record. A run
// whose ctx is canceled by now did not offer every candidate a start, so it
// commits nothing and leaves the previous record alone. A nil ctx is never
// canceled.
func (p *startDeferralPass) finish(ctx context.Context) {
	if p == nil || (ctx != nil && ctx.Err() != nil) {
		return
	}
	p.memo.mu.Lock()
	defer p.memo.mu.Unlock()
	p.memo.committed = p.deferred
	p.memo.lastPassStart = p.start
}

// Holds reports whether info is a never-started pending create the memo keeps
// past its lease at now: the last completed run deferred it at a start gate, or
// it first appeared after that run began and so has not yet been offered a
// start. The hold ends startDeferralHoldCeiling after the lease's own anchor.
func (m *startDeferralMemo) Holds(info sessionpkg.Info, now time.Time) bool {
	if m == nil || !info.PendingCreateClaim || strings.TrimSpace(info.LastWokeAt) != "" {
		return false
	}
	anchor := pendingCreateLeaseAnchor(info)
	if anchor.IsZero() || now.After(anchor.Add(startDeferralHoldCeiling)) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.committed[info.ID]; ok {
		return true
	}
	born := info.CreatedAt
	if started, ok := parseRFC3339Metadata(info.PendingCreateStartedAt); ok && started.After(born) {
		born = started
	}
	return born.After(m.lastPassStart)
}

// holdPendingCreate keeps a never-started pending create past its lease when
// the memo holds it.
func (m *startDeferralMemo) holdPendingCreate(_ sessionpkg.Info, expired, capacityHeld bool, _ clock.Clock, _ io.Writer, _ *sessionReconcilerTraceCycle, _, _ string) (bool, bool) {
	return expired, capacityHeld
}

// withStartDeferralMemo installs the start-deferral memo for this run. Nil (or
// the option omitted) records nothing.
func withStartDeferralMemo(m *startDeferralMemo) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.deferralMemo = m
	}
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
