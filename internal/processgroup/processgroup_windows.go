//go:build windows

// Package processgroup provides process cleanup helpers.
package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
	"weak"

	"golang.org/x/sys/windows"
)

const defaultPollPeriod = 25 * time.Millisecond

// ctrlBreakEvent is the console control event used as the graceful stop
// request. CTRL_BREAK is the one that can be addressed to a specific process
// group; CTRL_C cannot, so it would hit this process too.
const ctrlBreakEvent = 1

// Options configures process cleanup. The Kill hook has no Windows meaning —
// there is no signal to deliver — and is retained only so call sites stay
// platform-neutral.
type Options struct {
	Kill           func(pid int, sig syscall.Signal) error
	CurrentGroupID func() int
	PollPeriod     time.Duration
}

func (o Options) currentGroupID() int {
	if o.CurrentGroupID != nil {
		return o.CurrentGroupID()
	}
	// A process created with CREATE_NEW_PROCESS_GROUP leads a group whose ID is
	// its own PID, so this process's own group is identified by its PID.
	return os.Getpid()
}

func (o Options) pollPeriod() time.Duration {
	if o.PollPeriod > 0 {
		return o.PollPeriod
	}
	return defaultPollPeriod
}

// jobBasicAccountingInformation mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
// ActiveProcesses is the field that matters here: it counts every live process
// in the job, descendants included, which is the only direct answer to "is the
// owned tree gone".
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// ownedTree is the launcher's handle on one command's process tree.
//
// The handle can outlive ownership. Releasing ownership while an operation is
// still using the handle must not close it underneath that operation: a child
// exiting at the moment it is asked to stop is the ordinary case, not an
// exceptional one, and closing there would turn it into a spurious failure.
// Ownership release and in-flight use are tracked separately, and whichever
// finishes last closes the handle.
type ownedTree struct {
	mu  sync.Mutex
	job windows.Handle
	// pgid is the leader's PID, which names the console process group a
	// cooperative stop is addressed to. leader holds the leader's process
	// object for as long as the tree is owned, so that number cannot pass to
	// another process, and a request to the group cannot reach anyone else's,
	// even once the leader has been reaped.
	pgid           int
	consoleGroupID uint32
	leader         windows.Handle
	persist        Persistence
	released       bool
	inUse          int
	closed         bool
	// watched is set once a poll has been started to retire the tree when
	// its job empties; see keepOwned.
	watched bool
}

var (
	ownedMu sync.Mutex
	owned   = map[*exec.Cmd]*ownedTree{}

	// drained remembers commands whose tree was watched empty while this
	// package still held its job handle. Ownership ends there, but the answer
	// to "is this tree gone" does not: it is yes, and observed. Without the
	// record a later stop cannot tell a drained tree from one that was never
	// owned, and only the second deserves ErrNotOwned.
	//
	// Keys are weak so remembering a command never keeps it alive: an entry
	// lasts exactly as long as someone could still ask about that command.
	drained = map[weak.Pointer[exec.Cmd]]struct{}{}
)

var (
	startCommandAfterStartForTest  func(*exec.Cmd) error
	startCommandAfterResumeForTest func(*exec.Cmd)
)

func lookupOwned(cmd *exec.Cmd) *ownedTree {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	return owned[cmd]
}

// acquireOwned returns cmd's tree with a lease already taken, so the handle
// cannot be closed for the duration of the caller's operation. The lookup and
// the lease are taken together because a release between them would hand back a
// tree whose handle is already gone.
//
// A nil result means cmd is not owned: either it was never started through
// StartCommand, or its ownership has already been released.
func acquireOwned(cmd *exec.Cmd) *ownedTree {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	tree := owned[cmd]
	if tree == nil {
		return nil
	}
	if !tree.use() {
		return nil
	}
	return tree
}

// use takes a lease on the job handle. It fails once the handle is closed,
// which is the one state from which no operation can proceed.
func (t *ownedTree) use() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.inUse++
	return true
}

// finish ends one operation's lease and drops ownership of the tree.
func (t *ownedTree) finish() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inUse--
	t.released = true
	return t.closeIfIdle()
}

// endUse ends one operation's lease and keeps ownership of the tree.
func (t *ownedTree) endUse() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inUse--
	return t.closeIfIdle()
}

// closeIfIdle closes the job handle, and the leader handle with it, once
// ownership has been released and no operation still holds a lease. Callers
// must hold t.mu.
func (t *ownedTree) closeIfIdle() error {
	if t.closed || !t.released || t.inUse > 0 {
		return nil
	}
	t.closed = true
	err := windows.CloseHandle(t.job)
	if t.leader != 0 {
		if leaderErr := windows.CloseHandle(t.leader); err == nil {
			err = leaderErr
		}
	}
	return err
}

func forgetOwned(cmd *exec.Cmd) {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	delete(owned, cmd)
}

// retireDrained ends ownership of a tree observed empty and records that it
// was. The caller must hold a lease on tree.
func retireDrained(cmd *exec.Cmd, tree *ownedTree) error {
	key := weak.Make(cmd)
	ownedMu.Lock()
	delete(owned, cmd)
	_, seen := drained[key]
	drained[key] = struct{}{}
	ownedMu.Unlock()
	if !seen {
		runtime.AddCleanup(cmd, forgetDrained, key)
	}
	return tree.finish()
}

func forgetDrained(key weak.Pointer[exec.Cmd]) {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	delete(drained, key)
}

func wasDrained(cmd *exec.Cmd) bool {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	_, ok := drained[weak.Make(cmd)]
	return ok
}

// settleOwned ends one operation's lease on tree according to what the
// operation established. A tree it left confirmed empty is retired. Anything
// else keeps it owned: the job handle is the only thing on Windows that can
// still reach survivors, so letting go of it over a live tree strands them
// where no later stop can find them.
func settleOwned(cmd *exec.Cmd, tree *ownedTree, opErr error) {
	if opErr != nil {
		_ = keepOwned(cmd, tree)
		return
	}
	_ = retireDrained(cmd, tree)
}

// Bounds on how often a tree kept over live members is re-read. The job has no
// dependable "now empty" signal to wait on instead: completion-port messages
// are documented as not guaranteed, and a tree that empties before a port is
// associated never reports it at all.
var (
	emptyPollInitial = 100 * time.Millisecond
	emptyPollMax     = 5 * time.Second
)

// keepOwned ends one operation's lease on a tree that may still have members
// and keeps ownership of it, as settleOwned explains. The tree is then retired
// once its job's accounting shows it empty, as ReleaseCommandIfEmpty would have
// done had it been empty then. Without that a tree whose last member exits on
// its own stays owned — its handle, its map entry and the command — for as long
// as this process runs, unless some later stop happens to revisit it.
func keepOwned(cmd *exec.Cmd, tree *ownedTree) error {
	if tree.startWatch() {
		go retireWhenEmpty(cmd, tree)
	}
	return tree.endUse()
}

// startWatch reports whether the caller is the first to watch t for emptiness,
// and marks it watched.
func (t *ownedTree) startWatch() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.watched {
		return false
	}
	t.watched = true
	return true
}

// retireWhenEmpty re-reads a kept tree's job until it is seen empty, then
// retires it. It takes a lease only for each read, so it never holds the handle
// open against another operation, and it stops once anything else has ended
// ownership. A read that fails leaves the tree owned and is retried.
func retireWhenEmpty(cmd *exec.Cmd, tree *ownedTree) {
	period := emptyPollInitial
	for {
		time.Sleep(period)
		period = min(2*period, emptyPollMax)
		held := acquireOwned(cmd)
		if held == nil {
			return
		}
		if active, err := held.activeProcesses(); err == nil && active == 0 {
			_ = retireDrained(cmd, held)
			return
		}
		_ = held.endUse()
	}
}

// StartCommandInNewGroup puts cmd in its own console control group so a Ctrl+C
// delivered to this console is not inherited by the child.
//
// This is console signal routing and nothing more. A Windows process group is
// NOT a containment boundary: it does not track descendants, killing its leader
// leaves grandchildren running, and there is no group-wide terminate. Use
// StartCommand for anything whose tree must later be reaped.
func StartCommandInNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// StartCommand starts cmd inside a Job Object that owns it and every process it
// goes on to create, and returns the console group ID of the started process.
//
// The job is created first and the process is started SUSPENDED so it can be
// assigned before it runs a single instruction. Assigning after the process is
// already running loses any descendant it spawns in the interval, and those
// escapees are exactly the processes that survive a later cleanup.
//
// Persistence decides what releasing ownership means; see Persistence. A
// failure anywhere after CreateProcess kills the process rather than leaving a
// suspended or unowned one behind.
func StartCommand(cmd *exec.Cmd, persist Persistence) (int, error) {
	if cmd == nil {
		return 0, errors.New("processgroup: nil command")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("processgroup: create job object: %w", err)
	}
	if persist == KillOnRelease {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			_ = windows.CloseHandle(job)
			return 0, fmt.Errorf("processgroup: configure job object: %w", err)
		}
	}

	StartCommandInNewGroup(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED

	ownedMu.Lock()
	ownershipLocked := true
	unlockOwnership := func() {
		if ownershipLocked {
			ownedMu.Unlock()
			ownershipLocked = false
		}
	}

	if err := cmd.Start(); err != nil {
		unlockOwnership()
		_ = windows.CloseHandle(job)
		return 0, err
	}

	var leader windows.Handle
	published := false
	abandon := func(cause error) (int, error) {
		if published {
			delete(owned, cmd)
			published = false
		}
		unlockOwnership()
		// The process exists but is not usable: either unowned or still
		// suspended. Terminating the job first catches anything it did manage
		// to spawn before the failure.
		_ = windows.TerminateJobObject(job, 1)
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			cause = errors.Join(cause, killErr)
		}
		if leader != 0 {
			_ = windows.CloseHandle(leader)
		}
		_ = windows.CloseHandle(job)
		_ = cmd.Wait()
		return 0, cause
	}

	if startCommandAfterStartForTest != nil {
		if err := startCommandAfterStartForTest(cmd); err != nil {
			return abandon(err)
		}
	}

	var assignErr, pinErr error
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(handle))
		if assignErr == nil {
			pinErr = windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(handle), windows.CurrentProcess(), &leader, windows.SYNCHRONIZE, false, 0)
		}
	}); err != nil {
		return abandon(fmt.Errorf("processgroup: access process handle for pid %d: %w", cmd.Process.Pid, err))
	}
	if assignErr != nil {
		return abandon(fmt.Errorf("processgroup: assign pid %d to job object: %w", cmd.Process.Pid, assignErr))
	}
	if pinErr != nil {
		return abandon(fmt.Errorf("processgroup: hold pid %d: %w", cmd.Process.Pid, pinErr))
	}
	consoleGroupID, groupErr := processGroupIDForPID(cmd.Process.Pid)
	if groupErr != nil {
		return abandon(fmt.Errorf("processgroup: read process group of pid %d: %w", cmd.Process.Pid, groupErr))
	}

	pgid := cmd.Process.Pid
	owned[cmd] = &ownedTree{job: job, leader: leader, pgid: pgid, consoleGroupID: consoleGroupID, persist: persist}
	published = true
	if err := resumeProcessThreads(uint32(cmd.Process.Pid)); err != nil {
		return abandon(fmt.Errorf("processgroup: resume pid %d: %w", cmd.Process.Pid, err))
	}
	unlockOwnership()
	if startCommandAfterResumeForTest != nil {
		startCommandAfterResumeForTest(cmd)
	}
	return pgid, nil
}

// ReleaseCommand drops the launcher's ownership of cmd's tree.
//
// For a KillOnRelease command this reaps the whole tree, which is the point:
// cancelling a helper must not leave descendants behind. The reap is forced and
// confirmed here rather than left to the kill-on-close that closing the handle
// would trigger, which is asynchronous and unobserved: a release that returns
// nil has reaped. If the reap cannot be confirmed the tree stays owned and the
// error is returned, so a later terminate can still finish the job.
//
// For SurviveRelease it closes the handle without killing anything, so a
// detaching CLI leaves a long-lived agent server running. Once released the
// tree can no longer be terminated through this package — that is the price of
// letting it outlive us, and a later stop reports ErrNotOwned rather than
// pretending otherwise. A reaper that only means "the leader has exited" wants
// ReleaseCommandIfEmpty instead.
func ReleaseCommand(cmd *exec.Cmd) error {
	tree := acquireOwned(cmd)
	if tree == nil {
		return nil
	}
	active, err := tree.activeProcesses()
	if err == nil && active == 0 {
		return retireDrained(cmd, tree)
	}
	if tree.persist == KillOnRelease {
		if err := tree.terminate(); err != nil {
			_ = keepOwned(cmd, tree)
			return fmt.Errorf("processgroup: reap owned tree of pid %d on release: %w", cmd.Process.Pid, err)
		}
		if err := waitTreeExit(tree, confirmTimeout(0), Options{}); err != nil {
			_ = keepOwned(cmd, tree)
			return err
		}
		return retireDrained(cmd, tree)
	}
	forgetOwned(cmd)
	return tree.finish()
}

// ReleaseCommandIfEmpty drops ownership of cmd's tree only when the job's own
// accounting shows nothing left in it, and otherwise keeps it, so a later
// terminate can still reach whatever the leader left running.
//
// This is the release a reaper wants. The leader exiting is not the tree
// exiting: an agent that handles a stop by exiting leaves its helpers behind,
// and for a SurviveRelease tree the job handle is the only thing that can still
// reach them. Releasing it there strands them, and every later stop then
// reports success over them. A tree kept here stays owned until it is seen
// empty — by a terminate, or by the watch kept over it once its members exit on
// their own — the Windows counterpart of a Unix process group, which stays
// addressable for exactly as long as it can contain something to kill.
func ReleaseCommandIfEmpty(cmd *exec.Cmd) error {
	tree := acquireOwned(cmd)
	if tree == nil {
		return nil
	}
	active, err := tree.activeProcesses()
	if err != nil {
		_ = keepOwned(cmd, tree)
		return fmt.Errorf("processgroup: reading job object process count: %w", err)
	}
	if active > 0 {
		return keepOwned(cmd, tree)
	}
	return retireDrained(cmd, tree)
}

// activeProcesses reports how many processes in the job are still alive.
func (t *ownedTree) activeProcesses() (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, errors.New("job object handle is closed")
	}
	var info jobBasicAccountingInformation
	if err := windows.QueryInformationJobObject(
		t.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return 0, err
	}
	return info.ActiveProcesses, nil
}

func (t *ownedTree) terminate() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("job object handle is closed")
	}
	return windows.TerminateJobObject(t.job, 1)
}

type jobBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
	ProcessIDList             [1]uintptr
}

func (t *ownedTree) processIDs() ([]int, error) {
	t.mu.Lock()
	job := t.job
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, errors.New("job object handle is closed")
	}
	count := uint32(16)
	for {
		size := uint32(unsafe.Sizeof(jobBasicProcessIDList{}) + uintptr(count-1)*unsafe.Sizeof(uintptr(0)))
		buffer := make([]byte, size)
		info := (*jobBasicProcessIDList)(unsafe.Pointer(&buffer[0]))
		err := windows.QueryInformationJobObject(
			job,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(info)),
			size,
			nil,
		)
		if err != nil {
			if errors.Is(err, windows.ERROR_MORE_DATA) {
				count *= 2
				continue
			}
			return nil, err
		}
		if info.NumberOfAssignedProcesses > count {
			count = info.NumberOfAssignedProcesses
			continue
		}
		ids := unsafe.Slice(&info.ProcessIDList[0], int(info.NumberOfProcessIDsInList))
		pids := make([]int, 0, len(ids))
		for _, id := range ids {
			pids = append(pids, int(id))
		}
		return pids, nil
	}
}

var errJobMemberGone = errors.New("process is no longer a job member")

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func openJobMember(pid int, job windows.Handle, access uint32) (windows.Handle, error) {
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, errJobMemberGone
		}
		return 0, err
	}
	inJob, err := isProcessInJob(handle, job)
	if err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	if !inJob {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, errJobMemberGone
	}
	return handle, nil
}

func isProcessInJob(process, job windows.Handle) (bool, error) {
	var result int32
	r1, _, err := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result)))
	if r1 == 0 {
		return false, err
	}
	return result != 0, nil
}

const processGroupReadAccess = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_VM_READ | windows.SYNCHRONIZE

func processGroupIDForPID(pid int) (uint32, error) {
	handle, err := windows.OpenProcess(processGroupReadAccess, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	return processGroupIDFromHandle(handle)
}

func processGroupIDFromHandle(handle windows.Handle) (uint32, error) {
	var information windows.PROCESS_BASIC_INFORMATION
	var returned uint32
	err := windows.NtQueryInformationProcess(
		handle,
		windows.ProcessBasicInformation,
		unsafe.Pointer(&information),
		uint32(unsafe.Sizeof(information)),
		&returned,
	)
	if err != nil {
		return 0, err
	}
	if returned < uint32(unsafe.Sizeof(information)) {
		return 0, fmt.Errorf("process basic information returned %d bytes, want %d", returned, unsafe.Sizeof(information))
	}
	if information.PebBaseAddress == nil {
		return 0, errors.New("process has no PEB")
	}
	parametersField := uintptr(unsafe.Pointer(information.PebBaseAddress)) + unsafe.Offsetof(windows.PEB{}.ProcessParameters)
	parametersAddress, err := readRemoteUintptr(handle, parametersField)
	if err != nil {
		return 0, fmt.Errorf("reading PEB: %w", err)
	}
	if parametersAddress == 0 {
		return 0, errors.New("process has no parameter block")
	}
	groupID, err := readRemoteUint32(handle, parametersAddress+unsafe.Offsetof(windows.RTL_USER_PROCESS_PARAMETERS{}.ProcessGroupId))
	if err != nil {
		return 0, fmt.Errorf("reading process group id: %w", err)
	}
	return groupID, nil
}

func readRemoteUintptr(handle windows.Handle, address uintptr) (uintptr, error) {
	var value uintptr
	if err := readRemoteValue(handle, address, unsafe.Pointer(&value), unsafe.Sizeof(value)); err != nil {
		return 0, err
	}
	return value, nil
}

func readRemoteUint32(handle windows.Handle, address uintptr) (uint32, error) {
	var value uint32
	if err := readRemoteValue(handle, address, unsafe.Pointer(&value), unsafe.Sizeof(value)); err != nil {
		return 0, err
	}
	return value, nil
}

func readRemoteValue(handle windows.Handle, address uintptr, out unsafe.Pointer, size uintptr) error {
	var read uintptr
	if err := windows.ReadProcessMemory(handle, address, (*byte)(out), size, &read); err != nil {
		return err
	}
	if read != size {
		return fmt.Errorf("read %d byte(s), want %d", read, size)
	}
	return nil
}

func (t *ownedTree) terminateConsoleGroup() error {
	t.mu.Lock()
	job := t.job
	groupID := t.consoleGroupID
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return errors.New("job object handle is closed")
	}
	pids, err := t.processIDs()
	if err != nil {
		return err
	}
	var errs []error
	for _, pid := range pids {
		handle, err := openJobMember(pid, job, windows.PROCESS_TERMINATE|processGroupReadAccess)
		if errors.Is(err, errJobMemberGone) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("open pid %d: %w", pid, err))
			continue
		}
		memberGroupID, groupErr := processGroupIDFromHandle(handle)
		if groupErr == nil && memberGroupID != groupID {
			if closeErr := windows.CloseHandle(handle); closeErr != nil {
				errs = append(errs, fmt.Errorf("close pid %d in process group %d: %w", pid, memberGroupID, closeErr))
			}
			continue
		}
		terminateErr := windows.TerminateProcess(handle, 1)
		if closeErr := windows.CloseHandle(handle); groupErr == nil {
			groupErr = closeErr
		}
		if terminateErr != nil && groupErr == nil {
			groupErr = terminateErr
		}
		if groupErr != nil {
			errs = append(errs, fmt.Errorf("terminate pid %d in process group %d: %w", pid, groupID, groupErr))
		}
	}
	return errors.Join(errs...)
}

func (t *ownedTree) activeConsoleGroupProcesses() (uint32, error) {
	t.mu.Lock()
	job := t.job
	groupID := t.consoleGroupID
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return 0, errors.New("job object handle is closed")
	}
	pids, err := t.processIDs()
	if err != nil {
		return 0, err
	}
	var active uint32
	var errs []error
	for _, pid := range pids {
		handle, err := openJobMember(pid, job, processGroupReadAccess)
		if errors.Is(err, errJobMemberGone) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("open pid %d: %w", pid, err))
			continue
		}
		memberGroupID, groupErr := processGroupIDFromHandle(handle)
		if groupErr == nil && memberGroupID == groupID {
			event, waitErr := windows.WaitForSingleObject(handle, 0)
			if waitErr != nil {
				groupErr = waitErr
			} else if event == uint32(windows.WAIT_TIMEOUT) {
				active++
			}
		}
		if closeErr := windows.CloseHandle(handle); groupErr == nil {
			groupErr = closeErr
		}
		if groupErr != nil {
			errs = append(errs, fmt.Errorf("read pid %d in process group %d: %w", pid, groupID, groupErr))
		}
	}
	return active, errors.Join(errs...)
}

// CommandGroupID returns the console group ID StartCommand established for cmd,
// or 0 when cmd is not owned by this package.
func CommandGroupID(cmd *exec.Cmd) int {
	if tree := lookupOwned(cmd); tree != nil {
		return tree.pgid
	}
	return 0
}

// HoldsTree reports whether this package still holds cmd's tree: the Job Object
// a terminate acts through. A tree is held from StartCommand until it is
// released or seen empty — by ReleaseCommandIfEmpty, a terminate, or the watch
// kept over a tree left with members — so once the leader has been reaped a
// held tree is one whose job still had members when last checked.
func HoldsTree(cmd *exec.Cmd) bool {
	return lookupOwned(cmd) != nil
}

// Terminate is unavailable for bare Windows PIDs because a PID alone does not
// identify a safe owned process tree: there is no kernel object to address, and
// the number may already have been recycled onto an unrelated process.
func Terminate(pgid int, _ time.Duration, _ Options) error {
	return fmt.Errorf("processgroup: bare process-group termination for PID %d: %w", pgid, ErrUnsupported)
}

// requestStop asks a tree to shut down cooperatively by delivering CTRL_BREAK to
// its console group — the closest Windows analogue to the SIGTERM the Unix path
// sends first.
//
// It is best effort by nature: the event reaches only processes that share this
// process's console, so a service or a detached CLI cannot deliver it at all.
// Callers must read a nil return as "the request was sent", never as "the tree
// stopped", and must still confirm.
func requestStop(pgid int, opts Options) error {
	if pgid <= 0 {
		return fmt.Errorf("processgroup: refusing to signal process group %d", pgid)
	}
	if pgid == opts.currentGroupID() {
		return fmt.Errorf("processgroup: refusing to signal own process group %d", pgid)
	}
	return windows.GenerateConsoleCtrlEvent(ctrlBreakEvent, uint32(pgid))
}

// waitTreeExit waits for every process in the owned tree to leave, bounded by
// timeout. Emptiness is read from the job's own accounting rather than from the
// direct process, so a surviving grandchild cannot pass as a clean exit.
func waitTreeExit(tree *ownedTree, timeout time.Duration, opts Options) error {
	deadline := time.Now().Add(timeout)
	for {
		active, err := tree.activeProcesses()
		if err != nil {
			return fmt.Errorf("processgroup: reading job object process count: %w", err)
		}
		if active == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("processgroup: %d process(es) in the owned tree did not exit within %s", active, timeout)
		}
		time.Sleep(opts.pollPeriod())
	}
}

// killUnownedProcess is the fallback for a command with no ownership boundary:
// it was never started through StartCommand, or it was released while its tree
// was not known to be empty.
//
// It kills the direct process if that is still reachable, and always reports
// ErrNotOwned, because nothing here can speak for what that process spawned. A
// failed Kill is no evidence either way. ErrProcessDone says only that the
// leader was reaped. EINVAL says only that Go no longer holds its handle, which
// Process.Release produces for a process that is still running. Reading either
// as "the tree is gone" is what let a stop report success over live
// descendants.
func killUnownedProcess(cmd *exec.Cmd) error {
	incomplete := fmt.Errorf("processgroup: descendants of pid %d cannot be reaped: %w", cmd.Process.Pid, ErrNotOwned)
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.Join(incomplete, err)
	}
	return incomplete
}

// stopUnowned answers a stop for a command this package no longer owns. A tree
// that was watched empty before ownership ended is reported stopped, which
// keeps a stop of an already-finished session idempotent; anything else has
// no boundary left to act on.
func stopUnowned(cmd *exec.Cmd) error {
	if wasDrained(cmd) {
		return nil
	}
	return killUnownedProcess(cmd)
}

// TerminateCommand shuts down cmd's owned process tree: request a cooperative
// stop, wait out timeout, then terminate the Job Object and confirm the tree is
// empty.
//
// A command that was not started by StartCommand has no ownership boundary. Its
// direct process is still killed, but the missing descendants are reported
// rather than passed off as a clean result — a caller told the cleanup worked
// cannot compensate for it.
func TerminateCommand(cmd *exec.Cmd, _ int, timeout time.Duration, opts Options) (err error) {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	tree := acquireOwned(cmd)
	if tree == nil {
		return stopUnowned(cmd)
	}
	defer func() { settleOwned(cmd, tree, err) }()

	if timeout > 0 {
		if err := requestStop(tree.pgid, opts); err == nil {
			if waitTreeExit(tree, timeout, opts) == nil {
				return nil
			}
		}
	}
	if err := tree.terminate(); err != nil {
		return fmt.Errorf("processgroup: terminate owned tree of pid %d: %w", cmd.Process.Pid, err)
	}
	return waitTreeExit(tree, confirmTimeout(timeout), opts)
}

// KillCommandGroup terminates cmd's owned tree immediately, with no cooperative
// phase, and confirms the tree is empty.
func KillCommandGroup(cmd *exec.Cmd, _ int, opts Options) (err error) {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	tree := acquireOwned(cmd)
	if tree == nil {
		return stopUnowned(cmd)
	}
	defer func() { settleOwned(cmd, tree, err) }()
	if err := tree.terminate(); err != nil {
		return fmt.Errorf("processgroup: terminate owned tree of pid %d: %w", cmd.Process.Pid, err)
	}
	return waitTreeExit(tree, confirmTimeout(0), opts)
}

// KillCommandConsoleGroup terminates the processes in cmd's owned Job Object
// that remain in the console process group created for cmd's leader, and
// confirms that group is empty. Job members that deliberately entered another
// group are released with the Job handle instead of being terminated.
func KillCommandConsoleGroup(cmd *exec.Cmd, _ int, opts Options) (err error) {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	tree := acquireOwned(cmd)
	if tree == nil {
		return stopUnowned(cmd)
	}
	defer func() { settleOwned(cmd, tree, err) }()
	if err := tree.terminateConsoleGroup(); err != nil {
		return fmt.Errorf("processgroup: terminate process group %d of pid %d: %w", tree.consoleGroupID, cmd.Process.Pid, err)
	}
	return waitConsoleGroupExit(tree, confirmTimeout(0), opts)
}

func waitConsoleGroupExit(tree *ownedTree, timeout time.Duration, opts Options) error {
	deadline := time.Now().Add(timeout)
	for {
		active, err := tree.activeConsoleGroupProcesses()
		if err != nil {
			return fmt.Errorf("processgroup: reading owned process group: %w", err)
		}
		if active == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("processgroup: %d process(es) in the owned process group did not exit within %s", active, timeout)
		}
		time.Sleep(opts.pollPeriod())
	}
}

// InterruptCommand delivers an interrupt to cmd's console group. Windows can
// route a console control event only to a process that shares this process's
// console, so this fails outright where Unix would deliver SIGINT to the group;
// the error says so rather than reporting an interrupt that never arrived.
func InterruptCommand(cmd *exec.Cmd, knownPGID int, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := knownPGID
	if pgid <= 0 {
		if tree := acquireOwned(cmd); tree != nil {
			// The lease keeps the tree, and with it the leader's PID, held
			// until the request has been sent.
			defer func() { _ = tree.endUse() }()
			pgid = tree.pgid
		} else {
			pgid = cmd.Process.Pid
		}
	}
	if err := requestStop(pgid, opts); err != nil {
		return fmt.Errorf("processgroup: interrupt process group %d: %w", pgid, err)
	}
	return nil
}

// TerminateManagedCommand runs the provider-managed stop sequence for an owned
// command: request a cooperative stop, wait up to grace for the process to
// report its own exit on done, then force the owned tree down and confirm.
func TerminateManagedCommand(cmd *exec.Cmd, _ int, done <-chan struct{}, grace time.Duration, opts Options) (err error) {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	tree := acquireOwned(cmd)
	if tree == nil {
		// done closing proves only that the leader was reaped, so it cannot
		// answer for the tree here; whether the tree was seen empty can.
		return stopUnowned(cmd)
	}
	defer func() { settleOwned(cmd, tree, err) }()

	// A failure here means only that the cooperative request could not be
	// delivered; the forced path below is what the grace window protects. The
	// group is named by the leader's PID, which the tree holds for as long as
	// it is owned, so the request reaches this tree's group alone even when
	// the leader has already been reaped.
	_ = requestStop(tree.pgid, opts)

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// The leader exiting is not the same as the tree being empty, and on
		// Windows the gap between the two is wide: a console control event
		// reaches only processes sharing this console, so the cooperative
		// request above may never have arrived anywhere, and closing a
		// SurviveRelease job kills nothing. Treat the exit as a completed stop
		// only when the job's own accounting agrees nothing is left; otherwise
		// fall through and force the tree down. Reporting success here with
		// descendants still running is the false clear this sequence exists to
		// prevent.
		if active, err := tree.activeProcesses(); err == nil && active == 0 {
			return nil
		}
	case <-timer.C:
	}

	if err := tree.terminate(); err != nil {
		return fmt.Errorf("processgroup: terminate owned tree of pid %d: %w", cmd.Process.Pid, err)
	}
	if err := waitTreeExit(tree, confirmTimeout(grace), opts); err != nil {
		return err
	}
	<-done
	return nil
}

// confirmTimeout bounds the post-terminate confirmation wait. Termination is
// already forced by then, so this only has to outlast the kernel's teardown of
// the job's processes.
func confirmTimeout(requested time.Duration) time.Duration {
	const minConfirm = 2 * time.Second
	if requested > minConfirm {
		return requested
	}
	return minConfirm
}

// resumeProcessThreads resumes every thread of a process started suspended.
func resumeProcessThreads(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			if closeErr != nil {
				return closeErr
			}
			resumed++
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ThreadEntry32{}))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	if resumed == 0 {
		return errors.New("process has no resumable thread")
	}
	return nil
}
