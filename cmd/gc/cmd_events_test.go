package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
)

func TestDoEventsCityDefaultUsesJSONLItems(t *testing.T) {
	items := []genclient.Event{
		{Actor: "human", Seq: 1, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", Seq: 2, Subject: stringPtr("mayor"), Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, genclient.ListBodyEvent{Items: &items, Total: int64(len(items))})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d JSONL lines, want 2; output=%q", len(lines), stdout.String())
	}
	var got []genclient.Event
	for _, line := range lines {
		var item genclient.Event
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("unmarshal line: %v; line=%q", err, line)
		}
		got = append(got, item)
	}
	if got[0].Type != "bead.created" || got[1].Type != "session.woke" {
		t.Fatalf("unexpected events: %+v", got)
	}
}

func TestDoEventsSupervisorDefaultUsesTaggedJSONLItems(t *testing.T) {
	items := []genclient.TaggedEvent{
		{Actor: "human", City: "alpha", Seq: 3, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, genclient.SupervisorEventListOutputBody{Items: &items, Total: int64(len(items))})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{apiURL: server.URL}, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got genclient.TaggedEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.City != "alpha" || got.Type != "bead.created" || got.Seq != 3 {
		t.Fatalf("unexpected tagged event: %+v", got)
	}
}

func TestDoEventsSeqCityUsesIndexHeader(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "7")
			items := []genclient.Event{}
			writeJSONResponse(t, w, genclient.ListBodyEvent{Items: &items, Total: 0})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "7" {
		t.Fatalf("seq = %q, want 7", got)
	}
}

func TestDoEventsSeqSupervisorPrintsCompositeCursor(t *testing.T) {
	items := []genclient.TaggedEvent{
		{Actor: "human", City: "beta", Seq: 9, Ts: time.Unix(1700000001, 0).UTC(), Type: "mail.sent"},
		{Actor: "human", City: "alpha", Seq: 4, Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, genclient.SupervisorEventListOutputBody{Items: &items, Total: int64(len(items))})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(eventsAPIScope{apiURL: server.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "alpha:4,beta:9" {
		t.Fatalf("cursor = %q, want alpha:4,beta:9", got)
	}
}

func TestDoEventsWatchCityBufferedReplayUsesEnvelopeSchema(t *testing.T) {
	items := []genclient.Event{
		{Actor: "human", Seq: 1, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "human", Message: stringPtr("hello"), Seq: 2, Subject: stringPtr("gc-2"), Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, genclient.ListBodyEvent{Items: &items, Total: int64(len(items))})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, "", nil, 1, "", 50*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr=%s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d JSON lines, want 1; output=%q", len(lines), stdout.String())
	}
	var envelope genclient.EventStreamEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.Seq != 2 || envelope.Type != "mail.sent" {
		t.Fatalf("envelope = %+v, want seq=2 type=mail.sent", envelope)
	}
}

func TestDoEventsWatchSupervisorBufferedReplayUsesTaggedEnvelopeSchema(t *testing.T) {
	items := []genclient.TaggedEvent{
		{Actor: "human", City: "alpha", Seq: 2, Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", City: "beta", Seq: 5, Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, genclient.SupervisorEventListOutputBody{Items: &items, Total: int64(len(items))})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{apiURL: server.URL}, "", nil, 0, "alpha:2", 50*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr=%s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d JSON lines, want 1; output=%q", len(lines), stdout.String())
	}
	var envelope genclient.TaggedEventStreamEnvelope
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.City != "beta" || envelope.Seq != 5 || envelope.Type != "session.woke" {
		t.Fatalf("envelope = %+v, want beta/5/session.woke", envelope)
	}
}

func TestDoEventsWatchTimesOutWithoutMatch(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "3")
			items := []genclient.Event{}
			writeJSONResponse(t, w, genclient.ListBodyEvent{Items: &items, Total: 0})
		},
		cityStream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = io.WriteString(w, "event: heartbeat\n")
					_, _ = io.WriteString(w, "data: {\"timestamp\":\"2026-01-01T00:00:00Z\"}\n\n")
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, "bead.closed", nil, 0, "", 30*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty timeout output", stdout.String())
	}
}

func TestMatchPayload(t *testing.T) {
	t.Run("nil filter always matches", func(t *testing.T) {
		if !matchPayload(nil, nil) {
			t.Fatal("nil filter should match")
		}
	})

	t.Run("matches map payload", func(t *testing.T) {
		payload := map[string]any{"type": "merge-request", "count": 42.0}
		if !matchPayload(payload, map[string][]string{"type": {"merge-request"}}) {
			t.Fatal("expected merge-request payload to match")
		}
		if !matchPayload(payload, map[string][]string{"count": {"42"}}) {
			t.Fatal("expected numeric payload value to match string form")
		}
	})

	t.Run("repeated keys mean OR", func(t *testing.T) {
		payload := map[string]any{"type": "message"}
		if !matchPayload(payload, map[string][]string{"type": {"merge-request", "message"}}) {
			t.Fatal("expected OR payload match to succeed")
		}
	})
}

func TestParsePayloadMatch(t *testing.T) {
	m, err := parsePayloadMatch([]string{"type=merge-request", "state=open", "state=closed"})
	if err != nil {
		t.Fatalf("parsePayloadMatch: %v", err)
	}
	if len(m["state"]) != 2 {
		t.Fatalf("state values = %v, want 2 entries", m["state"])
	}

	if _, err := parsePayloadMatch([]string{"broken"}); err == nil {
		t.Fatal("expected invalid payload-match to fail")
	}
}

type testEventRoutes struct {
	cityEvents       func(http.ResponseWriter, *http.Request)
	cityStream       func(http.ResponseWriter, *http.Request)
	supervisorEvents func(http.ResponseWriter, *http.Request)
	supervisorStream func(http.ResponseWriter, *http.Request)
}

func newEventsTestServer(t *testing.T, routes testEventRoutes) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/city/mc-city/events":
			if routes.cityEvents == nil {
				t.Fatalf("unexpected city events request: %s", r.URL.String())
			}
			routes.cityEvents(w, r)
		case "/v0/city/mc-city/events/stream":
			if routes.cityStream == nil {
				t.Fatalf("unexpected city stream request: %s", r.URL.String())
			}
			routes.cityStream(w, r)
		case "/v0/events":
			if routes.supervisorEvents == nil {
				t.Fatalf("unexpected supervisor events request: %s", r.URL.String())
			}
			routes.supervisorEvents(w, r)
		case "/v0/events/stream":
			if routes.supervisorStream == nil {
				t.Fatalf("unexpected supervisor stream request: %s", r.URL.String())
			}
			routes.supervisorStream(w, r)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode JSON response: %v", err)
	}
}

var _ = context.Background

func newTestProvider(t *testing.T, dir string) *events.FileRecorder {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := events.NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec
}
