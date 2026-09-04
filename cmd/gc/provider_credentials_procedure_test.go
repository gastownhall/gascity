package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The apply procedure `gc provider credentials` prints has been wrong three
// times, each time because someone read `ensureSupervisorRunning`, saw it
// regenerate the service file, and concluded that `gc supervisor start`
// applies a credential. It does not, and the sequence that follows from that
// reading produces the blank-credential failure the command warns about.
//
// A guard that greps the printed paragraph cannot catch the fourth reading:
// it passes whenever the words are right, including when the code beneath
// them has changed. So these tests drive the real command functions against
// fakes and assert the behavior the paragraph claims — which supervisor is
// launched, and with which environment.

const procedureTestCredential = "sk-ant-only-in-the-secrets-file"

// newSupervisorApplyTestEnv isolates GC_HOME, writes a secrets file holding a
// credential that exists NOWHERE in the process environment, and stubs the
// launchctl calls. The value's absence from os.Environ() is what makes the
// install/start contrast meaningful rather than incidental, so it is asserted
// rather than assumed.
func newSupervisorApplyTestEnv(t *testing.T) (launchctlCalls *[]string) {
	t.Helper()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_SUPERVISOR_SYSTEMD_UNIT", "")
	t.Setenv("GC_SUPERVISOR_ENV", "")
	t.Setenv("GC_SUPERVISOR_OMIT_PROVIDER_CREDS", "")

	secrets := filepath.Join(gcHome, supervisorSecretsEnvFileName)
	if err := os.WriteFile(secrets, []byte("ANTHROPIC_API_KEY="+procedureTestCredential+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", secrets, err)
	}
	for _, kv := range os.Environ() {
		if strings.Contains(kv, procedureTestCredential) {
			t.Fatalf("the test credential is already in the process environment (%q); the install/start contrast would be vacuous", kv)
		}
	}

	var calls []string
	oldRun := supervisorLaunchctlRun
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })
	return &calls
}

// TestSupervisorInstallAppliesTheSecretsFileCredential is the positive half of
// the printed procedure: `gc supervisor install` is what carries a new
// credential to the supervisor. It renders the merged environment into the
// service file and hands that file to the service manager, so the supervisor
// the manager (re-)launches runs with the new value.
func TestSupervisorInstallAppliesTheSecretsFileCredential(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	launchctlCalls := newSupervisorApplyTestEnv(t)
	setSupervisorInstallForceForTest(t, false)

	oldAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 0 }
	t.Cleanup(func() { supervisorAliveHook = oldAlive })

	// supervisorServiceExtraEnv is the merge the service file is rendered
	// from. Assert the credential is in it before asserting it reaches the
	// plist, so a plist that happened to omit it reports the right cause.
	var merged string
	for _, e := range supervisorServiceExtraEnv() {
		if e.Name == "ANTHROPIC_API_KEY" {
			merged = e.Value
		}
	}
	if merged != procedureTestCredential {
		t.Fatalf("supervisorServiceExtraEnv ANTHROPIC_API_KEY = %q; want the secrets-file value %q — install cannot apply what it never merges", merged, procedureTestCredential)
	}

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorLaunchd = %d, want 0; stderr=%q", code, stderr.String())
	}

	plistPath := supervisorLaunchdPlistPath()
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading %s: %v", plistPath, err)
	}
	if !strings.Contains(string(plist), procedureTestCredential) {
		t.Fatalf("installed service file does not carry the credential from the secrets file:\n%s", plist)
	}
	if !strings.Contains(string(plist), "ANTHROPIC_API_KEY") {
		t.Fatalf("installed service file does not name ANTHROPIC_API_KEY:\n%s", plist)
	}

	// The service manager, not gc, launches the supervisor from that file —
	// which is why install re-execs it and start does not.
	joined := strings.Join(*launchctlCalls, "\n")
	if !strings.Contains(joined, "load "+plistPath) {
		t.Errorf("install did not hand the regenerated service file to launchd; calls = %v", *launchctlCalls)
	}
	if !strings.Contains(joined, "kickstart -p "+supervisorLaunchdServiceTarget(data.LaunchdLabel)) {
		t.Errorf("install did not (re-)launch the service, so a running supervisor would keep its old environment; calls = %v", *launchctlCalls)
	}
}

// TestSupervisorStartDoesNotApplyTheSecretsFileCredential is the negative half,
// and the one the procedure gets wrong when it is wrong: `gc supervisor start`
// spawns the supervisor itself, with the CALLING SHELL's environment. It never
// reads the secrets file, so after a stop+start the fleet serves whatever the
// shell had — the old value, or nothing.
func TestSupervisorStartDoesNotApplyTheSecretsFileCredential(t *testing.T) {
	newSupervisorApplyTestEnv(t)

	oldAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 4242 }
	t.Cleanup(func() { supervisorAliveHook = oldAlive })

	var launched *exec.Cmd
	oldStart := startSupervisorChild
	startSupervisorChild = func(cmd *exec.Cmd) error {
		launched = cmd
		return nil
	}
	t.Cleanup(func() { startSupervisorChild = oldStart })

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStartJSON(&stdout, &stderr, false); code != 0 {
		t.Fatalf("doSupervisorStartJSON = %d, want 0; stderr=%q", code, stderr.String())
	}
	if launched == nil {
		t.Fatal("gc supervisor start launched no supervisor; the rest of this guard assumes it does")
	}
	if got := strings.Join(launched.Args[1:], " "); got != "supervisor run" {
		t.Fatalf("gc supervisor start launched %q; want `supervisor run`", got)
	}

	// Non-vacuity: install's merge does carry the credential, so its absence
	// below is the difference between the two paths and not a broken fixture.
	merged := false
	for _, e := range supervisorServiceExtraEnv() {
		if e.Name == "ANTHROPIC_API_KEY" && e.Value == procedureTestCredential {
			merged = true
		}
	}
	if !merged {
		t.Fatal("supervisorServiceExtraEnv does not carry the secrets-file credential, so this test cannot show that start skips it")
	}

	for _, kv := range launched.Env {
		if strings.Contains(kv, procedureTestCredential) {
			t.Fatalf("gc supervisor start handed the supervisor the secrets-file credential (%q). If start now merges the service environment it may have become a valid apply step, and the procedure printed by `gc provider credentials` — and docs/getting-started/troubleshooting.md — must be re-derived rather than left describing install as the only one.", kv)
		}
	}

	// What it carries instead is this process's own environment. gc appends
	// its product-metrics opt-out for the child, so the check is containment,
	// not equality.
	got := make(map[string]bool, len(launched.Env))
	for _, kv := range launched.Env {
		got[kv] = true
	}
	for _, kv := range os.Environ() {
		if !got[kv] {
			t.Fatalf("gc supervisor start dropped %q from the calling shell's environment; the procedure states it carries that environment, which is why it cannot apply a credential the shell does not hold", kv)
		}
	}
}

// TestSupervisorStartRefusesWhileASupervisorIsAlive pins why the wrong
// procedure reaches for a stop first: start will not replace a live
// supervisor. An operator following "stop then start" therefore lands in the
// case above with no service file regenerated at all.
func TestSupervisorStartRefusesWhileASupervisorIsAlive(t *testing.T) {
	newSupervisorApplyTestEnv(t)

	// The liveness pre-check calls supervisorAlive directly, so simulate a
	// live supervisor with the lock it takes rather than with the hook.
	lock, err := acquireSupervisorLock()
	if err != nil {
		t.Fatalf("acquiring supervisor lock: %v", err)
	}
	defer lock.Close() //nolint:errcheck // test cleanup

	startSupervisorChildCalled := false
	oldStart := startSupervisorChild
	startSupervisorChild = func(*exec.Cmd) error {
		startSupervisorChildCalled = true
		return nil
	}
	t.Cleanup(func() { startSupervisorChild = oldStart })

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStartJSON(&stdout, &stderr, false); code == 0 {
		t.Fatalf("doSupervisorStartJSON = 0 while a supervisor holds the lock; want a refusal. stdout=%q", stdout.String())
	}
	if startSupervisorChildCalled {
		t.Error("gc supervisor start spawned a second supervisor over a live one")
	}
}

// TestSupervisorInstallIsSilentlyANoOpWhenNothingChanged pins the one caveat
// the printed procedure asks the operator to watch for: when the rendered
// service file is byte-identical to the installed one and a supervisor is
// already alive, install prints its success line and exits 0 WITHOUT
// re-execing anything. Nothing was applied.
//
// The other refusal — an installed service file naming a different gc binary —
// is not silent: it exits 1 and says to pass --force
// (TestInstallSupervisorLaunchdBinaryMismatchGuard).
func TestSupervisorInstallIsSilentlyANoOpWhenNothingChanged(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	launchctlCalls := newSupervisorApplyTestEnv(t)
	setSupervisorInstallForceForTest(t, false)

	alive := 0
	oldAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return alive }
	t.Cleanup(func() { supervisorAliveHook = oldAlive })

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("first install = %d, want 0; stderr=%q", code, stderr.String())
	}
	if len(*launchctlCalls) == 0 {
		t.Fatal("first install made no launchctl calls; the second-install contrast would be vacuous")
	}

	*launchctlCalls = nil
	alive = 4242
	stdout.Reset()
	stderr.Reset()
	if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("second install = %d, want 0; stderr=%q", code, stderr.String())
	}
	if len(*launchctlCalls) != 0 {
		t.Fatalf("second install re-execed the supervisor (%v); the printed caveat says it does not, and would have to be re-derived", *launchctlCalls)
	}
	if !strings.Contains(stdout.String(), "Installed launchd service") {
		t.Errorf("second install did not print its success line (%q); the caveat is that it reports success while applying nothing", stdout.String())
	}
}

// TestCredentialApplyProcedureNamesInstallNotStart is the cheap companion to
// the behavioral guards above: they pin what the code does, this pins that
// the printed text says it. It asserts on the command-listing lines only, so
// the paragraph warning AGAINST stop+start is still allowed to name them.
func TestCredentialApplyProcedureNamesInstallNotStart(t *testing.T) {
	procedure := credentialApplyProcedure + managedSupervisorNote

	if !strings.Contains(procedure, "gc supervisor install") {
		t.Error("the procedure does not name `gc supervisor install`, which is the only step that re-execs the supervisor with the regenerated service file")
	}
	if !strings.Contains(procedure, "gc restart") {
		t.Error("the procedure does not name `gc restart`, which cycles the agents onto the new value")
	}

	for _, line := range strings.Split(procedure, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "gc ") {
			continue
		}
		if strings.HasPrefix(trimmed, "gc supervisor stop") || strings.HasPrefix(trimmed, "gc supervisor start") {
			t.Errorf("the procedure lists %q as a step. `gc supervisor start` neither regenerates the service file nor merges the secrets file: it launches a supervisor with the calling shell's environment (TestSupervisorStartDoesNotApplyTheSecretsFileCredential) and refuses outright when one is already running (TestSupervisorStartRefusesWhileASupervisorIsAlive). Following it after a stop serves the shell's old or blank value — the exact failure this command exists to warn about.", trimmed)
		}
	}
}

// TestSupervisorForwardsSecretsFileKeyOptedInViaGCSupervisorEnv closes the loop
// on the remedy `gc provider credentials` prints. When a credential's source
// variable is a name the supervisor's persist allow-list does not recognize,
// the report tells the operator to opt it in with GC_SUPERVISOR_ENV. That is
// only true if the secrets-file tier honors the opt-in, and it is gated on
// exactly one predicate — supervisorForwardsEnvKey — which the report reads
// too. If the gate ever narrows to the allow-list alone, the report's advice
// becomes a dead end and this fails.
func TestSupervisorForwardsSecretsFileKeyOptedInViaGCSupervisorEnv(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, ".gc")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_SUPERVISOR_SYSTEMD_UNIT", "")
	t.Setenv("GC_SUPERVISOR_OMIT_PROVIDER_CREDS", "")
	if err := os.MkdirAll(gcHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// ACME_KEY matches no credential prefix, so only the opt-in can carry it.
	// UNRELATED_SECRET is the control: without an opt-in the tier must still
	// refuse, or the gate has stopped bounding the service env.
	secrets := filepath.Join(gcHome, supervisorSecretsEnvFileName)
	if err := os.WriteFile(secrets, []byte("ACME_KEY=from-the-file\nUNRELATED_SECRET=do-not-persist\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACME_KEY", "")
	t.Setenv("UNRELATED_SECRET", "")
	t.Setenv("GC_SUPERVISOR_ENV", "ACME_KEY")

	got := supervisorServiceEnvMap(supervisorServiceExtraEnv())
	if got["ACME_KEY"] != "from-the-file" {
		t.Errorf("ACME_KEY = %q, want %q — `gc provider credentials` tells operators GC_SUPERVISOR_ENV opts a secrets-file key in", got["ACME_KEY"], "from-the-file")
	}
	if _, ok := got["UNRELATED_SECRET"]; ok {
		t.Errorf("UNRELATED_SECRET reached the service env without an opt-in: %#v", got)
	}
}
