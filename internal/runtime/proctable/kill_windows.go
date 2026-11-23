//go:build windows

package proctable

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

// KillByPID terminates pid and waits, bounded by
// runtime.ManagedProcessReapGrace, for the process to be confirmed dead before
// returning. Already-gone processes are success. A process that outlives its
// own TerminateProcess past the reap grace yields an error so callers can
// refuse to start a replacement that would race it for the same work.
//
// There is no cooperative phase, because Windows has no PID-addressable one.
// The nearest analogue to SIGTERM is a console control event, and
// GenerateConsoleCtrlEvent is addressed to a console process GROUP, reaches
// only processes sharing this process's console, and does nothing at all for a
// service or a detached CLI — see internal/processgroup. Sending it here would
// either fail outright or hit unrelated processes that happen to share the
// group, so this path goes straight to the one mechanism that is both precise
// and reliable.
//
// It terminates that one process and nothing else. There is no Windows
// equivalent of signalling a process group by PID, and the tree-wide boundary
// — a Job Object — belongs to whoever launched the tree, not to a scanner
// holding a bare PID. Its identity fence is read here, at kill time, so it
// cannot tell the process a caller scanned from a later one holding the same
// number; runtime teardown uses KillRuntime, which fences on the identity the
// scan recorded and reaches the whole tree.
func KillByPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	// Capture the target's identity BEFORE opening it for termination, so the
	// handle below can be checked against the process the caller meant rather
	// than against itself.
	identity, err := ProcessIdentity(pid)
	if errors.Is(err, ErrProcessGone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("proctable: identifying PID %d before termination: %w", pid, err)
	}
	return killProcessWithIdentity(pid, identity, runtime.ManagedProcessReapGrace)
}

// errPIDRecycled marks a termination refused because the PID now belongs to a
// different process than the one the caller identified. The process the caller
// meant is already gone.
var errPIDRecycled = errors.New("the PID was recycled")

// maxRuntimeKillRounds bounds how many times KillRuntime goes back for
// processes the tree spawned while it was being taken down.
const maxRuntimeKillRounds = 3

// treeMemberAccess is what KillRuntime holds each member with: enough to
// terminate it, wait on it, and read the creation time that identifies it.
// treeClassifyAccess adds what reading its command line takes.
const (
	treeMemberAccess   = windows.PROCESS_TERMINATE | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	treeClassifyAccess = treeMemberAccess | windows.PROCESS_VM_READ
)

// KillRuntime terminates a runtime the process-table scan found — its root and
// every process descended from it — and confirms they are gone.
//
// It is the Windows counterpart of the Unix group kill. There the runtime's
// root leads a process group, and one signal to -pgid reaches everything it
// started. Nothing groups a tree on Windows that this process did not launch,
// so the tree is found through parent links and each member is terminated on
// its own.
//
// A parent link is trusted only through a process this call holds a handle to.
// Windows never clears a child's parent PID, and a PID is free for reuse once
// its process is gone, so a link through a parent that has exited could name
// any later holder of the number. A held handle keeps the PID from being
// reused, so a process that names a held member's PID and was created no
// earlier than it is that member's child — for as long as the handle is held,
// which is the whole call, including after the member is terminated. The root
// is held first, against the identity its scan recorded (live.StartIdentity),
// never one re-read here where a recycled PID would vouch for itself. Each
// child is held as it is found, against the creation time its snapshot row
// recorded.
//
// The tree is found before anything in it is terminated. Terminating a member
// ends whatever was waiting on it — a child reading its parent's pipe exits at
// end of file — and a child that exits before it is held takes with it the
// only link its own children had back to the root. So each round reads the
// table and holds every member it can reach, and only then terminates them,
// parents first. The next round goes back for whatever they started in the
// meantime.
//
// City infrastructure the runtime started — tmux, the managed Dolt scope
// watchdog, bd's proxy child, the supervisor — is left alone together with
// everything under it: on Unix each leads its own process group, and a group
// kill never reaches it either. Command lines are read through the held
// handle.
//
// Unlike Unix, any other process that detached from the runtime is still
// reached. That is a decision, not a blind spot: each Windows process does
// record a console process group, but Windows starts new groups implicitly —
// start and Start-Process open a new console, and with it a new group —
// where Unix takes a deliberate setsid or setpgid. Honouring that boundary
// would let ordinary helpers outlive the session they belong to.
//
// It returns nil only once a table read after every held member was confirmed
// dead finds nothing more to attribute. Anything it cannot settle is an error,
// never a success: a process naming a member that exited before it could be
// held, a process it could not classify, or a tree still spawning after
// maxRuntimeKillRounds rounds. What it can attribute is still terminated
// before such an error is returned.
//
// A descendant whose connecting parent had already exited when this call first
// read the table is beyond it: nothing links that descendant to the root any
// more. A scan made after that exit reports it as a root of its own when it
// carries the session's identity; a scan made before the exit does not, which
// is why RescanAfterTermination tells a caller to scan again.
func KillRuntime(live runtime.LiveRuntime) error {
	if live.PID <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", live.PID)
	}
	creation, err := strconv.ParseInt(live.StartIdentity, 10, 64)
	if err != nil || creation <= 0 {
		return fmt.Errorf("proctable: refusing to terminate the runtime at PID %d without the start identity its scan observed", live.PID)
	}
	tree := &runtimeTree{rootPID: live.PID, held: map[int]heldMember{}, known: map[treeMember]bool{}}
	defer tree.release()

	root := treeMember{pid: live.PID, creation: creation}
	tree.known[root] = true
	handle, held, err := holdTreeMember(root, treeMemberAccess)
	if err != nil {
		return fmt.Errorf("proctable: opening the runtime root at PID %d for termination: %w", live.PID, err)
	}
	if held {
		tree.hold(root, handle)
	} else {
		tree.blocked = append(tree.blocked, root)
	}

	for round := 0; ; round++ {
		// With nothing pending, every member held so far was confirmed dead
		// before this read, so none of them can add to the tree after it.
		quiesced := len(tree.pending) == 0
		rows, err := systemProcesses()
		if err != nil {
			return fmt.Errorf("proctable: reading the process table to terminate the runtime at PID %d: %w", live.PID, err)
		}
		admitted, admitErr := tree.admitReachable(indexProcessTable(rows), round)
		if admitErr == nil && quiesced && admitted == 0 {
			return tree.settled()
		}
		terminateErr := tree.terminatePending()
		if admitErr != nil || terminateErr != nil {
			return errors.Join(admitErr, terminateErr, tree.settled())
		}
	}
}

// testHookRuntimeMemberTerminated, when set, is called with each member's PID
// once KillRuntime has confirmed it dead. It lets a test act at the moment a
// member's death can first be observed by the processes around it.
var testHookRuntimeMemberTerminated func(pid int)

// RescanAfterTermination reports whether terminating the roots a scan found
// can leave processes a later scan reports as roots of their own, so that a
// caller confirming a session gone must scan again. Here it can: KillRuntime
// reaches a descendant only through parents it holds, so one whose connecting
// parent had already exited when it began is beyond it — and with that parent
// gone, the scan reports it as a root.
const RescanAfterTermination = true

// treeMember is one process incarnation: a PID and the creation time that
// distinguishes it from any later process given the same number.
type treeMember struct {
	pid      int
	creation int64
}

// heldMember is a member of the runtime's tree together with the handle that
// keeps its PID from being reused.
type heldMember struct {
	id     treeMember
	handle windows.Handle
}

// runtimeTree is KillRuntime's state for one call.
type runtimeTree struct {
	rootPID int
	// held are the members whose children are followed and who are
	// terminated. Their handles are held until release.
	held map[int]heldMember
	// pending are the held members not yet terminated, in the order they were
	// admitted, which puts every parent before its children.
	pending []treeMember
	// blocked are members whose children cannot be attributed: the member
	// exited before it could be held, or could not be classified.
	blocked []treeMember
	// known is every incarnation already admitted, whatever the outcome.
	known map[treeMember]bool
	// unsettled records members left running because they could not be
	// classified.
	unsettled []string
	// extra are handles held for blocked members, so that their PIDs are not
	// reused while this call runs.
	extra []windows.Handle
}

func (t *runtimeTree) heldCreations() map[int]int64 {
	creations := make(map[int]int64, len(t.held))
	for pid, member := range t.held {
		creations[pid] = member.id.creation
	}
	return creations
}

// hold makes id a held member, to be terminated with the rest of the tree.
func (t *runtimeTree) hold(id treeMember, handle windows.Handle) {
	t.held[id.pid] = heldMember{id: id, handle: handle}
	t.pending = append(t.pending, id)
}

// admitReachable admits every member table shows that this call has not met
// yet, following each new member's children in the same table, and reports how
// many it admitted.
func (t *runtimeTree) admitReachable(table processTable, round int) (int, error) {
	admitted := 0
	for {
		children, unattributable := treeChildren(table, t.heldCreations(), t.blocked, t.known)
		if len(unattributable) > 0 {
			return admitted, t.unattributableError(unattributable)
		}
		if len(children) == 0 {
			return admitted, nil
		}
		if round == maxRuntimeKillRounds {
			return admitted, fmt.Errorf("proctable: the runtime tree at PID %d was still spawning processes after %d rounds of termination",
				t.rootPID, maxRuntimeKillRounds)
		}
		for _, child := range children {
			if err := t.admit(child); err != nil {
				return admitted, err
			}
			admitted++
		}
	}
}

// admit holds and classifies one child of a held member, and — unless it is
// infrastructure — makes it a held member.
func (t *runtimeTree) admit(row windowsProcess) error {
	id := treeMember{pid: row.pid, creation: row.creation}
	t.known[id] = true
	if isInfrastructureImage(row.image) {
		return nil
	}
	handle, held, err := holdTreeMember(id, treeClassifyAccess)
	classifiable := true
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// Terminating a process and reading its memory are separate rights,
		// and a process may grant the first while refusing the second.
		handle, held, err = holdTreeMember(id, treeMemberAccess)
		classifiable = false
	}
	if err != nil {
		return fmt.Errorf("proctable: opening PID %d of the runtime tree at PID %d: %w", id.pid, t.rootPID, err)
	}
	if !held {
		t.blocked = append(t.blocked, id)
		return nil
	}
	var argv []string
	if classifiable {
		argv, err = handleArgv(handle)
		classifiable = err == nil
	}
	if !classifiable {
		t.leaveUnclassified(id, row.image, handle, "could not have its command line read")
		return nil
	}
	if IsCityInfrastructureArgv(argv) {
		_ = windows.CloseHandle(handle)
		return nil
	}
	if isSupervisorArgv(argv) {
		supervisor, err := isSupervisorImage(handle)
		if err != nil {
			t.leaveUnclassified(id, row.image, handle, fmt.Sprintf("has the supervisor's arguments but its image could not be checked (%v)", err))
			return nil
		}
		if supervisor {
			_ = windows.CloseHandle(handle)
			return nil
		}
	}
	t.hold(id, handle)
	return nil
}

// leaveUnclassified records a member whose classification could not be
// completed. Whether it is infrastructure cannot be told, so neither it nor
// what it started is touched. Keeping the handle keeps its PID from being
// reused, so its children stay recognizable as unattributable.
func (t *runtimeTree) leaveUnclassified(id treeMember, image string, handle windows.Handle, reason string) {
	t.blocked = append(t.blocked, id)
	t.extra = append(t.extra, handle)
	if !processHandleExited(handle) {
		t.unsettled = append(t.unsettled, fmt.Sprintf("PID %d (%s) %s, so it may be city infrastructure; it was left running", id.pid, image, reason))
	}
}

// terminatePending terminates the held members not yet terminated, in the
// order they were admitted, and confirms each is gone. A member that cannot be
// confirmed gone does not stop the rest.
func (t *runtimeTree) terminatePending() error {
	var failures []error
	for _, id := range t.pending {
		if err := terminateHeldProcess(t.held[id.pid].handle, id.pid, runtime.ManagedProcessReapGrace); err != nil {
			failures = append(failures, err)
			continue
		}
		if testHookRuntimeMemberTerminated != nil {
			testHookRuntimeMemberTerminated(id.pid)
		}
	}
	t.pending = nil
	if len(failures) > 0 {
		return fmt.Errorf("proctable: terminating the runtime tree at PID %d: %w", t.rootPID, errors.Join(failures...))
	}
	return nil
}

func (t *runtimeTree) unattributableError(rows []windowsProcess) error {
	details := make([]string, 0, len(rows))
	for _, row := range rows {
		details = append(details, fmt.Sprintf("PID %d (%s), child of PID %d", row.pid, row.image, row.ppid))
	}
	return fmt.Errorf("proctable: the runtime tree at PID %d cannot be confirmed gone: %s: each names a parent this call could not hold or classify, or has no creation time to fence it with, so it may belong to the runtime or to a later holder of that PID; left running",
		t.rootPID, strings.Join(details, "; "))
}

// settled reports what the walk had to leave running, once nothing more can be
// attributed.
func (t *runtimeTree) settled() error {
	if len(t.unsettled) == 0 {
		return nil
	}
	return fmt.Errorf("proctable: the runtime tree at PID %d cannot be confirmed gone: %s", t.rootPID, strings.Join(t.unsettled, "; "))
}

func (t *runtimeTree) release() {
	for _, member := range t.held {
		_ = windows.CloseHandle(member.handle)
	}
	for _, handle := range t.extra {
		_ = windows.CloseHandle(handle)
	}
}

// holdTreeMember opens id's PID and returns the handle when it still names that
// incarnation. held is false when the process is gone or its PID has passed to
// another process; either way that incarnation can no longer be held.
func holdTreeMember(id treeMember, access uint32) (windows.Handle, bool, error) {
	handle, err := windows.OpenProcess(access, false, uint32(id.pid))
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	created, err := handleCreationTime(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return 0, false, fmt.Errorf("proctable: confirming the identity of PID %d: %w", id.pid, err)
	}
	if created != id.creation {
		_ = windows.CloseHandle(handle)
		return 0, false, nil
	}
	return handle, true, nil
}

// treeChildren returns the rows of table that are children of a held member and
// not yet known, and the rows whose parent link runs through a member that
// could not be held.
//
// A row is a held member's child when it names the member's PID and was created
// no earlier than the member: the held handle kept that PID from passing to
// anyone else. A row naming a blocked member belongs to nobody this walk can
// name — not even when the snapshot still shows the blocked member, because
// whether that member was infrastructure is unknown — unless the PID has since
// passed to a later process created before the row, which makes the row that
// process's child and not the tree's. A creation time equal to that process's
// orders nothing: the member may have started the row in the same clock tick
// in which it exited and gave up its PID. A row naming a member without a
// creation time of its own cannot be fenced at all.
func treeChildren(table processTable, held map[int]int64, blocked []treeMember, known map[treeMember]bool) (children, unattributable []windowsProcess) {
	earliestBlocked := make(map[int]int64, len(blocked))
	latestBlocked := make(map[int]int64, len(blocked))
	for _, member := range blocked {
		if creation, ok := earliestBlocked[member.pid]; !ok || member.creation < creation {
			earliestBlocked[member.pid] = member.creation
		}
		if creation, ok := latestBlocked[member.pid]; !ok || member.creation > creation {
			latestBlocked[member.pid] = member.creation
		}
	}
	for _, row := range table.rows {
		if row.pid <= 0 || row.ppid <= 0 || row.ppid == row.pid {
			continue
		}
		memberCreation, isHeld := held[row.ppid]
		blockedCreation, isBlocked := earliestBlocked[row.ppid]
		if !isHeld && !isBlocked {
			continue
		}
		if !row.creationOK {
			unattributable = append(unattributable, row)
			continue
		}
		if isHeld && row.creation >= memberCreation {
			if !known[treeMember{pid: row.pid, creation: row.creation}] {
				children = append(children, row)
			}
			continue
		}
		if !isBlocked || row.creation < blockedCreation {
			continue
		}
		if holder, ok := table.byPID[row.ppid]; ok && holder.creationOK &&
			holder.creation > latestBlocked[row.ppid] && holder.creation < row.creation {
			continue
		}
		unattributable = append(unattributable, row)
	}
	return children, unattributable
}

// isSupervisorArgv reports whether argv has the shape of gc's supervisor, which
// runs as `gc supervisor run` with an agent's session identity removed from
// its environment. On Unix it leads its own process group, so no agent's
// teardown reaches it; here nothing but this test and isSupervisorImage keeps a
// walk that started below it from doing so.
func isSupervisorArgv(argv []string) bool {
	return len(argv) >= 3 && argv[1] == "supervisor" && argv[2] == "run"
}

// thisProgram is the main package path this binary's build information
// records, or "" when it records none.
var thisProgram = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Path
	}
	return ""
})

// isSupervisorImage reports whether the process behind handle runs a build of
// the program running this code. gc's supervisor is gc re-executed, so its
// image is a build of gc wherever it was installed and whatever it was named,
// while any process at all can be given the supervisor's arguments.
func isSupervisorImage(handle windows.Handle) (bool, error) {
	path, err := processImagePath(handle)
	if err != nil {
		return false, err
	}
	return imageIsThisProgram(path)
}

// imageIsThisProgram reports whether the executable at path is a build of the
// program running this code.
func imageIsThisProgram(path string) (bool, error) {
	program := thisProgram()
	if program == "" {
		return false, errors.New("this binary records no build information to recognize gc by")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("opening its image: %w", err)
	}
	defer file.Close() //nolint:errcheck
	same, err := buildOfProgram(file, program)
	if err != nil {
		return false, fmt.Errorf("reading its image %s: %w", path, err)
	}
	return same, nil
}

// buildOfProgram reports whether image is a Go executable built from the main
// package program. An image that reads cleanly but carries no Go build
// information is not; one that cannot be read is an error, since then nothing
// is known about it.
func buildOfProgram(image io.ReaderAt, program string) (bool, error) {
	reader := &readErrorRecorder{reader: image}
	info, err := buildinfo.Read(reader)
	if reader.err != nil {
		return false, reader.err
	}
	if err != nil {
		return false, nil
	}
	return info.Path == program, nil
}

// readErrorRecorder remembers the first read failure other than end of file.
// debug/buildinfo reports a file it could not read the same way as one that is
// not a Go executable; this tells the two apart.
type readErrorRecorder struct {
	reader io.ReaderAt
	err    error
}

func (r *readErrorRecorder) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.reader.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// maxImagePathLength is the longest path, in UTF-16 code units, that Windows
// can report for a process image.
const maxImagePathLength = 32768

// processImagePath returns the path of the executable behind handle, which
// must carry PROCESS_QUERY_LIMITED_INFORMATION.
func processImagePath(handle windows.Handle) (string, error) {
	for size := uint32(windows.MAX_PATH); ; size *= 2 {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size >= maxImagePathLength {
			return "", fmt.Errorf("reading its image path: %w", err)
		}
	}
}

// killProcessWithIdentity terminates pid only if it is still the process whose
// start identity is wantIdentity.
//
// The handle is opened first and the identity is checked ON THAT HANDLE. A
// handle pins the kernel's process object, so once it is open the number can be
// recycled without the handle ever following it — which closes the window
// between "read the creation time" and "terminate", the window in which a bare
// PID check is worthless. A target whose identity no longer matches is refused
// rather than killed: that PID now belongs to a process this caller knows
// nothing about.
//
// The identity is taken as an argument rather than read here so the refusal
// itself is testable. A version that re-reads its own identity agrees with
// itself by construction and passes every test while shipping the bug.
func killProcessWithIdentity(pid int, wantIdentity string, reapGrace time.Duration) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	if wantIdentity == "" {
		return fmt.Errorf("proctable: refusing to terminate PID %d without a start identity to fence it against", pid)
	}

	const access = windows.PROCESS_TERMINATE | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return nil
		}
		return fmt.Errorf("proctable: opening PID %d for termination: %w", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	creation, err := handleCreationTime(handle)
	if err != nil {
		return fmt.Errorf("proctable: confirming the identity of PID %d before termination: %w", pid, err)
	}
	if gotIdentity := strconv.FormatInt(creation, 10); gotIdentity != wantIdentity {
		return fmt.Errorf(
			"proctable: refusing to terminate PID %d: its start identity is %s, not %s: %w",
			pid, gotIdentity, wantIdentity, errPIDRecycled)
	}
	return terminateHeldProcess(handle, pid, reapGrace)
}

// terminateHeldProcess terminates the process behind handle and waits, bounded
// by reapGrace, for it to be confirmed dead. A process that has already exited,
// or is exiting and refuses termination, is success.
func terminateHeldProcess(handle windows.Handle, pid int, reapGrace time.Duration) error {
	if processHandleExited(handle) {
		return nil
	}
	if err := terminateProcess(handle, 1); err != nil {
		// A process already on its way out refuses termination; that is the
		// outcome the caller wanted, not a failure.
		if waitForProcessHandleFn(handle, reapGrace) {
			return nil
		}
		return fmt.Errorf("proctable: terminating PID %d: %w", pid, err)
	}
	if waitForProcessHandleFn(handle, reapGrace) {
		return nil
	}
	return fmt.Errorf("proctable: PID %d was still running %s after TerminateProcess (not confirmed dead)", pid, reapGrace)
}

var (
	terminateProcess       = windows.TerminateProcess
	waitForProcessHandleFn = waitForProcessHandle
)

func processHandleExited(handle windows.Handle) bool {
	return waitForProcessHandleFn(handle, 0)
}

// waitForProcessHandle reports whether the process behind handle has exited,
// waiting up to timeout. A failed wait reports false: an observation that did
// not happen must never be read as a death that did.
func waitForProcessHandle(handle windows.Handle, timeout time.Duration) bool {
	milliseconds := timeout.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	state, err := windows.WaitForSingleObject(handle, uint32(milliseconds))
	if err != nil {
		return false
	}
	return state == windows.WAIT_OBJECT_0
}

// KillByPIDIdentity is KillByPID bound to the process a scan reported: it
// terminates pid's tree only while pid's creation time still equals identity,
// so a PID recycled since the scan is never terminated. An empty identity
// falls back to KillByPID's own capture, as on Unix.
func KillByPIDIdentity(pid int, identity string) error {
	if identity == "" {
		return KillByPID(pid)
	}
	return KillRuntime(runtime.LiveRuntime{PID: pid, StartIdentity: identity})
}
