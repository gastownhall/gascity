package dashboardbff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
)

// runDetailStreamHeartbeat is how often an idle detail stream emits a comment
// frame so a proxy or the browser does not reap an otherwise-silent connection.
// A var (not a const) so tests can shorten it.
var runDetailStreamHeartbeat = 25 * time.Second

// runDetailStreamAfterPrecheck is a test-only seam invoked once, after the
// precheck detail() read and BEFORE the subscribe + first-frame re-read. It lets
// a test deterministically stage a fold that publishes in the
// [precheck → connect] window — the exact race the post-subscribe re-read fixes
// — so it can assert the FIRST frame reflects the newer generation. Nil (a
// no-op) in production.
var runDetailStreamAfterPrecheck func()

// detailStreamSub is one detail-stream connection's wakeup channel. The channel
// is buffered with capacity 1 and coalescing: a full buffer means a rebuild is
// already pending for this subscriber, so build()'s non-blocking send is a
// harmless no-op. The handler goroutine is the sole receiver.
type detailStreamSub struct {
	notify chan struct{}
}

// subscribe registers a new detail-stream subscriber and returns it. The caller
// MUST call unsubscribe when the connection ends (deferred in the handler) so
// the registry never leaks a channel for a gone connection.
func (t *cityRunTailer) subscribe() *detailStreamSub {
	sub := &detailStreamSub{notify: make(chan struct{}, 1)}
	t.subMu.Lock()
	if t.subs == nil {
		t.subs = make(map[*detailStreamSub]struct{})
	}
	t.subs[sub] = struct{}{}
	t.subMu.Unlock()
	return sub
}

// unsubscribe removes a subscriber from the registry. Idempotent: a double call
// (e.g. an early-return path plus the deferred cleanup) is safe.
func (t *cityRunTailer) unsubscribe(sub *detailStreamSub) {
	t.subMu.Lock()
	delete(t.subs, sub)
	t.subMu.Unlock()
}

// subscriberCount reports the number of live detail-stream subscribers. It backs
// the goroutine-leak test (the count must return to zero after every connection
// closes) and carries no production behavior.
func (t *cityRunTailer) subscriberCount() int {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	return len(t.subs)
}

// notifySubscribers wakes every detail-stream subscriber with a NON-BLOCKING
// send: a subscriber whose buffer is already full has a rebuild pending, so
// dropping the extra wakeup coalesces bursts without ever blocking the
// fold-publish path. The lock is held only to walk the registry (never across a
// network write). Called from build().
func (t *cityRunTailer) notifySubscribers() {
	t.subMu.Lock()
	for sub := range t.subs {
		select {
		case sub.notify <- struct{}{}:
		default:
		}
	}
	t.subMu.Unlock()
}

// registerRunDetailStream wires GET /api/city/{cityName}/runs/{runId}/detail/stream:
// a per-run Server-Sent-Events stream that pushes the whole FormulaRunDetail as a
// snapshot frame on connect and again whenever the fold changes this run's bytes.
// It is a plain mux route on the sanctioned non-Huma plane (GET, so it passes the
// mutation guard) — deliberately NOT the typed /v0 event stream (which pays a
// per-event beads-DB Get) and NOT a bus event type (which would persist every
// frame into events.jsonl). The frame body is the same struct the GET serves, so
// the client renders a pushed frame with zero refetch.
func (p *Plane) registerRunDetailStream() {
	p.mux.HandleFunc("GET /api/city/{cityName}/runs/{runId}/detail/stream", p.handleRunDetailStream)
}

// handleRunDetailStream serves one detail-stream connection. It prechecks the run
// exactly like the GET so a 422/404/503 is returned BEFORE any SSE body is
// committed, commits the event-stream headers, then hands off to
// serveRunDetailStream for the subscribe + first-frame + push loop.
func (p *Plane) handleRunDetailStream(w http.ResponseWriter, r *http.Request) {
	t, ok := p.cityRunTailer(r.PathValue("cityName"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown city")
		return
	}
	runID := r.PathValue("runId")

	flusher, ok := w.(http.Flusher)
	if !ok {
		// A ResponseWriter that cannot flush cannot stream; fail closed so the
		// SPA falls back to the GET + nudge path rather than hanging on a stream
		// that never delivers a frame.
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Precheck exactly like the GET so the HTTP error is returned BEFORE any SSE
	// body is committed: 422 for an unsupported (v1/wisp) run, 404 for a missing
	// run once warm, 503 while the projection is still warming.
	value, ready, err := t.detail(r.Context(), runID)
	if err != nil {
		writeRunDetailStreamPrecheckError(w, err, ready)
		return
	}

	writeRunDetailStreamHeaders(w)

	// Test seam: stage a fold that publishes in the [precheck → connect] window.
	if runDetailStreamAfterPrecheck != nil {
		runDetailStreamAfterPrecheck()
	}

	t.serveRunDetailStream(r.Context(), w, flusher, runID, value)
}

// writeRunDetailStreamPrecheckError maps a failed precheck detail() read to the
// HTTP status the SPA's stream fallback expects, returned before any SSE body is
// committed: 422 for an unsupported (v1/wisp) run, 503 while the projection is
// still warming, 404 for a run absent once warm.
func writeRunDetailStreamPrecheckError(w http.ResponseWriter, err error, ready bool) {
	var unsupported *runproj.UnsupportedRunError
	if errors.As(err, &unsupported) {
		writeJSON(w, http.StatusUnprocessableEntity, runDetailErrorBody{
			Error:  unsupported.Message,
			Reason: string(unsupported.Reason),
		})
		return
	}
	if !ready {
		writeError(w, http.StatusServiceUnavailable, "run view is warming")
		return
	}
	writeError(w, http.StatusNotFound, "unknown run")
}

// writeRunDetailStreamHeaders commits the SSE response headers and the 200 status.
// After this the response is an event-stream body, so every later failure is
// surfaced by a failed frame write rather than an HTTP status.
func writeRunDetailStreamHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering (nginx and friends) so frames flush end-to-end.
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
}

// serveRunDetailStream registers this connection as a subscriber, sends the
// current fold as the first frame, then pushes a new frame on every real change
// until the client disconnects. precheckValue is the already-validated precheck
// read, used only as the first-frame fallback when the post-subscribe re-read
// transiently fails.
//
// Register BEFORE re-reading the current fold, then send THAT re-read as the
// first frame — not the precheck value. A build() landing in the
// [precheck → subscribe] window publishes but reaches no subscriber, so a first
// frame built from the precheck value would pin this connection on a stale fold
// until the NEXT build (which may never come for a run that goes idle).
// Registering first closes that window: the buffered(1) notify is
// level-triggered, so a build in the narrower [subscribe → re-read] sub-window
// fills the buffer, the loop's first select drains it, and byte-dedupe collapses
// the redundant wakeup if the re-read already captured it. The precheck read is
// kept only for the 422/404/503 HTTP decision in the caller.
func (t *cityRunTailer) serveRunDetailStream(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	runID string,
	precheckValue runDetailMemoValue,
) {
	sub := t.subscribe()
	defer t.unsubscribe(sub)

	current, _, rebuildErr := t.detail(ctx, runID)
	if rebuildErr != nil {
		// The precheck already proved the run exists and is renderable; a
		// transient re-read failure just falls back to that value.
		current = precheckValue
	}
	lastSent := writeDetailFrame(w, flusher, current)

	heartbeat := time.NewTicker(runDetailStreamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// A comment frame keeps the connection warm without perturbing the
			// client's rendered detail.
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-sub.notify:
			rebuilt, _, rebuildErr := t.detail(ctx, runID)
			if rebuildErr != nil {
				// A run that vanished from the fold (rotated out) or a transient
				// projection error: keep the connection open on the last good frame
				// rather than tearing down — a later fold may resurface it, and the
				// client already has a coherent snapshot.
				continue
			}
			// Byte-dedupe: only push when THIS run's marshaled bytes actually
			// changed. An unrelated-run event published a new fold and woke us, but
			// if the run's own detail is identical we send nothing.
			if bytes.Equal(lastSent, rebuilt.bytes) {
				continue
			}
			lastSent = writeDetailFrame(w, flusher, rebuilt)
		}
	}
}

// writeDetailFrame writes one `id: / event: detail / data:` SSE frame carrying
// the memoized detail bytes and flushes. It returns the bytes written so the
// caller can byte-dedupe the next frame against them. A write error is ignored
// here — the next loop iteration's ctx.Done or a subsequent write surfaces the
// dead connection and returns.
func writeDetailFrame(w http.ResponseWriter, flusher http.Flusher, value runDetailMemoValue) []byte {
	// The frame id is the snapshot's event seq (the tailer's publish cursor),
	// which the detail DTO also carries as snapshotEventSeq — so a reconnect's
	// Last-Event-ID (if a proxy replays one) is meaningful, though the stream
	// needs no replay because every frame is a whole snapshot.
	// A write error here is not actionable — the next loop iteration's ctx.Done
	// (client disconnect) or a subsequent failed write tears the connection down.
	_, _ = fmt.Fprintf(w, "id: %d\nevent: detail\ndata: %s\n\n", frameSeq(value.detail), value.bytes)
	flusher.Flush()
	return value.bytes
}

// frameSeq returns the snapshot event seq to stamp as the SSE frame id, or 0
// when the snapshot carries no known seq.
func frameSeq(detail runproj.FormulaRunDetail) int64 {
	if detail.SnapshotEventSeq.Kind == "known" {
		return detail.SnapshotEventSeq.Seq
	}
	return 0
}
