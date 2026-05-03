package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withTestStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = old
		_ = r.Close()
	}()
	fn()
}

func writeFakeBdBridgeScript(t *testing.T, binDir, envFile, argsFile string) {
	t.Helper()
	path := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
set -eu
printf 'BEADS_DIR=%s
GC_DOLT_HOST=%s
GC_DOLT_PORT=%s
GC_DOLT_USER=%s
GC_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_HOST=%s
BEADS_DOLT_SERVER_PORT=%s
BEADS_DOLT_SERVER_USER=%s
BEADS_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_DATABASE=%s
BEADS_CREDENTIALS_FILE=%s
GC_BEADS=%s
GC_BEADS_PREFIX=%s
BD_EXPORT_AUTO=%s
' \
  "${BEADS_DIR:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_DOLT_USER:-}" "${GC_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_DATABASE:-}" "${BEADS_CREDENTIALS_FILE:-}" "${GC_BEADS:-}" "${GC_BEADS_PREFIX:-}" \
  "${BD_EXPORT_AUTO:-}" > "` + envFile + `"
printf '%s
' "$*" > "` + argsFile + `"
case "${1:-}" in
  create)
    cat <<'JSON'
{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}
JSON
    ;;
  list)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"message","assignee":"mayor","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  update)
    exit 0
    ;;
  dep)
    if [ "${2:-}" = "list" ]; then
      cat <<'JSON'
[{"id":"BD-2","dependency_type":"blocks"}]
JSON
      exit 0
    fi
    exit 2
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBdStoreBridgeCreateCmdProjectsCanonicalEnvAndClearsAmbientAuthority(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong-db")
	t.Setenv("BEADS_CREDENTIALS_FILE", "/tmp/stale-creds")
	t.Setenv("GC_BEADS", "ambient-bd")
	t.Setenv("GC_BEADS_PREFIX", "ambient-prefix")
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	var stdout, stderr bytes.Buffer

	withTestStdin(t, `{"title":"captured","type":"task","labels":["triage"]}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"--user", "root",
			"create",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})

	var bead map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &bead); err != nil {
		t.Fatalf("stdout JSON: %v\n%s", err, stdout.String())
	}
	if bead["id"] != "BD-1" || bead["type"] != "task" {
		t.Fatalf("unexpected bead payload: %#v", bead)
	}

	envText, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ReadFile(env): %v", err)
	}
	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["BEADS_DIR"]; got != filepath.Join(scopeDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(scopeDir, ".beads"))
	}
	if got := envMap["GC_DOLT_HOST"]; got != "db.example.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want db.example.internal", got)
	}
	if got := envMap["GC_DOLT_PORT"]; got != "3317" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3317", got)
	}
	if got := envMap["BEADS_DOLT_SERVER_DATABASE"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BEADS_CREDENTIALS_FILE"]; got != "" {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS"]; got != "" {
		t.Fatalf("GC_BEADS = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS_PREFIX"]; got != "" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false to suppress bridge auto-export\n%s", got, string(envText))
	}

	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"create", "--json", "captured", "-t", "task", "--labels", "triage"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("bd args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeDepListCmdReturnsJSON(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"dep-list",
		"BD-1",
		"up",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != `[{"issue_id":"BD-2","depends_on_id":"BD-1","type":"blocks"}]` {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if !strings.Contains(string(argsText), "dep list BD-1 --json --direction=up") {
		t.Fatalf("dep-list args = %q", string(argsText))
	}
}

func TestBdStoreBridgeUpdateCommandPassesType(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	withTestStdin(t, `{"type":"bug"}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"update",
			"BD-1",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"update", "--json", "BD-1", "--type", "bug"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("update args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeListCommandForwardsFilters(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"list",
		"--status=open",
		"--assignee=mayor",
		"--type=message",
		"--limit=7",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); !strings.Contains(got, `"id":"BD-1"`) {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"list", "--json", "--status=open", "--assignee=mayor", "--type=message", "--include-infra", "--include-gates", "--limit", "7"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("list args missing %q: %s", want, string(argsText))
		}
	}
}
