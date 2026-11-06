package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type availabilityObservationHandle struct {
	observation LiveObservation
	err         error
	reads       int
}

func (h *availabilityObservationHandle) LiveObservation(context.Context) (LiveObservation, error) {
	return h.observation, h.err
}

func (h *availabilityObservationHandle) ProviderAvailability(_ context.Context, expectedSessionID string) *runtime.ProviderAvailability {
	h.reads++
	if expectedSessionID != h.observation.SessionID {
		panic("availability was not bound to the base-observed session")
	}
	return &runtime.ProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", RuntimeGeneration: "opaque"}
}

func TestProviderAvailabilityObservationIsExplicitlyOptIn(t *testing.T) {
	h := &availabilityObservationHandle{observation: LiveObservation{Running: true, Alive: true, SessionID: "exact"}}
	before, err := ObserveHandle(context.Background(), h)
	if err != nil || h.reads != 0 || before.ProviderAvailability != nil {
		t.Fatalf("ordinary observation changed: %+v %v", before, err)
	}
	after, err := ObserveHandleWithAvailability(context.Background(), h)
	if err != nil || h.reads != 1 || after.ProviderAvailability == nil || after.SessionID != "exact" || !after.Running || !after.Alive {
		t.Fatalf("opt-in observation=%+v err=%v reads=%d", after, err, h.reads)
	}
}

func TestProviderAvailabilityDoesNotMaskBaseObservationErrorOrProbeStoppedSession(t *testing.T) {
	for _, obs := range []LiveObservation{{Running: false}, {Running: true, Suspended: true}} {
		h := &availabilityObservationHandle{observation: obs}
		got, err := ObserveHandleWithAvailability(context.Background(), h)
		if err != nil || !reflect.DeepEqual(got, obs) || h.reads != 0 {
			t.Fatalf("unneeded availability read=%+v %v", got, err)
		}
	}
	want := errors.New("runtime observation unavailable")
	h := &availabilityObservationHandle{observation: LiveObservation{Running: true}, err: want}
	if _, err := ObserveHandleWithAvailability(context.Background(), h); !errors.Is(err, want) || h.reads != 0 {
		t.Fatalf("base error changed: %v", err)
	}
}

type availabilityRuntimeFixture struct {
	*runtime.Fake
	request runtime.ProviderAvailabilityRequest
	name    string
	onRead  func()
}

func (p *availabilityRuntimeFixture) ObserveProviderAvailability(_ context.Context, name string, req runtime.ProviderAvailabilityRequest) *runtime.ProviderAvailability {
	p.request, p.name = req, name
	if p.onRead != nil {
		p.onRead()
	}
	return &runtime.ProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", ObservedAt: time.Now().UTC(), RuntimeGeneration: req.Fence.Digest()}
}

func TestProviderAvailabilityUsesExactSessionFamilyAndExistingIncarnationFence(t *testing.T) {
	sp := &availabilityRuntimeFixture{Fake: runtime.NewFake()}
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), sp, sessionpkg.WithCityPath(t.TempDir()))
	h, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{Profile: ProfileCodexTmuxCLI, Template: "probe", Title: "Probe", Command: "codex", WorkDir: t.TempDir(), Provider: "custom-alias"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Get(h.currentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	fence, ok := sessionpkg.InputFenceFor(info)
	if !ok {
		t.Fatal("started session has no fence")
	}
	sp.Calls = nil
	obs, err := ObserveHandleWithAvailability(context.Background(), h)
	if err != nil || obs.ProviderAvailability == nil || obs.SessionID != info.ID {
		t.Fatalf("observation=%+v %v", obs, err)
	}
	if sp.request.Family != "codex" || sp.request.Fence != fence || sp.name != info.SessionName || obs.ProviderAvailability.RuntimeGeneration != fence.Digest() {
		t.Fatalf("wrong bound observation: %+v %q %+v", sp.request, sp.name, obs.ProviderAvailability)
	}
	if calls := typedCalls(sp.Fake, info.SessionName); len(calls) != 0 {
		t.Fatalf("availability mutated runtime: %+v", calls)
	}
}

// A live worker handle may be rebound by concurrent lifecycle reconciliation.
// Its current diagnostic must never be grafted onto the prior SID observation.
func TestProviderAvailabilityRejectsSessionReassignmentBeforeAndDuringProbe(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before probe", true: "during capture"}[during], func(t *testing.T) {
			sp := &availabilityRuntimeFixture{Fake: runtime.NewFake()}
			manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), sp, sessionpkg.WithCityPath(t.TempDir()))
			newHandle := func(title string) *SessionHandle {
				t.Helper()
				h, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{Profile: ProfileCodexTmuxCLI, Template: title, Title: title, Command: "codex", WorkDir: t.TempDir(), Provider: "codex"}})
				if err != nil {
					t.Fatal(err)
				}
				if err = h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
					t.Fatal(err)
				}
				return h
			}
			h, next := newHandle("first"), newHandle("next")
			observedID := h.currentSessionID()
			rebind := func() { h.mu.Lock(); h.sessionID = next.currentSessionID(); h.mu.Unlock() }
			if during {
				sp.onRead = rebind
			} else {
				rebind()
			}
			got := h.ProviderAvailability(context.Background(), observedID)
			if got == nil || got.State != "unknown" || got.Code != "" || got.RuntimeGeneration != "" {
				t.Fatalf("reassigned handle leaked another/current generation diagnostic: %+v", got)
			}
			if !during && sp.name != "" {
				t.Fatalf("mismatched SID was captured: %q", sp.name)
			}
		})
	}
}
