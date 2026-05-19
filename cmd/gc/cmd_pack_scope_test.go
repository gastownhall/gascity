package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestPackScopeContracts(t *testing.T) {
	type checkFunc func(t *testing.T, stdout, stderr string)

	tests := []struct {
		name    string
		setup   func(t *testing.T) []string
		want    int
		checks  []checkFunc
		stubbed bool
	}{
		{
			name: "no city or pack root rejects",
			setup: func(t *testing.T) []string {
				t.Helper()
				chdirPackScopeTest(t, t.TempDir())
				return []string{"pack", "add", "https://github.com/example/tools.git", "--name", "tools"}
			},
			want: 1,
			checks: []checkFunc{
				func(t *testing.T, _, stderr string) {
					t.Helper()
					if !strings.Contains(stderr, "could not find city or pack root") {
						t.Fatalf("stderr = %q, want no-root diagnostic", stderr)
					}
				},
			},
		},
		{
			name: "city target writes root pack import",
			setup: func(t *testing.T) []string {
				t.Helper()
				city := t.TempDir()
				source := packScopeLocalPack(t, "tools")
				t.Setenv("GC_CITY", city)
				t.Setenv("PACK_SCOPE_WANT_SOURCE", source)
				writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
				writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")
				return []string{"pack", "add", source, "--name", "tools"}
			},
			want:    0,
			stubbed: true,
			checks: []checkFunc{
				func(t *testing.T, stdout, _ string) {
					t.Helper()
					if !strings.Contains(stdout, "Added pack dependency") || strings.Contains(stdout, "Added import") {
						t.Fatalf("stdout = %q", stdout)
					}
				},
				func(t *testing.T, _, _ string) {
					t.Helper()
					city := os.Getenv("GC_CITY")
					cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city, "pack.toml"))
					if err != nil {
						t.Fatalf("Load(pack.toml): %v", err)
					}
					source := os.Getenv("PACK_SCOPE_WANT_SOURCE")
					if got := cfg.Imports["tools"].Source; got != source {
						t.Fatalf("pack imports.tools.source = %q, want %q", got, source)
					}
					cityData, err := os.ReadFile(filepath.Join(city, "city.toml"))
					if err != nil {
						t.Fatalf("ReadFile(city.toml): %v", err)
					}
					if strings.Contains(string(cityData), "[imports") {
						t.Fatalf("city.toml gained pack import scope:\n%s", cityData)
					}
				},
			},
		},
		{
			name: "standalone pack root writes local pack import",
			setup: func(t *testing.T) []string {
				t.Helper()
				root := t.TempDir()
				source := packScopeLocalPack(t, "tools")
				t.Setenv("PACK_SCOPE_ROOT", root)
				t.Setenv("PACK_SCOPE_WANT_SOURCE", source)
				writePackToml(t, root, "[pack]\nname = \"root\"\nschema = 1\n")
				chdirPackScopeTest(t, root)
				return []string{"pack", "add", source, "--name", "tools"}
			},
			want:    0,
			stubbed: true,
			checks: []checkFunc{
				func(t *testing.T, stdout, _ string) {
					t.Helper()
					if !strings.Contains(stdout, "Added pack dependency") {
						t.Fatalf("stdout = %q, want pack dependency message", stdout)
					}
				},
				func(t *testing.T, _, _ string) {
					t.Helper()
					root := os.Getenv("PACK_SCOPE_ROOT")
					cfg, err := config.Load(fsys.OSFS{}, filepath.Join(root, "pack.toml"))
					if err != nil {
						t.Fatalf("Load(pack.toml): %v", err)
					}
					source := os.Getenv("PACK_SCOPE_WANT_SOURCE")
					if got := cfg.Imports["tools"].Source; got != source {
						t.Fatalf("standalone pack import source = %q, want %q", got, source)
					}
				},
			},
		},
		{
			name: "registry selector stores concrete source durably",
			setup: func(t *testing.T) []string {
				t.Helper()
				home := t.TempDir()
				city := t.TempDir()
				t.Setenv("GC_HOME", home)
				t.Setenv("GC_CITY", city)
				writeCityToml(t, city, "[workspace]\nname = \"demo\"\n")
				writePackToml(t, city, "[pack]\nname = \"demo\"\nschema = 1\n")

				source, commit, hash := writeGitPackRepo(t)
				t.Setenv("PACK_SCOPE_WANT_SOURCE", source)
				catalogDir := writeRegistryCatalog(t, strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(packRegistryTestCatalog,
					`https://packages.example/lighthouse.git`, source),
					`0123456789abcdef0123456789abcdef01234567`, commit),
					`sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7`, hash))

				var stdout, stderr bytes.Buffer
				if code := doPackRegistryAdd("main", catalogDir, false, false, &stdout, &stderr); code != 0 {
					t.Fatalf("registry add code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				return []string{"pack", "add", "main:lighthouse", "--name", "lighthouse"}
			},
			want: 0,
			checks: []checkFunc{
				func(t *testing.T, stdout, _ string) {
					t.Helper()
					if !strings.Contains(stdout, "selected via registry main:lighthouse") {
						t.Fatalf("stdout missing registry selector provenance: %q", stdout)
					}
					if strings.Contains(stdout, "registry:main:lighthouse") {
						t.Fatalf("stdout leaked durable registry selector: %q", stdout)
					}
				},
				func(t *testing.T, _, _ string) {
					t.Helper()
					city := os.Getenv("GC_CITY")
					cfg, err := config.Load(fsys.OSFS{}, filepath.Join(city, "pack.toml"))
					if err != nil {
						t.Fatalf("Load(pack.toml): %v", err)
					}
					got := cfg.Imports["lighthouse"].Source
					source := os.Getenv("PACK_SCOPE_WANT_SOURCE")
					if got != source {
						t.Fatalf("durable import source = %q, want concrete source %q", got, source)
					}
					if strings.Contains(got, "registry:") {
						t.Fatalf("durable import stored registry selector: %+v", cfg.Imports["lighthouse"])
					}
					if got == "" {
						t.Fatalf("durable import source is empty: %+v", cfg.Imports["lighthouse"])
					}
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGCEnv(t)
			if tt.stubbed {
				stubPackScopeSync(t)
			}
			args := tt.setup(t)

			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != tt.want {
				t.Fatalf("run(%v) code=%d, want %d\nstdout=%q\nstderr=%q", args, code, tt.want, stdout.String(), stderr.String())
			}
			for _, check := range tt.checks {
				check(t, stdout.String(), stderr.String())
			}
		})
	}
}

func stubPackScopeSync(t *testing.T) {
	t.Helper()
	prevSync := syncImports
	t.Cleanup(func() { syncImports = prevSync })
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		packs := make(map[string]packman.LockedPack, len(imports))
		for _, imp := range imports {
			packs[imp.Source] = packman.LockedPack{Version: imp.Version}
		}
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: packs}, nil
	}
}

func packScopeLocalPack(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	writePackToml(t, dir, "[pack]\nname = \""+name+"\"\nschema = 1\n")
	return dir
}

func chdirPackScopeTest(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}
