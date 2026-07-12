package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeSQLiteBackendUsesStockBdInit(t *testing.T) {
	cityPath := t.TempDir()
	writeNativeBackendTestCity(t, cityPath, "sqlite", `sqlite_path = "city.db"`, `
resource = "file"
namespace_from = "prefix"
`)
	capture := installNativeBackendTestBD(t)

	handled, err := initBeadsViaNativeBackend(cityPath, cityPath, "hq")
	if err != nil {
		t.Fatalf("initBeadsViaNativeBackend: %v", err)
	}
	if !handled {
		t.Fatal("initBeadsViaNativeBackend handled = false")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	for _, want := range []string{"init", "--backend=sqlite", "--prefix=hq", "--sqlite-path=city.db", "--init-if-missing"} {
		if !strings.Contains(args, want) {
			t.Fatalf("bd args missing %q: %s", want, args)
		}
	}
	if got := beadsProvider(cityPath); got != "bd" {
		t.Fatalf("beadsProvider = %q, want direct bd", got)
	}
	cityConfigPath := filepath.Join(cityPath, "city.toml")
	cityConfig, err := os.ReadFile(cityConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cityConfig = []byte(strings.Replace(string(cityConfig), `provider = "bd"`, `provider = "plugin"`, 1))
	if err := os.WriteFile(cityConfigPath, cityConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := beadsProvider(cityPath); got != "bd" {
		t.Fatalf("registry-selected native beadsProvider = %q, want direct bd", got)
	}
}

func TestNativePostgresBackendUsesSchemaAndCredential(t *testing.T) {
	cityPath := t.TempDir()
	writeNativeBackendTestCity(t, cityPath, "postgres", `postgres_url = "postgres://operator@db.example.com:5432/beads?sslmode=require"`, `
resource = "schema"
namespace_from = "city_or_prefix"
inherits_city_connection = true
`)
	t.Setenv("BEADS_PG_PASSWORD", "test-secret")
	capture := installNativeBackendTestBD(t)

	handled, err := initBeadsViaNativeBackend(cityPath, cityPath, "gc")
	if err != nil {
		t.Fatalf("initBeadsViaNativeBackend: %v", err)
	}
	if !handled {
		t.Fatal("initBeadsViaNativeBackend handled = false")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	for _, want := range []string{"--backend=postgres", "--pg-schema=hq", "operator:test-secret@db.example.com"} {
		if !strings.Contains(args, want) {
			t.Fatalf("bd args missing %q: %s", want, args)
		}
	}
}

func TestNativePostgresBackendProvisionsLocalDatabaseWhenURLIsAbsent(t *testing.T) {
	cityPath := t.TempDir()
	writeNativeBackendTestCity(t, cityPath, "postgres", "", `
resource = "schema"
namespace_from = "city_or_prefix"
inherits_city_connection = true
`)
	t.Setenv("GC_POSTGRES_URL", "")
	t.Setenv("BEADS_POSTGRES_URL", "")
	t.Setenv("GC_POSTGRES_PASSWORD", "")
	t.Setenv("BEADS_PG_PASSWORD", "")
	originalProvisioner := nativePostgresProvisioner
	t.Cleanup(func() { nativePostgresProvisioner = originalProvisioner })
	called := false
	nativePostgresProvisioner = func(gotCityPath, schema string) (string, error) {
		called = true
		if gotCityPath != cityPath || schema != "hq" {
			t.Fatalf("provision args = (%q, %q), want (%q, hq)", gotCityPath, schema, cityPath)
		}
		if err := writeNativePostgresPassword(cityPath, "generated-secret"); err != nil {
			t.Fatal(err)
		}
		return "postgres://gc_native@127.0.0.1:5432/gc_native?sslmode=disable", nil
	}
	capture := installNativeBackendTestBD(t)

	handled, err := initBeadsViaNativeBackend(cityPath, cityPath, "gc")
	if err != nil {
		t.Fatalf("initBeadsViaNativeBackend: %v", err)
	}
	if !handled || !called {
		t.Fatalf("handled = %v, provisioner called = %v", handled, called)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	for _, want := range []string{"--backend=postgres", "--pg-schema=hq", "gc_native:generated-secret@127.0.0.1"} {
		if !strings.Contains(args, want) {
			t.Fatalf("bd args missing %q: %s", want, args)
		}
	}
	info, err := os.Stat(filepath.Join(cityPath, ".beads", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %#o, want 0600", info.Mode().Perm())
	}
}

func TestNativeBackendRuntimeEnvUsesSelfDescribingMetadata(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		cityPath := t.TempDir()
		writeNativeBackendTestCity(t, cityPath, "sqlite", "", `resource = "file"`)
		if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"sqlite","sqlite_path":"beads.db"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		env, err := bdRuntimeEnvWithError(cityPath)
		if err != nil {
			t.Fatal(err)
		}
		if env["BEADS_BACKEND"] != "sqlite" || env["GC_BEADS_BACKEND"] != "sqlite" {
			t.Fatalf("sqlite env = %+v", env)
		}
	})

	t.Run("postgres", func(t *testing.T) {
		cityPath := t.TempDir()
		writeNativeBackendTestCity(t, cityPath, "postgres", "", `resource = "schema"`)
		if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"backend":"postgres","postgres_dsn":"postgres://operator@db.example.com:5432/beads","postgres_schema":"hq"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BEADS_PG_PASSWORD", "runtime-secret")
		env, err := bdRuntimeEnvWithError(cityPath)
		if err != nil {
			t.Fatal(err)
		}
		if env["BEADS_PG_PASSWORD"] != "runtime-secret" || env["BEADS_POSTGRES_PASSWORD"] != "runtime-secret" {
			t.Fatalf("postgres password projection missing: %+v", env)
		}
	})
}

func writeNativeBackendTestCity(t *testing.T, cityPath, backend, beadsExtra, scope string) {
	t.Helper()
	city := "[workspace]\nname = \"native-test\"\nprefix = \"gc\"\n\n[beads]\nprovider = \"bd\"\nbackend = \"" + backend + "\"\n" + beadsExtra + "\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(city), 0o644); err != nil {
		t.Fatal(err)
	}
	pack := "[pack]\nname = \"native-" + backend + "\"\nschema = 2\n\n[[backend_plugins]]\nbackend = \"" + backend + "\"\nkind = \"native\"\ndriver = \"" + backend + "\"\n\n[backend_plugins.scope]\nmodel = \"per_scope\"\nmetadata_owner = \"bd\"\n" + scope
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(pack), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installNativeBackendTestBD(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	capture := filepath.Join(binDir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NATIVE_BD_CAPTURE\"\nmkdir -p \"$BEADS_DIR\"\nprintf '%s\\n' '{\"backend\":\"dolt\",\"dolt_mode\":\"server\",\"dolt_database\":\"hq\"}' > \"$BEADS_DIR/metadata.json\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NATIVE_BD_CAPTURE", capture)
	return capture
}
