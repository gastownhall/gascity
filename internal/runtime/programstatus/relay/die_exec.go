//go:build linux || darwin

package relay

import (
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// killPath is the kill(1) dieByExec replaces the process with.
const killPath = "/bin/kill"

// survivors are the signals whose default action, on Linux and Darwin
// alike, stops the process or does nothing to it.
var survivors = []syscall.Signal{
	unix.SIGSTOP, unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU,
	unix.SIGCONT, unix.SIGCHLD, unix.SIGURG, unix.SIGWINCH,
}

// dieByExec ends the process by sig, as if sig had killed it, without
// touching the kernel's signal dispositions directly.
//
// The Go runtime keeps its own handler on nearly every signal, so a signal
// the process sends itself does not end it the way it ends a C program:
// SIGQUIT and SIGSEGV become a traceback and exit status 2, and SIGUSR1 is
// ignored outright. dieByExec instead replaces the process image with
// kill(1), keeping the PID, and has it send sig to that PID: exec resets
// every caught signal to its default action. Core dumps are off; the
// program already dumped its own core if it was going to, and kill's would
// only duplicate it with nothing of the program's in it.
//
// It is not a shell that sends the signal because bash, macOS's /bin/sh,
// ignores SIGQUIT even when not interactive.
//
// dieByExec returns, without exec, when sig would leave the process
// running: its default action stops or ignores (survivors, plus the
// platform's own extra ones), or it is ignored (a SIG_IGN the process
// inherited survives exec). It also returns when the exec fails.
func dieByExec(sig syscall.Signal, extraSurvivors ...syscall.Signal) {
	if slices.Contains(survivors, sig) || slices.Contains(extraSurvivors, sig) || signal.Ignored(sig) {
		return
	}
	name, ok := strings.CutPrefix(unix.SignalName(sig), "SIG")
	if !ok {
		return
	}
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}) // on failure an extra core is the only cost
	// An empty environment: kill needs none.
	_ = unix.Exec(killPath, []string{"kill", "-s", name, strconv.Itoa(unix.Getpid())}, []string{}) // on failure the caller exits 128+sig
}
