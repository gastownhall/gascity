package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestCodexHooksDriftCheckReportsManagedMissingPreCompact(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck([]string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "missing PreCompact") {
		t.Fatalf("message = %q, want missing PreCompact", result.Message)
	}
}

func TestCodexHooksDriftCheckPassesCurrentHooks(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }],
    "PreCompact": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto --hook-format codex \"context cycle\""
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck([]string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok; message=%s", result.Status, result.Message)
	}
}

func TestCodexHooksDriftCheckIgnoresCustomHooks(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "printf custom-codex-hook"
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck([]string{dir})
	result := check.Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok for user-owned hooks; message=%s", result.Status, result.Message)
	}
}

func TestCodexHooksDriftCheckFixUpgradesManagedHooks(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc prime --hook --hook-format codex"
      }]
    }]
  }
}`)

	check := newCodexHooksDriftCheck([]string{dir})
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want ok; message=%s", result.Status, result.Message)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	if !strings.Contains(string(data), "PreCompact") {
		t.Fatalf("fixed hooks missing PreCompact:\n%s", string(data))
	}
}

func writeCodexHooksForDoctorTest(t *testing.T, dir, data string) {
	t.Helper()
	hookDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "hooks.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("write hooks: %v", err)
	}
}
