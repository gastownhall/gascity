package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectiveWorkQueryNoWorkBdInvocationBudget is the `gc hook --claim` row
// of the hook request budget (engdocs/design/worker-hook-request-budget.md).
// `gc hook --claim` issues no store operation of its own before the work query;
// the default work query is a generated script whose every `bd` invocation is a
// process start and a connection handshake against the ledger. With no work
// available the script walks every tier, so this pins how many `bd` processes
// that costs a configured named session (GC_SESSION_NAME == GC_ALIAS).
//
// The ceiling is today's count; it ratchets down as the script stops visiting
// duplicate identities and re-reading the cached ephemeral set.
func TestEffectiveWorkQueryNoWorkBdInvocationBudget(t *testing.T) {
	const ceiling = 11

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
