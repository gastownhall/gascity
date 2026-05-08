package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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

func TestNewCodexHooksDriftCheckCleansDedupesAndSortsDirs(t *testing.T) {
	check := newCodexHooksDriftCheck([]string{" /z/../z ", "", "/a", "/a/."})

	if got, want := strings.Join(check.dirs, ","), "/a,/z"; got != want {
		t.Fatalf("dirs = %q, want %q", got, want)
	}
	if got, want := check.Name(), "codex-hooks-drift"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
}

func TestCodexHookWorkDirsIncludesActiveRigPaths(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "active", Path: "/rig/active"},
			{Name: "blank", Path: " "},
			{Name: "suspended", Path: "/rig/suspended", Suspended: true},
		},
	}

	got := codexHookWorkDirs("/city", cfg)
	if strings.Join(got, ",") != "/city,/rig/active" {
		t.Fatalf("work dirs = %#v, want city plus active rig only", got)
	}
	if got := codexHookWorkDirs("/city", nil); len(got) != 1 || got[0] != "/city" {
		t.Fatalf("nil config work dirs = %#v, want city only", got)
	}
}

func TestCodexHooksMissingPreCompactRejectsUnreadableAndMalformedFiles(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, ".codex", "hooks.json")
	if codexHooksMissingPreCompact(missingPath) {
		t.Fatal("missing file reported as stale")
	}

	writeCodexHooksForDoctorTest(t, dir, `{not-json`)
	if codexHooksMissingPreCompact(missingPath) {
		t.Fatal("malformed JSON reported as stale")
	}

	writeCodexHooksForDoctorTest(t, dir, `{"notHooks": {}}`)
	if codexHooksMissingPreCompact(missingPath) {
		t.Fatal("file without hooks map reported as stale")
	}
}

func TestCodexHooksMissingPreCompactRequiresManagedCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codex", "hooks.json")
	writeCodexHooksForDoctorTest(t, dir, `{
  "hooks": {
    "UserPromptSubmit": [{
      "hooks": [{
        "type": "command",
        "command": "printf custom"
      }]
    }]
  }
}`)

	if codexHooksMissingPreCompact(path) {
		t.Fatal("custom-only hooks reported as missing managed PreCompact")
	}
	if codexHookCommandLooksManaged("printf custom") {
		t.Fatal("custom command detected as managed")
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
