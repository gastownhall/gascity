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

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// The tests below were added in review. They pin acceptance clauses the tests
// above leave open: ordered, name-bearing provider-call parity over every
// operation that names its session, with the operation's own event as proof the
// comparison is not vacuous; the transport probe; Kill ending on Stop; the
// persisted-only read's agreement with the enriched read across the records the
// live overlay changes; its empty-Type heal; and the payload per operation.

// opEventOp is one SessionHandle operation that names its session in a worker
// operation event. A fresh operation runs on a handle with no session yet, as a
// worker's first Start and StartResolved do; the others run on a started, Active
// session.
type opEventOp struct {
	name      string
	operation workerOperation
	fresh     bool
	run       func(t *testing.T, h *SessionHandle) error
}

// opEventOps lists the operations whose worker operation event reads identity.
func opEventOps() []opEventOp {
	return []opEventOp{
		{name: "start", operation: workerOperationStart, fresh: true, run: func(t *testing.T, h *SessionHandle) error {
			return h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent))
		}},
		{name: "start_resolved", operation: workerOperationStartResolved, fresh: true, run: func(t *testing.T, h *SessionHandle) error {
			return startResolved(t, h, "claude", runtime.Config{Command: "claude", WorkDir: t.TempDir()})
		}},
		{name: "stop", operation: workerOperationStop, run: func(t *testing.T, h *SessionHandle) error {
			return h.Stop(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{name: "stop_for_shutdown", operation: workerOperationStop, run: func(_ *testing.T, h *SessionHandle) error {
			return h.StopForShutdown(context.Background())
		}},
		{name: "kill", operation: workerOperationKill, run: func(t *testing.T, h *SessionHandle) error {
			return h.Kill(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{name: "close", operation: workerOperationClose, run: func(t *testing.T, h *SessionHandle) error {
			return h.Close(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{name: "attach", operation: workerOperationAttach, run: func(t *testing.T, h *SessionHandle) error {
			return h.Attach(context.Background(), handleActor(t, h, sessionpkg.ActorOperator))
		}},
		{name: "rename", operation: workerOperationRename, run: func(_ *testing.T, h *SessionHandle) error {
			return h.Rename(context.Background(), "Renamed")
		}},
		{name: "reset", operation: workerOperationReset, run: func(_ *testing.T, h *SessionHandle) error {
			return h.Reset(context.Background())
		}},
		{name: "interrupt", operation: workerOperationInterrupt, run: func(_ *testing.T, h *SessionHandle) error {
			return h.Interrupt(context.Background(), InterruptRequest{})
		}},
		{name: "nudge", operation: workerOperationNudge, run: func(t *testing.T, h *SessionHandle) error {
			_, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryImmediate})
			return err
		}},
	}
}

// newOpEventHandle is a handle over spec on a fresh fixture: started and Active
// for an operation that reads an existing record, or with no session yet when
// fresh. The provider call record is cleared, so a test sees only the calls of
// the operation it runs.
func newOpEventHandle(t *testing.T, spec SessionSpec, recorder events.Recorder, fresh bool) (*SessionHandle, *runtime.Fake) {
	t.Helper()
	handle, _, sp, _ := newTestSessionHandleWithRecorder(t, spec, recorder)
	if !fresh {
		if _, err := handle.Create(context.Background(), CreateModeStarted); err != nil {
			t.Fatalf("Create(started): %v", err)
		}
	}
	sp.Calls = nil
	return handle, sp
}

// callTrail is the ordered provider call record: each call's method and session
// name, in the order the provider saw them.
func callTrail(calls []runtime.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method+" "+c.Name)
	}
	return out
}

// lastOperationPayload is the payload of the last event the recorder holds.
func lastOperationPayload(t *testing.T, recorder *recordingEventRecorder) operationEventPayload {
	t.Helper()
	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	return payload
}

// TestWorkerOperationEventKeepsProviderCallTrail is the ordered form of the
// parity pin. Each operation runs on two identical fixtures, one whose handle
// has no event recorder and one with a live recorder, and the provider must see
// the same calls in the same order, by method and session name. The recorder
// must also end on the operation's own event, so a run whose operation recorded
// nothing cannot pass on the strength of the setup's events.
//
// The trail sees the overlay only where it probes liveness, which it does for an
// Active record. StartResolved runs Manager.StartRuntimeOnly, which does not
// mutate the row's persisted lifecycle metadata, so a fresh row is not Active
// when its event is read and the overlay reaches the provider only through
// transport detection, which the trail does not record;
// TestWorkerOperationStartResolvedEventAddsNoTransportProbe pins that operation.
func TestWorkerOperationEventKeepsProviderCallTrail(t *testing.T) {
	for _, op := range opEventOps() {
		t.Run(op.name, func(t *testing.T) {
			quiet, quietSP := newOpEventHandle(t, opEventSpec(t), nil, op.fresh)
			errQuiet := op.run(t, quiet)

			recorder := &recordingEventRecorder{}
			loud, loudSP := newOpEventHandle(t, opEventSpec(t), recorder, op.fresh)
			recorder.events = nil
			errLoud := op.run(t, loud)

			if (errQuiet == nil) != (errLoud == nil) {
				t.Fatalf("%s: error without recorder = %v, with recorder = %v; want the same outcome", op.name, errQuiet, errLoud)
			}
			if got := lastOperationPayload(t, recorder).Operation; got != string(op.operation) {
				t.Fatalf("%s: last recorded operation = %q, want %q", op.name, got, op.operation)
			}
			want, got := callTrail(quietSP.Calls), callTrail(loudSP.Calls)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("%s: provider calls with an event recorder = %v, without = %v; the event path must add none", op.name, got, want)
			}
		})
	}
}

// probeCountingProvider is a Fake that, like a tmux-backed provider, can name a
// session's transport and register an ACP route, and counts both, so a session
// read that reaches the provider's transport detection or route registration is
// visible here and not only through Fake.Calls.
type probeCountingProvider struct {
	*runtime.Fake
	transport string
	detected  atomic.Int32
	routed    atomic.Int32
}

// DetectTransport counts the call and answers with the configured transport.
func (p *probeCountingProvider) DetectTransport(string) string {
	p.detected.Add(1)
	return p.transport
}

// RouteACP counts the registration.
func (p *probeCountingProvider) RouteACP(string) { p.routed.Add(1) }

// Unroute is the registration's undo; nothing here waits on it.
func (p *probeCountingProvider) Unroute(string) {}

// TestWorkerOperationEventAddsNoTransportProbe runs the operations that end or
// stop a session on a record that names no transport, over a provider that
// answers DetectTransport with "acp". The event path must neither ask the
// provider for the transport nor register an ACP route, so the detector and
// route counts with a live recorder equal those without one.
func TestWorkerOperationEventAddsNoTransportProbe(t *testing.T) {
	probe := func(t *testing.T, op opEventOp, recorder events.Recorder) (detected, routed int32) {
		t.Helper()
		store := beads.NewMemStore()
		sp := &probeCountingProvider{Fake: runtime.NewFake()}
		manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithCityPath(t.TempDir()))
		handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Recorder: recorder, Session: opEventSpec(t)})
		if err != nil {
			t.Fatalf("NewSessionHandle: %v", err)
		}
		info, err := handle.Create(context.Background(), CreateModeStarted)
		if err != nil {
			t.Fatalf("Create(started): %v", err)
		}
		// The record names no transport, so an enriched read has to ask the
		// provider, and the provider names one.
		if err := store.SetMetadata(info.ID, "transport", ""); err != nil {
			t.Fatalf("SetMetadata(transport): %v", err)
		}
		sp.transport = "acp"
		sp.detected.Store(0)
		if _, _, err := sessionRecordViaManager(manager, info.ID); err != nil {
			t.Fatalf("sessionRecordViaManager: %v", err)
		}
		if sp.detected.Load() == 0 {
			t.Fatal("an enriched read never asked the provider for the transport; the fixture proves nothing")
		}
		sp.detected.Store(0)
		sp.routed.Store(0)
		if err := op.run(t, handle); err != nil {
			t.Logf("%s: %v", op.name, err)
		}
		return sp.detected.Load(), sp.routed.Load()
	}
	for _, op := range opEventOps() {
		switch op.name {
		case "stop", "stop_for_shutdown", "kill", "close":
		default:
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			quietDetected, quietRouted := probe(t, op, nil)
			loudDetected, loudRouted := probe(t, op, &recordingEventRecorder{})
			if loudDetected != quietDetected {
				t.Fatalf("%s: DetectTransport calls with an event recorder = %d, without = %d; the event path must add none", op.name, loudDetected, quietDetected)
			}
			if loudRouted != quietRouted {
				t.Fatalf("%s: ACP route registrations with an event recorder = %d, without = %d; the event path must add none", op.name, loudRouted, quietRouted)
			}
		})
	}
}

// TestWorkerOperationStartResolvedEventAddsNoTransportProbe is the transport pin
// for StartResolved. Manager.StartRuntimeOnly does not mutate the row's persisted
// lifecycle metadata, so a fresh row is not Active when the event is read and the
// live overlay reaches the provider there only through transport detection and
// ACP routing, which the provider call trail does not record. On a record that
// names no transport, over a provider that names "acp", the detector and route
// counts with a live recorder must equal those without one.
func TestWorkerOperationStartResolvedEventAddsNoTransportProbe(t *testing.T) {
	probe := func(t *testing.T, recorder events.Recorder) (detected, routed int32, err error) {
		t.Helper()
		store := beads.NewMemStore()
		sp := &probeCountingProvider{Fake: runtime.NewFake()}
		manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp, sessionpkg.WithCityPath(t.TempDir()))
		handle, herr := NewSessionHandle(SessionHandleConfig{Manager: manager, Recorder: recorder, Session: opEventSpec(t)})
		if herr != nil {
			t.Fatalf("NewSessionHandle: %v", herr)
		}
		// The steps of startResolved, so the counts below are StartResolved's own.
		by := startLeaseActor(t, handle)
		id := handle.currentSessionID()
		info, gerr := manager.Get(id)
		if gerr != nil {
			t.Fatalf("Get(%s): %v", id, gerr)
		}
		inc, merr := manager.PersistedStore().MintIncarnation(context.Background(), by.Lease, sessionpkg.MintRequest{Expect: info, By: sessionpkg.ActorController, Now: time.Now()})
		if merr != nil {
			t.Fatalf("minting %s: %v", id, merr)
		}
		// The record names no transport, so an enriched read has to ask the
		// provider, and the provider names one. A fresh deferred row also carries
		// pending_create_claim, which makes the overlay resolve the transport from
		// config and skip the detector (transportForInfo), so it is cleared too.
		for _, key := range []string{"transport", "pending_create_claim"} {
			if serr := store.SetMetadata(id, key, ""); serr != nil {
				t.Fatalf("SetMetadata(%s): %v", key, serr)
			}
		}
		sp.transport = "acp"
		sp.detected.Store(0)
		if _, _, rerr := sessionRecordViaManager(manager, id); rerr != nil {
			t.Fatalf("sessionRecordViaManager: %v", rerr)
		}
		if sp.detected.Load() == 0 {
			t.Fatal("an enriched read never asked the provider for the transport; the fixture proves nothing")
		}
		sp.detected.Store(0)
		sp.routed.Store(0)
		err = handle.StartResolved(context.Background(), by, inc, "claude", runtime.Config{Command: "claude", WorkDir: t.TempDir()})
		return sp.detected.Load(), sp.routed.Load(), err
	}
	quietDetected, quietRouted, errQuiet := probe(t, nil)
	recorder := &recordingEventRecorder{}
	loudDetected, loudRouted, errLoud := probe(t, recorder)
	t.Logf("StartResolved error without recorder = %v, with recorder = %v", errQuiet, errLoud)

	if (errQuiet == nil) != (errLoud == nil) {
		t.Fatalf("error without recorder = %v, with recorder = %v; want the same outcome", errQuiet, errLoud)
	}
	if got := lastOperationPayload(t, recorder).Operation; got != string(workerOperationStartResolved) {
		t.Fatalf("last recorded operation = %q, want %q", got, workerOperationStartResolved)
	}
	if loudDetected != quietDetected {
		t.Fatalf("DetectTransport calls with an event recorder = %d, without = %d; the event path must add none", loudDetected, quietDetected)
	}
	if loudRouted != quietRouted {
		t.Fatalf("ACP route registrations with an event recorder = %d, without = %d; the event path must add none", loudRouted, quietRouted)
	}
}

// TestWorkerOperationKillEventEndsOnStop is the strict form of the cmd/gc kill
// test, which tolerates a trailing probe: with a live recorder the last provider
// call of Kill is its Stop of the session, because the event names its session
// from the persisted record and so makes no call after the runtime mutation.
func TestWorkerOperationKillEventEndsOnStop(t *testing.T) {
	recorder := &recordingEventRecorder{}
	w := newOpEventWorld(t, recorder)
	recorder.events = nil

	if err := w.handle.Kill(context.Background(), handleActor(t, w.handle, sessionpkg.ActorOperator)); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	calls := append([]runtime.Call(nil), w.sp.Calls...)
	if got := lastOperationPayload(t, recorder).Operation; got != string(workerOperationKill) {
		t.Fatalf("last recorded operation = %q, want %q", got, workerOperationKill)
	}
	persisted, _, err := sessionRecordPersisted(w.handle.manager, w.handle.currentSessionID())
	if err != nil {
		t.Fatalf("sessionRecordPersisted: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("Kill made no provider call")
	}
	if last := calls[len(calls)-1]; last.Method != "Stop" || last.Name != persisted.SessionName {
		t.Fatalf("Kill's last provider call = %s %q, want Stop %q; calls = %v", last.Method, last.Name, persisted.SessionName, callTrail(calls))
	}
}

// recordRead is one read of a session record: the Info, what a worker operation
// event names from it (id, session name, provider, template, agent name, alias,
// and the run id the event derives from the record's metadata), and how many
// DetectTransport and RouteACP calls the read made.
type recordRead struct {
	info     sessionpkg.Info
	identity [7]string
	detected int32
	routed   int32
}

// readRecordCounting runs read on id and reports what it returned and what it
// cost the provider.
func readRecordCounting(t *testing.T, sp *probeCountingProvider, manager *sessionpkg.Manager, id string, read func(*sessionpkg.Manager, string) (sessionpkg.Info, sessionpkg.PersistedResponse, error)) recordRead {
	t.Helper()
	detected, routed := sp.detected.Load(), sp.routed.Load()
	info, pr, err := read(manager, id)
	if err != nil {
		t.Fatalf("read %q: %v", id, err)
	}
	return recordRead{
		info:     info,
		identity: [7]string{info.ID, info.SessionName, info.Provider, info.Template, info.AgentName, info.Alias, beadmeta.ResolveRunID(pr.Metadata, info.ID, info.ID)},
		detected: sp.detected.Load() - detected,
		routed:   sp.routed.Load() - routed,
	}
}

// TestSessionRecordPersistedMatchesEnrichedIdentityMatrix pins the invariant that
// makes the persisted-only read safe for the event. Over the records the live
// overlay changes (an Active record on a provider that reports it not running, a
// provider that names another transport, a closed record, a record with ACP
// transport metadata) the persisted read and sessionRecordViaManager agree on the
// id, session name, provider, template, agent name, alias and run id, while the
// fields the overlay owns really do move, so the overlay is real and the
// identity ignores it.
func TestSessionRecordPersistedMatchesEnrichedIdentityMatrix(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, store *beads.MemStore, sp *probeCountingProvider, id string)
		check func(t *testing.T, persisted, enriched recordRead)
	}{
		{
			name: "active record on a provider that reports it not running",
			setup: func(t *testing.T, store *beads.MemStore, _ *probeCountingProvider, id string) {
				if err := store.SetMetadata(id, "state", string(sessionpkg.StateActive)); err != nil {
					t.Fatalf("SetMetadata(state): %v", err)
				}
			},
			check: func(t *testing.T, persisted, enriched recordRead) {
				if persisted.info.State != sessionpkg.StateActive || enriched.info.State != sessionpkg.StateAsleep {
					t.Fatalf("state persisted/enriched = %q/%q, want %q/%q", persisted.info.State, enriched.info.State, sessionpkg.StateActive, sessionpkg.StateAsleep)
				}
			},
		},
		{
			name: "provider names another transport",
			setup: func(_ *testing.T, _ *beads.MemStore, sp *probeCountingProvider, _ string) {
				sp.transport = "acp"
			},
			check: func(t *testing.T, persisted, enriched recordRead) {
				if persisted.info.Transport != "" || enriched.info.Transport != "acp" {
					t.Fatalf("transport persisted/enriched = %q/%q, want \"\"/acp", persisted.info.Transport, enriched.info.Transport)
				}
				if persisted.detected != 0 || enriched.detected == 0 {
					t.Fatalf("DetectTransport calls persisted/enriched = %d/%d, want 0/>0", persisted.detected, enriched.detected)
				}
			},
		},
		{
			name: "closed record",
			setup: func(t *testing.T, store *beads.MemStore, _ *probeCountingProvider, id string) {
				if err := store.Close(id); err != nil {
					t.Fatalf("Close: %v", err)
				}
			},
			check: func(t *testing.T, persisted, enriched recordRead) {
				if !persisted.info.Closed || !enriched.info.Closed {
					t.Fatalf("closed persisted/enriched = %v/%v, want true/true", persisted.info.Closed, enriched.info.Closed)
				}
			},
		},
		{
			name: "record with ACP transport metadata",
			setup: func(t *testing.T, store *beads.MemStore, _ *probeCountingProvider, id string) {
				if err := store.SetMetadata(id, "transport", "acp"); err != nil {
					t.Fatalf("SetMetadata(transport): %v", err)
				}
			},
			check: func(t *testing.T, persisted, enriched recordRead) {
				if persisted.info.Transport != "acp" || enriched.info.Transport != "acp" {
					t.Fatalf("transport persisted/enriched = %q/%q, want acp/acp", persisted.info.Transport, enriched.info.Transport)
				}
				if persisted.routed != 0 || enriched.routed == 0 {
					t.Fatalf("ACP route registrations persisted/enriched = %d/%d, want 0/>0", persisted.routed, enriched.routed)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := &probeCountingProvider{Fake: runtime.NewFake()}
			info := seedEquivSession(t, store, sp)
			for k, v := range map[string]string{
				// The seeded start-pending record carries pending_create_claim,
				// which makes the overlay skip the provider's transport detection;
				// clearing it leaves the overlay's provider reads as the thing
				// under test.
				"pending_create_claim": "",
				"agent_name":           "matrix-agent",
				"alias":                "matrix-alias",
			} {
				if err := store.SetMetadata(info.ID, k, v); err != nil {
					t.Fatalf("SetMetadata(%s): %v", k, err)
				}
			}
			tc.setup(t, store, sp, info.ID)
			manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), sp)

			persisted := readRecordCounting(t, sp, manager, info.ID, sessionRecordPersisted)
			enriched := readRecordCounting(t, sp, manager, info.ID, sessionRecordViaManager)

			// A session row carries none of the run-chain keys (the session field
			// registry has no row for workflow_id, molecule_id or gc.root_bead_id,
			// and its write guard refuses them), so the run id is the session id.
			want := [7]string{info.ID, info.SessionName, "legacy-provider", "worker", "matrix-agent", "matrix-alias", info.ID}
			if persisted.identity != want {
				t.Fatalf("persisted identity = %v, want %v", persisted.identity, want)
			}
			if enriched.identity != want {
				t.Fatalf("enriched identity = %v, want %v", enriched.identity, want)
			}
			tc.check(t, persisted, enriched)
		})
	}
}

// TestSessionRecordPersistedHealsEmptyType pins the heal sessionRecordViaManager
// does, on the persisted-only read too: a record that carries the session label
// but an empty Type reads as a session, and its Type is written back to the store.
func TestSessionRecordPersistedHealsEmptyType(t *testing.T) {
	reads := []struct {
		name string
		read func(*sessionpkg.Manager, string) (sessionpkg.Info, sessionpkg.PersistedResponse, error)
	}{
		{name: "persisted", read: sessionRecordPersisted},
		{name: "enriched", read: sessionRecordViaManager},
	}
	for _, rd := range reads {
		t.Run(rd.name, func(t *testing.T) {
			store := beads.NewMemStore()
			manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), runtime.NewFake())
			created, err := store.Create(beads.Bead{
				Title:    "repairable",
				Labels:   []string{sessionpkg.LabelSession},
				Metadata: map[string]string{"session_name": "repairable"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			empty := ""
			if err := store.Update(created.ID, beads.UpdateOpts{Type: &empty}); err != nil {
				t.Fatalf("Update(Type): %v", err)
			}
			if b, err := store.Get(created.ID); err != nil || b.Type != "" {
				t.Fatalf("fixture Type = %q (err %v), want empty", b.Type, err)
			}

			info, _, err := rd.read(manager, created.ID)
			if err != nil {
				t.Fatalf("%s read: %v", rd.name, err)
			}
			if info.Type != sessionpkg.BeadType {
				t.Fatalf("%s read Type = %q, want %q", rd.name, info.Type, sessionpkg.BeadType)
			}
			if b, err := store.Get(created.ID); err != nil || b.Type != sessionpkg.BeadType {
				t.Fatalf("stored Type after the %s read = %q (err %v), want %q", rd.name, b.Type, err, sessionpkg.BeadType)
			}
		})
	}
}

// TestWorkerOperationEventPayloadNamesPersistedIdentity runs every operation with
// a live recorder over a record that carries an agent name, and requires the
// recorded payload's session id, session name, provider label, template, agent
// name and run id to be the persisted record's. A session row carries none of the
// run-chain keys (the session field registry has no row for workflow_id,
// molecule_id or gc.root_bead_id, and its write guard refuses them), so the run
// id is the session id.
func TestWorkerOperationEventPayloadNamesPersistedIdentity(t *testing.T) {
	for _, op := range opEventOps() {
		t.Run(op.name, func(t *testing.T) {
			spec := opEventSpec(t)
			spec.Metadata = map[string]string{"agent_name": "payload-agent"}
			recorder := &recordingEventRecorder{}
			handle, _ := newOpEventHandle(t, spec, recorder, op.fresh)
			persisted := func() (sessionpkg.Info, sessionpkg.PersistedResponse) {
				t.Helper()
				info, pr, err := sessionRecordPersisted(handle.manager, handle.currentSessionID())
				if err != nil {
					t.Fatalf("sessionRecordPersisted: %v", err)
				}
				return info, pr
			}
			// A closed session is read before the operation that closes it.
			var info sessionpkg.Info
			var pr sessionpkg.PersistedResponse
			if !op.fresh {
				info, pr = persisted()
			}
			recorder.events = nil
			if err := op.run(t, handle); err != nil {
				t.Logf("%s: %v", op.name, err)
			}
			if op.fresh {
				info, pr = persisted()
			}

			payload := lastOperationPayload(t, recorder)
			if payload.Operation != string(op.operation) {
				t.Fatalf("last recorded operation = %q, want %q", payload.Operation, op.operation)
			}
			if info.AgentName != "payload-agent" {
				t.Fatalf("persisted agent name = %q, want payload-agent; the fixture does not carry it", info.AgentName)
			}
			if handle.providerLabel() == "" {
				t.Fatal("handle provider label is empty; the provider check proves nothing")
			}
			got := [6]string{payload.SessionID, payload.SessionName, payload.Provider, payload.Template, payload.AgentName, payload.RunID}
			want := [6]string{info.ID, info.SessionName, handle.providerLabel(), "probe", "payload-agent", beadmeta.ResolveRunID(pr.Metadata, info.ID, info.ID)}
			if got != want {
				t.Fatalf("payload identity (id, name, provider, template, agent, run) = %v, want the persisted %v", got, want)
			}
			if payload.RunID != info.ID {
				t.Fatalf("payload.RunID = %q, want the session id %q", payload.RunID, info.ID)
			}
		})
	}
}
