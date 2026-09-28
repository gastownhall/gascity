package acceptancehelpers

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
//
// And one property of its trap mode (round4 missed low: the library-level
// no-spawn control), in the same table so the instrument's one proof covers
// both of its modes: armed for this process, the sentinel is the dolt this
// process resolves BY NAME — the way the linked library's exec.LookPath does —
// and it records the exec with this process as parent and then refuses,
// without running the real dolt.
//
// And the parent capture is proved on both of its paths: procfs, which is what
// Linux uses, and POSIX ps, which is the only one macOS has. The ps row hides
// procfs from the shim (SentinelDoltProcEnv) so Linux CI exercises the branch
// the macOS jobs depend on, rather than leaving it to be discovered there.
func TestSentinelDoltRecordsArgvAndParentThenExecs(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "dolt-real")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + shellQuote(marker) + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // test double must be executable
		t.Fatal(err)
	}

	// A pass-through ps first on PATH that leaves a mark, so each row can prove
	// which branch the shim took rather than passing on whichever one worked.
	realPS, psErr := exec.LookPath("ps")
	psDir := filepath.Join(dir, "ps-shim")
	psMarker := filepath.Join(dir, "ps-ran")
	if psErr == nil {
		if err := os.MkdirAll(psDir, 0o755); err != nil {
			t.Fatal(err)
		}
		psScript := "#!/bin/sh\n: >>" + shellQuote(psMarker) + "\nexec " + shellQuote(realPS) + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(psDir, "ps"), []byte(psScript), 0o755); err != nil { //nolint:gosec // test double must be executable
			t.Fatal(err)
		}
	}

	sentinel := NewSentinelDolt(t, fake)
	if got := sentinel.Invocations(); len(got) != 0 {
		t.Fatalf("a fresh sentinel already recorded %d invocation(s)", len(got))
	}

	for _, tc := range []struct {
		name   string
		trap   bool
		noProc bool
	}{
		{name: "pass-through"},
		{name: "trapped in this process", trap: true},
		{name: "pass-through without procfs (the macOS path)", noProc: true},
		{name: "trapped without procfs (the macOS path)", trap: true, noProc: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if psErr == nil {
				t.Setenv("PATH", psDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			if tc.noProc {
				if psErr != nil {
					t.Skipf("no ps on PATH, so the non-procfs branch cannot run here: %v", psErr)
				}
				t.Setenv(SentinelDoltProcEnv, filepath.Join(t.TempDir(), "no-proc"))
			}
			if err := os.Remove(psMarker); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			sentinel.Reset()
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			name := sentinel.Path
			if tc.trap {
				sentinel.TrapThisProcess(t)
				name = "dolt"
			}
			cmd := exec.Command(name, "sql-server", "--config", "a b.yaml") //nolint:gosec // resolved shim
			if cmd.Path != sentinel.Path {
				t.Fatalf("%q resolved to %q, want the sentinel %q", name, cmd.Path, sentinel.Path)
			}
			out, err := cmd.CombinedOutput()
			ran, readErr := os.ReadFile(marker)
			if tc.trap {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != sentinelDoltTrapStatus {
					t.Fatalf("a trapped dolt exited %v, want status %d:\n%s", err, sentinelDoltTrapStatus, out)
				}
				if readErr == nil {
					t.Fatal("the trapped sentinel ran the real dolt; an in-process control would start a server")
				}
			} else {
				if err != nil {
					t.Fatalf("run the sentinel: %v\n%s", err, out)
				}
				if readErr != nil {
					t.Fatalf("the sentinel did not exec the real dolt: %v", readErr)
				}
				if got := strings.Fields(string(ran)); len(got) != 4 || got[0] != "sql-server" {
					t.Errorf("the real dolt received %q, want the shim's own argv", strings.TrimSpace(string(ran)))
				}
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
			if psErr == nil {
				_, statErr := os.Stat(psMarker)
				switch {
				case tc.noProc && statErr != nil:
					t.Errorf("procfs was hidden but the shim never ran ps, so the macOS branch was not exercised")
				case !tc.noProc && statErr == nil && runtime.GOOS == "linux":
					t.Errorf("the shim ran ps on Linux with procfs available; the procfs branch was not exercised")
				}
			}
			if mine := sentinel.FromThisProcess(); len(mine) != 1 {
				t.Errorf("FromThisProcess = %v, want the one exec this process made", mine)
			}
		})
	}
	if os.Getenv(SentinelDoltTrapEnv) != "" {
		t.Fatalf("%s outlived the subtest that armed it", SentinelDoltTrapEnv)
	}
	// The trap the no-spawn row fell into: a substring match on the whole parent
	// command line answers yes for any process whose ARGUMENTS happen to mention
	// the binary, which in an acceptance run is every process under a
	// /tmp/gc-acceptance-* city.
	decoy := SentinelDoltInvocation{ParentExe: "/usr/bin/bd", Parent: "/usr/bin/bd db-proxy-child --root /tmp/gc-acceptance-1/x/.beads/dolt", Argv: []string{"sql-server"}}
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
	good := "123" + fieldSeparator + "/bin/bd" + fieldSeparator + "/bin/bd" + argvSeparator + "db-proxy-child" + fieldSeparator + "sql-server" + fieldSeparator + recordSeparator + "\n"
	got, err := parseSentinelDolt([]byte(good))
	if err != nil || len(got) != 1 {
		t.Fatalf("parse a good record: %v (%d records)", err, len(got))
	}
	if got[0].Parent != "/bin/bd db-proxy-child" || !got[0].ParentCommandIs("bd") || !got[0].IsServer() {
		t.Fatalf("parsed %+v, want parent %q by bd running sql-server", got[0], "/bin/bd db-proxy-child")
	}
	bad := "sql-server" + fieldSeparator + "123" + fieldSeparator + "/bin/bd" + fieldSeparator + recordSeparator + "\n"
	if _, err := parseSentinelDolt([]byte(bad)); err == nil {
		t.Fatal("parseSentinelDolt accepted a record whose first field is not a pid; a dropped record would read as proof that gc spawned nothing")
	}
}

// TestSentinelDoltParentCommandSurvivesASpaceInThePath pins why the parent's
// program is its own field. ps's args (the macOS path) and a space-joined
// /proc cmdline both lose argv boundaries, so cutting the program from the
// command line misreads any path with a space — ordinary under a macOS home
// directory — and the no-spawn row would then attribute a server to "My".
func TestSentinelDoltParentCommandSurvivesASpaceInThePath(t *testing.T) {
	exe := "/Users/ci/My Tools/gc"
	record := "123" + fieldSeparator + exe + fieldSeparator + exe + " status --json" + fieldSeparator + "version" + fieldSeparator + recordSeparator + "\n"
	got, err := parseSentinelDolt([]byte(record))
	if err != nil || len(got) != 1 {
		t.Fatalf("parse: %v (%d records)", err, len(got))
	}
	if !got[0].ParentCommandIs("gc") {
		t.Fatalf("ParentCommand() = %q for program %q, want gc", got[0].ParentCommand(), exe)
	}
	if unknown := (SentinelDoltInvocation{Parent: "/usr/bin/gc status"}); unknown.ParentCommand() != "" {
		t.Fatalf("ParentCommand() = %q with no recorded program; an uncaptured parent must match nothing", unknown.ParentCommand())
	}
}

// TestSentinelDoltFromThisProcessKeepsOnlyThisProcesssExecs pins the filter the
// library-level rows count with, over a hand-written log: an exec whose parent
// is any other process — gc, bd, a shell this test started — is not the linked
// library's.
func TestSentinelDoltFromThisProcessKeepsOnlyThisProcesssExecs(t *testing.T) {
	dir := t.TempDir()
	s := &SentinelDolt{t: t, logPath: filepath.Join(dir, "invocations.log")}
	record := func(ppid, parent string, argv ...string) string {
		return ppid + fieldSeparator + strings.Fields(parent)[0] + fieldSeparator + parent + fieldSeparator + strings.Join(argv, fieldSeparator) + fieldSeparator + recordSeparator + "\n"
	}
	self := strconv.Itoa(os.Getpid())
	log := record(self, os.Args[0], "version") +
		record(strconv.Itoa(os.Getpid()+1), "/usr/bin/bd db-proxy-child", "sql-server") +
		record(self, os.Args[0], "sql-server", "--port", "0")
	if err := os.WriteFile(s.logPath, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := s.FromThisProcess()
	if len(mine) != 2 || mine[0].Subcommand() != "version" || !mine[1].IsServer() {
		t.Fatalf("FromThisProcess = %v, want this process's version and sql-server execs only", mine)
	}
}
