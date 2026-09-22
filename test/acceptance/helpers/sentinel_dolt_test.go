package acceptancehelpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSentinelDoltRecordsArgvAndParentThenExecs is the instrument's own proof.
//
// The no-spawn assertion in test/acceptance rests on three properties of this
// shim, and all three are asserted here against a stand-in "dolt" so the proof
// does not need a real Dolt, a real bd or a real city:
//
//   - the exec still happens, with the arguments unchanged. A shim that recorded
//     and did not exec would make every city under it fail in a way that looks
//     like a product defect.
//   - the recorded parent is the process that ran it, captured at exec time. That
//     is the whole ancestry signal, and a parent that has already exited by the
//     time a test reads the log cannot be looked up afterwards.
//   - sql-server is distinguished from every other verb. gc legitimately runs
//     `dolt version`; what it must never do is start a server.
func TestSentinelDoltRecordsArgvAndParentThenExecs(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "dolt-real")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + shellQuote(marker) + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // test double must be executable
		t.Fatal(err)
	}

	sentinel := NewSentinelDolt(t, fake)
	if got := sentinel.Invocations(); len(got) != 0 {
		t.Fatalf("a fresh sentinel already recorded %d invocation(s)", len(got))
	}

	cmd := exec.Command(sentinel.Path, "sql-server", "--config", "a b.yaml") //nolint:gosec // resolved shim
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run the sentinel: %v\n%s", err, out)
	}
	ran, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the sentinel did not exec the real dolt: %v", err)
	}
	if got := strings.Fields(string(ran)); len(got) != 4 || got[0] != "sql-server" {
		t.Errorf("the real dolt received %q, want the shim's own argv", strings.TrimSpace(string(ran)))
	}

	invocations := sentinel.Invocations()
	if len(invocations) != 1 {
		t.Fatalf("recorded %d invocation(s), want 1:\n%s", len(invocations), sentinel.Describe())
	}
	got := invocations[0]
	if !got.IsServer() {
		t.Errorf("IsServer() = false for %v", got.Argv)
	}
	if want := []string{"sql-server", "--config", "a b.yaml"}; strings.Join(got.Argv, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %v, want %v (an argument with a space must stay one argument)", got.Argv, want)
	}
	// The parent is this test binary, which is the only process that ran it.
	if got.Parent == "" {
		t.Errorf("no parent command line recorded; the ancestry signal is the whole point:\n%s", sentinel.Describe())
	} else if got.ParentCommand() != filepath.Base(os.Args[0]) {
		t.Errorf("ParentCommand() = %q (from parent %q), want this test binary %q",
			got.ParentCommand(), got.Parent, filepath.Base(os.Args[0]))
	}
	// The trap the no-spawn row fell into: a substring match on the whole parent
	// command line answers yes for any process whose ARGUMENTS happen to mention
	// the binary, which in an acceptance run is every process under a
	// /tmp/gc-acceptance-* city.
	decoy := SentinelDoltInvocation{Parent: "/usr/bin/bd db-proxy-child --root /tmp/gc-acceptance-1/x/.beads/dolt", Argv: []string{"sql-server"}}
	if decoy.ParentCommandIs("gc") {
		t.Error("ParentCommandIs(\"gc\") matched a bd process whose argv merely names a gc-acceptance path")
	}
	if !decoy.ParentCommandIs("bd") {
		t.Errorf("ParentCommandIs(\"bd\") = false for parent %q", decoy.Parent)
	}

	sentinel.Reset()
	if got := sentinel.Invocations(); len(got) != 0 {
		t.Errorf("Reset left %d invocation(s)", len(got))
	}
}

// TestParseSentinelDoltRefusesAnInterleavedRecord pins the refusal without
// having to provoke a real interleave.
func TestParseSentinelDoltRefusesAnInterleavedRecord(t *testing.T) {
	good := "123" + fieldSeparator + "/bin/bd db-proxy-child" + fieldSeparator + "sql-server" + fieldSeparator + recordSeparator + "\n"
	if got, err := parseSentinelDolt([]byte(good)); err != nil || len(got) != 1 {
		t.Fatalf("parse a good record: %v (%d records)", err, len(got))
	}
	bad := "sql-server" + fieldSeparator + "123" + fieldSeparator + recordSeparator + "\n"
	if _, err := parseSentinelDolt([]byte(bad)); err == nil {
		t.Fatal("parseSentinelDolt accepted a record whose first field is not a pid; a dropped record would read as proof that gc spawned nothing")
	}
}
