package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLaunchd models the launchd state that matters for the supervisor: the
// persistent disable override that `gc supervisor stop` writes, and whether
// the agent is registered. `launchctl disable` survives logout and reboot, so
// only `launchctl enable` clears it.
type fakeLaunchd struct {
	disabled bool
	loaded   bool
	calls    []string
}

func (f *fakeLaunchd) run(args ...string) error {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "disable":
		f.disabled = true
	case "enable":
		f.disabled = false
	case "bootout", "unload":
		f.loaded = false
	case "load", "kickstart":
		f.loaded = true
	}
	return nil
}

// installFakeLaunchd points the launchd seams at a temporary plist and a
// fake launchctl, and pretends the host is macOS so the test runs anywhere.
func installFakeLaunchd(t *testing.T) (*fakeLaunchd, string) {
	t.Helper()
	plist := filepath.Join(t.TempDir(), "com.gascity.supervisor.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLaunchd{}
	origGOOS := supervisorRuntimeGOOS
	origPlist := supervisorLaunchdPlistPath
	origRun := supervisorLaunchctlRun
	supervisorRuntimeGOOS = "darwin"
	supervisorLaunchdPlistPath = func() string { return plist }
	supervisorLaunchctlRun = fake.run
	t.Cleanup(func() {
		supervisorRuntimeGOOS = origGOOS
		supervisorLaunchdPlistPath = origPlist
		supervisorLaunchctlRun = origRun
	})
	return fake, plist
}

// TestSupervisorStartClearsLaunchdOverrideAfterStop is the regression for
// sc-10j84: `gc supervisor stop` disabled the launchd agent persistently and
// `gc supervisor start` forked a bare child, so the machine reported healthy
// while nothing would restart the supervisor after logout or reboot.
func TestSupervisorStartClearsLaunchdOverrideAfterStop(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
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
	if strings.Contains(stderr.String(), "falling back") {
		t.Errorf("start fell back to a foreground supervisor: %q", stderr.String())
	}
}

// TestStartSupervisorViaLaunchdSkipsNonDarwin keeps the systemd path clear of
// the macOS fix: on Linux, start must never touch launchctl.
func TestStartSupervisorViaLaunchdSkipsNonDarwin(t *testing.T) {
	fake, _ := installFakeLaunchd(t)
	supervisorRuntimeGOOS = "linux"

	var stderr bytes.Buffer
	if startSupervisorViaLaunchd(&stderr) {
		t.Fatal("startSupervisorViaLaunchd = true on linux, want false")
	}
	if len(fake.calls) != 0 {
		t.Errorf("launchctl calls on linux = %v, want none", fake.calls)
	}
}

// TestStartSupervisorViaLaunchdSkipsUninstalledAgent keeps `gc supervisor
// start` working on a Mac that never installed the service.
func TestStartSupervisorViaLaunchdSkipsUninstalledAgent(t *testing.T) {
	fake, plist := installFakeLaunchd(t)
	if err := os.Remove(plist); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if startSupervisorViaLaunchd(&stderr) {
		t.Fatal("startSupervisorViaLaunchd = true without a plist, want false")
	}
	if len(fake.calls) != 0 {
		t.Errorf("launchctl calls without a plist = %v, want none", fake.calls)
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

func TestSupervisorLaunchdSupervisionWarning(t *testing.T) {
	cases := []struct {
		name     string
		disabled bool
		absent   bool
		want     string
	}{
		{name: "healthy", want: ""},
		{name: "disabled override", disabled: true, want: "is disabled"},
		{name: "not loaded", absent: true, want: "is not loaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeLaunchd(t)
			origDisabled := supervisorLaunchdDisabled
			origLoaded := supervisorLaunchdLoaded
			supervisorLaunchdDisabled = func(string) (bool, bool) { return tc.disabled, true }
			supervisorLaunchdLoaded = func(string) (bool, bool, string) { return !tc.absent, tc.absent, "" }
			t.Cleanup(func() {
				supervisorLaunchdDisabled = origDisabled
				supervisorLaunchdLoaded = origLoaded
			})

			got := supervisorLaunchdSupervisionWarning()
			if tc.want == "" && got != "" {
				t.Fatalf("warning = %q, want empty", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestSupervisorStatusJSONReportsDisabledLaunchdAgent proves status no longer
// answers a bare running=true while launchd holds a disable override.
func TestSupervisorStatusJSONReportsDisabledLaunchdAgent(t *testing.T) {
	clearGCEnv(t)
	installFakeLaunchd(t)

	origSM := supervisorServiceManagerActive
	origAPI := supervisorAPIReachable
	origDisabled := supervisorLaunchdDisabled
	supervisorServiceManagerActive = func() bool { return true }
	supervisorAPIReachable = func() bool { return false }
	supervisorLaunchdDisabled = func(string) (bool, bool) { return true, true }
	t.Cleanup(func() {
		supervisorServiceManagerActive = origSM
		supervisorAPIReachable = origAPI
		supervisorLaunchdDisabled = origDisabled
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
	if !strings.Contains(payload.SupervisionWarning, "is disabled") {
		t.Errorf("supervision_warning = %q, want a disabled-agent warning", payload.SupervisionWarning)
	}
}
