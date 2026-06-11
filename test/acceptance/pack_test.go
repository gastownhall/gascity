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
// The gastown pack arrives via the pinned public import, so its content is
// materialized into the user-global repo cache rather than a city-local
// packs/ directory.
func TestGastownPackMaterialization(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))
	packDir := gastownCachePackDir(t, c)

	t.Run("GastownScriptsExecutable", func(t *testing.T) {
		scriptsDir := filepath.Join(packDir, "assets", "scripts")
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
				t.Errorf("cached gastown assets/scripts/%s is not executable (mode %o)", e.Name(), info.Mode())
			}
		}
		if count == 0 {
			t.Fatal("no .sh scripts found in cached gastown assets/scripts/")
		}
	})

	t.Run("Completeness", func(t *testing.T) {
		// City-side wiring: the import pin and lock entry replace the old
		// city-local packs/gastown materialization.
		for _, rel := range []string{"pack.toml", "packs.lock"} {
			if !c.HasFile(rel) {
				t.Errorf("missing: %s", rel)
			}
		}
		// Cached pack content.
		expected := []string{
			"pack.toml",
			"agents",
			"template-fragments",
			"formulas",
			filepath.Join("assets", "scripts"),
			"commands",
		}
		for _, rel := range expected {
			if _, err := os.Stat(filepath.Join(packDir, rel)); err != nil {
				t.Errorf("missing cached pack artifact %s: %v", rel, err)
			}
		}
	})

	t.Run("CoreScriptsExecutable", func(t *testing.T) {
		scriptsDir := filepath.Join(c.Dir, ".gc", "system", "packs", "core", "assets", "scripts")
		entries, err := os.ReadDir(scriptsDir)
		if err != nil {
			t.Fatalf("reading core pack scripts dir: %v", err)
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
				t.Errorf("core pack script %s is not executable (mode %o)", e.Name(), info.Mode())
			}
		}
	})
}
