package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBd131InProgressList models bd 1.3.1's `bd list --status in_progress`: the
// blocked row comes first, and --ready (which 1.3.1 intersects with --status)
// drops it before --limit. `bd show` reports the blocked row's open blocker.
const fakeBd131InProgressList = `#!/bin/sh
case "$1" in
  list)
    case "$*" in
      *"--status in_progress"*"--ready"*) printf '%s' '[{"id":"ga-workable","status":"in_progress","assignee":"builder"}]' ;;
      *"--status in_progress"*) printf '%s' '[{"id":"ga-blocked","status":"in_progress","assignee":"builder"}]' ;;
      *) printf '[]' ;;
    esac ;;
  show)
    case "$2" in
      ga-blocked) printf '%s' '[{"id":"ga-blocked","status":"in_progress","dependencies":[{"id":"ga-gate","status":"open","dependency_type":"blocks"}]}]' ;;
      *) printf '%s' '[{"id":"'"$2"'","status":"in_progress","dependencies":[]}]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`

// TestCmdHookClaimBDListReadyStatusAdoptsWorkableInProgressBehindBlocked runs
// gc hook --claim on the DEFAULT work query: with [beads]
// bd_list_ready_status set, an agent whose first in_progress bead is blocked
// adopts its other, workable in_progress bead (existing_assignment). Unset, the
// blocked row fills the limit-1 read and the hook finds no work.
func TestCmdHookClaimBDListReadyStatusAdoptsWorkableInProgressBehindBlocked(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, tc := range []struct {
		name      string
		beads     string
		wantCode  int
		wantBead  string
		wantDrain bool
	}{
		{name: "set", beads: "\n[beads]\nbd_list_ready_status = true\n", wantCode: 0, wantBead: "ga-workable"},
		{name: "unset", beads: "", wantCode: 1, wantDrain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			cityDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			cityToml := `[workspace]
name = "test-city"
` + tc.beads + `
[[agent]]
name = "builder"
max_active_sessions = 3

[[named_session]]
template = "builder"
mode = "on_demand"
`
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			fakeBin := t.TempDir()
			if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(fakeBd131InProgressList), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_TEMPLATE", "builder")
			t.Setenv("GC_ALIAS", "builder")
			t.Setenv("GC_SESSION_NAME", "builder-session")
			t.Setenv("GC_SESSION_ID", "session-builder")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("cmdHookWithOptions(--claim) = %d, want %d; stdout=%q stderr=%s", code, tc.wantCode, stdout.String(), stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
			}
			if tc.wantDrain {
				if result.Action != "drain" || result.Reason != "no_work" {
					t.Fatalf("result = %+v, want action=drain reason=no_work", result)
				}
				return
			}
			if result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != tc.wantBead {
				t.Fatalf("result = %+v, want work/existing_assignment/%s", result, tc.wantBead)
			}
			if strings.Contains(stdout.String(), "ga-blocked") {
				t.Fatalf("the blocked row reached the claim result: %s", stdout.String())
			}
		})
	}
}
