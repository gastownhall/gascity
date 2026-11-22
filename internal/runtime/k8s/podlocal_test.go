package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// fakePodLocalRunner records every argv a pod-local provider produces and
// answers from a script of (output, error) pairs, falling back to empty
// success so unscripted commands still return.
type fakePodLocalRunner struct {
	calls   [][]string
	answers []struct {
		out string
		err error
	}
}

func (r *fakePodLocalRunner) run(argv []string) (string, error) {
	r.calls = append(r.calls, argv)
	if len(r.answers) > 0 {
		a := r.answers[0]
		r.answers = r.answers[1:]
		return a.out, a.err
	}
	return "", nil
}

// errPodLocalRun is the injected runner's transport error shape: what
// exec.Command produces for a non-zero exit (the message alone carries no
// output; the output string does).
var errPodLocalRun = errors.New("exit status 1")

func newPodLocalForTest(run *fakePodLocalRunner) *podLocalProvider {
	return &podLocalProvider{self: "reviewer-7", run: run.run}
}

func wantArgv(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// TestNewPodLocalRejectsMissingMarker pins the constructor's contract: no
// marker value (absent, empty or whitespace) means ordinary selection, and a
// direct construction attempt fails instead of building a provider with no
// session identity.
func TestNewPodLocalRejectsMissingMarker(t *testing.T) {
	t.Setenv(PodSessionEnv, "")
	if _, err := NewPodLocal(); err == nil {
		t.Fatal("NewPodLocal without a marker: err = nil, want error")
	}
	t.Setenv(PodSessionEnv, "   ")
	if _, err := NewPodLocal(); err == nil {
		t.Fatal("NewPodLocal with a whitespace marker: err = nil, want error")
	}
}

// TestMarkedPodSessionTrimsTheMarker reads the selection helper: whitespace
// around the logical session name is trimmed, absent and empty markers
// report not-marked, and a value reports it.
func TestMarkedPodSessionTrimsTheMarker(t *testing.T) {
	t.Setenv(PodSessionEnv, "")
	if _, ok := MarkedPodSession(); ok {
		t.Fatal("empty marker reported marked")
	}
	t.Setenv(PodSessionEnv, " reviewer-7 ")
	if name, ok := MarkedPodSession(); !ok || name != "reviewer-7" {
		t.Fatalf("MarkedPodSession = (%q, %t), want (\"reviewer-7\", true)", name, ok)
	}
}

// TestPodLocalMetadataRoundTripOnOwnSession drives the metadata operations
// as the drain protocol uses them: SetMeta, GetMeta and RemoveMeta on the
// pod's own logical session name and on the container-local "main" both
// address the exact "=main" tmux session on the default pod-local server —
// the session the controller observes through pod exec — with plain argument
// arrays and no socket or server-name flags.
func TestPodLocalMetadataRoundTripOnOwnSession(t *testing.T) {
	for _, name := range []string{"reviewer-7", "main"} {
		t.Run(name, func(t *testing.T) {
			run := &fakePodLocalRunner{}
			p := newPodLocalForTest(run)
			if err := p.SetMeta(name, "GC_DRAIN_ACK", "a=b c"); err != nil {
				t.Fatalf("SetMeta: %v", err)
			}
			wantArgv(t, run.calls[len(run.calls)-1],
				"tmux", "set-environment", "-t", "=main", "GC_DRAIN_ACK", "a=b c")

			run.answers = append(run.answers, struct {
				out string
				err error
			}{"GC_DRAIN_ACK=a=b c\n", nil})
			got, err := p.GetMeta(name, "GC_DRAIN_ACK")
			if err != nil {
				t.Fatalf("GetMeta: %v", err)
			}
			if got != "a=b c" {
				t.Fatalf("GetMeta = %q, want %q (embedded = and whitespace preserved)", got, "a=b c")
			}
			wantArgv(t, run.calls[len(run.calls)-1],
				"tmux", "show-environment", "-t", "=main", "GC_DRAIN_ACK")

			if err := p.RemoveMeta(name, "GC_DRAIN_ACK"); err != nil {
				t.Fatalf("RemoveMeta: %v", err)
			}
			wantArgv(t, run.calls[len(run.calls)-1],
				"tmux", "set-environment", "-t", "=main", "-u", "GC_DRAIN_ACK")
		})
	}
}

// TestPodLocalGetMetaDistinguishesMissingVariableFromMissingSession pins the
// read-back contract: an explicitly unset variable ("-KEY" or tmux's
// missing-variable diagnostic) reads as an empty value that succeeded, while
// a missing session or server is runtime.ErrSessionNotFound so existing
// ack/read-back handling can tell the difference, and any other failure
// reaches the caller with context.
func TestPodLocalGetMetaDistinguishesMissingVariableFromMissingSession(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		err        error
		want       string
		wantErr    error // nil = the read succeeds
		wantAnyErr bool  // the read fails, with no sentinel to match
	}{
		{name: "explicitly unset", out: "-GC_DRAIN_ACK\n", want: ""},
		{name: "missing variable diagnostic", out: "no such variable: GC_DRAIN_ACK\n", err: errPodLocalRun, want: ""},
		{name: "missing session", out: "no such session: =main\n", err: errPodLocalRun, wantErr: runtime.ErrSessionNotFound},
		{name: "no server", out: "no server running on /tmp/tmux-1000/default\n", err: errPodLocalRun, wantErr: runtime.ErrSessionNotFound},
		{name: "other failure propagates", out: "something else went wrong\n", err: errPodLocalRun, wantAnyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &fakePodLocalRunner{}
			p := newPodLocalForTest(run)
			run.answers = append(run.answers, struct {
				out string
				err error
			}{tc.out, tc.err})
			got, err := p.GetMeta("main", "GC_DRAIN_ACK")
			if tc.wantErr == nil && !tc.wantAnyErr {
				if err != nil {
					t.Fatalf("GetMeta: %v", err)
				}
				if got != tc.want {
					t.Fatalf("GetMeta = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("GetMeta = (%q, nil), want an error", got)
			}
			if !tc.wantAnyErr && !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetMeta error = %v, want wrapping %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "GC_DRAIN_ACK") && !strings.Contains(err.Error(), "get-meta") {
				t.Fatalf("GetMeta error = %v, want the key or the operation in its context", err)
			}
		})
	}
}

// TestPodLocalMetadataRefusesOtherSessions pins the scoping rule: metadata
// on a session other than the pod's own is a typed controller-owned refusal
// that executes nothing.
func TestPodLocalMetadataRefusesOtherSessions(t *testing.T) {
	run := &fakePodLocalRunner{}
	p := newPodLocalForTest(run)
	other := "somebody-else"
	if err, want := p.SetMeta(other, "K", "V"), "set-meta"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("SetMeta(other) = %v, want a %s refusal", err, want)
	}
	if _, err := p.GetMeta(other, "K"); err == nil || !strings.Contains(err.Error(), "get-meta") {
		t.Fatalf("GetMeta(other) = %v, want a get-meta refusal", err)
	}
	if err := p.RemoveMeta(other, "K"); err == nil || !strings.Contains(err.Error(), "remove-meta") {
		t.Fatalf("RemoveMeta(other) = %v, want a remove-meta refusal", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("refusals executed commands: %+v", run.calls)
	}
}

// TestPodLocalIsRunningProbesOwnSession exercises the boolean liveness probe:
// the own session's existence is a local tmux has-session on the exact
// "=main" target, and a failed probe stays false without turning into an
// absence the caller could mistake for a stronger verdict.
func TestPodLocalIsRunningProbesOwnSession(t *testing.T) {
	run := &fakePodLocalRunner{}
	p := newPodLocalForTest(run)
	if !p.IsRunning("main") {
		t.Fatal("IsRunning(main) = false, want true for a successful probe")
	}
	wantArgv(t, run.calls[0], "tmux", "has-session", "-t", "=main")

	// The logical session name addresses the same container-local session.
	run = &fakePodLocalRunner{
		answers: []struct {
			out string
			err error
		}{{"", nil}},
	}
	p = newPodLocalForTest(run)
	if !p.IsRunning("reviewer-7") {
		t.Fatal("IsRunning(own logical name) = false, want true")
	}
	wantArgv(t, run.calls[0], "tmux", "has-session", "-t", "=main")

	// A failing probe is false, never an error and never a command run for
	// another session.
	run = &fakePodLocalRunner{answers: []struct {
		out string
		err error
	}{{"no such session: =main\n", errPodLocalRun}}}
	p = newPodLocalForTest(run)
	if p.IsRunning("main") {
		t.Fatal("IsRunning(main) = true, want false for a failed probe")
	}
	if p.IsRunning("somebody-else") {
		t.Fatal("IsRunning(other) = true, want false without executing anything")
	}
	wantArgv(t, run.calls[0], "tmux", "has-session", "-t", "=main") // only the own-session probe ran
}

// TestPodLocalSetMetaMapsMissingSession reports the write-side counterpart:
// a SetMeta that finds no session or server is runtime.ErrSessionNotFound —
// the answer a drain-ack's ack lands on `errDrainAckFoundNoSession` — and a
// genuine failure keeps its operation and output context.
func TestPodLocalSetMetaMapsMissingSession(t *testing.T) {
	run := &fakePodLocalRunner{answers: []struct {
		out string
		err error
	}{{"no server running on /tmp/tmux-1000/default\n", errPodLocalRun}}}
	p := newPodLocalForTest(run)
	err := p.SetMeta("main", "GC_DRAIN_ACK", "1")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SetMeta on a missing server = %v, want runtime.ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "set-meta") {
		t.Fatalf("SetMeta error = %v, want the session and the operation in context", err)
	}

	run = &fakePodLocalRunner{answers: []struct {
		out string
		err error
	}{{"spectacular failure\n", errPodLocalRun}}}
	p = newPodLocalForTest(run)
	if err := p.SetMeta("reviewer-7", "K", "V"); err == nil || errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SetMeta on a foreign failure = %v, want a contextual error that is not session-not-found", err)
	} else if !strings.Contains(err.Error(), "spectacular failure") {
		t.Fatalf("SetMeta error = %v, want the command output kept", err)
	}
}

// TestPodLocalRemoveMetaIsIdempotentOnUnsetVariable keeps the removal a
// benign no-op when the variable is already unset, mirroring the runtime
// contract's idempotent removal.
func TestPodLocalRemoveMetaIsIdempotentOnUnsetVariable(t *testing.T) {
	run := &fakePodLocalRunner{answers: []struct {
		out string
		err error
	}{{"no such variable: GC_DRAIN\n", errPodLocalRun}}}
	p := newPodLocalForTest(run)
	if err := p.RemoveMeta("main", "GC_DRAIN"); err != nil {
		t.Fatalf("RemoveMeta of an unset variable = %v, want nil", err)
	}
}

// TestPodLocalRefusesLifecycleAndDeliveryOperations pins the refusal
// surface: every lifecycle and delivery operation is the controller-owned
// refusal for any name — not session absence, not a transient availability
// failure — and executes nothing. Stop is additionally a stop refusal so
// cleanup paths that absorb a gone session cannot mistake it.
func TestPodLocalRefusesLifecycleAndDeliveryOperations(t *testing.T) {
	p := &podLocalProvider{self: "reviewer-7", run: (&fakePodLocalRunner{}).run}
	for _, check := range []struct {
		name string
		err  error
	}{
		{"Start", p.Start(context.Background(), "main", runtime.Config{})},
		{"Stop", p.Stop("reviewer-7")},
		{"Interrupt", p.Interrupt("main")},
		{"Attach", p.Attach("main")},
		{"Nudge", p.Nudge("main", runtime.TextContent("hi"))},
		{"Peek", func() error { _, err := p.Peek("main", 10); return err }()},
		{"ListRunning", func() error { _, err := p.ListRunning(""); return err }()},
		{"GetLastActivity", func() error { _, err := p.GetLastActivity("main"); return err }()},
		{"ClearScrollback", p.ClearScrollback("main")},
		{"CopyTo", p.CopyTo("main", "/src", "dst")},
		{"SendKeys", p.SendKeys("main", "Enter")},
		{"RunLive", p.RunLive("main", runtime.Config{})},
	} {
		t.Run(check.name, func(t *testing.T) {
			if !errors.Is(check.err, guard.ErrControllerOwnedRuntime) {
				t.Fatalf("%s = %v, want the controller-owned refusal", check.name, check.err)
			}
			if errors.Is(check.err, runtime.ErrSessionNotFound) || errors.Is(check.err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s = %v, refusal must not classify as session absence or unavailable runtime", check.name, check.err)
			}
		})
	}
	if !errors.Is(p.Stop("main"), runtime.ErrStopRefused) {
		t.Fatalf("Stop = %v, want a stop refusal (runtime.ErrStopRefused)", p.Stop("main"))
	}
}

// TestPodLocalStopRefusalSurvivesCleanup drives the stop the recycle path
// takes: runtime.StopForCleanup absorbs only an all-session-gone answer, so
// the pod-local Stop refusal — a stop refusal, not a gone session —
// propagates out of the cleanup wrapper instead of reporting the session
// cleaned up.
func TestPodLocalStopRefusalSurvivesCleanup(t *testing.T) {
	p := &podLocalProvider{self: "reviewer-7", run: (&fakePodLocalRunner{}).run}
	err := runtime.StopForCleanup(p, "reviewer-7")
	if err == nil {
		t.Fatal("StopForCleanup(pod-local) = nil, want the refusal to propagate")
	}
	if !errors.Is(err, guard.ErrControllerOwnedRuntime) || !errors.Is(err, runtime.ErrStopRefused) {
		t.Fatalf("StopForCleanup = %v, want both the controller-owned and the stop refusal", err)
	}
}

// TestPodLocalHasNoFreshObservationCapability pins the observation contract:
// the provider implements no fresh-observation interface, so a false probe of
// its IsRunning can never be escalated by Manager.runtimeConfirmedGone into
// confirmed absence of another session; IsAttached and ProcessAlive answer
// false without probing at all.
func TestPodLocalHasNoFreshObservationCapability(t *testing.T) {
	p := &podLocalProvider{self: "reviewer-7", run: (&fakePodLocalRunner{}).run}
	if runtime.FreshReadable(p) {
		t.Fatal("FreshReadable(pod-local) = true, want false: no fresh-observation capability")
	}
	var sp runtime.Provider = p
	if _, ok := sp.(runtime.LivenessObserverWithError); ok {
		t.Fatal("pod-local claims LivenessObserverWithError, want no fresh liveness observation")
	}
	if _, ok := sp.(runtime.FreshLivenessObserver); ok {
		t.Fatal("pod-local claims FreshLivenessObserver, want no fresh liveness observation")
	}
	if p.IsAttached("main") {
		t.Fatal("IsAttached = true, want false")
	}
	if p.ProcessAlive("main", nil) {
		t.Fatal("ProcessAlive = true, want false")
	}
}
