package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errBoxUnreachable is shaped as shellRunner.run renders an ssh exit 255: the
// box could not be reached, so nothing about the session is known.
var errBoxUnreachable = errors.New("ssh u@box: connection failed (ssh exit 255)")

// errBoxRefused is shellRunner.run's error for an ssh exit 255 whose stderr
// names a refusal at authentication: the box answered, and refused this
// client's key.
var errBoxRefused = exit255Error(Endpoint{User: "u", Host: "box"}, "u@box: Permission denied (publickey).\n")

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

// ssh's own exit 255 is a refusal at authentication when its stderr says so
// (a rejected key, too many offers, a host key that does not match its pin),
// and a failure to reach the box otherwise; both keep ssh's own words.
func TestExit255Error_ClassifiesRefusals(t *testing.T) {
	ep := Endpoint{User: "u", Host: "box"}
	for _, tc := range []struct {
		stderr  string
		refused bool
	}{
		{"u@box: Permission denied (publickey).", true},
		{"Received disconnect from 10.0.0.2 port 22:2: Too many authentication failures\nDisconnected from 10.0.0.2 port 22", true},
		{"@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@\nHost key for box has changed and you have requested strict checking.\nHost key verification failed.", true},
		{"ssh: connect to host box port 22: Connection timed out", false},
		{"ssh: connect to host box port 22: Connection refused", false},
		{"ssh: Could not resolve hostname box: Name or service not known", false},
		{"", false},
	} {
		_, _, err := transportResult(nil, -1, exit255Error(ep, tc.stderr))
		if got := errors.Is(err, errAuthFailed); got != tc.refused {
			t.Errorf("stderr %q: refused = %v, want %v (err %v)", tc.stderr, got, tc.refused, err)
		}
		if got := errors.Is(err, runtime.ErrRuntimeUnavailable); got == tc.refused {
			t.Errorf("stderr %q: ErrRuntimeUnavailable = %v, want %v (err %v)", tc.stderr, got, !tc.refused, err)
		}
		if last := lastLine(tc.stderr); !strings.Contains(err.Error(), last) {
			t.Errorf("stderr %q: err %q dropped ssh's own words", tc.stderr, err)
		}
	}
}

// A box that refuses this client answered, and keeps refusing until an
// operator fixes the key or the pin: a start fails with ssh's message (so it
// is recorded and backs off) instead of deferring as unavailable on every
// tick, while every probe still holds rather than reading the box empty, and
// no write reads as done.
func TestProvider_AuthFailureFailsStartAndHoldsProbes(t *testing.T) {
	p := providerWith(&fakeRunner{rawPanes: true, code: -1, err: errBoxRefused})
	err := p.Start(context.Background(), "s", runtime.Config{Command: "agent"})
	if !errors.Is(err, errAuthFailed) || errors.Is(err, runtime.ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "Permission denied (publickey)") {
		t.Errorf("Start on a refusing box = %v, want a start failure naming ssh's refusal, not ErrRuntimeUnavailable", err)
	}
	sp := newSeamBacked(p)
	if _, err := runtime.ObserveLivenessWithError(sp, "s", []string{"claude"}); err == nil || errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("liveness on a refusing box: err %v, want the refusal", err)
	}
	if attached, err := runtime.IsAttachedWithError(sp, "s"); !runtime.AttachProbeHolds(attached, err) {
		t.Errorf("attach probe on a refusing box = (%v, %v), want it to hold a destructive action", attached, err)
	}
	if val, err := sp.GetMeta("s", "GC_INSTANCE_TOKEN"); err == nil || val != "" {
		t.Errorf("GetMeta on a refusing box = (%q, %v), want the refusal", val, err)
	}
	if !p.ProcessAlive("s", []string{"claude"}) {
		t.Error("ProcessAlive = false on a refusing box: could-not-tell must not read dead")
	}
	// A control marker and the instance token are not argv-safe, so SetMeta
	// stages them on the box first: a refused staging call is no write.
	for composition, prov := range map[string]runtime.Provider{"raw": p, "seam-backed": sp} {
		if out, err := prov.Peek("s", 5); !errors.Is(err, errAuthFailed) || out != "" {
			t.Errorf("%s Peek on a refusing box = (%q, %v), want the refusal, not an empty pane", composition, out, err)
		}
		for _, key := range []string{"GC_DRAIN_ACK", "GC_INSTANCE_TOKEN"} {
			if err := prov.SetMeta("s", key, "1"); !errors.Is(err, errAuthFailed) {
				t.Errorf("%s SetMeta(%s) on a refusing box = %v, want the refusal, not a write", composition, key, err)
			}
		}
	}
}

// A start's context is its caller's startup budget, not the provider's bound
// on the box. A start or relaunch that overruns it inside one of its steps
// fails with the deadline, which the reconciler records as a timeout and
// backs off; as runtime.ErrRuntimeUnavailable, which the step's ssh call
// wraps around the ended context, it would be deferred on every tick with no
// failure recorded.
func TestProvider_StartDeadlineIsNotUnavailable(t *testing.T) {
	isPreStart := func(argv []string) bool {
		return len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c" && strings.HasSuffix(argv[2], "prepare-workdir")
	}
	cfg := runtime.Config{
		Command:  "agent",
		PreStart: []string{"prepare-workdir"},
		Env:      map[string]string{"GC_INSTANCE_TOKEN": "tok-not-a-real-credential"},
	}
	for _, tc := range []struct {
		verb, step string
		hangs      func([]string) bool
	}{
		{"start", "staging", isStagingScript},
		{"start", "pre_start", isPreStart},
		{"start", "new-session", isTmux("start-server")},
		{"relaunch", "staging", isStagingScript},
		{"relaunch", "agent pane", isAgentPaneLookup},
		{"relaunch", "respawn-pane", isTmux("respawn-pane")},
	} {
		t.Run(tc.verb+"/"+tc.step, func(t *testing.T) {
			relaunch := tc.verb == "relaunch"
			r := &deadlineRunner{hangs: tc.hangs, fakeRunner: fakeRunner{respond: answerStaging(func(argv []string) ([]byte, int, error) {
				if isTmux("has-session")(argv) && !relaunch {
					return nil, 1, nil // Start's precheck: not yet running
				}
				return nil, 0, nil
			})}}
			p := &Provider{conn: &Conn{ep: Endpoint{User: "u", Host: "box"}, run: r}}
			start := p.Start
			if relaunch {
				start = p.Relaunch
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := start(ctx, "s", cfg)
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("%s whose deadline ran out in %s = %v, want the deadline, not runtime.ErrRuntimeUnavailable", tc.verb, tc.step, err)
			}
		})
	}
}

// A start or probe that cannot reach the box defers without recording why,
// so the provider logs the cause once per episode: when the box stops
// answering, and when it starts refusing this client, until it answers again.
// A caller's own cancellation is no episode.
func TestProvider_LogsEachTransportEpisodeOnce(t *testing.T) {
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	var fail error
	p := providerWith(&fakeRunner{respond: func([]string) ([]byte, int, error) {
		if fail != nil {
			return nil, -1, fail
		}
		return nil, 0, nil
	}})
	observe := func(err error, times int) {
		fail = err
		for range times {
			_, _ = p.ObserveLivenessWithError("s", nil)
		}
	}
	lines := func() int { return strings.Count(logged.String(), "\n") }

	observe(errBoxUnreachable, 3)
	if n := strings.Count(logged.String(), "box unreachable"); n != 1 || lines() != 1 {
		t.Fatalf("unreachable episode logged %d times:\n%s", n, logged.String())
	}
	observe(errBoxRefused, 2)
	if n := strings.Count(logged.String(), "Permission denied (publickey)"); n != 1 || lines() != 2 {
		t.Fatalf("refusing box logged %d times, want once with ssh's words:\n%s", n, logged.String())
	}
	observe(nil, 1)
	observe(fmt.Errorf("ssh u@box: %w", context.Canceled), 2)
	if lines() != 2 {
		t.Fatalf("an answering box or a canceled call logged:\n%s", logged.String())
	}
	observe(errBoxUnreachable, 2)
	if n := strings.Count(logged.String(), "box unreachable"); n != 2 || lines() != 3 {
		t.Fatalf("a new unreachable episode logged %d unreachable lines, want 2:\n%s", n, logged.String())
	}
	if !strings.Contains(logged.String(), "u@box") {
		t.Errorf("log does not name the box:\n%s", logged.String())
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

// A human attaches to an ssh session with ssh -t, and the box does not report
// it. The production composition must say so: implementing the error-bearing
// liveness probe makes the destructive-action fence read a leaf with no
// terminal as one nobody can be watching, and skip its quiet window.
func TestSeamBacked_ReportsAnAttachableTerminal(t *testing.T) {
	raw := providerWith(&fakeRunner{})
	got, want := newSeamBacked(raw).Capabilities(), raw.Capabilities()
	if got != want || !got.CanAttachTTY || got.CanReportAttachment {
		t.Errorf("seam-backed ssh Capabilities = %+v, want the raw provider's %+v: a terminal (CanAttachTTY) whose attachment is not reported", got, want)
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
// for each call hangs reports, waits out that deadline as a hung ssh client
// would, failing on it as shellRunner.run does.
type deadlineRunner struct {
	fakeRunner
	deadlines []bool
	hangs     func(argv []string) bool // nil hangs no call
}

func (d *deadlineRunner) run(ctx context.Context, ep Endpoint, argv []string, stdin []byte) ([]byte, int, error) {
	_, ok := ctx.Deadline()
	d.deadlines = append(d.deadlines, ok)
	if d.hangs != nil && d.hangs(argv) {
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

	hung := &deadlineRunner{hangs: func([]string) bool { return true }}
	p = &Provider{conn: &Conn{ep: Endpoint{User: "u", Host: "box"}, run: hung}, verbTimeout: 10 * time.Millisecond}
	_, err := p.GetMeta("s", "K")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetMeta on a hung box = %v, want ErrRuntimeUnavailable and DeadlineExceeded", err)
	}
}

// sshArgs adds connect and keepalive bounds so a dead box fails fast, but
// only where the user's ssh configuration leaves them unset: a command-line
// -o beats ~/.ssh/config, so adding one unconditionally would override it.
// The keepalive count is never added, so the user's ServerAliveCountMax (or
// ssh's default of 3) applies to the interval gc adds. LogLevel is always
// ERROR: under the user's QUIET, ssh prints no refusal for exit255Error to
// read, and the refusal would read as an unreachable box.
func TestSSHArgs_TransportDefaults(t *testing.T) {
	defaults := []string{
		"-o", "ConnectTimeout=" + defaultConnectTimeout,
		"-o", "ServerAliveInterval=" + defaultServerAliveInterval,
	}
	got := sshArgs(Endpoint{Host: "box"}, sshConfigured{}, []string{"true"})
	want := append(append([]string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "LogLevel=ERROR"}, defaults...), "--", "box", "'true'")
	if !slices.Equal(got, want) {
		t.Errorf("sshArgs (nothing configured) =\n  %v\nwant\n  %v", got, want)
	}
	for _, arg := range got {
		if strings.HasPrefix(arg, "ServerAliveCountMax=") {
			t.Errorf("sshArgs set %q, overriding the user's keepalive count: %v", arg, got)
		}
	}

	got = sshArgs(Endpoint{Host: "box"}, sshConfigured{connectTimeout: true, serverAlive: true}, []string{"true"})
	for _, d := range defaults {
		if d != "-o" && slices.Contains(got, d) {
			t.Errorf("sshArgs overrode a user-configured option with %q: %v", d, got)
		}
	}
	if !slices.Contains(got, "LogLevel=ERROR") {
		t.Errorf("sshArgs left ssh's log level to the user's configuration: %v", got)
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
		{"user set only the keepalive count", "serveralivecountmax 7\nserveraliveinterval 0\nconnecttimeout none\n", sshConfigured{}},
		{"no answer", "", sshConfigured{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSSHConfigured(tc.out); got != tc.want {
				t.Errorf("parseSSHConfigured = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The ssh -G probe runs apart from its callers: one whose context ends first
// proceeds with every default instead of waiting, and its cancellation does
// not cost a later caller the probe's answer.
func TestShellRunner_ConfigProbeHoldsNoCallerPastItsContext(t *testing.T) {
	release := make(chan struct{})
	r := &shellRunner{readConfig: func(Endpoint) sshConfigured {
		<-release
		return sshConfigured{connectTimeout: true}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := r.configuredFor(ctx, Endpoint{Host: "box"}); got != (sshConfigured{}) {
		t.Errorf("a canceled caller got %+v, want every default", got)
	}
	close(release)
	if got := r.configuredFor(context.Background(), Endpoint{Host: "box"}); got != (sshConfigured{connectTimeout: true}) {
		t.Errorf("a later caller got %+v, want the probe's answer", got)
	}
}

// A command a Match exec starts can outlive ssh -G holding its output open.
// The probe stops waiting once ssh is gone, whether ssh exited (its answer
// stands) or was killed, not once that command exits.
func TestReadSSHConfigured_StopsWaitingWhenSSHIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  string // how the fake ssh ends, after starting the command
		want sshConfigured
	}{
		{"ssh exits", "exit 0", sshConfigured{connectTimeout: true}},
		{"ssh is killed", "kill -9 $$", sshConfigured{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "match-exec.pid")
			client := filepath.Join(dir, "ssh")
			script := "#!/bin/sh\necho 'connecttimeout 7'\nsleep 600 &\necho $! > '" + pidFile + "'\n" + tc.end + "\n"
			if err := os.WriteFile(client, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { killRecordedProcess(t, pidFile) })
			got := make(chan sshConfigured, 1)
			go func() { got <- readSSHConfigured(context.Background(), client, Endpoint{Host: "box"}) }()
			select {
			case c := <-got:
				if c != tc.want {
					t.Errorf("readSSHConfigured = %+v, want %+v", c, tc.want)
				}
			case <-time.After(time.Minute):
				t.Fatal("the probe is still waiting on the output the Match exec command holds open")
			}
		})
	}
}

// killRecordedProcess kills the process whose pid the file records, if any.
func killRecordedProcess(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Errorf("pid file %s: %v", pidFile, err)
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}
