package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// These pin hookStoreUnavailableToken's contract; its doc states the scope. A
// work query that fails timed-out or transport-class is exit 2 plus the token,
// never the exit-1 no-work answer. Plain gc hook exits 2 for any other failed
// query too (hookExitWorkQueryFailed), without the token.

func TestDoHookStoreUnavailableExitsTwoWithToken(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json", beads.ErrStoreUnavailable)
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 2 {
		t.Fatalf("doHook = %d, want 2 for ErrStoreUnavailable; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}

func TestDoHookTransportClassErrorClassifiedUnavailable(t *testing.T) {
	// Work queries shell out to bd; a wedged store presents as a raw exec
	// error whose stderr carries the pinned transport markers.
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused", "bd ready --json")
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 2 {
		t.Fatalf("doHook = %d, want 2 for a transport-class work-query failure; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}

func TestDoHookOrdinaryErrorExitsFailedWithoutToken(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: exit status 1: unknown flag --bogus", "bd ready --bogus")
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --bogus", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != hookExitWorkQueryFailed {
		t.Fatalf("doHook = %d, want %d for an ordinary work-query failure (not the no-work exit 1)", code, hookExitWorkQueryFailed)
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q carries the store-unavailable token for an application error", stderr.String())
	}
}

// TestDoHookStoreUnavailableSuppressesPartialStdout pins that a store-unavailable
// failure prints none of the query's partial stdout: shellWorkQueryWithEnv hands
// back whatever a timed-out query wrote alongside the typed deadline, and a
// partial "[]" on stdout is the no-work answer. TestHookCommandErrorPrintsPartialOutput
// is the companion: an ordinary failure still prints it.
func TestDoHookStoreUnavailableSuppressesPartialStdout(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "[]\n", fmt.Errorf("running work query %q: timed out after 1s with partial stdout %q: %w", "bd ready", "[]", context.DeadlineExceeded)
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 2 {
		t.Fatalf("doHook = %d, want 2 for a timed-out work query; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want no partial output when the store is unavailable", got)
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
	if !strings.Contains(stderr.String(), `partial stdout "[]"`) {
		t.Fatalf("stderr = %q, want the timeout diagnostic with the partial output", stderr.String())
	}
}

func TestDoHookNoWorkStaysExitOne(t *testing.T) {
	runner := func(string, string) (string, error) { return "", nil }
	var stdout, stderr bytes.Buffer
	if code := doHook("bd ready --json", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{}); code != 1 {
		t.Fatalf("doHook = %d, want 1 for empty output (no work)", code)
	}
}

func TestClassifyWorkQueryStoreUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"already typed", fmt.Errorf("x: %w", beads.ErrStoreUnavailable), true},
		{"dial tcp", errors.New("Error: dial tcp 127.0.0.1:3307: connect: connection refused"), true},
		{"server unreachable", errors.New("bd: server unreachable"), true},
		{"silent fallback pair", errors.New("auto-importing 312 issues into empty database"), true},
		// bd's circuit-breaker fail-fast names an outage but is not a retry
		// marker, so the hook classifies it outside the shared table.
		{"breaker open", errors.New("Error: failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"), true},
		// Markers match case-insensitively: io.ErrUnexpectedEOF renders as
		// "unexpected EOF" against the lowercase "unexpected eof" marker.
		{"unexpected EOF", errors.New("running work query \"bd ready --json\": exit status 1: Error: read tcp 127.0.0.1:52144->127.0.0.1:3307: unexpected EOF"), true},
		{"application error from the work-query runner", errors.New(`running work query "bd ready --bogus": exit status 1: unknown flag --bogus`), false},
		// A federated reader times a store leg out on that store's own deadline,
		// inside hookWorkQueryTimeout, and the leg's timeout reaches gc hook
		// only as stderr text.
		{"federated bd leg timed out", errors.New(`running work query "gc ready --json": exit status 1: gc ready: rig "rig-a" store: bd ready: timed out after 2m0s`), true},
		{"federated leg context deadline", errors.New(`running work query "gc ready --json": exit status 1: gc ready: rig "rig-a" store: context deadline exceeded`), true},
		{"federated leg socket timeout", errors.New(`running work query "gc ready --json": exit status 1: gc ready: city store: read tcp 127.0.0.1:52144->127.0.0.1:3307: i/o timeout`), true},
		// The runner quotes the command line into its error, and a work query
		// is user-authored shell, so a marker or timeout needle that appears
		// only in the command text says nothing about the store. The quoted
		// span is cut as a Go string literal, so an escaped quote does not end
		// it early, and the cut is measured on the text it slices, so runes
		// whose lowercase is narrower (U+212A KELVIN SIGN lowercases to a
		// one-byte "k") do not shift it.
		{"marker only in the command text", fmt.Errorf("running work query %q: exit status 1: %s", "bd ready --json; exit 1 # connection refused", "unknown flag --bogus"), false},
		{"timeout needle only in the command text", fmt.Errorf("running work query %q: exit status 1: %s", "bd ready --json; exit 1 # timed out after", "unknown flag --bogus"), false},
		{"marker after an escaped quote in the command text", fmt.Errorf("running work query %q: exit status 1: %s", `bd ready --json --title "x" # dial tcp`, "unknown flag --bogus"), false},
		{"breaker phrase only in the command text", fmt.Errorf("running work query %q: exit status 1: %s", "bd ready --json; exit 1 # server appears down, failing fast", "unknown flag --bogus"), false},
		{"killed-query needle only in the command text", fmt.Errorf("running work query %q: exit status 1: %s", "bd ready --json; exit 1 # signal: killed", "unknown flag --bogus"), false},
		{"marker behind narrowing runes in the command text", fmt.Errorf("running work query %q: exit status 1: %s", strings.Repeat("\u212a", 10)+" # connection refused", "unknown flag --bogus"), false},
		{"stderr marker behind a quoted command", fmt.Errorf("running work query %q: exit status 1: %s", `bd ready --json --title "x"`, "Error: dial tcp 127.0.0.1:3307: connection refused"), true},
		{"stderr marker behind the claim-window note", hookClaimRetryWindowClosed(fmt.Errorf("running work query %q: exit status 1: %s", "bd ready --json", "Error: dial tcp 127.0.0.1:3307: connection refused"), &hookClaimOps{}), true},
		{"unterminated command quote scans the whole message", errors.New(`running work query "bd ready --json: exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused`), true},
		{"runner-wrapped context deadline", fmt.Errorf("running work query: %w", context.DeadlineExceeded), true},
		{"context deadline behind a message", fmt.Errorf("gc hook: work query exceeded 30s: %w", context.DeadlineExceeded), true},
		// No typed error crosses the process boundary, so bd's own deadlines
		// arrive as text; a query that timed out never answered.
		{"bd deadline text", errors.New("Error: failed to list ready issues: context deadline exceeded"), true},
		{"bd timeout text", errors.New("Error: failed to list ready issues: timed out after 30s"), true},
		{"killed work query", errors.New(`running work query "bd ready --json": signal: killed`), true},
		// Read-phase failures strike once a query is under way, past every
		// connect-time marker.
		{"invalid connection", errors.New("Error: failed to list ready issues: invalid connection"), true},
		{"read i/o timeout", errors.New("Error: failed to list ready issues: read tcp 127.0.0.1:52144->127.0.0.1:3307: i/o timeout"), true},
		{"connection reset by peer", errors.New("Error: failed to list ready issues: read tcp 127.0.0.1:52144->127.0.0.1:3307: read: connection reset by peer"), true},
		{"lost connection", errors.New("Error: failed to list ready issues: Error 2013 (HY000): Lost connection to MySQL server during query"), true},
		{"server has gone away", errors.New("Error: failed to list ready issues: Error 2006 (HY000): MySQL server has gone away"), true},
		{"too many connections", errors.New("Error: failed to open database: Error 1040 (HY000): Too many connections"), true},
		{"application error", errors.New("exit status 1: unknown flag"), false},
		{"issue not found", errors.New("Error: issue gc-123 not found"), false},
		{"command not found", errors.New(`running work query "bdx ready": exit status 127: sh: 1: bdx: not found`), false},
		// The store answered and refused: the dispatcher retries this as
		// semantic contention, but it is an answer, not an outage.
		{"blocked close refusal", errors.New("Error: cannot close blocked issue gc-1"), false},
	}
	for _, tc := range cases {
		got := classifyWorkQueryStoreUnavailable(tc.err)
		if tc.want && !errors.Is(got, beads.ErrStoreUnavailable) {
			t.Errorf("%s: classified err = %v, want ErrStoreUnavailable", tc.name, got)
		}
		if !tc.want && errors.Is(got, beads.ErrStoreUnavailable) {
			t.Errorf("%s: classified err = %v, want NOT ErrStoreUnavailable", tc.name, got)
		}
		if tc.err == nil && got != nil {
			t.Errorf("%s: classified nil to %v", tc.name, got)
		}
	}
}

// TestClassifyWorkQueryStoreUnavailableCoversBdConnectionErrors pins parity with
// the messages bd's Dolt store counts as the server being down (isConnectionError
// in beads v1.3.1, internal/storage/dolt/circuit.go, the breaker's trip test):
// a work query failing with any of them got no answer from the store.
func TestClassifyWorkQueryStoreUnavailableCoversBdConnectionErrors(t *testing.T) {
	for _, marker := range []string{
		"connection refused",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"bad connection",
		"invalid connection",
		"lost connection",
		"gone away",
	} {
		err := fmt.Errorf("running work query %q: exit status 1: Error: failed to list ready issues: %s", "bd ready --json", marker)
		if !workQueryStoreUnavailable(err) {
			t.Errorf("bd connection error %q is not classified store-unavailable", marker)
		}
	}
}

// TestClaimHookStoreUnavailableEmitsToken pins the token on the --claim path.
//
// --claim is the form agents run in the dispatch loop, so this is where a
// transport-class failure most needs to be distinguishable from "no work":
// both gc hook doors must emit the token, not only the discovery read.
func TestClaimHookStoreUnavailableEmitsToken(t *testing.T) {
	withFastClaimRetries(t)
	var stdout, stderr bytes.Buffer

	failing := func(string, string, []string) (string, error) {
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json",
			errors.New("exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused"))
	}

	code := claimHookWorkWithRunner(
		"bd ready --json", "", nil,
		[]hookStore{{dir: ""}},
		hookClaimOptions{Assignee: "worker"},
		hookClaimOps{},
		failing,
		func(string, error) {},
		&stdout, &stderr,
	)

	// The code, not just the token: hookStoreUnavailableToken's doc publishes
	// exit 2, and a consumer gating on the code must be able to tell a dead
	// store from no-work on the path agents actually run.
	if code != 2 {
		t.Fatalf("claim hook returned %d on a transport failure, want 2 (the exit code the token documents); stderr=%q",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("claim path did not emit %s on a transport-class failure; a dead store is still "+
			"indistinguishable from a drained queue on the path agents actually run:\nstderr=%q",
			hookStoreUnavailableToken, stderr.String())
	}
}

// TestClaimHookStoreUnavailableEmitsTokenOnFederatedRevalidation pins the token
// on the SECOND read of a federated claim. With more than one leg,
// claimStoreWithFallback re-runs the query on the selected store before the
// mutation; when that store is the primary and the re-read is a transport-class
// failure, the claim must exit 2 with the token exactly as the first read does.
// TestClaimHookStoreUnavailableEmitsToken cannot see this path: with one leg
// claimStoreWithFallback short-circuits and never re-reads.
func TestClaimHookStoreUnavailableEmitsTokenOnFederatedRevalidation(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}}, // primary (own) store
		{dir: "riga", env: []string{"GC_STORE=riga"}}, // federated leg, empty
	}
	cityCalls := 0
	run := func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			cityCalls++
			if cityCalls == 1 {
				// Discovery sees ready work, so the claim commits to this store.
				return `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
			}
			// The store died between discovery and claim-time re-validation.
			return "", fmt.Errorf("running work query %q: %w", "bd ready --json",
				errors.New("exit status 1: Error: dial tcp 127.0.0.1:3307: connect: connection refused"))
		case "riga":
			return "[]", nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	emitted := false
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", stores[0].env, stores, opts, hookClaimOps{}, run, func(string, error) { emitted = true }, &stdout, &stderr)

	if cityCalls < 2 {
		t.Fatalf("primary store queried %d times, want the claim-time re-validation to run (>= 2); the test never reached the path it pins", cityCalls)
	}
	if !emitted {
		t.Fatal("re-validation failure did not emit the work-query failure event")
	}
	if code != 2 {
		t.Fatalf("claim hook returned %d on a transport failure at claim-time re-validation, want 2; stderr=%q",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("federated claim re-validation did not emit %s on a transport-class failure; a dead store "+
			"is indistinguishable from a drained queue on exactly the path a multi-store city runs:\nstderr=%q",
			hookStoreUnavailableToken, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("a failed read must not write a drain result; stdout=%q", stdout.String())
	}
}

// The non-transport counterpart: an application error on the same re-validation
// path stays an ordinary exit-1 failure without the token, so the token is a
// classification and not a blanket reaction to any re-read error.
func TestClaimHookApplicationErrorOnFederatedRevalidationStaysExitOne(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
	cityCalls := 0
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "riga" {
			return "[]", nil
		}
		cityCalls++
		if cityCalls == 1 {
			return `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		}
		return "", fmt.Errorf("running work query %q: %w", "bd ready --json", errors.New("exit status 1: unknown flag"))
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("bd ready --json", "city", stores[0].env, stores,
		hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true},
		hookClaimOps{}, run, func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claim hook returned %d on an application error at re-validation, want 1; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("application error was reported as store-unavailable:\nstderr=%q", stderr.String())
	}
}

// hookExitCodeCity writes a one-agent city and points GC_CITY at it, so `gc
// hook worker` runs the agent's work query through the production shell
// runner. A non-empty workQuery becomes the agent's work_query; an empty one
// leaves the generated default work query in place.
func hookExitCodeCity(t *testing.T, workQuery string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if workQuery != "" {
		cityToml += fmt.Sprintf("work_query = %q\n", workQuery)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
}

// TestHookCommandRendersStoreUnavailableAsExitTwo pins the exit-2 contract at
// the process boundary: doHook returning 2 is worthless if the cobra RunE
// folds every non-zero code into errExit (exit 1), because that is exactly the
// code a hook consumer reads to tell "store down" from "no work". RunE must map
// the code through exitForCode; folding non-zero into errExit hides exit 2 at
// the process boundary.
func TestHookCommandRendersStoreUnavailableAsExitTwo(t *testing.T) {
	cases := []struct {
		name     string
		bdStderr string
	}{
		{"transport", "Error: dial tcp 127.0.0.1:3307: connection refused"},
		{"breaker fail-fast", "Error: failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"},
		{"read-phase disconnect", "Error: failed to list ready issues: Error 2013 (HY000): Lost connection to MySQL server during query"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The query copies bd's message from a file, so the marker reaches
			// the classifier only through the captured stderr, never through
			// the command text the runner quotes into its error.
			stderrPath := filepath.Join(t.TempDir(), "bd-stderr")
			if err := os.WriteFile(stderrPath, []byte(tc.bdStderr+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			hookExitCodeCity(t, fmt.Sprintf("cat '%s' >&2; exit 1", stderrPath))

			var stdout, stderr bytes.Buffer
			code := run([]string{"hook", "worker"}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("gc hook rendered exit %d, want 2 for a store-unavailable work-query failure; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
				t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
			}
		})
	}
}

// TestHookCommandRendersNoWorkAsExitOne is the companion pin: the process
// boundary still renders an empty poll as exit 1, so restoring exit 2 above
// did not widen the no-work code.
func TestHookCommandRendersNoWorkAsExitOne(t *testing.T) {
	hookExitCodeCity(t, "printf '[]'")

	var stdout, stderr bytes.Buffer
	code := run([]string{"hook", "worker"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gc hook rendered exit %d, want 1 for no work; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q carries the store-unavailable token on an empty poll", stderr.String())
	}
}

// TestHookCommandRendersFederatedLegTimeoutAsExitTwo pins the federated shape
// end to end. A federated reader times a store leg out on that store's own
// deadline, well inside hookWorkQueryTimeout, and exits non-zero with only the
// leg's timeout text on stderr: the runner's deadline never fires, so nothing
// typed reaches gc hook. The answer is still unknown, not empty, so the process
// renders exit 2 with the token, not the no-work exit 1.
func TestHookCommandRendersFederatedLegTimeoutAsExitTwo(t *testing.T) {
	cases := []struct {
		name   string
		legErr string
	}{
		{"bd leg timed out", `gc ready: rig "rig-a" store: bd ready: timed out after 2m0s`},
		{"leg context deadline", `gc ready: rig "rig-a" store: context deadline exceeded`},
		{"leg socket timeout", `gc ready: city store: read tcp 127.0.0.1:52144->127.0.0.1:3307: i/o timeout`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The runner quotes the command line into its error, so the leg
			// text comes from a file: it can reach gc hook only through the
			// query's stderr, never through the command text.
			legErrPath := filepath.Join(t.TempDir(), "leg.err")
			if err := os.WriteFile(legErrPath, []byte(tc.legErr+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			hookExitCodeCity(t, "cat '"+legErrPath+"' >&2; exit 1")

			var stdout, stderr bytes.Buffer
			code := run([]string{"hook", "worker"}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("gc hook rendered exit %d, want 2 for a federated leg timeout; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
				t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
			}
			if !strings.Contains(stderr.String(), tc.legErr) {
				t.Fatalf("stderr %q does not name the failed leg %q", stderr.String(), tc.legErr)
			}
		})
	}
}

// TestHookCommandClassifiesStderrNotCommandText pins the classifier's scope at
// the process boundary. The runner quotes the work query's command line into
// its error, and that command is user-authored shell, so a marker or timeout
// needle that appears only there says nothing about the store: a query that
// fails with an application error carries no token (it still exits
// hookExitWorkQueryFailed, as every failed plain gc hook query does). The
// report still names the command, because only the classification skips it.
func TestHookCommandClassifiesStderrNotCommandText(t *testing.T) {
	for _, needle := range []string{"connection refused", "timed out after"} {
		t.Run(needle, func(t *testing.T) {
			hookExitCodeCity(t, "printf 'unknown flag --bogus\\n' >&2; exit 1 # "+needle)

			var stdout, stderr bytes.Buffer
			code := run([]string{"hook", "worker"}, &stdout, &stderr)
			if code != hookExitWorkQueryFailed {
				t.Fatalf("gc hook rendered exit %d, want %d for an application error whose command text carries %q; stderr=%s", code, hookExitWorkQueryFailed, needle, stderr.String())
			}
			if strings.Contains(stderr.String(), hookStoreUnavailableToken) {
				t.Fatalf("stderr %q carries the store-unavailable token for an application error", stderr.String())
			}
			if !strings.Contains(stderr.String(), "# "+needle) {
				t.Fatalf("stderr %q no longer names the failed command", stderr.String())
			}
		})
	}
}

// TestDoHookClaimRunnerErrorClassified pins tryHookClaim's own read on the
// shared contract. claimHookWorkWithRunner hands it a pre-captured runner that
// cannot fail, so only doHookClaim reaches this read today; it still reports
// through reportClaimWorkQueryFailure, so a caller that passes a live runner
// gets the token and exit 2 for a dead store, not the no-work exit 1.
func TestDoHookClaimRunnerErrorClassified(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantToken bool
	}{
		{"transport-class read exits 2", fmt.Errorf("running work query %q: exit status 1: Error: dial tcp 127.0.0.1:3307: connection refused", "bd ready --json"), 2, true},
		{"timed-out read exits 2", fmt.Errorf("running work query %q: timed out after 150s: %w", "bd ready --json", context.DeadlineExceeded), 2, true},
		{"application read error exits 1", fmt.Errorf("running work query %q: exit status 1: unknown flag --bogus", "bd ready --bogus"), 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := hookClaimOps{Runner: func(string, string) (string, error) { return "", tc.err }}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("bd ready --json", "/tmp/work", hookClaimOptions{Assignee: "worker"}, ops, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("doHookClaim = %d, want %d; stderr=%q", code, tc.wantCode, stderr.String())
			}
			if got := strings.Contains(stderr.String(), hookStoreUnavailableToken); got != tc.wantToken {
				t.Fatalf("stderr carries token = %v, want %v; stderr=%q", got, tc.wantToken, stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "gc hook --claim: ") {
				t.Fatalf("stderr %q lost the gc hook --claim prefix", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("a failed read must not write a drain result; stdout=%q", stdout.String())
			}
		})
	}
}

// TestHookCommandJSONFailurePointsAtStderrOnExitTwo pins the --json envelope
// for a store-unavailable failure: the token and the cause are on stderr, so
// the envelope must say so instead of carrying a bare "exit 2". --claim --json
// is the form agents run, and its failed read must reach stdout as that same
// envelope, not as a drain result.
func TestHookCommandJSONFailurePointsAtStderrOnExitTwo(t *testing.T) {
	// The --claim read retries a transport-class failure on a paced interval;
	// run the retries without sleeping through them.
	prev := hookClaimQueryRetryInterval
	hookClaimQueryRetryInterval = time.Nanosecond
	t.Cleanup(func() { hookClaimQueryRetryInterval = prev })
	for _, args := range [][]string{
		{"hook", "worker", "--json"},
		{"hook", "worker", "--claim", "--json"},
	} {
		t.Run(strings.Join(args[2:], " "), func(t *testing.T) {
			hookExitCodeCity(t, "printf 'Error: dial tcp 127.0.0.1:3307: connection refused\\n' >&2; exit 1")

			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("gc %s rendered exit %d, want 2; stderr=%s", strings.Join(args, " "), code, stderr.String())
			}
			var envelope struct {
				OK     bool   `json:"ok"`
				Action string `json:"action"`
				Error  struct {
					Code     string `json:"code"`
					Message  string `json:"message"`
					ExitCode int    `json:"exit_code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("stdout is not a single JSON failure envelope: %v; stdout=%q", err, stdout.String())
			}
			if envelope.OK || envelope.Action != "" || envelope.Error.ExitCode != 2 {
				t.Fatalf("envelope = %+v, want ok=false exit_code=2 and no action", envelope)
			}
			if !strings.Contains(envelope.Error.Message, "see stderr") {
				t.Fatalf("envelope message %q does not point at stderr, where the token and cause are", envelope.Error.Message)
			}
			if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
				t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
			}
		})
	}
}

// TestHookCommandRendersWorkQueryTimeoutAsExitTwo pins the wedged-store shape
// end to end: a work query that hangs past hookWorkQueryTimeout is a
// transport-class failure (the runner wraps context.DeadlineExceeded for
// exactly this), so the process renders exit 2 with the token — not the
// no-work exit 1 that drains the seat while the store is merely hung.
func TestHookCommandRendersWorkQueryTimeoutAsExitTwo(t *testing.T) {
	oldTimeout := hookWorkQueryTimeout
	hookWorkQueryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookWorkQueryTimeout = oldTimeout })
	// The query blocks well past the (shortened) timeout; the runner's
	// deadline, not the sleep, ends it.
	hookExitCodeCity(t, "sleep 5; printf '[]'")

	var stdout, stderr bytes.Buffer
	code := run([]string{"hook", "worker"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("gc hook rendered exit %d, want 2 for a timed-out work query; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}

// TestHookCommandRendersDefaultWorkQueryTimeoutAsExitTwo pins the deadline half
// on the generated default work query, which every agent without a work_query
// runs. Its single-store tiers discard bd's stderr and fall through to "[]", so
// a store that fails fast still reads as no work there; a bd that never answers
// must still overrun hookWorkQueryTimeout into exit 2 with the token.
//
// It runs plain `gc hook` in a session's context, as agents do. That context
// names the session, so no store lookup runs before the work query; an explicit
// target resolves its session through the store first, under the store's own
// command timeout rather than hookWorkQueryTimeout.
func TestHookCommandRendersDefaultWorkQueryTimeoutAsExitTwo(t *testing.T) {
	oldTimeout := hookWorkQueryTimeout
	hookWorkQueryTimeout = 500 * time.Millisecond
	t.Cleanup(func() { hookWorkQueryTimeout = oldTimeout })
	hookExitCodeCity(t, "")
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_SESSION_NAME", "worker-session")

	var stdout, stderr bytes.Buffer
	code := run([]string{"hook"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("gc hook rendered exit %d, want 2 for a default work query that overran its deadline; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want no output when the store is unavailable", got)
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
	}
}
