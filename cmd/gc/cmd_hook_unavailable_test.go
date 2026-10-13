package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// gc hook must distinguish "store unreachable" (exit 2 + token) from
// "no work" (exit 1): rendering an unreachable store as no-work is the
// chronic idle-agents-with-work-waiting dead-drop (R-INV, plan item 1.3).

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

func TestDoHookOrdinaryErrorStaysExitOne(t *testing.T) {
	runner := func(string, string) (string, error) {
		return "", fmt.Errorf("running work query %q: exit status 1: unknown flag --bogus", "bd ready --bogus")
	}
	var stdout, stderr bytes.Buffer
	code := doHook("bd ready --bogus", "/tmp/work", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 1 {
		t.Fatalf("doHook = %d, want 1 for an ordinary work-query failure", code)
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
// code a hook consumer reads to tell "store down" from "no work". The fork
// parent mapped the code through exitForCode like `hook run` does; a resync
// regressed it to `!= 0 → errExit`.
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
