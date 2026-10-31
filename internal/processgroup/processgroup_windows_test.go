//go:build windows

package processgroup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// spawnTreeCommand runs a shell that starts a detached grandchild and then
// waits. The grandchild is the process a direct Process.Kill cannot reach, and
// the whole point of the Job Object is that it does.
func spawnTreeCommand() *exec.Cmd {
	return exec.Command("cmd.exe", "/c", "start /B ping -n 61 127.0.0.1 >NUL & ping -n 61 127.0.0.1 >NUL")
}

func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false
	}
	return event == uint32(windows.WAIT_TIMEOUT)
}

// processCreationTime returns pid's creation time, which together with the PID
// forms an identity that survives PID recycling.
func processCreationTime(pid int) (int64, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	return handleCreationTime(handle)
}

func handleCreationTime(handle windows.Handle) (int64, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return creation.Nanoseconds(), true
}

// terminateIfSameProcess ends id's process only if the process now holding
// id.pid is still the one the test recorded.
//
// The creation time is read from the same handle that terminates. Opening a
// bare PID and terminating it kills whatever holds that number now, and once
// the recorded process has exited that can be any unrelated process on the
// machine: the test's Job Object limits what the test runs, not what it can
// open.
func terminateIfSameProcess(id processIdentity) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(id.pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	if start, ok := handleCreationTime(handle); !ok || start != id.start {
		return
	}
	_ = windows.TerminateProcess(handle, 1)
}

// descendantPIDs returns every process reachable from root through the parent
// links in a Toolhelp snapshot, root excluded.
//
// A parent link alone is not enough to identify a descendant on Windows.
// ParentProcessID is recorded at creation and never cleared, and PIDs are
// recycled aggressively, so a process orphaned long ago keeps pointing at a
// number that now belongs to something else — here, to the very command the
// test just started. Walking those links transitively then drags in whole
// unrelated subtrees; an earlier version of this test collected 62 such
// strangers and reported them as survivors of the cleanup.
//
// Creation time settles it. A real descendant cannot predate the root it
// descends from, so a candidate that started earlier is an impostor holding a
// stale link, and is not walked through either.
func descendantPIDs(root int) []int {
	rootStart, ok := processCreationTime(root)
	if !ok {
		return nil
	}

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil
	}
	children := map[int][]int{}
	for {
		children[int(entry.ParentProcessID)] = append(children[int(entry.ParentProcessID)], int(entry.ProcessID))
		entry.Size = uint32(unsafe.Sizeof(windows.ProcessEntry32{}))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			break
		}
	}

	var out []int
	seen := map[int]bool{root: true}
	stack := append([]int(nil), children[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		start, ok := processCreationTime(pid)
		if !ok || start < rootStart {
			continue
		}
		out = append(out, pid)
		stack = append(stack, children[pid]...)
	}
	return out
}

// processIdentity is a PID together with the creation time that distinguishes
// it from any later process that inherits the same number.
type processIdentity struct {
	pid   int
	start int64
}

func (p processIdentity) alive() bool {
	if !processAlive(p.pid) {
		return false
	}
	start, ok := processCreationTime(p.pid)
	if !ok {
		// Live but unreadable: do not invent a death.
		return true
	}
	return start == p.start
}

func identitiesOf(pids []int) []processIdentity {
	out := make([]processIdentity, 0, len(pids))
	for _, pid := range pids {
		start, ok := processCreationTime(pid)
		if !ok {
			continue
		}
		out = append(out, processIdentity{pid: pid, start: start})
	}
	return out
}

func stillAlive(identities []processIdentity) []int {
	var alive []int
	for _, id := range identities {
		if id.alive() {
			alive = append(alive, id.pid)
		}
	}
	return alive
}

func waitForDescendants(t *testing.T, root int) []processIdentity {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if identities := identitiesOf(descendantPIDs(root)); len(identities) > 0 {
			return identities
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d never spawned a descendant", root)
	return nil
}

func TestStartCommandFailureAfterStartWaitsForCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("ComSpec"), "/c", "exit", "0")
	cmd.WaitDelay = 50 * time.Millisecond
	cancelDone := make(chan struct{})
	cmd.Cancel = func() error {
		defer close(cancelDone)
		return KillCommandGroup(cmd, 0, Options{})
	}
	injected := errors.New("injected start failure")
	startCommandAfterStartForTest = func(*exec.Cmd) error {
		cancel()
		return injected
	}
	t.Cleanup(func() { startCommandAfterStartForTest = nil })

	if _, err := StartCommand(cmd, SurviveRelease); !errors.Is(err, injected) {
		t.Fatalf("StartCommand = %v, want the injected failure", err)
	}
	if cmd.Process == nil {
		t.Fatal("test did not reach the post-Start failure path")
	}
	if cmd.ProcessState == nil {
		t.Fatal("StartCommand returned after a successful Start without waiting for the command")
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("pid %d is still alive after StartCommand abandoned it", cmd.Process.Pid)
	}
	select {
	case <-cancelDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the CommandContext cancellation watcher did not finish")
	}

	time.Sleep(200 * time.Millisecond)
	runtime.Gosched()
	var stacks bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&stacks, 2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stacks.String(), "os/exec.(*Cmd).watchCtx") {
		for _, stack := range strings.Split(stacks.String(), "\n\n") {
			if strings.Contains(stack, "os/exec.(*Cmd).watchCtx") {
				t.Log(stack)
			}
		}
		t.Fatal("StartCommand returned with an exec context watcher still waiting for Wait")
	}
}

func TestStartCommandCancellationAfterResumeUsesPublishedJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe", "/c", "start /B ping -n 61 127.0.0.1 >NUL & ping -n 61 127.0.0.1 >NUL")
	cancelResult := make(chan error, 1)
	cmd.Cancel = func() error {
		err := KillCommandGroup(cmd, 0, Options{})
		cancelResult <- err
		return err
	}
	var descendants []processIdentity
	startCommandAfterResumeForTest = func(started *exec.Cmd) {
		descendants = waitForDescendants(t, started.Process.Pid)
		t.Cleanup(func() {
			for _, id := range descendants {
				terminateIfSameProcess(id)
			}
		})
		cancel()
		select {
		case err := <-cancelResult:
			if errors.Is(err, ErrNotOwned) {
				t.Fatalf("cancellation after resume reported an unowned command: %v", err)
			}
			if err != nil {
				t.Fatalf("cancellation after resume: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("CommandContext did not invoke Cancel after resume")
		}
	}
	t.Cleanup(func() { startCommandAfterResumeForTest = nil })

	if _, err := StartCommand(cmd, SurviveRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	if len(descendants) == 0 {
		t.Fatal("test did not observe a descendant before cancellation")
	}
	_ = cmd.Wait()
	if err := ReleaseCommand(cmd); err != nil {
		t.Fatalf("ReleaseCommand: %v", err)
	}
	waitTreeReaped(t, descendants, "cancellation after resume returned success")
}

// TestTerminateCommandReapsDescendants is the regression test for the defect a
// reviewer demonstrated by hand: TerminateCommand killed only the direct
// process, returned nil, and left a grandchild running.
func TestTerminateCommandReapsDescendants(t *testing.T) {
	cmd := spawnTreeCommand()
	pgid, err := StartCommand(cmd, KillOnRelease)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("StartCommand returned group %d, want %d", pgid, cmd.Process.Pid)
	}
	go func() { _ = cmd.Wait() }()

	descendants := waitForDescendants(t, cmd.Process.Pid)

	// timeout 0 skips the cooperative phase, so this measures the forced path.
	if err := TerminateCommand(cmd, pgid, 0, Options{}); err != nil {
		t.Fatalf("TerminateCommand: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := stillAlive(descendants)
		if len(alive) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("TerminateCommand returned nil but descendants %v are still alive", alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("TerminateCommand returned nil but pid %d is still alive", cmd.Process.Pid)
	}
}

// TestReleaseCommandKillsOnRelease pins the helper half of the persistence
// boundary: cancelling a helper reaps its tree.
func TestReleaseCommandKillsOnRelease(t *testing.T) {
	cmd := spawnTreeCommand()
	if _, err := StartCommand(cmd, KillOnRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	rootStart, ok := processCreationTime(cmd.Process.Pid)
	if !ok {
		t.Fatalf("reading creation time for pid %d", cmd.Process.Pid)
	}
	rootIdentity := processIdentity{pid: cmd.Process.Pid, start: rootStart}
	descendants := waitForDescendants(t, cmd.Process.Pid)

	if err := ReleaseCommand(cmd); err != nil {
		t.Fatalf("ReleaseCommand: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := stillAlive(append(descendants, rootIdentity))
		if len(alive) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("releasing a kill-on-release tree left %v alive", alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReleaseCommandSurvivesRelease pins the other half: detaching from a
// long-lived agent server must not kill it.
func TestReleaseCommandSurvivesRelease(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 61 127.0.0.1 >NUL")
	if _, err := StartCommand(cmd, SurviveRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()

	if err := ReleaseCommand(cmd); err != nil {
		t.Fatalf("ReleaseCommand: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("releasing a survive-release tree killed pid %d", pid)
	}
}

// TestTerminateCommandReportsUnownedTree pins the honesty requirement: a
// command with no ownership boundary cannot have its descendants reaped, and
// saying so is the contract.
func TestTerminateCommandReportsUnownedTree(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 61 127.0.0.1 >NUL")
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	err := TerminateCommand(cmd, 0, 0, Options{})
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("TerminateCommand on an unowned command = %v, want ErrNotOwned", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(cmd.Process.Pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("unowned fallback left pid %d alive; the reachable part of the cleanup must still happen", cmd.Process.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTerminateBarePIDUnsupported pins that a bare PID is refused rather than
// acted on: there is no kernel object behind it and the number may already have
// been recycled.
func TestTerminateBarePIDUnsupported(t *testing.T) {
	if err := Terminate(4321, time.Second, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Terminate(bare pid) = %v, want ErrUnsupported", err)
	}
}

// waitTreeReaped fails unless every identity is gone within the deadline.
func waitTreeReaped(t *testing.T, identities []processIdentity, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := stillAlive(identities)
		if len(alive) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s: processes %v are still alive", what, alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestTerminateManagedCommandReapsTreeAfterLeaderExit is the regression test
// for a managed stop reporting success over a live tree.
//
// The leader exiting is the signal the stop sequence waits on, and it used to
// be the whole test: done closing ended the sequence with a nil return. That is
// precisely the case a cooperating agent produces — it handles the stop request
// by exiting, while the helpers it spawned keep running — and on Windows
// nothing else reaps them, because closing a SurviveRelease job kills nothing.
// The caller was told the session had stopped while its tree ran on.
func TestTerminateManagedCommandReapsTreeAfterLeaderExit(t *testing.T) {
	cmd := spawnTreeCommand()
	pgid, err := StartCommand(cmd, SurviveRelease)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	descendants := waitForDescendants(t, cmd.Process.Pid)

	// Retire the leader only, leaving its descendants running.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing leader: %v", err)
	}
	<-done

	if err := TerminateManagedCommand(cmd, pgid, done, 5*time.Second, Options{}); err != nil {
		t.Fatalf("TerminateManagedCommand: %v", err)
	}
	waitTreeReaped(t, descendants, "managed stop returned success")

	if tree := lookupOwned(cmd); tree != nil {
		t.Fatal("managed stop left the command owned; the job handle and map entry leak for the life of the process")
	}
}

// TestTerminateManagedCommandToleratesConcurrentRelease runs a managed stop
// against a provider-style reaper, end to end.
//
// A provider's reaper releases ownership as soon as it finds the tree empty,
// and emptying the tree is exactly what a stop is trying to cause — so the two
// race on every stop, not just unlucky ones. The stop must still report
// success and leave nothing running. The lease that keeps a release from
// closing the job handle underneath the stop is pinned by
// TestRetiringAWatchedTreeLeavesAnInFlightLeaseUsable: here the reaper keeps a
// tree that still has members, so this test passes without the lease.
func TestTerminateManagedCommandToleratesConcurrentRelease(t *testing.T) {
	cmd := spawnTreeCommand()
	pgid, err := StartCommand(cmd, SurviveRelease)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
		reapLikeAProvider(cmd)
	}()

	descendants := waitForDescendants(t, cmd.Process.Pid)
	t.Cleanup(func() {
		for _, id := range descendants {
			terminateIfSameProcess(id)
		}
	})

	errCh := make(chan error, 1)
	go func() { errCh <- TerminateManagedCommand(cmd, pgid, done, 5*time.Second, Options{}) }()

	// Let the stop enter and take its lease, then spring the reaper on it.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		// The stop's cooperative request can end the leader first. The reaper
		// has then waited it already, which releases the Process (so Kill
		// reports EINVAL), and the release races the stop all the same.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("killing leader: %v", err)
		}
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("TerminateManagedCommand raced with ReleaseCommand: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("TerminateManagedCommand did not return; the release and the stop deadlocked")
	}
	waitTreeReaped(t, descendants, "managed stop racing a release returned success")
}

// TestTerminateManagedCommandOnReapedSessionIsIdempotent pins that a stop
// arriving after the reaper has already released ownership reports success.
//
// The reaper released only because it saw the tree empty, so the tree's exit
// is observed fact and answering with ErrNotOwned would fail a stop that has
// nothing left to do. That keeps parity with the Unix path, where signalling
// the group of an exited leader returns ESRCH and is treated as success.
func TestTerminateManagedCommandOnReapedSessionIsIdempotent(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	pgid, err := StartCommand(cmd, SurviveRelease)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
		reapLikeAProvider(cmd)
	}()
	<-done
	// Let the release land before the stop arrives.
	deadline := time.Now().Add(5 * time.Second)
	for lookupOwned(cmd) != nil {
		if !time.Now().Before(deadline) {
			t.Fatal("reaper never released ownership")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := TerminateManagedCommand(cmd, pgid, done, time.Second, Options{}); err != nil {
		t.Fatalf("stopping an already-reaped session = %v, want success", err)
	}
}

// TestTerminateCommandOnReapedProcessIsIdempotent covers the same situation on
// the entry point that has no done channel to consult.
//
// This is the service shutdown path: Manager.Close stops a proxy process that
// may already have exited and been released by its own reaper, and a shutdown
// that reports failure for a service that is already down fails the whole
// close.
func TestTerminateCommandOnReapedProcessIsIdempotent(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	if _, err := StartCommand(cmd, KillOnRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := ReleaseCommand(cmd); err != nil {
		t.Fatalf("ReleaseCommand: %v", err)
	}

	if err := TerminateCommand(cmd, 0, time.Second, Options{}); err != nil {
		t.Fatalf("terminating an already-reaped, already-released command = %v, want success", err)
	}
}

// reapLikeAProvider is what the subprocess and acp reapers do once the leader
// has been waited on.
func reapLikeAProvider(cmd *exec.Cmd) {
	_ = ReleaseCommandIfEmpty(cmd)
}

// startSurvivorTree starts a SurviveRelease tree under a provider-style reaper,
// retires the leader, and returns once the reaper has finished with it, leaving
// the leader's descendants running. That is the ordinary shape of a cooperative
// stop: the agent exits in response, and the helpers it spawned do not.
func startSurvivorTree(t *testing.T) (cmd *exec.Cmd, pgid int, done chan struct{}, descendants []processIdentity) {
	t.Helper()
	cmd = spawnTreeCommand()
	pgid, err := StartCommand(cmd, SurviveRelease)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	done = make(chan struct{})
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
		reapLikeAProvider(cmd)
		close(reaped)
	}()
	descendants = waitForDescendants(t, cmd.Process.Pid)
	t.Cleanup(func() {
		for _, id := range descendants {
			terminateIfSameProcess(id)
		}
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing leader: %v", err)
	}
	<-reaped
	if alive := stillAlive(descendants); len(alive) == 0 {
		t.Fatal("the descendants died with the leader; the test needs them to outlive it")
	}
	return cmd, pgid, done, descendants
}

// TestAnOwnedTreeKeepsItsLeadersPIDFromReuse is the invariant a request to a
// tree's console group rests on. The group is named by the leader's PID, and
// once the leader was reaped nothing kept that number: a later process could
// be given it, and with it the cooperative stop meant for this tree. The owned
// tree now holds the leader's process object until it is retired, and lets it
// go then.
func TestAnOwnedTreeKeepsItsLeadersPIDFromReuse(t *testing.T) {
	cmd, pgid, done, descendants := startSurvivorTree(t)
	if !HoldsTree(cmd) {
		t.Fatal("the tree was released with members still running")
	}
	// Whatever else held the leader - its console host, until it learns of
	// the exit - lets go within moments. The tree's hold has to last.
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, ok := exitCodeAt(pgid); !ok || code == stillActive {
			t.Fatalf("after the leader was reaped, PID %d opens as %v (exit code %d); want the reaped leader, held by the owned tree", pgid, ok, code)
		}
	}

	if err := TerminateManagedCommand(cmd, pgid, done, time.Second, Options{}); err != nil {
		t.Fatalf("TerminateManagedCommand: %v", err)
	}
	waitTreeReaped(t, descendants, "TerminateManagedCommand returned success")
	if HoldsTree(cmd) {
		t.Fatal("the tree is still held after a confirmed stop")
	}
	// A watcher's read can hold the handles a moment past the stop.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		code, ok := exitCodeAt(pgid)
		if !ok || code == stillActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("PID %d still names the reaped leader (exit code %d) after its tree was retired; the hold outlived the tree", pgid, code)
		}
	}
}

// stillActive is STILL_ACTIVE, the exit code of a process that has not exited.
const stillActive = 259

// exitCodeAt opens whatever process pid names now and returns its exit code,
// or false when pid names no process.
func exitCodeAt(pid int) (uint32, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return 0, false
	}
	return code, true
}

// TestManagedStopAfterTheReaperRunsReachesSurvivors is the regression test for
// a managed stop that arrives after the provider's reaper has finished with a
// session whose helpers outlived its leader.
//
// The survivors are 60-second pings, so a stop that merely reports success
// leaves them running long past the reap deadline below. Liveness is polled
// rather than read once: the job's accounting drops a process as its last
// thread exits, which can precede the moment its handle is signalled.
func TestManagedStopAfterTheReaperRunsReachesSurvivors(t *testing.T) {
	cmd, pgid, done, descendants := startSurvivorTree(t)

	if err := TerminateManagedCommand(cmd, pgid, done, 5*time.Second, Options{}); err != nil {
		t.Fatalf("TerminateManagedCommand: %v; the tree was still owned and should have been reaped", err)
	}
	waitTreeReaped(t, descendants, "TerminateManagedCommand returned success")
}

// TestForcedCleanupAfterTheReaperRunsReachesSurvivors covers the failed-start
// path, which forces the tree down with KillCommandGroup rather than asking.
func TestForcedCleanupAfterTheReaperRunsReachesSurvivors(t *testing.T) {
	cmd, pgid, _, descendants := startSurvivorTree(t)

	if err := KillCommandGroup(cmd, pgid, Options{}); err != nil {
		t.Fatalf("KillCommandGroup: %v; the tree was still owned and should have been reaped", err)
	}
	waitTreeReaped(t, descendants, "KillCommandGroup returned success")
}

// TestAReapedUnownedCommandIsNotReportedStopped pins that a leader having
// exited says nothing about what it spawned. With no ownership boundary there
// is nothing that can speak for the descendants, so the honest answer is
// ErrNotOwned, not success.
func TestAReapedUnownedCommandIsNotReportedStopped(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	done := make(chan struct{})
	close(done)

	for name, stop := range map[string]func() error{
		"TerminateCommand":        func() error { return TerminateCommand(cmd, 0, time.Second, Options{}) },
		"KillCommandGroup":        func() error { return KillCommandGroup(cmd, 0, Options{}) },
		"TerminateManagedCommand": func() error { return TerminateManagedCommand(cmd, 0, done, time.Second, Options{}) },
	} {
		if err := stop(); !errors.Is(err, ErrNotOwned) {
			t.Errorf("%s on a reaped, never-owned command = %v, want ErrNotOwned", name, err)
		}
	}
}

// TestAReleasedLiveProcessIsNotReportedStopped pins that EINVAL from Kill is not
// evidence of an exit. os.Process.Release gives up the handle of a process that
// is still running, and a later Kill then fails with EINVAL before it ever
// reaches TerminateProcess.
func TestAReleasedLiveProcessIsNotReportedStopped(t *testing.T) {
	cmd := exec.Command("ping.exe", "-n", "61", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Hold a handle of our own before Release gives Go's away, so the process
	// can still be observed and reaped by this test without addressing a PID.
	pin, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("pinning pid %d: %v", cmd.Process.Pid, err)
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(pin, 1)
		_ = windows.CloseHandle(pin)
	})
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	err = TerminateCommand(cmd, 0, time.Second, Options{})
	if event, waitErr := windows.WaitForSingleObject(pin, 0); waitErr != nil || event != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("the process was not running when the stop returned (event %d, %v); the test proves nothing", event, waitErr)
	}
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("TerminateCommand on a released process that is still running = %v, want ErrNotOwned", err)
	}
}

// TestADetachedTreeIsNotReportedStopped pins that a deliberate detach is not
// mistaken for a drained tree. Releasing a SurviveRelease tree that still has
// live processes gives up the only boundary that could reach them, so a later
// stop must say so rather than report them gone.
func TestADetachedTreeIsNotReportedStopped(t *testing.T) {
	cmd := spawnTreeCommand()
	if _, err := StartCommand(cmd, SurviveRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	descendants := waitForDescendants(t, cmd.Process.Pid)
	t.Cleanup(func() {
		for _, id := range descendants {
			terminateIfSameProcess(id)
		}
	})

	if err := ReleaseCommand(cmd); err != nil {
		t.Fatalf("ReleaseCommand: %v", err)
	}
	if alive := stillAlive(descendants); len(alive) == 0 {
		t.Fatal("releasing a SurviveRelease tree killed it; the test needs it to survive the release")
	}

	if err := TerminateCommand(cmd, 0, time.Second, Options{}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("TerminateCommand after detaching a live tree = %v, want ErrNotOwned", err)
	}
}

// startBriefSurvivorTree starts a tree whose leader exits at once and leaves a
// child that exits on its own about two seconds later, and reaps the leader the
// way a provider does. It returns once the reaper has run, with the tree kept
// because the child was still running.
func startBriefSurvivorTree(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/c", "start /B ping -n 3 127.0.0.1 >NUL & exit 0")
	if _, err := StartCommand(cmd, KillOnRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _ = KillCommandGroup(cmd, 0, Options{}) })
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		reapLikeAProvider(cmd)
		close(reaped)
	}()
	select {
	case <-reaped:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader was never reaped")
	}
	if !HoldsTree(cmd) {
		t.Fatal("the tree was retired at leader exit although its child was still running; the test proves nothing")
	}
	return cmd
}

// waitUntilRetired waits for cmd's tree to stop being held.
func waitUntilRetired(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for HoldsTree(cmd) {
		if !time.Now().Before(deadline) {
			t.Fatal("the tree is still owned after its last member exited; its job handle, map entry and command leak for the life of the process")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAKeptTreeIsRetiredOnceItsLastMemberExits is the leak a one-shot empty
// check leaves: the reaper keeps a tree whose child is still running, and when
// that child exits on its own nothing ever looks again.
func TestAKeptTreeIsRetiredOnceItsLastMemberExits(t *testing.T) {
	cmd := startBriefSurvivorTree(t)

	waitUntilRetired(t, cmd)
	if !wasDrained(cmd) {
		t.Fatal("the tree was let go without being recorded as seen empty")
	}
	if err := TerminateCommand(cmd, 0, time.Second, Options{}); err != nil {
		t.Fatalf("stopping a tree retired as drained = %v, want success", err)
	}
}

// TestATreeKeptByAnUnconfirmedStopIsRetiredOnceEmpty covers the other way a tree
// is kept over live members: a stop that could not confirm the tree empty.
func TestATreeKeptByAnUnconfirmedStopIsRetiredOnceEmpty(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 3 127.0.0.1 >NUL")
	if _, err := StartCommand(cmd, KillOnRelease); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _ = KillCommandGroup(cmd, 0, Options{}) })
	go func() { _ = cmd.Wait() }()

	tree := acquireOwned(cmd)
	if tree == nil {
		t.Fatal("StartCommand did not take ownership of the tree")
	}
	settleOwned(cmd, tree, errors.New("injected: stop not confirmed"))
	if !HoldsTree(cmd) {
		t.Fatal("an unconfirmed stop let go of a tree that was still running")
	}

	waitUntilRetired(t, cmd)
	if !wasDrained(cmd) {
		t.Fatal("the tree was let go without being recorded as seen empty")
	}
}

// TestRetiringAWatchedTreeLeavesAnInFlightLeaseUsable pins that the watch
// retires through the lease protocol: an operation still using the job handle
// when the tree is seen empty keeps a working handle, and the handle closes
// when that operation ends.
func TestRetiringAWatchedTreeLeavesAnInFlightLeaseUsable(t *testing.T) {
	cmd := startBriefSurvivorTree(t)
	lease := acquireOwned(cmd)
	if lease == nil {
		t.Fatal("the kept tree is not owned")
	}

	waitUntilRetired(t, cmd)
	active, err := lease.activeProcesses()
	if err != nil {
		t.Fatalf("the watch closed the job handle under an operation still using it: %v", err)
	}
	if active != 0 {
		t.Fatalf("a retired tree reports %d active process(es)", active)
	}
	if err := lease.endUse(); err != nil {
		t.Fatalf("ending the last lease: %v", err)
	}
	lease.mu.Lock()
	closed := lease.closed
	lease.mu.Unlock()
	if !closed {
		t.Fatal("the job handle stayed open after the last lease on a retired tree ended")
	}
}
