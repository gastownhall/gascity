package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestStaleDBFormulaRuntimeContract(t *testing.T) {
	root := repoRoot(t)
	f, err := formula.NewParser().ParseFile(filepath.Join(root, "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if f.Version != 1 {
		t.Fatalf("Version = %d, want 1", f.Version)
	}
	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1 so shell state stays inside one formula step", len(f.Steps))
	}

	desc := f.Steps[0].Description
	for _, want := range []string{
		`set -euo pipefail`,
		`WORK_BEAD="${GC_BEAD_ID:?GC_BEAD_ID required`,
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
		`gc session nudge deacon "WARN: $ORPHAN_TOTAL Dolt orphan(s) seen this scan`,
		`gc session nudge deacon "DOG_DONE: stale-db - orphans: ${ORPHAN_TOTAL}, applied: ${APPLIED}, escalated: ${ESCALATED}" || true`,
		`escalated=${ESCALATED}`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("formula step missing %q", want)
		}
	}
	for _, bad := range []string{
		`/tmp/dolt-cleanup`,
		`gc nudge deacon`,
		`GC_BEAD_ID:-<work-bead>`,
		`Dolt orphan(s) detected`,
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
		`WORK_BEAD="${GC_BEAD_ID:?GC_BEAD_ID required`,
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
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
		"bd update bead-1 --append-notes",
		"## apply (--force, refused)",
		"gc event emit mol-dog-stale-db.escalate",
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
	if strings.Contains(log, "bd close bead-1") {
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
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
	if !strings.Contains(log, "## apply (--force, failed)") {
		t.Fatalf("failed apply JSON was not appended to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, `"stage":"drop"`) {
		t.Fatalf("appended apply note missing JSON errors\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "bd close bead-1") {
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON"),
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
	if !strings.Contains(log, "bd update bead-1 --append-notes") {
		t.Fatalf("dry-run failure did not append scan JSON to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "bd close bead-1") {
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
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
		"gc event emit mol-dog-stale-db.done --message 1200 bytes freed; 0 errors",
		"bd close bead-1",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated at dropped.count == max_orphans_for_sql; want apply because threshold is >\nlog:\n%s\noutput:\n%s", log, out)
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
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
		"bd close bead-1",
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
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
		"bd update bead-1 --append-notes",
		"## apply (--force, failed)",
		`"stage":"purge"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "bd close bead-1") {
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
		"bd close bead-1",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("exit-zero max-orphan refusal logged forbidden success path %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
	for _, want := range []string{
		"bd update bead-1 --append-notes",
		"## apply (--force, refused)",
		"apply refused by max-orphan safety guard",
		"gc event emit mol-dog-stale-db.escalate",
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
		scanJSON: `{"schema":"gc.dolt.cleanup.v1","force_blockers":[{"kind":"rig-protection","name":"missing","error":"missing metadata"}],"dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
		wantNote: "## scan (dry-run)",
		wantLog:  "force blocker",
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want dry-run force blockers to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"gc event emit mol-dog-stale-db.escalate",
		"gc mail send mayor",
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
		"bd close bead-1",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("force-blocker path logged forbidden command %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
}

type staleDBFailureCase struct {
	scanJSON     string
	scanExit     string
	applyJSON    string
	applyExit    string
	failContains string
	wantNote     string
	wantLog      string
	forbidLog    string
	forbidOutput string
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
	}{
		{
			name: "dry run command failure",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`,
				scanExit: "42",
				wantNote: "## scan (dry-run failed)",
			},
		},
		{
			name: "invalid scan JSON",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"wrong"}`,
				wantNote: "## scan (invalid JSON)",
			},
		},
		{
			name: "apply command failure",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","error":"drop failed"}]}`,
				applyExit: "42",
				wantNote:  "## apply (--force, failed)",
				wantLog:   "gc dolt cleanup --server-down-ok",
				forbidLog: "agent with --server-down-ok",
			},
		},
		{
			name: "invalid apply JSON",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"wrong"}`,
				wantNote:  "## apply (--force, invalid JSON)",
			},
		},
		{
			name: "apply misses dry-run reclaimable bytes",
			spec: staleDBFailureCase{
				scanJSON:     `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
				applyJSON:    `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"ok":true,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote:     "## apply (--force)",
				wantLog:      "apply missed 4096 reclaimable bytes",
				forbidLog:    "apply reported",
				forbidOutput: "apply reported",
			},
		},
		{
			name: "invalid identifier skipped in scan",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[{"name":"testdb_bad;drop","reason":"invalid-identifier"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote: "## scan (dry-run)",
				wantLog:  "invalid stale database identifier",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, tc.spec)
			if err == nil {
				t.Fatalf("rendered script exited successfully; want failure path to keep work open\nlog:\n%s\noutput:\n%s", log, out)
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
			if strings.Contains(log, "bd close bead-1") {
				t.Fatalf("failure path closed bead despite non-zero outcome\nlog:\n%s\noutput:\n%s", log, out)
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
	}{
		{
			name: "scan event failure",
			fail: "gc event emit mol-dog-stale-db.scan",
		},
		{
			name: "scan note failure",
			fail: "bd update bead-1 --append-notes",
		},
		{
			name:        "close failure",
			fail:        "bd close bead-1",
			wantFailure: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON:     cleanScan,
				failContains: tc.fail,
			})
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
			if !tc.wantFailure && !strings.Contains(log, "bd close bead-1") {
				t.Fatalf("%q path did not close work after nonessential failure\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
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
	writeTestFile(t, scanPath, tc.scanJSON)
	writeTestFile(t, applyPath, tc.applyJSON)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
maybe_fail() {
  local rendered="$1"
  if [ -n "${GC_TEST_FAIL_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_FAIL_CONTAINS"* ]]; then
    exit 70
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
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
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
	cmd.Env = append(filteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_SCAN_EXIT", "GC_TEST_APPLY_JSON", "GC_TEST_APPLY_EXIT", "GC_TEST_FAIL_CONTAINS"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_SCAN_EXIT="+tc.scanExit,
		"GC_TEST_APPLY_JSON="+applyPath,
		"GC_TEST_APPLY_EXIT="+tc.applyExit,
		"GC_TEST_FAIL_CONTAINS="+tc.failContains,
	)
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
	return markdown[start:end]
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
