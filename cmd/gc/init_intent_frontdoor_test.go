package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestInitSelectorDefaultsToProxiedLocal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{}).applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "proxied-server" || cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
		t.Fatalf("got dolt config %+v, want proxied local", cfg.Dolt)
	}
}

func TestInitSelectorDirectLocal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{Transport: "direct", Target: "local"}).applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "server" {
		t.Fatalf("mode = %q, want server", cfg.Dolt.Mode)
	}
}

func TestInitSelectorProxiedExternal(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	opts := hostedDoltInitOptions{Transport: "proxied", Target: "external", Host: "db.example", Port: "3306", Database: "bd_proj", ProjectID: "proj"}
	if err := opts.applySelectorToCityConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dolt.Mode != "proxied-server" || cfg.Dolt.Host != "db.example" || cfg.Dolt.Port != 3306 {
		t.Fatalf("got dolt config %+v, want proxied external", cfg.Dolt)
	}
}

func TestInitSelectorAcceptsExecBDProvider(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	cfg.Beads.Provider = "exec:/opt/gc-beads-bd.sh"
	opts := hostedDoltInitOptions{Transport: "proxied", Target: "local"}
	if err := opts.validateRequest(cfg.Beads.Provider); err != nil {
		t.Fatalf("exec gc-beads-bd provider rejected during preflight: %v", err)
	}
	if err := opts.applySelectorToCityConfig(&cfg); err != nil {
		t.Fatalf("exec gc-beads-bd provider rejected: %v", err)
	}
	if cfg.Dolt.Mode != "proxied-server" {
		t.Fatalf("mode = %q, want proxied-server", cfg.Dolt.Mode)
	}
}

func TestGcInitSelectorRefusalLeavesDestinationUntouched(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_BACKEND", "")
	destination := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "sentinel"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotInitTree(t, destination)

	var stdout, stderr bytes.Buffer
	cmd := newInitCmd(&stdout, &stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"--template", "gascity",
		"--default-provider", "claude",
		"--skip-provider-readiness",
		"--no-start",
		"--beads-transport", "proxied",
		"--beads-target", "local",
		destination,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("gc init with file beads provider = nil error, want refusal; stderr=%s", stderr.String())
	}
	if got := snapshotInitTree(t, destination); !reflect.DeepEqual(got, before) {
		t.Fatalf("destination mutated on selector refusal:\n got %#v\nwant %#v", got, before)
	}
}

func snapshotInitTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = append([]byte(nil), data...)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return files
}

func TestInitSelectorRejectsIncompleteIntent(t *testing.T) {
	cfg := config.DefaultCity("fresh")
	if err := (hostedDoltInitOptions{Transport: "proxied"}).applySelectorToCityConfig(&cfg); err == nil {
		t.Fatal("incomplete selector accepted")
	}
}
