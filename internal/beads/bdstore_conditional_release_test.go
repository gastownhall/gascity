package beads_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdExit stands in for the *exec.ExitError the bd runner wraps. bdExitCode
// matches the ExitCode() method rather than the concrete type precisely so this
// classifier can be driven without spawning a process: the untagged
// test-resource ledger's invariant is that untagged subprocess call sites cannot
// grow (TESTING.md), and a shell that exits with a code adds one.
// bdstore_conditional_release.go asserts at compile time that the real
// *exec.ExitError satisfies the same interface, so the substitution cannot drift
// away from what production reads.
type bdExit struct{ code int }

func (e *bdExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e *bdExit) ExitCode() int { return e.code }

// exitErrorWithCode returns the shape the production runner surfaces for a
// failed bd process.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	return exitErrorWithDetail(t, code, "bd said something")
}

// exitErrorWithDetail is exitErrorWithCode with bd's own stderr wording, which
// classifyBDExecResult appends to the process error with %w.
func exitErrorWithDetail(t *testing.T, code int, detail string) error {
	t.Helper()
	if code == 0 {
		t.Fatal("exit 0 is not a process failure; the classifier never sees one")
	}
	return fmt.Errorf("%w: %s", &bdExit{code: code}, detail)
}

// releaseVerbRunner records every bd invocation, answers the exact-ID preflight
// (`bd show --json <id>`) with the bead the caller named, and answers the
// conditional release verb with reply.
type releaseVerbRunner struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) ([]byte, error)
	// show overrides the preflight answer; the default resolves exactly, which
	// is the non-colliding case every cell but the collision one wants.
	show func(id string) ([]byte, error)
}

func (r *releaseVerbRunner) run(_, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if id, ok := showedID(args); ok {
		if r.show != nil {
			return r.show(id)
		}
		return []byte(`[{"id":"` + id + `"}]`), nil
	}
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

// releaseVerbArgv returns the recorded calls with the exact-ID preflight reads
// dropped, so a cell can pin the mutation argv without restating the guard.
func (r *releaseVerbRunner) releaseVerbArgv() [][]string {
	var mutations [][]string
	for _, call := range r.argv() {
		if _, ok := showedID(call[1:]); ok {
			continue
		}
		mutations = append(mutations, call)
	}
	return mutations
}

func showedID(args []string) (string, bool) {
	if len(args) != 3 || args[0] != "show" {
		return "", false
	}
	return args[2], true
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
	calls := runner.releaseVerbArgv()
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
	if len(runner.releaseVerbArgv()) != 1 {
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
//
// The fixture is bd's measured wording and exit status for an id that resolves
// to nothing (`bd update tst-nosuchbead --status open` → exit 1, `Error
// resolving …: no issue found matching …`), not an invented one: the verdict is
// gated on the exit code as well as the prose, so a fabricated error carries no
// evidence.
func TestReleaseIfCurrentTreatsAnUnresolvableIDAsNotHeld(t *testing.T) {
	runner := &releaseVerbRunner{
		show: func(id string) ([]byte, error) {
			return nil, exitErrorWithDetail(t, 1, `Error resolving `+id+`: no issue found matching "`+id+`"`)
		},
		reply: func(args []string) ([]byte, error) {
			if isReleaseVerb(args) {
				return []byte(`{"error":"1 of 1 issues failed to update","failed":[{"id":"bd-42","error":"resolving issue: no issue found matching \"bd-42\""}],"schema_version":1}`),
					exitErrorWithDetail(t, 1, `Error resolving bd-42: no issue found matching "bd-42"`)
			}
			return nil, fmt.Errorf("unexpected %v", args)
		},
	}
	released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
	if err != nil {
		t.Fatalf("an unresolvable id must not error: %v", err)
	}
	if released {
		t.Fatal("released = true for an unresolvable id")
	}
}

// TestReleaseIfCurrentRefusesAFuzzyIDCollision is the exact-ID guard: bd's
// resolver prefix/substring-matches an id with no exact hit, so handing it
// through unverified releases a bead the caller never named — and reports
// success. The CAS preconditions are no defense, because bd evaluates them
// against the bead it resolved, and one worker commonly holds several. The
// statement this verb replaced matched `WHERE id = <literal>` and was therefore
// exact by construction.
func TestReleaseIfCurrentRefusesAFuzzyIDCollision(t *testing.T) {
	runner := &releaseVerbRunner{
		// bd resolved a LONGER id — the gcy-g4o shape ("gcy-dv7" →
		// "gcy-wisp-dv78"), which gascity meets constantly because formula steps
		// run as <prefix>-wisp-<suffix> wisps.
		show: func(string) ([]byte, error) { return []byte(`[{"id":"bd-42-wisp-7"}]`), nil },
		// A capable bd would happily perform this write and exit 0.
		reply: func([]string) ([]byte, error) { return nil, nil },
	}

	released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
	if !errors.Is(err, beads.ErrIDCollision) {
		t.Fatalf("ReleaseIfCurrent released a bead the caller never named: released=%v err=%v, want an error wrapping ErrIDCollision", released, err)
	}
	if released {
		t.Fatal("released = true for an id that resolved to a different bead")
	}
	if mutations := runner.releaseVerbArgv(); len(mutations) != 0 {
		t.Fatalf("the release still reached bd: %v", mutations)
	}
}

// TestReleaseIfCurrentSurfacesInfraFailuresAsErrors guards the other half of
// "the CAS verdict is a number": a failure that is not a precondition miss and
// not an unresolvable bead must surface as an error, never as the authoritative
// "nobody holds it" answer. The package-wide isBdNotFound matches a bare
// "not found" anywhere in the runner error, which every one of these carries.
func TestReleaseIfCurrentSurfacesInfraFailuresAsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{
			name: "a renamed or unmounted database",
			err:  exitErrorWithDetail(t, 1, `Error 1049 (42000): database "fe" not found on Dolt server at 127.0.0.1:3306`),
		},
		{
			name: "bd fell off PATH",
			err:  errors.New(`exec: "bd": executable file not found in $PATH`),
		},
		{
			name: "a hosted gateway misroute",
			out:  "404 page not found",
			err:  exitErrorWithDetail(t, 1, "404 page not found"),
		},
		{
			name: "the beads workspace is gone",
			err:  exitErrorWithDetail(t, 1, "beads workspace not found: /city/.beads"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
				if isReleaseVerb(args) {
					return []byte(tc.out), tc.err
				}
				return nil, fmt.Errorf("unexpected %v", args)
			}}
			released, err := beads.NewBdStore("/city", runner.run).ReleaseIfCurrent("bd-42", "worker-1")
			if err == nil {
				t.Fatalf("an infrastructure failure became a CAS verdict: released=%v err=<nil>", released)
			}
			if released {
				t.Fatal("released = true on a failure")
			}
		})
	}
}

// TestReleaseIfCurrentFallsBackToSQLOnAnOldBd is the bd 1.0.4 compatibility
// proof: the minimum supported bd rejects the flags, and the store must reach
// the raw-SQL statement. The statement mints a fresh revision — a release that
// left the pre-release token current would keep a stale fence valid — so the
// token is the one part of it that cannot be pinned.
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
	calls := runner.releaseVerbArgv()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want the verb probe then the SQL fallback", calls)
	}
	wantQuery := "UPDATE issues SET status = 'open', assignee = '', updated_at = CURRENT_TIMESTAMP, revision = <revision>" +
		" WHERE id = 'bd-42' AND status = 'in_progress' AND assignee = 'worker-''1'"
	got := append([]string(nil), calls[1]...)
	got[len(got)-1] = normalizeReleaseRevisionQuery(t, got[len(got)-1])
	want := []string{"bd", "sql", "--json", wantQuery}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
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
