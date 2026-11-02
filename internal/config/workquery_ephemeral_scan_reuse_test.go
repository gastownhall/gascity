package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Regression coverage for #5712.
//
// The two ephemeral probes emitted inside the `for id in "$GC_SESSION_ID"
// "$GC_SESSION_NAME" "$GC_ALIAS"` identity loops each read a full-store
// `bd query --limit=0` scan whose predicate does not mention the loop variable
// — the identity filter is applied by jq AFTER the array comes back. The
// generated text spells the scan once, so no golden pins the cost; the `for`
// loop is what multiplies it, and only EXECUTING the script shows that. On the
// store in the report each scan cost 8-12s, so three identities spent ~40s of
// `gc hook`'s 60s work-query budget re-fetching an identical array.
//
// These tests therefore run the generated shell against a fake `bd` that logs
// every `bd query` it receives, and pin the execution count rather than the
// spelling.

// fakeBdLoggingQueries returns a fake `bd` that appends each `query`
// invocation's arguments to $GC_TEST_QUERY_LOG and serves rows verbatim.
func fakeBdLoggingQueries(inProgressRows, openRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    printf '%s\n' "$*" >> "$GC_TEST_QUERY_LOG"
    case "$*" in
      *status=in_progress*) printf '%s' '` + inProgressRows + `' ;;
      *status=open*) printf '%s' '` + openRows + `' ;;
      *) printf '[]' ;;
    esac
    ;;
  show) printf '%s' '[{"id":"wk-9","status":"in_progress","dependencies":[]}]' ;;
  *) printf '[]' ;;
esac
`
}

// scanCounts runs script against the fake bd and reports how many `bd query`
// scans each ephemeral status tier actually executed.
func scanCounts(t *testing.T, script string, bdScript string, env map[string]string) (inProgress, open int, out string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "queries")
	full := map[string]string{"GC_TEST_QUERY_LOG": log}
	for k, v := range env {
		full[k] = v
	}
	out = runShellWithFakeBd(t, script, full, bdScript)

	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, out
		}
		t.Fatalf("read query log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.Contains(line, "status=in_progress"):
			inProgress++
		case strings.Contains(line, "status=open"):
			open++
		}
	}
	return inProgress, open, out
}

// threeIdentities is the worker shape from the report: a pool session whose id,
// tmux name and alias all differ, so every identity loop runs its full three
// iterations before falling through.
var threeIdentities = map[string]string{
	"GC_SESSION_ID":   "sess-1",
	"GC_SESSION_NAME": "claude-sess-1",
	"GC_ALIAS":        "act/claude-1",
}

// TestEphemeralScanRunsOncePerStatusNotPerIdentity is the #5712 regression: the
// identity-independent scan must be read once per status for the whole query,
// not once per identity. Reverting the memo in ephemeralStatusSnapshotShell
// fails this with inProgress=3, open=3.
func TestEphemeralScanRunsOncePerStatusNotPerIdentity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := standardAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
	inProgress, open, _ := scanCounts(t, script, fakeBdLoggingQueries("[]", "[]"), threeIdentities)

	if inProgress != 1 {
		t.Errorf("ephemeral in_progress scan ran %d times across three identities, want 1", inProgress)
	}
	if open != 1 {
		t.Errorf("ephemeral open scan ran %d times across three identities, want 1", open)
	}
}

// TestLegacyControlEphemeralScanRunsOncePerStatus covers the nested
// control-dispatcher loops, where the same scan was executed up to six times.
func TestLegacyControlEphemeralScanRunsOncePerStatus(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	script := legacyControlAssignedWorkQueryScript(QueryTopology{}) + `printf "[]"`
	env := map[string]string{
		"GC_SESSION_ID":   "rig-a/control-dispatcher",
		"GC_SESSION_NAME": "claude-control-dispatcher",
		"GC_ALIAS":        "act/control-dispatcher",
	}
	inProgress, open, _ := scanCounts(t, script, fakeBdLoggingQueries("[]", "[]"), env)

	if inProgress != 1 {
		t.Errorf("legacy-control in_progress scan ran %d times, want 1", inProgress)
	}
	if open != 1 {
		t.Errorf("legacy-control open scan ran %d times, want 1", open)
	}
}

// TestEphemeralSnapshotStillMatchesLaterIdentities is the semantics half: the
// snapshot is taken while the FIRST identity is in scope, so a bead assigned to
// the third identity must still be found by filtering that shared array. A memo
// that captured the first identity's filtered result instead of the raw scan
// would pass the count tests above and strand this bead.
func TestEphemeralSnapshotStillMatchesLaterIdentities(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, tc := range []struct {
		name   string
		script string
		bd     string
		wantID string
	}{
		{
			name:   "ready tier",
			script: standardAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`,
			bd: fakeBdLoggingQueries("[]",
				`[{"id":"wk-9","status":"open","assignee":"act/claude-1","dependency_count":0}]`),
			wantID: "wk-9",
		},
		{
			name:   "in_progress tier",
			script: standardAssignedInProgressWorkQueryScript(QueryTopology{}) + `printf "[]"`,
			bd: fakeBdLoggingQueries(
				`[{"id":"wk-9","status":"in_progress","assignee":"act/claude-1"}]`, "[]"),
			wantID: "wk-9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, out := scanCounts(t, tc.script, tc.bd, threeIdentities)

			var rows []map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("tier output is not a JSON array: %v (output %q)", err, out)
			}
			if len(rows) != 1 || rows[0]["id"] != tc.wantID {
				t.Fatalf("bead assigned to the third identity was not served from the shared snapshot; got %q", out)
			}
		})
	}
}
