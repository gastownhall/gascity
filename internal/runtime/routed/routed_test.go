package routed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRouteDelegatesToNamedBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("exec:/bin/worker", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p.Route("worker", "exec:/bin/worker")
	if err := p.Start(context.Background(), "worker", runtime.Config{Command: "run"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if remoteSP.LastStartConfig("worker") == nil {
		t.Fatal("remote backend did not receive Start")
	}
	if defaultSP.LastStartConfig("worker") != nil {
		t.Fatal("default backend received routed Start")
	}
}

func TestRouteToUnknownBackendDoesNotUseDefault(t *testing.T) {
	defaultSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	p.Route("worker", "missing")

	err := p.Start(context.Background(), "worker", runtime.Config{})
	if err == nil {
		t.Fatal("Start error = nil, want missing backend error")
	}
	if defaultSP.LastStartConfig("worker") != nil {
		t.Fatal("default backend received Start for unknown route")
	}
}

func TestListRunningReturnsPartialWhenSomeBackendsFail(t *testing.T) {
	defaultSP := runtime.NewFake()
	brokenSP := runtime.NewFailFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", brokenSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = defaultSP.Start(context.Background(), "alive", runtime.Config{})

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning partial error = %v, want nil", err)
	}
	if len(names) != 1 || names[0] != "alive" {
		t.Fatalf("ListRunning = %v, want [alive]", names)
	}
}

func TestListRunningErrorsWhenAllBackendsFail(t *testing.T) {
	p := New("tmux", runtime.NewFailFake())
	if err := p.Register("remote", runtime.NewFailFake()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	names, err := p.ListRunning("")
	if err == nil {
		t.Fatal("ListRunning error = nil, want error")
	}
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil", names)
	}
}

func TestStopFallsThroughToOtherBackend(t *testing.T) {
	defaultSP := runtime.NewFailFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = remoteSP.Start(context.Background(), "worker", runtime.Config{})

	if err := p.Stop("worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if remoteSP.IsRunning("worker") {
		t.Fatal("remote session still running")
	}
}

func TestStopFallsThroughWhenPrimaryStopIsIdempotent(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = remoteSP.Start(context.Background(), "worker", runtime.Config{})

	if err := p.Stop("worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if remoteSP.IsRunning("worker") {
		t.Fatal("remote session still running")
	}
}

func TestStopDoesNotMaskPrimaryFailureWithMissingSessionBackend(t *testing.T) {
	defaultSP := runtime.NewFailFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := p.Stop("worker"); err == nil {
		t.Fatal("Stop error = nil, want primary backend failure")
	}
}

func TestIsAttachedFallsThroughToRunningBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = remoteSP.Start(context.Background(), "worker", runtime.Config{})
	remoteSP.SetAttached("worker", true)

	if !p.IsAttached("worker") {
		t.Fatal("IsAttached = false, want true from running remote backend")
	}
}

func TestProcessAliveFallsThroughToRunningBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = remoteSP.Start(context.Background(), "worker", runtime.Config{})

	if !p.ProcessAlive("worker", []string{"agent"}) {
		t.Fatal("ProcessAlive = false, want true from running remote backend")
	}
}

func TestLiveOperationsFallThroughToRunningBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_ = remoteSP.Start(context.Background(), "worker", runtime.Config{})
	remoteSP.SetPeekOutput("worker", "remote output")
	lastActivity := time.Unix(123, 0)
	remoteSP.SetActivity("worker", lastActivity)
	remoteSP.WaitForIdleErrors["worker"] = nil

	if err := p.Nudge("worker", runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got, err := p.Peek("worker", 10); err != nil || got != "remote output" {
		t.Fatalf("Peek = %q, %v; want remote output, nil", got, err)
	}
	if got, err := p.GetLastActivity("worker"); err != nil || !got.Equal(lastActivity) {
		t.Fatalf("GetLastActivity = %v, %v; want %v, nil", got, err, lastActivity)
	}
	if err := p.WaitForIdle(context.Background(), "worker", time.Second); err != nil {
		t.Fatalf("WaitForIdle: %v", err)
	}
	if err := p.SetMeta("worker", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := p.GetMeta("worker", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	if err := p.RunLive("worker", runtime.Config{}); err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	for _, method := range []string{"Nudge", "Peek", "GetLastActivity", "WaitForIdle", "SetMeta", "GetMeta", "RunLive"} {
		if !fakeHasCall(remoteSP, method, "worker") {
			t.Fatalf("remote backend did not receive %s", method)
		}
		if fakeHasCall(defaultSP, method, "worker") {
			t.Fatalf("default backend received %s", method)
		}
	}
}

func TestAttachReturnsACPError(t *testing.T) {
	p := New("tmux", runtime.NewFake())
	if err := p.Register("acp", runtime.NewFake()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p.Route("assistant", "acp")

	err := p.Attach("assistant")
	if err == nil {
		t.Fatal("Attach error = nil, want ACP error")
	}
	if want := `agent "assistant" uses ACP transport (no terminal to attach to)`; err.Error() != want {
		t.Fatalf("Attach error = %q, want %q", err.Error(), want)
	}
}

func TestDetectTransportUsesRouteThenProbe(t *testing.T) {
	defaultSP := runtime.NewFake()
	remoteSP := runtime.NewFake()
	p := New("tmux", defaultSP)
	if err := p.Register("remote", remoteSP); err != nil {
		t.Fatalf("Register: %v", err)
	}

	p.Route("planned", "remote")
	if got := p.DetectTransport("planned"); got != "remote" {
		t.Fatalf("DetectTransport(planned) = %q, want remote", got)
	}

	_ = remoteSP.Start(context.Background(), "lost-route", runtime.Config{})
	if got := p.DetectTransport("lost-route"); got != "remote" {
		t.Fatalf("DetectTransport(lost-route) = %q, want remote", got)
	}
}

func TestPendingUnsupportedWhenBackendLacksInteractionSupport(t *testing.T) {
	p := New("tmux", runtime.NewFake())
	if err := p.Register("plain", noInteractionProvider{Provider: runtime.NewFake()}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p.Route("plain-session", "plain")

	_, err := p.Pending("plain-session")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

func TestCapabilitiesIntersectsBackends(t *testing.T) {
	p := New("tmux", runtime.NewFake())
	if err := p.Register("remote", noCapabilitiesProvider{Provider: runtime.NewFake()}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	caps := p.Capabilities()
	if caps.CanReportAttachment {
		t.Fatal("CanReportAttachment = true, want false when any backend cannot report attachment")
	}
	if caps.CanReportActivity {
		t.Fatal("CanReportActivity = true, want false when any backend cannot report activity")
	}
}

type noInteractionProvider struct {
	runtime.Provider
}

type noCapabilitiesProvider struct {
	runtime.Provider
}

func (noCapabilitiesProvider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{}
}

func fakeHasCall(f *runtime.Fake, method, name string) bool {
	for _, call := range f.Calls {
		if call.Method == method && call.Name == name {
			return true
		}
	}
	return false
}
