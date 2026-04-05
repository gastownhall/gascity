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

// --- Rig registry tests ---

func TestRigRegisterAndList(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", "/some/city"); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(rigs))
	}
	if rigs[0].Name != "myapp" {
		t.Errorf("expected name myapp, got %s", rigs[0].Name)
	}
	if rigs[0].DefaultCity != "/some/city" {
		t.Errorf("expected default_city /some/city, got %s", rigs[0].DefaultCity)
	}
}

func TestRigRegisterIdempotentUpdate(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", ""); err != nil {
		t.Fatal(err)
	}
	// Re-register same path with same name — updates default.
	if err := r.RegisterRig(rigPath, "myapp", "/new/city"); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(rigs))
	}
	if rigs[0].DefaultCity != "/new/city" {
		t.Errorf("expected default_city /new/city, got %s", rigs[0].DefaultCity)
	}
}

func TestRigGlobalNameUniqueness(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	path1 := filepath.Join(dir, "app1")
	path2 := filepath.Join(dir, "app2")
	if err := os.MkdirAll(path1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path2, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(path1, "myapp", ""); err != nil {
		t.Fatal(err)
	}
	err := r.RegisterRig(path2, "myapp", "")
	if err == nil {
		t.Fatal("expected error for duplicate rig name")
	}
}

func TestRigUnregister(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.UnregisterRig(rigPath); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 0 {
		t.Errorf("expected 0 rigs after unregister, got %d", len(rigs))
	}
}

func TestRigUnregisterNotFound(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	err := r.UnregisterRig(filepath.Join(dir, "nonexistent"))
	if err == nil {
		t.Fatal("expected error for unregistering non-existent rig")
	}
}

func TestRigLookupByPath(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(filepath.Join(rigPath, "src", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", "/some/city"); err != nil {
		t.Fatal(err)
	}

	// Exact match.
	entry, ok := r.LookupRigByPath(rigPath)
	if !ok {
		t.Fatal("expected to find rig by exact path")
	}
	if entry.Name != "myapp" {
		t.Errorf("expected myapp, got %s", entry.Name)
	}

	// Prefix match (subdir).
	entry, ok = r.LookupRigByPath(filepath.Join(rigPath, "src", "deep"))
	if !ok {
		t.Fatal("expected to find rig by prefix path")
	}
	if entry.Name != "myapp" {
		t.Errorf("expected myapp, got %s", entry.Name)
	}

	// No match.
	_, ok = r.LookupRigByPath(filepath.Join(dir, "other"))
	if ok {
		t.Fatal("expected no match for unrelated path")
	}
}

func TestRigLookupByPathLongestPrefix(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	outerPath := filepath.Join(dir, "workspace")
	innerPath := filepath.Join(dir, "workspace", "subrig")
	if err := os.MkdirAll(filepath.Join(innerPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(outerPath, "outer", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterRig(innerPath, "inner", ""); err != nil {
		t.Fatal(err)
	}

	// Should match inner (longest prefix).
	entry, ok := r.LookupRigByPath(filepath.Join(innerPath, "src"))
	if !ok {
		t.Fatal("expected to find rig")
	}
	if entry.Name != "inner" {
		t.Errorf("expected inner (longest prefix), got %s", entry.Name)
	}
}

func TestRigLookupByName(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", "/some/city"); err != nil {
		t.Fatal(err)
	}

	entry, ok := r.LookupRigByName("myapp")
	if !ok {
		t.Fatal("expected to find rig by name")
	}
	if entry.Path != rigPath {
		t.Errorf("expected path %s, got %s", rigPath, entry.Path)
	}

	_, ok = r.LookupRigByName("nonexistent")
	if ok {
		t.Fatal("expected no match for nonexistent name")
	}
}

func TestRigSetDefault(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RegisterRig(rigPath, "myapp", "/city-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRigDefault(rigPath, "/city-b"); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if rigs[0].DefaultCity != "/city-b" {
		t.Errorf("expected /city-b, got %s", rigs[0].DefaultCity)
	}
}

func TestRigSetDefaultNotFound(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	err := r.SetRigDefault(filepath.Join(dir, "nope"), "/city")
	if err == nil {
		t.Fatal("expected error for non-existent rig")
	}
}

func TestRigReconcile(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rig1 := filepath.Join(dir, "rig1")
	rig2 := filepath.Join(dir, "rig2")
	rig3 := filepath.Join(dir, "rig3")
	for _, p := range []string{rig1, rig2, rig3} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Pre-populate: rig1 with explicit default, rig3 (will be removed).
	if err := r.RegisterRig(rig1, "rig1", "/city-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterRig(rig3, "rig3", "/city-old"); err != nil {
		t.Fatal(err)
	}

	// Reconcile: rig1 in city-a + city-b, rig2 in city-a only, rig3 gone.
	mappings := []RigCityMapping{
		{RigPath: rig1, RigName: "rig1", CityPath: "/city-a"},
		{RigPath: rig1, RigName: "rig1", CityPath: "/city-b"},
		{RigPath: rig2, RigName: "rig2", CityPath: "/city-a"},
	}
	if err := r.ReconcileRigs(mappings); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 2 {
		t.Fatalf("expected 2 rigs, got %d", len(rigs))
	}

	rigMap := make(map[string]RigEntry)
	for _, e := range rigs {
		rigMap[e.Name] = e
	}

	// rig1: in 2 cities, had default /city-a which is still valid — keep it.
	if rigMap["rig1"].DefaultCity != "/city-a" {
		t.Errorf("rig1 default: expected /city-a, got %s", rigMap["rig1"].DefaultCity)
	}

	// rig2: in 1 city — auto-default.
	if rigMap["rig2"].DefaultCity != "/city-a" {
		t.Errorf("rig2 default: expected /city-a (auto), got %s", rigMap["rig2"].DefaultCity)
	}

	// rig3: not in mappings — should be removed.
	if _, ok := rigMap["rig3"]; ok {
		t.Error("rig3 should have been removed by reconciliation")
	}
}

func TestRigReconcileClearsStaleDefault(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	rigPath := filepath.Join(dir, "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Rig was in city-a (default), now only in city-b.
	if err := r.RegisterRig(rigPath, "myrig", "/city-a"); err != nil {
		t.Fatal(err)
	}

	mappings := []RigCityMapping{
		{RigPath: rigPath, RigName: "myrig", CityPath: "/city-b"},
	}
	if err := r.ReconcileRigs(mappings); err != nil {
		t.Fatal(err)
	}

	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	// Only one city — auto-default should be city-b (old default was stale).
	if rigs[0].DefaultCity != "/city-b" {
		t.Errorf("expected /city-b, got %s", rigs[0].DefaultCity)
	}
}

func TestRigPreservedWhenSavingCities(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "cities.toml"))

	// Register a city and a rig.
	cityPath := filepath.Join(dir, "mycity")
	rigPath := filepath.Join(dir, "myrig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.Register(cityPath, "mycity"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterRig(rigPath, "myrig", cityPath); err != nil {
		t.Fatal(err)
	}

	// Register another city — this calls saveLocked for cities only.
	city2 := filepath.Join(dir, "city2")
	if err := os.MkdirAll(city2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(city2, "city2"); err != nil {
		t.Fatal(err)
	}

	// Rigs must survive the city save.
	rigs, err := r.ListRigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(rigs) != 1 || rigs[0].Name != "myrig" {
		t.Errorf("rig lost after city save: %v", rigs)
	}
}

func TestPathHasPrefix(t *testing.T) {
	tests := []struct {
		path, prefix string
		want         bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false}, // not a dir boundary
		{"/a/b/c", "/a/b/c/d", false},
		{"/a", "/a", true},
	}
	for _, tt := range tests {
		if got := pathHasPrefix(tt.path, tt.prefix); got != tt.want {
			t.Errorf("pathHasPrefix(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
		}
	}
}
