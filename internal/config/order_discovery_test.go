package config

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludes_WarnsForDeprecatedPackOrderDirectory(t *testing.T) {
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
gate = "cron"
schedule = "*/5 * * * *"
`)

	cityDir := filepath.Join(dir, "city")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	logs := captureConfigLogs(t, func() {
		if _, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml")); err != nil {
			t.Fatalf("LoadWithIncludes: %v", err)
		}
	})

	if !strings.Contains(logs, "rename to orders/health-check.order.toml") {
		t.Fatalf("logs = %q, want rename warning", logs)
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
	writeTestFile(t, filepath.Join(packDir, "orders"), "health-check.order.toml", `
[order]
formula = "health-check"
gate = "cron"
schedule = "*/5 * * * *"
`)

	cityDir := filepath.Join(dir, "city")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../mypk"]
`)

	logs := captureConfigLogs(t, func() {
		if _, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml")); err != nil {
			t.Fatalf("LoadWithIncludes: %v", err)
		}
	})

	if strings.Contains(logs, "health-check.order.toml") {
		t.Fatalf("logs = %q, want no order deprecation warning", logs)
	}
}

func captureConfigLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	origPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
	}()

	fn()
	return buf.String()
}
