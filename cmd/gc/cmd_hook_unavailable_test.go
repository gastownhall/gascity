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
		{"application error", errors.New("exit status 1: unknown flag"), false},
		// A bare "timed out" STRING stays application-class until the marker
		// tables converge (ga-y8qzd); the typed deadline the runner wraps is
		// the transport signal, and it must classify.
		{"timeout string without marker or typed deadline", errors.New("timed out after 30s"), false},
		{"runner-wrapped context deadline", fmt.Errorf("running work query: %w", context.DeadlineExceeded), true},
		{"context deadline behind a message", fmt.Errorf("gc hook: work query exceeded 30s: %w", context.DeadlineExceeded), true},
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

// hookExitCodeCity writes a one-agent city whose work_query is the given shell
// command and points GC_CITY at it, so `gc hook worker` runs that command as
// its work query through the production shell runner.
func hookExitCodeCity(t *testing.T, workQuery string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
work_query = %q
`, workQuery)
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
	hookExitCodeCity(t, "printf 'Error: dial tcp 127.0.0.1:3307: connection refused\\n' >&2; exit 1")

	var stdout, stderr bytes.Buffer
	code := run([]string{"hook", "worker"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("gc hook rendered exit %d, want 2 for a transport-class work-query failure; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("stderr %q missing token %q", stderr.String(), hookStoreUnavailableToken)
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
