package tmux

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStageStartFilesSurfacesKiroPreservationWarning(t *testing.T) {
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
	err := stageStartFiles(runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
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

func TestStageStartFilesKeepsScaffoldOutOfSpawnerCWD(t *testing.T) {
	root := t.TempDir()
	sharedWorktree := filepath.Join(root, "shared-builder")
	beadSlug := "ga-ajw1no-1-as-a-maintainer-i-can-reproduce-stray-session-scaffold-leakage"
	leakedWorkDir := filepath.Join(sharedWorktree, beadSlug)
	workDir := filepath.Join(root, "city", ".gc", "worktrees", "gascity", "builder", beadSlug)
	packOverlay := filepath.Join(root, "city", "packs", "core", "overlay")

	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".claude", "skills", "triage", "SKILL.md"), "---\nname: triage\n---\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".codex", "hooks.json"), `{"hooks":{"SessionStart":[]}}`+"\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".gc", "settings.json"), "{}\n")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", workDir, err)
	}
	if err := os.MkdirAll(sharedWorktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sharedWorktree, err)
	}
	t.Chdir(sharedWorktree)

	var warnings bytes.Buffer
	err := stageStartFiles(runtime.Config{
		WorkDir:             workDir,
		ProviderName:        "codex",
		ProviderOverlayName: "codex",
		PackOverlayDirs:     []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}

	for _, rel := range []string{
		filepath.Join(".claude", "skills", "triage", "SKILL.md"),
		filepath.Join(".codex", "hooks.json"),
	} {
		if _, err := os.Stat(filepath.Join(workDir, rel)); err != nil {
			t.Errorf("target scaffold %s missing under workdir %q: %v", rel, workDir, err)
		}
	}
	// A top-level .gc/ in the overlay source is a runtime mirror and must never
	// be staged into a session workdir (overlay.skipRuntimeMirror). The session's
	// own .gc/settings.json is staged separately through the hook-file path, not
	// copied verbatim from the pack overlay.
	if _, err := os.Stat(filepath.Join(workDir, ".gc", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("overlay .gc runtime mirror must not be staged under workdir %q (stat err = %v)", workDir, err)
	}
	if _, err := os.Stat(leakedWorkDir); err == nil {
		t.Fatalf("shared cwd contains stray bead-slug scaffold directory %q; scaffold must stay under %q", leakedWorkDir, workDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat leaked workdir %q: %v", leakedWorkDir, err)
	}
}

func TestStageStartFilesRejectsEscapingCopyDestination(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	outside := filepath.Join(root, "outside.txt")

	err := stageStartFiles(runtime.Config{
		WorkDir:   workDir,
		CopyFiles: []runtime.CopyEntry{{Src: src, RelDst: filepath.Join("..", "outside.txt")}},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("stageStartFiles() succeeded, want escaping destination error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestCopyToRejectsEscapingDestination(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	src := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	outside := filepath.Join(root, "outside.txt")
	p := &Provider{workDirs: map[string]string{"worker": workDir}}

	if err := p.CopyTo("worker", src, filepath.Join("..", "outside.txt")); err == nil {
		t.Fatal("CopyTo() succeeded, want escaping destination error")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestCopyToRejectsEscapingDestinationBeforeBestEffortChecks(t *testing.T) {
	invalid := filepath.Join("..", "outside.txt")
	tests := []struct {
		name string
		p    *Provider
		src  string
	}{
		{name: "unknown session", p: &Provider{workDirs: map[string]string{}}, src: "/missing-source"},
		{name: "missing source", p: &Provider{workDirs: map[string]string{"worker": t.TempDir()}}, src: "/missing-source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.p.CopyTo("worker", tt.src, invalid); err == nil {
				t.Fatal("CopyTo() succeeded, want destination validation error")
			}
		})
	}
}

func TestCopyToRejectsNestedDestinationSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	srcDir := filepath.Join(root, "source")
	for _, dir := range []string{
		filepath.Join(workDir, "dest"),
		outsideDir,
		filepath.Join(srcDir, "nested"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "dest", "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	p := &Provider{workDirs: map[string]string{"worker": workDir}}

	err := p.CopyTo("worker", srcDir, "dest")
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn")); !os.IsNotExist(statErr) {
		t.Fatalf("CopyTo() error = %v; outside destination stat error = %v, want not exist", err, statErr)
	}
	if err == nil {
		t.Fatal("CopyTo() succeeded, want nested symlink escape error")
	}
}

func TestPlaceStagePreflightsWholeBatchBeforeMutation(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "linked")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	safe := filepath.Join(root, "safe.txt")
	payload := filepath.Join(root, "payload.txt")
	if err := os.WriteFile(safe, []byte("safe"), 0o644); err != nil {
		t.Fatalf("write safe source: %v", err)
	}
	if err := os.WriteFile(payload, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write payload source: %v", err)
	}

	pl := &tmuxPlace{
		p:    &Provider{workDirs: map[string]string{"worker": workDir}},
		name: "worker",
	}
	err := pl.Stage(context.Background(), []runtime.CopyEntry{
		{Src: safe, RelDst: "safe.txt"},
		{Src: payload, RelDst: filepath.Join("linked", "pwn.txt")},
	})
	if err == nil {
		t.Fatal("Place.Stage() succeeded, want symlink containment error")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "safe.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("first batch entry was partially staged: stat error = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("outside destination stat error = %v, want not exist", statErr)
	}
}

func TestTmuxLaunchBoundaryRejectsMissingWorkDir(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "not-created")
	ops := &tmuxStartOps{}

	if err := ops.createSession("worker", workDir, "agent", nil); err == nil {
		t.Fatal("createSession() succeeded, want missing workdir error before launch")
	}
	if err := ops.respawnAgent("worker", workDir, "agent", nil); err == nil {
		t.Fatal("respawnAgent() succeeded, want missing workdir error before relaunch")
	}
}

func writeTmuxScaffoldFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
