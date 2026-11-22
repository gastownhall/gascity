//go:build !windows

// Package processgroup provides Unix process-group cleanup helpers.
package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const defaultPollPeriod = 25 * time.Millisecond

// defaultEPERMSettle is how long a signal that keeps answering EPERM for a
// group with a live member is retried. A member that has begun to exit on its
// own (an agent that reads EOF on its stdin as the stop closes it) is not yet a
// zombie, and Darwin answers EPERM until the exit finishes: milliseconds on an
// idle host, longer under load.
const defaultEPERMSettle = 2 * time.Second

// ManagedStopSignalRetryBudget is the most TerminateManagedCommand can add to
// its stop and reap graces by retrying EPERM: one settle window at SIGTERM and
// one at SIGKILL.
const ManagedStopSignalRetryBudget = 2 * defaultEPERMSettle

// Options configures process-group cleanup.
type Options struct {
	Kill           func(pid int, sig syscall.Signal) error
	CurrentGroupID func() int
	PollPeriod     time.Duration

	// LiveMembers reports how many members of process group pgid are not
	// zombies. It is how an EPERM from kill(-pgid, ...) is told apart: on
	// Darwin it means either "every member is a zombie" or "a live member we
	// may not signal".
	LiveMembers func(pgid int) (int, error)

	// MemberStates lists the members of process group pgid with their states,
	// for the error of a persistent EPERM.
	MemberStates func(pgid int) ([]MemberState, error)

	// EPERMSettle bounds how long a signal that keeps answering EPERM for a
	// group with a live member is retried before it is reported.
	EPERMSettle time.Duration
}

// MemberState is one member of a process group and its kernel state.
type MemberState struct {
	Pid   int
	State string
}

func (o Options) kill(pid int, sig syscall.Signal) error {
	if o.Kill != nil {
		return o.Kill(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

func (o Options) currentGroupID() int {
	if o.CurrentGroupID != nil {
		return o.CurrentGroupID()
	}
	return syscall.Getpgrp()
}

func (o Options) liveMembers(pgid int) (int, error) {
	if o.LiveMembers != nil {
		return o.LiveMembers(pgid)
	}
	return defaultLiveMembers(pgid)
}

// groupGone reports whether err, returned by kill(-pgid, ...), means pgid has
// no member left to signal. ESRCH always does. EPERM does only when the member
// reader finds no live member: on Darwin kill(2) skips zombies, so a group of
// nothing but zombies answers EPERM, indistinguishable by errno from a live
// member this process may not signal. When the members cannot be read the
// answer stays "not gone", the safe reading.
func groupGone(pgid int, err error, opts Options) bool {
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if !errors.Is(err, syscall.EPERM) {
		return false
	}
	live, rerr := opts.liveMembers(pgid)
	return rerr == nil && live == 0
}

// signalGroup delivers sig to every member of process group pgid and returns nil
// once it has, or once the group has nothing left to signal.
//
// An EPERM for a group the member reader shows to hold a live member is retried
// for the settle window before it is believed: on Darwin a member that is
// exiting is neither a zombie nor signalable, so a stop that races the agent's
// own exit sees EPERM for a moment and then ESRCH or a zombie. An EPERM that
// outlasts the window is a member this process may not signal. One for a group
// that cannot be read at all (no reader on this platform, or the read failed) is
// not retried, since waiting would not tell it apart. Either is returned,
// wrapped, with what the member reader saw.
func signalGroup(pgid int, sig syscall.Signal, opts Options) error {
	settle := opts.epermSettle()
	deadline := time.Now().Add(settle)
	retried := false
	for {
		err := opts.kill(-pgid, sig)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return err
		}
		live, rerr := opts.liveMembers(pgid)
		if rerr == nil && live == 0 {
			return nil
		}
		if rerr != nil {
			return unreadableMembersError(pgid, err, retried, rerr, opts)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: signal still refused (EPERM) after %s: %s", err, settle, memberOutcome(pgid, live, rerr, opts))
		}
		retried = true
		time.Sleep(opts.pollPeriod())
	}
}

// unreadableMembersError reports an EPERM from kill(-pgid, ...) whose members
// could not be read, saying whether earlier attempts had already retried it.
func unreadableMembersError(pgid int, err error, retried bool, rerr error, opts Options) error {
	if !retried {
		return fmt.Errorf("%w: signal refused (EPERM); members unreadable, not retried: %s", err, memberOutcome(pgid, 0, rerr, opts))
	}
	return fmt.Errorf("%w: signal refused (EPERM); members became unreadable after retrying: %s", err, memberOutcome(pgid, 0, rerr, opts))
}

// memberOutcome describes what the member reader says about process group pgid
// (live and rerr are the answer of liveMembers), for the error of an EPERM that
// persisted.
func memberOutcome(pgid, live int, rerr error, opts Options) string {
	var b strings.Builder
	if rerr != nil {
		fmt.Fprintf(&b, "member query failed: %v", rerr)
	} else {
		fmt.Fprintf(&b, "live members=%d", live)
	}
	members, err := opts.memberStates(pgid)
	if err != nil {
		fmt.Fprintf(&b, "; member states unavailable: %v", err)
		return b.String()
	}
	if len(members) == 0 {
		b.WriteString("; no members listed")
		return b.String()
	}
	b.WriteString("; members:")
	for i, m := range members {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " pid %d %s", m.Pid, m.State)
	}
	return b.String()
}

func (o Options) memberStates(pgid int) ([]MemberState, error) {
	if o.MemberStates != nil {
		return o.MemberStates(pgid)
	}
	return defaultMemberStates(pgid)
}

func (o Options) epermSettle() time.Duration {
	if o.EPERMSettle > 0 {
		return o.EPERMSettle
	}
	return defaultEPERMSettle
}

func (o Options) pollPeriod() time.Duration {
	if o.PollPeriod > 0 {
		return o.PollPeriod
	}
	return defaultPollPeriod
}

// StartCommandInNewGroup configures cmd to start as a new process-group leader.
func StartCommandInNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// StartCommand starts cmd as the leader of its own process group and returns
// the group's identifier.
//
// Persistence is accepted for a platform-neutral call shape and has no effect
// here: a Unix process group already behaves both ways by construction. The
// group outlives the parent, and cancellation reaches the whole tree by
// signaling -pgid, so there is no handle whose closure could imply a kill.
func StartCommand(cmd *exec.Cmd, _ Persistence) (int, error) {
	if cmd == nil {
		return 0, errors.New("processgroup: nil command")
	}
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return CommandGroupID(cmd), nil
}

// ReleaseCommand drops the launcher's ownership of cmd's tree. It is a no-op on
// Unix, where ownership is expressed by the group ID rather than by a handle.
func ReleaseCommand(_ *exec.Cmd) error {
	return nil
}

// ReleaseCommandIfEmpty drops ownership of cmd's tree once nothing is left in
// it. It is a no-op on Unix: the group ID stays addressable for as long as the
// group has members, with no handle to give up.
func ReleaseCommandIfEmpty(_ *exec.Cmd) error {
	return nil
}

// HoldsTree reports whether this package holds cmd's tree. It never does on
// Unix: a process group is addressed by its ID, and nothing is held.
func HoldsTree(_ *exec.Cmd) bool {
	return false
}

// CommandGroupID returns the process-group identifier that StartCommandInNewGroup
// established for cmd, or 0 if cmd was not started that way.
//
// It is derived from the spawn request rather than queried from the kernel, and
// that is the whole point. syscall.Getpgid answers only while the group LEADER
// is still a live, unreaped process; once the leader has exited and been waited
// on it fails with ESRCH even though surviving descendants still carry the group
// ID. Reading the ID from the request keeps it valid for exactly as long as the
// group can still contain something to kill.
func CommandGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return 0
	}
	if cmd.SysProcAttr.Pgid != 0 {
		return cmd.SysProcAttr.Pgid
	}
	// Setpgid with Pgid 0 makes the child the leader of a new group whose ID
	// is its own PID.
	return cmd.Process.Pid
}

// resolveGroupID returns the group to act on for cmd, preferring an explicitly
// captured ID, then the ID implied by the spawn request, and only then a live
// kernel lookup for commands started outside this package.
func resolveGroupID(cmd *exec.Cmd, knownPGID int) (int, error) {
	if knownPGID > 0 {
		return knownPGID, nil
	}
	if pgid := CommandGroupID(cmd); pgid > 0 {
		return pgid, nil
	}
	resolved, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return 0, fmt.Errorf("resolve process group for pid %d: %w", cmd.Process.Pid, err)
	}
	return resolved, nil
}

// unsafeGroup reports whether pgid must never be signaled: init, the
// nonpositive values kill(2) reads as broadcast or current-group, and the
// caller's own group.
func unsafeGroup(pgid int, opts Options) bool {
	return pgid <= 1 || pgid == opts.currentGroupID()
}

// Terminate sends SIGTERM to pgid, waits for exit, then escalates to SIGKILL.
func Terminate(pgid int, timeout time.Duration, opts Options) error {
	if unsafeGroup(pgid, opts) {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := signalGroup(pgid, syscall.SIGTERM, opts); err != nil {
		return err
	}
	if err := waitForExit(pgid, timeout, opts); err == nil {
		return nil
	}
	if err := signalGroup(pgid, syscall.SIGKILL, opts); err != nil {
		return err
	}
	return waitForExit(pgid, timeout, opts)
}

// TerminateCommand terminates cmd's process group and falls back to direct kill.
func TerminateCommand(cmd *exec.Cmd, knownPGID int, timeout time.Duration, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return killDirect(cmd.Process, err)
	}
	if err := Terminate(pgid, timeout, opts); err != nil {
		return killDirect(cmd.Process, fmt.Errorf("terminate process group %d: %w", pgid, err))
	}
	return nil
}

// KillCommandGroup delivers SIGKILL to cmd's whole process group immediately,
// with no graceful phase and no wait.
//
// This is the force path, kept separate from TerminateCommand so a caller that
// means "kill now" cannot be quietly upgraded to a graceful stop with a timeout.
// Failed-start cleanup uses it: the process never became usable, there is
// nothing to shut down politely, and the caller is about to return an error to
// someone waiting on it.
//
// A group that is already gone is success: ESRCH, or on Darwin an EPERM from a
// group whose members are all zombies.
func KillCommandGroup(cmd *exec.Cmd, knownPGID int, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return killDirect(cmd.Process, err)
	}
	if unsafeGroup(pgid, opts) {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := signalGroup(pgid, syscall.SIGKILL, opts); err != nil {
		return killDirect(cmd.Process, fmt.Errorf("kill process group %d: %w", pgid, err))
	}
	return nil
}

// InterruptCommand delivers an interrupt to cmd's whole process group: the
// request an interactive user makes with Ctrl+C, which the agent is expected to
// handle rather than die from.
func InterruptCommand(cmd *exec.Cmd, knownPGID int, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if unsafeGroup(pgid, opts) {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := signalGroup(pgid, syscall.SIGINT, opts); err != nil {
		return err
	}
	return nil
}

// TerminateManagedCommand runs the provider-managed stop sequence for an owned
// command: request a graceful stop, wait up to grace for the process to report
// its own exit on done and for the rest of its process group to exit, then
// force the group down and wait for confirmation.
//
// done must be closed by whoever reaps cmd. Waiting on it rather than polling
// liveness is what makes the returned nil mean "observed exited" instead of
// "stopped looking". A command that outlives SIGKILL yields
// [ErrProcessNotConfirmedDead] after [ManagedProcessReapGrace].
func TerminateManagedCommand(cmd *exec.Cmd, knownPGID int, done <-chan struct{}, grace time.Duration, opts Options) error {
	return terminateManagedCommandGraces(cmd, knownPGID, done, grace, ManagedProcessReapGrace, opts)
}

func terminateManagedCommandGraces(cmd *exec.Cmd, knownPGID int, done <-chan struct{}, grace, reapGrace time.Duration, opts Options) error {
	var pgid int
	if cmd != nil && cmd.Process != nil {
		var err error
		pgid, err = resolveGroupID(cmd, knownPGID)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	if pgid > 0 && !unsafeGroup(pgid, opts) {
		if termErr := signalGroup(pgid, syscall.SIGTERM, opts); termErr != nil {
			return fmt.Errorf("request stop of process group %d: %w", pgid, termErr)
		}
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// The leader exiting is not the group exiting: a member that handles
		// or ignores SIGTERM outlives a leader that does not, and nothing
		// would kill it afterwards. Wait out the grace for the whole group.
		if pgid <= 0 || unsafeGroup(pgid, opts) || groupGoneBefore(pgid, timer.C, opts) {
			return nil
		}
	case <-timer.C:
	}

	if err := KillCommandGroup(cmd, pgid, opts); err != nil {
		return err
	}
	reap := time.NewTimer(reapGrace)
	defer reap.Stop()
	select {
	case <-done:
		return nil
	case <-reap.C:
		return fmt.Errorf("%w: still running %v after SIGKILL", ErrProcessNotConfirmedDead, reapGrace)
	}
}

// groupGoneBefore polls process group pgid until it has no members (true) or
// expired fires (false). A live member this process may not signal still counts.
func groupGoneBefore(pgid int, expired <-chan time.Time, opts Options) bool {
	ticker := time.NewTicker(opts.pollPeriod())
	defer ticker.Stop()
	for {
		if !alive(pgid, opts) {
			return true
		}
		select {
		case <-expired:
			return false
		case <-ticker.C:
		}
	}
}

func waitForExit(pgid int, timeout time.Duration, opts Options) error {
	deadline := time.Now().Add(timeout)
	for {
		if !alive(pgid, opts) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d did not exit within %s", pgid, timeout)
		}
		time.Sleep(opts.pollPeriod())
	}
}

func alive(pgid int, opts Options) bool {
	if pgid <= 0 {
		return false
	}
	err := opts.kill(-pgid, 0)
	return err == nil || (errors.Is(err, syscall.EPERM) && !groupGone(pgid, err, opts))
}

func killDirect(process *os.Process, cause error) error {
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if cause != nil {
			return fmt.Errorf("%w; direct kill failed: %w", cause, err)
		}
		return err
	}
	return cause
}
