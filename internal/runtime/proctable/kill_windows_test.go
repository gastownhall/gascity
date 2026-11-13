//go:build windows

package proctable

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

func TestTerminateHeldProcessWaitsOnTerminateAccessDenied(t *testing.T) {
	oldTerminate := terminateProcess
	oldWait := waitForProcessHandleFn
	t.Cleanup(func() {
		terminateProcess = oldTerminate
		waitForProcessHandleFn = oldWait
	})
	var waits []time.Duration
	terminateProcess = func(windows.Handle, uint32) error {
		return windows.ERROR_ACCESS_DENIED
	}
	waitForProcessHandleFn = func(_ windows.Handle, timeout time.Duration) bool {
		waits = append(waits, timeout)
		return timeout > 0
	}

	if err := terminateHeldProcess(windows.Handle(1234), 4321, 250*time.Millisecond); err != nil {
		t.Fatalf("terminateHeldProcess: %v", err)
	}
	if len(waits) != 2 || waits[0] != 0 || waits[1] != 250*time.Millisecond {
		t.Fatalf("waits = %v; want initial zero check, then reap grace", waits)
	}
}

func TestTerminateHeldProcessReportsTerminateAccessDeniedWhenStillAlive(t *testing.T) {
	oldTerminate := terminateProcess
	oldWait := waitForProcessHandleFn
	t.Cleanup(func() {
		terminateProcess = oldTerminate
		waitForProcessHandleFn = oldWait
	})
	terminateProcess = func(windows.Handle, uint32) error {
		return windows.ERROR_ACCESS_DENIED
	}
	waitForProcessHandleFn = func(windows.Handle, time.Duration) bool {
		return false
	}

	err := terminateHeldProcess(windows.Handle(1234), 4321, 250*time.Millisecond)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("terminateHeldProcess error = %v; want access denied", err)
	}
}

func TestKillByPIDRefusesReservedPIDs(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if err := KillByPID(pid); err == nil {
			t.Errorf("KillByPID(%d) succeeded; reserved PIDs must be refused outright", pid)
		}
	}
}

func TestKillByPIDSucceedsForAProcessThatCannotExist(t *testing.T) {
	if err := KillByPID(impossiblePID); err != nil {
		t.Fatalf("KillByPID(%d) = %v, want nil: a process that is already gone is the outcome the caller wanted", impossiblePID, err)
	}
}

func TestKillProcessWithIdentityRefusesWithoutAnIdentity(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "kill-no-identity"), t.TempDir())

	if err := killProcessWithIdentity(helper.pid, "", time.Second); err == nil {
		t.Fatalf("killProcessWithIdentity(%d, \"\") succeeded; an unfenced kill is exactly the recycled-PID bug", helper.pid)
	}
	if !helper.running() {
		t.Fatalf("helper PID %d was terminated despite the refusal", helper.pid)
	}
}

// TestKillProcessWithIdentityFencesAgainstARecycledPID is the core contract of
// the kill path: the same PID is refused under a stale identity and terminated
// under the right one, so the fence is proven to discriminate rather than to
// refuse everything.
func TestKillProcessWithIdentityFencesAgainstARecycledPID(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "kill-fence"), t.TempDir())

	const staleIdentity = "1"
	err := killProcessWithIdentity(helper.pid, staleIdentity, runtime.ManagedProcessReapGrace)
	if err == nil {
		t.Fatalf("killProcessWithIdentity(%d, %q) succeeded; that PID no longer belongs to the process the caller meant", helper.pid, staleIdentity)
	}
	if !strings.Contains(err.Error(), "recycled") {
		t.Errorf("refusal reads %q; it must say the PID was recycled so the caller can tell it from a failure to kill", err)
	}
	if !helper.running() {
		t.Fatalf("helper PID %d was terminated despite the identity mismatch", helper.pid)
	}
	if current, identityErr := ProcessIdentity(helper.pid); identityErr != nil || current != helper.identity {
		t.Fatalf("helper PID %d now reports identity %q (err %v), want the original %q", helper.pid, current, identityErr, helper.identity)
	}

	if err := killProcessWithIdentity(helper.pid, helper.identity, runtime.ManagedProcessReapGrace); err != nil {
		t.Fatalf("killProcessWithIdentity(%d, %q) with the correct identity: %v", helper.pid, helper.identity, err)
	}
	if !helper.awaitExit(10 * time.Second) {
		t.Fatalf("helper PID %d was not reaped after a successful termination", helper.pid)
	}
}

// TestKillByPIDTerminatesAndConfirms exercises the exported path end to end:
// it must not return until the process is confirmed gone, because its callers
// use the return to decide whether a replacement may start.
func TestKillByPIDTerminatesAndConfirms(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "kill-confirm"), t.TempDir())

	if err := KillByPID(helper.pid); err != nil {
		t.Fatalf("KillByPID(%d): %v", helper.pid, err)
	}
	if !helper.awaitExit(10 * time.Second) {
		t.Fatalf("KillByPID(%d) returned success but the process was still running", helper.pid)
	}
}

// pinnedChildren waits for root to have at least want direct children and
// returns a handle to each, opened while the child is alive. A held handle pins
// the process object, so cleanup through it can only ever reach that process,
// never a later one given the same PID.
func pinnedChildren(t *testing.T, root int, want int) []windows.Handle {
	t.Helper()
	rootIdentity, err := ProcessIdentity(root)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d): %v", root, err)
	}
	rootCreation, _ := strconv.ParseInt(rootIdentity, 10, 64)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := systemProcesses()
		if err != nil {
			t.Fatalf("systemProcesses: %v", err)
		}
		var handles []windows.Handle
		for _, row := range rows {
			if row.ppid != root || !row.creationOK || row.creation < rootCreation {
				continue
			}
			const access = windows.PROCESS_TERMINATE | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
			handle, err := windows.OpenProcess(access, false, uint32(row.pid))
			if err != nil {
				continue
			}
			if created, err := handleCreationTime(handle); err != nil || created != row.creation {
				_ = windows.CloseHandle(handle)
				continue
			}
			handles = append(handles, handle)
		}
		if len(handles) >= want {
			t.Cleanup(func() {
				for _, handle := range handles {
					_ = windows.TerminateProcess(handle, 1)
					_ = windows.CloseHandle(handle)
				}
			})
			return handles
		}
		for _, handle := range handles {
			_ = windows.CloseHandle(handle)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("PID %d never had %d children", root, want)
	return nil
}

// TestKillRuntimeReachesTheWholeTree is the regression test for orphan cleanup
// reporting success after killing only a runtime's root. On Unix the root leads
// a process group and one signal reaches its helpers; here the helpers are
// separate processes that nothing addressed.
func TestKillRuntimeReachesTheWholeTree(t *testing.T) {
	root := exec.Command("cmd.exe", "/c", "start /B ping -n 61 127.0.0.1 >NUL & ping -n 61 127.0.0.1 >NUL")
	if err := root.Start(); err != nil {
		t.Fatalf("starting the tree: %v", err)
	}
	t.Cleanup(func() { _ = root.Process.Kill(); _ = root.Wait() })
	children := pinnedChildren(t, root.Process.Pid, 2)
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}

	if err := KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity}); err != nil {
		t.Fatalf("KillRuntime: %v", err)
	}
	for i, handle := range children {
		if event, err := windows.WaitForSingleObject(handle, 0); err != nil || event != windows.WAIT_OBJECT_0 {
			t.Errorf("child %d of the runtime is still running after KillRuntime reported the runtime gone", i)
		}
	}
}

// TestKillRuntimeLeavesALaterProcessAtTheScannedPIDAlone is the regression test
// for a recycled PID vouching for itself. The runtime the scan saw is gone; the
// process now holding its PID is a different one, and must not be killed on
// the strength of the old scan.
func TestKillRuntimeLeavesALaterProcessAtTheScannedPIDAlone(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "kill-runtime-stale"), t.TempDir())

	if err := KillRuntime(runtime.LiveRuntime{PID: helper.pid, StartIdentity: "1"}); err != nil {
		t.Fatalf("KillRuntime for a runtime that is already gone = %v, want nil", err)
	}
	// Waited out rather than read once: the helper's exit is observed by its
	// reaper goroutine, so an immediate check can report a terminated helper
	// as running.
	if helper.awaitExit(time.Second) {
		t.Fatalf("KillRuntime terminated PID %d, whose identity is not the one the scan recorded", helper.pid)
	}
}

func TestKillRuntimeRefusesWithoutAScanIdentity(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "kill-runtime-no-identity"), t.TempDir())

	if err := KillRuntime(runtime.LiveRuntime{PID: helper.pid}); err == nil {
		t.Fatal("KillRuntime succeeded without the start identity its scan observed")
	}
	if helper.awaitExit(time.Second) {
		t.Fatalf("KillRuntime terminated PID %d without an identity to fence it against", helper.pid)
	}
}

// startLiveTree runs script under a cmd.exe root. Processes the script starts
// inherit the root's stdin, which the test holds open, so argv mocks among them
// idle until cleanup closes it. The command line is passed as written: Go's
// argument quoting escapes a quote as \", which cmd.exe does not understand.
func startLiveTree(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	root := exec.Command("cmd.exe")
	root.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /c "` + script + `"`}
	stdin, err := root.StdinPipe()
	if err != nil {
		t.Fatalf("opening the tree's stdin pipe: %v", err)
	}
	if err := root.Start(); err != nil {
		t.Fatalf("starting the tree: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = root.Process.Kill()
		_ = root.Wait()
	})
	return root
}

// descendant is one process under a test's tree, held so that whatever is done
// through it reaches that process and no later holder of its PID.
type descendant struct {
	pid    int
	ppid   int
	image  string
	argv   []string
	handle windows.Handle
}

// awaitDescendants waits until ready accepts root's descendants and returns
// them, each held. Every parent link is taken from one snapshot in which the
// parent was alive, so it cannot name a stranger. Cleanup terminates each
// descendant through its handle.
func awaitDescendants(t *testing.T, root int, ready func([]descendant) bool) []descendant {
	t.Helper()
	identity, err := ProcessIdentity(root)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d): %v", root, err)
	}
	rootCreation, _ := strconv.ParseInt(identity, 10, 64)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		found := holdDescendants(t, root, rootCreation)
		if ready(found) {
			// The test may release a descendant's handle early by closing it
			// and zeroing it in the returned slice, which shares this one's
			// backing array.
			t.Cleanup(func() {
				for _, d := range found {
					if d.handle == 0 {
						continue
					}
					_ = windows.TerminateProcess(d.handle, 1)
					_ = windows.CloseHandle(d.handle)
				}
			})
			return found
		}
		for _, d := range found {
			_ = windows.CloseHandle(d.handle)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("PID %d never grew the tree this test needs", root)
	return nil
}

func holdDescendants(t *testing.T, root int, rootCreation int64) []descendant {
	t.Helper()
	rows, err := systemProcesses()
	if err != nil {
		t.Fatalf("systemProcesses: %v", err)
	}
	creations := map[int]int64{root: rootCreation}
	var found []descendant
	for added := true; added; {
		added = false
		for _, row := range rows {
			parentCreation, isChild := creations[row.ppid]
			if _, seen := creations[row.pid]; seen || !isChild || !row.creationOK || row.creation < parentCreation {
				continue
			}
			handle, err := windows.OpenProcess(treeClassifyAccess, false, uint32(row.pid))
			if err != nil {
				continue
			}
			if created, err := handleCreationTime(handle); err != nil || created != row.creation {
				_ = windows.CloseHandle(handle)
				continue
			}
			argv, _ := handleArgv(handle)
			creations[row.pid] = row.creation
			found = append(found, descendant{pid: row.pid, ppid: row.ppid, image: row.image, argv: argv, handle: handle})
			added = true
		}
	}
	return found
}

func withImage(found []descendant, image string) []descendant {
	var matches []descendant
	for _, d := range found {
		if strings.EqualFold(d.image, image) {
			matches = append(matches, d)
		}
	}
	return matches
}

func withArgv1(found []descendant, argv1 string) (descendant, bool) {
	for _, d := range found {
		if len(d.argv) >= 2 && d.argv[1] == argv1 {
			return d, true
		}
	}
	return descendant{}, false
}

// TestKillRuntimeReachesAGrandchild takes the walk below the root's own
// children: a helper's helpers are the runtime's too.
func TestKillRuntimeReachesAGrandchild(t *testing.T) {
	root := startLiveTree(t, "start /B cmd.exe /c ping -n 61 127.0.0.1 & ping -n 61 127.0.0.1 >NUL")
	tree := awaitDescendants(t, root.Process.Pid, func(found []descendant) bool {
		grandchild := false
		for _, d := range withImage(found, "ping.exe") {
			grandchild = grandchild || d.ppid != root.Process.Pid
		}
		return len(withImage(found, "ping.exe")) >= 2 && grandchild
	})
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}

	if err := KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity}); err != nil {
		t.Fatalf("KillRuntime: %v", err)
	}
	for _, d := range tree {
		if !waitForProcessHandle(d.handle, 0) {
			t.Errorf("PID %d (%s, child of %d) is still running after KillRuntime reported the runtime gone", d.pid, d.image, d.ppid)
		}
	}
}

// TestKillRuntimeFindsTheTreeBeforeTerminatingAnyOfIt is the regression test
// for a teardown that reported success over a live grandchild. Terminating the
// root can end a child that was waiting on it, and a child gone before the walk
// holds it takes with it the only link its own children had back to the root.
// The hook ends the middle process at the first moment the root's death can be
// observed, which is when such a child would exit on its own, and gives up the
// test's handle to it so that nothing but KillRuntime can keep it in the table.
func TestKillRuntimeFindsTheTreeBeforeTerminatingAnyOfIt(t *testing.T) {
	root := startLiveTree(t, "start /B cmd.exe /c ping -n 61 127.0.0.1 & ping -n 61 127.0.0.1 >NUL")
	tree := awaitDescendants(t, root.Process.Pid, func(found []descendant) bool {
		_, _, ok := middleAndGrandchild(found, root.Process.Pid)
		return ok
	})
	middle, grandchild, _ := middleAndGrandchild(tree, root.Process.Pid)
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}
	middlePID := tree[middle].pid
	testHookRuntimeMemberTerminated = func(pid int) {
		if pid != root.Process.Pid || tree[middle].handle == 0 {
			return
		}
		handle := tree[middle].handle
		tree[middle].handle = 0
		_ = windows.TerminateProcess(handle, 1)
		exited := waitForProcessHandle(handle, 5*time.Second)
		_ = windows.CloseHandle(handle)
		if !exited {
			t.Errorf("the middle process, PID %d, did not exit when the test ended it", middlePID)
		}
	}
	t.Cleanup(func() { testHookRuntimeMemberTerminated = nil })

	if err := KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity}); err != nil {
		t.Fatalf("KillRuntime: %v", err)
	}
	if tree[middle].handle != 0 {
		t.Fatal("KillRuntime never reported the root terminated, so the middle process was not ended where the defect needs it")
	}
	if !waitForProcessHandle(tree[grandchild].handle, 0) {
		t.Fatalf("PID %d is still running after KillRuntime reported the runtime gone; its parent, PID %d, exited when the root was terminated", tree[grandchild].pid, middlePID)
	}
}

// middleAndGrandchild finds, among root's descendants, a child of root with a
// ping.exe child of its own, and returns both indexes.
func middleAndGrandchild(found []descendant, root int) (middle, grandchild int, ok bool) {
	for i, parent := range found {
		if parent.ppid != root || !strings.EqualFold(parent.image, "cmd.exe") {
			continue
		}
		for j, child := range found {
			if child.ppid == parent.pid && strings.EqualFold(child.image, "ping.exe") {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

// supervisorMockGateVar makes this test binary stand in for gc's supervisor.
// Started with that variable set as `<test binary> supervisor run`, it idles
// until stdin closes. It is then a build of the same program as the tests that
// run KillRuntime, as gc's supervisor is of gc. The name follows the GC_*_HELPER
// convention internal/testenv leaves unscrubbed.
const supervisorMockGateVar = "GC_PROCTABLE_SUPERVISOR_HELPER"

func init() {
	if os.Getenv(supervisorMockGateVar) != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// supervisorMockCommand is the cmd.exe command that starts this test binary as
// a supervisor mock in the background.
func supervisorMockCommand(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("finding the test binary: %v", err)
	}
	t.Setenv(supervisorMockGateVar, "1")
	return `start "" /B "` + executable + `" supervisor run`
}

// TestKillRuntimeSparesInfrastructureInTheTree drives classification against
// real processes: argv mocks standing in for the managed Dolt watchdog and the
// supervisor survive the teardown, and the helper beside them does not.
func TestKillRuntimeSparesInfrastructureInTheTree(t *testing.T) {
	root := startLiveTree(t, "start /B cmd.exe "+ManagedDoltScopeWatchdogVerb+" & "+supervisorMockCommand(t)+" & ping -n 61 127.0.0.1 >NUL")
	tree := awaitDescendants(t, root.Process.Pid, func(found []descendant) bool {
		_, watchdog := withArgv1(found, ManagedDoltScopeWatchdogVerb)
		_, supervisor := withArgv1(found, "supervisor")
		return watchdog && supervisor && len(withImage(found, "ping.exe")) >= 1
	})
	watchdog, _ := withArgv1(tree, ManagedDoltScopeWatchdogVerb)
	supervisor, _ := withArgv1(tree, "supervisor")
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}

	if err := KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity}); err != nil {
		t.Fatalf("KillRuntime: %v", err)
	}
	for _, spared := range []descendant{watchdog, supervisor} {
		if waitForProcessHandle(spared.handle, 500*time.Millisecond) {
			t.Errorf("KillRuntime terminated PID %d (argv %q), which is city infrastructure", spared.pid, spared.argv)
		}
	}
	for _, d := range withImage(tree, "ping.exe") {
		if !waitForProcessHandle(d.handle, 0) {
			t.Errorf("the runtime's helper PID %d is still running", d.pid)
		}
	}
}

// TestKillRuntimeTerminatesAnotherProgramGivenTheSupervisorsArguments keeps the
// supervisor's exemption from covering whatever is started with its arguments:
// cmd.exe given `supervisor run` is not gc, and it is part of the runtime.
func TestKillRuntimeTerminatesAnotherProgramGivenTheSupervisorsArguments(t *testing.T) {
	root := startLiveTree(t, "start /B cmd.exe supervisor run & ping -n 61 127.0.0.1 >NUL")
	tree := awaitDescendants(t, root.Process.Pid, func(found []descendant) bool {
		_, lookalike := withArgv1(found, "supervisor")
		return lookalike && len(withImage(found, "ping.exe")) >= 1
	})
	lookalike, _ := withArgv1(tree, "supervisor")
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}

	if err := KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity}); err != nil {
		t.Fatalf("KillRuntime: %v", err)
	}
	if !waitForProcessHandle(lookalike.handle, 0) {
		t.Errorf("PID %d (argv %q) is still running: its image is %s, not a build of gc, so the supervisor's arguments do not make it the supervisor", lookalike.pid, lookalike.argv, lookalike.image)
	}
}

func TestIsSupervisorImageRecognizesABuildOfThisProgram(t *testing.T) {
	supervisor, err := isSupervisorImage(windows.CurrentProcess())
	if err != nil || !supervisor {
		t.Fatalf("isSupervisorImage(this process) = %v, %v; want true: it is a build of this program", supervisor, err)
	}
}

func TestImageIsThisProgramRejectsOtherExecutables(t *testing.T) {
	for _, name := range []string{"cmd.exe", "go.exe"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Logf("skipping %s: %v", name, err)
			continue
		}
		same, err := imageIsThisProgram(path)
		if err != nil || same {
			t.Errorf("imageIsThisProgram(%s) = %v, %v; want false with no error: it reads cleanly and is not a build of this program", path, same, err)
		}
	}
}

// failingReaderAt fails every read, as an image on a failing volume would.
type failingReaderAt struct{}

func (failingReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, windows.ERROR_READ_FAULT
}

func TestBuildOfProgramTellsAnUnreadableImageFromAForeignOne(t *testing.T) {
	if same, err := buildOfProgram(strings.NewReader("not an executable"), thisProgram()); err != nil || same {
		t.Errorf("buildOfProgram(a readable non-executable) = %v, %v; want false with no error", same, err)
	}
	if same, err := buildOfProgram(failingReaderAt{}, thisProgram()); err == nil || same {
		t.Errorf("buildOfProgram(an unreadable image) = %v, %v; want an error: nothing was learned about it", same, err)
	}
}

// denyProcessRead adds an entry to pid's own DACL refusing this user
// PROCESS_VM_READ, which leaves every other right it holds in place. It skips
// when the refusal does not take, as it does not for a caller holding
// SeDebugPrivilege.
func denyProcessRead(t *testing.T, pid int) {
	t.Helper()
	denyProcessAccess(t, pid, windows.PROCESS_VM_READ)
}

// denyProcessAccess adds an entry to pid's own DACL refusing this user the
// rights in mask, which leaves every other right it holds in place. It skips
// when the refusal does not take.
func denyProcessAccess(t *testing.T, pid int, mask uint32) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.READ_CONTROL|windows.WRITE_DAC, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening PID %d to edit its DACL: %v", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("reading the DACL of PID %d: %v", pid, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("reading the DACL of PID %d: %v", pid, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("reading this process's user: %v", err)
	}
	deny, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, dacl)
	if err != nil {
		t.Fatalf("building the refusing DACL: %v", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, deny, nil); err != nil {
		t.Fatalf("setting the refusing DACL on PID %d: %v", pid, err)
	}
	if probe, err := windows.OpenProcess(mask, false, uint32(pid)); err == nil {
		_ = windows.CloseHandle(probe)
		t.Skipf("PID %d still grants %#x after refusing it; this caller bypasses process DACLs", pid, mask)
	}
}

// TestKillRuntimeLeavesAChildItCannotClassifyAndSaysSo is the case where
// inspection and termination rights part: the watchdog mock grants the second
// and refuses the first. It must be neither exempted nor killed on an argv
// nobody read, and the teardown must not report success.
func TestKillRuntimeLeavesAChildItCannotClassifyAndSaysSo(t *testing.T) {
	root := startLiveTree(t, "start /B cmd.exe "+ManagedDoltScopeWatchdogVerb+" & ping -n 61 127.0.0.1 >NUL")
	tree := awaitDescendants(t, root.Process.Pid, func(found []descendant) bool {
		_, watchdog := withArgv1(found, ManagedDoltScopeWatchdogVerb)
		return watchdog && len(withImage(found, "ping.exe")) >= 1
	})
	watchdog, _ := withArgv1(tree, ManagedDoltScopeWatchdogVerb)
	denyProcessRead(t, watchdog.pid)
	identity, err := ProcessIdentity(root.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessIdentity(root): %v", err)
	}

	err = KillRuntime(runtime.LiveRuntime{PID: root.Process.Pid, StartIdentity: identity})
	if err == nil {
		t.Fatal("KillRuntime reported the runtime gone while a child it could not classify was left running")
	}
	if !strings.Contains(err.Error(), "PID "+strconv.Itoa(watchdog.pid)+" ") {
		t.Fatalf("KillRuntime error = %v; it must name the child it could not classify, PID %d", err, watchdog.pid)
	}
	if waitForProcessHandle(watchdog.handle, 500*time.Millisecond) {
		t.Fatalf("KillRuntime terminated PID %d on a command line it could not read", watchdog.pid)
	}
	for _, d := range withImage(tree, "ping.exe") {
		if !waitForProcessHandle(d.handle, 0) {
			t.Errorf("the runtime's helper PID %d is still running", d.pid)
		}
	}
}

func treeRowPIDs(rows []windowsProcess) []int {
	pids := make([]int, 0, len(rows))
	for _, row := range rows {
		pids = append(pids, row.pid)
	}
	return pids
}

func TestTreeChildrenFindsTheChildrenOfAHeldMember(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 100, ppid: 5, creation: 1_000, creationOK: true},
		{pid: 200, ppid: 100, creation: 2_000, creationOK: true},
		{pid: 300, ppid: 200, creation: 3_000, creationOK: true},
		{pid: 400, ppid: 100, creation: 500, creationOK: true},
	})
	children, unattributable := treeChildren(table, map[int]int64{100: 1_000}, nil, map[treeMember]bool{})
	if !slices.Equal(treeRowPIDs(children), []int{200}) || len(unattributable) != 0 {
		t.Fatalf("children = %v, unattributable = %v; want [200] and none: 300 waits for 200 to be held, and 400 predates PID 100's holder", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

// TestTreeChildrenReachesThroughAMemberAlreadyTerminated is the defect that
// left a grandchild running while teardown reported success. A member killed in
// an earlier round is gone from the table, and with it the only link its child
// had back to the root. A held handle keeps the member's PID from being reused,
// so the child can still be attributed to it.
func TestTreeChildrenReachesThroughAMemberAlreadyTerminated(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 300, ppid: 200, creation: 3_000, creationOK: true},
	})
	held := map[int]int64{100: 1_000, 200: 2_000}
	children, unattributable := treeChildren(table, held, nil, map[treeMember]bool{})
	if !slices.Equal(treeRowPIDs(children), []int{300}) || len(unattributable) != 0 {
		t.Fatalf("children = %v, unattributable = %v; want [300]: its parent was terminated, not forgotten", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

// TestTreeChildrenRefusesToAttributeThroughAnUnheldParent is the defect that
// could kill an unrelated process. The root exited before it could be held, so
// its PID may have passed through any number of later holders, and a process
// naming it could be the child of any of them.
func TestTreeChildrenRefusesToAttributeThroughAnUnheldParent(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 200, ppid: 100, creation: 6_000, creationOK: true},
	})
	blocked := []treeMember{{pid: 100, creation: 1_000}}
	children, unattributable := treeChildren(table, map[int]int64{}, blocked, map[treeMember]bool{})
	if len(children) != 0 || !slices.Equal(treeRowPIDs(unattributable), []int{200}) {
		t.Fatalf("children = %v, unattributable = %v; want PID 200 reported, not taken", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

// TestTreeChildrenLeavesTheChildrenOfALaterHolder keeps that refusal from
// spreading to processes it can place: a process now holding the unheld
// parent's PID, created no later than a row, is that row's parent.
func TestTreeChildrenLeavesTheChildrenOfALaterHolder(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 100, ppid: 5, creation: 5_000, creationOK: true},
		{pid: 200, ppid: 100, creation: 2_000, creationOK: true},
		{pid: 201, ppid: 100, creation: 6_000, creationOK: true},
	})
	blocked := []treeMember{{pid: 100, creation: 1_000}}
	children, unattributable := treeChildren(table, map[int]int64{}, blocked, map[treeMember]bool{})
	if len(children) != 0 || !slices.Equal(treeRowPIDs(unattributable), []int{200}) {
		t.Fatalf("children = %v, unattributable = %v; want only PID 200 reported: 201 is the later holder's", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

// TestTreeChildrenCannotOrderATieWithALaterHolder is the tie the later-holder
// rule must not settle. The unheld member may have started PID 200 in the same
// clock tick in which it exited and its PID passed to the later holder, so equal
// creation times do not make PID 200 the later holder's child.
func TestTreeChildrenCannotOrderATieWithALaterHolder(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 100, ppid: 5, creation: 5_000, creationOK: true},
		{pid: 200, ppid: 100, creation: 5_000, creationOK: true},
	})
	blocked := []treeMember{{pid: 100, creation: 1_000}}
	children, unattributable := treeChildren(table, map[int]int64{}, blocked, map[treeMember]bool{})
	if len(children) != 0 || !slices.Equal(treeRowPIDs(unattributable), []int{200}) {
		t.Fatalf("children = %v, unattributable = %v; want PID 200 reported: a tie with the later holder does not place it", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

// TestTreeChildrenRefusesTheChildrenOfAnUnclassifiedMember covers a blocked
// member the snapshot still shows. It held its PID when its child was created,
// so the link is certain — but whether the member was infrastructure is not,
// so neither is whether its child may be killed.
func TestTreeChildrenRefusesTheChildrenOfAnUnclassifiedMember(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 200, ppid: 100, creation: 2_000, creationOK: true},
		{pid: 300, ppid: 200, creation: 3_000, creationOK: true},
	})
	blocked := []treeMember{{pid: 200, creation: 2_000}}
	known := map[treeMember]bool{{pid: 200, creation: 2_000}: true}
	children, unattributable := treeChildren(table, map[int]int64{100: 1_000}, blocked, known)
	if len(children) != 0 || !slices.Equal(treeRowPIDs(unattributable), []int{300}) {
		t.Fatalf("children = %v, unattributable = %v; want PID 300 reported", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

func TestTreeChildrenCannotFenceARowWithoutACreationTime(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 200, ppid: 100},
	})
	children, unattributable := treeChildren(table, map[int]int64{100: 1_000}, nil, map[treeMember]bool{})
	if len(children) != 0 || !slices.Equal(treeRowPIDs(unattributable), []int{200}) {
		t.Fatalf("children = %v, unattributable = %v; want PID 200 reported, not taken or dropped", treeRowPIDs(children), treeRowPIDs(unattributable))
	}
}

func TestTreeChildrenSkipsKnownMembers(t *testing.T) {
	table := indexProcessTable([]windowsProcess{
		{pid: 200, ppid: 100, creation: 2_000, creationOK: true},
	})
	known := map[treeMember]bool{{pid: 200, creation: 2_000}: true}
	if children, _ := treeChildren(table, map[int]int64{100: 1_000}, nil, known); len(children) != 0 {
		t.Fatalf("children = %v, want none: PID 200 was already admitted", treeRowPIDs(children))
	}
}

func TestIsSupervisorArgv(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{`C:\tools\gc.exe`, "supervisor", "run"}, true},
		{[]string{"gc", "supervisor", "run", "--verbose"}, true},
		{[]string{"gc", "supervisor", "status"}, false},
		{[]string{"gc", "run", "supervisor"}, false},
		{[]string{"gc", "supervisor"}, false},
	} {
		if got := isSupervisorArgv(tc.argv); got != tc.want {
			t.Errorf("isSupervisorArgv(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}
