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

func TestCodexHookWorkDirsIncludesResolvedAgentWorkDirs(t *testing.T) {
	cityDir := t.TempDir()
	activeRig := filepath.Join(cityDir, "rigs", "active")
	suspendedRig := filepath.Join(cityDir, "rigs", "suspended")
	agentWorkDir := filepath.Join(cityDir, ".gc", "agents", "reviewer")
	cfg := &config.City{
		Workspace: config.Workspace{InstallAgentHooks: []string{"codex"}},
		Rigs: []config.Rig{
			{Name: "active", Path: activeRig},
			{Name: "suspended", Path: suspendedRig, Suspended: true},
		},
		Agents: []config.Agent{
			{Name: "reviewer", Dir: "active", WorkDir: agentWorkDir},
			{Name: "gemini", Dir: "active", InstallAgentHooks: []string{"gemini"}, WorkDir: filepath.Join(cityDir, ".gc", "agents", "gemini")},
			{Name: "parked", Dir: "active", WorkDir: filepath.Join(cityDir, ".gc", "agents", "parked"), Suspended: true},
			{Name: "codex", Dir: "suspended", WorkDir: filepath.Join(cityDir, ".gc", "agents", "suspended")},
		},
	}

	got := codexHookWorkDirs(cityDir, cfg)

	assertDoctorPathPresent(t, got, cityDir)
	assertDoctorPathPresent(t, got, activeRig)
	assertDoctorPathPresent(t, got, agentWorkDir)
	assertDoctorPathAbsent(t, got, suspendedRig)
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "gemini"))
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "parked"))
	assertDoctorPathAbsent(t, got, filepath.Join(cityDir, ".gc", "agents", "suspended"))
}

func TestCodexHookWorkDirsIncludesBoundedPoolInstanceWorkDirs(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "active")
	maxSessions := 2
	cfg := &config.City{
		Workspace: config.Workspace{InstallAgentHooks: []string{"codex"}},
		Rigs:      []config.Rig{{Name: "active", Path: rigDir}},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "active",
			WorkDir:           filepath.Join(".gc", "worktrees", "{{.Rig}}", "{{.AgentBase}}"),
			MaxActiveSessions: &maxSessions,
		}},
	}

	got := codexHookWorkDirs(cityDir, cfg)

	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker"))
	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker-1"))
	assertDoctorPathPresent(t, got, filepath.Join(cityDir, ".gc", "worktrees", "active", "worker-2"))
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
}

func assertDoctorPathPresent(t *testing.T, paths []string, want string) {
	t.Helper()
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			return
		}
	}
	t.Fatalf("paths = %#v, want %s present", paths, want)
}

func assertDoctorPathAbsent(t *testing.T, paths []string, want string) {
	t.Helper()
	want = filepath.Clean(want)
	for _, path := range paths {
		if filepath.Clean(path) == want {
			t.Fatalf("paths = %#v, want %s absent", paths, want)
		}
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
