package beads

import (
	"errors"
	"fmt"
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
// BOTH bd pins predate the flags, so the raw-SQL path stays as the fallback,
// selected by a per-store latch the first time bd rejects the flag as unknown:
// the contract-tested minimum (deps.env BD_PREV_VERSION, 1.0.4) and — the
// load-bearing one — the installable default CI and operators actually install
// (deps.env BD_VERSION, 1.1.0). The flags landed on beads main after that
// release, in beads#5008, and the only release carrying them so far is a
// prerelease (v1.2.1), below the published bar BD_VERSION holds, so on a stock
// install the FALLBACK is still the live path. The verb is exercised
// against the source-built deps.env BD_CURRENT_REF bd
// (make test-bd-conditional-release-contract).

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
// verb, after checking that id names exactly the bead bd resolves
// (releaseIDCollision).
//
// handled=false means this bd does not support the verb and the caller must take
// the raw-SQL fallback; it is the ONLY value that causes a fallback, so a real
// backend failure can never be mistaken for an old bd.
func (s *BdStore) releaseIfCurrentViaBdVerb(id, expectedAssignee string) (released, handled bool, err error) {
	if collision := s.releaseIDCollision(id); collision != nil {
		return false, true, collision
	}
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
	if isBdIssueNotFound(runErr) {
		// A bead this store cannot resolve can hold no assignment, which is the
		// same verdict the SQL statement reached by matching zero rows. Kept so
		// the observable contract does not change with the backend verb.
		return false, true, nil
	}
	return false, true, runErr
}

// releaseIDCollision reports bd having resolved id to a DIFFERENT bead.
//
// The statement this verb replaced was `WHERE id = <literal>` — exact by
// construction. bd's resolver is not: with no exact hit it falls back to
// prefix/substring matching, so `bd update tst-d6 --if-assignee worker-1 …`
// mutates tst-d6e and reports success. The CAS preconditions are no defense,
// because bd evaluates them against the bead it resolved rather than the one the
// caller named, and a worker usually holds several beads, so a sibling satisfies
// them. That is the incident shape gcy-g4o records ("gcy-dv7" → "gcy-wisp-dv78"),
// and gascity is systematically exposed to it because formula steps run as
// <prefix>-wisp-<suffix> wisps.
//
// Get already enforces the exact match and reports ErrIDCollision, so the guard
// costs one read. Only a confirmed collision blocks the release: a plain
// ErrNotFound falls through (the bead may be invisible to the read seam, and the
// verb reaches the same "not held" verdict anyway), and a failed read falls
// through rather than converting a broken read into a refused write — the
// fail-open tradeoff cmd_bd.go's pre-flight guard documents.
//
// Only the verb path needs this; the raw-SQL fallback still matches id
// literally.
func (s *BdStore) releaseIDCollision(id string) error {
	if _, err := s.Get(id); errors.Is(err, ErrIDCollision) {
		return fmt.Errorf("refusing to release %q: %w", id, err)
	}
	return nil
}

// bdIssueNotFoundPhrases are bd's wordings for "that id resolves to no issue".
// Measured against bd: `bd update tst-nosuchbead --status open` exits 1 with
// `Error resolving tst-nosuchbead: no issue found matching "tst-nosuchbead"`,
// and its --json envelope carries the plural form.
var bdIssueNotFoundPhrases = []string{
	"no issue found",
	"no issues found",
	"issue not found",
}

// isBdIssueNotFound reports whether err is bd refusing to resolve the bead id.
//
// It deliberately does not use the package-wide isBdNotFound, whose bare
// "not found" substring is unanchored and matched against the whole runner
// error. Everywhere else that helper produces an ERROR (ErrNotFound); here the
// verdict is a CAS ANSWER — "nobody holds it, do not retry" — so a bd that fell
// off PATH (`exec: "bd": executable file not found in $PATH`), a renamed
// database (`database "fe" not found on Dolt server …`) or a gateway's
// `404 page not found` would each be answered as a clean refusal instead of
// surfacing as the infrastructure failure it is. The replaced SQL path reported
// every one of those as an error, and a CAS verdict invented out of a dead
// backend is the exact failure this conversion exists to remove.
//
// The gate is numeric first: bd exits 1 for an unresolvable id (13 is reserved
// for a rejected precondition), so a spawn failure — which carries no process
// exit status at all — can never reach this verdict.
func isBdIssueNotFound(err error) bool {
	if err == nil || bdExitCode(err) != 1 {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, phrase := range bdIssueNotFoundPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// exitCoder is the process-status surface bdExitCode reads. *exec.ExitError —
// what the runner actually wraps — satisfies it through its embedded
// *os.ProcessState. Matching the method instead of the concrete type lets the
// CAS classifier be driven from a unit test without spawning a process, which
// the untagged test-resource ledger forbids (TESTING.md: "untagged subprocess
// call/file totals cannot grow"). The assertion below is the link back to
// reality: if bd's runner error ever stops carrying its exit status this way,
// this file stops compiling.
type exitCoder interface{ ExitCode() int }

var _ exitCoder = (*exec.ExitError)(nil)

// bdExitCode returns the process exit status carried by err, or -1 when err is
// not a process failure. The runner wraps the *exec.ExitError with %w
// (classifyBDExecResult), so the status survives to here.
func bdExitCode(err error) int {
	var coder exitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode()
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
