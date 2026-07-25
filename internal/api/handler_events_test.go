package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestEventList(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker", Subject: "gc-1"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2", resp.Total)
	}
}

func TestEventListFilterByType(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?type=bead.created"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Items []events.Event `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1", resp.Total)
	}
}

func TestEventListIncludesCustomEventTypes(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: "custom.untyped", Actor: "tester", Payload: json.RawMessage(`{"source":"test"}`)})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("response = %+v, want custom and registered events", resp)
	}
	custom := eventListItemByType(t, resp.Items, "custom.untyped")
	payload := assertJSONPayloadObject(t, custom["payload"])
	if payload["source"] != "test" {
		t.Fatalf("custom payload = %v, want source=test", payload)
	}
}

func TestEventListRejectsInvalidSince(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?since=notaduration"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid since duration") {
		t.Fatalf("body = %q, want invalid since duration", rec.Body.String())
	}
}

func TestEventListRejectsInvalidAfterSeq(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?after_seq=notanumber"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "after_seq must be a non-negative integer") {
		t.Fatalf("body = %q, want after_seq validation message", rec.Body.String())
	}
}

func TestEventListRejectsNegativeAfterSeq(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?after_seq=-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "after_seq must be a non-negative integer") {
		t.Fatalf("body = %q, want after_seq validation message", rec.Body.String())
	}
}

// TestEventListAfterSeqSkipsArchive proves the after_seq skip-fast path
// (archiveOverlapsFilter in internal/events/rotation_archive.go) is actually
// reached from the HTTP layer, not just internally. It corrupts an archive's
// on-disk bytes so opening it always errors, then shows a request whose
// after_seq excludes that archive still succeeds (never opened) while a
// request that doesn't exclude it fails (the gunzip error surfaces as a
// 500). Divergent status codes are a deterministic proxy for "was this
// archive opened" -- a corrupted archive can only yield 200 if it was
// genuinely skipped, which is a stronger proof than counting calls.
func TestEventListAfterSeqSkipsArchive(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// Archive: seq 1-3.
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "a1"})
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "a2"})
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "a3"})
	result, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !result.Rotated || result.ArchivePath == "" {
		t.Fatalf("ForceRotate result = %+v, want rotated with archive path", result)
	}
	rec.WaitForRotations()
	if result.LastSeq != 3 {
		t.Fatalf("archive LastSeq = %d, want 3", result.LastSeq)
	}

	// Active file now holds the rotation anchor (seq 4) plus two more events
	// (seq 5, 6) -- 3 events total, well under limit+1 below, so
	// fetchEventPageAscending's ListTail fast path can never be trusted and
	// every request below falls through to the archive-aware full scan.
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "b1"})
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "b2"})

	// Corrupt the archive on disk: any attempt to gzip.NewReader it errors.
	if err := os.WriteFile(result.ArchivePath, []byte("not a gzip file"), 0o644); err != nil {
		t.Fatalf("corrupting archive: %v", err)
	}

	state.eventProv = rec
	h := newTestCityHandler(t, state)

	// after_seq=3 excludes the archive (LastSeq=3 <= 3): must skip it and
	// succeed even though its bytes are garbage.
	skipReq := httptest.NewRequest("GET", cityURL(state, "/events?after_seq=3&limit=100"), nil)
	skipRec := httptest.NewRecorder()
	h.ServeHTTP(skipRec, skipReq)
	if skipRec.Code != http.StatusOK {
		t.Fatalf("after_seq=3 status = %d, want %d (archive should have been skipped); body: %s",
			skipRec.Code, http.StatusOK, skipRec.Body.String())
	}

	// No after_seq: the archive is in range and must be opened, so the
	// corruption surfaces as a 500 -- proving the skip above was a real
	// skip, not an artifact of some other short-circuit.
	controlReq := httptest.NewRequest("GET", cityURL(state, "/events?limit=100"), nil)
	controlRec := httptest.NewRecorder()
	h.ServeHTTP(controlRec, controlReq)
	if controlRec.Code != http.StatusInternalServerError {
		t.Fatalf("no after_seq status = %d, want %d (archive should have been opened and failed to gunzip); body: %s",
			controlRec.Code, http.StatusInternalServerError, controlRec.Body.String())
	}
}

// TestEventListOmittedAfterSeqIncludesArchive is the positive-data-flow
// companion to TestEventListAfterSeqSkipsArchive: confirms that omitting
// after_seq is unchanged -- archived events still flow into the response
// with a correct total, not just "doesn't error".
func TestEventListOmittedAfterSeqIncludesArchive(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "archived-1"})
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "archived-2"})
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	rec.WaitForRotations()
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "active-1"})

	state.eventProv = rec
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?limit=100"), nil)
	respRec := httptest.NewRecorder()
	h.ServeHTTP(respRec, req)
	if respRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", respRec.Code, http.StatusOK, respRec.Body.String())
	}

	items, total, _ := decodeEventList(t, respRec)
	// 2 archived + 1 rotation anchor + 1 active = 4 events total.
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if len(items) != 4 {
		t.Fatalf("items len = %d, want 4", len(items))
	}
	if items[len(items)-1].Subject != "archived-1" {
		t.Fatalf("oldest item subject = %q, want archived-1 (archive data must reach the response)", items[len(items)-1].Subject)
	}
}

func TestEventRotateFileRecorderGoldenPath(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	state.eventProv = rec
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec.Record(events.Event{Type: events.BeadCreated, Actor: "worker"})
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/events/rotate"), nil)
	httpRec := httptest.NewRecorder()
	h.ServeHTTP(httpRec, req)

	if httpRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", httpRec.Code, http.StatusOK, httpRec.Body.String())
	}
	var resp EventRotateResponse
	if err := json.NewDecoder(httpRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Rotated {
		t.Fatalf("rotated = false, want true; resp=%+v", resp)
	}
	if resp.Archive == nil {
		t.Fatalf("archive = nil, want metadata; resp=%+v", resp)
	}
	if resp.Archive.FirstSeq != 1 || resp.Archive.LastSeq != 2 {
		t.Fatalf("archive seq = %d-%d, want 1-2", resp.Archive.FirstSeq, resp.Archive.LastSeq)
	}
	if resp.Archive.CompressionStatus != "pending" {
		t.Fatalf("compression_status = %q, want pending", resp.Archive.CompressionStatus)
	}
	if resp.AnchorEvent == nil || resp.AnchorEvent.Seq != 3 || resp.AnchorEvent.Type != events.EventsRotated {
		t.Fatalf("anchor_event = %+v, want seq=3 type=%s", resp.AnchorEvent, events.EventsRotated)
	}
}

func TestEventRotateEmptyActiveLogIsNoOp(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	state.eventProv = rec
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/events/rotate"), nil)
	httpRec := httptest.NewRecorder()
	h.ServeHTTP(httpRec, req)

	if httpRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", httpRec.Code, http.StatusOK, httpRec.Body.String())
	}
	var resp EventRotateResponse
	if err := json.NewDecoder(httpRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Rotated || resp.Reason != "active log is empty" {
		t.Fatalf("response = %+v, want rotated=false reason", resp)
	}
}

func TestEventRotateUnsupportedProviderReturnsMethodNotAllowed(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Events.Provider = "exec:my-script"
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/events/rotate"), nil)
	httpRec := httptest.NewRecorder()
	h.ServeHTTP(httpRec, req)

	if httpRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body: %s", httpRec.Code, http.StatusMethodNotAllowed, httpRec.Body.String())
	}
	want := "rotation is only supported for the file-backed events provider; current provider is 'exec:my-script'"
	if !strings.Contains(httpRec.Body.String(), want) {
		t.Fatalf("body = %q, want %q", httpRec.Body.String(), want)
	}
}

func TestEventRotateWaitReturnsCompleteCompressionStatus(t *testing.T) {
	state := newFakeState(t)
	var stderr strings.Builder
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), &stderr)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	state.eventProv = rec
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	h := newTestCityHandler(t, state)

	req := newPostRequest(cityURL(state, "/events/rotate?wait=true"), nil)
	httpRec := httptest.NewRecorder()
	h.ServeHTTP(httpRec, req)

	if httpRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", httpRec.Code, http.StatusOK, httpRec.Body.String())
	}
	var resp EventRotateResponse
	if err := json.NewDecoder(httpRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Archive == nil || resp.Archive.CompressionStatus != "complete" {
		t.Fatalf("archive = %+v, want compression_status=complete", resp.Archive)
	}
}

func TestEventStream(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	h := newTestCityHandler(t, state)

	// Create a context with timeout to avoid hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run the handler in a goroutine since it blocks.
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the handler time to set up.
	time.Sleep(50 * time.Millisecond)

	// Record an event.
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "worker"})

	// Wait for event to be delivered or timeout.
	time.Sleep(100 * time.Millisecond)
	cancel() // Stop the stream.
	<-done

	body := rec.Body.String()
	// Event name is now "event" (documented in OpenAPI spec via sse.Register).
	// The actual event type is in the JSON body's "type" field.
	if !strings.Contains(body, "event: event") {
		t.Errorf("SSE body missing event: event line, got: %s", body)
	}
	if !strings.Contains(body, `"type":"session.woke"`) {
		t.Errorf("SSE body missing event type in JSON body, got: %s", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Errorf("SSE body missing event id, got: %s", body)
	}

	// Check SSE headers.
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
}

func TestEventStreamCommitsHeadersBeforeFirstEvent(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	server := httptest.NewServer(h)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+cityURL(state, "/events/stream"), nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events stream before first event: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestEventStreamProjectsWorkflowMetadata(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	root, err := store.Create(beads.Bead{
		Title: "Workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_123",
			"gc.scope_kind":  "city",
			"gc.scope_ref":   "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	payload, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal root: %v", err)
	}

	h := newTestCityHandler(t, state)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.BeadUpdated,
		Actor:   "worker",
		Subject: root.ID,
		Payload: payload,
	})

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, `"workflow":{"type":"workflow:event"`) {
		t.Fatalf("SSE body missing workflow projection: %s", body)
	}
	if !strings.Contains(body, `"workflow_id":"wf_123"`) {
		t.Fatalf("SSE body missing workflow id: %s", body)
	}
	if !strings.Contains(body, `"scope_kind":"city"`) {
		t.Fatalf("SSE body missing logical scope: %s", body)
	}
}

func TestWatcherCloseUnblocksNext(t *testing.T) {
	ep := events.NewFake()
	watcher, err := ep.Watch(context.Background(), 0)
	if err != nil {
		t.Fatalf("Watch() error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := watcher.Next()
		done <- err
	}()

	// Give Next time to block.
	time.Sleep(50 * time.Millisecond)

	// Close should unblock the blocked Next call.
	if err := watcher.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("Next() returned nil error after Close(); expected error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next() did not unblock after Close() — goroutine leak")
	}
}

func TestEventStreamNoEvents(t *testing.T) {
	state := newFakeState(t)
	state.eventProv = nil
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events/stream"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleEventEmit(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"type":"deploy.completed","actor":"ci","subject":"myapp","message":"v2.3.1"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	ep := state.eventProv.(*events.Fake)
	evts, err := ep.List(events.Filter{Type: "deploy.completed"})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Actor != "ci" {
		t.Errorf("actor = %q, want %q", evts[0].Actor, "ci")
	}
	if evts[0].Subject != "myapp" {
		t.Errorf("subject = %q, want %q", evts[0].Subject, "myapp")
	}
}

func TestHandleEventEmit_MissingType(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"actor":"ci"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleEventEmit_MissingActor(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"type":"test.event"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleEventEmit_NoEventsProvider(t *testing.T) {
	state := newFakeState(t)
	state.eventProv = nil
	h := newTestCityHandler(t, state)

	body := `{"type":"test.event","actor":"ci"}`
	req := newPostRequest(cityURL(state, "/events"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestEventListLimitReturnsTail is a regression test for the perf bug where
// the city events handler ignored the query limit and parsed the entire
// events.jsonl on every request (O(file size), ~4s on a 100 MB log).
// The fix routes limit=N through the TailProvider to return the N newest
// events without scanning history.
func TestEventListLimitReturnsTail(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "old"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "worker", Subject: "middle"})
	ep.Record(events.Event{Type: events.SessionStopped, Actor: "gc", Subject: "new"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?limit=1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if got := resp.Items[0]["subject"]; got != "new" {
		t.Fatalf("subject = %v, want \"new\" (newest event); tail semantics regressed", got)
	}
	// Total should report the full match count, not the page size, so
	// callers can tell "server has N events" from "limit truncated".
	if resp.Total != 3 {
		t.Fatalf("total = %d, want 3", resp.Total)
	}
}

// TestEventListLimitWithTypeFilterReturnsTail verifies the TailProvider
// path still yields newest-first semantics when a type filter narrows
// the result set. Total is best-effort (= returned count) because we
// can't cheaply compute the filtered match count.
func TestEventListLimitWithTypeFilterReturnsTail(t *testing.T) {
	state := newFakeState(t)
	ep := state.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "woke-old"})
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "gc", Subject: "ignored"})
	ep.Record(events.Event{Type: events.SessionWoke, Actor: "gc", Subject: "woke-new"})
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/events?type=session.woke&limit=1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(resp.Items))
	}
	if got := resp.Items[0]["subject"]; got != "woke-new" {
		t.Fatalf("subject = %v, want \"woke-new\" (newest matching); tail regressed", got)
	}
}
