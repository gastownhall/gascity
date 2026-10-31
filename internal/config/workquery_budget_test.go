package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectiveWorkQueryNoWorkBdInvocationBudget is the `gc hook --claim` row
// of the hook request budget, whose other rows are TestHookRequestBudget in
// cmd/gc. `gc hook --claim` issues no store operation of its own before the
// work query; the default work query is a generated script whose every `bd`
// invocation is a process start and a connection handshake against the ledger.
// With no work available the script walks every tier, so this pins how many
// `bd` processes that costs a configured named session
// (GC_SESSION_NAME == GC_ALIAS).
//
// The script visits each distinct identity once and scans the open ephemeral
// tier once, so the ceiling is the design's budget: 8 processes.
func TestEffectiveWorkQueryNoWorkBdInvocationBudget(t *testing.T) {
	const ceiling = 8

	logPath := filepath.Join(t.TempDir(), "bd-invocations.log")
	a := Agent{Name: "planner-a"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_ID":   "gc-session-1",
		"GC_SESSION_NAME": "planner-a",
		"GC_ALIAS":        "planner-a",
		"BD_INVOCATIONS":  logPath,
	}, `#!/bin/sh
printf '%s\n' "$*" >> "$BD_INVOCATIONS"
printf '[]'
`)
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("work query with no work = %q, want []", out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd invocation log: %v", err)
	}
	invocations := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(invocations) > ceiling {
		t.Fatalf("work query with no work ran %d bd processes, ceiling %d:\n%s", len(invocations), ceiling, strings.Join(invocations, "\n"))
	}
	t.Logf("work query with no work ran %d bd processes (ceiling %d)", len(invocations), ceiling)
}

// TestEffectiveWorkQueryVisitsEachDistinctIdentityOnce pins the identity
// loop's dedupe: an identity equal to one already visited is skipped, a
// distinct one is still asked, and each distinct identity is asked in the
// original order.
func TestEffectiveWorkQueryVisitsEachDistinctIdentityOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"name equals alias", map[string]string{"GC_SESSION_ID": "gc-1", "GC_SESSION_NAME": "worker", "GC_ALIAS": "worker"}, []string{"gc-1", "worker"}},
		{"name equals id", map[string]string{"GC_SESSION_ID": "gc-1", "GC_SESSION_NAME": "gc-1", "GC_ALIAS": "worker"}, []string{"gc-1", "worker"}},
		{"alias equals id", map[string]string{"GC_SESSION_ID": "gc-1", "GC_SESSION_NAME": "runtime-1", "GC_ALIAS": "gc-1"}, []string{"gc-1", "runtime-1"}},
		{"all distinct", map[string]string{"GC_SESSION_ID": "gc-1", "GC_SESSION_NAME": "runtime-1", "GC_ALIAS": "worker"}, []string{"gc-1", "runtime-1", "worker"}},
		{"only alias", map[string]string{"GC_ALIAS": "worker"}, []string{"worker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "bd-invocations.log")
			env := map[string]string{"BD_INVOCATIONS": logPath}
			for k, v := range tc.env {
				env[k] = v
			}
			runEffectiveWorkQuery(t, Agent{Name: "worker"}, env, `#!/bin/sh
printf '%s\n' "$*" >> "$BD_INVOCATIONS"
printf '[]'
`)
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read bd invocation log: %v", err)
			}
			var inProgress, ready []string
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if id, ok := assigneeOf(line, "list --status in_progress --assignee="); ok {
					inProgress = append(inProgress, id)
				}
				if id, ok := assigneeOf(line, "ready --assignee="); ok {
					ready = append(ready, id)
				}
			}
			if strings.Join(inProgress, ",") != strings.Join(tc.want, ",") || strings.Join(ready, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("asked in-progress for %v and ready for %v, want each of %v once in order", inProgress, ready, tc.want)
			}
		})
	}
}

func assigneeOf(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	return strings.Fields(rest)[0], true
}

// TestEffectiveWorkQueryPoolDemandReusesTheOpenEphemeralScan pins that the
// routed tier's legacy ephemeral candidates come from the open-ephemeral scan
// the assigned ready tier already took — one scan — and still serve the same
// routed wisp.
func TestEffectiveWorkQueryPoolDemandReusesTheOpenEphemeralScan(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bd-invocations.log")
	out := runEffectiveWorkQuery(t, Agent{Name: "worker", Dir: "foundations"}, map[string]string{
		"GC_SESSION_ID":   "gc-1",
		"GC_SESSION_NAME": "worker",
		"GC_ALIAS":        "worker",
		"BD_INVOCATIONS":  logPath,
	}, `#!/bin/sh
printf '%s\n' "$*" >> "$BD_INVOCATIONS"
case "$*" in
  *"ephemeral=true AND status=open"*)
    printf '[{"id":"ga-legacy-wisp","issue_type":"task","status":"open","ephemeral":true,"created_at":"2026-05-01T00:00:00Z","metadata":{"gc.routed_to":"foundations/worker"}}]' ;;
  *) printf '[]' ;;
esac
`)
	if !strings.Contains(out, "ga-legacy-wisp") {
		t.Fatalf("work query = %q, want the legacy ephemeral routed wisp", out)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd invocation log: %v", err)
	}
	if n := strings.Count(string(data), "ephemeral=true AND status=open"); n != 1 {
		t.Fatalf("scanned the open ephemeral tier %d times, want 1:\n%s", n, data)
	}
}
