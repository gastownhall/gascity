//go:build unix

package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/orders"
)

func staleDBFilteredEnv(keys ...string) []string {
	keys = append(keys,
		"GC_BEAD_ID",
		"GC_ESCALATE_SCRIPT",
		"GC_ESCALATE_SEARCH_PACKS",
		"GC_ESCALATION_RECIPIENT",
		"GC_SYSTEM_PACKS_DIR",
		"GC_MAINTENANCE_DONE_TARGET",
		// The store-call bound; never let a host value change a run's timing.
		"DOG_BD_TIMEOUT",
		// The close actor chain leads with BEADS_ACTOR; never let the host
		// session's own actor leak into a rendered-script run.
		"BEADS_ACTOR",
	)
	return filteredEnv(keys...)
}

func TestStaleDBFormulaRuntimeContract(t *testing.T) {
	root := repoRoot(t)
	f, err := formula.NewParser().ParseFile(filepath.Join(root, "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1 so shell state stays inside one formula step", len(f.Steps))
	}

	desc := f.Steps[0].Description
	for _, want := range []string{
		`set -euo pipefail`,
		`WORK_BEAD="${GC_BEAD_ID:-$(gc hook current --id-only)}"`,
		`TMP_DIR=$(mktemp -d`,
		`trap cleanup EXIT`,
		`drain_ack_once()`,
		`gc dolt-cleanup --json --probe > "$SCAN_FILE"`,
		`gc dolt-cleanup --json --probe --force --max-orphan-dbs "{{max_orphans_for_sql}}" > "$APPLY_FILE"`,
		`jq -r '.dropped.count // 0'`,
		`jq -r '[.dropped.skipped[]? | select(.reason == "invalid-identifier")] | length'`,
		`jq -r '[.force_blockers[]?] | length'`,
		`jq -r '.reaped.targets | length'`,
		`gc event emit mol-dog-stale-db.scan`,
		`gc event emit mol-dog-stale-db.drop`,
		`gc event emit mol-dog-stale-db.purge`,
		`gc event emit mol-dog-stale-db.reap`,
		`gc event emit mol-dog-stale-db.done`,
		`gc event emit mol-dog-stale-db.escalate`,
		`if [ "$APPLIED" -eq 1 ] && [ "$MISSED_PURGE_BYTES" -gt 0 ]; then`,
		`leaving work bead open`,
		`maintenance_notice "MAINTENANCE_WARN: $ORPHAN_TOTAL Dolt orphan(s) seen this scan`,
		`maintenance_notice "MAINTENANCE_DONE: stale-db - orphans: ${ORPHAN_TOTAL}, applied: ${APPLIED}, escalated: ${ESCALATED}"`,
		`escalated=${ESCALATED}`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("formula step missing %q", want)
		}
	}
	for _, bad := range []string{
		`/tmp/dolt-cleanup`,
		`gc nudge deacon`,
		`gc session nudge deacon`,
		`GC_BEAD_ID:-<work-bead>`,
		`GC_BEAD_ID:?`,
		`Dolt orphan(s) detected`,
		`GC_TRIGGER_`,
	} {
		if strings.Contains(desc, bad) {
			t.Errorf("formula step still contains retired or leaky pattern %q", bad)
		}
	}
}

func TestStaleDBFormulaRenderedShellIsStrictAndValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	for _, want := range []string{
		`set -euo pipefail`,
		`WORK_BEAD="${GC_BEAD_ID:-$(gc hook current --id-only)}"`,
		`if [ "$PROTECTED_NON_BASELINE" -ge "10" ]; then`,
		`protected_non_baseline: ((.summary.protected_total // 0) - (.summary.protected_by_kind["active-rig"] // 0))`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("rendered script missing %q", want)
		}
	}

	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

func TestStaleDBFormulaApplyErrorsLeaveWorkOpen(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want apply errors to fail before success close\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		staleDBReportWrite,
		"gc.dolt_cleanup.apply_report=apply (--force, refused)",
		"gc event emit mol-dog-stale-db.escalate",
		// A distinct call site/blocker-kind from the dry-run force-blocker
		// escalation; locks in that apply-path escalations dedup too.
		"--dedup mol-dog-stale-db:bead-1:apply-errors",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	for _, forbidden := range []string{
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		"gc event emit mol-dog-stale-db.done",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("rendered script logged forbidden success path %q despite apply errors\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
	if strings.Contains(log, staleDBCloseWrite("bead-1")) {
		t.Fatalf("rendered script closed bead successfully despite apply errors\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaApplyCommandFailureAppendsApplyJSON(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","error":"drop failed"}]}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        exit 42
        ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want failed apply command to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, "gc.dolt_cleanup.apply_report=apply (--force, failed)") {
		t.Fatalf("failed apply JSON was not appended to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, `"stage":"drop"`) {
		t.Fatalf("appended apply note missing JSON errors\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, staleDBCloseWrite("bead-1")) {
		t.Fatalf("rendered script closed bead despite failed apply command\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaDryRunFailureAppendsScanJSON(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    cat "$GC_TEST_SCAN_JSON"
    exit 42
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want dry-run failure to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, staleDBReportWrite) {
		t.Fatalf("dry-run failure did not append scan JSON to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	// The report is the stage title and timestamp, then the scan JSON verbatim
	// on the following lines, under the scan stage's own metadata key.
	if !strings.Contains(log, "gc.dolt_cleanup.scan_report=scan (dry-run failed) ") ||
		!strings.Contains(log, "\n"+`{"schema":"gc.dolt.cleanup.v1",`) || !strings.Contains(log, `"errors_total":1}}`) {
		t.Fatalf("dry-run failure report lost the scan JSON\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, staleDBCloseWrite("bead-1")) {
		t.Fatalf("rendered script closed bead despite dry-run failure\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaCleanApplyClosesWorkAndUsesDBThreshold(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":0,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":2,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"GC_ALIAS=dog-alpha",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 1200 bytes freed; 0 errors",
		staleDBCloseWrite("bead-1"),
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated at dropped.count == max_orphans_for_sql; want apply because threshold is >\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// TestStaleDBFormulaCloseIsIndependentOfIdentityEnv pins that the close
// no longer depends on the session's identity env. It used to be `bd close
// --actor <chain>`, because bd fences a close on actor == assignee byte for
// byte and an unaliased pool dog claims under its session bead ID (the #5716
// shape). The close is now `gc bd close --reason` with no --actor: on a
// single-store city gc runs bd's close as the identity that claimed the wisp
// (the session's BEADS_ACTOR is that identity, and gc re-pins it for a close
// of the session's own claim), and for a relocated wisp gc serves the close in
// process, which refuses --actor. So every identity env below must produce the
// same close and none may smuggle an --actor back in.
func TestStaleDBFormulaCloseIsIndependentOfIdentityEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "beads actor",
			env:  []string{"GC_ALIAS=", "BEADS_ACTOR=gc-dog7", "GC_SESSION_ID=gc-dog7", "GC_SESSION_NAME=dolt__dog-gc-dog7"},
			want: staleDBCloseWrite("bead-1"),
		},
		{
			name: "session id when beads actor absent",
			env:  []string{"GC_ALIAS=", "GC_SESSION_ID=gc-dog7", "GC_SESSION_NAME=dolt__dog-gc-dog7"},
			want: staleDBCloseWrite("bead-1"),
		},
		{
			name: "session name last",
			env:  []string{"GC_ALIAS=", "BEADS_ACTOR=", "GC_SESSION_NAME=dog-session-7"},
			want: staleDBCloseWrite("bead-1"),
		},
		{
			// The live pool-session shape (ga-n13msy, gm-5001gi): an unaliased
			// pool slot has BOTH GC_SESSION_ID and a stable GC_SESSION_NAME, and
			// the claim is stamped with the session ID. The close must still land.
			name: "pool session with distinct session id and name",
			env:  []string{"GC_ALIAS=", "BEADS_ACTOR=", "GC_SESSION_ID=dog-wisp-99", "GC_SESSION_NAME=dog-session-7"},
			want: staleDBCloseWrite("bead-1"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runStaleDBFormulaCloseActorCase(t, tc.env, tc.want)
		})
	}
}

func runStaleDBFormulaCloseActorCase(t *testing.T, identityEnv []string, wantClose string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":0,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":2,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	cmd.Env = append(cmd.Env, identityEnv...)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 1200 bytes freed; 0 errors",
		wantClose,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "--actor") {
		t.Fatalf("the close carries an --actor, which the in-process door refuses\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated at dropped.count == max_orphans_for_sql; want apply because threshold is >\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// TestStaleDBFormulaResolvesWorkBeadViaHookCurrentChain proves the formula
// runs in a real pool shell, where GC_BEAD_ID is not guaranteed: the bead-id
// chain must fall through to `gc hook current --id-only`, the back-channel
// stamped by `gc hook --claim`. Before this chain the formula required
// GC_BEAD_ID — an env var no session shell has ever received — so even a
// correctly claiming dog aborted at the WORK_BEAD guard (ga-2q2r0).
func TestStaleDBFormulaResolvesWorkBeadViaHookCurrentChain(t *testing.T) {
	log, out := runStaleDBFormulaViaHookCurrent(t)
	for _, want := range []string{
		"gc hook current --id-only",
		staleDBCloseWrite("bead-hook-1"),
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
}

// Kills a bead-id chain that consults GC_TRIGGER_BEAD_ID: a shell that still
// carries a foreign id there (an older gc exported the frozen demand stamp)
// closes the bead its own claim names, never the foreign one
// (gastownhall/gascity#5738).
func TestStaleDBFormulaIgnoresForeignTriggerBeadID(t *testing.T) {
	log, out := runStaleDBFormulaViaHookCurrent(t, "GC_TRIGGER_BEAD_ID=bead-foreign")
	for _, want := range []string{
		"gc hook current --id-only",
		staleDBCloseWrite("bead-hook-1"),
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "bead-foreign") {
		t.Fatalf("command log names the foreign trigger id\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// runStaleDBFormulaViaHookCurrent runs the rendered formula with GC_BEAD_ID
// unset, a fake gc whose `hook current --id-only` names bead-hook-1, and
// extraEnv appended, returning the command log and the combined output.
func runStaleDBFormulaViaHookCurrent(t *testing.T, extraEnv ...string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":100},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":100,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":100},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":100,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "hook current")
    echo "gc $*" >> "$GC_TEST_LOG"
    echo "bead-hook-1"
    ;;
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	return log, string(out)
}

// TestStaleDBFormulaFailsLoudlyWhenNoWorkBeadIDResolvable pins the chain's
// failure mode: a shell that cannot name its bead through any chain element
// must abort before touching Dolt, not run the cleanup and silently skip the
// close it owes.
func TestStaleDBFormulaFailsLoudlyWhenNoWorkBeadIDResolvable(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	writeTestFile(t, logPath, "")
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
echo "gc $*" >> "$GC_TEST_LOG"
case "${1:-} ${2:-}" in
  "hook current")
    echo "gc hook current: session has no current claim" >&2
    exit 1
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script succeeded with no resolvable work bead id\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "dolt-cleanup") {
		t.Fatalf("script must abort before running cleanup when it cannot name its bead\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaPurgeOnlyScanApplies(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 4096 bytes freed; 0 errors",
		staleDBCloseWrite("bead-1"),
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated purge-only cleanup\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaPurgeOnlyApplySQLFailureLeavesWorkOpen(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"ok":false,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":2},"errors":[{"stage":"drop","error":"open dolt connection: refused"},{"stage":"purge","error":"open dolt connection: refused"}]}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        exit 42
        ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want SQL-backed apply failure to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		staleDBReportWrite,
		"gc.dolt_cleanup.apply_report=apply (--force, failed)",
		`"stage":"purge"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, staleDBCloseWrite("bead-1")) {
		t.Fatalf("rendered script closed bead despite SQL-backed apply failure\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaExitZeroMaxOrphanRefusalLeavesWorkOpenWithoutSuccessEvents(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON:  `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
		applyJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":21,"failed":[]},"purge":{"ok":false,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","kind":"max-orphan-refusal","error":"stale database threshold tripped"}]}`,
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want exit-zero max-orphan refusal to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, forbidden := range []string{
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		staleDBCloseWrite("bead-1"),
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("exit-zero max-orphan refusal logged forbidden success path %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
	for _, want := range []string{
		staleDBReportWrite,
		"gc.dolt_cleanup.apply_report=apply (--force, refused)",
		"apply refused by max-orphan safety guard",
		"gc event emit mol-dog-stale-db.escalate",
		// Its own stream, apart from the apply errors that share this site.
		"--dedup mol-dog-stale-db:bead-1:apply-max-orphan-refusal\n",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("exit-zero max-orphan refusal log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
}

func TestStaleDBFormulaDryRunForceBlockersLeaveWorkOpenBeforeApply(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON: staleDBForceBlockerScan,
		wantNote: "gc.dolt_cleanup.scan_report=scan (dry-run)",
		wantLog:  "force blocker",
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want dry-run force blockers to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"gc event emit mol-dog-stale-db.escalate",
		"gc mail send human -s",
		// Dedup key must carry the blocker's own kind (not a static label) so a
		// *different* force blocker still alerts while repeats of this one are
		// suppressed; see pl-7etw2 (duplicate escalations ~7 minutes apart).
		"--dedup mol-dog-stale-db:bead-1:rig-protection",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("force-blocker path missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	for _, forbidden := range []string{
		"gc dolt-cleanup --json --probe --force",
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		"gc event emit mol-dog-stale-db.done",
		staleDBCloseWrite("bead-1"),
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("force-blocker path logged forbidden command %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
}

// staleDBForceBlockerScan is a dry-run report whose one force blocker sends
// the dry-run escalation and leaves the wisp open.
const staleDBForceBlockerScan = `{"schema":"gc.dolt.cleanup.v1","force_blockers":[{"kind":"rig-protection","name":"missing","error":"missing metadata"}],"dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`

// staleDBForceBlockerSubject is the subject of the escalation
// staleDBForceBlockerScan sends.
const staleDBForceBlockerSubject = "ESCALATION: Dolt cleanup dry-run reported 0 error(s), 0 invalid stale database identifier(s), 1 force blocker(s)"

// staleDBOverThresholdScan is a dry-run report with more stale databases than
// max_orphans_for_sql: it sends the max-orphan escalation and closes the wisp.
const staleDBOverThresholdScan = `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":21,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`

// staleDBCoreEscalateScript is the core pack's escalate.sh, the hook a city
// can point GC_ESCALATE_SCRIPT at.
func staleDBCoreEscalateScript(t *testing.T) string {
	t.Helper()
	// repoRoot is examples/bd/dolt; the core pack lives three levels up.
	return filepath.Join(repoRoot(t), "..", "..", "..", "internal", "bootstrap", "packs", "core", "assets", "scripts", "escalate.sh")
}

// TestStaleDBFormulaDryRunEscalationKeyIsTheSortedSetOfConditions pins the
// dry-run escalation's dedup key to every condition the scan reports, sorted.
// The scan appends blockers in a fixed order, so a key read from the first one
// alone would suppress a kind found after it; scan errors and invalid
// identifiers are conditions of their own.
func TestStaleDBFormulaDryRunEscalationKeyIsTheSortedSetOfConditions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	scan := func(blockers, skipped string, errs int) string {
		return fmt.Sprintf(`{"schema":"gc.dolt.cleanup.v1","force_blockers":[%s],"dropped":{"count":1,"failed":[],"skipped":[%s]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":%d}}`, blockers, skipped, errs)
	}
	const (
		legacyPort   = `{"kind":"legacy-fallback-port","error":"fell back to the legacy port"}`
		probeFailed  = `{"kind":"live-session-probe-failed","error":"probe timed out"}`
		rigA         = `{"kind":"rig-protection","name":"a","error":"missing metadata"}`
		rigB         = `{"kind":"rig-protection","name":"b","error":"missing metadata"}`
		invalidIdent = `{"name":"testdb_bad;drop","reason":"invalid-identifier"}`
	)
	for _, tc := range []struct {
		name     string
		scanJSON string
		wantKind string
	}{
		{"one blocker", scan(legacyPort, "", 0), "legacy-fallback-port"},
		{"a blocker found after the first", scan(legacyPort+","+probeFailed, "", 0), "legacy-fallback-port+live-session-probe-failed"},
		{"the same blockers reordered", scan(probeFailed+","+legacyPort, "", 0), "legacy-fallback-port+live-session-probe-failed"},
		{"one kind on two rigs", scan(rigA+","+rigB, "", 0), "rig-protection"},
		{"scan errors", scan("", "", 1), "scan-errors"},
		// The scan counts each invalid identifier as an error as well.
		{"invalid identifiers", scan("", invalidIdent, 1), "scan-errors+invalid-identifiers"},
		{"every condition", scan(rigA, invalidIdent, 1), "rig-protection+scan-errors+invalid-identifiers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{scanJSON: tc.scanJSON})
			if err == nil {
				t.Fatalf("rendered script exited successfully; want the dry-run escalation to keep work open\nlog:\n%s\noutput:\n%s", log, out)
			}
			if want := "--dedup mol-dog-stale-db:bead-1:" + tc.wantKind + "\n"; !strings.Contains(log, want) {
				t.Fatalf("dry-run escalation missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
			}
		})
	}
}

// TestStaleDBFormulaMaxOrphanEscalationKeyOutlivesTheWisp runs two cron cycles
// over the orphan threshold. That branch closes its wisp, so each cycle runs a
// new one, and a key naming the wisp would never repeat: the human would get
// the same escalation every cycle.
func TestStaleDBFormulaMaxOrphanEscalationKeyOutlivesTheWisp(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	for _, wisp := range []string{"wisp-a", "wisp-b"} {
		log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{scanJSON: staleDBOverThresholdScan, beadID: wisp})
		if err != nil {
			t.Fatalf("%s: rendered script failed: %v\nlog:\n%s\noutput:\n%s", wisp, err, log, out)
		}
		for _, want := range []string{
			"gc mail send human -s ESCALATION: 21 stale Dolt databases exceed max_orphans_for_sql=20",
			"--dedup mol-dog-stale-db:max-orphan-threshold\n",
			staleDBCloseWrite(wisp),
		} {
			if !strings.Contains(log, want) {
				t.Fatalf("%s: command log missing %q\nlog:\n%s\noutput:\n%s", wisp, want, log, out)
			}
		}
	}
}

// TestStaleDBFormulaEscalateHookKeepsARepeatQuiet runs the same escalation
// twice through the core pack's escalate.sh, which keeps an incident quiet
// only when the escalation names one: the repeat must not be mailed again.
func TestStaleDBFormulaEscalateHookKeepsARepeatQuiet(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	for _, tc := range []struct {
		name     string
		scanJSON string
		wisps    []string
		incident string
	}{
		{"a re-run of the wisp a force blocker leaves open", staleDBForceBlockerScan, []string{"bead-1", "bead-1"}, "mol-dog-stale-db:bead-1:rig-protection"},
		{"two cron cycles over the orphan threshold", staleDBOverThresholdScan, []string{"wisp-a", "wisp-b"}, "mol-dog-stale-db:max-orphan-threshold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{
				"GC_ESCALATE_SCRIPT=" + staleDBCoreEscalateScript(t),
				"GC_ESCALATE_INCIDENT_DIR=" + t.TempDir(),
			}
			var transcript strings.Builder
			mails := 0
			lastOut := ""
			for _, wisp := range tc.wisps {
				log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{scanJSON: tc.scanJSON, beadID: wisp, env: env})
				fmt.Fprintf(&transcript, "run on %s (exit: %v)\nlog:\n%s\noutput:\n%s\n", wisp, err, log, out)
				mails += strings.Count(log, "gc mail send ")
				lastOut = string(out)
			}
			if mails != 1 {
				t.Fatalf("%d escalation mails over two runs, want 1\n%s", mails, transcript.String())
			}
			if want := "escalate: incident " + tc.incident + " unchanged since"; !strings.Contains(lastOut, want) {
				t.Fatalf("second run output missing %q\n%s", want, transcript.String())
			}
		})
	}
}

// TestStaleDBFormulaEscalateHookOverrides covers escalate.sh overrides. One
// written before incident keys refuses --incident-key as a usage error, so the
// escalation is sent to it again without the incident flags: it still
// arrives, undeduplicated, and the run says so in one line. One that takes the
// flags and fails to send is not sent the escalation again.
func TestStaleDBFormulaEscalateHookOverrides(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	const keyedCall = "CALL --subject --message --incident-key --fingerprint\n"
	for _, tc := range []struct {
		name    string
		refusal string
		deliver string
		// resent is whether the escalation is sent again without the flags.
		resent bool
	}{
		{"1.5.0 escalate.sh parser, exit 2", escalateRefusalV150, escalateDelivers, true},
		{"getopt parser naming the flag, exit 1", escalateRefusalGetopt, escalateDelivers, true},
		{"usage error naming no flag, exit 2", escalateRefusalUsage, escalateDelivers, true},
		{"takes the flags and fails to send", escalateSkipsUnknown, escalateSendFails, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script, overrideLog, env := writeEscalateOverride(t, tc.refusal, tc.deliver)

			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{scanJSON: staleDBForceBlockerScan, env: env})
			if err == nil {
				t.Fatalf("rendered script exited successfully; want the force blocker to keep work open\nlog:\n%s\noutput:\n%s", log, out)
			}
			wantCalls, wantWarnings := keyedCall, 0
			if tc.resent {
				wantCalls += "CALL --subject --message\nSENT " + staleDBForceBlockerSubject + "\n"
				wantWarnings = 1
			} else if !strings.Contains(string(out), "escalate: pager unreachable") {
				t.Fatalf("the override's error must reach the output\noutput:\n%s", out)
			}
			if got := readEscalateLog(t, overrideLog); got != wantCalls {
				t.Fatalf("override calls:\n%s\nwant:\n%s\noutput:\n%s", got, wantCalls, out)
			}
			if got := strings.Count(string(out), "predates incident keys"); got != wantWarnings {
				t.Fatalf("%d warnings that the override predates incident keys, want %d\noutput:\n%s", got, wantWarnings, out)
			}
			if tc.resent && !strings.Contains(string(out), script+" predates incident keys") {
				t.Fatalf("the warning must name the override\noutput:\n%s", out)
			}
			if strings.Contains(log, "gc mail send ") {
				t.Fatalf("an escalation the hook takes must not also be mailed directly\nlog:\n%s", log)
			}
		})
	}
}

// TestStaleDBFormulaNeverResendsThroughTheCoreEscalateScript: the core
// escalate.sh takes the incident flags, so its failure, even under the
// usage-error status, is a failed send and the escalation is not sent again
// without them.
func TestStaleDBFormulaNeverResendsThroughTheCoreEscalateScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	core, err := os.ReadFile(staleDBCoreEscalateScript(t))
	if err != nil {
		t.Fatalf("read core escalate.sh: %v", err)
	}
	// The formula finds the core hook among the city's system packs.
	packs := t.TempDir()
	scripts := filepath.Join(packs, "core", "assets", "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestFile(t, filepath.Join(scripts, "escalate.sh"), string(core), 0o755)

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON:     staleDBForceBlockerScan,
		failContains: "gc mail send ",
		failExit:     "2",
		env:          []string{"GC_SYSTEM_PACKS_DIR=" + packs, "GC_ESCALATE_INCIDENT_DIR=" + t.TempDir()},
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want the force blocker to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	if got := strings.Count(log, "gc mail send "); got != 1 {
		t.Fatalf("%d mail sends, want the one failed keyed escalation\nlog:\n%s\noutput:\n%s", got, log, out)
	}
	if strings.Contains(string(out), "predates incident keys") {
		t.Fatalf("the core escalate.sh takes the incident flags\noutput:\n%s", out)
	}
}

// staleDBReportWrite is the logged prefix of bead-1's per-stage report write. The
// report rides the work bead's metadata through `gc bd update
// --set-metadata`, the shape gc serves in process when the order wisp lives in
// a relocated class store.
const staleDBReportWrite = "gc bd update bead-1 --set-metadata gc.dolt_cleanup."

// staleDBCloseWrite is the logged prefix of the successful close, `gc bd close
// --reason`: the one spelling that keeps the reason as the wisp's close reason
// both on a single-store city (bd's close_reason column) and where gc serves the
// wisp in process from a relocated class store.
func staleDBCloseWrite(bead string) string {
	return "gc bd close " + bead + " --reason Stale DB scan complete"
}

// staleDBOutcomeWrite is the logged outcome write the successful close follows.
func staleDBOutcomeWrite(bead string) string {
	return "gc bd update " + bead + " --set-metadata gc.outcome=pass"
}

// staleDBOutcomeWithdrawal is the logged write that takes the outcome back
// after a failed close, so the wisp left open carries no outcome.
func staleDBOutcomeWithdrawal(bead string) string {
	return "gc bd update " + bead + " --unset-metadata gc.outcome"
}

// TestStaleDBFormulaUsesOnlyShapesARelocatedWispAccepts pins the work-bead
// writes to the shapes both topologies take. On a city whose coordination
// classes live in their own store, the order wisp this formula works is held by
// that store: bare `bd` cannot see it (it reports the wisp missing), and gc
// serves `gc bd update` and `gc bd close --reason` in process but refuses
// `--append-notes` (the class store has no notes field) and `--actor`. On a
// single-store city a status-closed update would close the wisp with bd's
// close_reason column emptied, so the close must be the reasoned close and no
// update may carry a status.
func TestStaleDBFormulaUsesOnlyShapesARelocatedWispAccepts(t *testing.T) {
	script := renderStaleDBFormulaShell(t)
	updates, closes := 0, 0
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(line, `"$WORK_BEAD"`) {
			continue
		}
		if !strings.Contains(line, "bd ") {
			continue
		}
		switch {
		case strings.Contains(line, `gc bd update "$WORK_BEAD"`):
			updates++
		case strings.Contains(line, `gc bd close "$WORK_BEAD" --reason "`):
			closes++
		default:
			t.Errorf("work-bead write is neither `gc bd update` nor `gc bd close --reason`: %s", trimmed)
		}
		if strings.Count(line, "bd ") != strings.Count(line, "gc bd ") {
			t.Errorf("work-bead write runs bare bd, which cannot see a relocated wisp: %s", trimmed)
		}
		for _, refused := range []string{"--append-notes", "--notes", "--actor", "--force", "--status", " -s "} {
			if strings.Contains(line, refused) {
				t.Errorf("work-bead write uses %q, which one of the two topologies refuses or loses the close reason on: %s", refused, trimmed)
			}
		}
	}
	if updates < 2 || closes != 1 {
		t.Fatalf("found %d work-bead updates and %d closes; want the report and outcome updates and one reasoned close", updates, closes)
	}
}

// TestStaleDBFormulaRecordsTheOutcomeBeforeTheReasonedClose pins the success
// close end to end: the outcome is written first, so the wisp is never closed
// with no outcome, the close carries the scan summary as its reason, and only a
// failed close takes the outcome back (TestStaleDBFormulaSuccessPathFailuresDrainAck).
func TestStaleDBFormulaRecordsTheOutcomeBeforeTheReasonedClose(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
	})
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	outcome := strings.Index(log, staleDBOutcomeWrite("bead-1")+"\n")
	closeAt := strings.Index(log, "gc bd close bead-1 --reason Stale DB scan complete (orphans=0, applied=0, escalated=0)\n")
	if outcome < 0 || closeAt < 0 {
		t.Fatalf("command log is missing the outcome write (at %d) or the reasoned close (at %d)\nlog:\n%s\noutput:\n%s", outcome, closeAt, log, out)
	}
	if outcome > closeAt {
		t.Fatalf("the close ran before the outcome write\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, staleDBOutcomeWithdrawal("bead-1")) {
		t.Fatalf("a close that succeeded took its outcome back\nlog:\n%s\noutput:\n%s", log, out)
	}
}

type staleDBFailureCase struct {
	scanJSON     string
	scanExit     string
	applyJSON    string
	applyExit    string
	failContains string
	// failExit is the status of the injected failure; empty means 70.
	failExit     string
	wantNote     string
	wantLog      string
	forbidLog    string
	forbidOutput string
	// beadID is the work bead the run claims; empty means bead-1.
	beadID string
	// env is appended to the run's environment.
	env []string
}

func TestStaleDBFormulaFailurePathsDrainAck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	applyScanJSON := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	for _, tc := range []struct {
		name string
		spec staleDBFailureCase
		// wantDedup is the dedup key of the escalation mail the path sends.
		wantDedup string
	}{
		{
			name: "dry run command failure",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`,
				scanExit: "42",
				wantNote: "gc.dolt_cleanup.scan_report=scan (dry-run failed)",
			},
		},
		{
			name: "invalid scan JSON",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"wrong"}`,
				wantNote: "gc.dolt_cleanup.scan_report=scan (invalid JSON)",
			},
		},
		{
			name: "apply command failure",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","error":"drop failed"}]}`,
				applyExit: "42",
				wantNote:  "gc.dolt_cleanup.apply_report=apply (--force, failed)",
				wantLog:   "gc dolt cleanup --server-down-ok",
				forbidLog: "agent with --server-down-ok",
			},
			wantDedup: "mol-dog-stale-db:bead-1:apply-refused",
		},
		{
			name: "invalid apply JSON",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"wrong"}`,
				wantNote:  "gc.dolt_cleanup.apply_report=apply (--force, invalid JSON)",
			},
		},
		{
			name: "apply misses dry-run reclaimable bytes",
			spec: staleDBFailureCase{
				scanJSON:     `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
				applyJSON:    `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"ok":true,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote:     "gc.dolt_cleanup.apply_report=apply (--force)",
				wantLog:      "apply missed 4096 reclaimable bytes",
				forbidLog:    "apply reported",
				forbidOutput: "apply reported",
			},
		},
		{
			name: "invalid identifier skipped in scan",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[{"name":"testdb_bad;drop","reason":"invalid-identifier"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote: "gc.dolt_cleanup.scan_report=scan (dry-run)",
				wantLog:  "invalid stale database identifier",
			},
			wantDedup: "mol-dog-stale-db:bead-1:invalid-identifiers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, tc.spec)
			if err == nil {
				t.Fatalf("rendered script exited successfully; want failure path to keep work open\nlog:\n%s\noutput:\n%s", log, out)
			}
			if tc.wantDedup != "" && !strings.Contains(log, "--dedup "+tc.wantDedup+"\n") {
				t.Fatalf("failure path did not escalate with dedup key %q\nlog:\n%s\noutput:\n%s", tc.wantDedup, log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("failure path did not drain-ack before exit\nlog:\n%s\noutput:\n%s", log, out)
			}
			if tc.spec.wantNote != "" && !strings.Contains(log, tc.spec.wantNote) {
				t.Fatalf("failure path did not append expected note %q\nlog:\n%s\noutput:\n%s", tc.spec.wantNote, log, out)
			}
			if tc.spec.wantLog != "" && !strings.Contains(log, tc.spec.wantLog) {
				t.Fatalf("failure path log missing %q\nlog:\n%s\noutput:\n%s", tc.spec.wantLog, log, out)
			}
			if tc.spec.forbidLog != "" && strings.Contains(log, tc.spec.forbidLog) {
				t.Fatalf("failure path log still contains unsupported copy %q\nlog:\n%s\noutput:\n%s", tc.spec.forbidLog, log, out)
			}
			if tc.spec.forbidOutput != "" && strings.Contains(string(out), tc.spec.forbidOutput) {
				t.Fatalf("failure path output still contains unsupported copy %q\nlog:\n%s\noutput:\n%s", tc.spec.forbidOutput, log, out)
			}
			if strings.Contains(log, staleDBCloseWrite("bead-1")) || strings.Contains(log, staleDBOutcomeWrite("bead-1")) {
				t.Fatalf("failure path recorded a pass outcome or closed bead despite non-zero outcome\nlog:\n%s\noutput:\n%s", log, out)
			}
		})
	}
}

func TestStaleDBFormulaSuccessPathFailuresDrainAck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	cleanScan := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	for _, tc := range []struct {
		name        string
		fail        string
		wantFailure bool
		forbid      string
		// then is a write that must follow the failed one.
		then string
	}{
		{
			name: "scan event failure",
			fail: "gc event emit mol-dog-stale-db.scan",
		},
		{
			name: "scan note failure",
			fail: staleDBReportWrite,
		},
		{
			name:        "outcome write failure",
			fail:        staleDBOutcomeWrite("bead-1"),
			wantFailure: true,
			forbid:      staleDBCloseWrite("bead-1"),
		},
		{
			name:        "close failure",
			fail:        staleDBCloseWrite("bead-1"),
			wantFailure: true,
			then:        staleDBOutcomeWithdrawal("bead-1"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON:     cleanScan,
				failContains: tc.fail,
			})
			if tc.forbid != "" && strings.Contains(log, tc.forbid) {
				t.Fatalf("%q ran after the failed %q\nlog:\n%s\noutput:\n%s", tc.forbid, tc.fail, log, out)
			}
			if tc.wantFailure && err == nil {
				t.Fatalf("rendered script exited successfully; want %q failure to preserve non-zero status\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !tc.wantFailure && err != nil {
				t.Fatalf("rendered script failed; want %q to be non-fatal\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("%q path did not drain-ack\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !strings.Contains(log, tc.fail) {
				t.Fatalf("command log missing injected failure %q\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if tc.then != "" && strings.Index(log, tc.then) < strings.Index(log, tc.fail) {
				t.Fatalf("%q did not follow the failed %q\nlog:\n%s\noutput:\n%s", tc.then, tc.fail, log, out)
			}
			if !tc.wantFailure && !strings.Contains(log, staleDBCloseWrite("bead-1")) {
				t.Fatalf("%q path did not close work after nonessential failure\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
		})
	}
}

// staleDBCleanScan is a dry-run report with nothing to clean.
const staleDBCleanScan = `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`

// staleDBHungStoreDeadline is how long a hung-store run may take before the
// test kills it: well past a 2s bound on every call the step makes, and well
// short of the fakes' 120s hang.
const staleDBHungStoreDeadline = 30 * time.Second

// staleDBHungStoreCase is one rendered-script run against a fake gc whose
// matching command blocks, so the bound on that call can be observed.
type staleDBHungStoreCase struct {
	scanJSON string
	// beadID is the work bead the run claims; empty means bead-1.
	beadID string
	// hang is a substring of the logged gc command that blocks for 120s;
	// empty blocks nothing.
	hang string
	// path follows the fake gc's directory on PATH; empty means the host PATH.
	path string
	// bins are extra executables written next to the fake gc, by name.
	bins map[string]string
	// env is appended to the run's environment.
	env []string
}

// staleDBHungStoreRun is what a hung-store run left behind.
type staleDBHungStoreRun struct {
	log      string
	out      string
	timeouts string
	err      error
	timedOut bool
}

func (r staleDBHungStoreRun) String() string {
	return fmt.Sprintf("exit: %v\nlog:\n%s\noutput:\n%s\ntimeout log:\n%s", r.err, r.log, r.out, r.timeouts)
}

// staleDBRequireHungStoreTools skips a test that needs the rendered script to
// bound its store calls with a real timeout binary.
func staleDBRequireHungStoreTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		if _, gerr := exec.LookPath("gtimeout"); gerr != nil {
			t.Skipf("neither timeout nor gtimeout found: %v", err)
		}
	}
}

// runStaleDBFormulaHungStoreCase runs the rendered step in its own process
// group, kills the group if it is still running at staleDBHungStoreDeadline,
// and returns the command log, the output and the exit.
func runStaleDBFormulaHungStoreCase(t *testing.T, tc staleDBHungStoreCase) staleDBHungStoreRun {
	t.Helper()
	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	logPath := filepath.Join(dir, "commands.log")
	timeoutLogPath := filepath.Join(dir, "timeouts.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, tc.scanJSON)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
rendered="gc $*"
echo "$rendered" >> "$GC_TEST_LOG"
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    if [ -n "${GC_TEST_HANG_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_HANG_CONTAINS"* ]]; then
      sleep 120
    fi
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	for name, body := range tc.bins {
		writeTestFile(t, filepath.Join(binDir, name), body, 0o755)
	}
	if tc.beadID == "" {
		tc.beadID = "bead-1"
	}
	path := tc.path
	if path == "" {
		path = os.Getenv("PATH")
	}
	outFile, err := os.Create(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cmd := exec.Command("bash", "-s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(script)
	// A file, not a pipe: Wait returns when bash exits, not when the last
	// process that inherited the output closes it.
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	cmd.Env = append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_TIMEOUT_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON", "GC_TEST_HANG_CONTAINS"),
		"GC_BEAD_ID="+tc.beadID,
		"PATH="+binDir+string(os.PathListSeparator)+path,
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_TIMEOUT_LOG="+timeoutLogPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
		"GC_TEST_HANG_CONTAINS="+tc.hang,
	)
	cmd.Env = append(cmd.Env, tc.env...)
	if err := cmd.Start(); err != nil {
		_ = outFile.Close()
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var run staleDBHungStoreRun
	select {
	case run.err = <-done:
	case <-time.After(staleDBHungStoreDeadline):
		run.timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		run.err = <-done
	}
	// Reap anything the step left behind in its group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err := outFile.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	run.log = readStaleDBTestFile(t, logPath)
	run.out = readStaleDBTestFile(t, filepath.Join(dir, "output"))
	run.timeouts = readStaleDBTestFile(t, timeoutLogPath)
	return run
}

// readStaleDBTestFile returns a file's contents, or "" when it was never written.
func readStaleDBTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

// requireStaleDBLogInOrder fails unless the command log holds every want, each
// after the one before it.
func requireStaleDBLogInOrder(t *testing.T, run staleDBHungStoreRun, wants ...string) {
	t.Helper()
	at := 0
	for _, want := range wants {
		i := strings.Index(run.log[at:], want)
		if i < 0 {
			t.Fatalf("command log missing %q after offset %d\n%s", want, at, run)
		}
		at += i + len(want)
	}
}

// TestStaleDBFormulaHungReportIsRecordedAsOmitted: a scan-report write that
// hangs is bounded, the step still writes the outcome, closes and drain-acks,
// and the close reason and an escalate event say the report was omitted.
func TestStaleDBFormulaHungReportIsRecordedAsOmitted(t *testing.T) {
	staleDBRequireHungStoreTools(t)

	run := runStaleDBFormulaHungStoreCase(t, staleDBHungStoreCase{
		scanJSON: staleDBCleanScan,
		hang:     staleDBReportWrite,
		env:      []string{"DOG_BD_TIMEOUT=2"},
	})
	if run.timedOut {
		t.Fatalf("step still blocked on the hung report write after %s\n%s", staleDBHungStoreDeadline, run)
	}
	if run.err != nil {
		t.Fatalf("step failed; a skipped report must not fail the scan\n%s", run)
	}
	requireStaleDBLogInOrder(t, run,
		staleDBReportWrite,
		staleDBOutcomeWrite("bead-1")+"\n",
		"gc bd close bead-1 --reason Stale DB scan complete (orphans=0, applied=0, escalated=0, reports_omitted=scan)\n",
		"gc runtime drain-ack\n",
	)
	if want := "gc event emit mol-dog-stale-db.escalate --message scan (dry-run) report was not recorded on bead-1"; !strings.Contains(run.log, want) {
		t.Fatalf("command log missing %q\n%s", want, run)
	}
}

// TestStaleDBFormulaFailedReportsAreRecordedAsOmitted: a report write that
// fails fast is recorded as omitted the same way, one stage per skipped
// report.
func TestStaleDBFormulaFailedReportsAreRecordedAsOmitted(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	for _, tc := range []struct {
		name      string
		bead      string
		scanJSON  string
		wantClose string
	}{
		{"the scan report", "bead-1", staleDBCleanScan, "(orphans=0, applied=0, escalated=0, reports_omitted=scan)\n"},
		{"the scan and apply reports", "wisp-applied", `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`, "(orphans=1, applied=1, escalated=0, reports_omitted=scan+apply)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON:     tc.scanJSON,
				beadID:       tc.bead,
				failContains: "gc bd update " + tc.bead + " --set-metadata gc.dolt_cleanup.",
			})
			if err != nil {
				t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
			}
			if !strings.Contains(log, staleDBOutcomeWrite(tc.bead)+"\n") {
				t.Fatalf("a skipped report must not skip the outcome write\nlog:\n%s\noutput:\n%s", log, out)
			}
			if want := staleDBCloseWrite(tc.bead) + " " + tc.wantClose; !strings.Contains(log, want) {
				t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("step never drain-acked\nlog:\n%s\noutput:\n%s", log, out)
			}
		})
	}
}

// TestStaleDBFormulaHungOutcomeOrCloseFailsOpen: when the outcome write or the
// close hangs, the step bounds it, withdraws the outcome that may have landed,
// escalates naming the call, drain-acks and fails open. A hung outcome write
// is never followed by the close.
func TestStaleDBFormulaHungOutcomeOrCloseFailsOpen(t *testing.T) {
	staleDBRequireHungStoreTools(t)

	for _, tc := range []struct {
		name      string
		hang      string
		wantEvent string
	}{
		{"outcome write", staleDBOutcomeWrite("bead-1"), "gc event emit mol-dog-stale-db.escalate --message outcome write failed or overran 2s; leaving work bead open\n"},
		// A close that overran may have landed, so its event does not say the
		// work bead was left open.
		{"close", staleDBCloseWrite("bead-1"), "gc event emit mol-dog-stale-db.escalate --message gc bd close failed or overran 2s; the close may have landed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runStaleDBFormulaHungStoreCase(t, staleDBHungStoreCase{
				scanJSON: staleDBCleanScan,
				hang:     tc.hang,
				env:      []string{"DOG_BD_TIMEOUT=2"},
			})
			if run.timedOut {
				t.Fatalf("step still blocked on the hung %s after %s\n%s", tc.name, staleDBHungStoreDeadline, run)
			}
			if run.err == nil {
				t.Fatalf("step exited 0; a hung %s must leave the work bead open\n%s", tc.name, run)
			}
			requireStaleDBLogInOrder(t, run,
				tc.hang,
				staleDBOutcomeWithdrawal("bead-1")+"\n",
				tc.wantEvent,
				"gc runtime drain-ack\n",
			)
			if tc.hang != staleDBCloseWrite("bead-1") && strings.Contains(run.log, staleDBCloseWrite("bead-1")) {
				t.Fatalf("the close ran after the hung %s\n%s", tc.name, run)
			}
		})
	}
}

// staleDBHangingEscalateHook is an escalate.sh hook that logs its call, echoes
// its arguments, incident flags included, to stderr, and never answers.
const staleDBHangingEscalateHook = `#!/usr/bin/env bash
echo "hook $*" >> "$GC_TEST_LOG"
echo "hook: args: $*" >&2
sleep 120
`

// staleDBEchoingEscalateHook is an escalate.sh hook that logs its call, echoes
// its arguments, incident flags included, to stderr, and returns.
const staleDBEchoingEscalateHook = `#!/usr/bin/env bash
echo "hook $*" >> "$GC_TEST_LOG"
echo "hook: args: $*" >&2
`

// staleDBKillingGtimeout is a gtimeout that reports every escalate.sh hook it
// runs as killed by the bound, exit 137 as timeout exits once --kill-after
// fires, and runs every other command unbounded.
const staleDBKillingGtimeout = `#!/usr/bin/env bash
shift 2
case "$1" in
  */escalate.sh) "$@"; exit 137 ;;
esac
exec "$@"
`

// TestStaleDBFormulaHungNotificationsAreBounded: an escalation or maintenance
// notice that hangs is bounded, and the step still closes its work bead and
// drain-acks. A hook the bound killed (exit 124, or 137 once killed) is not
// read as refusing the incident flags, even when its stderr names them, so the
// escalation is not sent to it again.
func TestStaleDBFormulaHungNotificationsAreBounded(t *testing.T) {
	staleDBRequireHungStoreTools(t)

	for _, tc := range []struct {
		name     string
		scanJSON string
		hang     string
		hook     string
		bins     map[string]string
		env      []string
	}{
		{name: "escalation mail", scanJSON: staleDBOverThresholdScan, hang: "gc mail send ", env: []string{"GC_SYSTEM_PACKS_DIR=" + t.TempDir()}},
		{name: "escalate hook", scanJSON: staleDBOverThresholdScan, hook: staleDBHangingEscalateHook},
		{name: "escalate hook killed", scanJSON: staleDBOverThresholdScan, hook: staleDBEchoingEscalateHook, bins: map[string]string{"gtimeout": staleDBKillingGtimeout}},
		{name: "maintenance nudge", scanJSON: staleDBCleanScan, hang: "gc session nudge ", env: []string{"GC_MAINTENANCE_DONE_TARGET=ops"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"DOG_BD_TIMEOUT=2"}, tc.env...)
			if tc.hook != "" {
				hook := filepath.Join(t.TempDir(), "escalate.sh")
				writeTestFile(t, hook, tc.hook, 0o755)
				env = append(env, "GC_ESCALATE_SCRIPT="+hook)
			}
			run := runStaleDBFormulaHungStoreCase(t, staleDBHungStoreCase{scanJSON: tc.scanJSON, hang: tc.hang, bins: tc.bins, env: env})
			if run.timedOut {
				t.Fatalf("step still blocked on the hung %s after %s\n%s", tc.name, staleDBHungStoreDeadline, run)
			}
			if run.err != nil {
				t.Fatalf("step failed; a hung %s must not fail the scan\n%s", tc.name, run)
			}
			requireStaleDBLogInOrder(t, run, staleDBCloseWrite("bead-1"), "gc runtime drain-ack\n")
			if tc.hook != "" {
				if got := strings.Count(run.log, "hook --subject "); got != 1 {
					t.Fatalf("escalate hook called %d times, want 1: a bound kill is not a refusal of the incident flags\n%s", got, run)
				}
				if strings.Contains(run.out, "predates incident keys") {
					t.Fatalf("a hook the bound killed was read as refusing the incident flags\n%s", run)
				}
			}
		})
	}
}

// staleDBUnboundedWarning is the line the step prints when it has no timeout
// binary to bound its store calls with.
const staleDBUnboundedWarning = "mol-dog-stale-db: no timeout or gtimeout; bead-store calls run unbounded"

// TestStaleDBFormulaRunsUnboundedWithoutTimeout: on a host with neither
// timeout nor gtimeout the step warns once and calls gc directly, as it did
// before the bound, rather than failing with command-not-found and skipping
// the close.
func TestStaleDBFormulaRunsUnboundedWithoutTimeout(t *testing.T) {
	tools := t.TempDir()
	for _, tool := range []string{"bash", "cat", "date", "jq", "mktemp", "rm"} {
		target, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
		if err := os.Symlink(target, filepath.Join(tools, tool)); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}

	run := runStaleDBFormulaHungStoreCase(t, staleDBHungStoreCase{scanJSON: staleDBCleanScan, beadID: "wisp-unbounded", path: tools})
	if run.timedOut {
		t.Fatalf("step still running after %s\n%s", staleDBHungStoreDeadline, run)
	}
	if run.err != nil {
		t.Fatalf("step failed without a timeout binary; it must run its calls unbounded\n%s", run)
	}
	if got := strings.Count(run.out, staleDBUnboundedWarning); got != 1 {
		t.Fatalf("%d unbounded warnings, want 1\n%s", got, run)
	}
	requireStaleDBLogInOrder(t, run,
		"gc bd update wisp-unbounded --set-metadata gc.dolt_cleanup.scan_report=",
		staleDBOutcomeWrite("wisp-unbounded")+"\n",
		"gc bd close wisp-unbounded --reason Stale DB scan complete (orphans=0, applied=0, escalated=0)\n",
		"gc runtime drain-ack\n",
	)
}

// staleDBLoggingGtimeout is a gtimeout that logs its bound and the first three
// words of the command it runs, then runs the command unbounded.
const staleDBLoggingGtimeout = `#!/usr/bin/env bash
echo "$2 $3 $4 $5" >> "$GC_TEST_TIMEOUT_LOG"
shift 2
exec "$@"
`

// staleDBStoreCalls lists the first three words of each logged gc call that
// touches the bead store, in the order the step made them.
func staleDBStoreCalls(log string) []string {
	var calls []string
	for _, line := range strings.Split(log, "\n") {
		for _, prefix := range []string{"gc bd ", "gc mail send ", "gc session nudge "} {
			if strings.HasPrefix(line, prefix) {
				calls = append(calls, strings.Join(strings.Fields(line)[:3], " "))
			}
		}
	}
	return calls
}

// TestStaleDBFormulaStoreCallTimeoutKnob: DOG_BD_TIMEOUT sets the bound on
// every store call (report and outcome writes, close, escalation mail and
// maintenance nudges), and a value that is not a positive whole number of
// seconds falls back to 60. Event emits are not bounded.
func TestStaleDBFormulaStoreCallTimeoutKnob(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	type knob struct {
		name string
		env  []string
		want string
	}
	knobs := []knob{
		{"7", []string{"DOG_BD_TIMEOUT=7"}, "7"},
		{"unset", nil, "60"},
	}
	for _, v := range []string{"", "0", "00", "-5", "abc"} {
		knobs = append(knobs, knob{fmt.Sprintf("%q", v), []string{"DOG_BD_TIMEOUT=" + v}, "60"})
	}
	for _, tc := range knobs {
		t.Run(tc.name, func(t *testing.T) {
			run := runStaleDBFormulaHungStoreCase(t, staleDBHungStoreCase{
				scanJSON: staleDBOverThresholdScan,
				bins:     map[string]string{"gtimeout": staleDBLoggingGtimeout},
				env: append([]string{
					"GC_SYSTEM_PACKS_DIR=" + t.TempDir(),
					"GC_MAINTENANCE_DONE_TARGET=ops",
				}, tc.env...),
			})
			if run.timedOut || run.err != nil {
				t.Fatalf("step did not finish cleanly\n%s", run)
			}
			calls := staleDBStoreCalls(run.log)
			// The scan report, the escalation mail, two nudges, the outcome and the close.
			if len(calls) != 6 {
				t.Fatalf("%d store calls, want 6: %q\n%s", len(calls), calls, run)
			}
			var bounded []string
			for _, line := range strings.Split(strings.TrimSuffix(run.timeouts, "\n"), "\n") {
				bound, call, _ := strings.Cut(line, " ")
				if bound != tc.want {
					t.Errorf("%q bounded at %q, want %q", call, bound, tc.want)
				}
				bounded = append(bounded, call)
			}
			if strings.Join(bounded, "\n") != strings.Join(calls, "\n") {
				t.Fatalf("bounded calls:\n%s\nwant every store call:\n%s\n%s", strings.Join(bounded, "\n"), strings.Join(calls, "\n"), run)
			}
		})
	}
}

func runStaleDBFormulaFailureCase(t *testing.T, tc staleDBFailureCase) (string, []byte, error) {
	t.Helper()
	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	if tc.applyJSON == "" {
		tc.applyJSON = `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	}
	if tc.beadID == "" {
		tc.beadID = "bead-1"
	}
	writeTestFile(t, scanPath, tc.scanJSON)
	writeTestFile(t, applyPath, tc.applyJSON)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
maybe_fail() {
  local rendered="$1"
  if [ -n "${GC_TEST_FAIL_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_FAIL_CONTAINS"* ]]; then
    exit "${GC_TEST_FAIL_EXIT:-70}"
  fi
}
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        if [ -n "${GC_TEST_APPLY_EXIT:-}" ]; then exit "$GC_TEST_APPLY_EXIT"; fi
        ;;
      *)
        cat "$GC_TEST_SCAN_JSON"
        if [ -n "${GC_TEST_SCAN_EXIT:-}" ]; then exit "$GC_TEST_SCAN_EXIT"; fi
        ;;
    esac
    ;;
  "bd update"|"bd close"|"event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    rendered="gc $*"
    echo "$rendered" >> "$GC_TEST_LOG"
    maybe_fail "$rendered"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
maybe_fail() {
  local rendered="$1"
  if [ -n "${GC_TEST_FAIL_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_FAIL_CONTAINS"* ]]; then
    exit 70
  fi
}
case "${1:-}" in
  update|close)
    rendered="bd $*"
    echo "$rendered" >> "$GC_TEST_LOG"
    maybe_fail "$rendered"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_SCAN_EXIT", "GC_TEST_APPLY_JSON", "GC_TEST_APPLY_EXIT", "GC_TEST_FAIL_CONTAINS", "GC_TEST_FAIL_EXIT"),
		"GC_BEAD_ID="+tc.beadID,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_SCAN_EXIT="+tc.scanExit,
		"GC_TEST_APPLY_JSON="+applyPath,
		"GC_TEST_APPLY_EXIT="+tc.applyExit,
		"GC_TEST_FAIL_CONTAINS="+tc.failContains,
		"GC_TEST_FAIL_EXIT="+tc.failExit,
	)
	cmd.Env = append(cmd.Env, tc.env...)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	return string(logData), out, err
}

func renderStaleDBFormulaShell(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	f, err := formula.NewParser().ParseFile(filepath.Join(root, "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(f.Steps))
	}

	vars := make(map[string]string, len(f.Vars))
	for name, def := range f.Vars {
		if def.Default != nil {
			vars[name] = *def.Default
		}
	}
	rendered := formula.Substitute(f.Steps[0].Description, vars)
	if residual := formula.CheckResidualVars(rendered); len(residual) != 0 {
		t.Fatalf("rendered formula has residual vars: %v", residual)
	}
	return extractFirstBashFence(t, rendered)
}

func writeTestFile(t *testing.T, path string, data string, perm ...os.FileMode) {
	t.Helper()
	mode := os.FileMode(0o644)
	if len(perm) > 0 {
		mode = perm[0]
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func extractFirstBashFence(t *testing.T, markdown string) string {
	t.Helper()
	start := strings.Index(markdown, "```bash\n")
	if start < 0 {
		t.Fatal("missing bash code fence")
	}
	start += len("```bash\n")
	end := strings.LastIndex(markdown, "\n```")
	if end < 0 {
		t.Fatal("missing closing code fence")
	}
	if end <= start {
		t.Fatal("closing code fence appears before bash body")
	}
	body := markdown[start:end]
	// Readers of the rendered step (the dog agent, or a regex that does not
	// anchor fences to line starts) end the fence at its first ```, not its
	// last. TOML basic-string escapes such as \n can put one inside the script,
	// which then runs truncated (gascity#7967).
	if i := strings.Index(body, "```"); i >= 0 {
		lineStart := strings.LastIndex(body[:i], "\n") + 1
		lineEnd := strings.IndexByte(body[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(body)
		} else {
			lineEnd += i
		}
		t.Fatalf("bash fence body contains ``` on line %q: readers end the fence there", body[lineStart:lineEnd])
	}
	return body
}

func TestStaleDBOrderUsesParsedFieldsOnly(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "orders", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "\n[vars]\n") {
		t.Fatal("order contains [vars], but order parsing ignores that table")
	}

	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("orders.Parse: %v", err)
	}
	if err := orders.Validate(order); err != nil {
		t.Fatalf("orders.Validate: %v", err)
	}
	if order.Trigger != "cron" {
		t.Fatalf("Trigger = %q, want cron", order.Trigger)
	}
	if order.Schedule != "0 */4 * * *" {
		t.Fatalf("Schedule = %q, want 0 */4 * * *", order.Schedule)
	}
}
