package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestUnnamedCityLaunchEnvUsesDirectoryBasename pins GC_CITY_NAME for a city
// with no registration, no site-bound name and no [workspace].name to the city
// directory basename: the same value pack commands get from
// runDiscoveredCommand, and never the supervisor's ambient value.
func TestUnnamedCityLaunchEnvUsesDirectoryBasename(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_CITY_NAME", "neighbor-city")

	cityDir := filepath.Join(t.TempDir(), "basename-city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Base(cityDir)

	t.Run("worker session env", func(t *testing.T) {
		env := resolvedWorkerSessionEnvWithConfig(cityDir, &config.City{}, &config.ResolvedProvider{Name: "claude"})
		if got := env["GC_CITY_NAME"]; got != want {
			t.Fatalf("SessionEnv[GC_CITY_NAME] = %q, want directory basename %q", got, want)
		}
	})

	t.Run("worker session env with nil cfg", func(t *testing.T) {
		env := resolvedWorkerSessionEnvWithConfig(cityDir, nil, &config.ResolvedProvider{Name: "claude"})
		if got := env["GC_CITY_NAME"]; got != want {
			t.Fatalf("SessionEnv[GC_CITY_NAME] = %q, want directory basename %q", got, want)
		}
	})

	t.Run("exec order env with nil cfg", func(t *testing.T) {
		target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}
		a := orders.Order{Name: "unnamed-order", Trigger: "cooldown", Interval: "1m", Exec: "true"}
		env, err := orderExecEnvWithError(cityDir, nil, target, a, nil)
		if err != nil {
			t.Fatalf("orderExecEnvWithError() error = %v", err)
		}
		assertOrderProcessSeesCityName(t, env, want)
	})
}
