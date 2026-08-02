package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestHelperProcessNudgePollerHang is not a real test: it is exec'd by the
// reap tests as a stand-in nudge poller. Its argv tail carries the real
// poller command shape ("nudge poll --city ... --session ... <agent>") so
// pidutil.Cmdline-based identity checks see a genuine poller cmdline. It
// hangs until killed.
func TestHelperProcessNudgePollerHang(_ *testing.T) {
	if os.Getenv("GO_WANT_NUDGE_POLLER_HELPER") != "1" {
		return
	}
	time.Sleep(5 * time.Minute)
	os.Exit(0)
}

// spawnFakePoller launches a helper process whose cmdline matches a nudge
// poller for (cityPath, sessionName, agentName) and writes its pidfile the
// way acquireNudgePollerLease would. Returns the PID.
func spawnFakePoller(t *testing.T, cityPath, sessionName, agentName string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=TestHelperProcessNudgePollerHang", "--",
		"nudge", "poll", "--city", cityPath, "--session", sessionName, agentName)
	cmd.Env = append(os.Environ(), "GO_WANT_NUDGE_POLLER_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake poller: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	// Reap the child on exit so the kill assertions below see a dead process,
	// not a zombie parked on our un-Wait()ed handle.
	go func() { _, _ = cmd.Process.Wait() }()

	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir pollers dir: %v", err)
	}
	if err := writeNudgePollerPID(pidPath, pid); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	// The helper's argv must actually pass the matcher before the test
	// proceeds, or a slow exec would make the reap see an unverifiable PID.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if argv, err := pidutil.Cmdline(pid); err == nil && len(argv) > 0 {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake poller cmdline never became readable")
	return 0
}

func waitForDeath(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidutil.Alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after reap", pid)
}

func TestReapLiveNudgePollersKillsRejectedTarget(t *testing.T) {
	cityPath := t.TempDir()
	pid := spawnFakePoller(t, cityPath, "dead-session", "agent-1")

	reaped := reapLiveNudgePollers(cityPath, func(sessionName, _ string) bool {
		return sessionName != "dead-session"
	}, os.Stderr)

	if len(reaped) != 1 || reaped[0] != "dead-session/agent-1" {
		t.Fatalf("reaped = %v, want [dead-session/agent-1]", reaped)
	}
	waitForDeath(t, pid)
	pidPath := nudgePollerPIDPath(cityPath, "dead-session", "agent-1")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pidfile still present after reap: %v", err)
	}
}

func TestReapLiveNudgePollersKeepsAcceptedTarget(t *testing.T) {
	cityPath := t.TempDir()
	pid := spawnFakePoller(t, cityPath, "live-session", "agent-1")

	reaped := reapLiveNudgePollers(cityPath, func(string, string) bool { return true }, os.Stderr)

	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none", reaped)
	}
	if !pidutil.Alive(pid) {
		t.Fatalf("kept poller was killed")
	}
	pidPath := nudgePollerPIDPath(cityPath, "live-session", "agent-1")
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("kept poller's pidfile removed: %v", err)
	}
}

func TestReapLiveNudgePollersCleansDeadPIDFile(t *testing.T) {
	cityPath := t.TempDir()
	// Spawn and immediately kill so the pidfile points at a dead PID.
	pid := spawnFakePoller(t, cityPath, "gone-session", "agent-1")
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitForDeath(t, pid)

	reaped := reapLiveNudgePollers(cityPath, func(string, string) bool { return false }, os.Stderr)

	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none (dead pid is cleanup, not a reap)", reaped)
	}
	pidPath := nudgePollerPIDPath(cityPath, "gone-session", "agent-1")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("dead pidfile not cleaned: %v", err)
	}
}

func TestReapLiveNudgePollersLeavesForeignLivePIDAlone(t *testing.T) {
	cityPath := t.TempDir()
	// A pidfile pointing at a live process that is NOT a poller for this
	// city (this test process): the file goes, the process stays.
	pidPath := nudgePollerPIDPath(cityPath, "recycled-session", "agent-1")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeNudgePollerPID(pidPath, os.Getpid()); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}

	reaped := reapLiveNudgePollers(cityPath, func(string, string) bool { return false }, os.Stderr)

	if len(reaped) != 0 {
		t.Fatalf("reaped = %v, want none", reaped)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("stale pidfile for recycled PID not removed: %v", err)
	}
}

func TestReapNudgePollersForClosedSessionScopesToSession(t *testing.T) {
	cityPath := t.TempDir()
	closedPID := spawnFakePoller(t, cityPath, "closing-session", "agent-1")
	livePID := spawnFakePoller(t, cityPath, "other-session", "agent-2")

	reaped := reapNudgePollersForClosedSession(cityPath, "closing-session", os.Stderr)

	if len(reaped) != 1 || reaped[0] != "closing-session/agent-1" {
		t.Fatalf("reaped = %v, want [closing-session/agent-1]", reaped)
	}
	waitForDeath(t, closedPID)
	if !pidutil.Alive(livePID) {
		t.Fatalf("unrelated session's poller was killed")
	}
	if got := reapNudgePollersForClosedSession(cityPath, "", os.Stderr); got != nil {
		t.Fatalf("empty session name must be a no-op, got %v", got)
	}
}

func TestSessionDrainedPastPollerGraceFallsBackToLastActive(t *testing.T) {
	cr := &CityRuntime{sessionDrains: newDrainTracker()}
	now := time.Now()

	mk := func(state, sleepReason string, lastActive time.Time) sessionpkg.Info {
		return sessionpkg.Info{MetadataState: state, SleepReason: sleepReason, LastActive: lastActive}
	}
	cases := []struct {
		name string
		in   sessionpkg.Info
		want bool
	}{
		{"active session never reaped", mk("active", "", now.Add(-24*time.Hour)), false},
		{"freshly drained kept", mk("drained", "", now.Add(-time.Minute)), false},
		{"long drained reaped", mk("drained", "", now.Add(-time.Hour)), true},
		{"sleep-reason drained reaped", mk("asleep", "drained", now.Add(-time.Hour)), true},
		{"unknown age kept", mk("drained", "", time.Time{}), false},
	}
	for _, tc := range cases {
		if got := cr.sessionDrainedPastPollerGrace(tc.in, now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	// In-memory drain clock takes precedence over LastActive.
	info := mk("drained", "", now.Add(-time.Hour))
	info.ID = "gc-wisp-test"
	cr.sessionDrains.set("gc-wisp-test", &drainState{startedAt: now.Add(-time.Minute)})
	if cr.sessionDrainedPastPollerGrace(info, now) {
		t.Errorf("fresh in-memory drain clock must outrank stale LastActive")
	}
	cr.sessionDrains.set("gc-wisp-test", &drainState{startedAt: now.Add(-time.Hour)})
	if !cr.sessionDrainedPastPollerGrace(info, now) {
		t.Errorf("old in-memory drain clock must trigger the reap grace")
	}
}

