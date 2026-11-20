package main

import (
	"bytes"
	"errors"
	goruntime "runtime"
	"strings"
	"testing"
)

// On Windows gc invokes no systemctl, loginctl or launchctl. The supported
// lifecycle there is a foreground supervisor with explicit CLI start and stop;
// scheduled startup would be a later opt-in.

func TestSupervisorServiceManagerKindMatchesPlatform(t *testing.T) {
	kind := supervisorServiceManagerKind()
	want := map[string]string{"darwin": "launchd", "linux": "systemd"}[goruntime.GOOS]
	if kind != want {
		t.Fatalf("supervisorServiceManagerKind() = %q on %s, want %q", kind, goruntime.GOOS, want)
	}
	if got, expect := supervisorServiceManagerAvailable(), want != ""; got != expect {
		t.Fatalf("supervisorServiceManagerAvailable() = %v, want %v", got, expect)
	}
}

// TestServiceManagerUnsupportedErrorIsTypedAndActionable pins the shape a
// caller can branch on. A bare exit code cannot distinguish "this platform has
// no service manager" from "systemd refused", and the two want different
// responses.
func TestServiceManagerUnsupportedErrorIsTypedAndActionable(t *testing.T) {
	err := newSupervisorServiceManagerUnsupportedError("gc supervisor install")

	if !errors.Is(err, errSupervisorServiceManagerUnsupported) {
		t.Fatalf("errors.Is(%v, errSupervisorServiceManagerUnsupported) = false", err)
	}
	var typed *supervisorServiceManagerUnsupportedError
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As did not yield *supervisorServiceManagerUnsupportedError from %v", err)
	}
	if typed.GOOS != goruntime.GOOS {
		t.Fatalf("GOOS = %q, want %q", typed.GOOS, goruntime.GOOS)
	}

	msg := err.Error()
	for _, want := range []string{"gc supervisor install", goruntime.GOOS, "gc supervisor run", "gc supervisor start", "gc supervisor stop"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

// TestSupervisorInstallRefusesWithoutAServiceManager runs the real command
// path. It also pins that the refusal comes before anything is written: a
// half-installed unit on a platform that cannot load it is worse than a clean
// refusal.
func TestSupervisorInstallRefusesWithoutAServiceManager(t *testing.T) {
	if supervisorServiceManagerAvailable() {
		t.Skipf("%s has a %s service manager; this case covers platforms without one", goruntime.GOOS, supervisorServiceManagerKind())
	}

	var stdout, stderr bytes.Buffer
	if code := doSupervisorInstall(&stdout, &stderr); code == 0 {
		t.Fatalf("doSupervisorInstall succeeded on %s; stdout=%q stderr=%q", goruntime.GOOS, stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "no service manager gc installs into") {
		t.Fatalf("install stderr = %q, want the typed unsupported explanation", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doSupervisorUninstall(&stdout, &stderr); code == 0 {
		t.Fatalf("doSupervisorUninstall succeeded on %s; stdout=%q stderr=%q", goruntime.GOOS, stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "no service manager gc installs into") {
		t.Fatalf("uninstall stderr = %q, want the typed unsupported explanation", got)
	}
}

// TestEnsureSupervisorRunningAcceptsALiveSupervisorWithoutAServiceManager
// pins the contract of ensureSupervisorRunning: it must leave a supervisor
// running, not start one.
//
// Skipping the install step on a platform with no service manager is only
// correct if the alive check that step used to sit in front of survives with
// it. Without that, `gc register` against a supervisor that was already up
// reported "supervisor already running" as a failure and told the operator the
// supervisor had not started — while it was serving the very registration.
func TestEnsureSupervisorRunningAcceptsALiveSupervisorWithoutAServiceManager(t *testing.T) {
	if supervisorServiceManagerAvailable() {
		t.Skipf("%s installs into %s; this case covers platforms without a service manager", goruntime.GOOS, supervisorServiceManagerKind())
	}
	t.Setenv(supervisorSystemdUnitEnv, "")

	oldAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 4242 }
	t.Cleanup(func() { supervisorAliveHook = oldAlive })

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 0 {
		t.Fatalf("ensureSupervisorRunning = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := stderr.String(); strings.Contains(got, "already running") {
		t.Fatalf("stderr = %q, want no failure for a supervisor that is already serving", got)
	}
}

// TestInstallSupervisorServiceRefusesUnsupportedGOOSWithTypedMessage pins the
// install refusal text on every host: installSupervisorService takes the GOOS
// explicitly, so a platform without a service manager is exercised on Linux
// and macOS CI too.
func TestInstallSupervisorServiceRefusesUnsupportedGOOSWithTypedMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := installSupervisorService("freebsd", &supervisorServiceData{}, &stdout, &stderr); code == 0 {
		t.Fatalf("installSupervisorService(freebsd) = 0, want nonzero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	got := stderr.String()
	for _, want := range []string{"no service manager gc installs into", "freebsd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("install stderr = %q, want %q", got, want)
		}
	}
}
