// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ps deadlines exist to stop a WEDGED ps from stalling a caller forever; they
// are not latency budgets. That distinction matters because ps cost scales with
// the whole process table, not with the pids asked about: procps rebuilds its
// view of /proc before it filters, so `ps -p <pid>` costs the same as `ps -ax`.
// Measured on a 17k-process host, both take ~1.1s — so the original 1s budget
// turned a healthy-but-busy machine into "signal: killed" on every probe, which
// the identity checks then read as "cannot confirm".
//
// Linux never pays this: Alive, StartTime, Cmdline and ChildPIDs all read /proc
// directly for the pid in question and only reach ps where /proc is absent
// (darwin). The budget below therefore governs the darwin fallback, where a
// large process table is exactly as likely, so it is sized for one rather than
// against it. GC_PIDUTIL_PS_TIMEOUT overrides it for constrained hosts.
const (
	defaultPSTimeout = 10 * time.Second
	minPSTimeout     = time.Second
)

// psTimeout returns the deadline for a ps probe, honoring
// GC_PIDUTIL_PS_TIMEOUT and refusing anything below the floor so a
// misconfiguration cannot reintroduce the truncation this replaced.
//
// This is an operator escape hatch in the same family as GC_HOOK_CLAIM_WINDOW
// (cmd/gc/cmd_hook_claim.go): a Go duration read straight from the environment,
// where an unparseable or out-of-range value falls back to the default rather
// than weakening the guard — the direction that stays safe. It is deliberately
// NOT an internal/rollout gate: a rollout Spec selects between two mechanical
// code paths and carries a ConfigPath, Expires and VersionAnchor for its
// graduation, and this knob has none of those. It tunes one permanent bound.
func psTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GC_PIDUTIL_PS_TIMEOUT"))
	if raw == "" {
		return defaultPSTimeout
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < minPSTimeout {
		return defaultPSTimeout
	}
	return parsed
}

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
	state, ok := procStatState(string(data))
	if ok && state == "Z" {
		return false
	}
	return true
}

// procStatState returns field 3 (state) from a Linux /proc/<pid>/stat row.
// Field 2 (comm) may contain spaces and parentheses, so the state is the first
// token after comm's final closing parenthesis, not the third whitespace token.
func procStatState(stat string) (string, bool) {
	lparen := strings.IndexByte(stat, '(')
	rparen := strings.LastIndexByte(stat, ')')
	if lparen <= 0 || rparen <= lparen ||
		!isASCIIWhitespace(stat[lparen-1]) ||
		rparen+1 >= len(stat) || !isASCIIWhitespace(stat[rparen+1]) {
		return "", false
	}
	parsedPID, err := strconv.Atoi(strings.TrimSpace(stat[:lparen]))
	if err != nil || parsedPID <= 0 {
		return "", false
	}
	fields := strings.Fields(stat[rparen+1:])
	if len(fields) == 0 || len(fields[0]) != 1 {
		return "", false
	}
	return fields[0], true
}

func isASCIIWhitespace(b byte) bool {
	return b == ' ' || b >= '\t' && b <= '\r'
}

// StartTime returns a PID's start time — field 22 (starttime, in clock ticks
// since boot) of /proc/<pid>/stat — as an opaque token used to disambiguate a
// recycled PID from the original target. The kernel never reuses a (pid,
// starttime) pair for the lifetime of a boot, so a changed start time on the
// same PID proves the original process is gone and an unrelated one now holds
// the number. Where /proc is unavailable (e.g. darwin) it falls back to ps,
// which reports a wall-clock start date rather than jiffies; the token is
// opaque and only ever compared against another read the same way on the same
// host, so the differing format does not matter. It returns an error only when
// neither mechanism can answer; callers treat that as "no identity signal
// available" and fall back to plain liveness.
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
		return psStartTime(pid)
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
// when the original start time could not be captured before the wait. A
// non-empty startTime that no longer matches means the PID was recycled: the
// original target is dead, so this returns false. When the current start time
// cannot be read despite Alive reporting true (a transient race, or a host
// where neither /proc nor ps can answer), it keeps the conservative Alive
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
// command line satisfies match.
//
// It used to return true unconditionally off Linux, because Cmdline read only
// /proc. That turned an identity check into a bare existence check on those
// hosts: callers use this to decide whether the PID in a pidfile is still THEIR
// process, so a recycled PID owned by an unrelated live process passed the
// check, and the caller skipped work it should have done. Cmdline is portable
// now, so the platform branch is gone.
//
// An unreadable argv yields false — never a match. Callers treat "not my
// process" as "do the work", which is the recoverable direction.
func AliveWithCmdline(pid int, match func([]string) bool) bool {
	if !Alive(pid) {
		return false
	}
	if match == nil {
		return false
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

// Cmdline returns a PID's command line, normalized through NormalizeArgv.
// It reads /proc/<pid>/cmdline where available, kern.procargs2 on Darwin, and
// otherwise falls back to ps. It returns an error when no mechanism can read
// the process record.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return platformCmdline(pid)
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

// procChildPIDs reads a parent's direct children from
// /proc/<pid>/task/<tid>/children, which the kernel maintains per thread. The
// cost is proportional to the pids asked about rather than to the process
// table, so it does not degrade on a busy host the way ps does — the whole
// reason the ps path needs a generous deadline.
//
// Reports ok=false where /proc is absent (darwin) or the children file is not
// exposed (CONFIG_PROC_CHILDREN off), so the caller falls back to ps. A parent
// that genuinely has no children reads as an empty file, which is ok=true with
// no pids — distinguishable from "cannot answer" only because the task
// directory itself was readable.
func procChildPIDs(parent int) ([]int, bool) {
	taskDir := filepath.Join("/proc", strconv.Itoa(parent), "task")
	threads, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, false
	}

	var (
		children []int
		answered bool
	)
	seen := make(map[int]bool)
	for _, thread := range threads {
		data, err := os.ReadFile(filepath.Join(taskDir, thread.Name(), "children"))
		if err != nil {
			continue
		}
		answered = true
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			children = append(children, pid)
		}
	}
	if !answered {
		return nil, false
	}
	return children, true
}

// ChildPIDs returns the pids of all live direct child processes of parent,
// enumerated portably via `ps -axo pid=,ppid=` rather than a /proc walk, so
// it works on darwin as well as linux. It returns an error when the ps
// invocation itself fails or times out, so callers can tell "enumeration
// ran and found nothing" apart from "enumeration did not run" — collapsing
// the two into an empty slice would let an unavailable check masquerade as
// a clean result.
//
// ps is itself alive, and a child of the caller, at the instant it captures
// the process table — so a caller checking its own children (parent ==
// os.Getpid(), the pattern this package's callers use for self leak checks)
// always sees ps's own transient pid/ppid row alongside any real children.
// The enumeration helper's own pid is excluded below so it can never
// masquerade as a leaked child.
func ChildPIDs(parent int) ([]int, error) {
	if children, ok := procChildPIDs(parent); ok {
		return children, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), psTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pidutil: ps enumeration failed: %w", err)
	}
	selfPID := -1
	if cmd.Process != nil {
		selfPID = cmd.Process.Pid
	}

	var children []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, errPID := strconv.Atoi(fields[0])
		ppid, errPPID := strconv.Atoi(fields[1])
		if errPID != nil || errPPID != nil {
			continue
		}
		if pid == selfPID {
			continue
		}
		if ppid == parent {
			children = append(children, pid)
		}
	}
	return children, nil
}

// psStartTime reads a PID's start time with ps, for hosts without /proc.
//
// The two mechanisms return different formats — /proc gives jiffies since boot,
// ps gives a wall-clock date — and that is fine, because the identity check only
// ever compares a value captured earlier against one read later on the SAME
// host, so the same mechanism produces both. The values are never compared
// across platforms.
//
// One granularity limitation: ps -o lstart= has one-second resolution, so a PID
// recycled within the same second as its predecessor started would compare equal
// and the reuse would go undetected. That is strictly narrower than the window
// the check closes today, where the identity check does not run at all off
// Linux, and the consequence of a miss is the pre-existing conservative answer
// rather than a wrong death.
func psStartTime(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout())
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("reading start time for pid %d via ps: %w", pid, err)
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", fmt.Errorf("no start time reported for pid %d", pid)
	}
	return identity, nil
}

// psCmdline reads a PID's argv with ps on non-Darwin hosts without /proc.
// ps renders argv as a single space-joined string, so it cannot preserve an
// argument containing spaces. Darwin uses kern.procargs2 instead.
//
// -ww asks ps for full width, since a truncated argv fails the match on BSD ps.
func psCmdline(pid int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout())
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, fmt.Errorf("reading argv for pid %d via ps: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return nil, fmt.Errorf("no argv reported for pid %d", pid)
	}
	return NormalizeArgv(fields), nil
}

func psReportsZombie(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout())
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.HasPrefix(state, "Z")
}
