//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Guard against the suite leaking platform supervisor units into the
// operator's home. gc's platform install path (ensureSupervisorRunning →
// doSupervisorInstall in cmd/gc/cmd_supervisor_lifecycle.go) names the unit
// after GC_HOME, so every isolated env root has exactly one unit name it could
// leak, and that name hashes a path only this test owns. Checking that one
// name is precise: it never flags the operator's or a sibling run's units, and
// it needs no systemd, so it holds in CI containers and under Bazel too.

// passwdHome returns the passwd-db home of the current uid, which is where
// gc's platform install writes regardless of the HOME a test hands it.
func passwdHome() (string, bool) {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		return "", false
	}
	return lu.HomeDir, true
}

// platformSupervisorUnitPaths lists the files gc's platform install path
// writes under realHome for a supervisor whose GC_HOME is gcHome: the systemd
// user unit and its default.target.wants enable link on Linux, the
// LaunchAgent plist on macOS.
func platformSupervisorUnitPaths(realHome, gcHome string) []string {
	suffix := expectedSupervisorServiceSuffix(gcHome)
	if suffix == "" {
		return nil
	}
	switch runtime.GOOS {
	case "linux":
		unit := "gascity-supervisor-" + suffix + ".service"
		return []string{
			filepath.Join(realHome, ".local", "share", "systemd", "user", unit),
			filepath.Join(realHome, ".config", "systemd", "user", "default.target.wants", unit),
		}
	case "darwin":
		return []string{
			filepath.Join(realHome, "Library", "LaunchAgents", "com.gascity.supervisor."+suffix+".plist"),
		}
	default:
		return nil
	}
}

// leakedPlatformSupervisorUnits returns the platform unit files present under
// realHome for gcHome. Lstat, so a dangling enable link still counts.
func leakedPlatformSupervisorUnits(realHome, gcHome string) []string {
	var leaked []string
	for _, path := range platformSupervisorUnitPaths(realHome, gcHome) {
		if _, err := os.Lstat(path); err == nil {
			leaked = append(leaked, path)
		}
	}
	return leaked
}

// removeLeakedPlatformSupervisorUnit stops and removes the unit gc installed
// for gcHome. The name hashes gcHome, a directory this run created, so this
// only ever touches a unit this run leaked.
func removeLeakedPlatformSupervisorUnit(gcHome string, leaked []string) {
	suffix := expectedSupervisorServiceSuffix(gcHome)
	switch runtime.GOOS {
	case "linux":
		unit := "gascity-supervisor-" + suffix + ".service"
		_ = exec.Command("systemctl", "--user", "disable", "--now", unit).Run()
	case "darwin":
		target := fmt.Sprintf("gui/%d/com.gascity.supervisor.%s", os.Getuid(), suffix)
		_ = exec.Command("launchctl", "bootout", target).Run()
	}
	for _, path := range leaked {
		_ = os.Remove(path)
	}
	if runtime.GOOS == "linux" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
}

// platformUnitLeakReport checks gcHome for a leaked platform unit, removes
// it, and returns a description of the leak, or "" when there is none.
func platformUnitLeakReport(gcHome string) string {
	realHome, ok := passwdHome()
	if !ok {
		return ""
	}
	leaked := leakedPlatformSupervisorUnits(realHome, gcHome)
	if len(leaked) == 0 {
		return ""
	}
	removeLeakedPlatformSupervisorUnit(gcHome, leaked)
	return fmt.Sprintf("gc installed a platform supervisor unit for GC_HOME %s into the real home (%s); "+
		"the integration env must give gc an isolated HOME with %s=1 (see isolateGCHomeEnv). Removed it.",
		gcHome, strings.Join(leaked, ", "), supervisorIsolatedHomeEnv)
}

// registerPlatformUnitLeakGuard fails t if, by the time its cleanups run, gc
// installed a platform supervisor unit for gcHome. Register it before any
// supervisor is started so it runs after their stops (t.Cleanup is LIFO).
func registerPlatformUnitLeakGuard(t *testing.T, gcHome string) {
	t.Helper()
	// Resolve symlinks now, while gcHome surely exists: gc hashes the
	// resolved path into the unit name.
	normalized := gcHome
	if resolved, err := filepath.EvalSymlinks(gcHome); err == nil {
		normalized = resolved
	}
	t.Cleanup(func() {
		if report := platformUnitLeakReport(normalized); report != "" {
			t.Error(report)
		}
	})
}

func TestLeakedPlatformSupervisorUnitsFindsOnlyThisGCHome(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no platform supervisor unit on " + runtime.GOOS)
	}
	realHome := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	other := filepath.Join(t.TempDir(), "gc-home")
	for _, dir := range []string{gcHome, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := leakedPlatformSupervisorUnits(realHome, gcHome); len(got) != 0 {
		t.Fatalf("leaked units before install = %v, want none", got)
	}

	paths := platformSupervisorUnitPaths(realHome, other)
	if len(paths) == 0 {
		t.Fatal("no platform unit paths")
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(realHome, "deleted-target"), path); err != nil {
			t.Fatal(err)
		}
	}
	if got := leakedPlatformSupervisorUnits(realHome, gcHome); len(got) != 0 {
		t.Fatalf("leaked units for an untouched GC_HOME = %v, want none (another run's unit must not count)", got)
	}
	if got := leakedPlatformSupervisorUnits(realHome, other); len(got) != len(paths) {
		t.Fatalf("leaked units = %v, want %v (dangling enable links count)", got, paths)
	}
}

// Every env root the suite hands gc must opt into the isolated-HOME bare
// supervisor, or `gc init`/`gc start` installs a platform unit.
func TestIsolatedEnvRootsKeepGCOffTheRealHome(t *testing.T) {
	realHome, _ := passwdHome()
	drift := func(t *testing.T) (string, []string) {
		gcHome, _, env := newDriftIsolatedEnvRoot(t)
		return gcHome, env
	}
	isolated := func(t *testing.T) (string, []string) {
		gcHome, _, env := newIsolatedEnvRoot(t, false)
		return gcHome, env
	}
	shared := func(*testing.T) (string, []string) { return testGCHome, integrationEnv() }
	for name, build := range map[string]func(*testing.T) (string, []string){
		"newIsolatedEnvRoot":      isolated,
		"newDriftIsolatedEnvRoot": drift,
		"integrationEnv":          shared,
	} {
		t.Run(name, func(t *testing.T) {
			gcHome, env := build(t)
			got := parseEnvList(env)
			if got["GC_HOME"] != gcHome {
				t.Fatalf("GC_HOME = %q, want %q", got["GC_HOME"], gcHome)
			}
			if got["HOME"] != integrationIsolatedHome(gcHome) {
				t.Errorf("HOME = %q, want %q", got["HOME"], integrationIsolatedHome(gcHome))
			}
			if realHome != "" && got["HOME"] == realHome {
				t.Errorf("HOME is the passwd home %q", realHome)
			}
			if got[supervisorIsolatedHomeEnv] != "1" {
				t.Errorf("%s = %q, want 1", supervisorIsolatedHomeEnv, got[supervisorIsolatedHomeEnv])
			}
		})
	}
}
