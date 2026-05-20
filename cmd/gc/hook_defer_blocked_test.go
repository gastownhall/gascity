package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestDoHookFiltersDeferredBeads(t *testing.T) {
	future := "2099-01-01T00:00:00Z"
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"yijh.3","status":"open","defer_until":"` + future + `"},
			{"id":"ready-1","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "yijh.3") {
		t.Errorf("future-deferred bead surfaced in hook output: %s", out)
	}
	if !strings.Contains(out, "ready-1") {
		t.Errorf("ready bead missing from hook output: %s", out)
	}
}

func TestDoHookFiltersDepBlockedBeads(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"a4b8.6.11","status":"open","blocked_by":[{"id":"a4b8.6.10","status":"open"}]},
			{"id":"clear-1","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "a4b8.6.11") {
		t.Errorf("dep-blocked bead surfaced in hook output: %s", out)
	}
	if !strings.Contains(out, "clear-1") {
		t.Errorf("clear bead missing from hook output: %s", out)
	}
}

func TestDoHookFiltersUnreadyPartialOutputOnCommandError(t *testing.T) {
	future := "2099-01-01T00:00:00Z"
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"deferred-1","status":"open","defer_until":"` + future + `"},
			{"id":"blocked-1","status":"open","blocked_by":[{"id":"blocker-1","status":"open"}]},
			{"id":"ready-1","status":"open"}
		]`, fmt.Errorf("timed out after 15s with partial stdout")
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHook() = %d, want 1 on command error", code)
	}
	out := stdout.String()
	for _, blocked := range []string{"deferred-1", "blocked-1"} {
		if strings.Contains(out, blocked) {
			t.Errorf("unready bead %q surfaced in hook partial output: %s", blocked, out)
		}
	}
	if !strings.Contains(out, "ready-1") {
		t.Errorf("ready bead missing from hook partial output: %s", out)
	}
	if !strings.Contains(stderr.String(), "partial stdout") {
		t.Fatalf("stderr = %q, want command error diagnostic", stderr.String())
	}
}

func TestDoHookKeepsPastDeferredAndClosedBlockers(t *testing.T) {
	past := "2000-01-01T00:00:00Z"
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"past-deferred","status":"open","defer_until":"` + past + `"},
			{"id":"closed-blocker","status":"open","blocked_by":[{"id":"blocker-1","status":"closed"}]}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"past-deferred", "closed-blocker"} {
		if !strings.Contains(out, want) {
			t.Errorf("ready bead %q missing from hook output: %s", want, out)
		}
	}
}

func TestDoHookWarnsWhenJSONLikeOutputCannotBeFiltered(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[{"id":"ready-1"}`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0 for legacy fail-open behavior; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot filter work query JSON") {
		t.Fatalf("stderr = %q, want filter diagnostic", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "ready-1") {
		t.Fatalf("stdout = %q, want original output preserved", got)
	}
}
