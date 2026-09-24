package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeGCLoggingReadyCalls = `#!/bin/sh
case "$1" in
  ready)
    printf '%s\n' "$*" >> "$GC_TEST_READY_LOG"
    printf '[]'
    ;;
  *) printf '[]' ;;
esac
`

// TestFederatedAssignedTiersReadOnceForTheWholeIdentitySet is the cost guard:
// it counts reader invocations, not wall time. Before the batch snapshot each
// generated identity loop called gc ready once per identity (three times for a
// normal session and up to six for legacy control). Each tier must now pay for
// exactly one federated read while keeping the shell loop that preserves serve
// and fallback semantics.
func TestFederatedAssignedTiersReadOnceForTheWholeIdentitySet(t *testing.T) {
	requireJQ(t)
	tests := []struct {
		name          string
		script        string
		env           map[string]string
		wantAssignees int
	}{
		{
			name:          "standard in-progress",
			script:        standardAssignedInProgressWorkQueryScript(federatedTopology()) + `printf "[]"`,
			env:           threeIdentities,
			wantAssignees: 3,
		},
		{
			name:          "standard in-progress deferring anchor",
			script:        standardAssignedInProgressWorkQueryScriptDeferringGraphAnchor(federatedTopology()) + `printf "[]"`,
			env:           threeIdentities,
			wantAssignees: 3,
		},
		{
			name:          "standard ready",
			script:        standardAssignedReadyWorkQueryScript(federatedTopology()) + `printf "[]"`,
			env:           threeIdentities,
			wantAssignees: 3,
		},
		{
			name:   "legacy in-progress",
			script: legacyControlAssignedInProgressWorkQueryScript(federatedTopology()) + `printf "[]"`,
			env: map[string]string{
				"GC_SESSION_ID":   "rig/control-dispatcher",
				"GC_SESSION_NAME": "city/control-dispatcher",
				"GC_ALIAS":        "control-dispatcher",
			},
			wantAssignees: 6,
		},
		{
			name:   "legacy in-progress deferring anchor",
			script: legacyControlAssignedInProgressWorkQueryScriptDeferringGraphAnchor(federatedTopology()) + `printf "[]"`,
			env: map[string]string{
				"GC_SESSION_ID":   "rig/control-dispatcher",
				"GC_SESSION_NAME": "city/control-dispatcher",
				"GC_ALIAS":        "control-dispatcher",
			},
			wantAssignees: 6,
		},
		{
			name:   "legacy ready",
			script: legacyControlAssignedReadyWorkQueryScript(federatedTopology()) + `printf "[]"`,
			env: map[string]string{
				"GC_SESSION_ID":   "rig/control-dispatcher",
				"GC_SESSION_NAME": "city/control-dispatcher",
				"GC_ALIAS":        "control-dispatcher",
			},
			wantAssignees: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "ready.log")
			env := map[string]string{"GC_TEST_READY_LOG": logPath}
			for k, v := range tt.env {
				env[k] = v
			}
			res := runGeneratedQuery(t, tt.script, env, fakeGCLoggingReadyCalls)
			if res.exit != 0 {
				t.Fatalf("query exited %d; stderr=%s", res.exit, res.stderr)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read gc call log: %v", err)
			}
			lines := strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' })
			if len(lines) != 1 {
				t.Fatalf("gc ready ran %d times, want exactly 1 for the whole identity set; calls=%q", len(lines), string(data))
			}
			if got := strings.Count(lines[0], "--assignee="); got != tt.wantAssignees {
				t.Fatalf("snapshot carried %d assignee flags, want %d; call=%q", got, tt.wantAssignees, lines[0])
			}
		})
	}
}
