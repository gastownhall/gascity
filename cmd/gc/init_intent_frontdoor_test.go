package main

import (
	"bytes"
	"fmt"
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

// TestGcInitSelectorRefusalLeavesDestinationUntouchedAcrossFrontDoors is the
// no-mutation contract for every init entry point that can accept a fresh
// config. Selector/backend incompatibilities must be rejected before the
// runtime scaffold, hooks, or copied files are written. In particular, this
// covers the two failure classes that used to be easy to miss: a non-bd
// provider selected by the source config and a doltlite backend selected by
// the source config (or runtime environment).
func TestGcInitSelectorRefusalLeavesDestinationUntouchedAcrossFrontDoors(t *testing.T) {
	tests := []struct {
		name       string
		frontDoor  string
		provider   string
		backend    string
		envBackend string
	}{
		{name: "template non-bd provider", frontDoor: "template", provider: "file"},
		{name: "template doltlite backend", frontDoor: "template", envBackend: "doltlite"},
		{name: "file non-bd provider", frontDoor: "file", provider: "file"},
		{name: "file doltlite backend", frontDoor: "file", backend: "doltlite"},
		{name: "from non-bd provider", frontDoor: "from", provider: "file"},
		{name: "from doltlite backend", frontDoor: "from", backend: "doltlite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Keep the process-level selector overrides deterministic even when a
			// developer's shell has GC_BEADS configured.
			t.Setenv("GC_BEADS", "")
			t.Setenv("GC_BEADS_BACKEND", tt.envBackend)
			destination := seedInitSnapshotDestination(t)
			before := snapshotInitTree(t, destination)

			var source string
			switch tt.frontDoor {
			case "template":
				// The template path's fresh config has the canonical bd provider;
				// use GC_BEADS for the non-bd case and GC_BEADS_BACKEND for the
				// doltlite case.
				if tt.provider != "" {
					t.Setenv("GC_BEADS", tt.provider)
				}
			case "file", "from":
				var srcDir string
				if tt.frontDoor == "file" {
					srcDir = t.TempDir()
					source = filepath.Join(srcDir, "city.toml")
				} else {
					srcDir = t.TempDir()
					source = srcDir
				}
				beads := ""
				if tt.provider != "" {
					beads = fmt.Sprintf("\n[beads]\nprovider = %q\n", tt.provider)
				} else if tt.backend != "" {
					beads = fmt.Sprintf("\n[beads]\nbackend = %q\n", tt.backend)
				}
				cityTOML := []byte("[workspace]\nname = \"source\"\n" + beads)
				if tt.frontDoor == "file" {
					if err := os.WriteFile(source, cityTOML, 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(srcDir, "city.toml"), cityTOML, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			default:
				t.Fatalf("unknown front door %q", tt.frontDoor)
			}

			args := []string{
				"--skip-provider-readiness",
				"--no-start",
				"--beads-transport", "proxied",
				"--beads-target", "local",
			}
			switch tt.frontDoor {
			case "template":
				args = append(args, "--template", "gascity", "--default-provider", "claude", destination)
			case "file":
				args = append(args, "--file", source, destination)
			case "from":
				args = append(args, "--from", source, destination)
			}

			var stdout, stderr bytes.Buffer
			cmd := newInitCmd(&stdout, &stderr)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatalf("gc init selector refusal returned nil; stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			if got := snapshotInitTree(t, destination); !reflect.DeepEqual(got, before) {
				t.Fatalf("destination mutated on %s selector refusal:\n got %#v\nwant %#v", tt.frontDoor, got, before)
			}
		})
	}
}

// seedInitSnapshotDestination creates both nested directories and files so a
// snapshot detects accidental mkdir/write/chmod operations, not just content
// changes to one sentinel file.
func seedInitSnapshotDestination(t *testing.T) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(filepath.Join(destination, "existing", "nested"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "sentinel"), []byte("keep\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing", "nested", "config"), []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return destination
}

func snapshotInitTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if rel == "." {
				return nil
			}
			files[rel+string(filepath.Separator)] = []byte(fmt.Sprintf("dir:%#o", info.Mode().Perm()))
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entry := []byte(fmt.Sprintf("file:%#o:", info.Mode().Perm()))
		files[rel] = append(entry, data...)
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
