package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestWorkflowsFailHaltsDefaultsOff(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.FailHaltsEnabled() {
		t.Fatal("FailHaltsEnabled() = true with no [workflows] table, want false")
	}
	var nilCity *City
	if nilCity.FailHaltsEnabled() {
		t.Fatal("a nil config must read fail_halts as off")
	}
}

func TestWorkflowsFailHaltsFromAFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["workflows.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/workflows.toml"] = []byte(`
[workflows]
fail_halts = true
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if !cfg.FailHaltsEnabled() {
		t.Fatal("FailHaltsEnabled() = false, want true from the [workflows] fragment")
	}
}
