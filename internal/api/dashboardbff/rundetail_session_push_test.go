package dashboardbff

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestFoldNextSessionEventRefreshesSessionsAndNotifies proves that a session
// lifecycle event observed in the tail — with NO accompanying bead event, so the
// bead fold does not change and build() does not fire — still (a) expires the
// per-city sessions cache and (b) wakes any detail-stream subscribers (via
// notifySubscribers). That is what lets an idle run's session-link flip
// push over the SSE stream promptly instead of waiting for the next bead event
// or the sessions TTL.
func TestFoldNextSessionEventRefreshesSessionsAndNotifies(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	// A counting supervisor so we can prove the sessions cache was invalidated: a
	// read after the session event must re-hit upstream (the cached entry expired).
	var sessionsHits atomic.Int64
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions") {
			sessionsHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer supervisor.Close()

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()
	tl, _ := p.cityRunTailer("alpha")
	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	ctx := context.Background()

	// Warm the sessions cache (a hit if the eager prime hasn't already), then
	// snapshot the hit count.
	if _, ok := tl.mgr.fetchSessions(ctx, "alpha"); !ok {
		t.Fatal("sessions prime must be available")
	}
	hitsBefore := sessionsHits.Load()

	// Subscribe to the detail stream so we can observe the wakeup a session event
	// triggers, then drain any notify already pending from the cold replay / prime
	// so the next receive is unambiguously the session event's notify.
	sub := tl.subscribe()
	defer tl.unsubscribe(sub)
	select {
	case <-sub.notify:
	default:
	}

	// Append a session lifecycle event ONLY — seq past the fold cursor, no bead
	// change, so proj.Apply ignores it and build() (its subscriber notify) never
	// fires. Only the new session-aware path can react to it.
	appendEvents(t, logPath, events.Event{
		Type:    events.SessionUpdated,
		Seq:     currentLastSeq(tl) + 1,
		Ts:      time.Now(),
		Subject: "alpha__worker-1",
	})

	// The tail folds it: containsSessionEvent → refreshSessionEnrichment →
	// invalidate(sessions) + notifySubscribers → our subscriber wakes.
	select {
	case <-sub.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("session event did not notify detail-stream subscribers within 2s")
	}

	// The cache was invalidated: the next read re-hits upstream.
	if _, ok := tl.mgr.fetchSessions(ctx, "alpha"); !ok {
		t.Fatal("post-event sessions read must be available")
	}
	if got := sessionsHits.Load(); got <= hitsBefore {
		t.Fatalf("sessions upstream hits = %d, want > %d (a session event must invalidate the cache)", got, hitsBefore)
	}
}

// sessionLinkedStepEvent builds a step bead that resolves a session link: an
// in_progress status (→ presentation "active", not pending/ready) plus a
// session_id in metadata. detail()'s session enrichment then joins that id
// against the /v0 sessions read, so the resolved link's sessionName tracks the
// live session's alias — the exact field a session.updated must be able to move.
func sessionLinkedStepEvent(seq uint64, sessionID string) events.Event {
	return beadCreatedEvent(seq, beads.Bead{
		ID:        "run1.1",
		Title:     "preflight",
		Status:    "in_progress",
		Type:      "task",
		ParentID:  "run1",
		Ref:       "mol-adopt-pr-v2.preflight",
		CreatedAt: time.Date(2026, 6, 1, 10, 1, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.kind":         "step",
			"gc.root_bead_id": "run1",
			"gc.step_id":      "preflight",
			"gc.scope_ref":    "demo",
			"session_id":      sessionID,
		},
	})
}

// TestRunDetailStreamPushesFrameOnSessionAliasFlip is the end-to-end proof the
// mechanism test cannot give: a real session change flows all the way to a pushed
// SSE frame. A run resolves a session link (step bead → session_id gc-333573),
// the stateful fake supervisor first reports that session with alias
// "alpha-worker", then flips it to "beta-worker". A session.updated event (NO
// bead event, so the bead fold is unchanged and build() never fires) drives
// foldNext → refreshSessionEnrichment → invalidate(sessions) + notify → each
// subscriber rebuilds detail(), refetches the now-expired sessions, and the
// per-connection byte-dedupe lets the frame through BECAUSE the resolved link's
// sessionName actually moved. This closes the invalidate→refetch→rebuild→
// new-bytes→frame gap the empty-sessions mechanism test leaves open.
func TestRunDetailStreamPushesFrameOnSessionAliasFlip(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond
	defer func(prev time.Duration) { runDetailStreamHeartbeat = prev }(runDetailStreamHeartbeat)
	runDetailStreamHeartbeat = time.Hour // keep heartbeats out of the frame stream

	const sessionID = "gc-333573"
	// flipped=false → alias "alpha-worker"; flipped=true → alias "beta-worker".
	var flipped atomic.Bool
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sessions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		alias := "alpha-worker"
		if flipped.Load() {
			alias = "beta-worker"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"items":[{"id":%q,"alias":%q,"state":"active","running":true}],"total":1}`, sessionID, alias)
	}))
	defer supervisor.Close()

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		sessionLinkedStepEvent(2, sessionID),
	)
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	resp, sc, closeStream := startDetailStream(t, srv)
	defer closeStream()
	_ = resp

	// First frame: the link resolves against the initial "alpha-worker" alias.
	first, ok := readSSEFrame(t, sc)
	if !ok {
		t.Fatal("no first frame")
	}
	if !sessionLinkNameEquals(t, first.data, "alpha-worker") {
		t.Fatalf("first frame session link name != alpha-worker; data=%q", first.data)
	}

	// Flip the supervisor's alias, then land a session.updated ONLY (no bead
	// event). The bead fold does not change, so only the session-aware push path
	// can surface the new alias.
	flipped.Store(true)
	appendEvents(t, logPath, events.Event{
		Type:    events.SessionUpdated,
		Seq:     currentLastSeq(tl(t, p, "alpha")) + 1,
		Ts:      time.Now(),
		Subject: "alpha__" + sessionID,
	})

	// The pushed frame must reflect the moved link name — proving invalidate →
	// refetch → rebuild → new bytes → frame end-to-end, and that byte-dedupe did
	// NOT suppress it (the run's own link genuinely moved). Bounded so a
	// regression (no push) fails promptly here rather than hanging to the test
	// timeout.
	next, ok := readSSEFrameWithin(t, sc, 2*time.Second)
	if !ok {
		t.Fatal("no push frame after session.updated (the session alias flip did not reach the SSE stream)")
	}
	if !sessionLinkNameEquals(t, next.data, "beta-worker") {
		t.Fatalf("push frame session link name != beta-worker (session alias flip did not reach the frame); data=%q", next.data)
	}
	if next.data == first.data {
		t.Fatal("push frame bytes equal the first frame — byte-dedupe should have let the moved link through")
	}
}

// readSSEFrameWithin reads one SSE frame but gives up after d, returning
// (zero,false) on the deadline so a "no push" regression fails fast instead of
// blocking on the scanner until the whole test times out. The reader goroutine
// is abandoned on timeout (the deferred stream close unblocks it), which is
// acceptable in a test.
func readSSEFrameWithin(t *testing.T, sc *bufio.Scanner, d time.Duration) (sseFrame, bool) {
	t.Helper()
	type res struct {
		frame sseFrame
		ok    bool
	}
	ch := make(chan res, 1)
	go func() {
		f, ok := readSSEFrame(t, sc)
		ch <- res{f, ok}
	}()
	select {
	case r := <-ch:
		return r.frame, r.ok
	case <-time.After(d):
		return sseFrame{}, false
	}
}

// tl resolves the started tailer for a city in a test.
func tl(t *testing.T, p *Plane, city string) *cityRunTailer {
	t.Helper()
	tailer, ok := p.cityRunTailer(city)
	if !ok {
		t.Fatalf("no tailer for city %q", city)
	}
	return tailer
}

// sessionLinkNameEquals reports whether the run detail in data carries an
// attached session link whose sessionName equals want on any execution instance.
func sessionLinkNameEquals(t *testing.T, data, want string) bool {
	t.Helper()
	var detail struct {
		Nodes []struct {
			ExecutionInstances []struct {
				Session struct {
					Kind string `json:"kind"`
					Link struct {
						SessionName string `json:"sessionName"`
					} `json:"link"`
				} `json:"session"`
			} `json:"executionInstances"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(data), &detail); err != nil {
		t.Fatalf("decode detail frame: %v; data=%q", err, data)
	}
	for _, n := range detail.Nodes {
		for _, inst := range n.ExecutionInstances {
			if inst.Session.Kind == "attached" && inst.Session.Link.SessionName == want {
				return true
			}
		}
	}
	return false
}
