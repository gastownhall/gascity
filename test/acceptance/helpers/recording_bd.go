package acceptancehelpers

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The delimiters of one recorded invocation: ASCII unit separator between
// fields, ASCII record separator after the last one.
//
// Neither a newline nor a space can do this job. A bd argv routinely carries
// both — a bead title, a JSON payload on `bd create` — and a line-oriented
// record silently turns one such invocation into two, which is a fork count
// that overreports exactly when the city is doing real work. The two ASCII
// separators cannot appear in an argument gc or its provider script builds.
const (
	fieldSeparator  = "\x1f"
	recordSeparator = "\x1e"
)

// recordingBDRecordDir is the directory, beside the shim, that holds one file
// per recorded invocation.
const recordingBDRecordDir = "invocations.d"

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

	t         *testing.T
	recordDir string
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

// NewRecordingBD writes a shim that records into its own directory and execs
// realBD.
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
	recorder := &RecordingBD{
		Path:      filepath.Join(dir, "bd"),
		Real:      realBD,
		t:         t,
		recordDir: filepath.Join(dir, recordingBDRecordDir),
	}
	if err := os.MkdirAll(recorder.recordDir, 0o755); err != nil {
		t.Fatalf("create recording bd directory: %v", err)
	}

	// ONE file per invocation, so no two forks ever write to the same file.
	//
	// A shared append-only log cannot be made safe from sh. O_APPEND makes one
	// write(2) atomic, and the record is assembled first and emitted with one
	// printf, but how many writes that printf becomes is the shell's choice:
	// dash issues one, while bash — /bin/sh on macOS and Fedora — writes in
	// 4096-byte chunks. gc builds argv far over 4 KiB (bead bodies ride
	// `--description`, the reaper's `bd sql` carries its whole statement), so
	// two such forks in flight spliced their chunks and the log could no longer
	// be counted. A file of its own per fork removes that bound altogether.
	//
	// The file is named for the shell's pid, which no other live process holds,
	// so no concurrent fork can choose the same name; the suffix only steps past
	// a file left by an exited process that held the same pid earlier. Naming it
	// costs no fork: a counter, `[` and `$((…))` are builtins in dash and in
	// bash 3.2, and no mktemp, date or mv runs. A fork inside the instrument
	// would be a process the census cannot see.
	//
	// A file is a record once its record separator has landed. A reader that
	// meets one still being written (a bash chunk at a time) sees no separator
	// yet and leaves it for the next read: that bd has not been exec'd, so it
	// has not forked yet either. Invocations depends on that, not on a rename.
	//
	// Failures are swallowed: a test's instrument must not be able to fail the
	// city it is only observing.
	script := fmt.Sprintf(`#!/bin/sh
us='%s'
rs='%s'
dir=%s
record="${PPID:-0}$us"
for arg in "$@"; do
	record="$record$arg$us"
done
n=0
while [ -e "$dir/$$.$n" ]; do
	n=$((n + 1))
done
printf '%%s\n' "$record$rs" >"$dir/$$.$n" 2>/dev/null || true
exec %s "$@"
`, fieldSeparator, recordSeparator, shellQuote(recorder.recordDir), shellQuote(realBD))
	if err := os.WriteFile(recorder.Path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatalf("write recording bd shim: %v", err)
	}
	return recorder
}

// Invocations returns every recorded fork, oldest first as far as the
// filesystem's modification times resolve. No records means no bd ran, which
// is a legitimate answer and not an error.
//
// A record that does not parse fails the test. A fork census that silently
// dropped one would be worse than no census: the count the gate reads would be
// a number nobody can reproduce.
func (r *RecordingBD) Invocations() []Invocation {
	r.t.Helper()
	entries, err := os.ReadDir(r.recordDir)
	if err != nil {
		r.t.Fatalf("read recording bd records: %v", err)
	}
	type recorded struct {
		name       string
		modified   time.Time
		invocation Invocation
	}
	var records []recorded
	for _, entry := range entries {
		path := filepath.Join(r.recordDir, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				// Reset raced this read; the record is gone by design.
				continue
			}
			r.t.Fatalf("read recording bd record %s: %v", path, readErr)
		}
		invocation, complete, parseErr := parseInvocation(data)
		if parseErr != nil {
			r.t.Fatalf("recording bd record %s: %v", path, parseErr)
		}
		if !complete {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				continue
			}
			r.t.Fatalf("stat recording bd record %s: %v", path, infoErr)
		}
		records = append(records, recorded{name: entry.Name(), modified: info.ModTime(), invocation: invocation})
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].modified.Equal(records[j].modified) {
			return records[i].modified.Before(records[j].modified)
		}
		return records[i].name < records[j].name
	})
	out := make([]Invocation, 0, len(records))
	for _, record := range records {
		out = append(out, record.invocation)
	}
	return out
}

// parseInvocation decodes one record file in the shim's wire format: ppid, then
// argv, every field unit-separated, the record separator last, and the newline
// the shim writes for readability.
//
// complete is false while the record separator has not landed — a shell that
// writes in chunks is still writing it — and that is not an error. A file that
// holds anything other than exactly one record whose first field is a pid is
// corrupt, and is.
func parseInvocation(data []byte) (invocation Invocation, complete bool, err error) {
	content := string(data)
	end := strings.Index(content, recordSeparator)
	if end < 0 {
		return Invocation{}, false, nil
	}
	if rest := strings.TrimLeft(content[end+len(recordSeparator):], "\n"); rest != "" {
		return Invocation{}, false, fmt.Errorf("the file carries %d byte(s) after its record; one invocation's file holds exactly one record", len(rest))
	}
	fields := strings.Split(strings.TrimSuffix(content[:end], fieldSeparator), fieldSeparator)
	if _, convErr := strconv.Atoi(fields[0]); convErr != nil {
		return Invocation{}, false, fmt.Errorf("the record's first field %.64q is not a pid", fields[0])
	}
	return Invocation{PPID: fields[0], Argv: fields[1:]}, true, nil
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
//
// It empties the record directory rather than replacing it: a fork that lands
// while the directory was momentarily missing would go unrecorded.
func (r *RecordingBD) Reset() {
	r.t.Helper()
	entries, err := os.ReadDir(r.recordDir)
	if err != nil {
		r.t.Fatalf("reset recording bd records: %v", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(r.recordDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			r.t.Fatalf("reset recording bd records: %v", err)
		}
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
