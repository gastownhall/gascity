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
		`trap 'rm -rf "$TMP_DIR"' EXIT`,
		`gc dolt-cleanup --json --probe > "$SCAN_FILE"`,
		`gc dolt-cleanup --json --probe --force > "$APPLY_FILE"`,
		`jq -r '.dropped.count // 0'`,
		`jq -r '.reaped.targets | length'`,
		`gc event emit mol-dog-stale-db.scan`,
		`gc event emit mol-dog-stale-db.drop`,
		`gc event emit mol-dog-stale-db.purge`,
		`gc event emit mol-dog-stale-db.reap`,
		`gc event emit mol-dog-stale-db.done`,
		`gc event emit mol-dog-stale-db.escalate`,
		`if [ "$APPLIED" -eq 1 ] && [ "$DONE_ERRS" -gt 0 ]; then`,
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
		"gc event emit mol-dog-stale-db.done",
		"gc event emit mol-dog-stale-db.escalate",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "bd close bead-1") {
		t.Fatalf("rendered script closed bead successfully despite apply errors\nlog:\n%s\noutput:\n%s", log, out)
	}
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
