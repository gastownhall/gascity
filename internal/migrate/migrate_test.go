package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateCityCommonCase(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown", "../packs/qa-team"]
default_rig_includes = ["../packs/default rig"]

[[agent]]
name = "mayor"
scope = "city"
provider = "claude"
prompt_template = "prompts/mayor.md"
overlay_dir = "overlays/mayor"
namepool = "namepools/mayor.txt"
fallback = true

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "You are {{.Agent}}.\n")
	writeFile(t, cityDir, "prompts/worker.md", "You are a worker.\n")
	writeFile(t, cityDir, "overlays/mayor/CLAUDE.md", "city overlay\n")
	writeFile(t, cityDir, "namepools/mayor.txt", "Ada\nGrace\n")

	report, err := Apply(cityDir, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(report.Warnings) == 0 {
		t.Fatal("expected fallback warning, got none")
	}

	packToml := readFile(t, filepath.Join(cityDir, "pack.toml"))
	if !strings.Contains(packToml, "[pack]") {
		t.Fatalf("pack.toml missing [pack]:\n%s", packToml)
	}
	if !strings.Contains(packToml, "name = \"legacy-city\"") {
		t.Fatalf("pack.toml missing derived pack name:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[imports.gastown]") {
		t.Fatalf("pack.toml missing gastown import:\n%s", packToml)
	}
	if !strings.Contains(packToml, "source = \"../packs/gastown\"") {
		t.Fatalf("pack.toml missing gastown source:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[defaults.rig.imports.default-rig]") {
		t.Fatalf("pack.toml missing default rig import:\n%s", packToml)
	}

	cityToml := readFile(t, filepath.Join(cityDir, "city.toml"))
	if strings.Contains(cityToml, "[[agent]]") {
		t.Fatalf("city.toml still contains [[agent]]:\n%s", cityToml)
	}
	if strings.Contains(cityToml, "includes =") {
		t.Fatalf("city.toml still contains workspace.includes:\n%s", cityToml)
	}
	if strings.Contains(cityToml, "default_rig_includes") {
		t.Fatalf("city.toml still contains workspace.default_rig_includes:\n%s", cityToml)
	}

	mayorAgentToml := readFile(t, filepath.Join(cityDir, "agents", "mayor", "agent.toml"))
	if !strings.Contains(mayorAgentToml, "scope = \"city\"") {
		t.Fatalf("mayor agent.toml missing scope:\n%s", mayorAgentToml)
	}
	if !strings.Contains(mayorAgentToml, "provider = \"claude\"") {
		t.Fatalf("mayor agent.toml missing provider:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "prompt_template") {
		t.Fatalf("mayor agent.toml still contains prompt_template:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "overlay_dir") {
		t.Fatalf("mayor agent.toml still contains overlay_dir:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "namepool") {
		t.Fatalf("mayor agent.toml still contains namepool:\n%s", mayorAgentToml)
	}
	if strings.Contains(mayorAgentToml, "fallback") {
		t.Fatalf("mayor agent.toml still contains fallback:\n%s", mayorAgentToml)
	}

	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "prompt.md.tmpl")); !strings.Contains(got, "{{.Agent}}") {
		t.Fatalf("expected templated mayor prompt to be moved, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "worker", "prompt.md")); !strings.Contains(got, "worker") {
		t.Fatalf("expected plain worker prompt to be moved, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "overlay", "CLAUDE.md")); !strings.Contains(got, "city overlay") {
		t.Fatalf("expected overlay to move, got:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "agents", "mayor", "namepool.txt")); !strings.Contains(got, "Ada") {
		t.Fatalf("expected namepool to move, got:\n%s", got)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "prompts", "mayor.md")); !os.IsNotExist(err) {
		t.Fatalf("expected mayor prompt source to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "overlays", "mayor")); !os.IsNotExist(err) {
		t.Fatalf("expected overlay source to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "namepools", "mayor.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected namepool source to be removed, err=%v", err)
	}
}

func TestMigrateDryRunDoesNotWrite(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "hello\n")

	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	report, err := Apply(cityDir, Options{DryRun: true})
	if err != nil {
		t.Fatalf("Apply(dry-run): %v", err)
	}
	if len(report.Changes) == 0 {
		t.Fatal("expected dry-run changes, got none")
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("dry-run rewrote city.toml:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "pack.toml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create pack.toml, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "agents")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create agents dir, err=%v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeFile(t, cityDir, "prompts/mayor.md", "hello\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	beforePack := readFile(t, filepath.Join(cityDir, "pack.toml"))
	beforeCity := readFile(t, filepath.Join(cityDir, "city.toml"))

	report, err := Apply(cityDir, Options{})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(report.Changes) != 0 {
		t.Fatalf("expected no changes on second run, got %v", report.Changes)
	}
	if got := readFile(t, filepath.Join(cityDir, "pack.toml")); got != beforePack {
		t.Fatalf("pack.toml changed on second run:\n%s", got)
	}
	if got := readFile(t, filepath.Join(cityDir, "city.toml")); got != beforeCity {
		t.Fatalf("city.toml changed on second run:\n%s", got)
	}
}

func TestMigrateSharedPromptCopiesInsteadOfMoving(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/shared.md"

[[agent]]
name = "worker"
prompt_template = "prompts/shared.md"
`)
	writeFile(t, cityDir, "prompts/shared.md", "shared prompt\n")

	if _, err := Apply(cityDir, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "prompts", "shared.md")); err != nil {
		t.Fatalf("shared prompt should be retained for multi-use sources: %v", err)
	}
	for _, name := range []string{"mayor", "worker"} {
		got := readFile(t, filepath.Join(cityDir, "agents", name, "prompt.md"))
		if !strings.Contains(got, "shared prompt") {
			t.Fatalf("%s prompt missing copied content:\n%s", name, got)
		}
	}
}

func writeFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(contents, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}
