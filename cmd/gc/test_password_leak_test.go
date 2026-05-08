package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBdRuntimeEnvDoesNotTreatCityEnvPasswordAsCanonicalAuth(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace.env]
GC_DOLT_PASSWORD = "secret_from_toml"
BEADS_DOLT_PASSWORD = "beads_secret_from_toml"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`
issue_prefix: gc
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: ext-db.example.com
dolt.port: 3307
dolt.user: agent
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := bdRuntimeEnv(cityPath)
	if got := env["GC_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want empty when only city.toml workspace env supplies auth", got)
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want empty when only city.toml workspace env supplies auth", got)
	}
}
