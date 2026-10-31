package ssh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errBoxUnreachable is shaped as shellRunner.run renders an ssh exit 255: the
// box could not be reached, so nothing about the session is known.
var errBoxUnreachable = errors.New("ssh u@box: connection failed (ssh exit 255)")

// unreachableRunners are the transport failures a verb can meet: ssh's own
// exit 255 and a context deadline (shellRunner.run returns both as errors).
func unreachableRunners() map[string]func() *fakeRunner {
	return map[string]func() *fakeRunner{
		"ssh exit 255": func() *fakeRunner { return &fakeRunner{rawPanes: true, code: -1, err: errBoxUnreachable} },
		"deadline": func() *fakeRunner {
			return &fakeRunner{rawPanes: true, code: -1, err: fmt.Errorf("ssh u@box: %w", context.DeadlineExceeded)}
		},
	}
}

func wantUnavailable(t *testing.T, verb string, err error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("%s err = %v, want it to wrap runtime.ErrRuntimeUnavailable", verb, err)
	}
}

// An unreachable box is "could not tell", never "absent": every error-bearing
// read and write wraps runtime.ErrRuntimeUnavailable, so the fail-closed gates
// in cmd/gc defer instead of acting on an empty answer.
func TestProvider_TransportFailureIsRuntimeUnavailable(t *testing.T) {
	for kind, newRunner := range unreachableRunners() {
		t.Run(kind, func(t *testing.T) {
			p := providerWith(newRunner())
			val, err := p.GetMeta("s", "GC_DRAIN_ACK")
			wantUnavailable(t, "GetMeta", err)
			if val != "" {
				t.Errorf("GetMeta = %q on a transport failure, want empty", val)
			}
			wantUnavailable(t, "SetMeta", p.SetMeta("s", "GC_DRAIN_ACK", "1"))
			wantUnavailable(t, "SetMeta(secret)", p.SetMeta("s", "GC_INSTANCE_TOKEN", "tok-not-a-real-credential"))
			wantUnavailable(t, "RemoveMeta", p.RemoveMeta("s", "GC_DRAIN_ACK"))
			out, err := p.Peek("s", 5)
			wantUnavailable(t, "Peek", err)
			if out != "" {
				t.Errorf("Peek = %q on a transport failure, want empty", out)
			}
			attached, err := p.IsAttachedWithError("s")
			wantUnavailable(t, "IsAttachedWithError", err)
			if !runtime.AttachProbeHolds(attached, err) {
				t.Error("an unreachable box must hold a destructive action (AttachProbeHolds)")
			}
			_, err = p.ObserveLivenessWithError("s", []string{"claude"})
			wantUnavailable(t, "ObserveLivenessWithError", err)
			_, err = p.ObserveLivenessWithError("s", nil)
			wantUnavailable(t, "ObserveLivenessWithError(no names)", err)
			if !p.ProcessAlive("s", []string{"claude"}) {
				t.Error("ProcessAlive = false on a transport failure: an unreachable box must not read dead")
			}
		})
	}
}

// Peek whose pane lookup succeeds but whose capture is lost in transit
// reports the failure rather than an empty pane.
func TestProvider_PeekCaptureTransportFailureIsUnavailable(t *testing.T) {
	f := &fakeRunner{code: -1, err: errBoxUnreachable}
	_, err := providerWith(f).Peek("s", 5)
	wantUnavailable(t, "Peek", err)
}

// The production composition (NewSeamBacked) forwards the error-bearing
// probes, as exec's seam-backed provider does: without the passthrough the
// seam re-resolves the box through bool IsRunning and drops the failure.
func TestSeamBacked_ForwardsTransportFailures(t *testing.T) {
	for kind, newRunner := range unreachableRunners() {
		t.Run(kind, func(t *testing.T) {
			sp := newSeamBacked(providerWith(newRunner()))
			if _, ok := sp.(runtime.AttachmentObserverWithError); !ok {
				t.Fatal("seam-backed ssh provider is not an AttachmentObserverWithError")
			}
			if _, ok := sp.(runtime.LivenessObserverWithError); !ok {
				t.Fatal("seam-backed ssh provider is not a LivenessObserverWithError")
			}
			attached, err := runtime.IsAttachedWithError(sp, "s")
			wantUnavailable(t, "runtime.IsAttachedWithError", err)
			if !runtime.AttachProbeHolds(attached, err) {
				t.Error("an unreachable box must hold a destructive action (AttachProbeHolds)")
			}
			_, err = runtime.ObserveLivenessWithError(sp, "s", []string{"claude"})
			wantUnavailable(t, "runtime.ObserveLivenessWithError", err)
			_, err = sp.Peek("s", 5)
			wantUnavailable(t, "Peek", err)
			_, err = sp.GetMeta("s", "GC_DRAIN_ACK")
			wantUnavailable(t, "GetMeta", err)
			wantUnavailable(t, "SetMeta", sp.SetMeta("s", "GC_DRAIN_ACK", "1"))
			wantUnavailable(t, "RemoveMeta", sp.RemoveMeta("s", "GC_DRAIN_ACK"))
		})
	}
}

func TestProvider_IsAttachedWithError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		out          string
		code         int
		wantAttached bool
		wantErr      error
	}{
		{"one client", "s|1", 0, true, nil},
		{"two clients", "s|2", 0, true, nil},
		{"no client", "s|0", 0, false, nil},
		{"session missing", "", 1, false, runtime.ErrSessionNotFound},
		{"missing target answered empty", "", 0, false, runtime.ErrSessionNotFound},
		{"another session answered", "s2|1", 0, false, runtime.ErrRuntimeUnavailable},
		{"unparsable count", "s|garbage", 0, false, runtime.ErrRuntimeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if isTmux("display-message")(argv) {
					return []byte(tc.out + "\n"), tc.code, nil
				}
				return nil, 0, nil
			}}
			p := providerWith(f)
			attached, err := p.IsAttachedWithError("s")
			if attached != tc.wantAttached {
				t.Errorf("attached = %v, want %v", attached, tc.wantAttached)
			}
			if (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if got := p.IsAttached("s"); got != tc.wantAttached {
				t.Errorf("IsAttached = %v, want %v", got, tc.wantAttached)
			}
		})
	}
	if _, err := providerWith(&fakeRunner{}).IsAttachedWithError("bad.name"); !errors.Is(err, ErrInvalidSessionName) {
		t.Errorf("IsAttachedWithError(bad.name) err = %v, want ErrInvalidSessionName", err)
	}
}

func TestProvider_ObserveLivenessWithError(t *testing.T) {
	table := processTable("100", "100 1 zsh", "101 100 claude")
	for _, tc := range []struct {
		name    string
		hasCode int
		names   []string
		want    runtime.Liveness
	}{
		{"running, agent alive", 0, []string{"claude"}, runtime.Liveness{Running: true, Alive: true}},
		{"running, agent gone", 0, []string{"codex"}, runtime.Liveness{Running: true}},
		{"running, no names", 0, nil, runtime.Liveness{Running: true, Alive: true}},
		{"session missing", 1, []string{"claude"}, runtime.Liveness{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{respond: func(argv []string) ([]byte, int, error) {
				if isTmux("has-session")(argv) {
					return nil, tc.hasCode, nil
				}
				return []byte(table), 0, nil
			}}
			got, err := providerWith(f).ObserveLivenessWithError("s", tc.names)
			if err != nil {
				t.Fatalf("ObserveLivenessWithError: %v", err)
			}
			if got != tc.want {
				t.Errorf("ObserveLivenessWithError = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// deadlineRunner records whether each call's context carried a deadline and,
// when block is set, waits out that deadline as a hung ssh client would.
type deadlineRunner struct {
	fakeRunner
	deadlines []bool
	block     bool
}

func (d *deadlineRunner) run(ctx context.Context, ep Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	_, ok := ctx.Deadline()
	d.deadlines = append(d.deadlines, ok)
	if d.block {
		<-ctx.Done()
		return nil, -1, fmt.Errorf("ssh %s: %w", ep.target(), ctx.Err())
	}
	return d.fakeRunner.run(ctx, ep, argv, stdin)
}

// Provider methods take no context, so each bounds its own remote calls: a box
// that stops answering mid-call cannot hang the controller.
func TestProvider_VerbsAreBounded(t *testing.T) {
	d := &deadlineRunner{}
	p := &Provider{conn: &Conn{ep: Endpoint{User: "u", Host: "box"}, run: d}}
	_ = p.Stop("s")
	_ = p.IsRunning("s")
	_ = p.IsAttached("s")
	_, _ = p.IsAttachedWithError("s")
	_ = p.ProcessAlive("s", []string{"claude"})
	_, _ = p.ObserveLivenessWithError("s", []string{"claude"})
	_ = p.Nudge("s", runtime.TextContent("hi"))
	_ = p.SendKeys("s", "Enter")
	_, _ = p.Peek("s", 5)
	_ = p.Interrupt("s")
	_ = p.ClearScrollback("s")
	_ = p.SetMeta("s", "K", "v")
	_ = p.SetMeta("s", "GC_INSTANCE_TOKEN", "tok-not-a-real-credential")
	_, _ = p.GetMeta("s", "K")
	_ = p.RemoveMeta("s", "K")
	_, _ = p.ListRunning("")
	_, _ = p.GetLastActivity("s")
	if len(d.deadlines) < 17 {
		t.Fatalf("recorded %d remote calls, want one or more per verb", len(d.deadlines))
	}
	for i, ok := range d.deadlines {
		if !ok {
			t.Errorf("remote call %d %q ran without a deadline", i, d.calls[i])
		}
	}

	hung := &deadlineRunner{block: true}
	p = &Provider{conn: &Conn{ep: Endpoint{User: "u", Host: "box"}, run: hung}, verbTimeout: 10 * time.Millisecond}
	_, err := p.GetMeta("s", "K")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetMeta on a hung box = %v, want ErrRuntimeUnavailable and DeadlineExceeded", err)
	}
}

// sshArgs adds connect and keepalive bounds so a dead box fails fast, but
// only where the user's ssh configuration leaves them unset: a command-line
// -o beats ~/.ssh/config, so adding one unconditionally would override it.
func TestSSHArgs_TransportDefaults(t *testing.T) {
	defaults := []string{
		"-o", "ConnectTimeout=" + defaultConnectTimeout,
		"-o", "ServerAliveInterval=" + defaultServerAliveInterval,
		"-o", "ServerAliveCountMax=" + defaultServerAliveCountMax,
	}
	got := sshArgs(Endpoint{Host: "box"}, sshConfigured{}, []string{"true"})
	want := append(append([]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}, defaults...), "--", "box", "'true'")
	if !slices.Equal(got, want) {
		t.Errorf("sshArgs (nothing configured) =\n  %v\nwant\n  %v", got, want)
	}

	got = sshArgs(Endpoint{Host: "box"}, sshConfigured{connectTimeout: true, serverAlive: true}, []string{"true"})
	for _, d := range defaults {
		if d != "-o" && slices.Contains(got, d) {
			t.Errorf("sshArgs overrode a user-configured option with %q: %v", d, got)
		}
	}
}

func TestParseSSHConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want sshConfigured
	}{
		{"ssh defaults", "user u\nserveralivecountmax 3\nserveraliveinterval 0\nconnecttimeout none\n", sshConfigured{}},
		{"user set both", "connecttimeout 5\nserveraliveinterval 30\nserveralivecountmax 2\n", sshConfigured{connectTimeout: true, serverAlive: true}},
		{"user set connect timeout", "ConnectTimeout 7\nServerAliveInterval 0\n", sshConfigured{connectTimeout: true}},
		{"no answer", "", sshConfigured{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSSHConfigured(tc.out); got != tc.want {
				t.Errorf("parseSSHConfigured = %+v, want %+v", got, tc.want)
			}
		})
	}
}
