package auto

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

var _ runtime.Provider = (*Provider)(nil)

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

func TestProviderStopUnattendedSessionRoutesOnlySelectedBackend(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		defaultSP := newUnattendedStopperProvider(nil)
		acpSP := newUnattendedStopperProvider(nil)
		p := New(defaultSP, acpSP)

		if err := p.StopUnattendedSession("plain", "token-default"); err != nil {
			t.Fatalf("StopUnattendedSession(default): %v", err)
		}
		if got := defaultSP.calls; len(got) != 1 || got[0] != (unattendedStopCall{name: "plain", expectedToken: "token-default"}) {
			t.Fatalf("default unattended stops = %#v, want exact plain/token-default call", got)
		}
		if got := acpSP.calls; len(got) != 0 {
			t.Fatalf("ACP unattended stops = %#v, want none", got)
		}
	})

	t.Run("ACP", func(t *testing.T) {
		defaultSP := newUnattendedStopperProvider(nil)
		acpSP := newUnattendedStopperProvider(nil)
		p := New(defaultSP, acpSP)
		p.RouteACP("acpsess")

		if err := p.StopUnattendedSession("acpsess", "token-acp"); err != nil {
			t.Fatalf("StopUnattendedSession(ACP): %v", err)
		}
		if got := acpSP.calls; len(got) != 1 || got[0] != (unattendedStopCall{name: "acpsess", expectedToken: "token-acp"}) {
			t.Fatalf("ACP unattended stops = %#v, want exact acpsess/token-acp call", got)
		}
		if got := defaultSP.calls; len(got) != 0 {
			t.Fatalf("default unattended stops = %#v, want none", got)
		}
	})

	t.Run("unsupported default does not probe ACP", func(t *testing.T) {
		acpSP := newUnattendedStopperProvider(nil)
		p := New(runtime.NewFake(), acpSP)

		err := p.StopUnattendedSession("plain", "token")
		if err == nil || !strings.Contains(err.Error(), "default backend") {
			t.Fatalf("StopUnattendedSession error = %v, want contextual default-backend error", err)
		}
		if got := acpSP.calls; len(got) != 0 {
			t.Fatalf("ACP unattended stops = %#v, want no fallback probe", got)
		}
	})

	t.Run("ACP error does not probe default", func(t *testing.T) {
		sentinel := errors.New("ACP unattended stop unavailable")
		defaultSP := newUnattendedStopperProvider(nil)
		acpSP := newUnattendedStopperProvider(sentinel)
		p := New(defaultSP, acpSP)
		p.RouteACP("acpsess")

		err := p.StopUnattendedSession("acpsess", "token")
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "ACP backend") {
			t.Fatalf("StopUnattendedSession error = %v, want wrapped contextual ACP error", err)
		}
		if got := defaultSP.calls; len(got) != 0 {
			t.Fatalf("default unattended stops = %#v, want no fallback probe", got)
		}
	})
}

// Relaunch must reach the routed backend (default vs ACP), or the reconciler's
// RelaunchProvider type-assert would be masked by the auto router and fall back
// to Stop+Start.
func TestProvider_ForwardsRelaunchToRoutedBackend(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	p := New(def, acp)
	if err := def.Start(context.Background(), "plain", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(plain): %v", err)
	}
	if err := acp.Start(context.Background(), "acpsess", runtime.Config{Command: "c"}); err != nil {
		t.Fatalf("Start(acpsess): %v", err)
	}
	p.RouteACP("acpsess")

	if err := p.Relaunch(context.Background(), "plain", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(plain): %v", err)
	}
	if got := def.CountCalls("Relaunch", "plain"); got != 1 {
		t.Errorf("default backend Relaunch calls = %d, want 1", got)
	}
	if err := p.Relaunch(context.Background(), "acpsess", runtime.Config{Command: "c2"}); err != nil {
		t.Fatalf("Relaunch(acpsess): %v", err)
	}
	if got := acp.CountCalls("Relaunch", "acpsess"); got != 1 {
		t.Errorf("acp backend Relaunch calls = %d, want 1", got)
	}
}

type falseNegativeStopProvider struct {
	*runtime.Fake
	stopErr error
}

func (p *falseNegativeStopProvider) Stop(string) error { return p.stopErr }

func (p *falseNegativeStopProvider) IsRunning(string) bool { return false }

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

func TestRouteDefaultAndACP(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	// Unregistered session routes to default.
	if got := p.route("agent-a"); got != defaultSP {
		t.Fatal("unregistered session should route to default")
	}

	// Register as ACP.
	p.RouteACP("agent-b")
	if got := p.route("agent-b"); got != acpSP {
		t.Fatal("registered session should route to ACP")
	}
	if got := p.route("agent-a"); got != defaultSP {
		t.Fatal("other session should still route to default")
	}
}

func TestUnroute(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-x")
	if got := p.route("agent-x"); got != acpSP {
		t.Fatal("should route to ACP after registration")
	}

	p.Unroute("agent-x")
	if got := p.route("agent-x"); got != defaultSP {
		t.Fatal("should route to default after unroute")
	}
}

func TestAttachReturnsErrorForACP(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("headless-agent")
	err := p.Attach("headless-agent")
	if err == nil {
		t.Fatal("Attach on ACP session should return error")
	}
	if want := `agent "headless-agent" uses ACP transport (no terminal to attach to)`; err.Error() != want {
		t.Errorf("Attach error = %q, want %q", err.Error(), want)
	}

	// Default sessions with an existing session should not error.
	_ = defaultSP.Start(context.Background(), "normal-agent", runtime.Config{})
	if err := p.Attach("normal-agent"); err != nil {
		t.Errorf("Attach on default session should not error: %v", err)
	}
}

func TestListRunningMergesBothBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	// Start sessions on each backend.
	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})
	_ = acpSP.Start(context.Background(), "acp-1", runtime.Config{})

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListRunning returned %d names, want 2: %v", len(names), names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["default-1"] || !found["acp-1"] {
		t.Errorf("ListRunning = %v, want default-1 and acp-1", names)
	}
}

func TestStopPreservesRouteOnBothFail(t *testing.T) {
	defaultSP := runtime.NewFailFake() // both backends fail
	acpSP := runtime.NewFailFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return error when both backends fail")
	}

	// Route should be preserved since Stop failed on both.
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when Stop fails on both backends")
	}
}

func TestStopReturnsJoinedErrorsFromBothBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	defaultSP.StopErrors["agent-fail"] = errors.New("default stop failed")
	acpSP.StopErrors["agent-fail"] = errors.New("acp stop failed")
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return error when both backends fail")
	}
	for _, want := range []string{
		"acp backend: acp stop failed",
		"default backend: default stop failed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Stop error = %v, want to contain %q", err, want)
		}
	}
}

func TestStopPreservesRouteWhenFallbackBackendDidNotOwnSession(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	acpSP.StopErrors["agent-fail"] = errors.New("acp stop failed")
	if err := acpSP.Start(context.Background(), "agent-fail", runtime.Config{}); err != nil {
		t.Fatalf("acp Start: %v", err)
	}
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return error when the routed backend fails and fallback has no session")
	}
	if !strings.Contains(err.Error(), "acp backend: acp stop failed") {
		t.Fatalf("Stop error = %v, want primary backend failure", err)
	}
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when fallback backend did not own the session")
	}
	if !acpSP.IsRunning("agent-fail") {
		t.Fatal("session should still be running on ACP after failed stop")
	}
}

func TestStopTreatsSessionGoneOnBothBackendsAsIdempotent(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	defaultSP.StopErrors["ghost-agent"] = fmt.Errorf("%w: default missing", runtime.ErrSessionNotFound)
	acpSP.StopErrors["ghost-agent"] = fmt.Errorf("%w: acp missing", runtime.ErrSessionNotFound)
	p := New(defaultSP, acpSP)

	p.RouteACP("ghost-agent")
	if err := p.Stop("ghost-agent"); err != nil {
		t.Fatalf("Stop error = %v, want nil when both backends report session gone", err)
	}
}

func TestStopFallsThroughWhenPrimaryMissingSessionReturnsNil(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	if err := acpSP.Start(context.Background(), "orphan", runtime.Config{}); err != nil {
		t.Fatalf("acp Start: %v", err)
	}

	if err := p.Stop("orphan"); err != nil {
		t.Fatalf("Stop should fall through to ACP when default backend reports missing session as nil: %v", err)
	}
	if acpSP.IsRunning("orphan") {
		t.Fatal("session should be stopped on ACP backend after stale-route fallthrough")
	}
}

func TestStopReturnsPrimaryFailureWhenFallbackStopsSameNamedSession(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	acpSP.StopErrors["agent-fail"] = errors.New("acp stop failed")
	if err := acpSP.Start(context.Background(), "agent-fail", runtime.Config{}); err != nil {
		t.Fatalf("acp Start: %v", err)
	}
	if err := defaultSP.Start(context.Background(), "agent-fail", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return the routed backend failure even when fallback stops a same-named session")
	}
	if !strings.Contains(err.Error(), "acp backend: acp stop failed") {
		t.Fatalf("Stop error = %v, want primary backend failure", err)
	}
	if !acpSP.IsRunning("agent-fail") {
		t.Fatal("routed ACP session should remain running after primary stop failure")
	}
	if defaultSP.IsRunning("agent-fail") {
		t.Fatal("fallback default session should be stopped during stale-route recovery")
	}
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when the routed backend stop failed")
	}
}

func TestStopReturnsPrimaryFailureWhenPrimaryCannotConfirmLiveness(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	acpSP.StopErrors["agent-fail"] = errors.New("acp unavailable")
	if err := defaultSP.Start(context.Background(), "agent-fail", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return the routed backend failure even when primary IsRunning is false")
	}
	if !strings.Contains(err.Error(), "acp backend: acp unavailable") {
		t.Fatalf("Stop error = %v, want primary backend failure", err)
	}
	if defaultSP.IsRunning("agent-fail") {
		t.Fatal("fallback default session should still be stopped during stale-route recovery")
	}
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when the routed backend stop failed")
	}
}

func TestStopReturnsErrorWhenExplicitRouteOwnershipIsAmbiguous(t *testing.T) {
	defaultSP := runtime.NewFake()
	if err := defaultSP.Start(context.Background(), "agent-fail", runtime.Config{}); err != nil {
		t.Fatalf("default Start: %v", err)
	}
	acpSP := &falseNegativeStopProvider{Fake: runtime.NewFake()}
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return an error when the explicit route cannot confirm ownership and fallback is running")
	}
	if !strings.Contains(err.Error(), "acp backend: stop succeeded without liveness confirmation") {
		t.Fatalf("Stop error = %v, want explicit-route ambiguity error", err)
	}
	if !defaultSP.IsRunning("agent-fail") {
		t.Fatal("same-named fallback session should remain running when ownership is ambiguous")
	}
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when explicit-route ownership is ambiguous")
	}
}

func TestListRunningPartialError(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFailFake() // ListRunning returns error
	p := New(defaultSP, acpSP)

	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})

	names, err := p.ListRunning("")
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning error = %v, want partial list error", err)
	}
	// Should still return partial results from the working backend.
	if len(names) != 1 || names[0] != "default-1" {
		t.Errorf("ListRunning partial = %v, want [default-1]", names)
	}
}

func TestListRunningBothFail(t *testing.T) {
	defaultSP := runtime.NewFailFake()
	acpSP := runtime.NewFailFake()
	p := New(defaultSP, acpSP)

	names, err := p.ListRunning("")
	if err == nil {
		t.Fatal("ListRunning should return error when both backends fail")
	}
	if names != nil {
		t.Errorf("ListRunning both fail = %v, want nil", names)
	}
}

func TestListRunningPartialErrorIncludesBackendContext(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFailFake()
	p := New(defaultSP, acpSP)

	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})

	names, err := p.ListRunning("")
	if len(names) != 1 || names[0] != "default-1" {
		t.Fatalf("ListRunning partial = %v, want [default-1]", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning error = %v, want partial list error", err)
	}
}

func TestIsRunningFallsThrough(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	// Start on default backend but register route as ACP (simulates stale route).
	_ = defaultSP.Start(context.Background(), "stale-agent", runtime.Config{})
	p.RouteACP("stale-agent")

	// ACP says not running → should fall through to default → true.
	if !p.IsRunning("stale-agent") {
		t.Fatal("IsRunning should fall through to default when ACP reports not running")
	}

	// Reverse: start on ACP, don't register route (simulates lost route).
	_ = acpSP.Start(context.Background(), "lost-route", runtime.Config{})
	if !p.IsRunning("lost-route") {
		t.Fatal("IsRunning should fall through to ACP when default reports not running")
	}
}

func TestIsDeadRuntimeSessionChecksUnroutedFallbackChecker(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := newDeadRuntimeCheckProvider()
	acpSP.dead["lost-route"] = true
	p := New(defaultSP, acpSP)

	dead, err := p.IsDeadRuntimeSession("lost-route")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true from fallback checker")
	}
	if got := acpSP.checks; len(got) != 1 || got[0] != "lost-route" {
		t.Fatalf("fallback checks = %v, want [lost-route]", got)
	}
}

func TestIsDeadRuntimeSessionFindsDefaultCorpseBehindStaleACPRoute(t *testing.T) {
	defaultSP := newDeadRuntimeCheckProvider()
	acpSP := newDeadRuntimeCheckProvider()
	defaultSP.dead["agent"] = true
	p := New(defaultSP, acpSP)
	p.RouteACP("agent")

	dead, err := p.IsDeadRuntimeSession("agent")
	if err != nil {
		t.Fatalf("IsDeadRuntimeSession: %v", err)
	}
	if !dead {
		t.Fatal("IsDeadRuntimeSession = false, want true from default backend")
	}
	if got := acpSP.checks; len(got) != 1 || got[0] != "agent" {
		t.Fatalf("primary checks = %v, want [agent]", got)
	}
	if got := defaultSP.checks; len(got) != 1 || got[0] != "agent" {
		t.Fatalf("fallback checks = %v, want [agent]", got)
	}
}

func TestStopFallsThrough(t *testing.T) {
	defaultSP := runtime.NewFailFake() // Stop always fails (simulates "not found")
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	// Start on ACP but don't register route (simulates lost route after restart).
	_ = acpSP.Start(context.Background(), "orphan", runtime.Config{})

	// Stop routes to default (no route entry), which fails → falls through to ACP.
	if err := p.Stop("orphan"); err != nil {
		t.Fatalf("Stop should fall through to ACP backend: %v", err)
	}
	if acpSP.IsRunning("orphan") {
		t.Fatal("session should be stopped on ACP backend after fallthrough")
	}
}

func TestStopCleansUpRoute(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("agent-z")
	_ = acpSP.Start(context.Background(), "agent-z", runtime.Config{})

	if err := p.Stop("agent-z"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After stop, route entry should be cleaned up.
	if got := p.route("agent-z"); got != defaultSP {
		t.Fatal("route should fall back to default after Stop")
	}
}

func TestPendingAndRespondDelegateToRoutedBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("interactive-agent")
	_ = acpSP.Start(context.Background(), "interactive-agent", runtime.Config{})
	acpSP.SetPendingInteraction("interactive-agent", &runtime.PendingInteraction{RequestID: "req-1"})

	pending, err := p.Pending("interactive-agent")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}
	if err := p.Respond("interactive-agent", runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := acpSP.Responses["interactive-agent"]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("Responses = %#v, want single approve", got)
	}
}

func TestPendingUnsupportedWhenBackendLacksInteractionSupport(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := &runtimeNoInteractionProvider{Provider: runtime.NewFake()}
	p := New(defaultSP, acpSP)

	p.RouteACP("plain-agent")

	_, err := p.Pending("plain-agent")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

type runtimeNoInteractionProvider struct {
	runtime.Provider
}

func TestWaitForInterruptBoundaryDelegatesToRoutedBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, acpSP)

	p.RouteACP("interactive-agent")
	since := time.Unix(1700000000, 123).UTC()
	if err := p.WaitForInterruptBoundary(context.Background(), "interactive-agent", since, 2*time.Second); err != nil {
		t.Fatalf("WaitForInterruptBoundary: %v", err)
	}
	if len(acpSP.Calls) == 0 {
		t.Fatal("expected routed backend to record WaitForInterruptBoundary")
	}
	last := acpSP.Calls[len(acpSP.Calls)-1]
	if last.Method != "WaitForInterruptBoundary" || last.Name != "interactive-agent" {
		t.Fatalf("last call = %#v, want WaitForInterruptBoundary for interactive-agent", last)
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
					&capsFake{runtime.NewFake(), capsWith(t, field, tc.second)})
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

	got := reflect.ValueOf(New(&capsFake{runtime.NewFake(), all}, &capsFake{runtime.NewFake(), all}).Capabilities())
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

// The composite must satisfy IdleSnapshotProvider itself. The idle-timeout
// reconciler type-asserts the provider it holds, and cmd/gc wraps the WHOLE
// city provider in auto.Provider as soon as any agent selects the ACP
// transport — so without this the content-based idle clock silently never runs
// in such a city (ga-07mi8).
var _ runtime.IdleSnapshotProvider = (*Provider)(nil)

func TestSnapshotIdle_RoutesToBackend(t *testing.T) {
	def, acp := newIdleSnapshotProvider(), newIdleSnapshotProvider()
	p := New(def, acp)
	p.RouteACP("acpsess")
	def.idle["plain"] = true

	idle, err := p.SnapshotIdle("plain")
	if err != nil {
		t.Fatalf("SnapshotIdle(plain): %v", err)
	}
	if !idle {
		t.Error("SnapshotIdle(plain) = false, want true from the default backend")
	}
	if !reflect.DeepEqual(def.calls, []string{"plain"}) {
		t.Errorf("default backend SnapshotIdle calls = %v, want [plain]", def.calls)
	}
	if len(acp.calls) != 0 {
		t.Errorf("acp backend SnapshotIdle calls = %v, want none", acp.calls)
	}

	if _, err := p.SnapshotIdle("acpsess"); err != nil {
		t.Fatalf("SnapshotIdle(acpsess): %v", err)
	}
	if !reflect.DeepEqual(acp.calls, []string{"acpsess"}) {
		t.Errorf("acp backend SnapshotIdle calls = %v, want [acpsess]", acp.calls)
	}
}

func TestSnapshotIdle_FailsClosedWhenRouteCannotSnapshot(t *testing.T) {
	p := New(runtime.NewFake(), runtime.NewFake())

	idle, err := p.SnapshotIdle("plain")
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
func TestAutoListRunningByBackend_MergesToListRunning(t *testing.T) {
	partial := &runtime.PartialListError{Err: errors.New("one socket unanswered")}
	absent := &runtime.PartialListError{Err: errors.New("tmux server unreachable"), ServerAbsent: true}
	cases := []struct {
		name               string
		defNames, acpNames []string
		defErr, acpErr     error
	}{
		{name: "both ok", defNames: []string{"gc-a", "gc-b"}, acpNames: []string{"gc-c"}},
		{name: "acp partial", defNames: []string{"gc-a"}, acpNames: []string{"gc-c"}, acpErr: partial},
		{name: "default server absent", acpNames: []string{"gc-c"}, defErr: absent},
		{name: "default failed", acpNames: []string{"gc-c"}, defErr: errors.New("list-sessions timed out")},
		{name: "both failed", defErr: errors.New("default down"), acpErr: errors.New("acp down")},
		{name: "both empty", defNames: []string{}, acpNames: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := &scriptedListProvider{Fake: runtime.NewFake(), names: tc.defNames, err: tc.defErr}
			acp := &scriptedListProvider{Fake: runtime.NewFake(), names: tc.acpNames, err: tc.acpErr}
			p := New(def, acp)

			merged, mergedErr := p.ListRunning("gc-")
			listings := p.ListRunningByBackend("gc-")

			if len(listings) != 2 {
				t.Fatalf("ListRunningByBackend() returned %d listings, want 2", len(listings))
			}
			want := []struct {
				label string
				sp    *scriptedListProvider
			}{{"default", def}, {"acp", acp}}
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
			if got := runtime.IsRuntimeServerAbsent(listings[0].Err); got != runtime.IsRuntimeServerAbsent(tc.defErr) {
				t.Errorf("default listing ServerAbsent = %v, want %v", got, runtime.IsRuntimeServerAbsent(tc.defErr))
			}

			// The flat merge ListRunning computed before it was expressed
			// over ListRunningByBackend.
			names, err := runtime.MergeBackendListResults(
				runtime.BackendListResult{Label: "default", Names: tc.defNames, Err: tc.defErr},
				runtime.BackendListResult{Label: "acp", Names: tc.acpNames, Err: tc.acpErr},
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
			for _, sp := range []*scriptedListProvider{def, acp} {
				if !reflect.DeepEqual(sp.prefixes, []string{"gc-", "gc-"}) {
					t.Errorf("backend prefixes = %q, want one gc- call per listing method", sp.prefixes)
				}
			}
		})
	}
}

// Kills: a composite attested while one of its backends is not. auto(tmux,
// acp) must read unattested until acp itself attests.
func TestListRunningAttested_CompositeRequiresEveryBackend(t *testing.T) {
	attested := func() runtime.Provider { return runtime.NewFake() }
	unattested := func() runtime.Provider {
		f := runtime.NewFake()
		f.ListingUnattested = true
		return f
	}
	undeclared := func() runtime.Provider { return struct{ runtime.Provider }{runtime.NewFake()} }
	cases := []struct {
		name     string
		def, acp runtime.Provider
		want     bool
	}{
		{name: "both attested", def: attested(), acp: attested(), want: true},
		{name: "acp unattested", def: attested(), acp: unattested(), want: false},
		{name: "default unattested", def: unattested(), acp: attested(), want: false},
		{name: "acp undeclared", def: attested(), acp: undeclared(), want: false},
	}
	for _, tc := range cases {
		if got := runtime.ListRunningAttested(New(tc.def, tc.acp)); got != tc.want {
			t.Errorf("%s: ListRunningAttested(auto) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A nested composite (auto over hybrid) is listed once per leaf: the default
// entry carries hybrid's own merged result, and neither ListRunningByBackend
// nor ListRunning lists a leaf twice. A caller that recurses into the nested
// composite must use its ListRunningByBackend in place of, not in addition
// to, the observation it already holds.
// Kills: a composite listing that recurses into nested composites and lists
// their leaves twice.
func TestAutoListRunningByBackend_NestedCompositeListsEachLeafOnce(t *testing.T) {
	local := &scriptedListProvider{Fake: runtime.NewFake(), names: []string{"gc-a"}}
	remote := &scriptedListProvider{Fake: runtime.NewFake(), err: errors.New("apiserver timeout")}
	acp := &scriptedListProvider{Fake: runtime.NewFake(), names: []string{"gc-c"}}
	nested := hybrid.New(local, remote, func(string) bool { return false })
	p := New(nested, acp)

	listings := p.ListRunningByBackend("gc-")
	if len(listings) != 2 || listings[0].Provider != runtime.Provider(nested) {
		t.Fatalf("listings = %#v, want [hybrid, acp]", listings)
	}
	wantNames, wantErr := nested.ListRunning("gc-")
	if !reflect.DeepEqual(listings[0].Names, wantNames) || errText(listings[0].Err) != errText(wantErr) {
		t.Errorf("default entry = (%#v, %q), want hybrid's merged (%#v, %q)", listings[0].Names, errText(listings[0].Err), wantNames, errText(wantErr))
	}
	if _, ok := listings[0].Provider.(runtime.BackendListingProvider); !ok {
		t.Error("nested hybrid entry does not expose BackendListingProvider for a recursing caller")
	}
	// One ListRunningByBackend plus the direct nested.ListRunning above.
	for label, sp := range map[string]*scriptedListProvider{"local": local, "remote": remote} {
		if len(sp.prefixes) != 2 {
			t.Errorf("%s leaf listed %d times, want once per listing call (2)", label, len(sp.prefixes))
		}
	}
	if len(acp.prefixes) != 1 {
		t.Errorf("acp listed %d times, want 1", len(acp.prefixes))
	}
}

// partialLister is a backend whose listing is partial: some names plus a
// [runtime.PartialListError], as acp returns when a socket cannot be classified.
type partialLister struct {
	*runtime.Fake
	names []string
}

func (p partialLister) ListRunning(string) ([]string, error) {
	return p.names, &runtime.PartialListError{Err: runtime.ErrRuntimeUnavailable}
}

// Kills: the composite hiding a backend's partial listing, which would make a
// name the backend could not classify read as dead in the merged list.
func TestAutoMergedListIsPartialWhenACPIsPartial(t *testing.T) {
	defaultSP := runtime.NewFake()
	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})
	p := New(defaultSP, partialLister{Fake: runtime.NewFake(), names: []string{"acp-1"}})

	names, err := p.ListRunning("")
	if !runtime.IsPartialListError(err) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ListRunning err = %v, want a partial list wrapping the acp failure", err)
	}
	if !slices.Equal(names, []string{"default-1", "acp-1"}) {
		t.Fatalf("ListRunning names = %v, want [default-1 acp-1]", names)
	}
}

// Backends names each backend without listing it, in the order
// ListRunningByBackend lists them, so a caller can walk a nested composite and
// list every leaf exactly once.
// Kills: an accessor that lists (a second listing per observation), and a
// Backends order or label that disagrees with ListRunningByBackend.
func TestAutoBackends_NamesBackendsWithoutListing(t *testing.T) {
	def := &scriptedListProvider{Fake: runtime.NewFake()}
	acp := &scriptedListProvider{Fake: runtime.NewFake()}
	p := New(def, acp)

	backends := p.Backends()
	if len(def.prefixes)+len(acp.prefixes) != 0 {
		t.Fatalf("Backends() listed its backends (default %d, acp %d calls), want none", len(def.prefixes), len(acp.prefixes))
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

// LL6: auto routes Start unchanged, so FreshOnly reaches whichever backend
// hosts the name. Kills a router that drops or rebuilds the Config.
func TestAutoStartPassesFreshOnlyThrough(t *testing.T) {
	def, acp := runtime.NewFake(), runtime.NewFake()
	p := New(def, acp)
	p.RouteACP("acpsess")

	for _, tc := range []struct {
		name    string
		backend *runtime.Fake
	}{{"plain", def}, {"acpsess", acp}} {
		if err := p.Start(context.Background(), tc.name, runtime.Config{Command: "c", FreshOnly: true}); err != nil {
			t.Fatalf("Start(%s): %v", tc.name, err)
		}
		calls := tc.backend.Calls
		if len(calls) == 0 || calls[len(calls)-1].Method != "Start" || !calls[len(calls)-1].Config.FreshOnly {
			t.Errorf("Start(%s) did not reach its backend with FreshOnly: %+v", tc.name, calls)
		}
	}
}

// serverDeathFake is a Fake backend that confirms, or refuses to confirm, that
// its server is dead, as tmux does through runtime.ServerDeathConfirmer.
type serverDeathFake struct {
	*runtime.Fake
	dead bool
}

func (f *serverDeathFake) ServerConfirmedDead() bool { return f.dead }

// A live tmux server whose socket file was deleted answers "no server
// running" exactly as a dead one does, while its sessions keep running. auto
// must not merge that answer into success unless the backend confirms its
// server dead, and must forward the capability so StopForCleanup decides the
// same way. A backend without the capability keeps the old rule.
func TestStopMergesMissingServerOnlyWhenConfirmedDead(t *testing.T) {
	serverGone := fmt.Errorf("killing session sky: %w", errors.New("no tmux server running"))
	for _, tc := range []struct {
		name       string
		confirmer  bool
		dead       bool
		acpRoute   bool
		acpRunning bool
		wantErr    bool
	}{
		{name: "default server not confirmed dead", confirmer: true, wantErr: true},
		{name: "default server confirmed dead", confirmer: true, dead: true},
		{name: "ACP gone, default server not confirmed dead", confirmer: true, acpRoute: true, wantErr: true},
		{name: "ACP gone, default server confirmed dead", confirmer: true, dead: true, acpRoute: true},
		{name: "stale route stopped on ACP beside an unconfirmed default server", confirmer: true, acpRunning: true},
		{name: "default backend without the capability", confirmer: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newProvider := func() *Provider {
				fake := runtime.NewFake()
				fake.StopErrors["sky"] = serverGone
				var defaultSP runtime.Provider = fake
				if tc.confirmer {
					defaultSP = &serverDeathFake{Fake: fake, dead: tc.dead}
				}
				acpSP := runtime.NewFake()
				if tc.acpRoute {
					acpSP.StopErrors["sky"] = fmt.Errorf("%w: acp missing", runtime.ErrSessionNotFound)
				}
				if tc.acpRunning {
					if err := acpSP.Start(context.Background(), "sky", runtime.Config{}); err != nil {
						t.Fatalf("acp Start: %v", err)
					}
				}
				p := New(defaultSP, acpSP)
				if tc.acpRoute {
					p.RouteACP("sky")
				}
				return p
			}

			err := newProvider().Stop("sky")
			if tc.wantErr && !errors.Is(err, serverGone) {
				t.Fatalf("Stop = %v, want the missing-server answer", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Stop = %v, want nil", err)
			}
			if err := runtime.StopForCleanup(newProvider(), "sky"); tc.wantErr != (err != nil) {
				t.Fatalf("StopForCleanup = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// auto forwards ServerDeathConfirmer to whichever backend implements it.
func TestServerConfirmedDeadForwardsToConfirmingBackend(t *testing.T) {
	for _, dead := range []bool{false, true} {
		p := New(&serverDeathFake{Fake: runtime.NewFake(), dead: dead}, runtime.NewFake())
		if got := p.ServerConfirmedDead(); got != dead {
			t.Errorf("ServerConfirmedDead() = %v, want the tmux backend's %v", got, dead)
		}
	}
}
