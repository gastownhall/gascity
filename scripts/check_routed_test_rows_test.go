package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const routedMatrixRows = `package main

var rows = []struct{ name string }{
	{name: "api-happy-path"},
	{name: "api-cache-not-live"},
	{name: "api-500-fallback"},
	{name: "api-404-error"},
	{name: "controller-down"},
	{name: "escape-hatch"},
}
`

func synthRoutedRowsRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(repoRoot(t), "scripts", "check-routed-test-rows.sh")
	content, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read routed-rows guard: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "scripts", "check-routed-test-rows.sh"), string(content))
	manifest := "cmd/gc/cmd_matrix_test.go\n"
	if _, ok := files["cmd/gc/cmd_matrix_test.go"]; !ok {
		manifest = ""
	}
	writeTestFile(t, filepath.Join(root, "scripts", "routed-test-rows.manifest"), manifest)
	for name, content := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), content)
	}
	return root
}

func runSyntheticRoutedRowsGuard(t *testing.T, files map[string]string) (string, int) {
	t.Helper()
	root := synthRoutedRowsRepo(t, files)
	return runGuardScript(t, filepath.Join(root, "scripts", "check-routed-test-rows.sh"))
}

// TestCheckRoutedTestRowsPassesOnThisRepo catches markers in focused tests
// being mistaken for an incomplete matrix.
func TestCheckRoutedTestRowsPassesOnThisRepo(t *testing.T) {
	root := repoRoot(t)
	out, code := runGuardScript(t, filepath.Join(root, "scripts", "check-routed-test-rows.sh"))
	if code != 0 {
		t.Fatalf("routed-rows guard failed on this repository (exit %d):\n%s", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("routed-rows guard passed but was not silent:\n%s", out)
	}
}

// TestCheckRoutedTestRowsIgnoresIncidentalMarkers distinguishes matrix row
// declarations from route reasons and comments that happen to use the same
// vocabulary.
func TestCheckRoutedTestRowsIgnoresIncidentalMarkers(t *testing.T) {
	out, code := runSyntheticRoutedRowsGuard(t, map[string]string{
		"cmd/gc/cmd_matrix_test.go": routedMatrixRows,
		"cmd/gc/cmd_other_test.go": `package main

// escape-hatch and api-500-fallback are examples, not row declarations.
const nilReason = "controller-down"
`,
	})
	if code != 0 {
		t.Fatalf("incidental markers reported as a matrix (exit %d):\n%s", code, out)
	}
}

// TestCheckRoutedTestRowsRejectsCommentOnlyManifestRows keeps the manifest
// denominator non-vacuous when every marker occurs only in documentation.
func TestCheckRoutedTestRowsRejectsCommentOnlyManifestRows(t *testing.T) {
	out, code := runSyntheticRoutedRowsGuard(t, map[string]string{
		"cmd/gc/cmd_matrix_test.go": `package main

// api-happy-path
// api-cache-not-live
// api-500-fallback
// api-404-error
// controller-down
// escape-hatch
`,
	})
	if code == 0 {
		t.Fatalf("comment-only matrix passed the guard:\n%s", out)
	}
	if !strings.Contains(out, "MANIFEST FILE UNDER-COVERED") {
		t.Fatalf("guard output does not name the under-covered manifest file:\n%s", out)
	}
}
