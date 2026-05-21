package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestInstructionsFileCheck_NilConfig(t *testing.T) {
	r := NewInstructionsFileCheck(nil, "").Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK", r.Status)
	}
}

func TestInstructionsFileCheck_NoRigs(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "a", Provider: "claude"}}}
	r := NewInstructionsFileCheck(cfg, t.TempDir()).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestInstructionsFileCheck_AllFilesPresent(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	writeFile(t, filepath.Join(rig, "AGENTS.md"), "agent instructions\n")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "demo", Provider: "claude"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestInstructionsFileCheck_FlagsMissingButRecoverable(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	// Rig only ships CLAUDE.md; an agent on a non-Claude provider that
	// expects AGENTS.md should be flagged.
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "demo", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `rig "demo"`) || !strings.Contains(r.Details[0], "AGENTS.md") || !strings.Contains(r.Details[0], "CLAUDE.md") {
		t.Errorf("Details[0] missing expected components: %q", r.Details[0])
	}
	if !NewInstructionsFileCheck(cfg, city).CanFix() {
		t.Error("expected CanFix() = true")
	}
}

func TestInstructionsFileCheck_NoFallbackNoWarning(t *testing.T) {
	// A rig with no instruction files at all should not be flagged —
	// we cannot recover, and emitting a warning that the user cannot
	// act on is noise.
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "bare")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "bare", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "bare", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestInstructionsFileCheck_FixSymlinksFallback(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "demo", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	c := NewInstructionsFileCheck(cfg, city)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	link, err := os.Readlink(filepath.Join(rig, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not symlinked: %v", err)
	}
	if link != "CLAUDE.md" {
		t.Errorf("AGENTS.md link target = %q, want CLAUDE.md", link)
	}
	// Re-run: check now passes.
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("after Fix, Status = %v, want StatusOK; details=%v", r.Status, r.Details)
	}
}

func TestInstructionsFileCheck_FixReportsExpectedDirectoryCollision(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")
	if err := os.MkdirAll(filepath.Join(rig, "AGENTS.md"), 0o755); err != nil {
		t.Fatalf("MkdirAll(AGENTS.md): %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "demo", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	c := NewInstructionsFileCheck(cfg, city)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix() error = nil, want directory collision error")
	}
	if !strings.Contains(err.Error(), "AGENTS.md exists as a directory") {
		t.Fatalf("Fix() error = %v, want AGENTS.md directory collision", err)
	}
	info, statErr := os.Lstat(filepath.Join(rig, "AGENTS.md"))
	if statErr != nil {
		t.Fatalf("Lstat(AGENTS.md): %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("AGENTS.md is not a directory after Fix")
	}
}

func TestInstructionsFileCheck_RelativeRigPathResolves(t *testing.T) {
	city := t.TempDir()
	rigRel := filepath.Join("rigs", "rel")
	writeFile(t, filepath.Join(city, rigRel, "CLAUDE.md"), "x")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "a", Dir: "rel", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "rel", Path: rigRel}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
}

func TestInstructionsFileCheck_DeterministicOrdering(t *testing.T) {
	city := t.TempDir()
	for _, name := range []string{"alpha", "zulu"} {
		writeFile(t, filepath.Join(city, "rigs", name, "CLAUDE.md"), "x")
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "z", Dir: "zulu", Provider: "codex"},
			{Name: "a", Dir: "alpha", Provider: "codex"},
		},
		Rigs: []config.Rig{
			{Name: "zulu", Path: filepath.Join(city, "rigs", "zulu")},
			{Name: "alpha", Path: filepath.Join(city, "rigs", "alpha")},
		},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 2 {
		t.Fatalf("Details = %d, want 2: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `rig "alpha"`) {
		t.Errorf("Details[0] should mention alpha first: %q", r.Details[0])
	}
	if !strings.Contains(r.Details[1], `rig "zulu"`) {
		t.Errorf("Details[1] should mention zulu second: %q", r.Details[1])
	}
}

func TestInstructionsFileCheck_CityScopedAgentChecksCityRootOnly(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "unrelated")
	writeFile(t, filepath.Join(city, "CLAUDE.md"), "city instructions\n")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "rig instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "city-worker", Scope: "city", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "unrelated", Path: rig}},
	}
	c := NewInstructionsFileCheck(cfg, city)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "city root") {
		t.Fatalf("Details[0] = %q, want city root warning", r.Details[0])
	}
	if strings.Contains(r.Details[0], `rig "unrelated"`) {
		t.Fatalf("Details[0] = %q, should not warn for unrelated rig", r.Details[0])
	}

	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(city, "AGENTS.md")); err != nil || link != "CLAUDE.md" {
		t.Fatalf("city AGENTS.md link = %q, err=%v; want CLAUDE.md symlink", link, err)
	}
	if _, err := os.Lstat(filepath.Join(rig, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("unrelated rig AGENTS.md exists after Fix; err=%v", err)
	}
}

func TestInstructionsFileCheck_DefaultAgentChecksCityRoot(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "unrelated")
	writeFile(t, filepath.Join(city, "CLAUDE.md"), "city instructions\n")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "rig instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "default-worker", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "unrelated", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "city root") {
		t.Fatalf("Details[0] = %q, want city root warning", r.Details[0])
	}
	if strings.Contains(r.Details[0], `rig "unrelated"`) {
		t.Fatalf("Details[0] = %q, should not warn for unrelated rig", r.Details[0])
	}
}

func TestInstructionsFileCheck_WorkDirOnlyAgentChecksWorkDirRoot(t *testing.T) {
	city := t.TempDir()
	workDir := filepath.Join("worktrees", "solo")
	workRoot := filepath.Join(city, workDir)
	rig := filepath.Join(city, "rigs", "unrelated")
	writeFile(t, filepath.Join(workRoot, "CLAUDE.md"), "workdir instructions\n")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "rig instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{{Name: "workdir-worker", WorkDir: workDir, Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "unrelated", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "work_dir") || !strings.Contains(r.Details[0], workRoot) {
		t.Fatalf("Details[0] = %q, want work_dir warning for %s", r.Details[0], workRoot)
	}
	if strings.Contains(r.Details[0], `rig "unrelated"`) {
		t.Fatalf("Details[0] = %q, should not warn for unrelated rig", r.Details[0])
	}
}

func TestInstructionsFileCheck_DanglingExpectedSymlinkCountsPresent(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")
	if err := os.Symlink("missing.md", filepath.Join(rig, "AGENTS.md")); err != nil {
		t.Fatalf("Symlink() setup error: %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", Dir: "demo", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	c := NewInstructionsFileCheck(cfg, city)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for preexisting symlink; details=%v", r.Status, r.Details)
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error = %v, want nil for preexisting symlink", err)
	}
	link, err := os.Readlink(filepath.Join(rig, "AGENTS.md"))
	if err != nil {
		t.Fatalf("Readlink(AGENTS.md): %v", err)
	}
	if link != "missing.md" {
		t.Fatalf("AGENTS.md link = %q, want existing missing.md link unchanged", link)
	}
}

func TestInstructionsFileCheck_DanglingFallbackSymlinkNotRecoverable(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.md", filepath.Join(rig, "CLAUDE.md")); err != nil {
		t.Fatalf("Symlink() setup error: %v", err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", Dir: "demo", Provider: "codex"}},
		Rigs:   []config.Rig{{Name: "demo", Path: rig}},
	}
	c := NewInstructionsFileCheck(cfg, city)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for unusable fallback; details=%v", r.Status, r.Details)
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error = %v, want nil for unusable fallback", err)
	}
	if _, err := os.Lstat(filepath.Join(rig, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENTS.md exists after Fix with unusable fallback; err=%v", err)
	}
}

func TestInstructionsFileCheck_MergesProvidersForSameGap(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "demo")
	writeFile(t, filepath.Join(rig, "CLAUDE.md"), "claude instructions\n")

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "gemini-worker", Dir: "demo", Provider: "gemini"},
			{Name: "codex-worker", Dir: "demo", Provider: "codex"},
		},
		Rigs: []config.Rig{{Name: "demo", Path: rig}},
	}
	r := NewInstructionsFileCheck(cfg, city).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want one merged warning: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `providers "codex", "gemini"`) {
		t.Fatalf("Details[0] = %q, want sorted plural provider list", r.Details[0])
	}
}
