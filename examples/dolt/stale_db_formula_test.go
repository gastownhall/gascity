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
		`gc session nudge deacon`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("formula step missing %q", want)
		}
	}
	for _, bad := range []string{
		`/tmp/dolt-cleanup`,
		`gc nudge deacon`,
		`GC_BEAD_ID:-<work-bead>`,
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
	script := extractFirstBashFence(t, rendered)
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
