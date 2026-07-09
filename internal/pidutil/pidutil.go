// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const psZombieTimeout = 100 * time.Millisecond

// Alive reports whether a PID exists and is not a zombie.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return !psReportsZombie(pid)
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[2] == "Z" {
		return false
	}
	return true
}

// StartTime returns a PID's start time — field 22 (starttime, in clock ticks
// since boot) of /proc/<pid>/stat — as an opaque token used to disambiguate a
// recycled PID from the original target. The kernel never reuses a (pid,
// starttime) pair for the lifetime of a boot, so a changed start time on the
// same PID proves the original process is gone and an unrelated one now holds
// the number. It returns an error on platforms without /proc (e.g. darwin) or
// when the process record is unreadable; callers treat that as "no identity
// signal available" and fall back to plain liveness.
//
// The comm field (field 2) is wrapped in parens and may itself contain spaces
// and parens, so parsing anchors on the final ')' and counts fields from
// there: field 3 (state) is the first token after "') '", making field 22
// (starttime) the token at index 19 of that suffix.
func StartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pidutil: invalid PID %d", pid)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	stat := string(data)
	rparen := strings.LastIndexByte(stat, ')')
	if rparen < 0 || rparen+2 >= len(stat) {
		return "", fmt.Errorf("pidutil: malformed stat for PID %d", pid)
	}
	fields := strings.Fields(stat[rparen+2:])
	const starttimeIndexAfterComm = 19 // field 22 minus fields 1-3 offset
	if len(fields) <= starttimeIndexAfterComm {
		return "", fmt.Errorf("pidutil: stat for PID %d has %d post-comm fields, want > %d", pid, len(fields), starttimeIndexAfterComm)
	}
	return fields[starttimeIndexAfterComm], nil
}

// AliveWithStartTime reports whether pid is alive AND still the same process
// identified by startTime. It closes the PID-reuse hole in Alive: during a
// post-SIGKILL reap wait the target's PID can be reaped and recycled to an
// unrelated new process inside the window, at which point plain Alive would
// wrongly report the (dead) target as still alive.
//
// An empty startTime disables the identity check and falls back to Alive — used
// on platforms without /proc start-time support (darwin) or when the original
// start time could not be captured before the wait. A non-empty startTime that
// no longer matches means the PID was recycled: the original target is dead, so
// this returns false. When the current start time cannot be read despite Alive
// reporting true (a transient race, no /proc), it keeps the conservative Alive
// answer rather than inventing a death.
func AliveWithStartTime(pid int, startTime string) bool {
	if !Alive(pid) {
		return false
	}
	if startTime == "" {
		return true
	}
	current, err := StartTime(pid)
	if err != nil {
		return true
	}
	return current == startTime
}

// AliveWithCmdline reports whether a PID exists, is not a zombie, and its
// command line satisfies match. On platforms without /proc cmdline support it
// falls back to Alive so callers preserve existing non-Linux behavior.
func AliveWithCmdline(pid int, match func([]string) bool) bool {
	if !Alive(pid) {
		return false
	}
	if match == nil {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	argv, err := Cmdline(pid)
	if err != nil {
		return false
	}
	return match(argv)
}

// ArgvContainsSequence reports whether argv contains seq contiguously.
func ArgvContainsSequence(argv []string, seq ...string) bool {
	if len(seq) == 0 {
		return true
	}
	if len(argv) < len(seq) {
		return false
	}
	for i := 0; i <= len(argv)-len(seq); i++ {
		ok := true
		for j := range seq {
			if argv[i+j] != seq[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// ArgvHasFlagValue reports whether argv contains flag with value, either as
// "--flag value" or "--flag=value".
func ArgvHasFlagValue(argv []string, flag, value string) bool {
	if flag == "" || value == "" {
		return false
	}
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
		if strings.HasPrefix(arg, flag+"=") && strings.TrimPrefix(arg, flag+"=") == value {
			return true
		}
	}
	return false
}

// Cmdline returns a PID's command line from /proc, normalized through
// NormalizeArgv. It returns an error on hosts without /proc cmdline support
// or when the process record is unreadable.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return NormalizeArgv(strings.Split(trimmed, "\x00")), nil
}

// NormalizeArgv returns argv with empty and whitespace-only arguments
// dropped — the rule Cmdline applies to /proc command lines. Callers
// comparing a configured argv against Cmdline output must pass the
// configured side through this helper first so both sides share the same
// argument shape.
func NormalizeArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func psReportsZombie(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psZombieTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.HasPrefix(state, "Z")
}
