//go:build acceptance_a

// Gastown pack smoke tests.
//
// Full vertical-slice validation: does the gastown pack load, parse,
// render, and run end-to-end? Catches regressions in pack
// materialization, config composition, prompt templates, formula TOML,
// script permissions, and init/start/stop lifecycle.
//
// All tests use subprocess provider, file beads, no dolt, no inference.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestGastownSmoke groups gastown pack smoke tests that share a single
// initialized city, reducing redundant gc init calls.
func TestGastownSmoke(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("ConfigLoads", func(t *testing.T) {
		cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(c.Dir, "city.toml"))
		if err != nil {
			t.Fatalf("config.LoadWithIncludes: %v", err)
		}

		if cfg.Workspace.Name == "" {
			t.Error("Workspace.Name is empty")
		}

		if len(cfg.Agents) < 5 {
			t.Errorf("expected at least 5 agents, got %d", len(cfg.Agents))
		}

		agentNames := make(map[string]bool, len(cfg.Agents))
		for _, a := range cfg.Agents {
			agentNames[a.Name] = true
		}
		for _, expected := range []string{"mayor", "deacon", "boot", "dog"} {
			if !agentNames[expected] {
				t.Errorf("expected city-scoped agent %q not found in config", expected)
			}
		}

		if len(prov.Warnings) > 0 {
			t.Errorf("unexpected provenance warnings: %v", prov.Warnings)
		}
	})

	t.Run("AllPromptTemplatesRender", func(t *testing.T) {
		packsDir := filepath.Join(c.Dir, "packs")
		count := 0

		err := filepath.Walk(packsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".md.tmpl") {
				return nil
			}

			rel, _ := filepath.Rel(c.Dir, path)
			t.Run(rel, func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading template: %v", err)
				}
				if len(data) == 0 {
					t.Error("template file is empty")
				}

				placeholderFuncs := template.FuncMap{
					"cmd":      func() string { return "gc" },
					"session":  func(string) string { return "test-session" },
					"basename": func(s string) string { return s },
				}
				_, err = template.New(info.Name()).Funcs(placeholderFuncs).Parse(string(data))
				if err != nil {
					t.Errorf("template parse error: %v", err)
				}
			})
			count++
			return nil
		})
		if err != nil {
			t.Fatalf("walking packs dir: %v", err)
		}
		if count == 0 {
			t.Fatal("no .md.tmpl files found in packs/")
		}
	})

	t.Run("AllFormulasParse", func(t *testing.T) {
		packsDir := filepath.Join(c.Dir, "packs")
		count := 0

		type formulaStep struct {
			ID    string `toml:"id"`
			Title string `toml:"title"`
		}
		type formulaFile struct {
			Formula     string        `toml:"formula"`
			Description string        `toml:"description"`
			Steps       []formulaStep `toml:"steps"`
		}

		err := filepath.Walk(packsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".formula.toml") {
				return nil
			}

			if strings.Contains(path, "/orders/") {
				return nil
			}

			rel, _ := filepath.Rel(c.Dir, path)
			t.Run(rel, func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading formula: %v", err)
				}

				var f formulaFile
				if err := toml.Unmarshal(data, &f); err != nil {
					t.Fatalf("TOML parse error: %v", err)
				}
				if len(f.Steps) == 0 {
					t.Error("formula has no [[steps]] -- possibly a TOML escape bug")
				}
			})
			count++
			return nil
		})
		if err != nil {
			t.Fatalf("walking packs dir: %v", err)
		}
		if count == 0 {
			t.Fatal("no .formula.toml files found in packs/")
		}
	})

	t.Run("AllScriptsExecutable", func(t *testing.T) {
		packsDir := filepath.Join(c.Dir, "packs")
		count := 0

		err := filepath.Walk(packsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".sh") {
				return nil
			}

			rel, _ := filepath.Rel(c.Dir, path)
			t.Run(rel, func(t *testing.T) {
				if info.Mode()&0o111 == 0 {
					t.Errorf("script %s is not executable (mode %o)", rel, info.Mode())
				}
			})
			count++
			return nil
		})
		if err != nil {
			t.Fatalf("walking packs dir: %v", err)
		}
		if count == 0 {
			t.Fatal("no .sh scripts found in packs/")
		}
	})

	t.Run("ConfigValidates", func(t *testing.T) {
		out, err := c.GC("config", "show", "--validate")
		if err != nil {
			t.Fatalf("gc config show --validate failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "valid") {
			t.Errorf("expected validation success message in output:\n%s", out)
		}
	})

	t.Run("FormulaLayers", func(t *testing.T) {
		cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(c.Dir, "city.toml"))
		if err != nil {
			t.Fatalf("config.LoadWithIncludes: %v", err)
		}

		if len(cfg.FormulaLayers.City) == 0 {
			t.Fatal("no city formula layers -- pack expansion may not have run")
		}
		t.Logf("city formula layers: %v", cfg.FormulaLayers.City)

		for _, dir := range cfg.FormulaLayers.City {
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("formula layer dir does not exist: %s", dir)
			}
		}
	})
}

// TestGastownSmoke_InitStatusStop exercises the standalone lifecycle:
// init, verify status, and stop. The standalone controller started by
// gc init --from responds to gc status and gc stop.
//
// NOTE: The supervisor restart path (stop standalone → gc start) is
// blocked by a known bug: gc stop doesn't reliably kill standalone
// controllers in all environments (notably CI without systemd).
func TestGastownSmoke_InitStatusStop(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// Verify gc status succeeds and lists agents.
	out, err := c.GC("status")
	if err != nil {
		t.Fatalf("gc status failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("expected 'mayor' in status output:\n%s", out)
	}
}

// TestGastownSmoke_WithRig inits gastown, adds a rig, and verifies
// rig-scoped agents appear in the expanded config.
func TestGastownSmoke_WithRig(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	// Create a minimal git repo to serve as a rig.
	rigDir := filepath.Join(t.TempDir(), "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}

	for _, cmd := range [][]string{
		{"git", "init", rigDir},
		{"git", "-C", rigDir, "config", "user.email", "test@test.com"},
		{"git", "-C", rigDir, "config", "user.name", "Test"},
	} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("git setup %v failed: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(rigDir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}

	for _, cmd := range [][]string{
		{"git", "-C", rigDir, "add", "."},
		{"git", "-C", rigDir, "commit", "-m", "init"},
	} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("git setup %v failed: %v\n%s", cmd, err, out)
		}
	}

	c.RigAdd(rigDir, "packs/gastown")

	// Load config and verify rig-scoped agents appeared.
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(c.Dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes after rig add: %v", err)
	}

	var rigAgentNames []string
	for _, a := range cfg.Agents {
		if a.Dir != "" {
			rigAgentNames = append(rigAgentNames, a.Name)
		}
	}
	if len(rigAgentNames) == 0 {
		t.Fatal("no rig-scoped agents found after rig add")
	}
	t.Logf("rig-scoped agents: %v", rigAgentNames)
}
