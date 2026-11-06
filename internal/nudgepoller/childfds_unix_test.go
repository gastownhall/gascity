//go:build unix

package nudgepoller

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const fdReportHelperEnv = "GC_NUDGEPOLLER_FD_REPORT_HELPER"

const inheritedFDMarker = "inherited-fd="

// TestInheritedFDReportHelper is the child half of the ScrubInheritedFDs
// tests: re-executed with fdReportHelperEnv set, it prints every descriptor
// above stderr that it holds without close-on-exec. After exec such a
// descriptor can only have come from the parent, because the Go runtime opens
// its own descriptors close-on-exec. Without the env var it is a no-op.
func TestInheritedFDReportHelper(t *testing.T) {
	if os.Getenv(fdReportHelperEnv) != "1" {
		t.Skip("helper process for the ScrubInheritedFDs tests")
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("listing /dev/fd: %v", err)
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue // the directory handle ReadDir itself held
		}
		if err != nil {
			t.Fatalf("fcntl(%d, F_GETFD): %v", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			fmt.Printf("%s%d\n", inheritedFDMarker, fd)
		}
	}
}

// plantInheritableFD opens a lock file and returns a duplicate of it without
// close-on-exec, the shape of a descriptor a shell caller leaves behind with
// `exec 9>lock; flock 9`. Go itself never opens descriptors this way.
func plantInheritableFD(t *testing.T) int {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "held-*.lock")
	if err != nil {
		t.Fatalf("creating lock file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup lock file: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

// fdReportCommand returns an unstarted command that re-executes this test
// binary as the fd-reporting child.
func fdReportCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	return &exec.Cmd{
		Path: os.Args[0],
		Args: []string{os.Args[0], "-test.run=^TestInheritedFDReportHelper$", "-test.count=1"},
		Env:  append(os.Environ(), fdReportHelperEnv+"=1"),
	}
}

// reportInheritedFDs runs cmd and returns the descriptors its child reported.
func reportInheritedFDs(t *testing.T, cmd *exec.Cmd) []int {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("running fd-report child: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	var fds []int
	for _, line := range strings.Split(stdout.String(), "\n") {
		value, ok := strings.CutPrefix(line, inheritedFDMarker)
		if !ok {
			continue
		}
		fd, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("unparsable fd report %q", line)
		}
		fds = append(fds, fd)
	}
	return fds
}

// Kills: a detached poller that keeps a descriptor its caller held open
// without close-on-exec, such as the file behind a caller's flock. The poller
// outlives its caller by design, so a leaked descriptor pins that caller's
// lock until the poller exits. The unscrubbed run is the control: it proves
// the harness sees a leaked descriptor, so the scrubbed run's empty report
// is not vacuous.
func TestScrubInheritedFDsClosesCallerDescriptors(t *testing.T) {
	planted := plantInheritableFD(t)

	if got := reportInheritedFDs(t, fdReportCommand(t)); !slices.Contains(got, planted) {
		t.Fatalf("control: unscrubbed child reported fds %v, want it to inherit planted fd %d", got, planted)
	}

	cmd := fdReportCommand(t)
	if err := ScrubInheritedFDs(cmd); err != nil {
		t.Fatalf("ScrubInheritedFDs: %v", err)
	}
	if got := reportInheritedFDs(t, cmd); len(got) != 0 {
		t.Fatalf("scrubbed child still holds inherited fds %v (planted fd %d)", got, planted)
	}
}

// Kills: a scrub that leaves the planted descriptor's slot filled or out of
// range, which would hand the child a live file instead of a closed slot.
func TestScrubInheritedFDsLeavesPlantedSlotClosed(t *testing.T) {
	planted := plantInheritableFD(t)
	cmd := fdReportCommand(t)
	if err := ScrubInheritedFDs(cmd); err != nil {
		t.Fatalf("ScrubInheritedFDs: %v", err)
	}
	slot := planted - 3
	if slot >= len(cmd.ExtraFiles) {
		t.Fatalf("ExtraFiles has %d slots, want to cover planted fd %d", len(cmd.ExtraFiles), planted)
	}
	for i, f := range cmd.ExtraFiles {
		if f != nil {
			t.Fatalf("ExtraFiles[%d] = %v, want every slot nil (closed in the child)", i, f)
		}
	}
}

// Kills: a scrub that silently replaces files a caller deliberately passed
// through ExtraFiles. The poller passes none, so a non-empty list is a
// programming error to surface, not to overwrite.
func TestScrubInheritedFDsRefusesExistingExtraFiles(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "extra-*")
	if err != nil {
		t.Fatalf("creating extra file: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	cmd := fdReportCommand(t)
	cmd.ExtraFiles = []*os.File{f}
	if err := ScrubInheritedFDs(cmd); err == nil {
		t.Fatalf("ScrubInheritedFDs accepted a command that already has ExtraFiles")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != f {
		t.Fatalf("ScrubInheritedFDs modified the caller's ExtraFiles: %v", cmd.ExtraFiles)
	}
}
