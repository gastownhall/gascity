//go:build windows

package pidutil

import (
	"errors"
	"os"
	"testing"
)

// TestProcfsIsNeverConsultedOnWindows pins a local privilege boundary, not a
// portability gap.
//
// An absolute "/proc/<pid>/cmdline" resolves to C:\proc\<pid>\cmdline on the
// current drive. The root of the system drive grants Authenticated Users
// add-subdirectory rights by default and C:\proc does not exist, so any local
// user can create that tree and plant records that forge the identity of an
// arbitrary PID.
//
// The forgery itself cannot be staged from a test: "/proc" is drive-rooted, so
// reproducing it would mean creating C:\proc on the host running the suite —
// the very artifact this gate exists to make harmless. What is asserted instead
// is that the readers never reach a filesystem path at all: the Windows
// implementations are total functions over their arguments, so there is no
// input for which a planted file could be consulted.
func TestProcfsIsNeverConsultedOnWindows(t *testing.T) {
	pid := os.Getpid()

	if _, err := readProcFile(pid, "cmdline"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("readProcFile = %v, want ErrUnsupported", err)
	}
	if _, err := readProcDir(pid, "task"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("readProcDir = %v, want ErrUnsupported", err)
	}

	// ParentPID and Comm have no non-procfs fallback, so they must report the
	// gap rather than invent an ancestry.
	if _, err := ParentPID(pid); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ParentPID = %v, want ErrUnsupported", err)
	}
	if got := Comm(pid); got != "" {
		t.Fatalf("Comm = %q, want empty", got)
	}
	if got := DetectUserSubreaperPID(pid); got != 0 {
		t.Fatalf("DetectUserSubreaperPID = %d, want 0; Windows has no subreaper reparenting to model", got)
	}

	// Cmdline has no Windows implementation and must say so in a form callers
	// can branch on, rather than returning an empty argv that reads as a
	// process with no arguments.
	argv, err := Cmdline(pid)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Cmdline = (%q, %v), want ErrUnsupported", argv, err)
	}

	// StartTime does have a real Windows implementation, so it must answer from
	// the kernel rather than degrade to the unsupported path.
	token, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if token == "" {
		t.Fatal("StartTime(self) returned an empty identity token")
	}
}

// TestAliveWithCmdlineReportsUnidentifiableLivePID pins the singleton-gate
// contract: a live PID whose identity cannot be read is reported as an error,
// never as a plain "not a match". The false reading is what let a caller
// holding a pidfile start a duplicate poller over a process that was still
// running.
func TestAliveWithCmdlineReportsUnidentifiableLivePID(t *testing.T) {
	matched, err := AliveWithCmdline(os.Getpid(), func([]string) bool { return true })
	if matched {
		t.Fatal("AliveWithCmdline = true with unreadable argv")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("AliveWithCmdline error = %v, want ErrUnsupported", err)
	}
}
