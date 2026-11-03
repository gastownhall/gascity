package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// On Windows gc invokes no systemctl, loginctl or launchctl. The supported
// lifecycle there is a foreground supervisor with explicit CLI start and stop;
// scheduled startup would be a later opt-in.
//
// The risk is not that install does too much on Windows — it already refused —
// but that the refusal was indistinguishable from any other failure, and that
// the refusal could later be "fixed" by registering a Windows service.

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

// TestNoWindowsServiceRegistration is the guard the typed error exists to
// protect. Registering the supervisor as a Windows service would start it in a
// session with no desktop and no user profile, which is precisely what a
// supervisor that launches interactive agent sessions cannot work in — so the
// failure would appear as sessions that never come up rather than as an
// install error.
func TestNoWindowsServiceRegistration(t *testing.T) {
	root := repoRoot(t)
	banned := []string{
		"golang.org/x/sys/windows/svc",
		"OpenSCManager",
		"CreateServiceW",
		"svc.Run(",
		"mgr.Connect(",
	}

	self := filepath.Base(mustAbs(t, thisTestFile()))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || entry.Name() == self {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, needle := range banned {
			if bytes.Contains(data, []byte(needle)) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s references %q: gc must not register a Windows service", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

func thisTestFile() string { return "supervisor_service_manager_test.go" }

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

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q): %v", path, err)
	}
	return abs
}
