package main

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// writeExecutable writes body to path with the executable bit set.
//
// It lives in an untagged file because callers on both sides of the unix build
// tag need it: what each caller writes decides whether that caller is portable,
// not this helper.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// extractShellFunction returns the source text of a shell function declared in
// script. Reading the script as text is portable; only executing it is not, so
// the static-lint tests that assert on the text keep running everywhere.
func extractShellFunction(t *testing.T, script, name string) string {
	t.Helper()
	// Match the function header and capture lines until the matching
	// closing brace at column 0. The script uses the conventional
	// `name() {` ... `\n}` shape.
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := pattern.FindStringIndex(script)
	if loc == nil {
		t.Fatalf("could not find shell function %q in script", name)
	}
	return script[loc[0]:loc[1]]
}

// runGCBeadsBdCommand runs one program under the given environment and returns
// its stdout, stderr and run error separately. The gc-beads-bd shell tests all
// route through this one command construction rather than building their own,
// so the untagged subprocess census carries a single call site for the family
// instead of one per test file.
func runGCBeadsBdCommand(t *testing.T, env []string, name string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
