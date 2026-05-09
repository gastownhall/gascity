package dolt_test

import (
	"os"
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
	if !strings.Contains(text, `exec = "gc dolt health --json"`) {
		t.Fatalf("dolt-health order should run bounded health JSON, got:\n%s", text)
	}
	for _, forbidden := range []string{"gc dolt start", "gc dolt status"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("dolt-health order must not call %q directly:\n%s", forbidden, text)
		}
	}
}
