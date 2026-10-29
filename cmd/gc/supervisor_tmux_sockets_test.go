package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// tmuxTmpdirFixtures returns a TMUX_TMPDIR directory, a symlink to it, and a
// missing directory, for the socket-directory comparisons.
func tmuxTmpdirFixtures(t *testing.T) (dir, link, missing string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	dir = filepath.Join(root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	return dir, link, filepath.Join(root, "missing")
}

// answerTmuxSocket binds a listener at the -L socket name in the socket
// directory of TMUX_TMPDIR tmuxTmpdir, as a tmux server there would, and
// accepts connections until cleanup.
func answerTmuxSocket(t *testing.T, tmuxTmpdir, name string) {
	t.Helper()
	dir := tmux.SocketDir(tmuxTmpdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

// tmuxServiceData is service data whose TMUX_TMPDIR is tmuxTmpdir.
func tmuxServiceData(gcHome, tmuxTmpdir string) *supervisorServiceData {
	return &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "/tmp/gc-run",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv:      []supervisorServiceEnvVar{{Name: tmux.TmpdirEnv, Value: tmuxTmpdir}},
	}
}

// stubRunningSupervisor makes the supervisor that answers its socket pid
// 4242 with TMUX_TMPDIR tmuxTmpdir, or makes none answer when running is
// false; readErr fails the read of its TMUX_TMPDIR.
func stubRunningSupervisor(t *testing.T, running bool, tmuxTmpdir string, readErr error) {
	t.Helper()
	oldAlive, oldEnvValue := supervisorAliveHook, supervisorProcessEnvValue
	t.Cleanup(func() { supervisorAliveHook, supervisorProcessEnvValue = oldAlive, oldEnvValue })
	supervisorAliveHook = func() int {
		if running {
			return 4242
		}
		return 0
	}
	supervisorProcessEnvValue = func(pid int, key string) (string, error) {
		if pid != 4242 || key != tmux.TmpdirEnv {
			t.Errorf("read pid %d %s, want the active supervisor's %s", pid, key, tmux.TmpdirEnv)
		}
		return tmuxTmpdir, readErr
	}
}

// stubServiceManagers stubs the service managers' own TMUX_TMPDIR: systemd's
// environment dump (dump, dumpErr) and launchd's getenv (launchd).
func stubServiceManagers(t *testing.T, dump string, dumpErr error, launchd string) {
	t.Helper()
	oldSystemd, oldLaunchd := supervisorSystemdManagerEnvironment, supervisorLaunchctlGetenv
	t.Cleanup(func() { supervisorSystemdManagerEnvironment, supervisorLaunchctlGetenv = oldSystemd, oldLaunchd })
	supervisorSystemdManagerEnvironment = func() (string, error) { return dump, dumpErr }
	supervisorLaunchctlGetenv = func(key string) string {
		if key == tmux.TmpdirEnv {
			return launchd
		}
		return ""
	}
}

// A service file records TMUX_TMPDIR even when the installing shell has none:
// empty, which tmux reads as unset. A start of the service then takes nothing
// from the service manager's environment in its place.
func TestBuildSupervisorServiceDataRecordsAnUnsetTMUXTMPDIR(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("GC_SUPERVISOR_ENV", "")
	t.Setenv(tmux.TmpdirEnv, "")
	stubServiceManagers(t, "", nil, "")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if v, ok := supervisorServiceEnvMap(data.ExtraEnv)[tmux.TmpdirEnv]; !ok || v != "" {
		t.Fatalf("ExtraEnv[%s] = %q (present %v), want recorded empty", tmux.TmpdirEnv, v, ok)
	}
	if !slices.IsSortedFunc(data.ExtraEnv, func(a, b supervisorServiceEnvVar) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("ExtraEnv = %v, want sorted by name", data.ExtraEnv)
	}
	unit, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatalf("render unit: %v", err)
	}
	if v, ok := systemdUnitEnvValue([]byte(unit), tmux.TmpdirEnv); !ok || v != "" || !strings.Contains(unit, `Environment=TMUX_TMPDIR=""`) {
		t.Fatalf("unit records %s %q (present %v), want an explicit empty value:\n%s", tmux.TmpdirEnv, v, ok, unit)
	}
	plist, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatalf("render plist: %v", err)
	}
	if v, ok := launchdPlistEnvValue([]byte(plist), tmux.TmpdirEnv); !ok || v != "" {
		t.Fatalf("plist records %s %q (present %v), want an explicit empty value:\n%s", tmux.TmpdirEnv, v, ok, plist)
	}
}

// systemdInstall runs installSupervisorService for linux over a unit that
// holds installed ("" for none), with data's TMUX_TMPDIR next. When the
// supervisor runs, the unit is active until a warm refresh signals it and
// the supervisor is preserve-ready. It returns the exit code, stderr, the
// systemctl calls, and whether the unit is as it was.
func systemdInstall(t *testing.T, installed, next string) (int, string, []string, bool) {
	t.Helper()
	return systemdInstallReaching(t, installed, next, true)
}

// systemdInstallReaching is systemdInstall where systemctl --user reaches
// the user manager only when reachable.
func systemdInstallReaching(t *testing.T, installed, next string, reachable bool) (int, string, []string, bool) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	if installed != "" {
		writeSystemdUnit(t, installed)
	}
	path := supervisorSystemdServicePath()
	oldRun, oldActive := supervisorSystemctlRun, supervisorSystemctlActive
	t.Cleanup(func() { supervisorSystemctlRun, supervisorSystemctlActive = oldRun, oldActive })
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == "gascity-supervisor.service" && supervisorAliveHook() != 0 &&
			!slices.Contains(calls, "--user kill --kill-who=main --signal=SIGTERM "+service)
	}
	stubSupervisorSystemctlUserAvailable(t, reachable)
	stubSupervisorRunningPreserveSignalReady(t, true)
	oldLinger := supervisorLingerEnabled
	t.Cleanup(func() { supervisorLingerEnabled = oldLinger })
	supervisorLingerEnabled = func(string) bool { return true }

	var stdout, stderr bytes.Buffer
	code := installSupervisorService("linux", tmuxServiceData(os.Getenv("GC_HOME"), next), &stdout, &stderr)
	got, err := os.ReadFile(path)
	unchanged := installed == "" && errors.Is(err, os.ErrNotExist) || err == nil && string(got) == installed
	return code, stderr.String(), calls, unchanged
}

// writeSystemdUnit installs content as the supervisor's systemd unit.
func writeSystemdUnit(t *testing.T, content string) {
	t.Helper()
	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubLiveTmuxSockets makes tmux servers answer on names in the socket
// directory dir, and none answer anywhere else.
func stubLiveTmuxSockets(t *testing.T, dir string, names ...string) {
	t.Helper()
	old := supervisorLiveTmuxSockets
	t.Cleanup(func() { supervisorLiveTmuxSockets = old })
	supervisorLiveTmuxSockets = func(_ context.Context, probed string) ([]string, error) {
		if probed == dir {
			return names, nil
		}
		return nil, nil
	}
}

// errUnreachableUserManager is how systemctl --user fails where it cannot
// reach the user manager (cron, sudo -u, su: no XDG_RUNTIME_DIR to find it by).
var errUnreachableUserManager = errors.New("exit status 1: Failed to connect to bus: No medium found")

// tmuxHome points HOME and GC_HOME at a fresh directory.
func tmuxHome(t *testing.T) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
}

// A warm refresh replaces the active supervisor while its sessions survive on
// the tmux servers under its TMUX_TMPDIR, so a refreshed unit whose
// TMUX_TMPDIR resolves to another socket directory is refused before the
// unit or systemd is touched. The running supervisor's own environment is
// what counts, wherever it got its value: an upgrade from a shell exporting
// TMUX_TMPDIR, a refresh from a shell without the user manager's, a refresh
// with another value.
func TestInstallSupervisorServiceRefusesWarmRefreshMovingTmuxSockets(t *testing.T) {
	dir, _, _ := tmuxTmpdirFixtures(t)
	other, _, _ := tmuxTmpdirFixtures(t)
	for _, tc := range []struct {
		name          string
		running, next string
	}{
		{name: "upgrade from a shell exporting TMUX_TMPDIR", next: dir},
		{name: "running supervisor took the user manager's TMUX_TMPDIR, shell has none", running: dir},
		{name: "another TMUX_TMPDIR", running: dir, next: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxHome(t)
			stubRunningSupervisor(t, true, tc.running, nil)
			code, stderr, calls, unchanged := systemdInstall(t, "old unit\n", tc.next)
			if code != 1 || len(calls) != 0 || !unchanged {
				t.Fatalf("code %d, systemctl calls %v, unit unchanged %v; want a refusal that touches nothing; stderr=%q", code, calls, unchanged, stderr)
			}
			for _, want := range []string{tmux.SocketDir(tc.running), tmux.SocketDir(tc.next), "pid 4242", "gc supervisor stop --wait"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q, want %q", stderr, want)
				}
			}
		})
	}
}

// A refreshed unit whose TMUX_TMPDIR resolves where the active supervisor's
// does keeps its sessions reachable, so the warm refresh proceeds: neither
// sets it, the same value, a symlink to it, /tmp named outright, and a value
// tmux replaces by /tmp because it does not resolve.
func TestInstallSupervisorServiceWarmRefreshKeepingTmuxSockets(t *testing.T) {
	dir, link, missing := tmuxTmpdirFixtures(t)
	for _, tc := range []struct {
		name          string
		running, next string
	}{
		{name: "neither sets it"},
		{name: "same value", running: dir, next: dir},
		{name: "symlink to the same directory", running: dir, next: link},
		{name: "tmp named outright", next: "/tmp"},
		{name: "missing directory falls back to tmp", running: missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxHome(t)
			stubRunningSupervisor(t, true, tc.running, nil)
			code, stderr, calls, _ := systemdInstall(t, "old unit\n", tc.next)
			if code != 0 || !slices.Contains(calls, "--user start gascity-supervisor.service") {
				t.Fatalf("code %d, systemctl calls %v; want the warm refresh to start the refreshed unit; stderr=%q", code, calls, stderr)
			}
		})
	}
}

// A refresh that cannot read the active supervisor's TMUX_TMPDIR cannot tell
// whether it moves the sockets, so it is refused.
func TestInstallSupervisorServiceRefusesWhenTheActiveTmuxTmpdirIsUnreadable(t *testing.T) {
	tmuxHome(t)
	stubRunningSupervisor(t, true, "", errors.New("permission denied"))
	code, stderr, calls, unchanged := systemdInstall(t, "old unit\n", "")
	if code != 1 || len(calls) != 0 || !unchanged || !strings.Contains(stderr, "cannot read active supervisor pid 4242 TMUX_TMPDIR: permission denied") {
		t.Fatalf("code %d, systemctl calls %v, unit unchanged %v, stderr %q; want a refusal naming the failed read", code, calls, unchanged, stderr)
	}
}

// An unchanged unit restarts nothing, so it moves nothing and is never
// refused, whatever the running supervisor has.
func TestInstallSupervisorServiceLetsAnUnchangedUnitThrough(t *testing.T) {
	tmuxHome(t)
	dir, _, _ := tmuxTmpdirFixtures(t)
	stubRunningSupervisor(t, true, dir, nil)
	unit, err := renderSupervisorTemplate(supervisorSystemdTemplate, tmuxServiceData(os.Getenv("GC_HOME"), ""))
	if err != nil {
		t.Fatal(err)
	}
	if code, stderr, calls, _ := systemdInstall(t, unit, ""); code != 0 || slices.Contains(calls, "--user start gascity-supervisor.service") {
		t.Fatalf("code %d, systemctl calls %v, stderr %q; want an install that restarts nothing", code, calls, stderr)
	}
}

// With no supervisor running, its sessions stay on the tmux servers in the
// directory the installed unit keeps its sockets in (systemctl --user stop
// signals only the main pid). Starting the unit rewritten for another
// directory strands them while one of those servers still answers, so the
// install is refused; with none answering, it starts. A unit an earlier gc
// wrote records no TMUX_TMPDIR and gives its supervisor the user manager's.
func TestInstallSupervisorServiceIdleUnitStrandsAnsweringTmuxServers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool // the installed unit records its TMUX_TMPDIR
		answers  bool // a tmux server answers in its socket directory
		refuse   bool
	}{
		{name: "unit records the directory, a server answers there", recorded: true, answers: true, refuse: true},
		{name: "unit an earlier gc wrote, the user manager's directory answers", answers: true, refuse: true},
		{name: "no server answers in the unit's directory", recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxHome(t)
			old, next := shortTmuxTmpDir(t), shortTmuxTmpDir(t)
			stubRunningSupervisor(t, false, "", nil)
			stubServiceManagers(t, "PATH=/usr/bin\n"+tmux.TmpdirEnv+"="+old+"\n", nil, "")
			if tc.answers {
				answerTmuxSocket(t, old, "gc-city")
			}
			installed := "old unit\n"
			if tc.recorded {
				unit, err := renderSupervisorTemplate(supervisorSystemdTemplate, tmuxServiceData(os.Getenv("GC_HOME"), old))
				if err != nil {
					t.Fatal(err)
				}
				installed = unit
			}
			code, stderr, calls, unchanged := systemdInstall(t, installed, next)
			if !tc.refuse {
				if code != 0 || !slices.Contains(calls, "--user start gascity-supervisor.service") {
					t.Fatalf("code %d, systemctl calls %v; want the unit started; stderr=%q", code, calls, stderr)
				}
				return
			}
			if code != 1 || len(calls) != 0 || !unchanged {
				t.Fatalf("code %d, systemctl calls %v, unit unchanged %v; want a refusal that touches nothing; stderr=%q", code, calls, unchanged, stderr)
			}
			for _, want := range []string{tmux.SocketDir(old), "gc-city", tmux.SocketDir(next), "installed service"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr = %q, want %q", stderr, want)
				}
			}
		})
	}
}

// A unit an earlier gc wrote takes its TMUX_TMPDIR from the user manager. A
// manager that answers with a value the guard cannot read leaves the
// directory unknown, so a start that might strand the sessions there is
// refused.
func TestInstallSupervisorServiceRefusesWhenTheIdleUnitsDirectoryIsUnknown(t *testing.T) {
	tmuxHome(t)
	stubRunningSupervisor(t, false, "", nil)
	stubServiceManagers(t, tmux.TmpdirEnv+"=$'/run/tmux dir'\n", nil, "")
	code, stderr, calls, unchanged := systemdInstall(t, "old unit\n", shortTmuxTmpDir(t))
	if code != 1 || len(calls) != 0 || !unchanged || !strings.Contains(stderr, "shell-escaped") || !strings.Contains(stderr, "Refusing to start the supervisor") {
		t.Fatalf("code %d, systemctl calls %v, unit unchanged %v, stderr %q; want a refusal naming the unreadable value", code, calls, unchanged, stderr)
	}
}

// An install that cannot reach the user manager starts nothing, so it moves
// nothing: the socket guard leaves it to installSupervisorSystemd, which
// reports the unreachable manager and the detached fallback, as it did
// before the guard, even when a server answers where the manager's default
// keeps sockets and this shell would keep them elsewhere.
func TestInstallSupervisorServiceLeavesAnUnreachableUserManagerToTheInstall(t *testing.T) {
	tmuxHome(t)
	stubRunningSupervisor(t, false, "", nil)
	stubServiceManagers(t, "", errUnreachableUserManager, "")
	stubLiveTmuxSockets(t, tmux.SocketDir(""), "gc-city")
	code, stderr, calls, unchanged := systemdInstallReaching(t, "old unit\n", shortTmuxTmpDir(t), false)
	if code != 1 || !unchanged || slices.Contains(calls, "--user start gascity-supervisor.service") {
		t.Fatalf("code %d, systemctl calls %v, unit unchanged %v, stderr %q; want an install that writes and starts nothing", code, calls, unchanged, stderr)
	}
	if !strings.Contains(stderr, "per-user systemd instance is not available") || strings.Contains(stderr, "Refusing to start the supervisor") {
		t.Fatalf("stderr = %q, want installSupervisorSystemd's unreachable-manager message, not a socket-directory refusal", stderr)
	}
}

// launchdInstall runs installSupervisorService for darwin over a plist that
// holds installed, with data's TMUX_TMPDIR next. It returns the exit code,
// stderr, the launchctl calls, and whether the plist is as it was.
func launchdInstall(t *testing.T, installed, next string) (int, string, []string, bool) {
	t.Helper()
	path := supervisorLaunchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRun := supervisorLaunchctlRun
	t.Cleanup(func() { supervisorLaunchctlRun = oldRun })
	var calls []string
	supervisorLaunchctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := installSupervisorService("darwin", tmuxServiceData(os.Getenv("GC_HOME"), next), &stdout, &stderr)
	got, err := os.ReadFile(path)
	return code, stderr.String(), calls, err == nil && string(got) == installed
}

// launchdHome points HOME at a fresh directory and GC_HOME at an isolated one
// beside it, as the launchd install tests run.
func launchdHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "isolated-home"))
}

// A changed plist reloads the active supervisor, so one whose TMUX_TMPDIR
// moves the sockets is refused before the plist is written; one that keeps
// them reloads. With none running, a plist rewritten away from a socket
// directory where a tmux server still answers is refused too.
func TestInstallSupervisorServiceLaunchdKeepsTmuxSockets(t *testing.T) {
	dir, link, _ := tmuxTmpdirFixtures(t)
	t.Run("reload moving the sockets", func(t *testing.T) {
		launchdHome(t)
		stubRunningSupervisor(t, true, "", nil)
		code, stderr, calls, unchanged := launchdInstall(t, "old plist\n", dir)
		if code != 1 || len(calls) != 0 || !unchanged || !strings.Contains(stderr, tmux.SocketDir(dir)) {
			t.Fatalf("code %d, launchctl calls %v, plist unchanged %v, stderr %q; want a refusal that touches nothing", code, calls, unchanged, stderr)
		}
	})
	t.Run("reload keeping the sockets", func(t *testing.T) {
		launchdHome(t)
		stubRunningSupervisor(t, true, dir, nil)
		code, stderr, calls, _ := launchdInstall(t, "old plist\n", link)
		if code != 0 || !slices.Contains(calls, "load "+supervisorLaunchdPlistPath()) {
			t.Fatalf("code %d, launchctl calls %v, stderr %q; want the changed plist loaded", code, calls, stderr)
		}
	})
	t.Run("idle plist away from an answering server", func(t *testing.T) {
		launchdHome(t)
		old, next := shortTmuxTmpDir(t), shortTmuxTmpDir(t)
		stubRunningSupervisor(t, false, "", nil)
		stubServiceManagers(t, "", nil, old) // a plist an earlier gc wrote takes launchd's
		answerTmuxSocket(t, old, "gc-city")
		code, stderr, calls, unchanged := launchdInstall(t, "old plist\n", next)
		if code != 1 || len(calls) != 0 || !unchanged || !strings.Contains(stderr, "gc-city") {
			t.Fatalf("code %d, launchctl calls %v, plist unchanged %v, stderr %q; want a refusal naming the answering server", code, calls, unchanged, stderr)
		}
	})
}

// A supervisor forked outside its service inherits this process's
// TMUX_TMPDIR, so it is refused while the installed service keeps its
// sockets elsewhere and a tmux server there still answers; with the same
// directory, nothing answering there, or no service installed, it forks.
func TestRefuseForkedSupervisorTmuxSocketDirMove(t *testing.T) {
	for _, tc := range []struct {
		name               string
		installed, answers bool
		same               bool // this process has the installed service's TMUX_TMPDIR
		refuse             bool
	}{
		{name: "elsewhere, answering", installed: true, answers: true, refuse: true},
		{name: "same directory", installed: true, answers: true, same: true},
		{name: "elsewhere, nothing answering", installed: true},
		{name: "no service installed", answers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxHome(t)
			old, next := shortTmuxTmpDir(t), shortTmuxTmpDir(t)
			if tc.same {
				next = old
			}
			t.Setenv(tmux.TmpdirEnv, next)
			if tc.answers {
				answerTmuxSocket(t, old, "gc-city")
			}
			if tc.installed {
				unit, err := renderSupervisorTemplate(supervisorSystemdTemplate, tmuxServiceData(os.Getenv("GC_HOME"), old))
				if err != nil {
					t.Fatal(err)
				}
				writeSystemdUnit(t, unit)
			}
			var stderr bytes.Buffer
			if got := refuseForkedSupervisorTmuxSocketDirMove("linux", &stderr); got != tc.refuse {
				t.Fatalf("refused = %v, want %v; stderr=%q", got, tc.refuse, stderr.String())
			}
			if tc.refuse && !strings.Contains(stderr.String(), "gc supervisor start: tmux servers still answer in "+tmux.SocketDir(old)) {
				t.Fatalf("stderr = %q, want the answering directory named", stderr.String())
			}
		})
	}
}

// A unit an earlier gc wrote records no TMUX_TMPDIR and gives its supervisor
// the user manager's. A fork does not go through the manager, so where
// systemctl --user cannot reach it the fork is not refused for that: the
// manager's default, which holds none, stands in for its value. A fork that
// keeps the default directory starts; one elsewhere is refused while a
// server answers in the default directory, and the refusal names the
// assumption, systemctl's error, and how to reach the manager.
func TestRefuseForkedSupervisorTmuxSocketDirMoveWithAnUnreachableUserManager(t *testing.T) {
	for _, tc := range []struct {
		name      string
		elsewhere bool // this process sets a TMUX_TMPDIR outside the default directory
		answers   bool // a tmux server answers in the default directory
		refuse    bool
	}{
		{name: "fork keeps the default directory", answers: true},
		{name: "fork elsewhere, a server answers in the default directory", elsewhere: true, answers: true, refuse: true},
		{name: "fork elsewhere, nothing answers in the default directory", elsewhere: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmuxHome(t)
			next := ""
			if tc.elsewhere {
				next = shortTmuxTmpDir(t)
			}
			t.Setenv(tmux.TmpdirEnv, next)
			stubServiceManagers(t, "", errUnreachableUserManager, "")
			var answering []string
			if tc.answers {
				answering = []string{"gc-city"}
			}
			stubLiveTmuxSockets(t, tmux.SocketDir(""), answering...)
			writeSystemdUnit(t, "old unit\n")
			var stderr bytes.Buffer
			if got := refuseForkedSupervisorTmuxSocketDirMove("linux", &stderr); got != tc.refuse {
				t.Fatalf("refused = %v, want %v; stderr=%q", got, tc.refuse, stderr.String())
			}
			if !tc.refuse {
				return
			}
			for _, want := range []string{
				"gc supervisor start: tmux servers still answer in " + tmux.SocketDir("") + " (gc-city)",
				"Failed to connect to bus: No medium found",
				"the manager's default, TMUX_TMPDIR unset, is assumed",
				fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()),
				"rerun with TMUX_TMPDIR set as the installed service has it",
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), want)
				}
			}
		})
	}
}

// ensureSupervisorRunning on an upgraded linux install (a unit an earlier gc
// wrote) with no supervisor running, where systemctl --user cannot reach the
// user manager (cron, sudo -u, su), with no TMUX_TMPDIR set: its install
// fails on the unreachable manager with installSupervisorSystemd's own
// message, writing and starting nothing, and its fallback, a supervisor
// forked outside the service, is let through, as before the socket guard.
// ensureSupervisorRunning's install leg needs the real HOME, where the unit
// would be the operator's own, so the test drives the two legs it chains.
func TestEnsureSupervisorRunningLegsWithAnUnreachableUserManager(t *testing.T) {
	tmuxHome(t)
	t.Setenv(tmux.TmpdirEnv, "")
	stubRunningSupervisor(t, false, "", nil)
	stubServiceManagers(t, "", errUnreachableUserManager, "")
	stubLiveTmuxSockets(t, tmux.SocketDir(""), "gc-city")

	code, stderr, calls, unchanged := systemdInstallReaching(t, "old unit\n", "", false)
	if code != 1 || !unchanged || slices.Contains(calls, "--user start gascity-supervisor.service") || !strings.Contains(stderr, "per-user systemd instance is not available") {
		t.Fatalf("install: code %d, systemctl calls %v, unit unchanged %v, stderr %q; want installSupervisorSystemd's unreachable-manager failure", code, calls, unchanged, stderr)
	}
	var forkStderr bytes.Buffer
	if refuseForkedSupervisorTmuxSocketDirMove("linux", &forkStderr) {
		t.Fatalf("fallback fork refused: %s", forkStderr.String())
	}
}

// A drift restart starts the replacement with the TMUX_TMPDIR its service
// gives it: the one its file records, else (a file an earlier gc wrote) the
// service manager's, which a gc start's own shell cannot change; a direct
// restart forks it with this process's. Each is refused when that resolves
// to another socket directory than the running supervisor's.
func TestRefuseSupervisorRestartTmuxSocketDirMove(t *testing.T) {
	dir, _, _ := tmuxTmpdirFixtures(t)
	systemd := restartSpec{SystemdManaged: true, PID: 4242, ServiceName: "gascity-supervisor.service"}
	launchd := restartSpec{LaunchdManaged: true, PID: 4242}
	direct := restartSpec{PID: 4242, ExePath: "/tmp/gc", Argv: []string{"supervisor", "run"}}
	for _, tc := range []struct {
		name     string
		spec     restartSpec
		recorded *string // the service file's TMUX_TMPDIR; nil for a file that records none
		manager  string  // the service manager's TMUX_TMPDIR
		shell    string  // this process's TMUX_TMPDIR
		running  string
		want     string // "" for no refusal, else a remedy the refusal names
	}{
		{name: "systemd, file records none, manager moved", spec: systemd, manager: dir, want: "gc supervisor install"},
		{name: "systemd, file records none, manager matches", spec: systemd, manager: dir, running: dir},
		{name: "systemd, file records the running value", spec: systemd, recorded: new(string), manager: dir, shell: dir},
		{name: "systemd, file records another value", spec: systemd, recorded: &dir, want: "gc supervisor install"},
		{name: "launchd, plist records none, launchd moved", spec: launchd, manager: dir, want: "gc supervisor install"},
		{name: "direct, shell moved", spec: direct, shell: dir, want: "rerun with TMUX_TMPDIR set as the active supervisor has it"},
		{name: "direct, shell matches", spec: direct, shell: dir, running: dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launchdHome(t)
			t.Setenv(tmux.TmpdirEnv, tc.shell)
			stubRunningSupervisor(t, true, tc.running, nil)
			stubServiceManagers(t, tmux.TmpdirEnv+"="+tc.manager+"\n", nil, tc.manager)
			for path, tmpl := range map[string]string{supervisorSystemdServicePath(): supervisorSystemdTemplate, supervisorLaunchdPlistPath(): supervisorLaunchdTemplate} {
				content := "old service file\n"
				if tc.recorded != nil {
					rendered, err := renderSupervisorTemplate(tmpl, tmuxServiceData(os.Getenv("GC_HOME"), *tc.recorded))
					if err != nil {
						t.Fatal(err)
					}
					content = rendered
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := refuseSupervisorRestartTmuxSocketDirMove(tc.spec)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("restart refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "Refusing the restart")):
				t.Fatalf("restart error = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A restart that cannot read the running supervisor's TMUX_TMPDIR, or its
// service file, cannot tell where the replacement keeps its sockets, so it
// is refused.
func TestRefuseSupervisorRestartTmuxSocketDirMoveFailsClosed(t *testing.T) {
	t.Run("running supervisor unreadable", func(t *testing.T) {
		tmuxHome(t)
		stubRunningSupervisor(t, true, "", errors.New("permission denied"))
		if err := refuseSupervisorRestartTmuxSocketDirMove(restartSpec{PID: 4242}); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("restart error = %v, want the failed read", err)
		}
	})
	t.Run("service file missing", func(t *testing.T) {
		tmuxHome(t)
		stubRunningSupervisor(t, true, "", nil)
		if err := refuseSupervisorRestartTmuxSocketDirMove(restartSpec{SystemdManaged: true, PID: 4242}); err == nil || !strings.Contains(err.Error(), "reading the supervisor service file") {
			t.Fatalf("restart error = %v, want the failed read", err)
		}
	})
}

// A drift restart the socket-directory guard refuses stops nothing: no
// service-manager call, no kill and no spawn.
func TestRestartSupervisorStopsNothingTheTmuxSocketGuardRefuses(t *testing.T) {
	refusal := errors.New("refused")
	for _, spec := range []restartSpec{
		{SystemdManaged: true, ServiceName: "gascity-supervisor.service"},
		{LaunchdManaged: true},
		{PID: 4242, ExePath: "/tmp/gc", Argv: []string{"supervisor", "run"}},
	} {
		var calls []string
		record := func(name string) func(...string) error {
			return func(...string) error { calls = append(calls, name); return nil }
		}
		h := restartHelpers{
			Systemctl:       record("systemctl"),
			Launchctl:       record("launchctl"),
			Kill:            func(int) error { calls = append(calls, "kill"); return nil },
			WaitExit:        func(int) error { calls = append(calls, "wait"); return nil },
			Spawn:           func(string, ...string) error { calls = append(calls, "spawn"); return nil },
			KeepTmuxSockets: func(restartSpec) error { return refusal },
		}
		if err := restartSupervisor(spec, h); !errors.Is(err, refusal) || len(calls) != 0 {
			t.Fatalf("restartSupervisor(%+v) = %v with calls %v, want the refusal and no calls", spec, err, calls)
		}
	}
}

// A dump that cannot reach the manager names systemctl's own reason, which
// exec keeps out of the error's text.
func TestSystemdManagerEnvironmentCarriesSystemctlStderr(t *testing.T) {
	installFakeDelegatedSystemctl(t, 1, "Failed to connect to bus: No medium found")
	_, err := systemdManagerTmuxTmpdir()
	if !errors.Is(err, errSystemdUserManagerUnreachable) || !strings.Contains(err.Error(), "exit status 1: Failed to connect to bus: No medium found") {
		t.Fatalf("systemdManagerTmuxTmpdir() error = %v, want the unreachable manager with systemctl's stderr", err)
	}
}

// systemd dumps its environment one assignment per line, escaping a value
// that holds whitespace or a shell metacharacter as $'...', which is reported
// rather than guessed at. A dump that fails is a manager this process cannot
// reach, named apart from one whose value cannot be read.
func TestSystemdManagerTmuxTmpdir(t *testing.T) {
	for _, tc := range []struct {
		name, dump  string
		dumpErr     error
		want        string
		wantErr     string
		unreachable bool
	}{
		{name: "set", dump: "HOME=/home/u\nTMUX_TMPDIR=/run/user/1000/tmux\nPATH=/usr/bin\n", want: "/run/user/1000/tmux"},
		{name: "unset", dump: "HOME=/home/u\nTMUX_TMPDIRS=/x\n"},
		{name: "escaped", dump: "TMUX_TMPDIR=$'/run/tmux dir'\n", wantErr: "shell-escaped"},
		{name: "manager unreachable", dumpErr: errUnreachableUserManager, wantErr: "Failed to connect to bus", unreachable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubServiceManagers(t, tc.dump, tc.dumpErr, "")
			got, err := systemdManagerTmuxTmpdir()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("systemdManagerTmuxTmpdir() = %q, %v; want an error naming %q", got, err, tc.wantErr)
				}
				if got := errors.Is(err, errSystemdUserManagerUnreachable); got != tc.unreachable {
					t.Fatalf("errors.Is(%v, errSystemdUserManagerUnreachable) = %v, want %v", err, got, tc.unreachable)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("systemdManagerTmuxTmpdir() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
