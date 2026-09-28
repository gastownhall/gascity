package hybrid

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
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
// going silent: the merge stays open and keeps forwarding the healthy
// side's events even after real (fake, via synctest) time has advanced well
// past runtime.SessionEventStaleAfter, the point at which the old
// close-on-stale behavior would have killed the whole merge. Earlier drafts
// of this test ran zero elapsed time and passed even with the old
// close-after-30s ticker restored, so it was proving nothing; this version
// drives synctest's fake clock past the actual threshold.
func TestMergeSessionEvents_StaysOpenWhenOneSourceGoesSilent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		a := make(chan runtime.SessionEvent)
		b := make(chan runtime.SessionEvent)
		merged := runtime.MergeSessionEvents(ctx, a, b)

		// b delivers once, then goes silent (transport wedged, channel never
		// closed — herdr's actual behavior on a broken transport).
		b <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "b-sess"}
		if ev := <-merged; ev.Session != "b-sess" {
			t.Fatalf("first event = %q, want b-sess", ev.Session)
		}

		// Let fake time pass well past the old close-on-stale threshold while
		// b stays silent. synctest.Wait blocks until every other goroutine in
		// the bubble is durably blocked, so this proves the merge goroutine
		// is still alive on its select rather than merely that nobody yet
		// observed a close.
		<-time.After(runtime.SessionEventStaleAfter * 2)
		synctest.Wait()

		// a must still be able to deliver past the staleness window.
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
				t.Fatal("timed out waiting for a's event to be forwarded past the staleness window")
			}
		case <-time.After(time.Second):
			t.Fatal("timed out sending a's event past the staleness window (merge goroutine likely stopped selecting on a after closing on staleness)")
		}

		// b resumes: the merge must still be able to forward its events too.
		select {
		case b <- runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: "b-sess-2"}:
		case <-time.After(time.Second):
			t.Fatal("timed out resuming b")
		}
		select {
		case ev, ok := <-merged:
			if !ok {
				t.Fatal("merged stream closed before b's resumed event arrived")
			}
			if ev.Session != "b-sess-2" {
				t.Fatalf("resumed event = %q, want b-sess-2", ev.Session)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for b's resumed event")
		}
	})
}
