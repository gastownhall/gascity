package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// supervisorBinaryFixture describes the running supervisor and the invoking
// gc for a mismatch test.
type supervisorBinaryFixture struct {
	supervisorVersion string
	supervisorBuildID string
	// procExe is what /proc/<pid>/exe reports; "" simulates an unreadable
	// process executable (macOS, or a supervisor under another uid).
	procExe string
	// psExe is what ps(1) reports on macOS; "" simulates no answer.
	psExe string
	// serviceBinary/service simulate the launchd plist or systemd unit that
	// manages the supervisor; "" means not service-managed.
	serviceBinary string
	service       string
	localExe      string
	localVersion  string
	localBuildID  string
	healthErr     error
}

// writeFakeGCBinary creates a distinct regular file standing in for a gc
// executable so supervisorSameBinary can compare real inodes.
func writeFakeGCBinary(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubSupervisorBinary(t *testing.T, f supervisorBinaryFixture) {
	t.Helper()
	t.Setenv(supervisorSystemdUnitEnv, "")
	oldAlive := supervisorAliveHook
	oldBaseURL := supervisorAPIBaseURLHook
	oldHealth := supervisorHealthStatusHook
	oldReadExe := readSupervisorExePathHook
	oldService := supervisorServiceBinaryHook
	oldLocalExe := localGCExecutableHook
	oldPS := readProcessExePathViaPSHook
	oldGOOS := supervisorRuntimeGOOS
	oldVersion, oldCommit := version, commit
	oldAllow := allowSupervisorMismatch
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorAPIBaseURLHook = oldBaseURL
		supervisorHealthStatusHook = oldHealth
		readSupervisorExePathHook = oldReadExe
		supervisorServiceBinaryHook = oldService
		localGCExecutableHook = oldLocalExe
		readProcessExePathViaPSHook = oldPS
		supervisorRuntimeGOOS = oldGOOS
		version, commit = oldVersion, oldCommit
		allowSupervisorMismatch = oldAllow
	})

	supervisorAliveHook = func() int { return 4242 }
	supervisorAPIBaseURLHook = func() (string, error) { return "http://127.0.0.1:0", nil }
	supervisorHealthStatusHook = func(context.Context, string) (SupervisorStatus, error) {
		if f.healthErr != nil {
			return SupervisorStatus{}, f.healthErr
		}
		return SupervisorStatus{Version: f.supervisorVersion, BuildID: f.supervisorBuildID, UptimeSec: 5}, nil
	}
	readSupervisorExePathHook = func(int) (string, error) {
		if f.procExe == "" {
			return "", errors.New("no /proc on this platform")
		}
		return f.procExe, nil
	}
	supervisorServiceBinaryHook = func() (string, string) { return f.serviceBinary, f.service }
	localGCExecutableHook = func() (string, error) { return f.localExe, nil }
	readProcessExePathViaPSHook = func(int) (string, error) {
		if f.psExe == "" {
			return "", errors.New("ps unavailable")
		}
		return f.psExe, nil
	}
	version, commit = f.localVersion, f.localBuildID
	allowSupervisorMismatch = false
}

// macOSHomebrewSupervisorFixture reproduces the reported case: a Homebrew
// gc 1.3.5 launchd supervisor is running and a gc 1.5.0-rc1 tarball binary
// is invoked.
func macOSHomebrewSupervisorFixture(t *testing.T) supervisorBinaryFixture {
	return supervisorBinaryFixture{
		supervisorVersion: "1.3.5",
		supervisorBuildID: "8ffc009ded",
		serviceBinary:     writeFakeGCBinary(t, "homebrew"),
		service:           `launchd service "com.gascity.supervisor"`,
		localExe:          writeFakeGCBinary(t, "tarball"),
		localVersion:      "1.5.0-rc1",
		localBuildID:      "e38ce9cc55",
	}
}

func TestDetectSupervisorBinaryMismatchNoSupervisor(t *testing.T) {
	stubSupervisorBinary(t, macOSHomebrewSupervisorFixture(t))
	supervisorAliveHook = func() int { return 0 }

	if _, mismatched := detectSupervisorBinaryMismatch(); mismatched {
		t.Fatal("mismatch reported with no running supervisor")
	}
}

func TestDetectSupervisorBinaryMismatchUnreachableHealthIsSilent(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	f.healthErr = errors.New("supervisor /health returned 500")
	stubSupervisorBinary(t, f)

	if _, mismatched := detectSupervisorBinaryMismatch(); mismatched {
		t.Fatal("mismatch reported when /health failed; the advisory check must fail open")
	}
}

func TestDetectSupervisorBinaryMismatchLaunchdDifferentInstall(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	stubSupervisorBinary(t, f)

	m, mismatched := detectSupervisorBinaryMismatch()
	if !mismatched {
		t.Fatal("mismatch not detected for a Homebrew 1.3.5 supervisor vs a 1.5.0-rc1 binary")
	}
	if !m.DifferentInstall {
		t.Fatal("DifferentInstall = false, want true (supervisor runs the Homebrew binary)")
	}
	if m.Supervisor.ExePath != f.serviceBinary {
		t.Fatalf("supervisor exe = %q, want the launchd plist binary %q", m.Supervisor.ExePath, f.serviceBinary)
	}
}

func TestSupervisorBinaryMismatchClassify(t *testing.T) {
	a := writeFakeGCBinary(t, "a")
	b := writeFakeGCBinary(t, "b")
	link := filepath.Join(t.TempDir(), "gc-link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name             string
		sup, local       gcBinaryIdentity
		wantMismatch     bool
		wantDifferent    bool
		wantBuildDiffers bool
	}{
		{
			name:  "same binary same build",
			sup:   gcBinaryIdentity{ExePath: a, Version: "1.5.0", BuildID: "abc1234"},
			local: gcBinaryIdentity{ExePath: a, Version: "1.5.0", BuildID: "abc1234"},
		},
		{
			name:  "symlinked path to same binary",
			sup:   gcBinaryIdentity{ExePath: link, Version: "1.5.0", BuildID: "abc1234"},
			local: gcBinaryIdentity{ExePath: a, Version: "1.5.0", BuildID: "abc1234"},
		},
		{
			name:         "in-place upgrade: same path, new version",
			sup:          gcBinaryIdentity{ExePath: a, Version: "1.4.2"},
			local:        gcBinaryIdentity{ExePath: a, Version: "1.5.0"},
			wantMismatch: true,
		},
		{
			name:             "in-place rebuild: same path, new build",
			sup:              gcBinaryIdentity{ExePath: a, Version: "dev", BuildID: "abc1234"},
			local:            gcBinaryIdentity{ExePath: a, Version: "dev", BuildID: "def5678"},
			wantMismatch:     true,
			wantBuildDiffers: true,
		},
		{
			name:          "different install, different version",
			sup:           gcBinaryIdentity{ExePath: a, Version: "1.3.5"},
			local:         gcBinaryIdentity{ExePath: b, Version: "v1.5.0-rc1"},
			wantMismatch:  true,
			wantDifferent: true,
		},
		{
			name:          "different install, identical release",
			sup:           gcBinaryIdentity{ExePath: a, Version: "1.5.1", BuildID: "abc1234"},
			local:         gcBinaryIdentity{ExePath: b, Version: "v1.5.1", BuildID: "abc1234def"},
			wantDifferent: true,
		},
		{
			name:          "different install, same version, one build unknown (homebrew-core vs tarball)",
			sup:           gcBinaryIdentity{ExePath: a, Version: "1.5.0", BuildID: "unknown"},
			local:         gcBinaryIdentity{ExePath: b, Version: "v1.5.0", BuildID: "e38ce9cc55"},
			wantDifferent: true,
		},
		{
			name:          "different install, identity unknown",
			sup:           gcBinaryIdentity{ExePath: a, Version: "dev", BuildID: "unknown"},
			local:         gcBinaryIdentity{ExePath: b, Version: "dev", BuildID: "unknown"},
			wantMismatch:  true,
			wantDifferent: true,
		},
		{
			name:  "unknown supervisor path, matching version",
			sup:   gcBinaryIdentity{Version: "1.5.0"},
			local: gcBinaryIdentity{ExePath: b, Version: "1.5.0"},
		},
		{
			name:         "unknown supervisor path, different version",
			sup:          gcBinaryIdentity{Version: "1.3.5"},
			local:        gcBinaryIdentity{ExePath: b, Version: "1.5.0"},
			wantMismatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := supervisorBinaryMismatch{Supervisor: tc.sup, Local: tc.local, RelaunchPath: tc.sup.ExePath}
			if got := m.classify(); got != tc.wantMismatch {
				t.Fatalf("classify() = %v, want %v", got, tc.wantMismatch)
			}
			if m.DifferentInstall != tc.wantDifferent {
				t.Fatalf("DifferentInstall = %v, want %v", m.DifferentInstall, tc.wantDifferent)
			}
			if m.BuildDiffers != tc.wantBuildDiffers {
				t.Fatalf("BuildDiffers = %v, want %v", m.BuildDiffers, tc.wantBuildDiffers)
			}
		})
	}
}

func TestSameGCBuildID(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc1234", "abc1234", true},
		{"abc1234", "ABC1234def0", true},
		{"abc1234-dirty", "abc1234def0-dirty", true},
		{"abc1234-dirty", "abc1234", false},
		{"abc12", "abc1234", false},
		{"abc1234", "abd1234", false},
	}
	for _, tc := range cases {
		if got := sameGCBuildID(tc.a, tc.b); got != tc.want {
			t.Errorf("sameGCBuildID(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckSupervisorBinaryBeforeRegisterRefusesDifferentInstall(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	stubSupervisorBinary(t, f)

	var stderr bytes.Buffer
	proceed, accepted := checkSupervisorBinaryBeforeRegister("gc init", &stderr, true)
	if proceed || accepted {
		t.Fatalf("proceed=%v accepted=%v, want refusal", proceed, accepted)
	}
	text := stderr.String()
	for _, want := range []string{
		"gc init: warning: the running gc supervisor does not match this gc binary",
		"supervisor: gc 1.3.5 (build 8ffc009ded) at " + f.serviceBinary + `, pid 4242, launchd service "com.gascity.supervisor"`,
		"this gc:    gc 1.5.0-rc1 (build e38ce9cc55) at " + resolveExecutablePath(f.localExe),
		shellQuotePath(resolveExecutablePath(f.localExe)) + " supervisor install --force",
		"use its gc: " + shellQuotePath(f.serviceBinary),
		"--allow-supervisor-mismatch",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stderr missing %q:\n%s", want, text)
		}
	}
}

func TestCheckSupervisorBinaryBeforeRegisterAllowFlagProceeds(t *testing.T) {
	stubSupervisorBinary(t, macOSHomebrewSupervisorFixture(t))
	allowSupervisorMismatch = true

	var stderr bytes.Buffer
	proceed, accepted := checkSupervisorBinaryBeforeRegister("gc start", &stderr, false)
	if !proceed || !accepted {
		t.Fatalf("proceed=%v accepted=%v, want both true with --allow-supervisor-mismatch", proceed, accepted)
	}
	if !strings.Contains(stderr.String(), "warning: the running gc supervisor does not match") {
		t.Fatalf("allowed mismatch must still warn:\n%s", stderr.String())
	}
}

func TestCheckSupervisorBinaryBeforeRegisterDirectSupervisorFix(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	f.procExe = f.serviceBinary
	f.serviceBinary, f.service = "", ""
	stubSupervisorBinary(t, f)

	var stderr bytes.Buffer
	if proceed, _ := checkSupervisorBinaryBeforeRegister("gc start", &stderr, false); proceed {
		t.Fatal("proceed = true, want refusal for a direct supervisor from another install")
	}
	if !strings.Contains(stderr.String(), " supervisor stop'), then rerun with this gc") {
		t.Fatalf("stderr missing stop-the-other-supervisor fix:\n%s", stderr.String())
	}
}

func TestCheckSupervisorBinaryBeforeRegisterInPlaceUpgrade(t *testing.T) {
	bin := writeFakeGCBinary(t, "shared")
	f := supervisorBinaryFixture{
		supervisorVersion: "1.5.0",
		supervisorBuildID: "abc1234",
		procExe:           bin,
		localExe:          bin,
		localVersion:      "1.5.1",
		localBuildID:      "def5678",
	}
	stubSupervisorBinary(t, f)

	// gc start leaves build drift to its own auto-restart handling.
	var startErr bytes.Buffer
	proceed, accepted := checkSupervisorBinaryBeforeRegister("gc start", &startErr, false)
	if !proceed || accepted {
		t.Fatalf("gc start proceed=%v accepted=%v, want proceed without accepting a different install", proceed, accepted)
	}
	if startErr.Len() != 0 {
		t.Fatalf("gc start must defer build drift to the drift check, got:\n%s", startErr.String())
	}

	// gc init has no auto-restart, so it warns but proceeds.
	var initErr bytes.Buffer
	if proceed, _ := checkSupervisorBinaryBeforeRegister("gc init", &initErr, true); !proceed {
		t.Fatal("gc init refused an in-place upgrade; it should only warn")
	}
	for _, want := range []string{"gc init: warning:", "'gc supervisor stop', then 'gc start'"} {
		if !strings.Contains(initErr.String(), want) {
			t.Fatalf("gc init stderr missing %q:\n%s", want, initErr.String())
		}
	}
}

func TestWarnSupervisorBinaryMismatchIsQuietWhenMatched(t *testing.T) {
	bin := writeFakeGCBinary(t, "same")
	stubSupervisorBinary(t, supervisorBinaryFixture{
		supervisorVersion: "1.5.1",
		supervisorBuildID: "abc1234",
		procExe:           bin,
		localExe:          bin,
		localVersion:      "1.5.1",
		localBuildID:      "abc1234",
	})
	var stderr bytes.Buffer
	warnSupervisorBinaryMismatch("gc status", &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("matched binaries must not warn, got:\n%s", stderr.String())
	}
}

func TestCmdCityStatusWarnsOnSupervisorBinaryMismatch(t *testing.T) {
	stubSupervisorBinary(t, macOSHomebrewSupervisorFixture(t))
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"status-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	_ = cmdCityStatus([]string{cityPath}, true, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "gc status: warning: the running gc supervisor does not match this gc binary") {
		t.Fatalf("stderr missing mismatch warning:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "warning:") {
		t.Fatalf("mismatch warning leaked into stdout (would corrupt --json):\n%s", stdout.String())
	}
}

func TestFinalizeInitRefusesDifferentInstallBeforeRegistration(t *testing.T) {
	clearInheritedBeadsEnv(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	stubInitDependencyChecks(t)
	stubSupervisorBinary(t, macOSHomebrewSupervisorFixture(t))

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"mismatch-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{
		commandName:           "gc init",
		skipProviderReadiness: true,
	})
	if code != 1 {
		t.Fatalf("finalizeInit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if calledRegister {
		t.Fatal("city was registered with a supervisor from a different gc installation")
	}
	if !strings.Contains(stderr.String(), "but not registered; after fixing the supervisor, run 'gc start'") {
		t.Fatalf("stderr missing recovery hint:\n%s", stderr.String())
	}
}

func TestSupervisorServiceBinaryReadsLaunchdPlist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "")
	oldGOOS, oldActive := supervisorRuntimeGOOS, supervisorLaunchdActive
	t.Cleanup(func() { supervisorRuntimeGOOS, supervisorLaunchdActive = oldGOOS, oldActive })
	supervisorRuntimeGOOS = "darwin"
	active := true
	supervisorLaunchdActive = func(string) bool { return active }

	plist := supervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "<plist><dict><key>ProgramArguments</key><array><string>/opt/homebrew/bin/gc</string><string>supervisor</string><string>run</string></array></dict></plist>"
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	bin, service := supervisorServiceBinary()
	if bin != "/opt/homebrew/bin/gc" {
		t.Fatalf("binary = %q, want /opt/homebrew/bin/gc", bin)
	}
	if !strings.Contains(service, "launchd service") || !strings.Contains(service, supervisorLaunchdLabel()) {
		t.Fatalf("service = %q, want launchd label", service)
	}

	active = false
	if bin, service := supervisorServiceBinary(); bin != "" || service != "" {
		t.Fatalf("inactive launchd service reported (%q, %q), want none", bin, service)
	}
}

func TestSupervisorServiceBinaryReadsSystemdUnit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(supervisorSystemdUnitEnv, "")
	oldGOOS, oldActive := supervisorRuntimeGOOS, supervisorSystemctlActive
	t.Cleanup(func() { supervisorRuntimeGOOS, supervisorSystemctlActive = oldGOOS, oldActive })
	supervisorRuntimeGOOS = "linux"
	supervisorSystemctlActive = func(string) bool { return true }

	unit := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=\"/home/linuxbrew/.linuxbrew/bin/gc\" supervisor run\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin, service := supervisorServiceBinary()
	if bin != "/home/linuxbrew/.linuxbrew/bin/gc" {
		t.Fatalf("binary = %q, want the unit ExecStart binary", bin)
	}
	if !strings.Contains(service, "systemd user unit") {
		t.Fatalf("service = %q, want systemd description", service)
	}
}

func TestDoStartRefusesDifferentInstallSupervisorBeforeDriftCheck(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DOLT", "skip")
	stubSupervisorBinary(t, macOSHomebrewSupervisorFixture(t))

	cityDir := filepath.Join(t.TempDir(), "mismatch-city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"mismatch-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	restartCalled := false
	oldHelpers := restartHelpersHook
	restartHelpersHook = func() restartHelpers {
		restartCalled = true
		return restartHelpers{}
	}
	t.Cleanup(func() { restartHelpersHook = oldHelpers })
	calledRegister := false
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(string, string, io.Writer, io.Writer) (bool, int) {
		calledRegister = true
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := doStartWithNameOverride([]string{cityDir}, false, &stdout, &stderr, "")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if restartCalled || calledRegister {
		t.Fatalf("restart=%v register=%v; a different-install supervisor must be refused before drift restart or registration", restartCalled, calledRegister)
	}
	if !strings.Contains(stderr.String(), "gc start: refusing to hand the city to a supervisor running a different gc installation") {
		t.Fatalf("stderr missing refusal:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Drift detected") {
		t.Fatalf("drift check ran after refusal:\n%s", stdout.String())
	}
}

// A supervisor forked by `gc supervisor start` outside the launchd job
// (#6966) has no active service, and macOS has no /proc: ps(1) must supply
// its executable so a foreign install is still detected.
func TestDetectSupervisorBinaryMismatchMacOSForkedOutsideLaunchd(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	f.psExe = f.serviceBinary
	f.serviceBinary, f.service = "", ""
	stubSupervisorBinary(t, f)
	supervisorRuntimeGOOS = "darwin"

	m, mismatched := detectSupervisorBinaryMismatch()
	if !mismatched || !m.DifferentInstall {
		t.Fatalf("mismatched=%v DifferentInstall=%v, want a detected foreign install", mismatched, m.DifferentInstall)
	}
	if m.Supervisor.ExePath != f.psExe || m.Service != "" {
		t.Fatalf("supervisor exe=%q service=%q, want ps path and no service", m.Supervisor.ExePath, m.Service)
	}

	var stderr bytes.Buffer
	printSupervisorBinaryMismatch(&stderr, "gc status", m)
	if !strings.Contains(stderr.String(), " supervisor stop'), then rerun with this gc") {
		t.Fatalf("forked supervisor must get the stop-it fix, got:\n%s", stderr.String())
	}
}

func TestVerifiedPSExecutablePath(t *testing.T) {
	bin := writeFakeGCBinary(t, "ps")
	if got, err := verifiedPSExecutablePath(1, bin); err != nil || got != bin {
		t.Fatalf("regular file: got (%q, %v), want (%q, nil)", got, err, bin)
	}
	for name, path := range map[string]string{
		"argv0 relative":  "gc",
		"truncated":       bin[:len(bin)-1],
		"stale (removed)": filepath.Join(t.TempDir(), "gone", "gc"),
		"directory":       filepath.Dir(bin),
	} {
		if got, err := verifiedPSExecutablePath(1, path); err == nil {
			t.Errorf("%s: accepted %q (got %q), want rejection", name, path, got)
		}
	}
}

// When macOS ps(1) gives no usable path and no service defines one, the
// supervisor's binary is unknown: that must never refuse, only warn (init)
// or defer to the drift auto-restart (start).
func TestUnknownSupervisorPathNeverRefuses(t *testing.T) {
	f := macOSHomebrewSupervisorFixture(t)
	f.serviceBinary, f.service = "", ""
	stubSupervisorBinary(t, f)
	supervisorRuntimeGOOS = "darwin"

	m, mismatched := detectSupervisorBinaryMismatch()
	if !mismatched || m.DifferentInstall {
		t.Fatalf("mismatched=%v DifferentInstall=%v, want a version mismatch that is not a known different install", mismatched, m.DifferentInstall)
	}
	var initErr bytes.Buffer
	if proceed, _ := checkSupervisorBinaryBeforeRegister("gc init", &initErr, true); !proceed {
		t.Fatalf("gc init refused with an unknown supervisor path:\n%s", initErr.String())
	}
	if !strings.Contains(initErr.String(), "at (unknown path)") {
		t.Fatalf("warning should say the path is unknown:\n%s", initErr.String())
	}
	var startErr bytes.Buffer
	if proceed, _ := checkSupervisorBinaryBeforeRegister("gc start", &startErr, false); !proceed || startErr.Len() != 0 {
		t.Fatalf("gc start proceed=%v stderr=%q, want a silent hand-off to the drift check", proceed, startErr.String())
	}
}

func TestDetectGCBinaryDrift(t *testing.T) {
	cases := []struct {
		name  string
		local gcBinaryIdentity
		sv    SupervisorStatus
		want  bool
	}{
		{"builds equal", gcBinaryIdentity{Version: "1.5.0", BuildID: "abc1234"}, SupervisorStatus{Version: "1.5.1", BuildID: "abc1234"}, false},
		{"builds differ", gcBinaryIdentity{Version: "1.5.0", BuildID: "abc1234"}, SupervisorStatus{Version: "1.5.0", BuildID: "def5678"}, true},
		{"homebrew upgrade: no commits, versions differ", gcBinaryIdentity{Version: "1.5.1", BuildID: "unknown"}, SupervisorStatus{Version: "1.5.0", BuildID: "unknown"}, true},
		{"homebrew same version", gcBinaryIdentity{Version: "v1.5.1", BuildID: "unknown"}, SupervisorStatus{Version: "1.5.1", BuildID: "unknown"}, false},
		{"old supervisor without build_id", gcBinaryIdentity{Version: "1.5.1", BuildID: "abc1234"}, SupervisorStatus{Version: "1.3.5"}, true},
		{"dev builds without commits", gcBinaryIdentity{Version: "dev", BuildID: "unknown"}, SupervisorStatus{Version: "dev", BuildID: "unknown"}, false},
	}
	for _, tc := range cases {
		if got := detectGCBinaryDrift(tc.local, tc.sv); got != tc.want {
			t.Errorf("%s: detectGCBinaryDrift = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An in-place `brew upgrade` (homebrew-core builds carry no commit) must
// auto-restart the supervisor like any other binary drift.
func TestDecideDriftActionRestartsHomebrewInPlaceUpgrade(t *testing.T) {
	res := decideDriftAction(
		gcBinaryIdentity{Version: "1.5.1", BuildID: "unknown"},
		SupervisorStatus{Version: "1.5.0", BuildID: "unknown"},
		nil, driftFlags{})
	if !res.Restart || !res.BinaryDrift {
		t.Fatalf("decideDriftAction = %+v, want a binary-drift restart", res)
	}
	var buf bytes.Buffer
	printDriftReport(&buf, driftReport{BinaryDrift: true, LocalBuildID: "unknown", SupervisorID: "unknown", LocalVersion: "1.5.1", SupervisorVersion: "1.5.0"})
	if !strings.Contains(buf.String(), "binary: local=version 1.5.1 supervisor=version 1.5.0") {
		t.Fatalf("drift report = %q, want version tokens", buf.String())
	}
}
