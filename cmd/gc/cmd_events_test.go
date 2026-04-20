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

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/events"
)

func TestDoEventsCityDefaultUsesJSONLItems(t *testing.T) {
	items := []genclient.WireEvent{
		{Actor: "human", Seq: 1, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", Seq: 2, Subject: stringPtr("mayor"), Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, genclient.ListBodyWireEvent{Items: &items, Total: int64(len(items))})
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
	var got []genclient.WireEvent
	for _, line := range lines {
		var item genclient.WireEvent
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
	items := []genclient.WireTaggedEvent{
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

	var got genclient.WireTaggedEvent
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
			items := []genclient.WireEvent{}
			writeJSONResponse(t, w, genclient.ListBodyWireEvent{Items: &items, Total: 0})
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
	items := []genclient.WireTaggedEvent{
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

func TestDoEventsFallsBackToLocalCityEventsWhenCityStopped(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, map[string]any{
				"status": http.StatusNotFound,
				"title":  "Not Found",
				"detail": "not_found: city not found or not running: mc-city",
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got genclient.WireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != events.SessionStopped || got.Seq != 1 {
		t.Fatalf("fallback event = %+v, want session.stopped seq=1", got)
	}
}

func TestDoEventsFallsBackToLocalCityEventsOnTypedStoppedCityNotFound(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr("not_found: city not found or not running: mc-city"),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got genclient.WireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != events.SessionStopped || got.Seq != 1 {
		t.Fatalf("fallback event = %+v, want session.stopped seq=1", got)
	}
}

func TestDoEventsDoesNotFallbackToLocalCityEventsForGeneric404(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr("city is unavailable"),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEvents = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty when fallback is disabled", stdout.String())
	}
	if !strings.Contains(stderr.String(), "city is unavailable") {
		t.Fatalf("stderr = %q, want original API error", stderr.String())
	}
}

func TestDoEventsDoesNotFallbackToLocalCityEventsForExplicitAPI(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr("not_found: city not found or not running: mc-city"),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:      server.URL,
		cityName:    "mc-city",
		cityPath:    cityDir,
		explicitAPI: true,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEvents = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty when explicit API disables fallback", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not_found: city not found or not running: mc-city") {
		t.Fatalf("stderr = %q, want original API error", stderr.String())
	}
}

func TestDoEventsFallsBackToLocalCityEventsForExplicitLocalSupervisorAPI(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr("not_found: city not found or not running: mc-city"),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:             server.URL,
		cityName:           "mc-city",
		cityPath:           cityDir,
		explicitAPI:        true,
		localSupervisorAPI: true,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got genclient.WireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != events.SessionStopped || got.Seq != 1 {
		t.Fatalf("fallback event = %+v, want session.stopped seq=1", got)
	}
}

func TestDoEventsFallsBackToLocalCityEventsForExplicitLocalSupervisorAPITransportError(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:    events.SessionStopped,
		Actor:   "gc",
		Subject: "worker",
		Message: "stopped",
	})

	server := httptest.NewServer(http.NotFoundHandler())
	apiURL := server.URL
	server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:             apiURL,
		cityName:           "mc-city",
		cityPath:           cityDir,
		explicitAPI:        true,
		localSupervisorAPI: true,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got genclient.WireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != events.SessionStopped || got.Seq != 1 {
		t.Fatalf("fallback event = %+v, want session.stopped seq=1", got)
	}
}

func TestDoEventsSeqFallsBackToLocalCityEventHeadWhenCityStopped(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec.Record(events.Event{Type: events.SessionStopped, Actor: "gc"})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, map[string]any{
				"status": http.StatusNotFound,
				"title":  "Not Found",
				"detail": "not_found: city not found or not running: mc-city",
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "2" {
		t.Fatalf("seq = %q, want 2", got)
	}
}

func TestDoEventsSeqFallsBackToLocalCityEventHeadForExplicitLocalSupervisorAPI(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec.Record(events.Event{Type: events.SessionStopped, Actor: "gc"})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, map[string]any{
				"status": http.StatusNotFound,
				"title":  "Not Found",
				"detail": "not_found: city not found or not running: mc-city",
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(eventsAPIScope{
		apiURL:             server.URL,
		cityName:           "mc-city",
		cityPath:           cityDir,
		explicitAPI:        true,
		localSupervisorAPI: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "2" {
		t.Fatalf("seq = %q, want 2", got)
	}
}

func TestDoEventsSeqFallsBackToLocalCityEventHeadForExplicitLocalSupervisorAPITransportError(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{Type: events.SessionWoke, Actor: "gc"})
	rec.Record(events.Event{Type: events.SessionStopped, Actor: "gc"})

	server := httptest.NewServer(http.NotFoundHandler())
	apiURL := server.URL
	server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsSeq(eventsAPIScope{
		apiURL:             apiURL,
		cityName:           "mc-city",
		cityPath:           cityDir,
		explicitAPI:        true,
		localSupervisorAPI: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsSeq = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "2" {
		t.Fatalf("seq = %q, want 2", got)
	}
}

func TestDoEventsFollowStoppedCityRequiresRunningAPI(t *testing.T) {
	cityDir := t.TempDir()
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr(gcapi.CityNotFoundOrNotRunningDetail("mc-city")),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsFollow(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, "", nil, 0, "", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsFollow = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--follow requires a running city API") {
		t.Fatalf("stderr = %q, want explicit follow limitation", stderr.String())
	}
}

func TestDoEventsFollowStoppedCityAfterSeqRequiresRunningAPI(t *testing.T) {
	cityDir := t.TempDir()
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr(gcapi.CityNotFoundOrNotRunningDetail("mc-city")),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsFollow(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, "", nil, 5, "", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsFollow = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--follow requires a running city API") {
		t.Fatalf("stderr = %q, want explicit follow limitation", stderr.String())
	}
}

func TestDoEventsWatchStoppedCityRequiresRunningAPI(t *testing.T) {
	cityDir := t.TempDir()
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr(gcapi.CityNotFoundOrNotRunningDetail("mc-city")),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, "", nil, 0, "", 50*time.Millisecond, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsWatch = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--watch requires a running city API") {
		t.Fatalf("stderr = %q, want explicit watch limitation", stderr.String())
	}
}

func TestDoEventsWatchStoppedCityAfterSeqRequiresRunningAPI(t *testing.T) {
	cityDir := t.TempDir()
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponse(t, w, genclient.ErrorModel{
				Status: notFoundStatusPtr(),
				Title:  stringPtr("Not Found"),
				Detail: stringPtr(gcapi.CityNotFoundOrNotRunningDetail("mc-city")),
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, "", nil, 5, "", 50*time.Millisecond, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsWatch = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--watch requires a running city API") {
		t.Fatalf("stderr = %q, want explicit watch limitation", stderr.String())
	}
}

func TestDoEventsWatchCityBufferedReplayUsesEnvelopeSchema(t *testing.T) {
	items := []genclient.WireEvent{
		{Actor: "human", Seq: 1, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "human", Message: stringPtr("hello"), Seq: 2, Subject: stringPtr("gc-2"), Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, genclient.ListBodyWireEvent{Items: &items, Total: int64(len(items))})
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

func TestDoEventsWatchCityBufferedReplayAfterSeqSkipsHeadProbe(t *testing.T) {
	items := []genclient.WireEvent{
		{Actor: "human", Seq: 1, Subject: stringPtr("gc-1"), Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "human", Message: stringPtr("hello"), Seq: 2, Subject: stringPtr("gc-2"), Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			// Buffered replay for --after only needs the JSON body; a missing
			// X-GC-Index header should not block replay.
			writeJSONResponse(t, w, genclient.ListBodyWireEvent{Items: &items, Total: int64(len(items))})
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
	items := []genclient.WireTaggedEvent{
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
			items := []genclient.WireEvent{}
			writeJSONResponse(t, w, genclient.ListBodyWireEvent{Items: &items, Total: 0})
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

func TestCmdEventsValidatesLocalFlagsBeforeAPIDiscovery(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdEvents("", "", "notaduration", nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdEvents invalid since = 0, want non-zero")
	}
	if got := stderr.String(); !strings.Contains(got, "invalid --since") {
		t.Fatalf("stderr = %q, want invalid --since", got)
	}

	stdout.Reset()
	stderr.Reset()
	code = cmdEventsWatch("", "", nil, 0, "", "notaduration", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdEventsWatch invalid timeout = 0, want non-zero")
	}
	if got := stderr.String(); !strings.Contains(got, "invalid --timeout") {
		t.Fatalf("stderr = %q, want invalid --timeout", got)
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

func writeProblemResponse(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode problem response: %v", err)
	}
}

var _ = context.Background

func notFoundStatusPtr() *int64 {
	x := int64(http.StatusNotFound)
	return &x
}

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
