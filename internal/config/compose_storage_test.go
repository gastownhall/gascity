package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fragmentSplitStorageCity is the authoring shape the design blesses: the
// city assigns the six classes and an include fragment defines the binding
// those assignments name. Neither file is a complete storage configuration on
// its own.
const (
	fragmentSplitStorageRoot = `
include = ["storage.toml"]

[workspace]
name = "split-city"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"
`
	fragmentSplitStorageFragment = `
[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`
)

// TestLoadAcceptsFragmentSplitStorageDeferredToComposition guards the
// asymmetry that made every fragment-split city unloadable: config.Load reads
// one file and skips include expansion, so it must not enforce cross-layer
// storage invariants that only the composed root can satisfy.
func TestLoadAcceptsFragmentSplitStorageDeferredToComposition(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(fragmentSplitStorageRoot)
	fs.Files["/city/storage.toml"] = []byte(fragmentSplitStorageFragment)

	cfg, err := Load(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Storage.Classes.Graph; got != "infra" {
		t.Fatalf("Storage.Classes.Graph = %q, want %q", got, "infra")
	}

	composed, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	want := StorageConfig{
		Classes: StorageClasses{
			Work:      StorageWorkBinding,
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		},
		Bindings: map[string]StorageBindingConfig{
			"infra": {Provider: StorageProviderSQLiteBeads, Path: DefaultSQLiteStoragePath},
		},
	}
	if got := composed.EffectiveStorage(); !got.Equal(want) {
		t.Fatalf("EffectiveStorage() = %#v, want %#v", got, want)
	}
}

// TestLoadStillRejectsLayerLocalStorageErrors keeps the single-file gate that
// does belong in Parse: a layer owns its own binding definitions, so a
// reserved or malformed one fails without waiting for composition.
func TestLoadStillRejectsLayerLocalStorageErrors(t *testing.T) {
	tests := []struct {
		name    string
		fragmnt string
		wantErr string
	}{
		{
			name: "reserved work binding",
			fragmnt: `
[storage.bindings.work]
provider = "sqlite-beads"
`,
			wantErr: "storage.bindings.work is reserved",
		},
		{
			name: "sqlite rejects config_ref",
			fragmnt: `
[storage.bindings.infra]
provider = "sqlite-beads"
config_ref = "city-infra"
`,
			wantErr: `provider "sqlite-beads" does not accept config_ref`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(fragmentSplitStorageRoot)
			fs.Files["/city/storage.toml"] = []byte(tc.fragmnt)

			if _, err := Load(fs, "/city/storage.toml"); err == nil ||
				!strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load(fragment) error = %v, want %q", err, tc.wantErr)
			}
			if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil ||
				!strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadWithIncludes error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadWithIncludesRejectsIncompleteStorage proves the deferred cross-layer
// checks still run — in the single-file shape as well as the fragment-split
// one. Moving a check until it never fires would be worse than the asymmetry
// it fixed.
func TestLoadWithIncludesRejectsIncompleteStorage(t *testing.T) {
	completeClasses := `
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
`
	tests := []struct {
		name     string
		city     string
		fragment string
		wantErr  string
	}{
		{
			name: "single file missing class",
			city: `
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
`,
			wantErr: "storage.classes.nudges is required",
		},
		{
			name: "single file undefined binding",
			city: strings.Replace(completeClasses, `graph = "work"`, `graph = "missing"`, 1),
			wantErr: `storage.classes.graph references undefined binding "missing"` +
				``,
		},
		{
			name:     "fragment split undefined binding",
			city:     `include = ["storage.toml"]` + strings.Replace(completeClasses, `graph = "work"`, `graph = "infra"`, 1),
			fragment: "[storage.bindings.other]\nprovider = \"sqlite-beads\"\n",
			wantErr:  `storage.classes.graph references undefined binding "infra"`,
		},
		{
			name:     "fragment split missing class",
			city:     "include = [\"storage.toml\"]\n\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\n",
			fragment: "[storage.bindings.infra]\nprovider = \"sqlite-beads\"\n",
			wantErr:  "storage.classes.sessions is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(tc.city)
			if tc.fragment != "" {
				fs.Files["/city/storage.toml"] = []byte(tc.fragment)
			}
			_, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadWithIncludes error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadWithIncludesStorageSurvivesEveryConfigLayer covers AC1: a
// fragment-split storage configuration round-trips when patches and rig
// overrides are applied on top of it.
func TestLoadWithIncludesStorageSurvivesEveryConfigLayer(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, data string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	writeFile("city.toml", `
include = ["fragments/storage.toml", "fragments/patch.toml"]

[workspace]
name = "layered"

[storage.classes]
work = "tasks"
graph = "infra"

[storage.bindings.tasks]
provider = "test-go-provider"
config_ref = "city-work"

[[rigs]]
name = "hw"
path = "rig"
includes = ["packs/base"]

  [[rigs.overrides]]
  agent = "worker"
  prompt_template = "prompts/rig-worker.md"
`)
	writeFile("fragments/storage.toml", `
[storage.classes]
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "work"

[storage.bindings.infra]
provider = "sqlite-beads"
`)
	writeFile("fragments/patch.toml", `
[[patches.agent]]
dir = "hw"
name = "worker"
idle_timeout = "9m"
`)
	writeFile("packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
scope = "rig"
prompt_template = "prompts/base-worker.md"
`)
	writeFile("prompts/rig-worker.md", "rig override prompt\n")

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	want := StorageConfig{
		Classes: StorageClasses{
			Work:      "tasks",
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    StorageWorkBinding,
		},
		Bindings: map[string]StorageBindingConfig{
			"tasks": {Provider: "test-go-provider", ConfigRef: "city-work"},
			"infra": {Provider: StorageProviderSQLiteBeads, Path: DefaultSQLiteStoragePath},
		},
	}
	if got := cfg.EffectiveStorage(); !got.Equal(want) {
		t.Fatalf("EffectiveStorage() = %#v, want %#v", got, want)
	}

	agents := explicitAgents(cfg.Agents)
	if len(agents) != 1 {
		t.Fatalf("len(explicit agents) = %d, want 1", len(agents))
	}
	if agents[0].IdleTimeout != "9m" {
		t.Fatalf("IdleTimeout = %q, want 9m (patch layer applied)", agents[0].IdleTimeout)
	}
	if agents[0].PromptTemplate != "prompts/rig-worker.md" {
		t.Fatalf("PromptTemplate = %q, want prompts/rig-worker.md (rig override applied)",
			agents[0].PromptTemplate)
	}
}

// TestStorageInfrastructureClassKeepsWorkBindingAfterWorkMoves covers AC4:
// moving the work class onto its own binding does not force the
// infrastructure classes off the reserved work binding.
func TestStorageInfrastructureClassKeepsWorkBindingAfterWorkMoves(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[storage.classes]
work = "tasks"
graph = "work"
sessions = "work"
messaging = "infra"
orders = "infra"
nudges = "work"

[storage.bindings.tasks]
provider = "test-go-provider"
config_ref = "city-work"

[storage.bindings.infra]
provider = "sqlite-beads"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	effective := cfg.EffectiveStorage()
	for _, class := range []StorageClass{StorageClassGraph, StorageClassSessions, StorageClassNudges} {
		if got := effective.Classes.BindingFor(class); got != StorageWorkBinding {
			t.Errorf("BindingFor(%s) = %q, want %q", class, got, StorageWorkBinding)
		}
	}
	if got := effective.Classes.BindingFor(StorageClassWork); got != "tasks" {
		t.Fatalf("BindingFor(work) = %q, want tasks", got)
	}
}

func TestLoadWithIncludesMergesStorageClassesByFieldAndBindingsAdditively(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["sessions.toml", "remaining.toml"]

[storage.classes]
work = "tasks"
graph = "work"

[storage.bindings.tasks]
provider = "test-go-provider"
config_ref = "city-work"
`)
	fs.Files["/city/sessions.toml"] = []byte(`
[storage.classes]
graph = "infra"
sessions = "remote-infra"
messaging = "remote-infra"

[storage.bindings.infra]
provider = "sqlite-beads"

[storage.bindings.remote-infra]
provider = "test-rust-provider"
config_ref = "city-infrastructure"
`)
	fs.Files["/city/remaining.toml"] = []byte(`
[storage.classes]
orders = "work"
nudges = "infra"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	got := cfg.EffectiveStorage()
	want := StorageConfig{
		Classes: StorageClasses{
			Work:      "tasks",
			Graph:     "infra",
			Sessions:  "remote-infra",
			Messaging: "remote-infra",
			Orders:    "work",
			Nudges:    "infra",
		},
		Bindings: map[string]StorageBindingConfig{
			"tasks": {
				Provider:  "test-go-provider",
				ConfigRef: "city-work",
			},
			"infra": {
				Provider: StorageProviderSQLiteBeads,
				Path:     DefaultSQLiteStoragePath,
			},
			"remote-infra": {
				Provider:  "test-rust-provider",
				ConfigRef: "city-infrastructure",
			},
		},
	}
	if !got.Equal(want) {
		t.Fatalf("EffectiveStorage() = %#v, want %#v", got, want)
	}
}

func TestLoadWithIncludesRejectsDuplicateStorageBindingsAcrossLayers(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["override.toml"]

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`)
	fs.Files["/city/override.toml"] = []byte(`
[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/other"
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil || !strings.Contains(err.Error(), `storage binding "infra" is defined more than once`) {
		t.Fatalf("LoadWithIncludes error = %v, want duplicate binding rejection", err)
	}
}

func TestLoadWithIncludesRequiresCompleteStorageAfterLayering(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["partial.toml"]

[storage.classes]
work = "work"
graph = "work"
`)
	fs.Files["/city/partial.toml"] = []byte(`
[storage.classes]
sessions = "work"
messaging = "work"
orders = "work"
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil || !strings.Contains(err.Error(), "storage.classes.nudges is required") {
		t.Fatalf("LoadWithIncludes error = %v, want missing final assignment", err)
	}
}

func TestLoadWithIncludesRejectsUnknownStorageKeyInFragment(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`include = ["bad.toml"]`)
	fs.Files["/city/bad.toml"] = []byte(`
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
cache = "work"
`)

	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil || !strings.Contains(err.Error(), `unknown storage class "cache"`) {
		t.Fatalf("LoadWithIncludes error = %v, want unknown fragment class", err)
	}
}
