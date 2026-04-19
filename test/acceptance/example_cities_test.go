//go:build acceptance_a

// Example city acceptance tests.
//
// These verify that every example city shipped with the project can be
// initialized via gc init --from, passes config validation, and has
// the expected pack artifacts. This catches broken examples early —
// a user's first experience with gc is often "gc init --from gastown".
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestExampleInit_AllCities_Succeed is a table-driven test that verifies
// every example city with a city.toml can be initialized without error.
func TestExampleInit_AllCities_Succeed(t *testing.T) {
	examplesDir := helpers.ExamplesDir()
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("reading examples dir: %v", err)
	}

	var cities []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(examplesDir, e.Name(), "city.toml")); err == nil {
			cities = append(cities, e.Name())
		}
	}

	if len(cities) == 0 {
		t.Fatal("no example cities found")
	}

	for _, name := range cities {
		t.Run(name, func(t *testing.T) {
			c := helpers.NewCity(t, testEnv)
			c.InitFrom(filepath.Join(examplesDir, name))

			if !c.HasFile("city.toml") {
				t.Fatal("city.toml not created")
			}
			if !c.HasFile(".gc") {
				t.Fatal(".gc/ scaffold not created")
			}
		})
	}
}

// TestExampleValidate_AllCities_PassValidation verifies that every example
// city's config passes gc config show --validate after initialization.
func TestExampleValidate_AllCities_PassValidation(t *testing.T) {
	examplesDir := helpers.ExamplesDir()
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("reading examples dir: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(examplesDir, e.Name(), "city.toml")); err != nil {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			c := helpers.NewCity(t, testEnv)
			c.InitFrom(filepath.Join(examplesDir, name))

			out, err := c.GC("config", "show", "--validate")
			if err != nil {
				t.Fatalf("config validate failed for %s: %v\n%s", name, err, out)
			}
			if !strings.Contains(out, "Config valid.") {
				t.Errorf("expected 'Config valid.' for %s, got:\n%s", name, out)
			}
		})
	}
}

// TestExamplePacks_PackArtifacts groups tests that verify materialized pack
// artifacts for specific example cities, sharing one init per city.
func TestExamplePacks_PackArtifacts(t *testing.T) {
	t.Run("Gastown", func(t *testing.T) {
		c := helpers.NewCity(t, testEnv)
		c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

		expected := []string{
			"packs/gastown/pack.toml",
			"packs/gastown/agents",
			"packs/gastown/template-fragments",
			"packs/gastown/formulas",
			"packs/gastown/assets/scripts",
			"packs/maintenance/pack.toml",
			"packs/maintenance/agents",
			"packs/maintenance/formulas",
		}
		for _, rel := range expected {
			if !c.HasFile(rel) {
				t.Errorf("missing expected artifact: %s", rel)
			}
		}
	})

	t.Run("Hyperscale", func(t *testing.T) {
		c := helpers.NewCity(t, testEnv)
		c.InitFrom(filepath.Join(helpers.ExamplesDir(), "hyperscale"))

		expected := []string{
			"packs/hyperscale/pack.toml",
			"packs/hyperscale/agents",
			"packs/hyperscale/assets/scripts",
		}
		for _, rel := range expected {
			if !c.HasFile(rel) {
				t.Errorf("missing expected artifact: %s", rel)
			}
		}
	})
}

// TestExampleDoctor_AllCities_RunWithoutCrash verifies that gc doctor
// runs without crashing on every example city (it may report warnings
// for missing infrastructure, but should never panic).
func TestExampleDoctor_AllCities_RunWithoutCrash(t *testing.T) {
	examplesDir := helpers.ExamplesDir()
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("reading examples dir: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(examplesDir, e.Name(), "city.toml")); err != nil {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			c := helpers.NewCity(t, testEnv)
			c.InitFrom(filepath.Join(examplesDir, name))

			// Doctor may return non-zero for warnings, but should not crash.
			out, _ := c.GC("doctor")
			if out == "" {
				t.Error("gc doctor produced no output")
			}
		})
	}
}
