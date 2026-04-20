package materialize

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestMCPIdentityForFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		want     string
		wantOkay bool
	}{
		{name: "foo.toml", want: "foo", wantOkay: true},
		{name: "foo.template.toml", want: "foo", wantOkay: true},
		{name: "foo.md", wantOkay: false},
	}
	for _, tt := range tests {
		got, ok := MCPIdentityForFilename(tt.name)
		if ok != tt.wantOkay || got != tt.want {
			t.Fatalf("%q => (%q, %v), want (%q, %v)", tt.name, got, ok, tt.want, tt.wantOkay)
		}
	}
}

func TestLoadMCPDirParsesAndNormalizes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tool.template.toml"), []byte(`
name = "tool"
description = "desc"
command = "./scripts/run.sh"
args = ["--city", "{{.CityRoot}}"]
[env]
TOKEN = "{{.Token}}"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	servers, err := LoadMCPDir(dir, "city", map[string]string{
		"CityRoot": "/tmp/city",
		"Token":    "abc",
	})
	if err != nil {
		t.Fatalf("LoadMCPDir: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("len(servers)=%d, want 1", len(servers))
	}
	server := servers[0]
	if server.Transport != MCPTransportStdio {
		t.Fatalf("Transport=%q, want %q", server.Transport, MCPTransportStdio)
	}
	if server.Command != filepath.Join(dir, "scripts", "run.sh") {
		t.Fatalf("Command=%q, want %q", server.Command, filepath.Join(dir, "scripts", "run.sh"))
	}
	if server.Args[1] != "/tmp/city" {
		t.Fatalf("Args=%v, expected template expansion", server.Args)
	}
	if got := server.Env["TOKEN"]; got != "abc" {
		t.Fatalf("Env[TOKEN]=%q, want abc", got)
	}
}

func TestLoadMCPDirRejectsDuplicateLogicalNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "foo.toml"), `name = "foo"`+"\ncommand = \"uvx\"\n")
	mustWriteFile(t, filepath.Join(dir, "foo.template.toml"), `name = "foo"`+"\ncommand = \"uvx\"\n")

	_, err := LoadMCPDir(dir, "city", nil)
	if err == nil || !strings.Contains(err.Error(), `duplicate logical server "foo"`) {
		t.Fatalf("LoadMCPDir error=%v, want duplicate logical name", err)
	}
}

func TestLoadMCPDirWrapsReadDirErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	mustWriteFile(t, file, "x")

	_, err := LoadMCPDir(file, "city", nil)
	if err == nil || !strings.Contains(err.Error(), "reading mcp dir") {
		t.Fatalf("LoadMCPDir error=%v, want wrapped directory read error", err)
	}
}

func TestLoadMCPDirValidatesNameAndTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filename string
		body     string
		wantErr  string
	}{
		{
			filename: "foo.toml",
			body:     "name = \"bar\"\ncommand = \"uvx\"\n",
			wantErr:  `must match filename stem`,
		},
		{
			filename: "foo.toml",
			body:     "name = \"foo\"\ncommand = \"uvx\"\nurl = \"https://example.com\"\n",
			wantErr:  `mutually exclusive`,
		},
		{
			filename: "foo.toml",
			body:     "name = \"foo\"\nurl = \"https://example.com\"\nargs = [\"x\"]\n",
			wantErr:  `http server may not set args or env`,
		},
		{
			filename: "bad_name.toml",
			body:     "name = \"bad_name\"\ncommand = \"uvx\"\n",
			wantErr:  `invalid server name`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.wantErr, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, tt.filename), tt.body)
			_, err := LoadMCPDir(dir, "city", nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadMCPDir error=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestMergeMCPDirsLaterWins(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	override := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "foo.toml"), "name = \"foo\"\ncommand = \"uvx\"\n")
	mustWriteFile(t, filepath.Join(override, "foo.toml"), "name = \"foo\"\nurl = \"https://example.com\"\n")
	mustWriteFile(t, filepath.Join(override, "bar.toml"), "name = \"bar\"\ncommand = \"node\"\n")

	cat, err := MergeMCPDirs([]MCPDirSource{
		{Dir: base, Label: "base"},
		{Dir: override, Label: "override"},
	}, nil)
	if err != nil {
		t.Fatalf("MergeMCPDirs: %v", err)
	}
	if len(cat.Servers) != 2 {
		t.Fatalf("len(Servers)=%d, want 2", len(cat.Servers))
	}
	if got := cat.ByName["foo"].Layer; got != "override" {
		t.Fatalf("foo.Layer=%q, want override", got)
	}
	if len(cat.Shadows) != 1 || cat.Shadows[0].Winner != "override" || cat.Shadows[0].Loser != "base" {
		t.Fatalf("Shadows=%v, want override shadow", cat.Shadows)
	}
}

func TestNormalizeMCPServerStableMapOrder(t *testing.T) {
	t.Parallel()

	server := MCPServer{
		Name:      "foo",
		Transport: MCPTransportStdio,
		Command:   "uvx",
		Args:      []string{"b", "a"},
		Env: map[string]string{
			"Z": "2",
			"A": "1",
		},
		Headers: map[string]string{
			"Y": "2",
			"X": "1",
		},
	}
	got := NormalizeMCPServer(server)
	want := NormalizedMCPServer{
		Name:      "foo",
		Transport: MCPTransportStdio,
		Command:   "uvx",
		Args:      []string{"b", "a"},
		Env:       []MCPKV{{Key: "A", Value: "1"}, {Key: "Z", Value: "2"}},
		Headers:   []MCPKV{{Key: "X", Value: "1"}, {Key: "Y", Value: "2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeMCPServer()=%#v, want %#v", got, want)
	}
}

func TestMCPPackSourcesForAgentOrdersAndDedupes(t *testing.T) {
	t.Parallel()

	cfg := &config.City{
		BootstrapImportPackDirs: []string{"/packs/bootstrap", "/packs/shared"},
		ImplicitImportPackDirs:  []string{"/packs/implicit"},
		ExplicitImportPackDirs:  []string{"/packs/shared", "/packs/import"},
		PackGraphOnlyDirs:       []string{"/packs/city"},
		PackMCPDir:              "/packs/city/mcp",
		RigImportPackDirs: map[string][]string{
			"rig": {"/packs/shared", "/packs/rig-import"},
		},
		RigPackGraphOnlyDirs: map[string][]string{
			"rig": {"/packs/rig"},
		},
	}
	agent := &config.Agent{
		Dir:    "rig",
		MCPDir: "/packs/agent/mcp",
	}

	got := MCPPackSourcesForAgent(cfg, agent)
	want := []MCPDirSource{
		{Dir: "/packs/bootstrap/mcp", Label: "bootstrap", Origin: "bootstrap"},
		{Dir: "/packs/implicit/mcp", Label: "implicit", Origin: "implicit"},
		{Dir: "/packs/import/mcp", Label: "import", Origin: "import"},
		{Dir: "/packs/city/mcp", Label: "city", Origin: "city"},
		{Dir: "/packs/shared/mcp", Label: "rig-import", Origin: "rig-import"},
		{Dir: "/packs/rig-import/mcp", Label: "rig-import", Origin: "rig-import"},
		{Dir: "/packs/rig/mcp", Label: "rig", Origin: "rig"},
		{Dir: "/packs/agent/mcp", Label: "agent", Origin: "agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MCPPackSourcesForAgent()=%#v, want %#v", got, want)
	}
}

func TestEffectiveMCPForAgent_ExplicitImportBeatsShadowedImplicit(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	implicitCacheDir := config.GlobalRepoCachePath(gcHome, "github.com/someone/custom-pack", "zzzz999")
	if err := os.MkdirAll(filepath.Join(implicitCacheDir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(implicitCacheDir, "pack.toml"), `
[pack]
name = "custom-pack"
schema = 1
`)
	mustWriteFile(t, filepath.Join(implicitCacheDir, "mcp", "shared.toml"), `
name = "shared"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(gcHome, "implicit-import.toml"), `
schema = 1

[imports.custom]
source = "github.com/someone/custom-pack"
version = "1.0.0"
commit = "zzzz999"
`)

	cityDir := t.TempDir()
	explicitDir := filepath.Join(cityDir, "packs", "explicit-custom")
	if err := os.MkdirAll(filepath.Join(explicitDir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(explicitDir, "pack.toml"), `
[pack]
name = "explicit-custom"
schema = 1
`)
	mustWriteFile(t, filepath.Join(explicitDir, "mcp", "shared.toml"), `
name = "shared"
command = "node"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[imports.custom]
source = "./packs/explicit-custom"

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.ImplicitImportBindings["custom"] {
		t.Fatalf("ImplicitImportBindings[custom]=true, want false for shadowed explicit import")
	}

	var mayor *config.Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "mayor" && cfg.Agents[i].BindingName == "" {
			mayor = &cfg.Agents[i]
			break
		}
	}
	if mayor == nil {
		t.Fatal("missing mayor agent")
	}

	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Layer != "import" {
		t.Fatalf("shared.Layer=%q, want import", server.Layer)
	}
	if server.Command != "node" {
		t.Fatalf("shared.Command=%q, want node", server.Command)
	}
}

func TestEffectiveMCPForAgent_BootstrapLayerIncluded(t *testing.T) {
	coreCacheDir := t.TempDir()
	mustWriteFile(t, filepath.Join(coreCacheDir, "pack.toml"), `
[pack]
name = "core"
schema = 1
`)
	mustWriteFile(t, filepath.Join(coreCacheDir, "mcp", "bootstrap.toml"), `
name = "bootstrap"
command = "uvx"
`)

	cfg := &config.City{
		BootstrapImportPackDirs: []string{coreCacheDir},
		BootstrapImportMCPBindings: map[string]string{
			coreCacheDir: "core",
		},
		Agents: []config.Agent{{
			Name:  "mayor",
			Scope: "city",
		}},
	}

	mayor := mustFindAgent(t, cfg, "mayor")
	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["bootstrap"]
	if !ok {
		t.Fatalf("missing bootstrap server in %#v", catalog.ByName)
	}
	if server.Layer != "bootstrap" {
		t.Fatalf("bootstrap.Layer=%q, want bootstrap", server.Layer)
	}
}

func TestEffectiveMCPForAgent_CityGraphBeatsExplicitImport(t *testing.T) {
	cityDir := t.TempDir()
	baseDir := filepath.Join(cityDir, "packs", "base")
	importDir := filepath.Join(cityDir, "packs", "extra")
	mustWriteFile(t, filepath.Join(baseDir, "pack.toml"), `
[pack]
name = "base"
schema = 1
`)
	mustWriteFile(t, filepath.Join(baseDir, "mcp", "shared.toml"), `
name = "shared"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(importDir, "pack.toml"), `
[pack]
name = "extra"
schema = 1
`)
	mustWriteFile(t, filepath.Join(importDir, "mcp", "shared.toml"), `
name = "shared"
command = "node"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"
includes = ["packs/base"]

[imports.extra]
source = "./packs/extra"

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	mayor := mustFindAgent(t, cfg, "mayor")
	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Layer != "city" {
		t.Fatalf("shared.Layer=%q, want city", server.Layer)
	}
	if server.Command != "uvx" {
		t.Fatalf("shared.Command=%q, want uvx", server.Command)
	}
}

func TestEffectiveMCPForAgent_ExplicitImportBindingOrderFirstWins(t *testing.T) {
	cityDir := t.TempDir()
	alphaDir := filepath.Join(cityDir, "packs", "alpha")
	betaDir := filepath.Join(cityDir, "packs", "beta")
	mustWriteFile(t, filepath.Join(alphaDir, "pack.toml"), `
[pack]
name = "alpha"
schema = 1
`)
	mustWriteFile(t, filepath.Join(alphaDir, "mcp", "shared.toml"), `
name = "shared"
command = "alpha"
`)
	mustWriteFile(t, filepath.Join(betaDir, "pack.toml"), `
[pack]
name = "beta"
schema = 1
`)
	mustWriteFile(t, filepath.Join(betaDir, "mcp", "shared.toml"), `
name = "shared"
command = "beta"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[imports.alpha]
source = "./packs/alpha"

[imports.beta]
source = "./packs/beta"

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	mayor := mustFindAgent(t, cfg, "mayor")
	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Command != "alpha" {
		t.Fatalf("shared.Command=%q, want alpha", server.Command)
	}
	if server.Origin != "import:alpha" {
		t.Fatalf("shared.Origin=%q, want import:alpha", server.Origin)
	}
}

func TestEffectiveMCPForAgent_ExplicitImportBindingOrderFirstWinsAcrossSharedDependency(t *testing.T) {
	cityDir := t.TempDir()
	alphaDir := filepath.Join(cityDir, "packs", "alpha")
	betaDir := filepath.Join(cityDir, "packs", "beta")
	sharedDir := filepath.Join(cityDir, "packs", "shared")
	mustWriteFile(t, filepath.Join(alphaDir, "pack.toml"), `
[pack]
name = "alpha"
schema = 1

[imports.shared]
source = "../shared"
`)
	mustWriteFile(t, filepath.Join(betaDir, "pack.toml"), `
[pack]
name = "beta"
schema = 1

[imports.shared]
source = "../shared"
`)
	mustWriteFile(t, filepath.Join(betaDir, "mcp", "shared.toml"), `
name = "shared"
command = "beta"
`)
	mustWriteFile(t, filepath.Join(sharedDir, "pack.toml"), `
[pack]
name = "shared"
schema = 1
`)
	mustWriteFile(t, filepath.Join(sharedDir, "mcp", "shared.toml"), `
name = "shared"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[imports.alpha]
source = "./packs/alpha"

[imports.beta]
source = "./packs/beta"

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	mayor := mustFindAgent(t, cfg, "mayor")
	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Command != "uvx" {
		t.Fatalf("shared.Command=%q, want uvx from alpha's shared dependency", server.Command)
	}
	if server.Origin != "import:alpha" {
		t.Fatalf("shared.Origin=%q, want import:alpha", server.Origin)
	}
}

func TestEffectiveMCPForAgent_TransitiveFalseHidesNestedImportMCP(t *testing.T) {
	cityDir := t.TempDir()
	toolboxDir := filepath.Join(cityDir, "packs", "toolbox")
	utilDir := filepath.Join(cityDir, "packs", "util")
	mustWriteFile(t, filepath.Join(toolboxDir, "pack.toml"), `
[pack]
name = "toolbox"
schema = 1

[imports.util]
source = "../util"
`)
	mustWriteFile(t, filepath.Join(toolboxDir, "mcp", "direct.toml"), `
name = "direct"
command = "node"
`)
	mustWriteFile(t, filepath.Join(utilDir, "pack.toml"), `
[pack]
name = "util"
schema = 1
`)
	mustWriteFile(t, filepath.Join(utilDir, "mcp", "nested.toml"), `
name = "nested"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[imports.toolbox]
source = "./packs/toolbox"
transitive = false

[[agent]]
name = "mayor"
scope = "city"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	mayor := mustFindAgent(t, cfg, "mayor")
	catalog, err := EffectiveMCPForAgent(cfg, mayor, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	if _, ok := catalog.ByName["direct"]; !ok {
		t.Fatalf("missing direct server in %#v", catalog.ByName)
	}
	if _, ok := catalog.ByName["nested"]; ok {
		t.Fatalf("nested server should be hidden by transitive=false: %#v", catalog.ByName)
	}
}

func TestEffectiveMCPForAgent_RigGraphBeatsRigImport(t *testing.T) {
	cityDir := t.TempDir()
	baseDir := filepath.Join(cityDir, "packs", "rig-base")
	importDir := filepath.Join(cityDir, "packs", "rig-helper")
	mustWriteFile(t, filepath.Join(baseDir, "pack.toml"), `
[pack]
name = "rig-base"
schema = 1
`)
	mustWriteFile(t, filepath.Join(baseDir, "mcp", "shared.toml"), `
name = "shared"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(importDir, "pack.toml"), `
[pack]
name = "rig-helper"
schema = 1
`)
	mustWriteFile(t, filepath.Join(importDir, "mcp", "shared.toml"), `
name = "shared"
command = "node"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["packs/rig-base"]

[rigs.imports.helper]
source = "./packs/rig-helper"

[[agent]]
name = "witness"
scope = "rig"
dir = "proj"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	witness := mustFindAgent(t, cfg, "proj/witness")
	catalog, err := EffectiveMCPForAgent(cfg, witness, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Layer != "rig" {
		t.Fatalf("shared.Layer=%q, want rig", server.Layer)
	}
	if server.Command != "uvx" {
		t.Fatalf("shared.Command=%q, want uvx", server.Command)
	}
}

func TestEffectiveMCPForAgent_RigGraphNestedImportBeatsRigImport(t *testing.T) {
	cityDir := t.TempDir()
	baseDir := filepath.Join(cityDir, "packs", "rig-base")
	nestedDir := filepath.Join(cityDir, "packs", "rig-nested")
	importDir := filepath.Join(cityDir, "packs", "rig-helper")
	mustWriteFile(t, filepath.Join(baseDir, "pack.toml"), `
[pack]
name = "rig-base"
schema = 1

[imports.nested]
source = "../rig-nested"
`)
	mustWriteFile(t, filepath.Join(nestedDir, "pack.toml"), `
[pack]
name = "rig-nested"
schema = 1
`)
	mustWriteFile(t, filepath.Join(nestedDir, "mcp", "shared.toml"), `
name = "shared"
command = "uvx"
`)
	mustWriteFile(t, filepath.Join(importDir, "pack.toml"), `
[pack]
name = "rig-helper"
schema = 1
`)
	mustWriteFile(t, filepath.Join(importDir, "mcp", "shared.toml"), `
name = "shared"
command = "node"
`)
	mustWriteFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "test-city"

[[rigs]]
name = "proj"
path = "/tmp/proj"
includes = ["packs/rig-base"]

[rigs.imports.helper]
source = "./packs/rig-helper"

[[agent]]
name = "witness"
scope = "rig"
dir = "proj"
`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	witness := mustFindAgent(t, cfg, "proj/witness")
	catalog, err := EffectiveMCPForAgent(cfg, witness, nil)
	if err != nil {
		t.Fatalf("EffectiveMCPForAgent: %v", err)
	}
	server, ok := catalog.ByName["shared"]
	if !ok {
		t.Fatalf("missing shared server in %#v", catalog.ByName)
	}
	if server.Layer != "rig" {
		t.Fatalf("shared.Layer=%q, want rig", server.Layer)
	}
	if server.Command != "uvx" {
		t.Fatalf("shared.Command=%q, want uvx", server.Command)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func mustFindAgent(t *testing.T, cfg *config.City, qualifiedName string) *config.Agent {
	t.Helper()
	for i := range cfg.Agents {
		if cfg.Agents[i].QualifiedName() == qualifiedName {
			return &cfg.Agents[i]
		}
	}
	t.Fatalf("missing agent %q in %#v", qualifiedName, cfg.Agents)
	return nil
}
