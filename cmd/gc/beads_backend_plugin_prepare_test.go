package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPrepareBeadsBackendPluginForInitRunsDeclaredPackCommand(t *testing.T) {
	cityPath := t.TempDir()
	packDir := filepath.Join(cityPath, "packs", "bd-gc-dl")
	commandDir := filepath.Join(packDir, "commands", "build")
	if err := os.MkdirAll(commandDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(cityPath, "prepare.out")
	runScript := filepath.Join(commandDir, "run.sh")
	script := "#!/bin/sh\n" +
		"printf 'args=%s\\npack_state=%s\\npack_dir=%s\\npack_name=%s\\n' \"$*\" \"$GC_PACK_STATE_DIR\" \"$GC_PACK_DIR\" \"$GC_PACK_NAME\" > " + shellQuotePath(outPath) + "\n"
	if err := os.WriteFile(runScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Beads:     config.BeadsConfig{Provider: "plugin", Backend: "doltlite"},
		BackendPlugins: []config.DiscoveredBackendPlugin{{
			Backend:        "doltlite",
			PrepareCommand: []string{"build", "backend", "--install"},
			PackName:       "bd-gc-dl",
			PackDir:        packDir,
		}},
		PackCommands: []config.DiscoveredCommand{{
			Command:     []string{"build"},
			RunScript:   runScript,
			SourceDir:   commandDir,
			PackDir:     packDir,
			PackName:    "bd-gc-dl",
			BindingName: "bd-gc-dl",
		}},
	}

	var stdout, stderr bytes.Buffer
	if err := prepareBeadsBackendPluginForInit(cityPath, cfg, &stdout, &stderr); err != nil {
		t.Fatalf("prepareBeadsBackendPluginForInit: %v\nstderr:\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(prepare.out): %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"args=backend --install",
		"pack_state=" + citylayout.PackStateDir(cityPath, "bd-gc-dl"),
		"pack_dir=" + packDir,
		"pack_name=bd-gc-dl",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("prepare output missing %q:\n%s", want, text)
		}
	}
}
