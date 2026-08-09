package beads

import (
	"errors"
	"os/exec"
	"strings"
)

// This file consumes bd's native conditional-release verb, which is what the
// ReleaseIfCurrent SEAM was waiting for.
//
// The release is a compare-and-swap: clear an in-progress assignment only while
// the bead still carries the expected assignee. It used to be assembled as raw
// SQL — `UPDATE issues SET status='open', assignee='' WHERE id=… AND
// status='in_progress' AND assignee=…` — and the verdict was read out of
// rows_affected. That had three costs: the statement had to be built by
// concatenating hand-escaped MySQL string literals, `bd sql` is rejected outright
// in embedded mode (so the path needed a second implementation shelling directly
// to `dolt sql`), and a CAS that matches zero rows is indistinguishable from one
// that could not run.
//
// bd now expresses the same two preconditions natively:
//
//	bd update <id> --if-assignee <expected> --if-status in_progress \
//	               --status open --assignee ""
//
// and — unlike every other bd failure, which is exit 1 with a message — reports
// a failed precondition as its own exit status, 13, having written nothing. The
// CAS verdict is therefore a NUMBER, not a parse of bd's prose. That is the
// whole reason to prefer this verb: the load-bearing branch stops depending on
// message text.
//
// bd 1.0.4 is the contract-tested minimum (deps.env BD_PREV_VERSION) and predates
// the flags, so the raw-SQL path stays as the fallback and is selected by a
// per-store latch the first time bd rejects the flag as unknown.

// bdCASPreconditionExitCode is bd's dedicated exit status for a rejected
// --if-assignee / --if-status precondition: nothing was written, and the verdict
// is "the precondition no longer held", not "the command failed". Every other bd
// failure, including an unresolvable id, is exit 1.
const bdCASPreconditionExitCode = 13

// conditionalReleaseUnsupported reports whether this store has already seen bd
// reject the conditional-release flags. The latch is one-way: a bd that lacks
// the flags cannot grow them inside one process lifetime, and re-probing on
// every release would pay a failed subprocess per call.
func (s *BdStore) conditionalReleaseUnsupported() bool {
	s.condReleaseMu.Lock()
	defer s.condReleaseMu.Unlock()
	return s.condReleaseLatchedUnsupported
}

// latchConditionalReleaseUnsupported records that bd does not understand the
// conditional-release flags, sending every later release straight to the SQL
// fallback.
func (s *BdStore) latchConditionalReleaseUnsupported() {
	s.condReleaseMu.Lock()
	defer s.condReleaseMu.Unlock()
	s.condReleaseLatchedUnsupported = true
}

// releaseIfCurrentViaBdVerb performs the conditional release through bd's native
// verb.
//
// handled=false means this bd does not support the verb and the caller must take
// the raw-SQL fallback; it is the ONLY value that causes a fallback, so a real
// backend failure can never be mistaken for an old bd.
func (s *BdStore) releaseIfCurrentViaBdVerb(id, expectedAssignee string) (released, handled bool, err error) {
	// --assignee "" is how bd clears an assignment; the paired --status open
	// completes the same swap the SQL statement performed, and both --if-* flags
	// carry the preconditions bd now evaluates server-side.
	out, runErr := s.runBDTransientWriteOutput(
		"update", id,
		"--if-assignee", expectedAssignee,
		"--if-status", "in_progress",
		"--status", "open",
		"--assignee", "",
	)
	if runErr == nil {
		return true, true, nil
	}
	if bdExitCode(runErr) == bdCASPreconditionExitCode {
		// The precondition no longer held. bd wrote nothing, and the caller must
		// read this as an authoritative "someone else holds it", not an error.
		return false, true, nil
	}
	detail := strings.TrimSpace(string(out)) + " " + runErr.Error()
	if isBdUnknownFlagError(detail, "--if-assignee") || isBdUnknownFlagError(detail, "--if-status") {
		return false, false, nil
	}
	if isBdNotFound(runErr) {
		// A bead this store cannot resolve can hold no assignment, which is the
		// same verdict the SQL statement reached by matching zero rows. Kept so
		// the observable contract does not change with the backend verb.
		return false, true, nil
	}
	return false, true, runErr
}

// bdExitCode returns the process exit status carried by err, or -1 when err is
// not a process failure. The runner wraps the *exec.ExitError with %w
// (classifyBDExecResult), so the status survives to here.
func bdExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// isBdUnknownFlagError matches the usage error bd's flag parser emits for a flag
// this build does not have.
//
// It is ANCHORED to the flag name immediately following the parser's marker. A
// cobra usage echo lists every flag in its help block on ANY flag error, so a
// floating "contains --if-assignee" check would latch a CAPABLE bd the moment
// gascity passed some unrelated bad flag — the exact silent degrade the latch
// exists to avoid. isBdUnknownIfRevisionFlag applies the same rule to the
// revision-CAS probe.
func isBdUnknownFlagError(msg, flag string) bool {
	flag = strings.ToLower(strings.TrimLeft(flag, "-"))
	if flag == "" {
		return false
	}
	lower := strings.ToLower(msg)
	for _, anchor := range []string{
		"unknown flag: --" + flag,
		"unknown flag: -" + flag,
		"unknown flag '--" + flag + "'",
		"flag provided but not defined: -" + flag,
		"flag provided but not defined: --" + flag,
	} {
		if strings.Contains(lower, anchor) {
			return true
		}
	}
	return false
}
