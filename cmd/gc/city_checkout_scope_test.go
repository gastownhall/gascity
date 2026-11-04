package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// A checkout or worktree of a city repo carries city.toml but not the
// machine-local .gc/site.toml, so every declared rig is unbound (Path "").
// normalizedRigConfig maps an empty path onto the city root, so each unbound
// rig was reconciled AS the city root: its prefix overwrote the city's own
// .beads/config.yaml, and the last declared rig won (gm-u94m5c: "hc", from
// hold-court, the last [[rigs]] entry in gc-management's city.toml).
func TestSyncConfiguredDoltPortFilesUnboundRigsLeaveCityPrefixAlone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cityMetadata bool
	}{
		{name: "initialized city", cityMetadata: true},
		{name: "fresh checkout", cityMetadata: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if tc.cityMetadata {
				writeScopeMetadata(t, cityDir, map[string]string{
					"database":      "dolt",
					"backend":       "dolt",
					"dolt_mode":     "server",
					"dolt_database": "gm",
				})
			}
			if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}

			rigs := []config.Rig{{Name: "gascity"}, {Name: "hold-court"}}
			requireSyncConfiguredDoltPortFiles(t, cityDir, "bd", config.DoltConfig{}, "gm", rigs)

			cfg := string(mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml")))
			for _, needle := range []string{"issue_prefix: gm", "issue-prefix: gm"} {
				if !strings.Contains(cfg, needle) {
					t.Fatalf("city config missing %q after reconciling unbound rigs:\n%s", needle, cfg)
				}
			}
		})
	}
}

// gc bd, run against a directory that merely carries city.toml (a checkout or
// worktree of a city repo, reached by cwd discovery or by --city "$(pwd)"),
// found no managed Dolt and "recovered" one: health failed, recover ran, and a
// managed dolt sql-server plus its scope watchdog started in that directory,
// orphaned under systemd --user (gm-u94m5c). Recovery restarts a server a city
// already had; it must never be a city's first start.
func TestBdRuntimeEnvDoesNotRecoverManagedDoltForNeverInitializedCity(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)

	opsFile := filepath.Join(t.TempDir(), "provider-ops.log")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptBody := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> %q
case "$1" in
  health)
    echo unhealthy >&2
    exit 1
    ;;
esac
exit 0
`, opsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "bd")

	_, _ = bdRuntimeEnvWithError(cityPath)

	ops, err := os.ReadFile(opsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read provider ops: %v", err)
	}
	if strings.Contains(string(ops), "recover") {
		t.Fatalf("provider ops = %q: recovery started a managed Dolt for a city whose bead store was never initialized (no .beads/metadata.json)", strings.TrimSpace(string(ops)))
	}
}
