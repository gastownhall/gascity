package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludes_RejectsDeprecatedPackOrderDirectory(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, filepath.Join(packDir, "orders", "health-check"), "order.toml", `
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	cityDir := filepath.Join(dir, "city")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err == nil {
		t.Fatal("LoadWithIncludes succeeded, want hard error for deprecated order directory")
	}
	if !strings.Contains(err.Error(), "rename to orders/health-check.toml") {
		t.Fatalf("error = %v, want rename guidance", err)
	}
}

func TestLoadWithIncludes_DoesNotWarnForFlatPackOrders(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")
	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "mypk"
schema = 1
`)
	writeTestFile(t, filepath.Join(packDir, "orders"), "health-check.toml", `
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	cityDir := filepath.Join(dir, "city")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	if _, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml")); err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
}
