package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryEmptyFile(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty list, got %d entries", len(entries))
	}
}

func TestRegistryRegisterAndList(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	// Register canonicalizes via filepath.EvalSymlinks; resolve expected
	// path so the comparison holds on macOS (/var → /private/var).
	wantCity, _ := filepath.EvalSymlinks(cityPath)
	if entries[0].Path != wantCity {
		t.Errorf("expected path %s, got %s", wantCity, entries[0].Path)
	}
}

func TestRegistryRegisterIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}
	// Registering again should be a no-op.
	if err := r.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after double register, got %d", len(entries))
	}
}

func TestRegistryDuplicateNameRejected(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	path1 := filepath.Join(dir, "sub1", "myproject")
	path2 := filepath.Join(dir, "sub2", "myproject")
	if err := os.MkdirAll(path1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path2, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(path1, ""); err != nil {
		t.Fatal(err)
	}
	err := r.Register(path2, "")
	if err == nil {
		t.Fatal("expected error for duplicate city name")
	}
}

func TestRegistryUnregister(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Unregister(cityPath); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after unregister, got %d", len(entries))
	}
}

func TestRegistryUnregisterNotFound(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	err := r.Unregister(filepath.Join(dir, "nonexistent"))
	if err == nil {
		t.Fatal("expected error for unregistering non-existent city")
	}
}

func TestRegistryMultipleCities(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	path1 := filepath.Join(dir, "city-a")
	path2 := filepath.Join(dir, "city-b")
	if err := os.MkdirAll(path1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path2, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(path1, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(path2, ""); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Unregister first, second remains.
	if err := r.Unregister(path1); err != nil {
		t.Fatal(err)
	}
	entries, err = r.List()
	if err != nil {
		t.Fatal(err)
	}
	// Register canonicalizes via filepath.EvalSymlinks; resolve expected
	// path so the comparison holds on macOS (/var → /private/var).
	wantPath2, _ := filepath.EvalSymlinks(path2)
	if len(entries) != 1 || entries[0].Path != wantPath2 {
		t.Errorf("expected only city-b, got %v", entries)
	}
}

func TestRegistryReRegisterNameUpdate(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Register with initial name.
	if err := r.Register(cityPath, "alpha"); err != nil {
		t.Fatal(err)
	}

	// Re-register same path with different name — should update.
	if err := r.Register(cityPath, "beta"); err != nil {
		t.Fatal(err)
	}

	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "beta" {
		t.Errorf("expected updated name 'beta', got %q", entries[0].Name)
	}
}

func TestRegistryReRegisterNameConflict(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	path1 := filepath.Join(dir, "city-a")
	path2 := filepath.Join(dir, "city-b")
	if err := os.MkdirAll(path1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path2, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(path1, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(path2, "beta"); err != nil {
		t.Fatal(err)
	}

	// Re-register path2 with name "alpha" — should conflict.
	err := r.Register(path2, "alpha")
	if err == nil {
		t.Fatal("expected error for name conflict on re-register")
	}
}

func TestCityEntryEffectiveName(t *testing.T) {
	// Without explicit name, returns empty string.
	e := CityEntry{Path: "/home/user/bright-lights"}
	if e.EffectiveName() != "" {
		t.Errorf("expected empty, got %s", e.EffectiveName())
	}

	// With explicit name, uses it.
	e2 := CityEntry{Path: "/home/user/bright-lights", Name: "neon-city"}
	if e2.EffectiveName() != "neon-city" {
		t.Errorf("expected neon-city, got %s", e2.EffectiveName())
	}
}
