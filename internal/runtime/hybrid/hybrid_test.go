package hybrid

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

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
