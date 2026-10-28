//go:build windows

package proctable

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	// helperGateVar follows the GC_*_HELPER convention internal/testenv leaves
	// unscrubbed, so the trampoline below survives that package's init().
	helperGateVar = "GC_PROCTABLE_WINDOWS_HELPER"

	helperReadyLine = "proctable-windows-helper-ready"

	// helperEpoch is the GC_RUNTIME_EPOCH every helper carries, so a test can
	// assert the scanner read the environment rather than guessed at it.
	helperEpoch = 11

	// helperSelfDestruct bounds a helper that outlives the test that started
	// it. The child is a copy of this test binary, so its own -test.timeout is
	// a backstop the parent cannot lose control of: a helper orphaned by a
	// panicking or killed parent reaps itself instead of sitting on the machine.
	helperSelfDestruct = 90 * time.Second

	helperReadyTimeout = 30 * time.Second
	helperStopGrace    = 5 * time.Second
)

// TestProctableWindowsHelperProcess is a trampoline, not a test.
//
// The live tests in this package re-execute this binary to obtain a real
// Windows process they own outright: one whose environment they choose, whose
// lifetime they control, and which nothing else on the machine has a stake in.
// Every process this package's tests inspect, signal or terminate comes from
// here.
func TestProctableWindowsHelperProcess(t *testing.T) {
	if os.Getenv(helperGateVar) != "1" {
		t.Skip("helper trampoline; runs only when this package re-executes the test binary")
	}
	fmt.Println(helperReadyLine)
	// Block until the parent closes stdin. A cooperative exit keeps ordinary
	// teardown clear of TerminateProcess, so a forced kill in a test run always
	// means some test asked for one.
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// helperProcess is one spawned child, owned end to end by the test that
// started it.
type helperProcess struct {
	t         *testing.T
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	pid       int
	sessionID string
	city      string
	identity  string
	exited    chan struct{}
	stopOnce  sync.Once
}

// startHelper spawns a child carrying sessionID and returns once it is running.
//
// Reaping is registered with t.Cleanup the moment the process exists and before
// anything else can fail, so every exit from here on — including t.Fatal inside
// this function — still takes the child down with it.
func startHelper(t *testing.T, sessionID, city string) *helperProcess {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating the helper readiness pipe: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestProctableWindowsHelperProcess$",
		"-test.timeout="+helperSelfDestruct.String(),
	)
	cmd.Dir = t.TempDir()
	cmd.Stdout = writer
	cmd.Env = append(os.Environ(),
		helperGateVar+"=1",
		// internal/testenv scrubs GC_SESSION_ID out of every test binary at
		// init, this child included. That scrub is os.Unsetenv, which rewrites
		// the very environment block the scanner reads, so without the
		// passthrough the helper would start with the variable and lose it
		// before any scan could see it.
		"GC_TESTENV_PASSTHROUGH=GC_SESSION_ID,GC_CITY_PATH",
		"GC_SESSION_ID="+sessionID,
		"GC_CITY_PATH="+city,
		"GC_RUNTIME_EPOCH="+strconv.Itoa(helperEpoch),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = writer.Close()
		t.Fatalf("opening the helper stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		t.Fatalf("starting the helper process: %v", err)
	}
	// The child now holds the only remaining write end, so the reader below
	// sees EOF exactly when the child exits.
	_ = writer.Close()

	helper := &helperProcess{
		t:         t,
		cmd:       cmd,
		stdin:     stdin,
		pid:       cmd.Process.Pid,
		sessionID: sessionID,
		city:      city,
		exited:    make(chan struct{}),
	}
	t.Cleanup(helper.stop)
	go func() {
		_ = cmd.Wait()
		close(helper.exited)
	}()

	helper.awaitReady(reader)
	identity, err := ProcessIdentity(helper.pid)
	if err != nil {
		t.Fatalf("reading the start identity of helper PID %d: %v", helper.pid, err)
	}
	helper.identity = identity
	return helper
}

func (h *helperProcess) awaitReady(reader *os.File) {
	h.t.Helper()
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == helperReadyLine {
				ready <- nil
				return
			}
		}
		if err := scanner.Err(); err != nil {
			ready <- err
			return
		}
		ready <- errors.New("the helper exited before it signalled readiness")
	}()
	select {
	case err := <-ready:
		if err != nil {
			h.t.Fatalf("helper PID %d never became ready: %v", h.pid, err)
		}
	case <-time.After(helperReadyTimeout):
		h.t.Fatalf("helper PID %d did not signal readiness within %s", h.pid, helperReadyTimeout)
	}
}

// stop reaps the helper and is idempotent. It is also correct after a test has
// already terminated the child by other means: what matters is that the process
// is gone and waited for before the test binary exits, whichever path got it
// there.
func (h *helperProcess) stop() {
	h.stopOnce.Do(func() {
		_ = h.stdin.Close()
		select {
		case <-h.exited:
			return
		case <-time.After(helperStopGrace):
		}
		_ = h.cmd.Process.Kill()
		select {
		case <-h.exited:
		case <-time.After(helperStopGrace):
			h.t.Errorf("helper PID %d survived Kill and may have leaked", h.pid)
		}
	})
}

// running reports whether the helper has yet to be reaped.
func (h *helperProcess) running() bool {
	select {
	case <-h.exited:
		return false
	default:
		return true
	}
}

// awaitExit waits for the helper to be reaped, so a test that asked for a kill
// can assert the kill happened rather than race it.
func (h *helperProcess) awaitExit(timeout time.Duration) bool {
	select {
	case <-h.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

func uniqueSessionID(t *testing.T, label string) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generating a unique session id: %v", err)
	}
	return fmt.Sprintf("proctable-%s-%x", label, suffix)
}

// startArgvMock starts a process whose argv, as rootArgv reads it, is
// ["cmd.exe", args...], and which does nothing else. cmd.exe ignores arguments
// that are not its own switches and reads commands from stdin, so it idles
// until stop closes that pipe. It stands in for city infrastructure without
// running any.
func startArgvMock(t *testing.T, args ...string) *helperProcess {
	t.Helper()
	cmd := exec.Command("cmd.exe", args...)
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening the argv mock's stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting cmd.exe %q: %v", args, err)
	}
	mock := &helperProcess{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		pid:    cmd.Process.Pid,
		exited: make(chan struct{}),
	}
	t.Cleanup(mock.stop)
	go func() {
		_ = cmd.Wait()
		close(mock.exited)
	}()
	identity, err := ProcessIdentity(mock.pid)
	if err != nil {
		t.Fatalf("reading the start identity of argv mock PID %d: %v", mock.pid, err)
	}
	mock.identity = identity
	return mock
}

func encodeEnvironmentBlock(entries ...string) []uint16 {
	var units []uint16
	for _, entry := range entries {
		units = append(units, utf16.Encode([]rune(entry))...)
		units = append(units, 0)
	}
	return append(units, 0)
}

func TestParseEnvironmentBlock(t *testing.T) {
	block := encodeEnvironmentBlock(
		"GC_SESSION_ID=abc",
		"=C:=C:\\work",
		"Path=C:\\windows;C:\\tools",
		"EMPTY=",
		"GC_CITY_PATH=C:\\city",
	)
	// Anything past the terminating double NUL is not part of the block.
	block = append(block, encodeEnvironmentBlock("GC_SESSION_ID=leaked")...)

	env := parseEnvironmentBlock(block)
	for key, want := range map[string]string{
		"GC_SESSION_ID": "abc",
		"PATH":          "C:\\windows;C:\\tools",
		"EMPTY":         "",
		"GC_CITY_PATH":  "C:\\city",
	} {
		if got, ok := env[key]; !ok || got != want {
			t.Errorf("env[%q] = %q (present %v), want %q", key, got, ok, want)
		}
	}
	if len(env) != 4 {
		t.Errorf("parsed %d variables from the block, want 4: %v", len(env), env)
	}
}

func TestParseEnvironmentBlockUppercasesKeys(t *testing.T) {
	env := parseEnvironmentBlock(encodeEnvironmentBlock("gc_session_id=lowercase"))
	if got := env["GC_SESSION_ID"]; got != "lowercase" {
		t.Fatalf("env[GC_SESSION_ID] = %q; Windows variable names are case-insensitive, so a lower-case spelling must still be found", got)
	}
}

// TestProcessTableFromRowsRefusesAnUnreadableSelf pins the fail-closed rule: a
// read that could not even see this process proves nothing about the host, and
// must not be handed back as a table.
func TestProcessTableFromRowsRefusesAnUnreadableSelf(t *testing.T) {
	rows := []windowsProcess{
		{pid: 100, ppid: 1, image: "other.exe"},
		{pid: 200, ppid: 100, image: "self.exe"},
	}
	table, err := processTableFromRows(rows, 200)
	if err == nil {
		t.Fatalf("processTableFromRows accepted a table with no readable environment at all: %d rows", len(table.rows))
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want one wrapping ErrUnsupported so callers can tell it from an empty host", err)
	}
}

func TestProcessTableFromRowsRefusesAnEmptySnapshot(t *testing.T) {
	if _, err := processTableFromRows(nil, 200); err == nil {
		t.Fatal("processTableFromRows accepted an empty process table; the host always has processes")
	}
}

// TestProcessTableFromRowsAcceptsUnreadablePeers is the other half of the same
// rule: processes this user may not open are ordinary on Windows and must not
// cost the caller the rest of the table.
func TestProcessTableFromRowsAcceptsUnreadablePeers(t *testing.T) {
	rows := []windowsProcess{
		{pid: 4, ppid: 0, image: "System"},
		{pid: 100, ppid: 1, image: "protected.exe"},
		{pid: 200, ppid: 100, image: "self.exe", env: map[string]string{"GC_SESSION_ID": "s"}, envOK: true},
	}
	table, err := processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows rejected a table whose own process was readable: %v", err)
	}
	if len(table.rows) != len(rows) {
		t.Fatalf("table reports %d rows, want all %d including the unreadable ones", len(table.rows), len(rows))
	}
	if _, ok := table.byPID[100]; !ok {
		t.Error("an unreadable process was dropped from the table; it still has a usable PID, PPID and image name")
	}
}

// TestProcessTableParentRejectsAParentCreatedAfterItsChild is the PID-reuse
// defence. Windows leaves a dead parent's PID in the child's record, so without
// the creation-time test a child adopts whatever new process inherited the
// number.
func TestProcessTableParentRejectsAParentCreatedAfterItsChild(t *testing.T) {
	child := windowsProcess{pid: 100, ppid: 200, creation: 1_000, creationOK: true}
	table := indexProcessTable([]windowsProcess{
		child,
		{pid: 200, ppid: 0, creation: 5_000, creationOK: true},
	})
	if parent, ok := table.parent(child); ok {
		t.Fatalf("parent = %+v, want no parent: PID 200 was created after its supposed child and cannot be it", parent)
	}
}

func TestProcessTableParentAcceptsAnOlderParent(t *testing.T) {
	child := windowsProcess{pid: 100, ppid: 200, creation: 5_000, creationOK: true}
	table := indexProcessTable([]windowsProcess{
		child,
		{pid: 200, ppid: 0, creation: 1_000, creationOK: true},
	})
	parent, ok := table.parent(child)
	if !ok || parent.pid != 200 {
		t.Fatalf("parent = %+v (found %v), want PID 200", parent, ok)
	}
}

func TestProcessTableParentRejectsMissingAndSelfReferentialParents(t *testing.T) {
	orphan := windowsProcess{pid: 100, ppid: 900, creation: 1_000, creationOK: true}
	loop := windowsProcess{pid: 101, ppid: 101, creation: 1_000, creationOK: true}
	table := indexProcessTable([]windowsProcess{orphan, loop})
	if _, ok := table.parent(orphan); ok {
		t.Error("a PPID that is not in the table must not resolve to a parent")
	}
	if _, ok := table.parent(loop); ok {
		t.Error("a self-referential PPID must not resolve to a parent")
	}
}

// TestProcessTableParentRejectsUnknownCreationTimes pins that an unknown
// creation time certifies nothing. The creation-time test is the only thing
// that tells a real parent from a later process holding its old PID, so a link
// it could not apply is not a link.
func TestProcessTableParentRejectsUnknownCreationTimes(t *testing.T) {
	for name, rows := range map[string][]windowsProcess{
		"unknown child":  {{pid: 100, ppid: 200}, {pid: 200, creation: 1_000, creationOK: true}},
		"unknown parent": {{pid: 100, ppid: 200, creation: 5_000, creationOK: true}, {pid: 200}},
	} {
		table := indexProcessTable(rows)
		if parent, ok := table.parent(rows[0]); ok {
			t.Errorf("%s: parent = %+v, want no parent when a creation time is unknown", name, parent)
		}
	}
}

// TestEnrichProcessRefusesAHandleFromALaterIncarnation is the regression test
// for hybrid rows. The snapshot fixes which process a row describes; a handle
// opened by PID afterwards may reach a later process that inherited the
// number, and reading through it would attach that process's environment to
// this row's parent and image.
func TestEnrichProcessRefusesAHandleFromALaterIncarnation(t *testing.T) {
	identity, err := ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatalf("ProcessIdentity(self): %v", err)
	}
	creation, err := strconv.ParseInt(identity, 10, 64)
	if err != nil {
		t.Fatalf("parsing identity %q: %v", identity, err)
	}

	stale := windowsProcess{pid: os.Getpid(), image: "earlier.exe", creation: creation - 1, creationOK: true}
	enrichProcess(&stale)
	if stale.envOK || stale.env != nil {
		t.Fatalf("a row whose snapshot identity does not match the handle was given the handle's environment (%d variables)", len(stale.env))
	}
	if stale.inspection != inspectionGone {
		t.Fatalf("inspection = %v, want inspectionGone: the process the snapshot saw is no longer at that PID", stale.inspection)
	}

	current := windowsProcess{pid: os.Getpid(), image: "self.exe", creation: creation, creationOK: true}
	enrichProcess(&current)
	if !current.envOK || current.inspection != inspected {
		t.Fatalf("a row matching the handle was not read: envOK=%v inspection=%v err=%v", current.envOK, current.inspection, current.inspectionErr)
	}
}

// TestProcessTableReportsProcessesItCouldNotInspect pins which unreadable rows
// make a scan incomplete. A process that exited, or that this user may not
// open at all, cannot be one of this user's sessions. A process that was
// opened and is still running but could not be read might be exactly the one
// a caller is looking for.
func TestProcessTableReportsProcessesItCouldNotInspect(t *testing.T) {
	rows := []windowsProcess{
		{pid: 200, image: "self.exe", env: map[string]string{}, envOK: true, inspection: inspected},
		{pid: 300, image: "exited.exe", inspection: inspectionGone},
		{pid: 400, image: "other-user.exe", inspection: inspectionDenied},
	}
	table, err := processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows: %v", err)
	}
	if err := table.incomplete(); err != nil {
		t.Fatalf("exited and inaccessible processes made the scan incomplete: %v", err)
	}

	rows = append(rows, windowsProcess{pid: 500, image: "agent.exe", inspection: inspectionFailed, inspectionErr: errors.New("partial copy")})
	table, err = processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows: %v", err)
	}
	err = table.incomplete()
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("incomplete() = %v, want ErrScanIncomplete for a process that was opened but not read", err)
	}
	if !strings.Contains(err.Error(), "PID 500") || !strings.Contains(err.Error(), "partial copy") {
		t.Fatalf("incomplete() = %q, want it to name the process and the reason", err)
	}
}

// enrichedRow reads the host table and returns pid's row as enrichment leaves
// it.
func enrichedRow(t *testing.T, pid int) windowsProcess {
	t.Helper()
	rows, err := systemProcesses()
	if err != nil {
		t.Fatalf("systemProcesses: %v", err)
	}
	for _, row := range rows {
		if row.pid == pid {
			enrichProcess(&row)
			return row
		}
	}
	t.Fatalf("PID %d is not in the process table", pid)
	return windowsProcess{}
}

// TestAScanIsIncompleteWhileThisUsersProcessRefusesTheRead covers the case
// where this user's own process, at this process's integrity, has a DACL that
// refuses the read. A refusal is not evidence of another account, so the
// scan must not claim to be complete while that process could be the session.
func TestAScanIsIncompleteWhileThisUsersProcessRefusesTheRead(t *testing.T) {
	defer allowLiveWindowsScanForTesting()()

	sessionID := uniqueSessionID(t, "refusing")
	helper := startHelper(t, sessionID, t.TempDir())
	found, err := ScanBySessionID(sessionID)
	if err != nil || len(found) != 1 {
		t.Fatalf("before the helper refuses the read: ScanBySessionID = %d root(s), %v; want the helper from a complete scan", len(found), err)
	}

	denyProcessRead(t, helper.pid)
	if row := enrichedRow(t, helper.pid); row.inspection != inspectionFailed {
		t.Errorf("this user's process refusing the read was classified %v (err %v), want inspectionFailed", row.inspection, row.inspectionErr)
	}
	found, err = ScanBySessionID(sessionID)
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("ScanBySessionID = %d root(s), %v; want ErrScanIncomplete while this user's own process refuses the read", len(found), err)
	}
	if !strings.Contains(err.Error(), "PID "+strconv.Itoa(helper.pid)+" ") {
		t.Fatalf("ScanBySessionID error = %v; it must name PID %d", err, helper.pid)
	}
}

// TestAScanIsIncompleteWhileThisUsersProcessRefusesEvenAQuery takes the same
// refusal one right further. A DACL that also refuses the query-limited open
// hides the token, and with it the account; the process object's own security
// descriptor, which its owner can always read, still names it.
func TestAScanIsIncompleteWhileThisUsersProcessRefusesEvenAQuery(t *testing.T) {
	defer allowLiveWindowsScanForTesting()()

	sessionID := uniqueSessionID(t, "refusing-query")
	helper := startHelper(t, sessionID, t.TempDir())
	denyProcessAccess(t, helper.pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if row := enrichedRow(t, helper.pid); row.inspection != inspectionFailed {
		t.Errorf("this user's process refusing even a query was classified %v (err %v), want inspectionFailed", row.inspection, row.inspectionErr)
	}
	found, err := ScanBySessionID(sessionID)
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("ScanBySessionID = %d root(s), %v; want ErrScanIncomplete while this user's own process refuses even a query", len(found), err)
	}
	if !strings.Contains(err.Error(), "PID "+strconv.Itoa(helper.pid)+" ") {
		t.Fatalf("ScanBySessionID error = %v; it must name PID %d", err, helper.pid)
	}
}

// TestProcessObjectOwnerReadsThisUsersProcess pins the descriptor read on a
// process nothing refuses: this user, at this process's own integrity.
func TestProcessObjectOwnerReadsThisUsersProcess(t *testing.T) {
	helper := startHelper(t, uniqueSessionID(t, "descriptor"), t.TempDir())
	owner, err := processObjectOwner(helper.pid)
	if err != nil {
		t.Fatalf("processObjectOwner(%d): %v", helper.pid, err)
	}
	self, err := currentProcessOwner()
	if err != nil {
		t.Fatalf("currentProcessOwner: %v", err)
	}
	if !owner.user.Equals(self.user) || owner.integrity != self.integrity {
		t.Fatalf("processObjectOwner(%d) = %v at %#x; want this user, %v, at %#x", helper.pid, owner.user, owner.integrity, self.user, self.integrity)
	}
}

// TestElevatedSinceReturnsOnlyElevatedProcessesCreatedSince pins the filter the
// session gate relies on.
func TestElevatedSinceReturnsOnlyElevatedProcessesCreatedSince(t *testing.T) {
	since := time.Unix(0, 1_000)
	rows := []windowsProcess{
		{pid: 10, image: "before.exe", creation: 999, creationOK: true, inspection: inspectionElevated},
		{pid: 11, image: "at.exe", creation: 1_000, creationOK: true, inspection: inspectionElevated},
		{pid: 12, image: "after.exe", creation: 2_000, creationOK: true, inspection: inspectionElevated},
		{pid: 13, image: "denied.exe", creation: 2_000, creationOK: true, inspection: inspectionDenied},
		{pid: 14, image: "foreign.exe", creation: 2_000, creationOK: true, inspection: inspectionForeign},
		{pid: 15, image: "read.exe", creation: 2_000, creationOK: true, inspection: inspected},
		{pid: 16, image: "unstamped.exe", inspection: inspectionElevated},
	}
	var got []int
	for _, record := range elevatedSince(rows, since) {
		got = append(got, record.PID)
		if !record.SessionIDUnknown {
			t.Errorf("PID %d returned without SessionIDUnknown; its environment was never read", record.PID)
		}
	}
	if len(got) != 3 || got[0] != 11 || got[1] != 12 || got[2] != 16 {
		t.Fatalf("elevatedSince = %v, want [11 12 16]: elevated, and not shown to predate since", got)
	}
}

// TestCurrentBootIsFixedAndPrecedesThisProcess pins what the boot identity
// gives within one boot: the same value every time, from before this process
// existed.
func TestCurrentBootIsFixedAndPrecedesThisProcess(t *testing.T) {
	first, err := CurrentBoot()
	if err != nil {
		t.Fatalf("CurrentBoot: %v", err)
	}
	second, err := CurrentBoot()
	if err != nil {
		t.Fatalf("CurrentBoot: %v", err)
	}
	if first <= 0 || first != second {
		t.Fatalf("CurrentBoot = %d, then %d; want one positive value for the whole boot", first, second)
	}
	_, created, err := CurrentProcessStart()
	if err != nil {
		t.Fatalf("CurrentProcessStart: %v", err)
	}
	if first >= created {
		t.Fatalf("CurrentBoot = %d, not before this process's creation at %d", first, created)
	}
}

// TestProcessRunningFollowsOneIncarnation pins the three answers a recorded
// process can get: running, replaced by a process created at another time, and
// exited while a handle still keeps its PID reserved.
func TestProcessRunningFollowsOneIncarnation(t *testing.T) {
	pid, created, err := CurrentProcessStart()
	if err != nil {
		t.Fatalf("CurrentProcessStart: %v", err)
	}
	if running, err := ProcessRunning(pid, created); err != nil || !running {
		t.Fatalf("ProcessRunning(this process) = %t, %v; want running", running, err)
	}
	if running, err := ProcessRunning(pid, created+1); err != nil || running {
		t.Fatalf("ProcessRunning(this PID, another creation time) = %t, %v; want not running", running, err)
	}

	cmd := exec.Command("cmd.exe", "/d", "/c", "set /p _=")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	held, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("holding the child: %v", err)
	}
	defer windows.CloseHandle(held) //nolint:errcheck
	childCreated, err := handleCreationTime(held)
	if err != nil {
		t.Fatalf("reading the child's creation time: %v", err)
	}
	if running, err := ProcessRunning(cmd.Process.Pid, childCreated); err != nil || !running {
		t.Fatalf("ProcessRunning(the waiting child) = %t, %v; want running", running, err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		// set /p exits 1 at end of input; only a wait that failed matters.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("waiting for the child: %v", err)
		}
	}
	if running, err := ProcessRunning(cmd.Process.Pid, childCreated); err != nil || running {
		t.Fatalf("ProcessRunning(the exited child, still held) = %t, %v; want not running", running, err)
	}
}

func TestElevatedProcessesSinceReadsTheLiveTable(t *testing.T) {
	level, err := CurrentIntegrity()
	if err != nil {
		t.Fatalf("CurrentIntegrity: %v", err)
	}
	if level < 0x1000 || level > 0x4000 {
		t.Fatalf("CurrentIntegrity = %#x, want a mandatory level between low and system", level)
	}
	found, err := ElevatedProcessesSince(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ElevatedProcessesSince: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("ElevatedProcessesSince(an hour from now) = %v, want nothing", found)
	}
}

// TestSnapshotReturnsWhatItCouldNotInspect pins the snapshot's half of an
// incomplete read: every process is still returned, the ones that could not be
// read are marked as having an unknown session ID rather than none, and the
// incompleteness comes back with them.
func TestSnapshotReturnsWhatItCouldNotInspect(t *testing.T) {
	rows := []windowsProcess{
		{pid: 200, image: "self.exe", env: map[string]string{"GC_SESSION_ID": "sess-self"}, envOK: true, inspection: inspected},
		{pid: 300, image: "exited.exe", inspection: inspectionGone},
		{pid: 400, image: "other-user.exe", inspection: inspectionDenied},
	}
	table, err := processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows: %v", err)
	}
	records, err := snapshotFromTable(table)
	if err != nil {
		t.Fatalf("snapshot of a fully inspected table = %v, want nil", err)
	}
	for _, record := range records {
		if record.SessionIDUnknown {
			t.Errorf("PID %d marked SessionIDUnknown although nothing was left unread", record.PID)
		}
	}

	rows = append(rows, windowsProcess{pid: 500, image: "agent.exe", inspection: inspectionFailed, inspectionErr: errors.New("partial copy")})
	table, err = processTableFromRows(rows, 200)
	if err != nil {
		t.Fatalf("processTableFromRows: %v", err)
	}
	records, err = snapshotFromTable(table)
	if !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("snapshot with an unread process = %v, want ErrScanIncomplete", err)
	}
	if len(records) != len(rows) {
		t.Fatalf("snapshot returned %d records for %d processes; an incomplete read must still return all of them", len(records), len(rows))
	}
	for _, record := range records {
		switch record.PID {
		case 200:
			if record.SessionID != "sess-self" || record.SessionIDUnknown {
				t.Errorf("inspected PID 200 = %+v, want its session ID read", record)
			}
		case 500:
			if !record.SessionIDUnknown || record.SessionID != "" {
				t.Errorf("unread PID 500 = %+v, want SessionIDUnknown and no session ID", record)
			}
		default:
			if record.SessionIDUnknown {
				t.Errorf("PID %d marked SessionIDUnknown; only a process that was opened but not read is", record.PID)
			}
		}
	}
}

func TestIsInfrastructureImage(t *testing.T) {
	for image, want := range map[string]bool{
		"tmux.exe":   true,
		"TMUX.EXE":   true,
		"tmux":       true,
		"claude.exe": false,
		"gc.exe":     false,
		"tmuxwrap":   false,
		"":           false,
	} {
		if got := isInfrastructureImage(image); got != want {
			t.Errorf("isInfrastructureImage(%q) = %v, want %v", image, got, want)
		}
	}
}
