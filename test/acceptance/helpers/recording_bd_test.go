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
	if !strings.Contains(body, "exec "+quoteForShell(realBD)) {
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

// quoteForShell renders a path the way %q does in the generated shim.
func quoteForShell(path string) string {
	return `"` + path + `"`
}
