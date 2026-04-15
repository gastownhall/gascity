package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAccountDefault_EmptyRegistry verifies that "gc account default <handle>"
// with no accounts registered returns exit code 1 and prompts the operator to
// run "gc account add".
//
// PRD Scenario #41: When gc account default is run with no accounts, the
// command exits with a non-zero status and prompts the operator.
// Audit GAP-8a (F-2/CC-3).
func TestAccountDefault_EmptyRegistry(t *testing.T) {
	// Set up a valid city directory with an empty accounts.json.
	cityDir := t.TempDir()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("creating .gc dir: %v", err)
	}

	// Point resolveCity at our temp city.
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	exitCode := doAccountDefault("work1", &stdout, &stderr)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "no accounts registered") {
		t.Errorf("stderr should contain %q, got: %s", "no accounts registered", errOut)
	}
	if !strings.Contains(errOut, "gc account add") {
		t.Errorf("stderr should contain %q, got: %s", "gc account add", errOut)
	}
}
