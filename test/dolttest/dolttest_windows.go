//go:build windows

package dolttest

import (
	"os"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// Process discovery is unavailable on Windows, so the reaper finds nothing to
// reap here.
//
// Attributing a PID to `dolt sql-server` needs its command line, and reading
// another process's command line on Windows means reading its PEB across the
// process boundary — pidutil.Cmdline reports that as unsupported rather than
// guessing. The one thing this package must never do is substitute the Unix
// path: an absolute "/proc/<pid>/cmdline" resolves to C:\proc\<pid>\cmdline,
// and the root of the system drive lets any local user create that tree and
// forge the identity of an arbitrary PID. Reaping on forged evidence would
// kill an unrelated process.
//
// Returning no candidates is therefore the honest result, and it matches the
// package's existing behaviour on every non-Linux host. The consequence is
// bounded and understood: a leaked dolt server survives the sweep. The
// symptom-based directory sweep (SweepOrphanStoreDirs) is platform-neutral and
// still removes the store directory such a leak leaves behind.

func scanDoltSQLServers() map[int]string {
	return nil
}

// pidIsDoltSQLServer cannot attribute a PID here, and it gates a kill, so
// "cannot tell" must read as "not a target".
func pidIsDoltSQLServer(_ int) bool {
	return false
}

// platformPIDAlive answers from the kernel's process object. It is used only
// to decide whether a *prior run* that owned a temp directory has finished, so
// it stays meaningful even though command-line attribution does not.
func platformPIDAlive(pid int) bool {
	return pidutil.Alive(pid)
}

// requestStop and forceKill are unreachable because scanDoltSQLServers yields
// no candidates. They exist so the shared reaper logic compiles and stays
// under test on this platform.
func requestStop(_ int) {}

func forceKill(_ int) {}

// reraise ends an interrupted run. Windows has no signal to re-raise, and the
// handler has already suppressed the default Ctrl+C termination, so the
// process must exit here or the run would continue after the interrupt.
func reraise(_ os.Signal) {
	os.Exit(1)
}
