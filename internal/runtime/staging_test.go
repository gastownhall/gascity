package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageDirPreservesBestEffortOverlayWarnings(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	if err := StageDir(srcDir, dstDir); err != nil {
		t.Fatalf("StageDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "ok.txt"))
	if err != nil {
		t.Fatalf("read copied overlay file: %v", err)
	}
	if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}

func TestStageWorkDirSkipsCopyWhenSourceAlreadyMatchesResolvedDestination(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	src := filepath.Join(workDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := StageWorkDir(workDir, "", []CopyEntry{{Src: src}}); err != nil {
		t.Fatalf("StageWorkDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read staged source file: %v", err)
	}
	if string(data) != "seed" {
		t.Fatalf("staged source file = %q, want %q", string(data), "seed")
	}
}

func TestStageWorkDirFailsWhenOverlayCopyWarns(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	err := StageWorkDir(workDir, srcDir, nil)
	if err == nil {
		t.Fatal("StageWorkDir() succeeded, want overlay staging error")
	}
	if data, readErr := os.ReadFile(filepath.Join(workDir, "ok.txt")); readErr != nil {
		t.Fatalf("read copied overlay file: %v", readErr)
	} else if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}

func TestStageSessionWorkDirUsesConcreteProviderOverlayName(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	kiroConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(kiroConfig), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(kiroConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("write Kiro overlay: %v", err)
	}
	claudeConfig := filepath.Join(packOverlay, "per-provider", "claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeConfig), 0o755); err != nil {
		t.Fatalf("mkdir Claude overlay: %v", err)
	}
	if err := os.WriteFile(claudeConfig, []byte("claude instructions"), 0o644); err != nil {
		t.Fatalf("write Claude overlay: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:             workDir,
		ProviderName:        "claude",
		ProviderOverlayName: "kiro",
		PackOverlayDirs:     []string{packOverlay},
	})
	if err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, ".kiro", "agents", "gascity.json")); err != nil {
		t.Fatalf("read staged Kiro config: %v", err)
	} else if string(got) != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro config = %q, want gascity config", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "CLAUDE.md")); err == nil {
		t.Fatal("staged Claude overlay for Kiro provider inheriting Claude launch behavior")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat Claude overlay: %v", err)
	}
}

func TestStageSessionWorkDirWithWarningsSurfacesKiroPreservationWarning(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	var warnings bytes.Buffer
	err := StageSessionWorkDirWithWarnings(Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

func TestStageProviderOverlayDirIgnoresWarningWriterFailure(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	err := StageProviderOverlayDir(packOverlay, workDir, []string{"kiro"}, failingWriter{})
	if err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("writer unavailable")
}
