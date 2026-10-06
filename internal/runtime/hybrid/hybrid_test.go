package hybrid

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func isRemote(name string) bool { return strings.Contains(name, "remote-agent") }

type unattendedStopCall struct {
	name          string
	expectedToken string
}

type unattendedStopperProvider struct {
	runtime.Provider
	calls []unattendedStopCall
	err   error
}

func newUnattendedStopperProvider(err error) *unattendedStopperProvider {
	return &unattendedStopperProvider{Provider: runtime.NewFake(), err: err}
}

func (p *unattendedStopperProvider) StopUnattendedSession(name, expectedToken string) error {
	p.calls = append(p.calls, unattendedStopCall{name: name, expectedToken: expectedToken})
	return p.err
}

type freshLivenessProvider struct {
	*runtime.Fake
	calls       []runtime.LivenessTarget
	observation runtime.Liveness
}

func newFreshLivenessProvider(observation runtime.Liveness) *freshLivenessProvider {
	return &freshLivenessProvider{Fake: runtime.NewFake(), observation: observation}
}

func (p *freshLivenessProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	p.calls = append(p.calls, target)
	return p.observation
}

func TestProviderObserveFreshLivenessRoutesOnlySelectedBackend(t *testing.T) {
	want := runtime.Liveness{Running: true, Alive: true, Complete: true}
	target := runtime.LivenessTarget{
		SessionID:            "durable-session-id",
		SessionName:          "local-agent",
		ProcessNames:         []string{"agent"},
		IncarnationStartedAt: time.Unix(123, 0),
	}

	t.Run("local", func(t *testing.T) {
		local := newFreshLivenessProvider(want)
		remote := newFreshLivenessProvider(runtime.Liveness{Complete: true})
		p := New(local, remote, isRemote)

		if got := runtime.ObserveFreshLiveness(p, target); got != want {
			t.Fatalf("ObserveFreshLiveness(local) = %#v, want %#v", got, want)
		}
		if got := local.calls; len(got) != 1 || !reflect.DeepEqual(got[0], target) {
			t.Fatalf("local fresh observations = %#v, want one exact target", got)
		}
		if got := remote.calls; len(got) != 0 {
			t.Fatalf("remote fresh observations = %#v, want no fallback probe", got)
		}
		if got := remote.SnapshotCalls(); len(got) != 0 {
			t.Fatalf("remote backend calls = %#v, want untouched backend", got)
		}
	})

	t.Run("remote", func(t *testing.T) {
		local := newFreshLivenessProvider(runtime.Liveness{Complete: true})
		remote := newFreshLivenessProvider(want)
		remoteTarget := target
		remoteTarget.SessionName = "remote-agent-1"
		p := New(local, remote, isRemote)

		if got := runtime.ObserveFreshLiveness(p, remoteTarget); got != want {
			t.Fatalf("ObserveFreshLiveness(remote) = %#v, want %#v", got, want)
		}
		if got := remote.calls; len(got) != 1 || !reflect.DeepEqual(got[0], remoteTarget) {
			t.Fatalf("remote fresh observations = %#v, want one exact target", got)
		}
		if got := local.calls; len(got) != 0 {
			t.Fatalf("local fresh observations = %#v, want no fallback probe", got)
		}
		if got := local.SnapshotCalls(); len(got) != 0 {
			t.Fatalf("local backend calls = %#v, want untouched backend", got)
		}
	})

	t.Run("unsupported selected backend is incomplete without probing remote", func(t *testing.T) {
		remote := newFreshLivenessProvider(want)
		p := New(runtime.NewFake(), remote, isRemote)

		if got := runtime.ObserveFreshLiveness(p, target); got.Complete {
			t.Fatalf("ObserveFreshLiveness(unsupported local) = %#v, want incomplete", got)
		}
		if got := remote.calls; len(got) != 0 {
			t.Fatalf("remote fresh observations = %#v, want no fallback probe", got)
		}
		if got := remote.SnapshotCalls(); len(got) != 0 {
			t.Fatalf("remote backend calls = %#v, want untouched backend", got)
		}
	})
}

func TestProviderStopUnattendedSessionRoutesOnlySelectedBackend(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		local := newUnattendedStopperProvider(nil)
		remote := newUnattendedStopperProvider(nil)
		p := New(local, remote, isRemote)

		if err := p.StopUnattendedSession("local-agent", "token-local"); err != nil {
			t.Fatalf("StopUnattendedSession(local): %v", err)
		}
		if got := local.calls; len(got) != 1 || got[0] != (unattendedStopCall{name: "local-agent", expectedToken: "token-local"}) {
			t.Fatalf("local unattended stops = %#v, want exact local-agent/token-local call", got)
		}
		if got := remote.calls; len(got) != 0 {
			t.Fatalf("remote unattended stops = %#v, want none", got)
		}
	})

	t.Run("remote", func(t *testing.T) {
		local := newUnattendedStopperProvider(nil)
		remote := newUnattendedStopperProvider(nil)
		p := New(local, remote, isRemote)

		if err := p.StopUnattendedSession("remote-agent-1", "token-remote"); err != nil {
			t.Fatalf("StopUnattendedSession(remote): %v", err)
		}
		if got := remote.calls; len(got) != 1 || got[0] != (unattendedStopCall{name: "remote-agent-1", expectedToken: "token-remote"}) {
			t.Fatalf("remote unattended stops = %#v, want exact remote-agent-1/token-remote call", got)
		}
		if got := local.calls; len(got) != 0 {
			t.Fatalf("local unattended stops = %#v, want none", got)
		}
	})

	t.Run("unsupported local does not probe remote", func(t *testing.T) {
		remote := newUnattendedStopperProvider(nil)
		p := New(runtime.NewFake(), remote, isRemote)

		err := p.StopUnattendedSession("local-agent", "token")
		if err == nil || !strings.Contains(err.Error(), "local backend") {
			t.Fatalf("StopUnattendedSession error = %v, want contextual local-backend error", err)
		}
		if got := remote.calls; len(got) != 0 {
			t.Fatalf("remote unattended stops = %#v, want no fallback probe", got)
		}
	})

	t.Run("remote error does not probe local", func(t *testing.T) {
		sentinel := errors.New("remote unattended stop unavailable")
		local := newUnattendedStopperProvider(nil)
		remote := newUnattendedStopperProvider(sentinel)
		p := New(local, remote, isRemote)

		err := p.StopUnattendedSession("remote-agent-1", "token")
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "remote backend") {
			t.Fatalf("StopUnattendedSession error = %v, want wrapped contextual remote error", err)
		}
		if got := local.calls; len(got) != 0 {
			t.Fatalf("local unattended stops = %#v, want no fallback probe", got)
		}
	})
}

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

// TestHybridForwardsIsAttachedWithError proves hybrid forwards the
// error-bearing attachment probe to the routed backend. Without the forward,
// the error is lost behind the bool IsAttached and a probe failure reads
// "not attached".
func TestHybridForwardsIsAttachedWithError(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	localErr := fmt.Errorf("local probe: %w", runtime.ErrRuntimeUnavailable)
	remoteErr := fmt.Errorf("remote probe: %w", runtime.ErrRuntimeUnavailable)
	local.AttachedErrors["local-agent"] = localErr
	remote.AttachedErrors["remote-agent-1"] = remoteErr
	remote.SetAttached("remote-agent-2", true)
	h := New(local, remote, isRemote)

	for _, tc := range []struct {
		name    string
		want    bool
		wantErr error
	}{
		{"local-agent", false, localErr},
		{"remote-agent-1", false, remoteErr},
		{"remote-agent-2", true, nil},
	} {
		got, err := runtime.IsAttachedWithError(h, tc.name)
		if got != tc.want || !errors.Is(err, tc.wantErr) {
			t.Errorf("IsAttachedWithError(%q) = (%v, %v), want (%v, %v)", tc.name, got, err, tc.want, tc.wantErr)
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

type livenessInvalidatorStub struct {
	*runtime.Fake
	invalidations []string
}

func (s *livenessInvalidatorStub) InvalidateLiveness(name string) {
	s.invalidations = append(s.invalidations, name)
}

func TestProvider_InvalidatesLivenessOnRoutedBackend(t *testing.T) {
	local := &livenessInvalidatorStub{Fake: runtime.NewFake()}
	remote := &livenessInvalidatorStub{Fake: runtime.NewFake()}
	h := New(local, remote, isRemote)

	h.InvalidateLiveness("local-agent")
	h.InvalidateLiveness("remote-agent-1")

	if got := local.invalidations; len(got) != 1 || got[0] != "local-agent" {
		t.Fatalf("local invalidations = %#v, want exact local name once", got)
	}
	if got := remote.invalidations; len(got) != 1 || got[0] != "remote-agent-1" {
		t.Fatalf("remote invalidations = %#v, want exact remote name once", got)
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

// scriptedListProvider answers ListRunning with a fixed result and records
// every prefix it is asked for.
type scriptedListProvider struct {
	*runtime.Fake
	names    []string
	err      error
	prefixes []string
}

func (p *scriptedListProvider) ListRunning(prefix string) ([]string, error) {
	p.prefixes = append(p.prefixes, prefix)
	return p.names, p.err
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Kills: per-backend results that diverge from the merged ListRunning (a
// label swap, a dropped error, dropped names), and a per-backend listing that
// loses the single-backend ServerAbsent signal the merge deliberately drops.
func TestHybridListRunningByBackend_MergesToListRunning(t *testing.T) {
	partial := &runtime.PartialListError{Err: errors.New("one pod unreadable")}
	absent := &runtime.PartialListError{Err: errors.New("tmux server unreachable"), ServerAbsent: true}
	cases := []struct {
		name                    string
		localNames, remoteNames []string
		localErr, remoteErr     error
	}{
		{name: "both ok", localNames: []string{"gc-a"}, remoteNames: []string{"gc-remote-agent-1", "gc-remote-agent-2"}},
		{name: "remote partial", localNames: []string{"gc-a"}, remoteNames: []string{"gc-remote-agent-1"}, remoteErr: partial},
		{name: "local server absent", remoteNames: []string{"gc-remote-agent-1"}, localErr: absent},
		{name: "remote failed", localNames: []string{"gc-a"}, remoteErr: errors.New("apiserver timeout")},
		{name: "both failed", localErr: errors.New("local down"), remoteErr: errors.New("remote down")},
		{name: "both empty", localNames: []string{}, remoteNames: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := &scriptedListProvider{Fake: runtime.NewFake(), names: tc.localNames, err: tc.localErr}
			remote := &scriptedListProvider{Fake: runtime.NewFake(), names: tc.remoteNames, err: tc.remoteErr}
			h := New(local, remote, isRemote)

			merged, mergedErr := h.ListRunning("gc-")
			listings := h.ListRunningByBackend("gc-")

			if len(listings) != 2 {
				t.Fatalf("ListRunningByBackend() returned %d listings, want 2", len(listings))
			}
			want := []struct {
				label string
				sp    *scriptedListProvider
			}{{"local", local}, {"remote", remote}}
			for i, l := range listings {
				if l.Label != want[i].label {
					t.Errorf("listing %d label = %q, want %q", i, l.Label, want[i].label)
				}
				if l.Provider != runtime.Provider(want[i].sp) {
					t.Errorf("listing %d provider = %T, want the %s backend", i, l.Provider, want[i].label)
				}
				if !reflect.DeepEqual(l.Names, want[i].sp.names) {
					t.Errorf("listing %d names = %#v, want %#v", i, l.Names, want[i].sp.names)
				}
				if !errors.Is(l.Err, want[i].sp.err) {
					t.Errorf("listing %d err = %v, want %v", i, l.Err, want[i].sp.err)
				}
			}
			if got := runtime.IsRuntimeServerAbsent(listings[0].Err); got != runtime.IsRuntimeServerAbsent(tc.localErr) {
				t.Errorf("local listing ServerAbsent = %v, want %v", got, runtime.IsRuntimeServerAbsent(tc.localErr))
			}

			// The flat merge ListRunning computed before it was expressed
			// over ListRunningByBackend.
			names, err := runtime.MergeBackendListResults(
				runtime.BackendListResult{Label: "local", Names: tc.localNames, Err: tc.localErr},
				runtime.BackendListResult{Label: "remote", Names: tc.remoteNames, Err: tc.remoteErr},
			)
			if relisted, relistedErr := runtime.MergeBackendListings(listings); !reflect.DeepEqual(relisted, names) || errText(relistedErr) != errText(err) {
				t.Errorf("MergeBackendListings = (%#v, %q), want (%#v, %q)", relisted, errText(relistedErr), names, errText(err))
			}
			if !reflect.DeepEqual(names, merged) {
				t.Errorf("flat-merge names = %#v, ListRunning names = %#v", names, merged)
			}
			if errText(err) != errText(mergedErr) {
				t.Errorf("flat-merge err = %q, ListRunning err = %q", errText(err), errText(mergedErr))
			}
			if runtime.IsPartialListError(err) != runtime.IsPartialListError(mergedErr) {
				t.Errorf("partial = %v, ListRunning partial = %v", runtime.IsPartialListError(err), runtime.IsPartialListError(mergedErr))
			}
			if runtime.IsRuntimeServerAbsent(err) != runtime.IsRuntimeServerAbsent(mergedErr) {
				t.Errorf("ServerAbsent = %v, ListRunning ServerAbsent = %v", runtime.IsRuntimeServerAbsent(err), runtime.IsRuntimeServerAbsent(mergedErr))
			}
			for _, sp := range []*scriptedListProvider{local, remote} {
				if !reflect.DeepEqual(sp.prefixes, []string{"gc-", "gc-"}) {
					t.Errorf("backend prefixes = %q, want one gc- call per listing method", sp.prefixes)
				}
			}
		})
	}
}

// Kills: a composite attested while one of its backends is not.
func TestListRunningAttested_CompositeRequiresEveryBackend(t *testing.T) {
	attested := func() runtime.Provider { return runtime.NewFake() }
	unattested := func() runtime.Provider {
		f := runtime.NewFake()
		f.ListingUnattested = true
		return f
	}
	undeclared := func() runtime.Provider { return struct{ runtime.Provider }{runtime.NewFake()} }
	cases := []struct {
		name          string
		local, remote runtime.Provider
		want          bool
	}{
		{name: "both attested", local: attested(), remote: attested(), want: true},
		{name: "remote unattested", local: attested(), remote: unattested(), want: false},
		{name: "local unattested", local: unattested(), remote: attested(), want: false},
		{name: "remote undeclared", local: attested(), remote: undeclared(), want: false},
	}
	for _, tc := range cases {
		if got := runtime.ListRunningAttested(New(tc.local, tc.remote, isRemote)); got != tc.want {
			t.Errorf("%s: ListRunningAttested(hybrid) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Kills: an accessor that lists, and a Backends order or label that disagrees
// with ListRunningByBackend (see the auto twin).
func TestHybridBackends_NamesBackendsWithoutListing(t *testing.T) {
	local := &scriptedListProvider{Fake: runtime.NewFake()}
	remote := &scriptedListProvider{Fake: runtime.NewFake()}
	p := New(local, remote, func(string) bool { return false })

	backends := p.Backends()
	if len(local.prefixes)+len(remote.prefixes) != 0 {
		t.Fatalf("Backends() listed its backends (local %d, remote %d calls), want none", len(local.prefixes), len(remote.prefixes))
	}
	listings := p.ListRunningByBackend("")
	if len(backends) != len(listings) {
		t.Fatalf("Backends() = %d entries, ListRunningByBackend = %d", len(backends), len(listings))
	}
	for i, b := range backends {
		if b.Label != listings[i].Label || b.Provider != listings[i].Provider {
			t.Errorf("backend %d = (%q, %T), listing = (%q, %T)", i, b.Label, b.Provider, listings[i].Label, listings[i].Provider)
		}
	}
}
