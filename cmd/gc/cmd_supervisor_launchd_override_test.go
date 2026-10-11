package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLaunchd models the launchd state that matters for the supervisor: the
// persistent disable override that `gc supervisor stop` writes, and whether
// the agent is registered. `launchctl disable` survives logout and reboot, so
// only `launchctl enable` clears it. Like launchctl, bootout fails for an
// agent that is not loaded, while the legacy unload exits zero for anything
// but improper usage, so stopping a booted-out agent succeeds only through
// stop's unload fallback. probes counts the read-only queries status makes
// against that state, and onKickstart lets a fake supervisor come up when
// launchd starts the job.
type fakeLaunchd struct {
	disabled    bool
	loaded      bool
	calls       []string
	probes      int
	onKickstart func()
}

func (f *fakeLaunchd) run(args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "disable":
		f.disabled = true
	case "enable":
		f.disabled = false
	case "bootout":
		if !f.loaded {
			return errors.New("exit status 3")
		}
		f.loaded = false
	case "unload":
		f.loaded = false
	case "load":
		f.loaded = true
	case "kickstart":
		f.loaded = true
		if f.onKickstart != nil {
			f.onKickstart()
		}
	}
	return nil
}

// launchdPlistRunning is a minimal agent plist whose ProgramArguments start
// binary.
func launchdPlistRunning(binary string) string {
	return "<plist><dict><key>ProgramArguments</key><array><string>" + binary + "</string><string>supervisor</string><string>run</string></array></dict></plist>"
}

// installFakeLaunchd points the launchd seams at a temporary plist and a
// fake launchctl, and pretends the host is macOS so the test runs anywhere.
// The plist launches this test binary, so `gc supervisor start` treats it as
// this gc's own service and starts it through launchd instead of forking.
// The disabled and loaded probes answer from the fake, so status sees what
// stop and start did to it.
func installFakeLaunchd(t *testing.T) (*fakeLaunchd, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(t.TempDir(), "com.gascity.supervisor.plist")
	if err := os.WriteFile(plist, []byte(launchdPlistRunning(self)), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLaunchd{}
	origGOOS := supervisorRuntimeGOOS
	origPlist := supervisorLaunchdPlistPath
	origStartPlist := supervisorStartLaunchdPlistPath
	origRun := supervisorLaunchctlRun
	origDisabled := supervisorLaunchdDisabled
	origLoaded := supervisorLaunchdLoaded
	supervisorRuntimeGOOS = "darwin"
	supervisorLaunchdPlistPath = func() string { return plist }
	supervisorStartLaunchdPlistPath = func() string { return plist }
	supervisorLaunchctlRun = fake.run
	supervisorLaunchdDisabled = func(string) (bool, bool) {
		fake.probes++
		return fake.disabled, true
	}
	supervisorLaunchdLoaded = func(string) (bool, bool, string) {
		fake.probes++
		return fake.loaded, !fake.loaded, ""
	}
	t.Cleanup(func() {
		supervisorRuntimeGOOS = origGOOS
		supervisorLaunchdPlistPath = origPlist
		supervisorStartLaunchdPlistPath = origStartPlist
		supervisorLaunchctlRun = origRun
		supervisorLaunchdDisabled = origDisabled
		supervisorLaunchdLoaded = origLoaded
	})
	return fake, plist
}

// installLiveSupervisorOutsideLaunchd sets up the state the status warning
// exists for: a supervisor answering its control socket as PID 4242 while
// launchd holds the disable override `gc supervisor stop` wrote and the job is
// booted out, as after a stop followed by a foreground `gc supervisor run`.
// The fake supervisor acknowledges stop at once and, like the real one, goes
// on shutting down after the acknowledgment: it stops answering exitAfterStop
// later, or not during the test when exitAfterStop is zero. A launchd
// kickstart brings it back, and start's readiness poll asks the same socket
// instead of the package's always-down test hook.
func installLiveSupervisorOutsideLaunchd(t *testing.T, exitAfterStop time.Duration) *fakeLaunchd {
	t.Helper()
	clearGCEnv(t)
	pinRealHome(t) // start refuses a HOME override before its already-running check
	t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	t.Setenv(supervisorSystemdUnitEnv, "")
	fake, _ := installFakeLaunchd(t)
	fake.disabled = true
	origAlive := supervisorAliveHook
	supervisorAliveHook = supervisorAlive
	t.Cleanup(func() { supervisorAliveHook = origAlive })

	var alive atomic.Bool
	alive.Store(true)
	fake.onKickstart = func() { alive.Store(true) }
	startTestSupervisorSocket(t, supervisorSocketPath(), func(cmd string) string {
		if !alive.Load() {
			return ""
		}
		switch cmd {
		case "ping":
			return "4242\n"
		case "stop":
			if exitAfterStop > 0 {
				time.AfterFunc(exitAfterStop, func() { alive.Store(false) })
			}
			return "ok\n"
		}
		return ""
	})
	return fake
}

// routeSupervisorStopThroughFakeLaunchd points stop's platform-service hooks
// at their launchd branches. Those hooks switch on the host OS rather than on
// supervisorRuntimeGOOS, so on a Linux host stop would otherwise ask systemd.
func routeSupervisorStopThroughFakeLaunchd(t *testing.T) {
	t.Helper()
	origUnload := unloadSupervisorServiceHook
	origVerify := verifySupervisorServiceStoppedHook
	unloadSupervisorServiceHook = func() error {
		return errors.Join(durablyStopSupervisorLaunchd(supervisorLaunchdLabel(), supervisorLaunchdPlistPath())...)
	}
	verifySupervisorServiceStoppedHook = func(deadline time.Time) error {
		label := supervisorLaunchdLabel()
		return waitForSupervisorLaunchdAbsent(label, supervisorLaunchdServiceTarget(label), deadline)
	}
	t.Cleanup(func() {
		unloadSupervisorServiceHook = origUnload
		verifySupervisorServiceStoppedHook = origVerify
	})
}

// TestSupervisorStartLaunchdPlistPathFollowsStatusSeam pins that start reads
// the installed plist through the seam status reads, so redirecting one can
// never leave the other on the real ~/Library/LaunchAgents plist.
func TestSupervisorStartLaunchdPlistPathFollowsStatusSeam(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "agent.plist")
	orig := supervisorLaunchdPlistPath
	supervisorLaunchdPlistPath = func() string { return plist }
	t.Cleanup(func() { supervisorLaunchdPlistPath = orig })

	if got := supervisorStartLaunchdPlistPath(); got != plist {
		t.Fatalf("supervisorStartLaunchdPlistPath() = %q, want the redirected %q", got, plist)
	}
}

// TestSupervisorStartClearsLaunchdOverrideAfterStop is the regression for
// sc-10j84: `gc supervisor stop` disabled the launchd agent persistently and
// `gc supervisor start` forked a bare child, so the machine reported healthy
// while nothing would restart the supervisor after logout or reboot.
func TestSupervisorStartClearsLaunchdOverrideAfterStop(t *testing.T) {
	clearGCEnv(t)
	pinRealHome(t)
	t.Setenv("GC_HOME", shortTempDir(t, "gc-home-"))
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	t.Setenv(supervisorSystemdUnitEnv, "")
	fake, plist := installFakeLaunchd(t)
	fake.loaded = true

	origAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 4242 }
	t.Cleanup(func() { supervisorAliveHook = origAlive })

	if errs := durablyStopSupervisorLaunchd(supervisorLaunchdLabel(), plist); len(errs) != 0 {
		t.Fatalf("durablyStopSupervisorLaunchd returned errors: %v", errs)
	}
	if !fake.disabled {
		t.Fatalf("stop did not write the disable override; calls=%v", fake.calls)
	}

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 0 {
		t.Fatalf("doSupervisorStart = %d, want 0; stderr=%q", code, stderr.String())
	}
	if fake.disabled {
		t.Errorf("launchd override still set after start; calls=%v", fake.calls)
	}
	if !fake.loaded {
		t.Errorf("launchd agent not loaded after start; calls=%v", fake.calls)
	}
	if strings.Contains(stderr.String(), "outside launchd") {
		t.Errorf("start fell back to a supervisor outside launchd: %q", stderr.String())
	}
}

func TestLaunchdPrintDisabledReportsDisabled(t *testing.T) {
	const out = `disabled services = {
	"com.gascity.supervisor" => %s
	"com.other.agent" => true
}`
	cases := []struct {
		name         string
		value        string
		wantDisabled bool
	}{
		{name: "legacy true spelling", value: "true", wantDisabled: true},
		{name: "modern disabled spelling", value: "disabled", wantDisabled: true},
		{name: "legacy false spelling", value: "false", wantDisabled: false},
		{name: "modern enabled spelling", value: "enabled", wantDisabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disabled, known := launchdPrintDisabledReportsDisabled(
				strings.Replace(out, "%s", tc.value, 1), "com.gascity.supervisor")
			if !known {
				t.Fatal("known = false, want true")
			}
			if disabled != tc.wantDisabled {
				t.Errorf("disabled = %v, want %v", disabled, tc.wantDisabled)
			}
		})
	}

	// A label with no entry carries no override. That is an answer, not an
	// unknown, so status must not warn about it.
	disabled, known := launchdPrintDisabledReportsDisabled(
		"disabled services = {\n\t\"com.other.agent\" => true\n}", "com.gascity.supervisor")
	if disabled || !known {
		t.Errorf("absent label = (%v, %v), want (false, true)", disabled, known)
	}
}

// TestSupervisorLaunchdSupervisionWarning pins the remedy to what each
// command accepts. start refuses while a supervisor answers the control
// socket, so a live one is stopped first, with --wait because a plain stop
// returns before it has exited. start also runs this gc outside launchd when
// the plist runs another gc, so that plist is rewritten by install --force.
func TestSupervisorLaunchdSupervisionWarning(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const (
		otherGC   = "/nonexistent/other-gc/bin/gc"
		disabled  = "is disabled; it will not start after logout or reboot. "
		notLoaded = "is not loaded; it will not restart if the supervisor exits. "
		stopFirst = "'gc supervisor stop --wait' then "
		startBack = "'gc supervisor start' to put it back under launchd"
	)
	reinstall := "'" + shellQuotePath(self) + " supervisor install --force' to make it run this gc and put it back under launchd"
	cases := []struct {
		name        string
		plist       string // "" runs this gc
		disabled    bool
		absent      bool
		socketAlive bool
		want        string // after "launchd agent <label> "; "" for no warning
	}{
		{name: "healthy"},
		{name: "healthy with live supervisor", socketAlive: true},
		{name: "healthy agent running another gc", plist: launchdPlistRunning(otherGC), socketAlive: true},
		{name: "disabled override", disabled: true, want: disabled + "Run 'gc supervisor start' to clear the override"},
		{name: "not loaded", absent: true, want: notLoaded + "Run 'gc supervisor start' to load it"},
		{name: "disabled override with live supervisor", disabled: true, socketAlive: true, want: disabled + "Run " + stopFirst + startBack},
		{name: "not loaded with live supervisor", absent: true, socketAlive: true, want: notLoaded + "Run " + stopFirst + startBack},
		{
			name: "disabled override running another gc", plist: launchdPlistRunning(otherGC), disabled: true,
			want: disabled + "It runs " + otherGC + ", not this gc; run " + reinstall,
		},
		{
			name: "not loaded running another gc with live supervisor", plist: launchdPlistRunning(otherGC), absent: true, socketAlive: true,
			want: notLoaded + "It runs " + otherGC + ", not this gc; run " + stopFirst + reinstall,
		},
		{
			name: "disabled override with unreadable ProgramArguments", plist: "<plist><dict></dict></plist>", disabled: true, socketAlive: true,
			want: disabled + "It runs (unreadable ProgramArguments), not this gc; run " + stopFirst + reinstall,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, plist := installFakeLaunchd(t)
			if tc.plist != "" {
				if err := os.WriteFile(plist, []byte(tc.plist), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fake.disabled = tc.disabled
			fake.loaded = !tc.absent

			want := ""
			if tc.want != "" {
				want = "launchd agent " + supervisorLaunchdLabel() + " " + tc.want
			}
			if got := supervisorLaunchdSupervisionWarning(tc.socketAlive); got != want {
				t.Fatalf("warning = %q, want %q", got, want)
			}
		})
	}
}

// TestSupervisorLaunchdSupervisionWarningSkipsAgentItCannotRead covers a Mac
// that never installed the agent, where start forks instead of loading
// anything, so a "not loaded" warning could never be cleared, and a plist
// status cannot read. Status warns only about a problem it found, so it stays
// quiet without asking launchd at all.
func TestSupervisorLaunchdSupervisionWarningSkipsAgentItCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		damage func(plist string) error
	}{
		{name: "uninstalled", damage: os.Remove},
		{name: "unreadable", damage: func(plist string) error {
			if err := os.Remove(plist); err != nil {
				return err
			}
			return os.Mkdir(plist, 0o700)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, plist := installFakeLaunchd(t)
			if err := tc.damage(plist); err != nil {
				t.Fatal(err)
			}
			fake.disabled = true
			fake.loaded = false

			for _, socketAlive := range []bool{false, true} {
				if got := supervisorLaunchdSupervisionWarning(socketAlive); got != "" {
					t.Errorf("warning(socketAlive=%v) = %q, want empty", socketAlive, got)
				}
			}
			if fake.probes != 0 || len(fake.calls) != 0 {
				t.Errorf("status asked launchd about an agent it cannot read: probes=%d calls=%v, want none", fake.probes, fake.calls)
			}
		})
	}
}

// TestSupervisorStatusJSONReportsDisabledLaunchdAgent proves status no longer
// answers a bare running=true while launchd holds a disable override. Here
// only the service manager vouches for the supervisor and the control socket
// is down, so start's already-running check passes and start alone is the
// remedy; stop would report that no supervisor is running.
func TestSupervisorStatusJSONReportsDisabledLaunchdAgent(t *testing.T) {
	clearGCEnv(t)
	fake, _ := installFakeLaunchd(t)
	fake.disabled = true
	fake.loaded = true

	origSM := supervisorServiceManagerActive
	origAPI := supervisorAPIReachable
	supervisorServiceManagerActive = func() bool { return true }
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() {
		supervisorServiceManagerActive = origSM
		supervisorAPIReachable = origAPI
	})

	var stdout, stderr bytes.Buffer
	if code := supervisorStatusWithOptions(&stdout, &stderr, true); code != 0 {
		t.Fatalf("status = %d, want 0; stderr=%q", code, stderr.String())
	}
	var payload struct {
		Running            bool   `json:"running"`
		Supervised         *bool  `json:"supervised"`
		SupervisionWarning string `json:"supervision_warning"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if !payload.Running {
		t.Fatalf("running = false, want true: %s", stdout.String())
	}
	if payload.Supervised == nil || *payload.Supervised {
		t.Errorf("supervised = %v, want false: %s", payload.Supervised, stdout.String())
	}
	want := "launchd agent " + supervisorLaunchdLabel() +
		" is disabled; it will not start after logout or reboot. Run 'gc supervisor start' to clear the override"
	if payload.SupervisionWarning != want {
		t.Errorf("supervision_warning = %q, want %q", payload.SupervisionWarning, want)
	}
}

// TestSupervisorStatusJSONOmitsSupervisionKeysWhenSupervised pins the absence
// contract: status asked launchd, launchd is supervising, and the payload
// carries neither supervision key.
func TestSupervisorStatusJSONOmitsSupervisionKeysWhenSupervised(t *testing.T) {
	clearGCEnv(t)
	fake, _ := installFakeLaunchd(t)
	fake.loaded = true

	origSM := supervisorServiceManagerActive
	origAPI := supervisorAPIReachable
	supervisorServiceManagerActive = func() bool { return true }
	supervisorAPIReachable = func() bool { return false }
	t.Cleanup(func() {
		supervisorServiceManagerActive = origSM
		supervisorAPIReachable = origAPI
	})

	var stdout, stderr bytes.Buffer
	if code := supervisorStatusWithOptions(&stdout, &stderr, true); code != 0 {
		t.Fatalf("status = %d, want 0; stderr=%q", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload["running"] != true {
		t.Fatalf("running = %v, want true: %s", payload["running"], stdout.String())
	}
	for _, key := range []string{"supervised", "supervision_warning"} {
		if value, ok := payload[key]; ok {
			t.Errorf("%s = %v for a supervised agent, want the key absent: %s", key, value, stdout.String())
		}
	}
	if fake.probes == 0 {
		t.Errorf("status never asked launchd, so the absent keys prove nothing")
	}
}

// TestSupervisorStatusTextWarnsLiveSupervisorToStopFirst covers the state the
// warning exists for: a supervisor answering its control socket while launchd
// holds the disable override. The warning goes to stderr and leaves the
// liveness report and the exit code alone. It names a stop first because
// start alone refuses while the socket answers, before it reaches launchctl.
func TestSupervisorStatusTextWarnsLiveSupervisorToStopFirst(t *testing.T) {
	fake := installLiveSupervisorOutsideLaunchd(t, 0)

	var stdout, stderr bytes.Buffer
	if code := supervisorStatusWithOptions(&stdout, &stderr, false); code != 0 {
		t.Fatalf("status = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "Supervisor is running (PID 4242)\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	want := "gc supervisor status: warning: launchd agent " + supervisorLaunchdLabel() +
		" is disabled; it will not start after logout or reboot." +
		" Run 'gc supervisor stop --wait' then 'gc supervisor start' to put it back under launchd\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}

	stderr.Reset()
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart = %d, want 1 while the supervisor answers; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "supervisor already running (PID 4242)") {
		t.Errorf("stderr = %q, want the already-running refusal", stderr.String())
	}
	if len(fake.calls) != 0 || !fake.disabled {
		t.Errorf("start alone acted on launchd: calls=%v disabled=%v, want no calls and the override still set", fake.calls, fake.disabled)
	}
}

// TestSupervisorLaunchdLiveRemedyPutsAgentBackUnderLaunchd runs the remedy the
// warning names for a live supervisor, against the fake supervisor socket and
// the fake launchctl. The agent is already booted out, so stop's bootout
// fails and stop exits 0 only through its unload fallback. stop --wait
// returns once the supervisor has exited, so the start after it reaches
// launchd, and status stops warning. A plain stop returns at the
// acknowledgment while the supervisor is still shutting down, so the start
// after it refuses and the override stays.
func TestSupervisorLaunchdLiveRemedyPutsAgentBackUnderLaunchd(t *testing.T) {
	t.Run("stop --wait then start", func(t *testing.T) {
		fake := installLiveSupervisorOutsideLaunchd(t, 50*time.Millisecond)
		routeSupervisorStopThroughFakeLaunchd(t)

		var stdout, stderr bytes.Buffer
		if code := stopSupervisorWithWait(&stdout, &stderr, true, 10*time.Second); code != 0 {
			t.Fatalf("stop --wait = %d, want 0; stderr=%q", code, stderr.String())
		}
		if code := doSupervisorStart(&stdout, &stderr); code != 0 {
			t.Fatalf("start after stop --wait = %d, want 0; stderr=%q", code, stderr.String())
		}
		if fake.disabled || !fake.loaded {
			t.Fatalf("after the remedy disabled=%v loaded=%v, want the agent enabled and loaded; calls=%v", fake.disabled, fake.loaded, fake.calls)
		}

		stdout.Reset()
		stderr.Reset()
		if code := supervisorStatusWithOptions(&stdout, &stderr, false); code != 0 {
			t.Fatalf("status after the remedy = %d, want 0; stderr=%q", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("status still warns after the remedy: %q", stderr.String())
		}
	})
	t.Run("plain stop then start", func(t *testing.T) {
		fake := installLiveSupervisorOutsideLaunchd(t, 0)
		routeSupervisorStopThroughFakeLaunchd(t)

		var stdout, stderr bytes.Buffer
		if code := stopSupervisorWithWait(&stdout, &stderr, false, 0); code != 0 {
			t.Fatalf("stop = %d, want 0; stderr=%q", code, stderr.String())
		}
		if code := doSupervisorStart(&stdout, &stderr); code != 1 {
			t.Fatalf("start after a plain stop = %d, want 1 while the supervisor is still shutting down; stderr=%q", code, stderr.String())
		}
		if !fake.disabled {
			t.Errorf("the override was cleared although start refused; calls=%v", fake.calls)
		}
	})
}
