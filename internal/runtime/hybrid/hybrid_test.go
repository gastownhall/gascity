package hybrid

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func isRemote(name string) bool { return strings.Contains(name, "remote-agent") }

type livenessObservationErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *livenessObservationErrorProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, p.err
}

func TestProvider_ForwardsLivenessObservationErrorToRoutedBackend(t *testing.T) {
	wantErr := errors.New("snapshot unavailable")
	local := &livenessObservationErrorProvider{Fake: runtime.NewFake(), err: wantErr}
	remote := &livenessObservationErrorProvider{Fake: runtime.NewFake(), err: wantErr}
	h := New(local, remote, isRemote)

	for _, name := range []string{"local-agent", "remote-agent-1"} {
		got, err := h.ObserveLivenessWithError(name, nil)
		if !errors.Is(err, wantErr) {
			t.Fatalf("ObserveLivenessWithError(%q) error = %v, want %v", name, err, wantErr)
		}
		if got != (runtime.Liveness{}) {
			t.Fatalf("ObserveLivenessWithError(%q) = %+v, want zero while routed result is unknown", name, got)
		}
	}
}

// Relaunch must reach the routed backend (local vs remote), or the reconciler's
// RelaunchProvider type-assert would be masked by the hybrid router and fall
// back to Stop+Start.
func TestProvider_ForwardsRelaunchToRoutedBackend(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)
	if err := local.Start(context.Background(), "local-agent", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(local): %v", err)
	}
	if err := remote.Start(context.Background(), "remote-agent-1", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(remote): %v", err)
	}

	if err := h.Relaunch(context.Background(), "local-agent", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(local): %v", err)
	}
	if got := local.CountCalls("Relaunch", "local-agent"); got != 1 {
		t.Errorf("local backend Relaunch calls = %d, want 1", got)
	}
	if err := h.Relaunch(context.Background(), "remote-agent-1", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(remote): %v", err)
	}
	if got := remote.CountCalls("Relaunch", "remote-agent-1"); got != 1 {
		t.Errorf("remote backend Relaunch calls = %d, want 1", got)
	}
}

func TestStart_RoutesToLocal(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	if err := h.Start(context.Background(), "local-agent", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if !local.IsRunning("local-agent") {
		t.Error("expected local to have session")
	}
	if remote.IsRunning("local-agent") {
		t.Error("remote should not have session")
	}
}

func TestStart_RoutesToRemote(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	if err := h.Start(context.Background(), "remote-agent-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if local.IsRunning("remote-agent-1") {
		t.Error("local should not have session")
	}
	if !remote.IsRunning("remote-agent-1") {
		t.Error("expected remote to have session")
	}
}

func TestListRunning_MergesBothBackends(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "gc-demo--local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "gc-demo--remote-agent-1", runtime.Config{})
	_ = h.Start(context.Background(), "gc-demo--remote-agent-2", runtime.Config{})

	names, err := h.ListRunning("gc-demo-")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 sessions, got %d: %v", len(names), names)
	}
}

func TestListRunning_PartialFailure(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFailFake()
	h := New(local, remote, isRemote)

	_ = local.Start(context.Background(), "gc-demo--local-agent", runtime.Config{})

	names, err := h.ListRunning("gc-demo-")
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning error = %v, want partial list error", err)
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 session from healthy backend, got %d", len(names))
	}
}

func TestListRunning_BothFail(t *testing.T) {
	local := runtime.NewFailFake()
	remote := runtime.NewFailFake()
	h := New(local, remote, isRemote)

	_, err := h.ListRunning("gc-demo-")
	if err == nil {
		t.Fatal("expected error when both backends fail")
	}
}

func TestAttach_RoutesCorrectly(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})

	if err := h.Attach("local-agent"); err != nil {
		t.Errorf("attach local: %v", err)
	}
	if err := h.Attach("remote-agent-1"); err != nil {
		t.Errorf("attach remote: %v", err)
	}

	// Verify calls went to correct backends.
	var localAttach, remoteAttach int
	for _, c := range local.Calls {
		if c.Method == "Attach" {
			localAttach++
		}
	}
	for _, c := range remote.Calls {
		if c.Method == "Attach" {
			remoteAttach++
		}
	}
	if localAttach != 1 {
		t.Errorf("expected 1 local attach, got %d", localAttach)
	}
	if remoteAttach != 1 {
		t.Errorf("expected 1 remote attach, got %d", remoteAttach)
	}
}

func TestStop_RoutesCorrectly(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "local-agent", runtime.Config{})
	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})

	if err := h.Stop("local-agent"); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop("remote-agent-1"); err != nil {
		t.Fatal(err)
	}

	if local.IsRunning("local-agent") {
		t.Error("local-agent should be stopped")
	}
	if remote.IsRunning("remote-agent-1") {
		t.Error("remote-agent-1 should be stopped")
	}
}

func TestPendingAndRespond_RouteToBackend(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	h := New(local, remote, isRemote)

	_ = h.Start(context.Background(), "remote-agent-1", runtime.Config{})
	remote.SetPendingInteraction("remote-agent-1", &runtime.PendingInteraction{RequestID: "req-1"})

	pending, err := h.Pending("remote-agent-1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}
	if err := h.Respond("remote-agent-1", runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := remote.Responses["remote-agent-1"]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("Responses = %#v, want single approve", got)
	}
}

func TestPendingUnsupportedWhenBackendLacksInteractionSupport(t *testing.T) {
	local := &runtimeNoInteractionProvider{Provider: runtime.NewFake()}
	remote := runtime.NewFake()
	h := New(local, remote, isRemote)

	_, err := h.Pending("local-agent")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

type runtimeNoInteractionProvider struct {
	runtime.Provider
}

type deadRuntimeCheckProvider struct {
	*runtime.Fake
	dead   map[string]bool
	errs   map[string]error
	checks []string
}

func newDeadRuntimeCheckProvider() *deadRuntimeCheckProvider {
	return &deadRuntimeCheckProvider{
		Fake: runtime.NewFake(),
		dead: make(map[string]bool),
		errs: make(map[string]error),
	}
}

func (p *deadRuntimeCheckProvider) IsDeadRuntimeSession(name string) (bool, error) {
	p.checks = append(p.checks, name)
	if err := p.errs[name]; err != nil {
		return false, err
	}
	return p.dead[name], nil
}

func TestIsDeadRuntimeSessionDelegatesToRoutedChecker(t *testing.T) {
	local := newDeadRuntimeCheckProvider()
	remote := newDeadRuntimeCheckProvider()
	remote.dead["remote-agent-1"] = true
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("remote-agent-1")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true from routed remote checker")
	}
	if len(local.checks) != 0 {
		t.Fatalf("local checks = %v, want none", local.checks)
	}
	if got := remote.checks; len(got) != 1 || got[0] != "remote-agent-1" {
		t.Fatalf("remote checks = %v, want [remote-agent-1]", got)
	}
}

func TestIsDeadRuntimeSessionReturnsFalseWhenRoutedBackendLacksChecker(t *testing.T) {
	local := runtime.NewFake()
	remote := newDeadRuntimeCheckProvider()
	remote.dead["local-agent"] = true
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("local-agent")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false for non-checker routed backend")
	}
	if len(remote.checks) != 0 {
		t.Fatalf("remote checks = %v, want none for local-routed session", remote.checks)
	}
}

func TestIsDeadRuntimeSessionReturnsRoutedCheckerError(t *testing.T) {
	local := newDeadRuntimeCheckProvider()
	remote := newDeadRuntimeCheckProvider()
	remote.errs["remote-agent-1"] = fmt.Errorf("runtime unavailable")
	h := New(local, remote, isRemote)

	dead, err := h.IsDeadRuntimeSession("remote-agent-1")
	if err == nil {
		t.Fatal("IsDeadRuntimeSession error = nil, want routed checker error")
	}
	if dead {
		t.Fatal("IsDeadRuntimeSession = true, want false on checker error")
	}
	if !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("IsDeadRuntimeSession error = %v, want runtime unavailable", err)
	}
}

// capsFake overrides the fake's capabilities so the intersection can be
// exercised with differing backend support.
type capsFake struct {
	*runtime.Fake
	caps runtime.ProviderCapabilities
}

func (c *capsFake) Capabilities() runtime.ProviderCapabilities { return c.caps }

// TestProvider_CapabilitiesIntersectsEachConnectionOp exercises one field at a
// time, in both backend orders, so a field cannot pass by being wired to the
// wrong field, the wrong backend, or with the wrong operator. Setting both
// fields on both backends, as an earlier shape did, leaves a CanStream wired to
// CanAttachTTY reporting the right answer for the wrong reason.
func TestProvider_CapabilitiesIntersectsEachConnectionOp(t *testing.T) {
	for _, field := range []string{"CanStream", "CanAttachTTY"} {
		for _, tc := range []struct {
			name          string
			first, second bool
			want          bool
		}{
			{name: "both backends capable", first: true, second: true, want: true},
			{name: "first backend only", first: true},
			{name: "second backend only", second: true},
			{name: "neither backend"},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				p := New(&capsFake{runtime.NewFake(), capsWith(t, field, tc.first)},
					&capsFake{runtime.NewFake(), capsWith(t, field, tc.second)}, isRemote)
				if got := capsField(t, p.Capabilities(), field); got != tc.want {
					t.Errorf("%s = %v, want %v", field, got, tc.want)
				}
			})
		}
	}
}

// capsWith returns capabilities with exactly one named bool field set, so each
// field's wiring is observed on its own.
func capsWith(t *testing.T, field string, v bool) runtime.ProviderCapabilities {
	t.Helper()
	var caps runtime.ProviderCapabilities
	f := reflect.ValueOf(&caps).Elem().FieldByName(field)
	if !f.IsValid() {
		t.Fatalf("runtime.ProviderCapabilities has no field %q", field)
	}
	f.SetBool(v)
	return caps
}

// capsField reads one named bool capability.
func capsField(t *testing.T, caps runtime.ProviderCapabilities, field string) bool {
	t.Helper()
	f := reflect.ValueOf(caps).FieldByName(field)
	if !f.IsValid() {
		t.Fatalf("runtime.ProviderCapabilities has no field %q", field)
	}
	return f.Bool()
}

// TestProvider_CapabilitiesIntersectsEveryField fails when a field is added to
// runtime.ProviderCapabilities and not wired into this composite. The
// intersection is a hand-maintained literal, and a field missing from it reads
// as "not supported" no matter what either backend reports, which is how
// CanStream and CanAttachTTY both went unnoticed: nothing consumes them yet, so
// the first consumer would have inherited the wrong answer with no test red.
//
// Both backends report everything true, so the check holds for the fields that
// intersect with AND and for NeedsClaimBackstop, which is an OR.
func TestProvider_CapabilitiesIntersectsEveryField(t *testing.T) {
	all := runtime.ProviderCapabilities{}
	set := reflect.ValueOf(&all).Elem()
	for i := 0; i < set.NumField(); i++ {
		if set.Field(i).Kind() != reflect.Bool {
			t.Fatalf("%s is not a bool, so this test no longer covers every capability", set.Type().Field(i).Name)
		}
		set.Field(i).SetBool(true)
	}

	got := reflect.ValueOf(New(&capsFake{runtime.NewFake(), all}, &capsFake{runtime.NewFake(), all}, isRemote).Capabilities())
	for i := 0; i < got.NumField(); i++ {
		if !got.Field(i).Bool() {
			t.Errorf("%s = false with both backends reporting it true: the field is missing from the intersection", got.Type().Field(i).Name)
		}
	}
}

// idleSnapshotProvider is a backend that can take a point-in-time idle
// observation. A plain runtime.Fake deliberately cannot, so it stands in for a
// backend without the capability.
type idleSnapshotProvider struct {
	*runtime.Fake
	idle  map[string]bool
	calls []string
}

func newIdleSnapshotProvider() *idleSnapshotProvider {
	return &idleSnapshotProvider{Fake: runtime.NewFake(), idle: make(map[string]bool)}
}

func (p *idleSnapshotProvider) SnapshotIdle(name string) (bool, error) {
	p.calls = append(p.calls, name)
	return p.idle[name], nil
}

// The composite must satisfy IdleSnapshotProvider itself, or the idle-timeout
// reconciler's type-assert fails for every session in a local/remote split
// city and the content-based idle clock silently never runs (ga-07mi8).
var _ runtime.IdleSnapshotProvider = (*Provider)(nil)

func TestSnapshotIdle_RoutesToBackend(t *testing.T) {
	local, remote := newIdleSnapshotProvider(), newIdleSnapshotProvider()
	h := New(local, remote, isRemote)
	local.idle["local-agent"] = true

	idle, err := h.SnapshotIdle("local-agent")
	if err != nil {
		t.Fatalf("SnapshotIdle(local-agent): %v", err)
	}
	if !idle {
		t.Error("SnapshotIdle(local-agent) = false, want true from the local backend")
	}
	if !reflect.DeepEqual(local.calls, []string{"local-agent"}) {
		t.Errorf("local backend SnapshotIdle calls = %v, want [local-agent]", local.calls)
	}
	if len(remote.calls) != 0 {
		t.Errorf("remote backend SnapshotIdle calls = %v, want none", remote.calls)
	}

	if _, err := h.SnapshotIdle("remote-agent-1"); err != nil {
		t.Fatalf("SnapshotIdle(remote-agent-1): %v", err)
	}
	if !reflect.DeepEqual(remote.calls, []string{"remote-agent-1"}) {
		t.Errorf("remote backend SnapshotIdle calls = %v, want [remote-agent-1]", remote.calls)
	}
}

func TestSnapshotIdle_FailsClosedWhenRouteCannotSnapshot(t *testing.T) {
	h := New(runtime.NewFake(), runtime.NewFake(), isRemote)

	idle, err := h.SnapshotIdle("local-agent")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("SnapshotIdle error = %v, want ErrInteractionUnsupported", err)
	}
	if idle {
		t.Error("SnapshotIdle = true on an unsupported route; must never report idle it could not observe")
	}
}

// fakeEventProvider adds a controllable SessionEventProvider to runtime.Fake
// so tests can drive both backends' streams independently.
type fakeEventProvider struct {
	*runtime.Fake
	ch chan runtime.SessionEvent
}

func newFakeEventProvider() *fakeEventProvider {
	return &fakeEventProvider{Fake: runtime.NewFake(), ch: make(chan runtime.SessionEvent, 4)}
}

func (f *fakeEventProvider) SubscribeSessionEvents(_ context.Context) (<-chan runtime.SessionEvent, error) {
	return f.ch, nil
}

var _ runtime.SessionEventProvider = (*fakeEventProvider)(nil)

// Both backends being event-capable must not mean only one is heard from:
// EventCapableRoute is checked per session, so a session routed to either
// backend needs its events to actually arrive.
func TestSubscribeSessionEvents_MergesBothEventCapableBackends(t *testing.T) {
	local := newFakeEventProvider()
	remote := newFakeEventProvider()
	h := New(local, remote, isRemote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	merged, err := h.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}

	local.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "local-sess"}
	remote.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "remote-agent-1"}

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case ev := <-merged:
			seen[ev.Session] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for merged events, got %v", seen)
		}
	}
	if !seen["local-sess"] || !seen["remote-agent-1"] {
		t.Errorf("merged events = %v, want both local-sess and remote-agent-1", seen)
	}
}

func TestSubscribeSessionEvents_NeitherBackendCapableErrors(t *testing.T) {
	h := New(runtime.NewFake(), runtime.NewFake(), isRemote)
	if _, err := h.SubscribeSessionEvents(context.Background()); err == nil {
		t.Fatal("SubscribeSessionEvents = nil error, want error when neither backend is event-capable")
	}
}

// A healthy backend's continuing traffic must not mask the other backend
// going silent, but the merge must not destroy itself to report that: it
// stays open and keeps forwarding the healthy side's events while
// MergedStreamStale (query-time, via the tracker) reports the silent side.
// Once the silent side resumes, staleness clears on its own — no restart
// needed, unlike the old close-on-stale behavior this replaces.
func TestMergeSessionEvents_StaysOpenAndReportsQueryTimeStalenessWhenOneSourceGoesSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan runtime.SessionEvent)
	b := make(chan runtime.SessionEvent)
	const staleAfter = 30 * time.Millisecond
	tracker := newSessionEventStaleTracker(staleAfter)
	merged := mergeSessionEvents(ctx, a, b, tracker)

	// b delivers once, then goes silent (transport wedged, channel never
	// closed — herdr's actual behavior on a broken transport).
	b <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "b-sess"}
	if ev := <-merged; ev.Session != "b-sess" {
		t.Fatalf("first event = %q, want b-sess", ev.Session)
	}

	// a keeps producing well past staleAfter; merged must NOT close, and
	// every one of a's events must still arrive.
	deadline := time.Now().Add(staleAfter * 5)
	sawStale := false
	for time.Now().Before(deadline) {
		select {
		case a <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "a-sess"}:
			select {
			case ev, ok := <-merged:
				if !ok {
					t.Fatal("merged stream closed despite b's silence and a's continuing traffic")
				}
				if ev.Session != "a-sess" {
					t.Fatalf("event = %q, want a-sess", ev.Session)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for a's event to be forwarded")
			}
		case <-time.After(time.Second):
			t.Fatal("timed out sending a's event")
		}
		if tracker.stale() {
			sawStale = true
		}
	}
	if !sawStale {
		t.Fatal("tracker.stale() never reported true while b was silent past staleAfter")
	}

	// b resumes: staleness must self-heal without any restart.
	select {
	case b <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "b-sess-2"}:
	case <-time.After(time.Second):
		t.Fatal("timed out resuming b")
	}
	select {
	case ev := <-merged:
		if ev.Session != "b-sess-2" {
			t.Fatalf("resumed event = %q, want b-sess-2", ev.Session)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for b's resumed event")
	}
	if tracker.stale() {
		t.Fatal("tracker.stale() still true after b resumed delivering")
	}
}

// TestProvider_SubscribeSessionEventsStale_RealSubscribeWiring exercises the
// actual wiring exposed via SubscribeSessionEventsStale's returned checker,
// not just the tracker in isolation.
// TestMergeSessionEvents_StaysOpenAndReportsQueryTimeStalenessWhenOneSourceGoesSilent
// above proves the tracker itself works, but a mutation that makes
// SubscribeSessionEventsStale return a checker that always reports false, or
// stops wiring the tracker returned by mergeSessionEvents into it, survives
// that test untouched: nothing calls SubscribeSessionEventsStale and then
// asks its returned checker (not the tracker directly) whether the merge is
// stale.
func TestProvider_SubscribeSessionEventsStale_RealSubscribeWiring(t *testing.T) {
	orig := sessionEventStaleAfter
	sessionEventStaleAfter = 20 * time.Millisecond
	defer func() { sessionEventStaleAfter = orig }()

	local := newFakeEventProvider()
	remote := newFakeEventProvider()
	h := New(local, remote, isRemote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	merged, stale, err := h.SubscribeSessionEventsStale(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEventsStale: %v", err)
	}
	if stale == nil {
		t.Fatal("SubscribeSessionEventsStale returned a nil checker for a dual-backend merge")
	}

	// Both sides emit: not stale.
	local.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "local-sess"}
	remote.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "remote-agent-1"}
	for i := 0; i < 2; i++ {
		select {
		case <-merged:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial merged events")
		}
	}
	if stale() {
		t.Fatal("stale() = true while both backends are emitting, want false")
	}

	// remote goes silent past the staleness window; local keeps emitting.
	deadline := time.Now().Add(sessionEventStaleAfter * 10)
	sawStale := false
	for time.Now().Before(deadline) && !sawStale {
		select {
		case local.ch <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "local-sess"}:
			select {
			case <-merged:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for local's event to be forwarded")
			}
		default:
		}
		if stale() {
			sawStale = true
		}
	}
	if !sawStale {
		t.Fatal("stale() never reported true after remote went silent")
	}
}

// multiSubEventProvider hands out a fresh channel on every
// SubscribeSessionEvents call, so independent subscribers each drive their
// own underlying stream instead of racing to consume the same channel.
type multiSubEventProvider struct {
	*runtime.Fake
	mu   sync.Mutex
	subs []chan runtime.SessionEvent
}

func newMultiSubEventProvider() *multiSubEventProvider {
	return &multiSubEventProvider{Fake: runtime.NewFake()}
}

func (f *multiSubEventProvider) SubscribeSessionEvents(_ context.Context) (<-chan runtime.SessionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan runtime.SessionEvent, 4)
	f.subs = append(f.subs, ch)
	return ch, nil
}

// sendToFirst delivers ev only to the first channel handed out, so a test
// can drive that one subscriber's stream while leaving a second
// subscriber's channel untouched (and undrained) without deadlocking on a
// full buffer.
func (f *multiSubEventProvider) sendToFirst(ev runtime.SessionEvent) {
	f.mu.Lock()
	ch := f.subs[0]
	f.mu.Unlock()
	ch <- ev
}

var _ runtime.SessionEventProvider = (*multiSubEventProvider)(nil)

// TestProvider_SubscribeSessionEventsStale_IndependentSubscribersDoNotClobber
// guards against a regression where a second, independent
// SubscribeSessionEventsStale call on the same Provider (e.g. cmd/gc's nudge
// dispatcher subscribing alongside the session-event pump) replaced a
// provider-wide staleness tracker, leaving the FIRST subscriber's checker
// silently reporting on the SECOND subscriber's merge instead of its own.
func TestProvider_SubscribeSessionEventsStale_IndependentSubscribersDoNotClobber(t *testing.T) {
	orig := sessionEventStaleAfter
	sessionEventStaleAfter = 20 * time.Millisecond
	defer func() { sessionEventStaleAfter = orig }()

	local := newMultiSubEventProvider()
	remote := newMultiSubEventProvider()
	h := New(local, remote, isRemote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First subscriber (e.g. the session-event pump).
	firstCh, firstStale, err := h.SubscribeSessionEventsStale(ctx)
	if err != nil {
		t.Fatalf("first SubscribeSessionEventsStale: %v", err)
	}

	// Both sides emit so the first subscriber's tracker is fresh.
	local.sendToFirst(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "local-sess"})
	remote.sendToFirst(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "remote-agent-1"})
	for i := 0; i < 2; i++ {
		select {
		case <-firstCh:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for first subscriber's initial merged events")
		}
	}
	if firstStale() {
		t.Fatal("first subscriber's checker = stale immediately after both backends emitted")
	}

	// A second, independent subscriber (e.g. the nudge dispatcher) subscribes
	// on the same Provider and never touches its channel again — it should
	// get its OWN tracker, not clobber the first subscriber's.
	secondCh, secondStale, err := h.SubscribeSessionEventsStale(ctx)
	if err != nil {
		t.Fatalf("second SubscribeSessionEventsStale: %v", err)
	}
	if secondCh == nil || secondStale == nil {
		t.Fatal("second subscription returned a nil channel or checker")
	}

	// The first subscriber's own merge keeps flowing on both sides; it must
	// never report stale, even while the second subscriber's own merge (fed
	// by no further sends here) would independently go stale.
	deadline := time.Now().Add(sessionEventStaleAfter * 10)
	sawSecondStale := false
	for time.Now().Before(deadline) && !sawSecondStale {
		local.sendToFirst(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "local-sess"})
		select {
		case <-firstCh:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for local's event on the first subscriber's channel")
		}
		remote.sendToFirst(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "remote-agent-1"})
		select {
		case <-firstCh:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for remote's event on the first subscriber's channel")
		}
		if firstStale() {
			t.Fatal("first subscriber's checker went stale even though its own merge kept receiving events on both sides; it must be reading its own tracker, not the second subscriber's")
		}
		if secondStale() {
			sawSecondStale = true
		}
	}
	if !sawSecondStale {
		t.Fatal("second subscriber's checker never went stale despite receiving nothing; the two subscriptions must not share one tracker")
	}
}
