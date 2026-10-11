package main

import (
	"maps"
	"strings"
	"sync"
	"time"
)

// An open soft reload under v2 (CONTRACT v5 M2 step 2, MAINT-025): reloadV2
// opens it for the env generation the reload published, A8 accepts each
// drifted row by CAS, and the reply waits for every drifted row to be
// attempted, at most softReloadReplyBound; the request stays open past the
// reply until it is done. A newer request, or a pass under a newer
// generation, closes it.

// softReloadReplyBound bounds only the reload's reply.
const softReloadReplyBound = 30 * time.Second

// softReloadRequest is one soft reload's acceptance.
type softReloadRequest struct {
	gen  uint64
	done chan struct{} // closed once every drifted row was attempted, or the request closed

	mu        sync.Mutex
	closed    bool
	attempted map[rowKey]bool
	result    softReloadAcceptanceResult
}

func newSoftReloadRequest(gen uint64) *softReloadRequest {
	return &softReloadRequest{gen: gen, done: make(chan struct{}), attempted: make(map[rowKey]bool)}
}

// softReloadView is what a pass reads of an open request.
type softReloadView struct {
	Gen       uint64
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
	return &softReloadView{Gen: q.gen, Attempted: maps.Clone(q.attempted)}
}

// settle records s, an A8 acceptance, as attempted: accepted when it
// landed, failed (by row ID) otherwise.
func (q *softReloadRequest) settle(s settlement) {
	if q == nil || s.Kind != intentBaseline || s.Reason != decideBaselineSoftReload {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.attempted[s.Key] {
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

// closeSoftReload closes the open request this pass read once no row is
// owed an acceptance (a row in flight, or decided by an earlier arm, still
// is). A pass under a newer generation closes it at once.
func (p *planner) closeSoftReload(w *World, a *allocDecision) {
	q := p.softReload.Load()
	if q == nil || w.SoftReload == nil {
		return
	}
	desired := false
	for k, e := range a.Snapshot.Entries {
		desired = desired || e.InDesired
		row, ok := w.Census.Rows[k]
		if !ok || w.SoftReload.Gen != w.Env.Gen {
			continue
		}
		if _, core, ok := w.coreHash(row.Info); ok && w.owesSoftReload(k, e, strings.TrimSpace(row.Info.StartedConfigHash), core) {
			return
		}
	}
	q.close(len(w.Census.Rows), !desired)
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
// drifted row was attempted, or at the bound.
func (rt *plannerRuntime) acceptSoftReload(intent reloadIntent, r *reloadControlReply) {
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
	}
	q.reply(r)
}
