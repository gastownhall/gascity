package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// These tests pin #6857: a city's nudge pollers must not outlive the city.
// `gc stop` and the supervisor's city shutdown retire the city's live
// pollers before they retire its bead store, and a poller that still
// survives never reopens the store of a city that is down, because with
// the proxied bd transport any read restarts the bd proxy and Dolt server
// the stop just retired. Queued nudges stay in the queue for the next start.

// markNudgePollCityRunningForTest makes the poll loop see a live city
// controller, so tests that drive cmdNudgePoll without a real controller
// exercise the loop instead of the stopped-city exit.
func markNudgePollCityRunningForTest(t *testing.T) {
	t.Helper()
	orig := nudgePollControllerAlive
	nudgePollControllerAlive = func(string) int { return os.Getpid() }
	t.Cleanup(func() { nudgePollControllerAlive = orig })
}

func writeNudgePollerPIDFileForTest(t *testing.T, cityPath, sessionName, agentName string, pid int) string {
	t.Helper()
	pidPath := nudgePollerPIDPath(cityPath, sessionName, agentName)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	return pidPath
}

func waitForProcessExitForTest(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("poller-like process %d still running after stopCityNudgePollers", cmd.Process.Pid)
	}
}

func TestStopCityNudgePollersTerminatesLivePollerAndKeepsQueuedNudges(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	cmd := startPollerLikeProcess(t, cityPath, "session-id")
	pidPath := writeNudgePollerPIDFileForTest(t, cityPath, "sess-worker", "session-id", cmd.Process.Pid)
	item := newQueuedNudgeWithOptions("session-id", "follow-up", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{})
	if err := nudgequeue.WithState(cityPath, func(state *nudgequeue.State) error {
		state.Pending = append(state.Pending, item)
		return nil
	}); err != nil {
		t.Fatalf("seeding queue: %v", err)
	}

	if err := stopCityNudgePollers(cityPath); err != nil {
		t.Fatalf("stopCityNudgePollers: %v", err)
	}

	waitForProcessExitForTest(t, cmd)
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poller PID file after stop: stat err = %v, want it removed", err)
	}
	if _, err := os.Stat(pidPath + ".lock"); err != nil {
		t.Fatalf("poller lock file after stop: %v, want the stable lock inode kept", err)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != item.ID {
		t.Fatalf("pending after stop = %+v, want queued nudge %s kept for the next start", state.Pending, item.ID)
	}
}

func TestStopCityNudgePollersSparesProcessesThatAreNotThisCitysPoller(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	otherCityPath := filepath.Join(t.TempDir(), "other-city")

	// A PID file naming a live process that is not a poller at all: a reused
	// PID. This test binary stands in for it.
	unrelatedPIDPath := writeNudgePollerPIDFileForTest(t, cityPath, "sess-reused", "session-id", os.Getpid())
	// A PID file naming another city's live poller.
	otherCity := startPollerLikeProcess(t, otherCityPath, "session-id")
	otherCityPIDPath := writeNudgePollerPIDFileForTest(t, cityPath, "sess-worker", "session-id", otherCity.Process.Pid)
	// A PID file naming this city's poller for a different session/target
	// than the file's own name.
	wrongTarget := startPollerLikeProcess(t, cityPath, "other-target")
	wrongTargetPIDPath := writeNudgePollerPIDFileForTest(t, cityPath, "sess-worker", "third-target", wrongTarget.Process.Pid)

	if err := stopCityNudgePollers(cityPath); err != nil {
		t.Fatalf("stopCityNudgePollers: %v", err)
	}

	for _, pid := range []int{otherCity.Process.Pid, wrongTarget.Process.Pid} {
		if !pidutil.Alive(pid) {
			t.Fatalf("pid %d was signaled; it is not the poller its PID file names", pid)
		}
	}
	for _, p := range []string{unrelatedPIDPath, otherCityPIDPath, wrongTargetPIDPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("PID file %s: %v, want it left alone", p, err)
		}
	}
}

func TestStopCityNudgePollersWithoutPollersDirIsNoOp(t *testing.T) {
	if err := stopCityNudgePollers(t.TempDir()); err != nil {
		t.Fatalf("stopCityNudgePollers on a city without pollers: %v", err)
	}
}

func TestShutdownBeadsProviderStopsTheCitysNudgePollers(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := filepath.Join(t.TempDir(), "city")
	cmd := startPollerLikeProcess(t, cityPath, "session-id")
	writeNudgePollerPIDFileForTest(t, cityPath, "sess-worker", "session-id", cmd.Process.Pid)

	if err := shutdownBeadsProvider(cityPath); err != nil {
		t.Fatalf("shutdownBeadsProvider: %v", err)
	}

	waitForProcessExitForTest(t, cmd)
}

func TestNudgePollCityStopped(t *testing.T) {
	cityPath := t.TempDir()
	target := nudgeTarget{cityPath: cityPath, sessionName: "worker-session"}
	running := runtime.NewFake()
	if err := running.Start(context.Background(), "worker-session", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cases := []struct {
		name       string
		controller int
		sp         runtime.Provider
		want       bool
	}{
		{name: "controller down, session gone", controller: 0, sp: runtime.NewFake(), want: true},
		{name: "controller down, session still running", controller: 0, sp: running, want: false},
		{name: "controller up, session gone", controller: 4242, sp: runtime.NewFake(), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := nudgePollControllerAlive
			nudgePollControllerAlive = func(got string) int {
				if got != cityPath {
					t.Errorf("controller probe for %q, want %q", got, cityPath)
				}
				return tc.controller
			}
			defer func() { nudgePollControllerAlive = orig }()
			if got := nudgePollCityStopped(target, tc.sp); got != tc.want {
				t.Fatalf("nudgePollCityStopped = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCmdNudgePollExitsWithoutObservingWhenItsCityIsStopped(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	item := newQueuedNudgeWithOptions("worker", "follow-up", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
		SessionID: created.ID,
	})
	if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	// No controller answers for the city, and the fake runtime has no
	// session: the city is stopped.
	origController := nudgePollControllerAlive
	nudgePollControllerAlive = func(string) int { return 0 }
	defer func() { nudgePollControllerAlive = origController }()

	observeCalls := 0
	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		observeCalls++
		return worker.LiveObservation{Running: false}, nil
	}
	defer func() { nudgeObserveTarget = origObserve }()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdNudgePoll([]string{created.ID}, "worker-session", time.Millisecond, 0, true, &stdout, &stderr)
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cmdNudgePoll kept polling a stopped city")
	}
	if code != 0 {
		t.Fatalf("cmdNudgePoll = %d, want 0; stderr=%s", code, stderr.String())
	}
	if observeCalls != 0 {
		t.Fatalf("observe calls = %d, want 0: observing reads the bead store, which restarts a stopped city's bd and Dolt", observeCalls)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Fatalf("stderr = %q, want the stopped-city exit explained", stderr.String())
	}
	state, err := nudgequeue.LoadState(cityDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != item.ID {
		t.Fatalf("pending after exit = %+v, want queued nudge %s kept for the next start", state.Pending, item.ID)
	}
}
