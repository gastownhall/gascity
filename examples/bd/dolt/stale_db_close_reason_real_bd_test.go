//go:build integration

package dolt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/test/toolhome"
)

// TestStaleDBFormulaCloseKeepsTheReasonInBdsColumn runs the formula's success
// path against a REAL bd scope, the shape of a single-store city: every
// `gc bd` the formula runs reaches bd unchanged, as gc's passthrough hands bd a
// command about a bead bd holds. The scan summary must land in bd's
// close_reason column, which `bd show` and gc's BdStore read as the bead's close
// reason. A status-closed `bd update` closes the bead with that column emptied
// and the summary only in metadata, so this fails on that shape.
//
// The wisp is claimed first and the formula runs as the claimant with no
// --actor, because bd fences a close on actor == assignee: that is the fence a
// dog session's close has to pass.
func TestStaleDBFormulaCloseKeepsTheReasonInBdsColumn(t *testing.T) {
	realBD, err := exec.LookPath("bd")
	if err != nil {
		t.Skipf("bd not found: %v", err)
	}
	for _, tool := range []string{"bash", "jq", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
	}

	root := t.TempDir()
	scope := filepath.Join(root, "scope")
	binDir := filepath.Join(root, "bin")
	home := filepath.Join(root, "tool-home")
	for _, dir := range []string{scope, binDir, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	logPath := filepath.Join(root, "commands.log")
	scanPath := filepath.Join(root, "scan.json")
	applyPath := filepath.Join(root, "apply.json")
	report := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":100},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":100,"bytes_freed_rss":0,"errors_total":0}}`
	writeTestFile(t, scanPath, report)
	writeTestFile(t, applyPath, report)
	writeTestFile(t, logPath, "")
	// The gc router: `gc bd ...` is bd's, the cleanup is the canned report, and
	// the session-side verbs are logged.
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "bd "*)
    shift
    exec bd "$@"
    ;;
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	const claimant = "dog-1"
	// bd and every process the formula starts resolve nothing from the invoking
	// user's home or a live Dolt server: the scope's own embedded database only.
	env := toolhome.Environ(append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"PATH="+binDir+string(os.PathListSeparator)+filepath.Dir(realBD)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+root,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
		"BEADS_DIR="+filepath.Join(scope, ".beads"),
		"BEADS_ACTOR="+claimant,
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_TEST_MODE=1",
		"DOLT_DISABLE_EVENT_FLUSH=true",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"),
	), home, "BEADS_DIR", "BEADS_ACTOR", "BEADS_DOLT_AUTO_START", "BEADS_TEST_MODE")

	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = scope
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("git", "init", "--quiet", scope)
	run("bd", "init", "-p", "tst", "--skip-hooks", "--skip-agents")
	var created struct {
		ID string `json:"id"`
	}
	createOut := run("bd", "create", "--title", "mol-dog-stale-db wisp", "--type", "task", "--json")
	if err := json.Unmarshal([]byte(createOut), &created); err != nil || created.ID == "" {
		t.Fatalf("bd create --json = %q: %v", createOut, err)
	}
	run("bd", "update", created.ID, "--claim", "--json")

	cmd := exec.Command("bash", "-s")
	cmd.Dir = scope
	cmd.Stdin = strings.NewReader(renderStaleDBFormulaShell(t))
	cmd.Env = append(append([]string{}, env...), "GC_BEAD_ID="+created.ID)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, logData, out)
	}

	var shown []struct {
		Status      string         `json:"status"`
		Assignee    string         `json:"assignee"`
		CloseReason string         `json:"close_reason"`
		Metadata    map[string]any `json:"metadata"`
	}
	showOut := run("bd", "show", created.ID, "--json")
	if err := json.Unmarshal([]byte(showOut), &shown); err != nil || len(shown) != 1 {
		t.Fatalf("bd show --json = %q: %v", showOut, err)
	}
	row := shown[0]
	if row.Status != "closed" {
		t.Fatalf("status = %q, want closed\nscript output:\n%s", row.Status, out)
	}
	if want := "Stale DB scan complete (orphans=1, applied=1, escalated=0)"; row.CloseReason != want {
		t.Errorf("bd's close_reason column = %q, want %q", row.CloseReason, want)
	}
	if row.Assignee != claimant {
		t.Errorf("assignee = %q, want the claimant %q", row.Assignee, claimant)
	}
	if got, _ := row.Metadata["gc.outcome"].(string); got != "pass" {
		t.Errorf("metadata gc.outcome = %v, want pass", row.Metadata["gc.outcome"])
	}
	for key, title := range map[string]string{
		"gc.dolt_cleanup.scan_report":  "scan (dry-run) ",
		"gc.dolt_cleanup.apply_report": "apply (--force) ",
	} {
		if got, _ := row.Metadata[key].(string); !strings.HasPrefix(got, title) || !strings.Contains(got, `"schema":"gc.dolt.cleanup.v1"`) {
			t.Errorf("metadata %s = %v, want the %q report", key, row.Metadata[key], strings.TrimSpace(title))
		}
	}
}
