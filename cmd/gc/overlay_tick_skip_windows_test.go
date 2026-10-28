//go:build windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func bareGCInlineCommands(data []byte) []string {
	text := string(data)
	var out []string
	codeStart := -1
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		if j-i != 1 {
			i = j
			continue
		}
		if codeStart < 0 {
			codeStart = i + 1
		} else {
			code := text[codeStart:i]
			if code == "gc" || strings.HasPrefix(code, "gc ") {
				out = append(out, code)
			}
			codeStart = -1
		}
		i = j
	}
	return out
}

func TestTickOverlaySkip(t *testing.T) {
	copilotHooks := filepath.Join(".github", "hooks", "gascity.json")
	free := tickOverlaySkip(nil, nil)
	if free(copilotHooks, false) {
		t.Errorf("Copilot's hook file is skipped on the tick though hooks.Install will not write it")
	}
	if !free(filepath.Join(".codex", "hooks.json"), false) {
		t.Errorf("a mergeable file outside the staging-owned set is staged on the tick")
	}
	if free(filepath.Join(".github", "copilot-instructions.md"), false) || free(".github", true) {
		t.Errorf("a non-mergeable file or a directory is skipped")
	}
	if !tickOverlaySkip([]string{"copilot"}, nil)(copilotHooks, false) {
		t.Errorf("Copilot's hook file is staged on the tick though hooks.Install writes it")
	}
}

// A copilot agent's first session keeps its config fingerprint. The tick
// stages Copilot's hook file before taking the fingerprint, so session start,
// which stages the same overlay again, changes neither the file nor the hooks
// the fingerprint covers. Before, the tick skipped the file, session start
// created it, and the next tick drained the session for config drift.
func TestCopilotHookFileIsStagedBeforeTheFirstFingerprint(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bundled, err := os.ReadFile(filepath.Join("..", "..", "internal", "bootstrap", "packs", "core", "overlay", "per-provider", "copilot", ".github", "hooks", "gascity.json"))
	if err != nil {
		t.Fatal(err)
	}
	overlayDir := filepath.Join(cityDir, "packs", "myrig", "overlay")
	hookSrc := filepath.Join(overlayDir, "per-provider", "copilot", ".github", "hooks", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(hookSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookSrc, bundled, 0o644); err != nil {
		t.Fatal(err)
	}
	bundledInstructions, err := os.ReadFile(filepath.Join("..", "..", "internal", "bootstrap", "packs", "core", "overlay", "per-provider", "copilot", ".github", "copilot-instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	instructionsSrc := filepath.Join(overlayDir, "per-provider", "copilot", ".github", "copilot-instructions.md")
	if err := os.WriteFile(instructionsSrc, bundledInstructions, 0o644); err != nil {
		t.Fatal(err)
	}

	copilotBase := "builtin:copilot"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "copilot",
			Scope:    "rig",
			Dir:      "myrig",
		}},
		Providers: map[string]config.ProviderSpec{
			"copilot": {Base: &copilotBase, Command: "cmd.exe"},
		},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}
	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	cfgAgent := &cfg.Agents[0]
	resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	workDir, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, "myrig/worker", cfgAgent, bp.rigs)
	if err != nil {
		t.Fatalf("resolveConfiguredWorkDir: %v", err)
	}
	instructionsFile := filepath.Join(workDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(instructionsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructionsFile, bundledInstructions, 0o644); err != nil {
		t.Fatal(err)
	}
	rigName := sessionSetupContextForAgent(bp.cityPath, bp.cityName, "myrig/worker", cfgAgent, bp.rigs).Rig

	materializeProviderOverlaysBeforeFingerprint(bp, cfgAgent, resolved, "myrig/worker", rigName, workDir, io.Discard)
	hookFile := filepath.Join(workDir, ".github", "hooks", "gascity.json")
	if _, err := os.Stat(hookFile); err != nil {
		t.Fatalf("the tick did not stage Copilot's hook file before the fingerprint: %v", err)
	}
	instructions, err := os.ReadFile(instructionsFile)
	if err != nil {
		t.Fatalf("read staged instructions: %v", err)
	}
	if bare := bareGCInlineCommands(instructions); len(bare) > 0 {
		t.Fatalf("staged instructions still contain bare gc inline commands: %q\n%s", bare, instructions)
	}
	if !strings.Contains(string(instructions), "`gc.exe bd update <id> --claim`") {
		t.Fatalf("staged instructions missing gc.exe command:\n%s", instructions)
	}
	fingerprint := func() map[string]string {
		out := map[string]string{}
		for _, entry := range stageHookFiles(nil, cityDir, workDir, []string{"copilot"}) {
			out[entry.RelDst] = entry.ContentHash
		}
		return out
	}
	before := fingerprint()

	// Session start stages the same overlay through the non-skipping path.
	if err := runtime.StageProviderOverlayDir(overlayDir, workDir, []string{"copilot"}, io.Discard); err != nil {
		t.Fatalf("session-start staging: %v", err)
	}
	after := fingerprint()
	if len(after) != len(before) {
		t.Fatalf("session start changed the fingerprinted hook files: before %v, after %v", before, after)
	}
	for rel, hash := range before {
		if after[rel] != hash {
			t.Errorf("session start changed %s: %s -> %s", rel, hash, after[rel])
		}
	}
}
