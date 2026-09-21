package acceptancehelpers

import (
	"fmt"
	"os"
	"path/filepath"
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

	// One append per invocation, written as a single small record so
	// concurrent gc processes appending to the same O_APPEND file do not
	// interleave. Failures are swallowed: a test's instrument must not be able
	// to fail the city it is only observing.
	script := fmt.Sprintf(`#!/bin/sh
{
	printf '%%s\037' "${PPID:-0}"
	for arg in "$@"; do printf '%%s\037' "$arg"; done
	printf '\036\n'
} >>%q 2>/dev/null || true
exec %q "$@"
`, recorder.logPath, realBD)
	if err := os.WriteFile(recorder.Path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatalf("write recording bd shim: %v", err)
	}
	return recorder
}

// Invocations returns every recorded fork, oldest first. An absent log means
// no bd ran, which is a legitimate answer and not an error.
func (r *RecordingBD) Invocations() []Invocation {
	r.t.Helper()
	data, err := os.ReadFile(r.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		r.t.Fatalf("read recording bd log: %v", err)
	}
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
		out = append(out, Invocation{PPID: fields[0], Argv: fields[1:]})
	}
	return out
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
