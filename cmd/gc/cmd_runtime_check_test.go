package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRPPScript creates an executable RPP shell script and returns its path.
func writeRPPScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// conformantRPPScript implements the required lifecycle ops against a
// state directory; everything else exits 2.
func conformantRPPScript(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeRPPScript(t, fmt.Sprintf(`
state=%q
op="$1"
name="$2"
case "$op" in
  start)      cat > /dev/null; touch "$state/$name.running" ;;
  stop)       rm -f "$state/$name.running" ;;
  is-running) if [ -f "$state/$name.running" ]; then echo true; else echo false; fi ;;
  *) exit 2 ;;
esac
`, state))
}

func TestRuntimeCheckCmd_ConformantExecutableExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{conformantRPPScript(t)})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "PASS protocol handshake") {
		t.Errorf("output missing handshake pass line:\n%s", out)
	}
	if !strings.Contains(out, "0 failed") {
		t.Errorf("output should report zero failures:\n%s", out)
	}
}

func TestRuntimeCheckCmd_FailingExecutableExitsNonZero(t *testing.T) {
	// is-running lies after stop: a required lifecycle check fails.
	state := t.TempDir()
	script := writeRPPScript(t, fmt.Sprintf(`
state=%q
op="$1"
name="$2"
case "$op" in
  start)      cat > /dev/null; touch "$state/$name.running" ;;
  stop)       ;;
  is-running) echo true ;;
  *) exit 2 ;;
esac
`, state))

	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{script})

	err := cmd.Execute()
	if !errors.Is(err, errExit) {
		t.Fatalf("Execute = %v, want errExit\nstdout:\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "FAIL lifecycle: is-running after stop") {
		t.Errorf("output missing failing check:\n%s", stdout.String())
	}
}

func TestRuntimeCheckCmd_MissingExecutableErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "does-not-exist")})

	err := cmd.Execute()
	if !errors.Is(err, errExit) {
		t.Fatalf("Execute = %v, want errExit", err)
	}
	if !strings.Contains(stderr.String(), "resolving executable") {
		t.Errorf("stderr should explain the resolution failure, got:\n%s", stderr.String())
	}
}

func TestRuntimeCheckCmd_FlagsReachTheChecker(t *testing.T) {
	state := t.TempDir()
	script := writeRPPScript(t, fmt.Sprintf(`
state=%q
op="$1"
name="$2"
case "$op" in
  start)      cat > "$state/start-config.json"; echo "$name" > "$state/session-name"; touch "$state/$name.running" ;;
  stop)       rm -f "$state/$name.running" ;;
  is-running) if [ -f "$state/$name.running" ]; then echo true; else echo false; fi ;;
  *) exit 2 ;;
esac
`, state))

	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{script, "--command", "custom-cmd-xyz", "--session-name", "custom-check-sess"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	cfg, err := os.ReadFile(filepath.Join(state, "start-config.json"))
	if err != nil {
		t.Fatalf("reading captured start config: %v", err)
	}
	if !strings.Contains(string(cfg), `"command":"custom-cmd-xyz"`) {
		t.Errorf("start config %q missing --command value", cfg)
	}
	name, err := os.ReadFile(filepath.Join(state, "session-name"))
	if err != nil {
		t.Fatalf("reading captured session name: %v", err)
	}
	if got := strings.TrimSpace(string(name)); got != "custom-check-sess" {
		t.Errorf("session name %q, want %q from --session-name", got, "custom-check-sess")
	}
}

func TestRuntimeCheckCmd_RegisteredUnderRuntime(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	for _, sub := range cmd.Commands() {
		if sub.Name() == "check" {
			return
		}
	}
	t.Fatal(`"gc runtime" has no "check" subcommand`)
}
