package main

import (
	"context"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// An open soft reload under v2 (CONTRACT v5 M2 step 2, MAINT-025): reloadV2
// opens it for the env generation the reload published, A8 accepts each
// drifted row by CAS, and the reply waits for every drifted row to be
// attempted, at most softReloadReplyBound; the request stays open past the
// reply until it is done. A newer request, or a pass under a newer
// generation, closes it.

const (
	// softReloadReplyBound bounds only the reload's reply.
	softReloadReplyBound = 30 * time.Second
	// softReloadRetries bounds a row's transient refusals (premise, CAS,
	// redecided, name-busy) before it counts failed; each retry waits out
	// the row's backoff.
	softReloadRetries = 3
)

// softReloadSeq numbers requests, so an acceptance settles only its own.
var softReloadSeq atomic.Uint64

// softReloadRequest is one soft reload's acceptance.
type softReloadRequest struct {
	gen    uint64
	reason string        // its acceptances' decide reason, which carries its number
	done   chan struct{} // closed once every drifted row was attempted, or the request closed

	mu        sync.Mutex
	closed    bool
	attempted map[rowKey]bool
	retried   map[rowKey]int
	result    softReloadAcceptanceResult
}

func newSoftReloadRequest(gen uint64) *softReloadRequest {
	return &softReloadRequest{
		gen: gen, reason: decideBaselineSoftReload + "#" + strconv.FormatUint(softReloadSeq.Add(1), 10),
		done: make(chan struct{}), attempted: make(map[rowKey]bool), retried: make(map[rowKey]int),
	}
}

// softReloadView is what a pass reads of an open request: the request
// itself, which only it closes, and the rows already attempted.
type softReloadView struct {
	Req       *softReloadRequest
	Gen       uint64
	Reason    string
	Attempted map[rowKey]bool
}

// accepts reports whether row k is still to be accepted under gen.
func (v *softReloadView) accepts(k rowKey, gen uint64) bool {
	return v != nil && v.Gen == gen && !v.Attempted[k]
}

// view is q's state for a pass; nil when q is nil or closed.
func (q *softReloadRequest) view() *softReloadView {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	return &softReloadView{Req: q, Gen: q.gen, Reason: q.reason, Attempted: maps.Clone(q.attempted)}
}

// settle records s, one of q's own acceptances: accepted when it landed;
// a transient refusal retries, up to softReloadRetries; anything else
// counts failed (by row ID).
func (q *softReloadRequest) settle(s settlement) {
	if q == nil || s.Kind != intentBaseline || s.Reason != q.reason {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.attempted[s.Key] {
		return
	}
	transient := s.Outcome == settledRefused && (s.Cause == causePremise || s.Cause == causeCAS || s.Cause == causeRedecided || s.Cause == causeNameBusy)
	if transient && q.retried[s.Key] < softReloadRetries-1 {
		q.retried[s.Key]++
		return
	}
	q.attempted[s.Key] = true
	if s.Outcome == settledLanded {
		q.result.Updated++
		return
	}
	q.result.Failed++
	q.result.FailedSessions = append(q.result.FailedSessions, s.Key.ID)
}

// close ends q with the city's open and desired counts for its warnings.
func (q *softReloadRequest) close(open int, desiredEmpty bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		q.result.OpenSessions, q.result.DesiredEmpty = open, desiredEmpty
		close(q.done)
	}
}

// reply amends r as legacy's applySoftReloadAcceptance does.
func (q *softReloadRequest) reply(r *reloadControlReply) {
	q.mu.Lock()
	defer q.mu.Unlock()
	accepted := q.result.Updated
	r.AcceptedDriftCount = &accepted
	r.Warnings = append(r.Warnings, q.result.warnings()...)
}

// closeSoftReload closes the request this pass read, and only it: at once
// under a newer generation; never from a pass under an older one, which
// read its env before the reload published; otherwise once a complete
// census owes no row an acceptance (a row in flight, or decided by an
// earlier arm, still is). Its open count is the sessions leg's open rows,
// as legacy counts them.
func closeSoftReload(w *World, a *allocDecision) {
	v := w.SoftReload
	if v == nil {
		return
	}
	if w.Env.Gen > v.Gen {
		v.Req.close(0, false)
		return
	}
	if _, complete := w.Census.complete(); !complete || w.Env.Gen < v.Gen {
		return
	}
	desired, open := false, 0
	for k, e := range a.Snapshot.Entries {
		desired = desired || e.InDesired
		r := &rowFacts{w: w, k: k, entry: e}
		if r.row, r.found = w.Census.Rows[k]; !r.found {
			continue
		}
		if len(w.Census.Legs) > 0 && k.Leg == w.Census.Legs[0].Ref {
			open++
		}
		if _, core, ok := w.coreHash(r.row.Info); ok && r.owesSoftReload(strings.TrimSpace(r.row.Info.StartedConfigHash), core) {
			return
		}
	}
	v.Req.close(open, !desired)
}

// openSoftReload makes q the planner's open request, closing an older one.
func (p *planner) openSoftReload(q *softReloadRequest) {
	if old := p.softReload.Swap(q); old != nil {
		old.close(0, false)
	}
	p.markDirty("soft-reload")
}

// acceptSoftReload is the reload's soft-reload hook under v2: it opens a
// request for the published env's generation and amends r once every
// drifted row was attempted, at the bound, or when ctx ends or the planner
// stops.
func (rt *plannerRuntime) acceptSoftReload(ctx context.Context, intent reloadIntent, r *reloadControlReply) {
	env := rt.env.Load()
	switch {
	case !intent.Soft || env == nil:
		return
	case rt.planner.effects == nil:
		v2SoftReloadUnavailable(intent, r)
		return
	}
	q := newSoftReloadRequest(env.Gen)
	rt.planner.openSoftReload(q)
	t := rt.planner.clock.NewTimer(softReloadReplyBound)
	defer t.Stop()
	select {
	case <-q.done:
	case <-t.C():
	case <-ctx.Done():
	case <-rt.planner.done:
	}
	q.reply(r)
}
