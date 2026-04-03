//go:build acceptance_a

// Pack materialization acceptance tests.
//
// Verifies that materialized packs have correct permissions (scripts
// executable) and contain all expected artifacts.
package acceptance_test

import (
	"os"
	"path/filepath"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestGastownPackMaterialization groups tests that verify materialized gastown
// pack properties (permissions, completeness), sharing a single gc init call.
func TestGastownPackMaterialization(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("GastownScriptsExecutable", func(t *testing.T) {
		scriptsDir := filepath.Join(c.Dir, "packs", "gastown", "scripts")
		entries, err := os.ReadDir(scriptsDir)
		if err != nil {
			t.Fatalf("reading gastown scripts dir: %v", err)
		}
		count := 0
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".sh" {
				continue
			}
			count++
			info, err := e.Info()
			if err != nil {
				t.Errorf("stat %s: %v", e.Name(), err)
				continue
			}
			if info.Mode()&0o111 == 0 {
				t.Errorf("packs/gastown/scripts/%s is not executable (mode %o)", e.Name(), info.Mode())
			}
		}
		if count == 0 {
			t.Fatal("no .sh scripts found in packs/gastown/scripts/")
		}
	})

	t.Run("Completeness", func(t *testing.T) {
		expected := []string{
			"packs/gastown/pack.toml",
			"packs/gastown/prompts",
			"packs/gastown/formulas",
			"packs/gastown/scripts",
			"packs/gastown/overlays",
			"packs/gastown/commands",
			"packs/maintenance/pack.toml",
			"packs/maintenance/prompts",
			"packs/maintenance/formulas",
		}
		for _, e := range expected {
			if !c.HasFile(e) {
				t.Errorf("missing: %s", e)
			}
		}
	})

	t.Run("MaintenanceScriptsExecutable", func(t *testing.T) {
		scriptsDir := filepath.Join(c.Dir, "packs", "maintenance", "scripts")
		entries, err := os.ReadDir(scriptsDir)
		if err != nil {
			// Not all maintenance packs have a scripts dir.
			t.Skip("no maintenance scripts dir")
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".sh" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				t.Errorf("stat %s: %v", e.Name(), err)
				continue
			}
			if info.Mode()&0o111 == 0 {
				t.Errorf("packs/maintenance/scripts/%s is not executable (mode %o)", e.Name(), info.Mode())
			}
		}
	})
}
