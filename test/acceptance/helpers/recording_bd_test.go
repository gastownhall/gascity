package acceptancehelpers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRecordingBDCountsConcurrentForksWithArgvOverTheShellBuffer races real
// shim processes whose argv is far larger than a shell's output buffer and
// requires every one of them back, intact.
//
// This is the flake a shared log could not survive on macOS: /bin/sh there is
// bash, bash writes a large printf in 4 KiB pieces, and two bd forks with a
// multi-KiB argv in flight (the reaper's `bd sql` racing anything else)
// spliced their records. The shim runs under every interpreter that can stand
// in as /bin/sh here, not only this host's: dash never splits a record, so a
// test that only ran /bin/sh on Linux would pass against the very scheme that
// fails on a Mac.
func TestRecordingBDCountsConcurrentForksWithArgvOverTheShellBuffer(t *testing.T) {
	realBD := filepath.Join(t.TempDir(), "bd-real")
	if err := os.WriteFile(realBD, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test double must be executable
		t.Fatal(err)
	}
	interpreters := []string{"/bin/sh"}
	if bash, err := exec.LookPath("bash"); err == nil {
		interpreters = append(interpreters, bash)
	}
	const (
		forks       = 48
		idWidth     = 4
		payloadSize = 6 * 4096
	)
	for _, interpreter := range interpreters {
		t.Run(filepath.Base(interpreter), func(t *testing.T) {
			recorder := NewRecordingBD(t, realBD)
			var wg sync.WaitGroup
			errs := make(chan error, forks)
			for i := 0; i < forks; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					payload := fmt.Sprintf("%0*d", idWidth, i) + strings.Repeat(string(rune('a'+i%26)), payloadSize)
					out, err := shimCommand(interpreter, recorder.Path, "sql", "--description", payload).CombinedOutput()
					if err != nil {
						errs <- fmt.Errorf("fork %d: %w\n%s", i, err, out)
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			invocations := recorder.Invocations()
			if len(invocations) != forks {
				t.Fatalf("recorded %d invocation(s) of %d concurrent forks", len(invocations), forks)
			}
			seen := make(map[string]bool, forks)
			for _, invocation := range invocations {
				argv := invocation.Argv
				if len(argv) != 3 || argv[0] != "sql" || argv[1] != "--description" || len(argv[2]) != idWidth+payloadSize {
					t.Fatalf("an invocation came back altered: %d field(s), verb %q, last field %d bytes", len(argv), invocation.Subcommand(), len(argv[len(argv)-1]))
				}
				id, body := argv[2][:idWidth], argv[2][idWidth:]
				if strings.Trim(body, body[:1]) != "" {
					t.Fatalf("fork %s's payload carries another fork's bytes", id)
				}
				if seen[id] {
					t.Fatalf("fork %s recorded twice", id)
				}
				seen[id] = true
			}
			if got := recorder.Count("sql"); got != forks {
				t.Fatalf("Count(sql) = %d, want %d", got, forks)
			}
		})
	}
}

// shimCommand is the one place these tests run an instrument's shim as a real
// process, so the instruments' proofs share a single subprocess call site.
func shimCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:gosec // a test-owned shim
}

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
// isolation the shim's records depend on: every invocation writes its own
// assembled record into a file named for its own pid, and no other.
//
// A shared append-only log interleaved: first one printf per field spliced
// fields, then one printf per record still spliced chunks wherever bash is
// /bin/sh and argv exceeds 4 KiB. TestRecordingBDCountsConcurrentForksWithArgvOverTheShellBuffer
// races real forks; this pins the script shape that makes that test pass on
// every shell rather than on the one this host happens to run.
func TestRecordingBDShimWritesOneRecordPerInvocation(t *testing.T) {
	recorder := NewRecordingBD(t, filepath.Join(t.TempDir(), "bd"))
	body := readShim(t, recorder)

	if got := strings.Count(body, "printf"); got != 1 {
		t.Fatalf("the shim issues %d printf(s); one invocation must write one record:\n%s", got, body)
	}
	// The one printf must write the whole assembled record into a file of its
	// own: truncating (>), not appending (>>), to a name carrying the shell's pid.
	if !strings.Contains(body, `printf '%s\n' "$record$rs" >"$dir/$$.$n"`) {
		t.Fatalf("the shim does not write one assembled record to a file of its own:\n%s", body)
	}
	if strings.Contains(body, ">>") {
		t.Fatalf("the shim appends to a shared file; concurrent forks interleave there:\n%s", body)
	}
	if !strings.Contains(body, "dir="+shellQuote(recorder.recordDir)+"\n") {
		t.Fatalf("the shim does not write into the recorder's record directory %s:\n%s", recorder.recordDir, body)
	}
	if !strings.Contains(body, `record="$record$arg$us"`) {
		t.Fatalf("the shim does not accumulate its argv into the record before writing:\n%s", body)
	}
	// Building the record and naming its file must cost no fork: a process
	// inside the instrument is one the fork census cannot see.
	if strings.Contains(strings.ReplaceAll(body, "$((", ""), "$(") || strings.Contains(body, "`") {
		t.Fatalf("the shim forks a subshell to build its record:\n%s", body)
	}
	for _, external := range []string{"mktemp", "date", "mv ", "mapfile"} {
		if strings.Contains(body, external) {
			t.Fatalf("the shim runs %q, which forks or is not portable to macOS bash 3.2:\n%s", external, body)
		}
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
	// Reset on an already-empty record directory is how a test attributes a
	// count to its first step as easily as its fifth.
	recorder.Reset()
}

// TestParseInvocationRefusesACorruptRecord pins that a record file which does
// not hold exactly one record led by a pid fails the census rather than
// lowering or raising its count. Each fork writes only its own file, so this is
// corruption, never contention; a count read past it is a number nobody can
// reproduce.
func TestParseInvocationRefusesACorruptRecord(t *testing.T) {
	good := "4242" + fieldSeparator + "ping" + fieldSeparator + recordSeparator + "\n"
	invocation, complete, err := parseInvocation([]byte(good))
	if err != nil || !complete {
		t.Fatalf("parseInvocation over an intact record = complete %v, err %v", complete, err)
	}
	if invocation.PPID != "4242" || strings.Join(invocation.Argv, "|") != "ping" {
		t.Fatalf("parseInvocation over an intact record = %+v, want ping from ppid 4242", invocation)
	}

	for name, data := range map[string]string{
		"first field is not a pid": "a title with spaces" + fieldSeparator + "--json" + fieldSeparator + recordSeparator + "\n",
		"empty record":             recordSeparator + "\n",
		"two records in one file":  good + good,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseInvocation([]byte(data)); err == nil {
				t.Fatalf("parseInvocation accepted %q; a corrupt record must fail the test, not move its fork count", data)
			}
		})
	}
}

// TestParseInvocationLeavesARecordStillBeingWritten pins how the reader treats
// a record file it meets mid-write.
//
// bash writes a large record in 4096-byte chunks, and dash writes a record over
// 8 KiB and its trailing newline as two writes, so a reader can see a file
// whose record separator has not landed yet, or one whose newline has not. The
// first is a fork that has not exec'd bd yet and is not counted; the second is
// a whole record and is.
func TestParseInvocationLeavesARecordStillBeingWritten(t *testing.T) {
	record := "4242" + fieldSeparator + "create" + fieldSeparator + "--description" + fieldSeparator + strings.Repeat("x", 9000) + fieldSeparator + recordSeparator

	if _, complete, err := parseInvocation([]byte(record[:4096])); err != nil || complete {
		t.Fatalf("parseInvocation over the first chunk of a record = complete %v, err %v; want an in-flight record, not counted and not an error", complete, err)
	}
	if _, complete, err := parseInvocation(nil); err != nil || complete {
		t.Fatalf("parseInvocation over a just-created empty file = complete %v, err %v; want an in-flight record", complete, err)
	}
	invocation, complete, err := parseInvocation([]byte(record))
	if err != nil || !complete {
		t.Fatalf("parseInvocation over a record whose newline has not landed = complete %v, err %v; want it counted", complete, err)
	}
	if got := invocation.Argv; len(got) != 3 || got[0] != "create" || len(got[2]) != 9000 {
		t.Fatalf("the oversized record's argv came back as %d fields, want create/--description/9000 bytes intact", len(got))
	}
}

// TestRecordingBDSkipsAnInFlightRecordFile drives the same property through
// the recorder: a record file without its separator yet is neither counted nor
// a failure.
func TestRecordingBDSkipsAnInFlightRecordFile(t *testing.T) {
	recorder := NewRecordingBD(t, filepath.Join(t.TempDir(), "bd"))
	writeInvocations(t, recorder, []string{"4242", "ping", "--json"})
	partial := "4243" + fieldSeparator + "create" + fieldSeparator + strings.Repeat("y", 100)
	if err := os.WriteFile(filepath.Join(recorder.recordDir, "4243.0"), []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Count(); got != 1 {
		t.Fatalf("Count() = %d, want 1 — the in-flight record is not a fork yet:\n%s", got, recorder.Describe())
	}
}

// writeInvocations writes records in the shim's wire format, one file per
// record as the shim does: ppid first, then argv, every field unit-separated,
// the record separator last and the readability newline the shim writes after
// it. Files are named so that their order is the argument order even where two
// writes share a modification time.
func writeInvocations(t *testing.T, recorder *RecordingBD, records ...[]string) {
	t.Helper()
	for i, fields := range records {
		var buf strings.Builder
		for _, field := range fields {
			buf.WriteString(field)
			buf.WriteString(fieldSeparator)
		}
		buf.WriteString(recordSeparator)
		buf.WriteString("\n")
		name := filepath.Join(recorder.recordDir, fmt.Sprintf("%s.%04d", fields[0], i))
		if err := os.WriteFile(name, []byte(buf.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
