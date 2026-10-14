//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// staticTestingM is a scripted inner runner for guard-wiring tests.
type staticTestingM struct{ code int }

func (m staticTestingM) Run() int { return m.code }

func TestTmuxLeakGuard_CleanRunPassesCodeThrough(t *testing.T) {
	for _, code := range []int{0, 7} {
		g := newTmuxLeakGuardedTestingM(staticTestingM{code: code}, t.TempDir())
		var out bytes.Buffer
		got := g.runWith(
			func() int { return code },
			func(string) []string { return nil },
			func() []tmuxProcInfo { return nil },
			func([]tmuxProcInfo) {},
			&out,
		)
		if got != code {
			t.Errorf("clean run exit = %d, want %d", got, code)
		}
		if strings.Contains(out.String(), "leaked") {
			t.Errorf("clean run wrote a leak report: %q", out.String())
		}
	}
}

func TestTmuxLeakGuard_LeakedSocketFailsBuild(t *testing.T) {
	root := t.TempDir()
	g := newTmuxLeakGuardedTestingM(staticTestingM{}, root)
	var out bytes.Buffer
	leakedSocket := filepath.Join(root, "tmux-1000", "test-city")
	got := g.runWith(
		func() int { return 0 },
		func(gotRoot string) []string {
			if gotRoot != root {
				t.Errorf("kill pass root = %q, want %q", gotRoot, root)
			}
			return []string{leakedSocket}
		},
		func() []tmuxProcInfo { return nil },
		func([]tmuxProcInfo) {},
		&out,
	)
	if got != 1 {
		t.Fatalf("exit = %d, want 1 when a leaked server was found on a green run", got)
	}
	if !strings.Contains(out.String(), leakedSocket) || !strings.Contains(out.String(), "leaked") {
		t.Fatalf("leak report missing socket path: %q", out.String())
	}
}

func TestTmuxLeakGuard_ZombieProcessFailsBuildAndReaps(t *testing.T) {
	g := newTmuxLeakGuardedTestingM(staticTestingM{}, t.TempDir())
	var out bytes.Buffer
	zombie := tmuxProcInfo{PID: 12345, Argv: []string{"tmux", "-u", "-L", "test-city", "new-session", "-s", "mayor"}}
	var reaped []tmuxProcInfo
	got := g.runWith(
		func() int { return 0 },
		func(string) []string { return nil },
		func() []tmuxProcInfo { return []tmuxProcInfo{zombie} },
		func(procs []tmuxProcInfo) { reaped = append(reaped, procs...) },
		&out,
	)
	if got != 1 {
		t.Fatalf("exit = %d, want 1 for an environ-attributed zombie server", got)
	}
	if len(reaped) != 1 || reaped[0].PID != zombie.PID {
		t.Fatalf("reaped = %+v, want the zombie PID handed to the reaper", reaped)
	}
	if !strings.Contains(out.String(), "pid=12345") || !strings.Contains(out.String(), "-L test-city") {
		t.Fatalf("leak report missing zombie pid/argv: %q", out.String())
	}
}

func TestTmuxLeakGuard_FailedRunKeepsOriginalCode(t *testing.T) {
	g := newTmuxLeakGuardedTestingM(staticTestingM{}, t.TempDir())
	var out bytes.Buffer
	got := g.runWith(
		func() int { return 2 },
		func(string) []string { return []string{"/x/tmux-1/sock"} },
		func() []tmuxProcInfo { return nil },
		func([]tmuxProcInfo) {},
		&out,
	)
	if got != 2 {
		t.Fatalf("exit = %d, want the original failing code 2 preserved", got)
	}
}

func TestProcEnvValueReadsOwnEnviron(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc")
	}
	// /proc/<pid>/environ is the process's STARTUP environment snapshot
	// (later Setenv calls don't appear), so probe a variable guaranteed
	// present at startup rather than one set here.
	got, ok := procEnvValue(os.Getpid(), "PATH")
	if !ok || got == "" {
		t.Fatalf("procEnvValue(self, PATH) = %q, %v; want the startup PATH", got, ok)
	}
	if _, ok := procEnvValue(os.Getpid(), "GC_TMUX_LEAK_GUARD_DEFINITELY_ABSENT"); ok {
		t.Fatal("procEnvValue reported a value for an absent key")
	}
}

func TestReadTmuxArgvRejectsNonTmuxProcess(t *testing.T) {
	if _, ok := readTmuxArgv(os.Getpid()); ok {
		t.Fatal("readTmuxArgv accepted the test binary as tmux")
	}
}

// spawnGuardTestTmuxServer starts a real detached tmux server in the shape
// the dip-73cr05 leak takes — city-name socket "test-city", session "mayor"
// — with its socket under socketRoot/tmux-<uid>/ and its environ carrying
// TMUX_TMPDIR=socketRoot, mirroring how cmd/gc tests' in-process tmux
// invocations inherit TestMain's TMUX_TMPDIR. Returns the socket path and
// the server PID. A cleanup kill is registered for failure paths.
func spawnGuardTestTmuxServer(t *testing.T, socketRoot string) (string, int) {
	t.Helper()
	socketDir := filepath.Join(socketRoot, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", socketDir, err)
	}
	socketPath := filepath.Join(socketDir, "test-city")
	env := append(os.Environ(), "TMUX_TMPDIR="+socketRoot)
	newSession := exec.Command("tmux", "-S", socketPath, "new-session", "-d", "-s", "mayor", "sleep 60")
	newSession.Env = env
	if out, err := newSession.CombinedOutput(); err != nil {
		t.Fatalf("spawn test tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() {
		kill := exec.Command("tmux", "-S", socketPath, "kill-server")
		kill.Env = env
		_ = kill.Run()
	})
	pidOut := exec.Command("tmux", "-S", socketPath, "display-message", "-p", "#{pid}")
	pidOut.Env = env
	raw, err := pidOut.Output()
	if err != nil {
		t.Fatalf("read test tmux server pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad tmux server pid %q: %v", raw, err)
	}
	return socketPath, pid
}

func waitForPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after 5s", pid)
}

// TestKillTmuxServersUnderSocketRoot_KillsCityNameServerLikeNamedSessionLeak
// reproduces the dip-73cr05 leak shape end-to-end: a server on a city-name
// socket ("test-city") with exit-empty off and zero remaining sessions —
// exactly what a real named-session test leaves behind after its stub pane
// exits — must be found and killed by the socket pass.
func TestKillTmuxServersUnderSocketRoot_KillsCityNameServerLikeNamedSessionLeak(t *testing.T) {
	tmuxtest.RequireTmux(t)
	socketRoot := shortSocketTempDir(t, "gct-guardcheck")
	socketPath, pid := spawnGuardTestTmuxServer(t, socketRoot)

	env := append(os.Environ(), "TMUX_TMPDIR="+socketRoot)
	for _, args := range [][]string{
		{"-S", socketPath, "set-option", "-g", "exit-empty", "off"},
		{"-S", socketPath, "kill-session", "-t", "mayor"},
	} {
		cmd := exec.Command("tmux", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	if !pidAlive(pid) {
		t.Fatal("posture control failed: server should survive its last session with exit-empty off (the leak under test)")
	}

	leaked := killTmuxServersUnderSocketRoot(socketRoot)
	if len(leaked) != 1 || leaked[0] != socketPath {
		t.Fatalf("killTmuxServersUnderSocketRoot = %v, want exactly [%s]", leaked, socketPath)
	}
	waitForPIDGone(t, pid)
}

// TestDiscoverTmuxProcessesWithSocketRootEnv_FindsDeletedSocketZombie covers
// the environ-attribution path: a server whose socket FILE is already gone
// (the per-run root was removed) but whose process survives. The /proc scan
// must attribute it to the fake root by TMUX_TMPDIR and the reaper must end
// it.
func TestDiscoverTmuxProcessesWithSocketRootEnv_FindsDeletedSocketZombie(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc; environ attribution degrades to the socket pass")
	}
	tmuxtest.RequireTmux(t)
	socketRoot := shortSocketTempDir(t, "gct-guardzomb")
	socketPath, pid := spawnGuardTestTmuxServer(t, socketRoot)
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("delete socket file: %v", err)
	}

	found := discoverTmuxProcessesWithSocketRootEnv(func(_ int, root string) bool { return root == socketRoot })
	var hit *tmuxProcInfo
	for i := range found {
		if found[i].PID == pid {
			hit = &found[i]
		}
	}
	if hit == nil {
		t.Fatalf("environ scan missed zombie pid %d (found %+v)", pid, found)
	}
	reapTmuxLeakProcesses([]tmuxProcInfo{*hit})
	waitForPIDGone(t, pid)
}

// TestIsTmuxTestSocketRoot pins which socket roots the startup sweep owns:
// only "<gct-<pid>-…>/tmux". The other real-tmux tests in this file keep their
// fixtures on roots outside that shape, which is what stops a sibling suite's
// sweep from reaping their servers mid-test.
func TestIsTmuxTestSocketRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		root string
		want bool
	}{
		{"per-run root", "/tmp/gct-4242-abc/tmux", true},
		{"empty", "", false},
		{"not a tmux dir", "/tmp/gct-4242-abc/other", false},
		{"parent without an owner pid", "/tmp/gct-abc/tmux", false},
		{"parent without the shared prefix", "/tmp/other-4242-abc/tmux", false},
		{"kill-pass fixture root", "/tmp/gct-guardcheck123", false},
		{"deleted-socket fixture root", "/tmp/gct-guardzomb123", false},
		{"boundary test fixture root", "/tmp/gct-sweeppar123/gone/tmux", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTmuxTestSocketRoot(tc.root); got != tc.want {
				t.Errorf("isTmuxTestSocketRoot(%q) = %v, want %v", tc.root, got, tc.want)
			}
		})
	}
}

// TestSweepStaleTmuxTestServers_ReapsRootGoneKeepsRootPresent proves the
// startup sweep's decision boundary: an owned socket root that no longer
// exists marks its server as dead-run residue (reap); a root still on disk
// protects its server (skip), including this run's own. The fixture roots sit
// outside the shape production startup sweeps own, and the sweep under test is
// handed a rule that owns exactly them, so a sibling suite's startup sweep —
// routine in a full gate, and injected below — cannot reap the fixture first.
func TestSweepStaleTmuxTestServers_ReapsRootGoneKeepsRootPresent(t *testing.T) {
	if _, err := os.ReadDir("/proc"); err != nil {
		t.Skip("host has no /proc; startup sweep degrades to no-op")
	}
	tmuxtest.RequireTmux(t)
	parent := shortSocketTempDir(t, "gct-sweeppar")
	goneRoot := filepath.Join(parent, "gone", "tmux")
	keptRoot := filepath.Join(parent, "kept", "tmux")
	ownsFixture := func(root string) bool { return filepath.Dir(filepath.Dir(root)) == parent }
	_, gonePID := spawnGuardTestTmuxServer(t, goneRoot)
	_, keptPID := spawnGuardTestTmuxServer(t, keptRoot)
	if err := os.RemoveAll(filepath.Dir(goneRoot)); err != nil {
		t.Fatalf("remove gone root: %v", err)
	}

	// A sibling test binary's startup sweep lands between fixture setup and
	// this test's own sweep. A full gate starts dozens of cmd/gc binaries at
	// once, so this is routine, not theoretical (ga-ok9ick): the fixture must
	// still prove the decision boundary through this test's own sweep.
	sweepStaleTmuxTestServers("sibling", io.Discard)

	var out bytes.Buffer
	sweepStaleTmuxServers("test", &out, ownsFixture)

	waitForPIDGone(t, gonePID)
	if !pidAlive(keptPID) {
		t.Fatal("sweep killed a server whose socket root still exists")
	}
	if !strings.Contains(out.String(), fmt.Sprintf("pid=%d", gonePID)) {
		t.Fatalf("sweep report missing reaped pid %d: %q", gonePID, out.String())
	}
	if strings.Contains(out.String(), fmt.Sprintf("pid=%d", keptPID)) {
		t.Fatalf("sweep report names the protected pid %d: %q", keptPID, out.String())
	}
}
