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
	items := []cliWireEvent{
		{Actor: "human", Seq: 1, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", Seq: 2, Subject: "mayor", Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
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
	var got []cliWireEvent
	for _, line := range lines {
		var item cliWireEvent
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
	items := []cliWireTaggedEvent{
		{Actor: "human", City: "alpha", Seq: 3, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{apiURL: server.URL}, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got cliWireTaggedEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.City != "alpha" || got.Type != "bead.created" || got.Seq != 3 {
		t.Fatalf("unexpected tagged event: %+v", got)
	}
}

func TestEventsJSONFlagIsSilentNoOp(t *testing.T) {
	clearGCEnv(t)
	t.Chdir(t.TempDir())

	items := []cliWireTaggedEvent{
		{Actor: "human", City: "alpha", Seq: 3, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := newEventsCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--api", server.URL, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc events --json execute: %v; stderr=%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var got cliWireTaggedEvent
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
			items := []cliWireEvent{}
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
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
	items := []cliWireTaggedEvent{
		{Actor: "human", City: "beta", Seq: 9, Ts: time.Unix(1700000001, 0).UTC(), Type: "mail.sent"},
		{Actor: "human", City: "alpha", Seq: 4, Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
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

	var got cliWireEvent
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

	var got cliWireEvent
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

	var got cliWireEvent
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

	var got cliWireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != events.SessionStopped || got.Seq != 1 {
		t.Fatalf("fallback event = %+v, want session.stopped seq=1", got)
	}
}

func TestDoEventsReadsCustomCityEventTypesThroughAPI(t *testing.T) {
	cityDir := t.TempDir()
	items := []cliWireEvent{{
		Actor:   "human",
		Seq:     1,
		Subject: "fixture",
		Ts:      time.Unix(1700000000, 0).UTC(),
		Type:    "app.custom",
		Message: "custom event",
		Payload: json.RawMessage(`{"source":"test"}`),
	}}

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("type"); got != "app.custom" {
				t.Fatalf("type query = %q, want app.custom", got)
			}
			w.Header().Set("X-GC-Index", "1")
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, "app.custom", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}

	var got cliWireEvent
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal stdout: %v; output=%s", err, stdout.String())
	}
	if got.Type != "app.custom" || got.Subject != "fixture" || got.Message != "custom event" {
		t.Fatalf("custom event = %+v", got)
	}
	if string(got.Payload) != `{"source":"test"}` {
		t.Fatalf("custom event payload = %s", got.Payload)
	}
}

func TestDoEventsDoesNotReadLocalUntypedCityEventsForExplicitRemoteAPI(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{Type: "app.custom", Actor: "human"})

	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "0")
			writeJSONResponse(t, w, cityEventsListResponse(t, []cliWireEvent{}))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{
		apiURL:      server.URL,
		cityName:    "mc-city",
		cityPath:    cityDir,
		explicitAPI: true,
	}, "app.custom", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("stdout = %q, want explicit remote API result", stdout.String())
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
	items := []cliWireEvent{
		{Actor: "human", Seq: 1, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "human", Message: "hello", Seq: 2, Subject: "gc-2", Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
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
	items := []cliWireEvent{
		{Actor: "human", Seq: 1, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "human", Message: "hello", Seq: 2, Subject: "gc-2", Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			// Buffered replay for --after only needs the JSON body; a missing
			// X-GC-Index header should not block replay.
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
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
	items := []cliWireTaggedEvent{
		{Actor: "human", City: "alpha", Seq: 2, Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", City: "beta", Seq: 5, Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
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

// assertCorrelationIDsInJSON decodes a single line of `gc events` stdout JSON
// through an independent struct (not the CLI wire structs under test) and
// asserts the run_id/session_id/step_id keys survived to the wire. Decoding
// into a fresh struct proves the keys are present in the emitted JSON rather
// than merely re-reading the producer's own struct.
func assertCorrelationIDsInJSON(t *testing.T, line, wantRun, wantSession, wantStep string) {
	t.Helper()
	var got struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
		StepID    string `json:"step_id"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal correlation ids: %v; line=%q", err, line)
	}
	if got.RunID != wantRun || got.SessionID != wantSession || got.StepID != wantStep {
		t.Fatalf("correlation ids = run_id=%q session_id=%q step_id=%q, want %q/%q/%q; line=%q",
			got.RunID, got.SessionID, got.StepID, wantRun, wantSession, wantStep, line)
	}
}

func TestDoEventsCityListForwardsCorrelationFields(t *testing.T) {
	items := []cliWireEvent{
		{Actor: "gc", Seq: 1, Subject: "gcg-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created", RunID: "run-abc", SessionID: "sess-1", StepID: "step-7"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "1")
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertCorrelationIDsInJSON(t, strings.TrimSpace(stdout.String()), "run-abc", "sess-1", "step-7")
}

func TestDoEventsSupervisorListForwardsCorrelationFields(t *testing.T) {
	items := []cliWireTaggedEvent{
		{Actor: "gc", City: "alpha", Seq: 3, Subject: "gcg-2", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created", RunID: "run-xyz", SessionID: "sess-2", StepID: "step-9"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEvents(eventsAPIScope{apiURL: server.URL}, "", "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertCorrelationIDsInJSON(t, strings.TrimSpace(stdout.String()), "run-xyz", "sess-2", "step-9")
}

func TestDoEventsWatchCityBufferedReplayForwardsCorrelationFields(t *testing.T) {
	items := []cliWireEvent{
		{Actor: "human", Seq: 1, Subject: "gc-1", Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", Seq: 2, Subject: "gc-2", Ts: time.Unix(1700000010, 0).UTC(), Type: "mail.sent", RunID: "run-abc", SessionID: "sess-1", StepID: "step-7"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "2")
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, "", nil, 1, "", 50*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertCorrelationIDsInJSON(t, strings.TrimSpace(stdout.String()), "run-abc", "sess-1", "step-7")
}

func TestDoEventsWatchSupervisorBufferedReplayForwardsCorrelationFields(t *testing.T) {
	items := []cliWireTaggedEvent{
		{Actor: "human", City: "alpha", Seq: 2, Ts: time.Unix(1700000000, 0).UTC(), Type: "bead.created"},
		{Actor: "gc", City: "beta", Seq: 5, Ts: time.Unix(1700000010, 0).UTC(), Type: "session.woke", RunID: "run-xyz", SessionID: "sess-2", StepID: "step-9"},
	}
	server := newEventsTestServer(t, testEventRoutes{
		supervisorEvents: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, supervisorEventsListResponse(t, items))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsWatch(eventsAPIScope{apiURL: server.URL}, "", nil, 0, "alpha:2", 50*time.Millisecond, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsWatch = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertCorrelationIDsInJSON(t, strings.TrimSpace(stdout.String()), "run-xyz", "sess-2", "step-9")
}

func TestDoEventsLocalCityFallbackForwardsCorrelationFields(t *testing.T) {
	cityDir := t.TempDir()
	rec := newTestProvider(t, filepath.Join(cityDir, ".gc"))
	rec.Record(events.Event{
		Type:      events.SessionStopped,
		Actor:     "gc",
		Subject:   "worker",
		Message:   "stopped",
		RunID:     "run-local",
		SessionID: "sess-local",
		StepID:    "step-local",
	})

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
	code := doEvents(eventsAPIScope{
		apiURL:   server.URL,
		cityName: "mc-city",
		cityPath: cityDir,
	}, events.SessionStopped, "", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEvents = %d, want 0; stderr=%s", code, stderr.String())
	}
	assertCorrelationIDsInJSON(t, strings.TrimSpace(stdout.String()), "run-local", "sess-local", "step-local")
}

func TestDoEventsWatchTimesOutWithoutMatch(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-GC-Index", "3")
			items := []cliWireEvent{}
			writeJSONResponse(t, w, cityEventsListResponse(t, items))
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

	t.Run("matches nested payload via dotted path", func(t *testing.T) {
		// bead.closed events have shape {"payload":{"bead":{"issue_type":"...",...}}}
		// where the filterable fields live nested under "bead". A dotted-key
		// filter must walk into the nested map.
		payload := map[string]any{
			"bead": map[string]any{
				"id":         "pc-wisp-foo",
				"issue_type": "molecule",
				"status":     "closed",
			},
		}
		if !matchPayload(payload, map[string][]string{"bead.issue_type": {"molecule"}}) {
			t.Fatal("expected nested key bead.issue_type to match molecule")
		}
		if !matchPayload(payload, map[string][]string{"bead.status": {"closed"}}) {
			t.Fatal("expected nested key bead.status to match closed")
		}
	})

	t.Run("nested key value mismatch returns false", func(t *testing.T) {
		payload := map[string]any{
			"bead": map[string]any{"issue_type": "molecule"},
		}
		if matchPayload(payload, map[string][]string{"bead.issue_type": {"task"}}) {
			t.Fatal("expected nested value mismatch to fail")
		}
	})

	t.Run("missing intermediate path returns false", func(t *testing.T) {
		payload := map[string]any{"foo": "bar"}
		if matchPayload(payload, map[string][]string{"bead.issue_type": {"molecule"}}) {
			t.Fatal("expected missing intermediate map to fail closed")
		}
	})

	t.Run("intermediate non-object returns false", func(t *testing.T) {
		// "bead" is a string here, not a map — walking should fail without panic.
		payload := map[string]any{"bead": "not-an-object"}
		if matchPayload(payload, map[string][]string{"bead.issue_type": {"molecule"}}) {
			t.Fatal("expected non-object intermediate to fail closed")
		}
	})

	t.Run("flat key still matches at top level (backward-compat)", func(t *testing.T) {
		payload := map[string]any{"type": "merge-request"}
		if !matchPayload(payload, map[string][]string{"type": {"merge-request"}}) {
			t.Fatal("flat top-level key must still match")
		}
	})

	t.Run("flat key with no dot does not silently traverse", func(t *testing.T) {
		// Guard against future refactors where lookupPayloadKey accidentally
		// walks even when there's no dot. A flat key "type" must not match
		// a nested {"bead":{"type":"..."}} value.
		payload := map[string]any{
			"bead": map[string]any{"type": "merge-request"},
		}
		if matchPayload(payload, map[string][]string{"type": {"merge-request"}}) {
			t.Fatal("flat key must not match nested value at the same name")
		}
	})

	t.Run("nested OR works across siblings", func(t *testing.T) {
		payload := map[string]any{
			"bead": map[string]any{"issue_type": "task"},
		}
		filter := map[string][]string{"bead.issue_type": {"bug", "task", "molecule"}}
		if !matchPayload(payload, filter) {
			t.Fatal("expected nested key OR-list to match task")
		}
	})

	t.Run("matches literal dotted key below nested map", func(t *testing.T) {
		payload := map[string]any{
			"bead": map[string]any{
				"metadata": map[string]any{
					"gc.root_bead_id": "ga-root",
				},
			},
		}
		if !matchPayload(payload, map[string][]string{"bead.metadata.gc.root_bead_id": {"ga-root"}}) {
			t.Fatal("expected dotted path to match literal metadata key gc.root_bead_id")
		}
	})

	t.Run("matches deeper nested payload via dotted path", func(t *testing.T) {
		payload := map[string]any{
			"request": map[string]any{
				"result": map[string]any{
					"status": "ok",
				},
			},
		}
		if !matchPayload(payload, map[string][]string{"request.result.status": {"ok"}}) {
			t.Fatal("expected 3-segment nested key to match")
		}
	})

	t.Run("matches flat and nested filters together", func(t *testing.T) {
		payload := map[string]any{
			"type": "bead.closed",
			"bead": map[string]any{"issue_type": "task"},
		}
		filter := map[string][]string{
			"type":            {"bead.closed"},
			"bead.issue_type": {"task"},
		}
		if !matchPayload(payload, filter) {
			t.Fatal("expected combined flat and nested filters to match")
		}
	})

	t.Run("matches nested numeric payload value", func(t *testing.T) {
		payload := map[string]any{
			"bead": map[string]any{"priority": 2.0},
		}
		if !matchPayload(payload, map[string][]string{"bead.priority": {"2"}}) {
			t.Fatal("expected nested numeric value to match string form")
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

func TestDoEventsRotateGoldenPathPrintsJSONL(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityRotate: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("X-GC-Request"); got == "" {
				t.Fatal("missing X-GC-Request header")
			}
			if got := r.URL.Query().Get("wait"); got != "" {
				t.Fatalf("wait query = %q, want absent", got)
			}
			writeJSONResponse(t, w, eventRotateTestResponse("pending"))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsRotate = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := `{"rotated":true,"archive":{"path":"/tmp/events.jsonl.archive-20260505T035000Z-seq-1234-5678.gz","first_seq":1234,"last_seq":5678,"compression_status":"pending"},"anchor_event":{"seq":5679,"type":"events.rotated","ts":"2026-05-05T03:50:00.123456Z"},"ok":true}` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestDoEventsRotateEmptyActiveLogNoOp(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityRotate: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, eventRotateNoopTestResponse())
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsRotate = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := `{"rotated":false,"reason":"active log is empty","ok":true}` + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestDoEventsRotateWaitRequestsServerSideWait(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityRotate: func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("wait"); got != "true" {
				t.Fatalf("wait query = %q, want true", got)
			}
			writeJSONResponse(t, w, eventRotateTestResponse("complete"))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doEventsRotate --wait = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"compression_status":"complete"`) {
		t.Fatalf("stdout = %q, want complete compression status", stdout.String())
	}
}

func TestDoEventsRotateUnsupportedProviderErrorIsPinned(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityRotate: func(w http.ResponseWriter, _ *http.Request) {
			writeProblemResponseStatus(t, w, http.StatusMethodNotAllowed, map[string]any{
				"title":  "Method Not Allowed",
				"status": http.StatusMethodNotAllowed,
				"detail": "rotation is only supported for the file-backed events provider; current provider is 'exec:my-script'",
			})
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsRotate = %d, want 1", code)
	}
	want := "gc events: rotate is only supported for the file-backed events provider; current provider is 'exec:my-script'\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestDoEventsRotateRequiresRunningSupervisor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{localOnly: true, cityName: "mc-city"}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsRotate localOnly = %d, want 1", code)
	}
	want := "gc events: rotate requires a running supervisor; start it with 'gc supervisor start'\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestDoEventsRotateCityNotFoundErrorIsPinned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/missing-city/events/rotate" {
			t.Fatalf("path = %s, want missing-city rotate path", r.URL.Path)
		}
		writeProblemResponseStatus(t, w, http.StatusNotFound, map[string]any{
			"title":  "Not Found",
			"status": http.StatusNotFound,
			"detail": gcapi.CityNotFoundOrNotRunningDetail("missing-city"),
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "missing-city"}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsRotate missing city = %d, want 1", code)
	}
	want := "gc events: city 'missing-city' not found; run 'gc supervisor cities' to list registered cities\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestDoEventsRotateWaitTimeoutIsPinned(t *testing.T) {
	server := newEventsTestServer(t, testEventRoutes{
		cityRotate: func(w http.ResponseWriter, _ *http.Request) {
			writeJSONResponse(t, w, eventRotateTestResponse("pending"))
		},
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := doEventsRotate(eventsAPIScope{apiURL: server.URL, cityName: "mc-city"}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doEventsRotate wait pending = %d, want 1", code)
	}
	want := "gc events: rotation succeeded but compression did not complete within 30s; archive_path=/tmp/events.jsonl.archive-20260505T035000Z-seq-1234-5678.gz; check disk space and retry\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestEventsRotateHelpIncludesFlagsAndExample(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"events", "rotate", "--help"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events rotate --help: %v; stderr=%s", err, stderr.String())
	}
	help := stdout.String()
	for _, want := range []string{
		"gc events rotate",
		"--api",
		"--city",
		"--wait",
		"gc events rotate --wait",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}

type testEventRoutes struct {
	cityEvents       func(http.ResponseWriter, *http.Request)
	cityRotate       func(http.ResponseWriter, *http.Request)
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
		case "/v0/city/mc-city/events/rotate":
			if routes.cityRotate == nil {
				t.Fatalf("unexpected city rotate request: %s", r.URL.String())
			}
			routes.cityRotate(w, r)
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

func eventRotateTestResponse(status string) cliEventsRotateResponse {
	return cliEventsRotateResponse{
		Rotated: true,
		Archive: &cliEventsRotateArchive{
			Path:              "/tmp/events.jsonl.archive-20260505T035000Z-seq-1234-5678.gz",
			FirstSeq:          1234,
			LastSeq:           5678,
			CompressionStatus: status,
		},
		AnchorEvent: &cliEventsRotateAnchor{
			Seq:  5679,
			Type: events.EventsRotated,
			Ts:   time.Date(2026, 5, 5, 3, 50, 0, 123456000, time.UTC),
		},
	}
}

func eventRotateNoopTestResponse() cliEventsRotateResponse {
	return cliEventsRotateResponse{
		Rotated: false,
		Reason:  "active log is empty",
	}
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode JSON response: %v", err)
	}
}

func cityEventsListResponse(t *testing.T, items []cliWireEvent) genclient.ListBodyWireEvent {
	t.Helper()
	typed := make([]genclient.TypedEventStreamEnvelope, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal city event item: %v", err)
		}
		var envelope genclient.TypedEventStreamEnvelope
		if err := envelope.UnmarshalJSON(data); err != nil {
			t.Fatalf("unmarshal typed city event item: %v; item=%s", err, data)
		}
		typed = append(typed, envelope)
	}
	return genclient.ListBodyWireEvent{Items: &typed, Total: int64(len(typed))}
}

func supervisorEventsListResponse(t *testing.T, items []cliWireTaggedEvent) genclient.SupervisorEventListOutputBody {
	t.Helper()
	typed := make([]genclient.TypedTaggedEventStreamEnvelope, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal supervisor event item: %v", err)
		}
		var envelope genclient.TypedTaggedEventStreamEnvelope
		if err := envelope.UnmarshalJSON(data); err != nil {
			t.Fatalf("unmarshal typed supervisor event item: %v; item=%s", err, data)
		}
		typed = append(typed, envelope)
	}
	return genclient.SupervisorEventListOutputBody{Items: &typed, Total: int64(len(typed))}
}

func writeProblemResponse(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	writeProblemResponseStatus(t, w, http.StatusNotFound, body)
}

func writeProblemResponseStatus(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
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

// TestFetchCityEventsSinglePageChronological pins the S3 keyset contract on
// the CLI: the server serves seq-DESC pages (newest first) with v1 sq
// cursors; gc events fetches ONE page (recent activity, pre-S3 parity — a
// full drain of a 100MB+ event history would blow the command timeout) and
// prints it chronologically (ascending seq), never following next_cursor.
func TestFetchCityEventsSinglePageChronological(t *testing.T) {
	page1 := []cliWireEvent{
		{Actor: "gc", Seq: 6, Type: "e.t", Ts: time.Unix(1700000060, 0).UTC()},
		{Actor: "gc", Seq: 5, Type: "e.t", Ts: time.Unix(1700000050, 0).UTC()},
		{Actor: "gc", Seq: 4, Type: "e.t", Ts: time.Unix(1700000040, 0).UTC()},
	}
	server := newEventsTestServer(t, testEventRoutes{
		cityEvents: func(w http.ResponseWriter, r *http.Request) {
			if c := r.URL.Query().Get("cursor"); c != "" {
				t.Errorf("gc events must not follow cursors, requested cursor %q", c)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("X-GC-Index", "6")
			body := cityEventsListResponse(t, page1)
			next := "v1:eyJrIjoic3EiLCJzIjo0fQ"
			body.NextCursor = &next
			writeJSONResponse(t, w, body)
		},
	})
	defer server.Close()

	client, err := genclient.NewClientWithResponses(server.URL)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, err := fetchCityEvents(context.Background(), client, "mc-city", "", "")
	if err != nil {
		t.Fatalf("fetchCityEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (one page, cursor not followed)", len(got))
	}
	for i, item := range got {
		if item.Seq != int64(i+4) {
			t.Fatalf("event[%d].Seq = %d, want %d (chronological ascending)", i, item.Seq, i+4)
		}
	}
}
