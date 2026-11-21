//go:build windows

package runtime

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runOverlayGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = trackedOverlayGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}

func initOverlayGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runOverlayGit(t, dir, "init", "-b", "main")
	runOverlayGit(t, dir, "config", "user.name", "Gas City Test")
	runOverlayGit(t, dir, "config", "user.email", "test@example.invalid")
	runOverlayGit(t, dir, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	runOverlayGit(t, dir, "add", "README.md")
	runOverlayGit(t, dir, "commit", "-m", "initial")
	return dir
}

func writeCopilotOverlay(t *testing.T, content string) string {
	t.Helper()
	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "per-provider", "copilot", ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll overlay: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile overlay: %v", err)
	}
	return srcDir
}

func writeCopilotOverlayFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	srcDir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(srcDir, "per-provider", "copilot", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll overlay: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile overlay: %v", err)
		}
	}
	return srcDir
}

func TestStageProviderOverlayDirSkipsTrackedFilesOnWindows(t *testing.T) {
	workDir := initOverlayGitRepo(t)
	target := filepath.Join(workDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	if err := os.WriteFile(target, []byte("repository instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	runOverlayGit(t, workDir, "add", ".github/copilot-instructions.md")
	runOverlayGit(t, workDir, "commit", "-m", "track instructions")

	if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != "repository instructions\n" {
		t.Fatalf("tracked file was overwritten: got %q", got)
	}
}

func TestStageProviderOverlayDirSkipsTrackedCaseAliasesOnWindows(t *testing.T) {
	workDir := initOverlayGitRepo(t)
	target := filepath.Join(workDir, ".GitHub", "Copilot-Instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	if err := os.WriteFile(target, []byte("repository instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	runOverlayGit(t, workDir, "add", ".GitHub/Copilot-Instructions.md")
	runOverlayGit(t, workDir, "commit", "-m", "track mixed-case instructions")

	if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != "repository instructions\n" {
		t.Fatalf("tracked case-alias file was overwritten: got %q", got)
	}
	if status := runOverlayGit(t, workDir, "status", "--porcelain"); status != "" {
		t.Fatalf("staging dirtied the repository:\n%s", status)
	}
}

func TestStageProviderOverlayDirStagesUntrackedFilesOnWindows(t *testing.T) {
	workDir := initOverlayGitRepo(t)
	target := filepath.Join(workDir, ".github", "copilot-instructions.md")

	if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != "gas city instructions\n" {
		t.Fatalf("untracked file was not staged: got %q", got)
	}
}

func TestStageProviderOverlayDirPreservesExistingWhenTrackednessUnknownOnWindows(t *testing.T) {
	workDir := t.TempDir()
	target := filepath.Join(workDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	if err := os.WriteFile(target, []byte("local instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	orig := probeOverlayTrackedStatuses
	probeOverlayTrackedStatuses = func(_ string, relPaths []string) (map[string]overlayTrackedStatus, bool) {
		statuses := map[string]overlayTrackedStatus{}
		for _, rel := range relPaths {
			statuses[cleanOverlayRelPath(rel)] = overlayTrackedUnknown
		}
		return statuses, true
	}
	defer func() { probeOverlayTrackedStatuses = orig }()

	if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != "local instructions\n" {
		t.Fatalf("unknown trackedness overwrote existing file: got %q", got)
	}
}

func TestStageProviderOverlayDirDoesNotCreateWhenTrackednessUnknownOnWindows(t *testing.T) {
	workDir := t.TempDir()
	target := filepath.Join(workDir, ".github", "copilot-instructions.md")

	orig := probeOverlayTrackedStatuses
	probeOverlayTrackedStatuses = func(_ string, relPaths []string) (map[string]overlayTrackedStatus, bool) {
		statuses := map[string]overlayTrackedStatus{}
		for _, rel := range relPaths {
			statuses[cleanOverlayRelPath(rel)] = overlayTrackedUnknown
		}
		return statuses, true
	}
	defer func() { probeOverlayTrackedStatuses = orig }()

	if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("unknown trackedness created %q (err=%v)", target, err)
	}
}

func TestStageProviderOverlayDirWarnsOnceForTrackedSkipOnWindows(t *testing.T) {
	workDir := initOverlayGitRepo(t)
	target := filepath.Join(workDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll target: %v", err)
	}
	if err := os.WriteFile(target, []byte("repository instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	runOverlayGit(t, workDir, "add", ".github/copilot-instructions.md")
	runOverlayGit(t, workDir, "commit", "-m", "track instructions")

	var warnings bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := StageProviderOverlayDir(writeCopilotOverlay(t, "gas city instructions\n"), workDir, []string{"copilot"}, &warnings); err != nil {
			t.Fatalf("StageProviderOverlayDir #%d: %v", i+1, err)
		}
	}
	if got := strings.Count(warnings.String(), "skipping provider overlay"); got != 1 {
		t.Fatalf("tracked skip warnings = %d, want 1; warnings:\n%s", got, warnings.String())
	}
}
