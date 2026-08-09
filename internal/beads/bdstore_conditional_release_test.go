package beads_test

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// exitErrorWithCode returns a real *exec.ExitError carrying code, which is what
// the bd runner surfaces and what the CAS classifier reads. Fabricating the
// process state directly is not possible, so a shell that exits with the code
// stands in for bd.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh -c 'exit %d' did not produce an *exec.ExitError: %v", code, err)
	}
	if exitErr.ExitCode() != code {
		t.Fatalf("fabricated exit code = %d, want %d", exitErr.ExitCode(), code)
	}
	// The production runner wraps the ExitError with %w and appends bd's stderr
	// detail (classifyBDExecResult); wrap here so the classifier is exercised
	// through the same shape it sees in production.
	return fmt.Errorf("%w: bd said something", exitErr)
}

// releaseVerbRunner records every bd invocation and answers the conditional
// release verb with reply.
type releaseVerbRunner struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) ([]byte, error)
}

func (r *releaseVerbRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply(args)
	}
	return nil, nil
}

func (r *releaseVerbRunner) argv() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func isReleaseVerb(args []string) bool {
	if len(args) == 0 || args[0] != "update" {
		return false
	}
	for _, a := range args {
		if a == "--if-assignee" {
			return true
		}
	}
	return false
}

// TestReleaseIfCurrentPrefersTheNativeVerb pins the exact argv, because the
// whole conversion is "stop assembling SQL, state the two preconditions".
func TestReleaseIfCurrentPrefersTheNativeVerb(t *testing.T) {
	runner := &releaseVerbRunner{}
	s := beads.NewBdStore("/city", runner.run)

	released, err := s.ReleaseIfCurrent("bd-42", "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	want := []string{"bd", "update", "bd-42", "--if-assignee", "worker-1", "--if-status", "in_progress", "--status", "open", "--assignee", ""}
	calls := runner.argv()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", calls)
	}
	if strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q\nwant  %q", calls[0], want)
	}
	// No SQL is assembled at all on this path.
	for _, arg := range calls[0] {
		if strings.Contains(strings.ToUpper(arg), "UPDATE ISSUES SET") {
			t.Fatalf("the native verb path still built SQL: %q", arg)
		}
	}
}

// TestReleaseIfCurrentReadsPreconditionMissFromTheExitCode is the heart of the
// change: the CAS verdict is a number, not a parse of bd's prose.
func TestReleaseIfCurrentReadsPreconditionMissFromTheExitCode(t *testing.T) {
	runner := &releaseVerbRunner{}
	runner.reply = func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return []byte("Error updating bd-42: assignee mismatch"), exitErrorWithCode(t, 13)
		}
		return nil, fmt.Errorf("unexpected command %v", args)
	}
	s := beads.NewBdStore("/city", runner.run)

	released, err := s.ReleaseIfCurrent("bd-42", "worker-1")
	if err != nil {
		t.Fatalf("a precondition miss must not be an error, got %v", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released = true on a precondition miss")
	}
	if len(runner.argv()) != 1 {
		t.Fatalf("a precondition miss must not fall back or retry; calls = %v", runner.argv())
	}
}

// TestReleaseIfCurrentPreconditionMissDoesNotClassifyOnMessageText proves the
// verdict is the exit code alone: bd prose that says nothing about a mismatch
// still reads as a precondition miss at exit 13, and prose that DOES say
// "assignee mismatch" at any other exit code does not.
func TestReleaseIfCurrentPreconditionMissDoesNotClassifyOnMessageText(t *testing.T) {
	t.Run("exit 13 with unrelated prose is still a miss", func(t *testing.T) {
		runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
			if isReleaseVerb(args) {
				return []byte("some future wording"), exitErrorWithCode(t, 13)
			}
			return nil, fmt.Errorf("unexpected %v", args)
		}}
		released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
		if err != nil || released {
			t.Fatalf("released=%v err=%v, want false/nil", released, err)
		}
	})
	t.Run("mismatch prose at exit 1 is a real error", func(t *testing.T) {
		runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
			if isReleaseVerb(args) {
				return []byte("assignee mismatch: bd-42 is held by \"other\""), exitErrorWithCode(t, 1)
			}
			return nil, fmt.Errorf("unexpected %v", args)
		}}
		released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
		if err == nil {
			t.Fatal("a non-13 failure must surface as an error, not a silent false")
		}
		if released {
			t.Fatal("released = true on an error")
		}
	})
}

// TestReleaseIfCurrentTreatsAnUnresolvableIDAsNotHeld keeps the observable
// contract the raw-SQL path had, where an absent bead matched zero rows.
func TestReleaseIfCurrentTreatsAnUnresolvableIDAsNotHeld(t *testing.T) {
	runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return []byte(`{"error":"issue not found: bd-42"}`), fmt.Errorf("exit status 1: issue not found: bd-42")
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}}
	released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
	if err != nil {
		t.Fatalf("an unresolvable id must not error: %v", err)
	}
	if released {
		t.Fatal("released = true for an unresolvable id")
	}
}

// TestReleaseIfCurrentFallsBackToSQLOnAnOldBd is the bd 1.0.4 compatibility
// proof: the minimum supported bd rejects the flags, and the store must reach
// the raw-SQL statement byte-identically to how it did before the conversion.
func TestReleaseIfCurrentFallsBackToSQLOnAnOldBd(t *testing.T) {
	runner := &releaseVerbRunner{}
	runner.reply = func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return nil, errors.New("unknown flag: --if-assignee")
		}
		return []byte(`{"rows_affected":1,"schema_version":1}`), nil
	}
	s := beads.NewBdStore("/city", runner.run)

	released, err := s.ReleaseIfCurrent("bd-42", "worker-'1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	calls := runner.argv()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want the verb probe then the SQL fallback", calls)
	}
	wantQuery := "UPDATE issues SET status = 'open', assignee = '', updated_at = CURRENT_TIMESTAMP" +
		" WHERE id = 'bd-42' AND status = 'in_progress' AND assignee = 'worker-''1'"
	want := []string{"bd", "sql", "--json", wantQuery}
	if strings.Join(calls[1], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("fallback argv = %q\nwant          %q", calls[1], want)
	}
}

// TestReleaseIfCurrentLatchesTheOldBdFallback proves the probe is paid once:
// a store that has seen the flags rejected must not spend a failed subprocess
// on every later release.
func TestReleaseIfCurrentLatchesTheOldBdFallback(t *testing.T) {
	runner := &releaseVerbRunner{}
	runner.reply = func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return nil, errors.New("unknown flag: --if-assignee")
		}
		return []byte(`{"rows_affected":1,"schema_version":1}`), nil
	}
	s := beads.NewBdStore("/city", runner.run)

	for i := range 3 {
		if _, err := s.ReleaseIfCurrent("bd-42", "worker-1"); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	verbProbes := 0
	for _, call := range runner.argv() {
		if isReleaseVerb(call[1:]) {
			verbProbes++
		}
	}
	if verbProbes != 1 {
		t.Fatalf("verb probes = %d across 3 releases, want exactly 1 (latched)", verbProbes)
	}
}

// TestReleaseIfCurrentDoesNotLatchOnARealFailure guards the inverse: a backend
// error must never be mistaken for an old bd and permanently downgrade a
// capable store to the SQL path.
func TestReleaseIfCurrentDoesNotLatchOnARealFailure(t *testing.T) {
	runner := &releaseVerbRunner{}
	fail := true
	runner.reply = func(args []string) ([]byte, error) {
		if !isReleaseVerb(args) {
			return nil, fmt.Errorf("fell back to %v after a transient failure", args)
		}
		if fail {
			return []byte("dial tcp: connection refused"), exitErrorWithCode(t, 1)
		}
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner.run)

	if _, err := s.ReleaseIfCurrent("bd-42", "worker-1"); err == nil {
		t.Fatal("a backend failure must surface as an error")
	}
	fail = false
	released, err := s.ReleaseIfCurrent("bd-42", "worker-1")
	if err != nil {
		t.Fatalf("the store latched the fallback after a transient failure: %v", err)
	}
	if !released {
		t.Fatal("released = false after recovery")
	}
}

// TestReleaseIfCurrentUnknownFlagMatchIsAnchored guards the latch against a
// capable bd's cobra usage echo, which lists every flag it HAS whenever any
// flag is wrong. A floating substring check would latch a capable bd into the
// SQL path forever the first time gascity passed an unrelated bad flag.
func TestReleaseIfCurrentUnknownFlagMatchIsAnchored(t *testing.T) {
	runner := &releaseVerbRunner{}
	runner.reply = func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			// A capable bd rejecting something else, echoing its own flags.
			return []byte("unknown flag: --nope\nFlags:\n  --if-assignee string\n  --if-status string\n"), exitErrorWithCode(t, 1)
		}
		return nil, fmt.Errorf("latched the fallback on a capable bd's usage echo: %v", args)
	}
	s := beads.NewBdStore("/city", runner.run)

	if _, err := s.ReleaseIfCurrent("bd-42", "worker-1"); err == nil {
		t.Fatal("expected the underlying failure to surface")
	}
	for _, call := range runner.argv() {
		if len(call) > 1 && call[1] == "sql" {
			t.Fatalf("a capable bd was latched into the SQL fallback: %v", runner.argv())
		}
	}
}
