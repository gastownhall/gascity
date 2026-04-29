package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/events"
)

func TestRequestIDFromPayloadCoversAsyncPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload events.Payload
		want    string
	}{
		{
			name: "city create",
			payload: CityCreateSucceededPayload{
				RequestID: "req-city-create",
			},
			want: "req-city-create",
		},
		{
			name: "city unregister",
			payload: CityUnregisterSucceededPayload{
				RequestID: "req-city-unregister",
			},
			want: "req-city-unregister",
		},
		{
			name: "session create",
			payload: SessionCreateSucceededPayload{
				RequestID: "req-session-create",
				Session:   sessionResponse{ID: "session-1"},
			},
			want: "req-session-create",
		},
		{
			name: "session message",
			payload: SessionMessageSucceededPayload{
				RequestID: "req-session-message",
			},
			want: "req-session-message",
		},
		{
			name: "session submit",
			payload: SessionSubmitSucceededPayload{
				RequestID: "req-session-submit",
			},
			want: "req-session-submit",
		},
		{
			name: "request failed",
			payload: RequestFailedPayload{
				RequestID: "req-failed",
			},
			want: "req-failed",
		},
		{
			name:    "unknown payload",
			payload: events.NoPayload{},
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestIDFromPayload(tc.payload); got != tc.want {
				t.Fatalf("requestIDFromPayload() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmitRequestFailedRecordsTypedPayload(t *testing.T) {
	rec := events.NewFake()

	EmitRequestFailed(rec, "req-1", RequestOperationCityCreate, "bad_dir", "directory is invalid")

	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	ev := rec.Events[0]
	if ev.Type != events.RequestFailed {
		t.Fatalf("event type = %q, want %q", ev.Type, events.RequestFailed)
	}
	if ev.Actor != "api" {
		t.Fatalf("actor = %q, want api", ev.Actor)
	}
	var payload RequestFailedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.RequestID != "req-1" || payload.Operation != RequestOperationCityCreate ||
		payload.ErrorCode != "bad_dir" || payload.ErrorMessage != "directory is invalid" {
		t.Fatalf("payload = %#v, want city.create failure for req-1", payload)
	}
}

func TestEmitCityCreateSucceededRecordsSupervisorResult(t *testing.T) {
	rec := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: rec,
	}

	emitCityCreateSucceeded(resolver, "req-city", &cityinit.InitResult{
		CityName: "mc-city",
		CityPath: "/tmp/mc-city",
	}, "/tmp/fallback")

	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	ev := rec.Events[0]
	if ev.Type != events.RequestResultCityCreate {
		t.Fatalf("event type = %q, want %q", ev.Type, events.RequestResultCityCreate)
	}
	if ev.Subject != "mc-city" {
		t.Fatalf("subject = %q, want mc-city", ev.Subject)
	}
	var payload CityCreateSucceededPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.RequestID != "req-city" || payload.Name != "mc-city" || payload.Path != "/tmp/mc-city" {
		t.Fatalf("payload = %#v, want mc-city city.create result", payload)
	}
}

func TestEmitCityCreateSucceededFallsBackToDirectory(t *testing.T) {
	rec := events.NewFake()
	resolver := &fakeCityResolver{
		cities:             map[string]*fakeState{},
		supervisorRecorder: rec,
	}

	emitCityCreateSucceeded(resolver, "req-city", nil, "/tmp/fallback-city")

	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.Events))
	}
	ev := rec.Events[0]
	if ev.Subject != "fallback-city" {
		t.Fatalf("subject = %q, want fallback-city", ev.Subject)
	}
	var payload CityCreateSucceededPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.RequestID != "req-city" || payload.Name != "fallback-city" || payload.Path != "/tmp/fallback-city" {
		t.Fatalf("payload = %#v, want fallback city.create result", payload)
	}
}

func TestClearPendingCityRequestIDOnlyConsumesStoredRequests(t *testing.T) {
	resolver := &fakeCityResolver{cities: map[string]*fakeState{}}
	sm := NewSupervisorMux(resolver, nil, false, "test", time.Now())
	const cityPath = "/tmp/mc-city"

	if err := resolver.StorePendingRequestID(cityPath, "req-1"); err != nil {
		t.Fatal(err)
	}
	sm.clearPendingCityRequestID(cityPath, false)
	if got := resolver.pending[cityPath]; got != "req-1" {
		t.Fatalf("pending after stored=false = %q, want req-1", got)
	}

	sm.clearPendingCityRequestID(cityPath, true)
	if _, ok := resolver.pending[cityPath]; ok {
		t.Fatalf("pending request for %q was not consumed", cityPath)
	}
}
