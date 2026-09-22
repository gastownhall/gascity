package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordingBDShimExecsThePinnedBinary pins the property that makes the
// shim safe to leave in a real acceptance city: it records and then hands off
// to the bd the test pinned, resolving nothing of its own. A shim that looked
// up bd for itself could run a different binary than the test believes it
// pinned, and every assertion downstream would be about the wrong bd.
func TestRecordingBDShimExecsThePinnedBinary(t *testing.T) {
	realBD := filepath.Join(t.TempDir(), "bd-1.3.0")
	recorder := NewRecordingBD(t, realBD)

	script, err := os.ReadFile(recorder.Path)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	body := string(script)
	if !strings.Contains(body, "exec '"+realBD+"' \"$@\"") {
		t.Fatalf("the shim does not exec the pinned bd:\n%s", body)
	}
	if !strings.Contains(body, `"$@"`) {
		t.Fatalf("the shim does not forward its arguments:\n%s", body)
	}
	if filepath.Base(recorder.Path) != "bd" {
		t.Errorf("shim basename = %q, want bd — gc carries BD_BIN into provider environments verbatim", filepath.Base(recorder.Path))
	}
	info, err := os.Stat(recorder.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("shim mode = %v, want it executable", info.Mode().Perm())
	}
}

// TestRecordingBDShimWritesOneRecordPerInvocation is the fence under the
// atomicity the shim's log depends on.
//
// O_APPEND makes a single write(2) atomic and says nothing about a block of
// them. The shim used to emit one printf per field — strace showed five writes
// for a three-argument fork — so two bd forks in flight interleaved their fields
// and Invocations() read one spliced record plus one empty one, under-reporting
// Count() by one. The next slice builds a deterministic fork gate on Count(),
// and this is what that gate stands on.
//
// It is asserted on the generated source rather than by racing real forks: the
// number of write syscalls is a property of the script's shape, and a test that
// spawned its own processes to observe it would grow the repo's shrink-only
// subprocess census for evidence strace already gives once. What must hold is
// that the record is assembled first and written once.
func TestRecordingBDShimWritesOneRecordPerInvocation(t *testing.T) {
	recorder := NewRecordingBD(t, filepath.Join(t.TempDir(), "bd"))
	script, err := os.ReadFile(recorder.Path)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	body := string(script)

	if got := strings.Count(body, "printf"); got != 1 {
		t.Fatalf("the shim issues %d printf(s); one invocation must be one write, or concurrent bd forks interleave their fields:\n%s", got, body)
	}
	// The one printf must be the one that appends the whole assembled record.
	if !strings.Contains(body, `printf '%s\n' "$record$rs" >>`) {
		t.Fatalf("the shim does not append one assembled record:\n%s", body)
	}
	if !strings.Contains(body, `record="$record$arg$us"`) {
		t.Fatalf("the shim does not accumulate its argv into the record before writing:\n%s", body)
	}
	// Building the record must cost no subshell: a fork per invocation inside
	// the instrument is a process the fork census cannot see.
	if strings.Contains(body, "$(") || strings.Contains(body, "`") {
		t.Fatalf("the shim forks a subshell to build its record:\n%s", body)
	}
	// The separators are literal bytes, so the record written is the record
	// Invocations parses.
	if !strings.Contains(body, "us='"+fieldSeparator+"'") || !strings.Contains(body, "rs='"+recordSeparator+"'") {
		t.Fatalf("the shim does not carry the literal field and record separators:\n%q", body)
	}
}

// TestRecordingBDShimQuotesPathsForTheShell pins that the generated shim is
// quoted for sh and not for Go.
//
// Go's %q produces DOUBLE quotes and leaves `$` and backticks alone inside them,
// so a pinned bd or a TMPDIR containing either would be expanded or
// command-substituted by the shell — the shim would exec a path nobody pinned,
// or nothing at all. It also renders non-ASCII bytes as \uNNNN, which sh does
// not decode. The assertions are on the bytes in the file rather than on a
// re-application of the quoting helper: a test that quoted the expectation the
// same way the shim quotes the path would pass while the shim ran the wrong
// binary, which is how this went unnoticed.
func TestRecordingBDShimQuotesPathsForTheShell(t *testing.T) {
	hostile := filepath.Join(t.TempDir(), "b$HOME and `id` and é", "bd")
	recorder := NewRecordingBD(t, hostile)
	script, err := os.ReadFile(recorder.Path)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	body := string(script)

	if !strings.Contains(body, "'"+hostile+"'") {
		t.Fatalf("the pinned bd is not carried as a single-quoted word:\n%s", body)
	}
	if strings.Contains(body, `"`+hostile+`"`) {
		t.Fatalf("the pinned bd is double-quoted, so sh expands $HOME and command-substitutes in it:\n%s", body)
	}
	if !strings.Contains(body, "and é") {
		t.Fatalf("a non-ASCII path byte was escaped into something sh does not decode:\n%q", body)
	}
	for _, escape := range []string{`\x`, `\u`} {
		if strings.Contains(body, escape) {
			t.Fatalf("the shim carries a Go escape %q, which sh does not decode:\n%q", escape, body)
		}
	}

	// And the one byte single quotes cannot carry is closed, escaped, reopened.
	quoted := NewRecordingBD(t, filepath.Join(t.TempDir(), "it's", "bd"))
	body = readShim(t, quoted)
	if !strings.Contains(body, `'\''`) {
		t.Fatalf("a single quote in the pinned path is not escaped for sh:\n%s", body)
	}
	if strings.Contains(body, `"it's"`) {
		t.Fatalf("a single quote in the pinned path was left to Go's %%q:\n%s", body)
	}
}

// readShim returns the generated shim's body.
func readShim(t *testing.T, recorder *RecordingBD) string {
	t.Helper()
	data, err := os.ReadFile(recorder.Path)
	if err != nil {
		t.Fatalf("read shim: %v", err)
	}
	return string(data)
}

// TestRecordingBDCountsInvocations drives the reader over records in the shim's
// own format, including the argv shapes a naive line-and-space parser loses.
func TestRecordingBDCountsInvocations(t *testing.T) {
	recorder := NewRecordingBD(t, filepath.Join(t.TempDir(), "bd"))

	if got := recorder.Count(); got != 0 {
		t.Fatalf("a shim that has not run recorded %d invocation(s)", got)
	}
	if got := recorder.Describe(); !strings.Contains(got, "no bd invocations") {
		t.Errorf("Describe() on an empty log = %q", got)
	}

	writeInvocations(t, recorder,
		[]string{"4242", "ping", "--json"},
		[]string{"4242", "list", "--json"},
		// An argument carrying spaces, a quote and a newline — a bead title or
		// a JSON payload on a `bd create` command line. The unit separator is
		// what keeps this one record.
		[]string{"4243", "create", "a title with spaces, a \" and a\nnewline", "--json"},
		[]string{"4244", "ping", "--json"},
		[]string{"4244", "dolt", "stop"},
	)

	if got := recorder.Count(); got != 5 {
		t.Fatalf("Count() = %d, want 5:\n%s", got, recorder.Describe())
	}
	if got := recorder.Count("ping"); got != 2 {
		t.Fatalf("Count(ping) = %d, want 2:\n%s", got, recorder.Describe())
	}
	if got := recorder.Count("dolt", "stop"); got != 1 {
		t.Fatalf("Count(dolt stop) = %d, want 1", got)
	}
	if got := recorder.Count("dolt", "start"); got != 0 {
		t.Fatalf("Count(dolt start) = %d, want 0 — a prefix must match in order", got)
	}
	if got := recorder.Count("ping", "--json", "--extra"); got != 0 {
		t.Fatalf("Count of a prefix longer than the argv = %d, want 0", got)
	}

	invocations := recorder.Invocations()
	if len(invocations) != 5 {
		t.Fatalf("Invocations() = %d, want 5", len(invocations))
	}
	if invocations[0].Subcommand() != "ping" || invocations[0].PPID != "4242" {
		t.Errorf("first invocation = %+v, want ping from ppid 4242", invocations[0])
	}
	if got := invocations[2].Argv[1]; !strings.Contains(got, "\n") {
		t.Errorf("an argument containing a newline was split across records: %q", got)
	}

	recorder.Reset()
	if got := recorder.Count(); got != 0 {
		t.Fatalf("Count() after Reset = %d, want 0", got)
	}
	// Reset on an already-empty log is how a test attributes a count to its
	// first step as easily as its fifth.
	recorder.Reset()
}

// TestRecordingBDCountWhereClassifiesByShapeNotPrefix pins the predicate form.
//
// Count's argv prefix answers "how many `bd dolt stop`", which is most of what
// a fork gate asks and none of what the proxied lane's gates ask. Those are
// about a fork's shape: which process forked it (gc itself, or the provider
// script running under it), whether it carried --json anywhere in argv, whether
// anything other than the one allowed passthrough ran at all. Without this, each
// gate re-implements the Invocations() loop — and a call site that loops itself
// is one that can forget the interleave refusal Invocations performs.
func TestRecordingBDCountWhereClassifiesByShapeNotPrefix(t *testing.T) {
	recorder := NewRecordingBD(t, filepath.Join(t.TempDir(), "bd"))

	if got := recorder.CountWhere(func(Invocation) bool { return true }); got != 0 {
		t.Fatalf("a shim that has not run matched %d invocation(s)", got)
	}

	writeInvocations(t, recorder,
		[]string{"4242", "list", "--json"},
		[]string{"4242", "ping"},
		// The provider script's own fork: a different ppid under the same run.
		[]string{"9001", "dolt", "status", "--json"},
		[]string{"9001", "ping"},
	)

	fromController := recorder.CountWhere(func(i Invocation) bool { return i.PPID == "4242" })
	if fromController != 2 {
		t.Errorf("forks from ppid 4242 = %d, want 2:\n%s", fromController, recorder.Describe())
	}

	jsonForks := recorder.CountWhere(func(i Invocation) bool {
		for _, arg := range i.Argv {
			if arg == "--json" {
				return true
			}
		}
		return false
	})
	if jsonForks != 2 {
		t.Errorf("--json forks = %d, want 2 — the flag is not always argv[1]", jsonForks)
	}

	// "Everything that is not the one allowed fork" is the shape a
	// zero-forks-except-this gate needs, and no prefix can express it.
	notList := recorder.CountWhere(func(i Invocation) bool { return i.Subcommand() != "list" })
	if notList != 3 {
		t.Errorf("non-list forks = %d, want 3", notList)
	}

	if got := recorder.CountWhere(func(Invocation) bool { return true }); got != recorder.Count() {
		t.Errorf("an always-true predicate counted %d, but Count() says %d", got, recorder.Count())
	}
}

// TestRecordingBDRefusesAnInterleavedLog pins the one thing the single-printf
// shim cannot promise: a record larger than the shell's stdout buffer, written
// by two forks at once, arrives spliced.
//
// /bin/dash emits any record in one write, but bash standing in as /bin/sh
// chunks at 4096 bytes, and gc does build argv over 4 KiB (bead bodies on
// --description). A count read off a spliced log is a number nobody can
// reproduce, so the reader refuses it instead of quietly under-reporting by one.
func TestRecordingBDRefusesAnInterleavedLog(t *testing.T) {
	good := "4242" + fieldSeparator + "ping" + fieldSeparator + recordSeparator + "\n"
	invocations, err := parseInvocations([]byte(good))
	if err != nil {
		t.Fatalf("parseInvocations over an intact log: %v", err)
	}
	if len(invocations) != 1 || invocations[0].PPID != "4242" {
		t.Fatalf("parseInvocations over an intact log = %+v, want one record from ppid 4242", invocations)
	}

	// What a chunked write looks like: the tail of one record's argv is the head
	// of what the reader takes for the next record.
	spliced := good + "a title with spaces" + fieldSeparator + "--json" + fieldSeparator + recordSeparator + "\n"
	if _, err := parseInvocations([]byte(spliced)); err == nil {
		t.Fatal("parseInvocations accepted a record whose first field is not a pid; an interleaved log must fail the test, not lower its fork count")
	}
}

// TestParseInvocationsToleratesDashsSeparateNewlineWrite pins the one shape
// /bin/dash really produces: a record over 8 KiB is one write and its trailing
// newline is a SECOND write (measured with strace: one write up to 8000 bytes,
// two from 8200 on). A concurrent fork's record can therefore land between the
// two, and the log carries the first record, then the second record and ITS
// newline, then the first record's newline — so the reader meets "\n\n" ahead of
// the next record, or a trailing "\n" segment of nothing but newlines.
//
// Both records are intact and the count is exact, so this must parse, not fail:
// dash is the shell this host runs and the one the shim's comment calls safe.
// Stripping only the first newline left a "\n" where the pid check wants a
// number and failed a correct log with "cannot be trusted".
func TestParseInvocationsToleratesDashsSeparateNewlineWrite(t *testing.T) {
	first := "4242" + fieldSeparator + "create" + fieldSeparator + "--description" + fieldSeparator + strings.Repeat("x", 9000) + fieldSeparator + recordSeparator
	second := "4243" + fieldSeparator + "ping" + fieldSeparator + recordSeparator + "\n"

	for _, tc := range []struct {
		name string
		log  string
	}{
		// The big record, then the small fork's record and newline, then the big
		// record's own newline: the doubled newline lands at the end of the log.
		{name: "doubled newline at the tail", log: first + second + "\n"},
		// The same interleave with a third fork behind it: the doubled newline
		// now sits in front of a record whose pid must still be read.
		{name: "doubled newline mid-log", log: first + second + "\n" + "4244" + fieldSeparator + "ready" + fieldSeparator + recordSeparator + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invocations, err := parseInvocations([]byte(tc.log))
			if err != nil {
				t.Fatalf("parseInvocations over a dash split-newline log: %v", err)
			}
			var ppids []string
			for _, invocation := range invocations {
				ppids = append(ppids, invocation.PPID)
			}
			want := []string{"4242", "4243"}
			if strings.Contains(tc.name, "mid-log") {
				want = append(want, "4244")
			}
			if len(ppids) != len(want) {
				t.Fatalf("parseInvocations = %v ppids, want %v; every record in this log is intact, so the count is exact", ppids, want)
			}
			for i := range want {
				if ppids[i] != want[i] {
					t.Fatalf("parseInvocations ppids = %v, want %v", ppids, want)
				}
			}
			if got := invocations[0].Argv; len(got) != 3 || got[0] != "create" || len(got[2]) != 9000 {
				t.Fatalf("the oversized record's argv came back as %d fields (first %q, last %d bytes), want create/--description/9000 bytes intact", len(got), got[0], len(got[len(got)-1]))
			}
		})
	}
}

// writeInvocations appends records in the shim's wire format: ppid first, then
// argv, every field unit-separated, every record separator-terminated and
// followed by the readability newline the shim writes.
func writeInvocations(t *testing.T, recorder *RecordingBD, records ...[]string) {
	t.Helper()
	var buf strings.Builder
	for _, fields := range records {
		for _, field := range fields {
			buf.WriteString(field)
			buf.WriteString(fieldSeparator)
		}
		buf.WriteString(recordSeparator)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(recorder.logPath, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
