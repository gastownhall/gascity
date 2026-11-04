package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

type availabilityObservationHandle struct {
	observation worker.LiveObservation
	diagnostic  *runtime.ProviderAvailability
	err         error
	reads       int
	expectedSID string
}

func (h *availabilityObservationHandle) LiveObservation(context.Context) (worker.LiveObservation, error) {
	return h.observation, h.err
}

func (h *availabilityObservationHandle) ProviderAvailability(_ context.Context, expectedSID string) *runtime.ProviderAvailability {
	h.reads++
	h.expectedSID = expectedSID
	return h.diagnostic
}

func TestSessionProviderAvailabilityProjectionKeepsIndependentRuntimeFacts(t *testing.T) {
	at := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
	handle := &availabilityObservationHandle{
		observation: worker.LiveObservation{SessionID: "session-one", Running: true, Alive: true},
		diagnostic:  &runtime.ProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", ObservedAt: at, RuntimeGeneration: "opaque-incarnation"},
	}
	view := sessionProviderAvailability(context.Background(), "session-one", handle)
	if view == nil || view.State != "unavailable" || view.Code != "connection_restore_failed" || view.Source != "native_ui" || !view.ObservedAt.Equal(at) || view.RuntimeGeneration != "opaque-incarnation" {
		t.Fatalf("availability = %+v, want exact native diagnostic", view)
	}
	if handle.expectedSID != "session-one" {
		t.Fatalf("availability requested for %q, want exact base-observed session", handle.expectedSID)
	}
	if handle.reads != 1 {
		t.Fatalf("native reads = %d, want one", handle.reads)
	}
	if !handle.observation.Running || !handle.observation.Alive {
		t.Fatal("availability changed runtime liveness")
	}
	view.State = "unknown"
	if handle.diagnostic.State != "unavailable" {
		t.Fatal("projection aliases domain observation")
	}
}

func TestSessionProviderAvailabilityScopeAndUncertainty(t *testing.T) {
	for _, tc := range []struct {
		name        string
		observation worker.LiveObservation
		err         error
	}{
		{name: "different session", observation: worker.LiveObservation{SessionID: "another", Running: true, Alive: true}},
		{name: "inactive", observation: worker.LiveObservation{SessionID: "exact"}},
		{name: "suspended", observation: worker.LiveObservation{SessionID: "exact", Running: true, Alive: true, Suspended: true}},
		{name: "base observation failed", err: errors.New("unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle := &availabilityObservationHandle{
				observation: tc.observation, err: tc.err,
				diagnostic: &runtime.ProviderAvailability{State: "unavailable", Source: "native_ui"},
			}
			if view := sessionProviderAvailability(context.Background(), "exact", handle); view != nil {
				t.Fatalf("view = %+v, want no other-incarnation or unobserved diagnostic", view)
			}
		})
	}
	handle := &availabilityObservationHandle{
		observation: worker.LiveObservation{SessionID: "exact", Running: true, Alive: true},
		diagnostic:  &runtime.ProviderAvailability{State: "unknown", Source: "native_ui", RuntimeGeneration: "generation"},
	}
	if view := sessionProviderAvailability(context.Background(), "exact", handle); view == nil || view.State != "unknown" {
		t.Fatalf("indeterminate = %+v, want explicit unknown", view)
	}
	if view := sessionProviderAvailability(context.Background(), "exact", struct{}{}); view != nil {
		t.Fatalf("unsupported = %+v, want omitted", view)
	}
}

func TestProviderAvailabilityFramesInitialSemanticChangesAndClear(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
	var current *SessionProviderAvailability
	frames := providerAvailabilityFrames{id: "exact", read: func() *SessionProviderAvailability { return current }}
	initial, changed := frames.next()
	if !changed || initial.ID != "exact" || initial.ProviderAvailability != nil {
		t.Fatalf("initial unsupported view = %+v, changed %v; want exact-session explicit clear", initial, changed)
	}
	if _, changed := frames.next(); changed {
		t.Fatal("unchanged unsupported view repeated its initial clear")
	}
	current = &SessionProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", ObservedAt: now, RuntimeGeneration: "one"}
	frame, changed := frames.next()
	if !changed || frame.ID != "exact" || frame.ProviderAvailability == nil {
		t.Fatalf("initial frame = %+v, changed %v", frame, changed)
	}
	current.ObservedAt = now.Add(time.Second)
	if _, changed := frames.next(); changed {
		t.Fatal("observation timestamp alone emitted a duplicate semantic frame")
	}
	for _, mutate := range []func(){
		func() { current.RuntimeGeneration = "two" },
		func() { current.State, current.Code = "unknown", "" },
		func() { current.Source = "different-constructed-source" },
	} {
		mutate()
		if frame, changed = frames.next(); !changed || frame.ID != "exact" {
			t.Fatalf("semantic change frame = %+v, changed %v", frame, changed)
		}
	}
	current = nil
	frame, changed = frames.next()
	if !changed || frame.ID != "exact" || frame.ProviderAvailability != nil {
		t.Fatalf("clear = %+v, changed %v", frame, changed)
	}
	if _, changed := frames.next(); changed {
		t.Fatal("unchanged cleared view repeated")
	}
	encoded, err := json.Marshal(frame)
	if err != nil || !strings.Contains(string(encoded), `"provider_availability":null`) {
		t.Fatalf("clear wire = %s, err %v", encoded, err)
	}
}

// availabilityNativeProvider observes only the public optional provider seam.
// No provider process, native log or terminal capture is fabricated by this fixture.
type availabilityNativeProvider struct {
	runtime.FullFake
	mu         sync.Mutex
	diagnostic *runtime.ProviderAvailability
	reads      int
	family     string
}

func (p *availabilityNativeProvider) ObserveProviderAvailability(_ context.Context, _ string, req runtime.ProviderAvailabilityRequest) *runtime.ProviderAvailability {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	p.family = req.Family
	if p.diagnostic == nil {
		return nil
	}
	value := *p.diagnostic
	value.ObservedAt = value.ObservedAt.Add(time.Duration(p.reads) * time.Second)
	value.RuntimeGeneration = req.Fence.Digest()
	return &value
}

func (p *availabilityNativeProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.reads }

func (p *availabilityNativeProvider) observedFamily() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.family
}

func (p *availabilityNativeProvider) setDiagnostic(value *runtime.ProviderAvailability) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if value == nil {
		p.diagnostic = nil
		return
	}
	copyValue := *value
	p.diagnostic = &copyValue
}

func (p *availabilityNativeProvider) unknown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.diagnostic.State, p.diagnostic.Code = "unknown", ""
}

func availabilityProviderFixture(t *testing.T, profile ...worker.Profile) (*fakeState, *availabilityNativeProvider, session.Info) {
	t.Helper()
	fs := newSessionFakeState(t)
	provider := &availabilityNativeProvider{
		FullFake:   runtime.FullFake{Fake: fs.sp},
		diagnostic: &runtime.ProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", ObservedAt: time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)},
	}
	fs.sessionProvider = provider
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, provider, session.WithCityPath(fs.cityPath))
	providerName := "codex"
	metadata := map[string]string{"session_origin": "manual"}
	if len(profile) > 1 {
		t.Fatal("fixture takes at most one explicit profile")
	}
	if len(profile) == 1 {
		providerName = "custom-codex-seat"
		metadata["worker_profile"] = string(profile[0])
	}
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "availability", Command: "codex", WorkDir: t.TempDir(), Provider: providerName, ExtraMeta: metadata})
	if err != nil {
		t.Fatal(err)
	}
	return fs, provider, info
}

func TestSessionGetProviderAvailabilityDoesNotEnrichLists(t *testing.T) {
	fs, provider, info := availabilityProviderFixture(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	var response sessionResponse
	getProgramStatusJSON(t, h, cityURL(fs, "/session/")+info.ID, &response)
	if response.ProviderAvailability == nil || response.ProviderAvailability.State != "unavailable" || response.ProviderAvailability.RuntimeGeneration == "" || !response.Running {
		t.Fatalf("single response = %+v, want live process plus opaque native diagnostic", response)
	}
	if got := provider.count(); got != 1 {
		t.Fatalf("single GET native observations = %d, want one", got)
	}
	var list struct {
		Items []sessionResponse `json:"items"`
	}
	getProgramStatusJSON(t, h, cityURL(fs, "/sessions"), &list)
	for _, row := range list.Items {
		if row.ProviderAvailability != nil {
			t.Fatal("session list performed optional native enrichment")
		}
	}
	if got := provider.count(); got != 1 {
		t.Fatalf("list native observations = %d, want still one", got)
	}
	t.Run("typed persisted profile on custom alias", func(t *testing.T) {
		aliasState, aliasProvider, aliasInfo := availabilityProviderFixture(t, worker.ProfileCodexTmuxCLI)
		if aliasInfo.Provider != "custom-codex-seat" {
			t.Fatal("fixture lost the custom provider alias")
		}
		aliasHandler := newTestCityHandlerWith(t, aliasState, New(aliasState))
		var aliasResponse sessionResponse
		getProgramStatusJSON(t, aliasHandler, cityURL(aliasState, "/session/")+aliasInfo.ID, &aliasResponse)
		if aliasResponse.ProviderAvailability == nil || aliasResponse.ProviderAvailability.State != "unavailable" || aliasProvider.observedFamily() != "codex" || aliasProvider.count() != 1 {
			t.Fatalf("typed record lost exact persisted profile: response=%+v family=%q reads=%d", aliasResponse, aliasProvider.observedFamily(), aliasProvider.count())
		}
	})
}

func TestSessionStreamProviderAvailabilityUsesExistingPollAndNoDurableID(t *testing.T) {
	for _, format := range []string{"raw", "conversation", "structured"} {
		t.Run(format, func(t *testing.T) {
			fs, provider, info := availabilityProviderFixture(t)
			initialDiagnostic := *provider.diagnostic
			provider.setDiagnostic(nil)
			h := newTestCityHandlerWith(t, fs, New(fs))
			ctx, cancel := context.WithCancel(context.Background())
			rec := newSyncResponseRecorder()
			done := make(chan struct{})
			url := cityURL(fs, "/session/") + info.ID + "/stream?format=" + format
			go func() { h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil).WithContext(ctx)); close(done) }()
			defer func() { cancel(); <-done }()
			frames := func() []SessionProviderAvailabilityEvent {
				var out []SessionProviderAvailabilityEvent
				body := rec.BodyString()
				end := strings.LastIndex(body, "\n\n")
				if end < 0 {
					return nil
				}
				for _, frame := range parseSSETestFrames(body[:end+2]) {
					if frame.Event != "provider_availability" {
						continue
					}
					if frame.ID != "" {
						t.Errorf("native diagnostic acquired durable cursor %q", frame.ID)
					}
					var view SessionProviderAvailabilityEvent
					if err := json.Unmarshal([]byte(frame.Data), &view); err != nil {
						t.Fatal(err)
					}
					if view.ProviderAvailability == nil {
						var wire map[string]json.RawMessage
						if err := json.Unmarshal([]byte(frame.Data), &wire); err != nil {
							t.Fatal(err)
						}
						if value, present := wire["provider_availability"]; !present || strings.TrimSpace(string(value)) != "null" {
							t.Fatalf("stream clear omitted explicit null: %s", frame.Data)
						}
					}
					out = append(out, view)
				}
				return out
			}
			await := func(label string, predicate func() bool) {
				t.Helper()
				met := false
				waitForRecorderBody(rec, time.Second, func(string) bool {
					met = predicate()
					if !met {
						fs.eventProv.(*events.Fake).Record(events.Event{Type: events.WorkerOperation, Subject: info.ID})
					}
					return met
				})
				if !met {
					t.Fatalf("%s missing; body=%s", label, rec.BodyString())
				}
			}
			unchanged := func(label string, want int) {
				t.Helper()
				reads := provider.count()
				await(label, func() bool { return provider.count() > reads })
				if got := len(frames()); got != want {
					t.Fatalf("%s emitted %d frames, want %d", label, got, want)
				}
			}
			await("initial explicit null", func() bool { return len(frames()) >= 1 })
			opening := frames()[0]
			if opening.ID != info.ID || opening.ProviderAvailability != nil {
				t.Fatalf("opening clear = %+v", opening)
			}
			unchanged("unchanged initial null", 1)
			provider.setDiagnostic(&initialDiagnostic)
			await("initial diagnostic", func() bool { return len(frames()) >= 2 })
			first := frames()[1]
			if first.ID != info.ID || first.ProviderAvailability == nil || first.ProviderAvailability.State != "unavailable" {
				t.Fatalf("first = %+v", first)
			}
			unchanged("timestamp-only re-observation", 2)
			provider.setDiagnostic(nil)
			await("unavailable cleared", func() bool { return len(frames()) >= 3 })
			if clearFrame := frames()[2]; clearFrame.ID != info.ID || clearFrame.ProviderAvailability != nil {
				t.Fatalf("unavailable clear = %+v", clearFrame)
			}
			unchanged("unchanged unavailable clear", 3)
			provider.setDiagnostic(&initialDiagnostic)
			await("diagnostic restored", func() bool { return len(frames()) >= 4 })
			if restored := frames()[3]; restored.ID != info.ID || restored.ProviderAvailability == nil || restored.ProviderAvailability.State != "unavailable" {
				t.Fatalf("restored diagnostic = %+v", restored)
			}
			provider.unknown()
			await("indeterminate native state", func() bool { return len(frames()) >= 5 })
			second := frames()[4]
			if second.ID != info.ID || second.ProviderAvailability == nil || second.ProviderAvailability.State != "unknown" || second.ProviderAvailability.RuntimeGeneration != first.ProviderAvailability.RuntimeGeneration {
				t.Fatalf("second = %+v", second)
			}
			provider.setDiagnostic(nil)
			await("unknown cleared", func() bool { return len(frames()) >= 6 })
			if clearFrame := frames()[5]; clearFrame.ID != info.ID || clearFrame.ProviderAvailability != nil {
				t.Fatalf("unknown clear = %+v", clearFrame)
			}
			unchanged("unchanged unknown clear", 6)
		})
	}
}
