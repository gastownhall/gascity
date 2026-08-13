package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestShippedStorageExampleCityLoads composes the storage example city that
// ships in examples/. The example is the only authoring reference for
// [storage.classes] and [storage.bindings] outside the design doc, so it has
// to keep loading and keep meaning what its comments say.
func TestShippedStorageExampleCityLoads(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	cityPath := filepath.Join(repoRoot, "examples", "storage", "city.toml")

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes(%s): %v", cityPath, err)
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
	if got := cfg.EffectiveStorage(); !got.Equal(want) {
		t.Fatalf("EffectiveStorage() = %#v, want %#v", got, want)
	}

	// The example teaches the fragment-split shape: the class map is in
	// city.toml and the binding it names is in the include fragment. If the
	// two halves ever merge into one file the example stops covering the
	// composition path it exists to demonstrate.
	root, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load(%s): %v", cityPath, err)
	}
	if root.Storage == nil || len(root.Storage.Bindings) != 0 {
		t.Fatalf("example city.toml defines bindings inline (%#v); the fragment should own them",
			root.Storage)
	}
	if len(root.Include) == 0 {
		t.Fatal("example city.toml no longer includes the storage fragment")
	}

	fragment := filepath.Join(repoRoot, "examples", "storage", "storage-bindings.toml")
	frag, err := Load(fsys.OSFS{}, fragment)
	if err != nil {
		t.Fatalf("Load(%s): %v", fragment, err)
	}
	if frag.Storage == nil || frag.Storage.Bindings["infra"].Provider != StorageProviderSQLiteBeads {
		t.Fatalf("example fragment no longer defines the sqlite infra binding: %#v", frag.Storage)
	}
	if strings.TrimSpace(frag.Storage.Classes.Work) != "" {
		t.Fatalf("example fragment unexpectedly assigns classes: %#v", frag.Storage.Classes)
	}
}
