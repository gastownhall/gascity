package acceptancehelpers

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The delimiters of one recorded invocation: ASCII unit separator between
// fields, ASCII record separator between invocations.
//
// Neither a newline nor a space can do this job. A bd argv routinely carries
// both — a bead title, a JSON payload on `bd create` — and a line-oriented log
// silently turns one such invocation into two records, which is a fork count
// that overreports exactly when the city is doing real work. The two ASCII
// separators cannot appear in an argument gc or its provider script builds.
const (
	fieldSeparator  = "\x1f"
	recordSeparator = "\x1e"
)

// RecordingBD is a bd that records every invocation and then execs the real
// one.
//
// It exists because gc forks bd from two places and neither one alone is a
// census. The in-process calls go through internal/beads and are traced there;
// the provider script's calls come from a separate process that traces nothing.
// Both resolve the executable through BD_BIN, so substituting it here is the
// single point every fork passes through, whatever spawned it — which is the
// property a fork-count gate needs and a source-level trace cannot have.
//
// The recording is a side effect of a real bd run, not a replacement for one:
// the shim execs the pinned binary, so the city under test behaves exactly as
// it would without it.
type RecordingBD struct {
	// Path is the shim, to be handed to gc as BD_BIN.
	Path string
	// Real is the bd the shim execs.
	Real string

	t       *testing.T
	logPath string
}

// Invocation is one recorded bd fork.
type Invocation struct {
	// Argv is the command line as bd received it, without argv[0].
	Argv []string
	// PPID is the process that forked it, when the shell reported one. It is
	// what separates a fork the controller made from one a provider script
	// made under it.
	PPID string
}

// Subcommand is the bd verb, or "" for a bare `bd`.
func (i Invocation) Subcommand() string {
	if len(i.Argv) == 0 {
		return ""
	}
	return i.Argv[0]
}

// NewRecordingBD writes a shim that records into its own log and execs realBD.
//
// realBD must already be resolved: the shim takes no part in deciding which bd
// runs, because a shim that resolved its own target could silently run a
// different binary than the test believes it pinned.
func NewRecordingBD(t *testing.T, realBD string) *RecordingBD {
	t.Helper()
	if strings.TrimSpace(realBD) == "" {
		t.Fatal("NewRecordingBD needs a resolved bd path")
	}
	// The shim lives alone in its own directory and is named "bd": gc carries
	// BD_BIN into provider script environments verbatim, and a shim named
	// anything else would make every argv[0] in the process table read as a
	// binary nobody pinned.
	dir := filepath.Join(TempDir(t), "recording-bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create recording bd directory: %v", err)
	}
	recorder := &RecordingBD{
		Path:    filepath.Join(dir, "bd"),
		Real:    realBD,
		t:       t,
		logPath: filepath.Join(dir, "invocations.log"),
	}

	// ONE write(2) per invocation, not one per field.
	//
	// O_APPEND makes a single write atomic; it does not make a block of them
	// atomic, and the block this used to be issued one write per printf —
	// strace confirms N+2 of them for an N-argument fork. Two bd forks in
	// flight (the daemon's reconciler cascade plus an operator command, or a
	// provider `health` racing an in-process call) therefore interleaved their
	// fields, and Invocations() read the result as one record with a foreign
	// ppid spliced into its argv plus one empty record — so Count() silently
	// under-reported by one, exactly when the city was doing real work. The
	// deterministic fork gate the next slice builds on Count() cannot stand on
	// that.
	//
	// The record is assembled in a variable and emitted with ONE printf. What
	// that buys is bounded by the shell's stdout buffer, not by PIPE_BUF, and
	// this used to claim otherwise. Measured with strace on a 20 000-byte
	// record:
	//
	//   - /bin/dash: one write(1, …, 20000) for the record plus a separate
	//     write of the trailing newline. Records of any size are one write;
	//     Invocations() already treats the newline as belonging to no record.
	//   - /bin/bash standing in as /bin/sh (Fedora, macOS): five writes in
	//     4096-byte chunks. Two concurrent forks whose argv exceeds 4 KiB can
	//     interleave chunks there.
	//
	// gc does build argv over 4 KiB — bead bodies ride `--description`
	// (internal/beads/bdstore.go) — so that is an ordinary size, not an exotic
	// one. The limitation is therefore stated rather than papered over, and
	// Invocations() refuses to guess when it meets an interleaved record instead
	// of quietly under-counting. A per-invocation file would remove the bound
	// altogether; it would also cost a directory scan per assertion, which is
	// the trade PR2's deterministic gate can make if it ever runs where /bin/sh
	// is bash.
	//
	// The separators are literal bytes rather than printf escapes so that
	// assembling a record costs no subshell: a fork per invocation inside the
	// instrument would be a process the fork census cannot see.
	//
	// The bytes on disk are unchanged (ppid US arg US … US RS newline), so
	// Invocations parses exactly what it parsed before.
	//
	// Failures are swallowed: a test's instrument must not be able to fail the
	// city it is only observing.
	script := fmt.Sprintf(`#!/bin/sh
us='%s'
rs='%s'
record="${PPID:-0}$us"
for arg in "$@"; do
	record="$record$arg$us"
done
printf '%%s\n' "$record$rs" >>%s 2>/dev/null || true
exec %s "$@"
`, fieldSeparator, recordSeparator, shellQuote(recorder.logPath), shellQuote(realBD))
	if err := os.WriteFile(recorder.Path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatalf("write recording bd shim: %v", err)
	}
	return recorder
}

// Invocations returns every recorded fork, oldest first. An absent log means
// no bd ran, which is a legitimate answer and not an error.
//
// A record whose first field is not a pid is an interleaved write (see
// NewRecordingBD: only possible where /bin/sh chunks a printf larger than its
// stdout buffer, with two such forks in flight) and fails the test. A fork
// census that silently dropped one would be worse than no census: the count the
// gate reads would be a number nobody can reproduce.
func (r *RecordingBD) Invocations() []Invocation {
	r.t.Helper()
	data, err := os.ReadFile(r.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		r.t.Fatalf("read recording bd log: %v", err)
	}
	invocations, parseErr := parseInvocations(data)
	if parseErr != nil {
		r.t.Fatalf("recording bd log %s: %v", r.logPath, parseErr)
	}
	return invocations
}

// parseInvocations decodes the shim's wire format. It is separated from
// Invocations so that the refusal above has a test which does not have to fail
// one.
func parseInvocations(data []byte) ([]Invocation, error) {
	var out []Invocation
	for _, record := range strings.Split(string(data), recordSeparator) {
		// The shim writes a newline after each record separator so the log is
		// still readable by eye; it belongs to neither record.
		record = strings.TrimPrefix(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(record, fieldSeparator), fieldSeparator)
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		if _, convErr := strconv.Atoi(fields[0]); convErr != nil {
			return nil, fmt.Errorf("a record's first field %q is not a pid: two bd forks with argv over the shell's output buffer interleaved their writes, so this log's fork count cannot be trusted", fields[0])
		}
		out = append(out, Invocation{PPID: fields[0], Argv: fields[1:]})
	}
	return out, nil
}

// Count returns how many recorded invocations begin with prefix. Calling it
// with no prefix counts every bd fork.
func (r *RecordingBD) Count(prefix ...string) int {
	r.t.Helper()
	count := 0
	for _, invocation := range r.Invocations() {
		if invocationHasPrefix(invocation.Argv, prefix) {
			count++
		}
	}
	return count
}

// Reset discards the recorded history, so a count can be attributed to one
// step of a test rather than to everything that ran before it.
func (r *RecordingBD) Reset() {
	r.t.Helper()
	if err := os.Remove(r.logPath); err != nil && !os.IsNotExist(err) {
		r.t.Fatalf("reset recording bd log: %v", err)
	}
}

// Describe renders the recorded forks for a failure message, so an assertion
// on a count says which commands produced it.
func (r *RecordingBD) Describe() string {
	invocations := r.Invocations()
	if len(invocations) == 0 {
		return "(no bd invocations recorded)"
	}
	lines := make([]string, 0, len(invocations))
	for _, invocation := range invocations {
		lines = append(lines, fmt.Sprintf("  ppid=%s bd %s", invocation.PPID, strings.Join(invocation.Argv, " ")))
	}
	return strings.Join(lines, "\n")
}

// shellQuote renders s as one POSIX shell word.
//
// Go's %q is not shell quoting and was standing in for it. It leaves `$` and
// backticks unescaped inside the double quotes it produces — so a bd path or a
// TMPDIR containing either would be expanded or command-substituted by sh — and
// it renders non-ASCII and control bytes as \xNN/\uNNNN, which sh does not
// decode inside double quotes at all. Single quotes suspend every expansion sh
// performs, and the one character they cannot carry is closed, escaped and
// reopened: the repo's existing idiom (internal/runtime/ssh, internal/runtime/exec).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// invocationHasPrefix reports whether argv begins with prefix.
func invocationHasPrefix(argv, prefix []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for i, want := range prefix {
		if argv[i] != want {
			return false
		}
	}
	return true
}
