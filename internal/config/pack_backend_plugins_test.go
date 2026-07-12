package config

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestExpandCityPacks_BackendPluginBundleResolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/dl/pack.toml", `
[pack]
name = "dl"
schema = 2

[[backend_plugins]]
backend = "doltlite"
setup_hook = "assets/scripts/gc-beads-doltlite-bd.sh"
provider_command = "assets/scripts/gc-beads-doltlite-bd.sh"
prepare_command = ["build", "backend", "--install"]
	store_path = ".beads/doltlite"
	bd_compatibility = "bd-1.0.5"
	capabilities = ["setup", "provider", "metadata", "fastpath", "store-health"]

	[backend_plugins.scope]
	model = "per_scope"
	resource = "schema"
	namespace_from = "prefix"
	inherits_city_connection = true
	metadata_owner = "plugin"
	routes = "gc-prefix-routes"
	adopt = "validate_or_repair"
	remove = "preserve"

[backend_plugins.beads_endpoint]
command = ".gc/runtime/packs/bd-gc-dl/bin/bd-backend-doltlite"
args = ["serve"]
protocol = "beads.backend.v1alpha1"

[backend_plugins.gascity_endpoint]
command = ".gc/runtime/packs/bd-gc-dl/bin/gc-doltlite-fastpath"
args = ["serve"]
protocol = "gascity.backend.v1alpha1"
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/dl"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if got, want := len(cfg.BackendPlugins), 1; got != want {
		t.Fatalf("len(BackendPlugins) = %d, want %d: %+v", got, want, cfg.BackendPlugins)
	}
	plugin := cfg.BackendPlugins[0]
	packDir := filepath.Join(dir, "packs/dl")
	if plugin.Backend != "doltlite" {
		t.Fatalf("Backend = %q, want doltlite", plugin.Backend)
	}
	if want := filepath.Join(packDir, "assets/scripts/gc-beads-doltlite-bd.sh"); plugin.SetupHook != want {
		t.Fatalf("SetupHook = %q, want %q", plugin.SetupHook, want)
	}
	if want := filepath.Join(dir, ".gc/runtime/packs/bd-gc-dl/bin/bd-backend-doltlite"); plugin.BeadsEndpoint.Command != want {
		t.Fatalf("BeadsEndpoint.Command = %q, want %q", plugin.BeadsEndpoint.Command, want)
	}
	if want := filepath.Join(dir, ".gc/runtime/packs/bd-gc-dl/bin/gc-doltlite-fastpath"); plugin.GascityEndpoint.Command != want {
		t.Fatalf("GascityEndpoint.Command = %q, want %q", plugin.GascityEndpoint.Command, want)
	}
	if got, want := plugin.PrepareCommand, []string{"build", "backend", "--install"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PrepareCommand = %#v, want %#v", got, want)
	}
	if plugin.BDCompatibility != "bd-1.0.5" {
		t.Fatalf("BDCompatibility = %q, want bd-1.0.5", plugin.BDCompatibility)
	}
	if plugin.Scope.Model != "per_scope" || plugin.Scope.Resource != "schema" || plugin.Scope.NamespaceFrom != "prefix" {
		t.Fatalf("Scope = %+v, want per_scope/schema/prefix", plugin.Scope)
	}
	if !plugin.Scope.InheritsCityConnection || plugin.Scope.MetadataOwner != "plugin" || plugin.Scope.Routes != "gc-prefix-routes" || plugin.Scope.Adopt != "validate_or_repair" || plugin.Scope.Remove != "preserve" {
		t.Fatalf("Scope = %+v, want resolved plugin rig policy", plugin.Scope)
	}
}

func TestExpandCityPacks_NativeBackendBundleResolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/sqlite/pack.toml", `
[pack]
name = "sqlite"
schema = 2

[[backend_plugins]]
backend = "sqlite"
kind = "native"
driver = "sqlite"
sqlite_path = "gascity.db"

[backend_plugins.scope]
model = "per_scope"
resource = "file"
namespace_from = "prefix"
metadata_owner = "bd"
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/sqlite"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if got, want := len(cfg.BackendPlugins), 1; got != want {
		t.Fatalf("len(BackendPlugins) = %d, want %d", got, want)
	}
	backend := cfg.BackendPlugins[0]
	if backend.Kind != "native" || backend.Driver != "sqlite" || backend.SQLitePath != "gascity.db" {
		t.Fatalf("native backend = %+v", backend)
	}
	if backend.SetupHook != "" || backend.ProviderCommand != "" {
		t.Fatalf("native backend unexpectedly resolved plugin commands: %+v", backend)
	}
}

func TestExpandCityPacks_NonTransitiveImportFiltersNestedBackendPlugins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/inner/pack.toml", `
[pack]
name = "inner"
schema = 2

[[backend_plugins]]
backend = "hidden"
setup_hook = "assets/hidden.sh"
`)
	writeFile(t, dir, "packs/middle/pack.toml", `
[pack]
name = "middle"
schema = 2

[imports.inner]
source = "../inner"

[[backend_plugins]]
backend = "visible"
setup_hook = "assets/visible.sh"
`)
	writeFile(t, dir, "packs/outer/pack.toml", `
[pack]
name = "outer"
schema = 2

[imports.middle]
source = "../middle"
transitive = false
`)

	cfg := &City{Workspace: Workspace{Includes: []string{"packs/outer"}}}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if got, want := len(cfg.BackendPlugins), 1; got != want {
		t.Fatalf("len(BackendPlugins) = %d, want %d: %+v", got, want, cfg.BackendPlugins)
	}
	if cfg.BackendPlugins[0].Backend != "visible" {
		t.Fatalf("Backend = %q, want visible", cfg.BackendPlugins[0].Backend)
	}
}

func TestLoadWithIncludes_RootPackBackendPluginBundleResolved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "city.toml", `
[beads]
provider = "plugin"
backend = "doltlite"
`)
	writeFile(t, dir, "pack.toml", `
[pack]
name = "city"
schema = 2

[[backend_plugins]]
backend = "doltlite"
setup_hook = "assets/scripts/gc-beads-doltlite-bd.sh"
store_path = ".beads/doltlite"
bd_compatibility = "bd-1.0.5"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got, want := len(cfg.BackendPlugins), 1; got != want {
		t.Fatalf("len(BackendPlugins) = %d, want %d: %+v", got, want, cfg.BackendPlugins)
	}
	plugin := cfg.BackendPlugins[0]
	if plugin.Backend != "doltlite" {
		t.Fatalf("Backend = %q, want doltlite", plugin.Backend)
	}
	if want := filepath.Join(dir, "assets/scripts/gc-beads-doltlite-bd.sh"); plugin.SetupHook != want {
		t.Fatalf("SetupHook = %q, want %q", plugin.SetupHook, want)
	}
	if plugin.StorePath != ".beads/doltlite" {
		t.Fatalf("StorePath = %q, want .beads/doltlite", plugin.StorePath)
	}
}
