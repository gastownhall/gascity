package main

import (
	"bytes"
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

func TestDoHookFiltersIsBlockedBeads(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"blocked-head","status":"open","is_blocked":true},
			{"id":"ready-behind","status":"open","is_blocked":false}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "blocked-head") {
		t.Errorf("is_blocked bead surfaced in hook output: %s", out)
	}
	if !strings.Contains(out, "ready-behind") {
		t.Errorf("ready bead behind blocked head missing from hook output: %s", out)
	}
}

func TestDoHookFiltersStatusBlockedBeads(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[
			{"id":"status-blocked","status":"blocked"},
			{"id":"clear-2","status":"open"}
		]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "status-blocked") {
		t.Errorf("status=blocked bead surfaced in hook output: %s", out)
	}
	if !strings.Contains(out, "clear-2") {
		t.Errorf("ready bead missing from hook output: %s", out)
	}
}

func TestDoHookKeepsAbsentIsBlocked(t *testing.T) {
	runner := func(_, _ string) (string, error) {
		return `[{"id":"no-is-blocked-field","status":"open"}]`, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHook("bd ready", ".", false, runner, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no-is-blocked-field") {
		t.Errorf("bead with absent is_blocked treated as blocked: %s", stdout.String())
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
