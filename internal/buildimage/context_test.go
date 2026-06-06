package buildimage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestAssembleContextBasic(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	// Create a minimal city structure.
	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	mkdirAll(t, cityDir, "prompts")
	writeFile(t, cityDir, "prompts/mayor.md", "You are the mayor.")

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
		BaseImage: "gc-agent:v1",
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Verify Dockerfile exists.
	df, err := os.ReadFile(filepath.Join(outputDir, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}
	if got := string(df); got == "" {
		t.Error("Dockerfile is empty")
	}

	// Verify workspace/city.toml exists.
	assertFileExists(t, outputDir, "workspace/city.toml")

	// Verify workspace/prompts/mayor.md exists.
	assertFileExists(t, outputDir, "workspace/prompts/mayor.md")

	// Verify manifest.
	assertFileExists(t, outputDir, "workspace/.gc-prebaked")
	manifestData, _ := os.ReadFile(filepath.Join(outputDir, "workspace/.gc-prebaked"))
	var m Manifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("manifest version = %d, want 1", m.Version)
	}
	if m.BaseImage != "gc-agent:v1" {
		t.Errorf("manifest base_image = %q, want gc-agent:v1", m.BaseImage)
	}
}

func TestAssembleContextExcludes(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)

	// Create files that should be excluded.
	mkdirAll(t, cityDir, ".gc/agents")
	writeFile(t, cityDir, ".gc/controller.lock", "locked")
	writeFile(t, cityDir, ".gc/controller.sock", "sock")
	writeFile(t, cityDir, ".gc/events.jsonl", "{}")
	writeFile(t, cityDir, ".gc/agents/mayor.json", "{}")
	writeFile(t, cityDir, ".env", "SECRET=x")
	writeFile(t, cityDir, "credentials.json", "{}")

	// Create files that should be included.
	mkdirAll(t, cityDir, "formulas")
	writeFile(t, cityDir, "formulas/test.toml", "formula")

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Verify excluded files are NOT present.
	assertFileNotExists(t, outputDir, "workspace/.gc/controller.lock")
	assertFileNotExists(t, outputDir, "workspace/.gc/controller.sock")
	assertFileNotExists(t, outputDir, "workspace/.gc/events.jsonl")
	assertFileNotExists(t, outputDir, "workspace/.gc/agents/mayor.json")
	assertFileNotExists(t, outputDir, "workspace/.env")
	assertFileNotExists(t, outputDir, "workspace/credentials.json")

	// Verify included files ARE present.
	assertFileExists(t, outputDir, "workspace/formulas/test.toml")
	assertFileExists(t, outputDir, "workspace/city.toml")
}

func TestAssembleContextExcludesAllGCSubdirs(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, cityDir, filepath.Join(citylayout.SystemPacksRoot, "bd", "pack.toml"), "[pack]\nname = \"bd\"\n")
	writeFile(t, cityDir, filepath.Join(citylayout.CachePacksRoot, "remote", ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, cityDir, filepath.Join(citylayout.RuntimeRoot, "runtime", "artifact.txt"), "runtime")
	writeFile(t, cityDir, filepath.Join(".gc", "prompts", "mayor.md"), "old prompt")
	writeFile(t, cityDir, filepath.Join(".gc", "formulas", "legacy.toml"), "name = \"legacy\"\n")
	writeFile(t, cityDir, filepath.Join(".gc", "settings.json"), "{}")
	writeFile(t, cityDir, filepath.Join(".gc", "scripts", "setup.sh"), "#!/bin/sh\n")

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// All .gc/ subdirs are now excluded.
	assertFileNotExists(t, outputDir, filepath.Join("workspace", citylayout.SystemPacksRoot, "bd", "pack.toml"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", citylayout.CachePacksRoot, "remote", ".git", "HEAD"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", citylayout.RuntimeRoot, "runtime", "artifact.txt"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", ".gc", "prompts", "mayor.md"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", ".gc", "formulas", "legacy.toml"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", ".gc", "scripts", "setup.sh"))
	assertFileNotExists(t, outputDir, filepath.Join("workspace", ".gc", "settings.json"))
}

func TestAssembleContextWithRigPaths(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()
	rigDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, rigDir, "main.go", "package main")
	writeFile(t, rigDir, "README.md", "# Rig")

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
		RigPaths:  map[string]string{"my-rig": rigDir},
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Verify rig content was copied.
	assertFileExists(t, outputDir, "workspace/my-rig/main.go")
	assertFileExists(t, outputDir, "workspace/my-rig/README.md")
}

func TestAssembleContextPreservesCustomReferencedCityFiles(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `include = ["fragments/ops.toml"]

[workspace]
name = "test-city"

[formulas]
dir = "my-formulas"
`)
	writeFile(t, cityDir, "fragments/ops.toml", `[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "my-formulas/custom.toml", "name = \"custom\"\n")
	writeFile(t, cityDir, "prompts/mayor.md", "You are the mayor.")

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	assertFileExists(t, outputDir, "workspace/fragments/ops.toml")
	assertFileExists(t, outputDir, "workspace/my-formulas/custom.toml")
	assertFileExists(t, outputDir, "workspace/prompts/mayor.md")
}

func TestAssembleContextSkipsSymlinkedDirs(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()
	targetDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	// Create a symlink to a directory (simulates .claude/skills/core.gc-agents).
	writeFile(t, targetDir, "skill.md", "# skill")
	writeFile(t, cityDir, ".claude/skills/local.md", "# local")
	mkdirAll(t, cityDir, ".claude/skills")
	if err := os.Symlink(targetDir, filepath.Join(cityDir, ".claude", "skills", "core.gc-agents")); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
		Stderr:    &stderr,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if !strings.Contains(stderr.String(), "skipping symlinked directory") ||
		!strings.Contains(stderr.String(), filepath.Join(".claude", "skills", "core.gc-agents")) {
		t.Fatalf("stderr = %q, want skipped directory symlink diagnostic", stderr.String())
	}

	// Symlinked directory should be skipped, not cause an error.
	assertFileNotExists(t, outputDir, "workspace/.claude/skills/core.gc-agents")
	assertFileExists(t, outputDir, "workspace/.claude/skills/local.md")
}

func TestAssembleContextCopiesRegularFileSymlink(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, cityDir, "targets/source.txt", "linked content")
	if err := os.Chmod(filepath.Join(cityDir, "targets", "source.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkdirAll(t, cityDir, "prompts")
	if err := os.Symlink(filepath.Join(cityDir, "targets", "source.txt"), filepath.Join(cityDir, "prompts", "linked.txt")); err != nil {
		t.Fatal(err)
	}

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outputDir, "workspace", "prompts", "linked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "linked content" {
		t.Fatalf("linked file content = %q, want linked content", got)
	}
	info, err := os.Stat(filepath.Join(outputDir, "workspace", "prompts", "linked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("linked file mode = %v, want 0600", got)
	}
}

func TestAssembleContextCopiesAbsoluteFileSymlinkFromRelativeCityPath(t *testing.T) {
	parentDir := t.TempDir()
	cityDir := filepath.Join(parentDir, "city")
	outputDir := filepath.Join(parentDir, "out")
	targetDir := t.TempDir()
	targetFile := filepath.Join(targetDir, "source.txt")

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, targetDir, "source.txt", "linked content")
	mkdirAll(t, cityDir, "prompts")
	if err := os.Symlink(targetFile, filepath.Join(cityDir, "prompts", "linked.txt")); err != nil {
		t.Fatal(err)
	}

	t.Chdir(parentDir)
	err := AssembleContext(Options{
		CityPath:  "city",
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outputDir, "workspace", "prompts", "linked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "linked content" {
		t.Fatalf("linked file content = %q, want linked content", got)
	}
}

func TestAssembleContextCopiesAbsoluteFileSymlinkFromRelativeRigPath(t *testing.T) {
	parentDir := t.TempDir()
	cityDir := filepath.Join(parentDir, "city")
	rigDir := filepath.Join(parentDir, "rig")
	outputDir := filepath.Join(parentDir, "out")
	targetDir := t.TempDir()
	targetFile := filepath.Join(targetDir, "source.txt")

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, targetDir, "source.txt", "linked rig content")
	mkdirAll(t, rigDir, "docs")
	if err := os.Symlink(targetFile, filepath.Join(rigDir, "docs", "linked.txt")); err != nil {
		t.Fatal(err)
	}

	t.Chdir(parentDir)
	err := AssembleContext(Options{
		CityPath:  "city",
		OutputDir: outputDir,
		RigPaths:  map[string]string{"demo": "rig"},
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outputDir, "workspace", "demo", "docs", "linked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "linked rig content" {
		t.Fatalf("linked rig file content = %q, want linked rig content", got)
	}
}

func TestAssembleContextSkipsFileSymlinkToExcludedTarget(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, cityDir, filepath.Join(citylayout.RuntimeRoot, "runtime", "private.txt"), "do not bake")
	mkdirAll(t, cityDir, "prompts")
	if err := os.Symlink(filepath.Join(cityDir, citylayout.RuntimeRoot, "runtime", "private.txt"), filepath.Join(cityDir, "prompts", "public.md")); err != nil {
		t.Fatal(err)
	}

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	assertFileNotExists(t, outputDir, "workspace/prompts/public.md")
	assertFileExists(t, outputDir, "workspace/city.toml")
}

func TestAssembleContextSkipsFileSymlinkToExternalGCRuntimeTarget(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()
	externalDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, externalDir, filepath.Join(".gc", "agents", "worker", "identity"), "do not bake")
	mkdirAll(t, cityDir, "prompts")
	if err := os.Symlink(filepath.Join(externalDir, ".gc", "agents", "worker", "identity"), filepath.Join(cityDir, "prompts", "public.md")); err != nil {
		t.Fatal(err)
	}

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	assertFileNotExists(t, outputDir, "workspace/prompts/public.md")
	assertFileExists(t, outputDir, "workspace/city.toml")
}

func TestAssembleContextSkipsBrokenSymlink(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	writeFile(t, cityDir, "prompts/keep.md", "# keep")
	if err := os.Symlink(filepath.Join(cityDir, "missing.md"), filepath.Join(cityDir, "prompts", "missing.md")); err != nil {
		t.Fatal(err)
	}

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	assertFileExists(t, outputDir, "workspace/prompts/keep.md")
	assertFileNotExists(t, outputDir, "workspace/prompts/missing.md")
}

func TestAssembleContextErrorsForSymlinkLoop(t *testing.T) {
	cityDir := t.TempDir()
	outputDir := t.TempDir()

	writeFile(t, cityDir, "city.toml", `[workspace]
name = "test-city"
`)
	mkdirAll(t, cityDir, "prompts")
	if err := os.Symlink("loop.md", filepath.Join(cityDir, "prompts", "loop.md")); err != nil {
		t.Fatal(err)
	}

	err := AssembleContext(Options{
		CityPath:  cityDir,
		OutputDir: outputDir,
	})
	if err == nil {
		t.Fatal("AssembleContext succeeded with symlink loop, want error")
	}
	if !strings.Contains(err.Error(), "resolving symlink") {
		t.Fatalf("AssembleContext error = %q, want symlink resolution context", err)
	}
}

func TestAssembleContextRequiresCityPath(t *testing.T) {
	err := AssembleContext(Options{OutputDir: t.TempDir()})
	if err == nil {
		t.Error("expected error for empty city path")
	}
}

func TestAssembleContextRequiresOutputDir(t *testing.T) {
	err := AssembleContext(Options{CityPath: t.TempDir()})
	if err == nil {
		t.Error("expected error for empty output dir")
	}
}

func TestExcludedPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".gc/controller.lock", true},
		{".gc/controller.sock", true},
		{".gc/events.jsonl", true},
		{".gc/agents/mayor.json", true},
		{".gc/system/packs/bd/pack.toml", true},
		{".gc/cache/packs/remote/.git/HEAD", true},
		{".gc/runtime/worktrees/agent/file.txt", true},
		{".gc/prompts/mayor.md", true},
		{".gc/formulas/test.toml", true},
		{".gc/scripts/setup.sh", true},
		{".gc/settings.json", true},
		{".env", true},
		{"credentials.json", true},
		{"path/to/secret.key", true},
		{"city.toml", false},
		{"formulas/test.toml", false},
		{"prompts/mayor.md", false},
		{"hooks/claude.json", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := excludedPath(tt.path); got != tt.want {
				t.Errorf("excludedPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// --- Test helpers ---

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertFileExists(t *testing.T, dir, rel string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("expected file to exist: %s", rel)
	}
}

func assertFileNotExists(t *testing.T, dir, rel string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected file to NOT exist: %s", rel)
	}
}
