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

type noFencedNudgeProvider struct{ runtime.Provider }

func TestProviderNudgeFencedRoutesOnlySelectedBackend(t *testing.T) {
	for _, test := range []struct {
		name    string
		session string
	}{
		{name: "local", session: "local-agent"},
		{name: "remote", session: "remote-agent-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := runtime.NewFake()
			remote := runtime.NewFake()
			provider := New(local, remote, isRemote)
			for _, backend := range []*runtime.Fake{local, remote} {
				if err := backend.Start(t.Context(), test.session, runtime.Config{}); err != nil {
					t.Fatalf("Start: %v", err)
				}
				if err := backend.SetMeta(test.session, "GC_INSTANCE_TOKEN", "launch-1"); err != nil {
					t.Fatalf("SetMeta: %v", err)
				}
			}

			if err := provider.NudgeFenced(test.session, "launch-1", runtime.TextContent("continue")); err != nil {
				t.Fatalf("NudgeFenced: %v", err)
			}
			selected, other := local, remote
			if isRemote(test.session) {
				selected, other = remote, local
			}
			if got := selected.CountCalls("NudgeFenced", test.session); got != 1 {
				t.Fatalf("selected fenced nudge calls = %d, want 1", got)
			}
			if got := other.CountCalls("NudgeFenced", test.session); got != 0 {
				t.Fatalf("other fenced nudge calls = %d, want 0", got)
			}
		})
	}
}

func TestProviderNudgeFencedFailsClosedForUnsupportedSelectedBackend(t *testing.T) {
	local := runtime.NewFake()
	remote := runtime.NewFake()
	session := "remote-agent-1"
	if err := remote.Start(t.Context(), session, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider := New(local, &noFencedNudgeProvider{Provider: remote}, isRemote)

	err := provider.NudgeFenced(session, "launch-1", runtime.TextContent("continue"))
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("NudgeFenced = %v, want ErrInteractionUnsupported", err)
	}
	if got := local.CountCalls("NudgeFenced", session); got != 0 {
		t.Fatalf("local fenced nudge calls = %d, want 0", got)
	}
	if got := remote.CountCalls("NudgeFenced", session); got != 0 {
		t.Fatalf("remote fenced nudge calls = %d, want 0", got)
	}
}

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
