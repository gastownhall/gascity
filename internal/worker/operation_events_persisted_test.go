package worker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The worker operation event names its session from the persisted record only.
// The live provider overlay (Manager.EnrichInfo) makes unbounded provider
// calls, so an event path that used it let a wedged provider hold a kill (on the
// reconciler tick goroutine) or a start (on an async limiter slot) before the
// operation itself ran. These tests pin that the event path issues no provider
// call of its own and still names the same identity.

// opEventSpec is the handle spec every test here builds.
func opEventSpec(t *testing.T) SessionSpec {
	t.Helper()
	return SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}
}

// opEventWorld is one handle over a started, Active session, with the provider
// call record cleared so a test sees only the calls its operation makes.
type opEventWorld struct {
	handle *SessionHandle
	sp     *runtime.Fake
}

func newOpEventWorld(t *testing.T, recorder events.Recorder) opEventWorld {
	t.Helper()
	handle, _, sp, _ := newTestSessionHandleWithRecorder(t, opEventSpec(t), recorder)
	if _, err := handle.Create(context.Background(), CreateModeStarted); err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	sp.Calls = nil
	return opEventWorld{handle: handle, sp: sp}
}

// providerMethods is the sorted multiset of provider method names called, so a
// comparison does not depend on call order.
func providerMethods(calls []runtime.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	sort.Strings(out)
	return out
}

// TestWorkerOperationEventAddsNoProviderCall runs each operation twice on
// identical worlds, once with no event recorder and once with one, and requires
// the same provider calls: emitting the event must not probe the provider.
func TestWorkerOperationEventAddsNoProviderCall(t *testing.T) {
	ops := []struct {
		name string
		run  func(t *testing.T, h *SessionHandle) error
	}{
		{"kill", func(t *testing.T, h *SessionHandle) error {
			return h.Kill(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{"stop", func(t *testing.T, h *SessionHandle) error {
			return h.Stop(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{"close", func(t *testing.T, h *SessionHandle) error {
			return h.Close(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{"rename", func(_ *testing.T, h *SessionHandle) error {
			return h.Rename(context.Background(), "Renamed")
		}},
		{"reset", func(_ *testing.T, h *SessionHandle) error {
			return h.Reset(context.Background())
		}},
		{"interrupt", func(_ *testing.T, h *SessionHandle) error {
			return h.Interrupt(context.Background(), InterruptRequest{})
		}},
		{"nudge", func(t *testing.T, h *SessionHandle) error {
			_, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryImmediate})
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			quiet := newOpEventWorld(t, nil)
			errQuiet := op.run(t, quiet.handle)

			recorder := &recordingEventRecorder{}
			loud := newOpEventWorld(t, recorder)
			errLoud := op.run(t, loud.handle)

			if (errQuiet == nil) != (errLoud == nil) {
				t.Fatalf("%s: error without recorder = %v, with recorder = %v; want the same outcome", op.name, errQuiet, errLoud)
			}
			if len(recorder.events) == 0 {
				t.Fatalf("%s: recorder saw no worker operation event; the comparison proves nothing", op.name)
			}
			want, got := providerMethods(quiet.sp.Calls), providerMethods(loud.sp.Calls)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("%s: provider calls with an event recorder = %v, without = %v; the event path must add none", op.name, got, want)
			}
		})
	}
}

// TestWorkerOperationStartEventAddsNoProviderCall is the start counterpart: a
// handle with no session yet creates one inside Start, so the finish-time
// identity read is the only event read there.
func TestWorkerOperationStartEventAddsNoProviderCall(t *testing.T) {
	start := func(t *testing.T, recorder events.Recorder) (*runtime.Fake, error) {
		t.Helper()
		handle, _, sp, _ := newTestSessionHandleWithRecorder(t, opEventSpec(t), recorder)
		err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent))
		return sp, err
	}
	quietSP, errQuiet := start(t, nil)
	recorder := &recordingEventRecorder{}
	loudSP, errLoud := start(t, recorder)
	if errQuiet != nil || errLoud != nil {
		t.Fatalf("Start errors = %v (no recorder), %v (recorder), want nil", errQuiet, errLoud)
	}
	if len(recorder.events) == 0 {
		t.Fatal("recorder saw no worker operation event; the comparison proves nothing")
	}
	want, got := providerMethods(quietSP.Calls), providerMethods(loudSP.Calls)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("start provider calls with an event recorder = %v, without = %v; the event path must add none", got, want)
	}
}

// TestOperationEventIdentityIssuesNoProviderCall pins the identity read itself:
// naming the event for an Active session calls the provider zero times.
func TestOperationEventIdentityIssuesNoProviderCall(t *testing.T) {
	w := newOpEventWorld(t, &recordingEventRecorder{})

	var payload operationEventPayload
	w.handle.populateOperationEventIdentity(&payload)

	if len(w.sp.Calls) != 0 {
		t.Fatalf("populateOperationEventIdentity made provider calls %v, want none", providerMethods(w.sp.Calls))
	}
	if payload.SessionID == "" || payload.SessionName == "" {
		t.Fatalf("identity not named: %+v", payload)
	}
}

// wedgedProvider blocks every liveness probe on wedge once armed, as a hung
// tmux server or ACP route does.
type wedgedProvider struct {
	*runtime.Fake
	armed atomic.Bool
	wedge chan struct{}
}

func (p *wedgedProvider) block() {
	if p.armed.Load() {
		<-p.wedge
	}
}

func (p *wedgedProvider) IsRunning(name string) bool {
	p.block()
	return p.Fake.IsRunning(name)
}

func (p *wedgedProvider) IsAttached(name string) bool {
	p.block()
	return p.Fake.IsAttached(name)
}

func (p *wedgedProvider) GetLastActivity(name string) (time.Time, error) {
	p.block()
	return p.Fake.GetLastActivity(name)
}

// TestOperationEventIdentityDoesNotWaitOnWedgedProvider is the incident shape:
// with every provider probe hung, the event still names its session.
func TestOperationEventIdentityDoesNotWaitOnWedgedProvider(t *testing.T) {
	store := beads.NewMemStore()
	sp := &wedgedProvider{Fake: runtime.NewFake(), wedge: make(chan struct{})}
	t.Cleanup(func() { close(sp.wedge) })
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:  manager,
		Recorder: &recordingEventRecorder{},
		Session:  opEventSpec(t),
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if _, err := handle.Create(context.Background(), CreateModeStarted); err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	sp.armed.Store(true)

	done := make(chan operationEventPayload, 1)
	go func() {
		var payload operationEventPayload
		handle.populateOperationEventIdentity(&payload)
		done <- payload
	}()
	select {
	case payload := <-done:
		if payload.SessionID == "" || payload.SessionName == "" {
			t.Fatalf("identity not named: %+v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("populateOperationEventIdentity blocked on a wedged provider")
	}
}

// TestSessionRecordPersistedMatchesEnrichedIdentity pins that dropping the
// overlay loses nothing the event names: the six identity fields and the
// persisted metadata agree with the enriched read.
func TestSessionRecordPersistedMatchesEnrichedIdentity(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := seedEquivSession(t, store, sp)
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp)

	enriched, enrichedPR, err := sessionRecordViaManager(manager, info.ID)
	if err != nil {
		t.Fatalf("sessionRecordViaManager: %v", err)
	}
	sp.Calls = nil
	persisted, persistedPR, err := sessionRecordPersisted(manager, info.ID)
	if err != nil {
		t.Fatalf("sessionRecordPersisted: %v", err)
	}
	if len(sp.Calls) != 0 {
		t.Fatalf("sessionRecordPersisted made provider calls %v, want none", providerMethods(sp.Calls))
	}
	fields := func(i sessionpkg.Info) [6]string {
		return [6]string{i.ID, i.SessionName, i.Provider, i.Template, i.AgentName, i.Alias}
	}
	if got, want := fields(persisted), fields(enriched); got != want {
		t.Fatalf("persisted identity fields = %v, want enriched %v", got, want)
	}
	if !reflect.DeepEqual(persistedPR.Metadata, enrichedPR.Metadata) {
		t.Fatalf("persisted metadata = %v, want %v", persistedPR.Metadata, enrichedPR.Metadata)
	}
}

// TestSessionRecordPersistedKeepsErrorContract pins the same 400/404 bridge as
// sessionRecordViaManager: a present non-session bead is ErrNotSession, an
// absent id stays on the beads.ErrNotFound chain.
func TestSessionRecordPersistedKeepsErrorContract(t *testing.T) {
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), runtime.NewFake())
	nonSession, err := store.Create(beads.Bead{Title: "task", Type: "task", Metadata: map[string]string{}})
	if err != nil {
		t.Fatalf("create non-session bead: %v", err)
	}

	if _, _, err = sessionRecordPersisted(manager, nonSession.ID); !errors.Is(err, sessionpkg.ErrNotSession) {
		t.Fatalf("present non-session error = %v, want errors.Is ErrNotSession", err)
	}
	if _, _, err = sessionRecordPersisted(manager, "does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("absent-id error = %v, want errors.Is beads.ErrNotFound", err)
	}
}

// TestWorkerOperationEventPayloadIdentityUnchanged pins the payload a consumer
// sees: identity and run id still come from the persisted record.
func TestWorkerOperationEventPayloadIdentityUnchanged(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, mgr := newTestSessionHandleWithRecorder(t, opEventSpec(t), recorder)
	info, err := handle.Create(context.Background(), CreateModeStarted)
	if err != nil {
		t.Fatalf("Create(started): %v", err)
	}
	recorder.events = nil

	if err := handle.Rename(context.Background(), "Renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	persisted, _, err := sessionRecordPersisted(mgr, info.ID)
	if err != nil {
		t.Fatalf("sessionRecordPersisted: %v", err)
	}
	if payload.SessionID != info.ID || payload.SessionName != persisted.SessionName {
		t.Fatalf("payload identity = (%q, %q), want (%q, %q)", payload.SessionID, payload.SessionName, info.ID, persisted.SessionName)
	}
	// The provider label is seeded when the event begins; the template is the
	// stored one.
	if payload.Provider != handle.providerLabel() || payload.Template != "probe" {
		t.Fatalf("payload provider/template = (%q, %q), want (%q, probe)", payload.Provider, payload.Template, handle.providerLabel())
	}
	// A manual chat has no run chain, so the session is its own run root.
	if payload.RunID != info.ID {
		t.Fatalf("payload.RunID = %q, want session id %q", payload.RunID, info.ID)
	}
}
