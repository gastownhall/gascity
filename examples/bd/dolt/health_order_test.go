package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoltHealthOrderIsDiagnosticOnly(t *testing.T) {
	root := repoRoot(t)
	orderPath := filepath.Join(root, "orders", "dolt-health.toml")
	data, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("read dolt-health order: %v", err)
	}

	text := string(data)
	if !strings.Contains(text, `exec = "gc dolt health --json | gc dolt health-check"`) {
		t.Fatalf("dolt-health order should run bounded health JSON, got:\n%s", text)
	}
	for _, forbidden := range []string{"gc dolt start", "gc dolt status"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("dolt-health order must not call %q directly:\n%s", forbidden, text)
		}
	}
}

func TestDoltHealthCheckFailsUnreachableReportWithUsefulMessage(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "commands", "health-check", "run.sh")
	input := `{
  "server": {
    "running": true,
    "reachable": false,
    "pid": 123,
    "port": 3311,
    "latency_ms": 0
  }
}`

	cmd := exec.Command("sh", script)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("health-check unexpectedly succeeded:\n%s", out)
	}
	for _, want := range []string{"Dolt server unreachable", "running=true", "pid=123", "port=3311"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("health-check output missing %q:\n%s", want, out)
		}
	}
}

func TestDoltHealthCheckPassesReachableReport(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "commands", "health-check", "run.sh")
	input := `{
  "server": {
    "running": true,
    "reachable": true,
    "pid": 123,
    "port": 3311,
    "latency_ms": 12
  }
}`

	cmd := exec.Command("sh", script)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health-check failed: %v\n%s", err, out)
	}
}
