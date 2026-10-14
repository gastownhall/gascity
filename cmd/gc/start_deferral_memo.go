package main

import (
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
// is rolled back as before.
const startDeferralHoldCeiling = 60 * time.Minute

// startDeferralMemo remembers which never-started pending creates the last
// start-executor run deferred at a start gate (the per-tick wake budget or the
// async start limiter). A create waiting only on such a gate has not been
// refused anything, so the 10-minute never-started lease must not roll it back
// while the pool drains at max_wakes_per_tick. The memo is process-local and
// in-memory by design: it holds no bead state, and a restart forgets it, which
// restores the lease's ordinary rollback.
//
// A nil memo records and holds nothing.
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

// beginPass opens a run that started at start. A nil memo returns a nil pass.
func (m *startDeferralMemo) beginPass(start time.Time) *startDeferralPass {
	if m == nil {
		return nil
	}
	return &startDeferralPass{memo: m, start: start, deferred: make(map[string]struct{})}
}

// noteGateDeferred records that the run deferred session id at a start gate.
func (p *startDeferralPass) noteGateDeferred(id string) {
	if p == nil || id == "" {
		return
	}
	p.deferred[id] = struct{}{}
}

// finish publishes the run to the memo, replacing the previous record. A
// canceled run did not offer every candidate a start, so it commits nothing
// and leaves the previous record alone.
func (p *startDeferralPass) finish(canceled bool) {
	if p == nil || canceled {
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
