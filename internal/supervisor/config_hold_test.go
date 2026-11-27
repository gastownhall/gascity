package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

// AC-7: supervisor.toml's hold_cities_after_unclean_exit decodes into the
// [supervisor] section, and is off when unset.
func TestLoadConfigHoldCitiesAfterUncleanExit(t *testing.T) {
	for raw, want := range map[string]bool{
		"[supervisor]\nhold_cities_after_unclean_exit = true\n": true,
		"[supervisor]\nport = 8372\n":                           false,
	} {
		path := filepath.Join(t.TempDir(), "supervisor.toml")
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%q): %v", raw, err)
		}
		if got := cfg.Supervisor.HoldCitiesAfterUncleanExit; got != want {
			t.Fatalf("LoadConfig(%q): HoldCitiesAfterUncleanExit = %t, want %t", raw, got, want)
		}
	}
}
