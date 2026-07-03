package importsvc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// stubDeps returns a Deps whose lock sync records the imports it was handed and
// returns a fixed lockfile, so add/remove can exercise the real manifest writes
// without touching the network or git.
func stubDeps(t *testing.T, captured *map[string]config.Import) Deps {
	t.Helper()
	return Deps{
		ResolveVersion: func(_, _ string) (packman.ResolvedVersion, error) {
			return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
		},
		DefaultConstraint: func(_ string) (string, error) { return "^1.4", nil },
		SyncLock: func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			if captured != nil {
				*captured = imports
			}
			return &packman.Lockfile{
				Schema: packman.LockfileSchema,
				Packs: map[string]packman.LockedPack{
					"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
				},
			}, nil
		},
	}
}

func TestAddImportHappyPathWritesManifestAndResult(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	res, err := AddImportWith(
		fsys.OSFS{}, dir,
		"https://github.com/example/tools.git", "", "",
		stubDeps(t, nil),
	)
	if err != nil {
		t.Fatalf("AddImportWith: %v", err)
	}
	if res.Name != "tools" {
		t.Fatalf("Name = %q, want tools", res.Name)
	}
	if res.Source != "https://github.com/example/tools.git" {
		t.Fatalf("Source = %q", res.Source)
	}
	if res.Version != "^1.4" {
		t.Fatalf("Version = %q, want ^1.4", res.Version)
	}
	if !res.GitBacked {
		t.Fatal("GitBacked = false, want true for a remote source")
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	imp, ok := cfg.Imports["tools"]
	if !ok {
		t.Fatalf("imports = %#v, want tools", cfg.Imports)
	}
	if imp.Version != "^1.4" {
		t.Fatalf("imports.tools.version = %q, want ^1.4", imp.Version)
	}
	lock, err := packman.ReadLockfile(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if _, ok := lock.Packs["https://github.com/example/tools.git"]; !ok {
		t.Fatalf("lock = %#v, want tools entry", lock.Packs)
	}
}

func TestAddImportAlreadyExistsReturnsTypedError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"
`)

	deps := Deps{
		SyncLock: func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			t.Fatal("SyncLock must not run when the import already exists")
			return nil, nil
		},
	}
	_, err := AddImportWith(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "^1.4", deps)
	if err == nil {
		t.Fatal("AddImportWith = nil error, want ErrImportExists")
	}
	if !errors.Is(err, ErrImportExists) {
		t.Fatalf("err = %v, want ErrImportExists", err)
	}
}

func TestAddImportBadSourceReturnsInvalidSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	deps := Deps{
		SyncLock: func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			t.Fatal("SyncLock must not run for an invalid source")
			return nil, nil
		},
	}
	// A local path with no pack.toml is not a valid pack target.
	_, err := AddImportWith(fsys.OSFS{}, dir, "./packs/missing", "", "", deps)
	if err == nil {
		t.Fatal("AddImportWith = nil error, want ErrInvalidSource")
	}
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("err = %v, want ErrInvalidSource", err)
	}
}

func TestAddImportVersionOnPathSourceReturnsInvalidSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	localPack := filepath.Join(dir, "packs", "local")
	if err := os.MkdirAll(localPack, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(localPack, "pack.toml"), "[pack]\nname = \"local\"\nschema = 1\n")

	_, err := AddImportWith(fsys.OSFS{}, dir, "./packs/local", "", "^1.2", Deps{})
	if err == nil {
		t.Fatal("AddImportWith = nil error, want ErrInvalidSource for version on path import")
	}
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("err = %v, want ErrInvalidSource", err)
	}
}

func TestAddImportReservedPrefixReturnsReservedPrefixError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	_, err := AddImportWith(fsys.OSFS{}, dir, "https://example.com/worker.git", "default-rig:worker", "^1.0", Deps{})
	if err == nil {
		t.Fatal("AddImportWith = nil error, want ErrReservedPrefix")
	}
	if !errors.Is(err, ErrReservedPrefix) {
		t.Fatalf("err = %v, want ErrReservedPrefix", err)
	}
}

func TestAddImportEmptyDerivedNameReturnsNameDeriveError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	// A bare "https://" trailing-slash source derives to an empty name.
	_, err := AddImportWith(fsys.OSFS{}, dir, "https://", "", "", Deps{
		SyncLock: func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			t.Fatal("SyncLock must not run when the name cannot be derived")
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("AddImportWith = nil error, want ErrNameDerive")
	}
	if !errors.Is(err, ErrNameDerive) {
		t.Fatalf("err = %v, want ErrNameDerive", err)
	}
}

func TestListImportsReturnsDirectRemovableBindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"

[imports.local]
source = "../packs/local"
`)

	imports, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if len(imports) != 2 {
		t.Fatalf("len(imports) = %d, want 2: %#v", len(imports), imports)
	}
	if got := imports["tools"]; got.Source != "https://github.com/example/tools.git" || got.Version != "^1.4" {
		t.Fatalf("tools = %#v", got)
	}
	if got := imports["local"]; got.Source != "../packs/local" {
		t.Fatalf("local = %#v", got)
	}
	// The returned map must be a copy: mutating it must not affect a re-read.
	delete(imports, "tools")
	again, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports (re-read): %v", err)
	}
	if _, ok := again["tools"]; !ok {
		t.Fatal("ListImports returned an aliased map; mutation leaked back")
	}
}

func TestListImportsEmptyPackHasNoBindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), "[pack]\nname = \"demo\"\nschema = 1\n")

	imports, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if len(imports) != 0 {
		t.Fatalf("len(imports) = %d, want 0: %#v", len(imports), imports)
	}
}

func TestListImportsRoundTripsAddedBinding(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	if _, err := AddImportWith(
		fsys.OSFS{}, dir,
		"https://github.com/example/tools.git", "", "",
		stubDeps(t, nil),
	); err != nil {
		t.Fatalf("AddImportWith: %v", err)
	}

	imports, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if _, ok := imports["tools"]; !ok {
		t.Fatalf("added binding not surfaced by ListImports: %#v", imports)
	}
}

// ListImports must surface city.toml root [imports] (which RemoveImport can
// delete and AddImport rejects) so the list namespace matches add/remove.
func TestListImportsSurfacesCityRootImports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), `[workspace]
name = "demo"

[imports.cityonly]
source = "https://github.com/example/cityonly.git"
version = "^3.0"

[imports.tools]
source = "https://github.com/example/city-tools.git"
version = "^9.9"
`)
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"
`)

	imports, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	// A city-only root import, invisible before this fix, is now listed.
	if got, ok := imports["cityonly"]; !ok || got.Source != "https://github.com/example/cityonly.git" {
		t.Fatalf("cityonly = %#v, ok=%v; want the city.toml root import surfaced", imports["cityonly"], ok)
	}
	// city.toml [imports] override wins over the same-named pack.toml entry, so
	// the listed binding matches the effective (removable) one.
	if got := imports["tools"]; got.Source != "https://github.com/example/city-tools.git" {
		t.Fatalf("tools = %#v; want the city.toml override to win", got)
	}
}

// ListImports must surface root default-rig imports keyed "default-rig:<name>",
// the exact form DELETE /packs/{name} accepts to remove them.
func TestListImportsSurfacesDefaultRigImports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), `[workspace]
name = "demo"

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"

[defaults.rig.imports.shared]
source = "https://github.com/example/shared.git"
version = "^2.0"
`)

	imports, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if _, ok := imports["tools"]; !ok {
		t.Fatalf("root import missing: %#v", imports)
	}
	got, ok := imports["default-rig:shared"]
	if !ok || got.Source != "https://github.com/example/shared.git" || got.Version != "^2.0" {
		t.Fatalf("default-rig:shared = %#v, ok=%v; want the default-rig import surfaced", got, ok)
	}
}

func TestRemoveImportFoundRewritesManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/tools.git"
version = "^1.4"
`)

	var captured map[string]config.Import
	deps := Deps{
		SyncLock: func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			captured = imports
			return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
		},
	}
	res, err := RemoveImportWith(fsys.OSFS{}, dir, "tools", deps)
	if err != nil {
		t.Fatalf("RemoveImportWith: %v", err)
	}
	if res.Name != "tools" {
		t.Fatalf("Name = %q, want tools", res.Name)
	}
	if _, ok := captured["pack:tools"]; ok {
		t.Fatalf("synced imports still contain removed import: %#v", captured)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if _, ok := cfg.Imports["tools"]; ok {
		t.Fatal("imports.tools still present after remove")
	}
}

func TestRemoveImportNotFoundReturnsTypedError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")
	writeFile(t, filepath.Join(dir, "pack.toml"), "[pack]\nname = \"demo\"\nschema = 1\n")

	deps := Deps{
		SyncLock: func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			t.Fatal("SyncLock must not run when the import is absent")
			return nil, nil
		},
	}
	_, err := RemoveImportWith(fsys.OSFS{}, dir, "ghost", deps)
	if err == nil {
		t.Fatal("RemoveImportWith = nil error, want ErrNotFound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAddImportDefaultExportsUsePackmanSeams(t *testing.T) {
	// The exported AddImport (no Deps) must wire the package-default seams.
	// Stub the package vars so the happy path runs without the network.
	prevResolve := resolveVersion
	prevConstraint := defaultConstraint
	prevSync := syncLock
	prevWrite := writeLockfile
	t.Cleanup(func() {
		resolveVersion = prevResolve
		defaultConstraint = prevConstraint
		syncLock = prevSync
		writeLockfile = prevWrite
	})
	resolveVersion = func(_, _ string) (packman.ResolvedVersion, error) {
		return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
	}
	defaultConstraint = func(_ string) (string, error) { return "^1.4", nil }
	syncLock = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
	}
	writeLockfile = func(_ fsys.FS, _ string, _ *packman.Lockfile) error { return nil }

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	res, err := AddImport(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "")
	if err != nil {
		t.Fatalf("AddImport: %v", err)
	}
	if res.Name != "tools" || res.Version != "^1.4" {
		t.Fatalf("res = %#v, want tools/^1.4", res)
	}
}

// TestAddImportWithSourcePolicyFencesDirectSource is the regression for the
// transitive-import SSRF fix at the service boundary: a SourcePolicy must fence
// the resolved source before the HEAD probe or lock sync runs, so the injected
// HTTP fence governs the direct git seam (and, via SyncLockWithPolicy, the
// transitive one) rather than only the handler's pre-check.
func TestAddImportWithSourcePolicyFencesDirectSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), "[workspace]\nname = \"demo\"\n")

	sentinel := errors.New("blocked by source policy")
	deps := Deps{
		SourcePolicy: func(string) error { return sentinel },
		ResolveHeadCommit: func(string) (string, error) {
			t.Fatal("HEAD probe must not run when the source policy blocks")
			return "", nil
		},
		ResolveVersion: func(string, string) (packman.ResolvedVersion, error) {
			t.Fatal("version resolve must not run when the source policy blocks")
			return packman.ResolvedVersion{}, nil
		},
		SyncLock: func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			t.Fatal("SyncLock must not run when the source policy blocks")
			return nil, nil
		},
	}
	_, err := AddImportWith(fsys.OSFS{}, dir, "https://github.com/example/tools.git", "", "", deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("AddImportWith err = %v, want the source-policy sentinel", err)
	}
}

// TestRemoveImportPeelsCityOverrideShadowingPack is the regression for the
// GET/DELETE namespace mismatch: when a root name is defined by BOTH pack.toml
// and a city.toml [imports] override, ListImports surfaces the city override as
// the effective binding, so remove must peel that override (not reject it 409).
// The shadowed pack.toml entry stays declared and becomes effective again, and
// lock sync re-points to it rather than dropping the name.
func TestRemoveImportPeelsCityOverrideShadowingPack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), `[workspace]
name = "demo"

[imports.tools]
source = "https://github.com/example/city-tools.git"
version = "^9.9"
`)
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://github.com/example/pack-tools.git"
version = "^1.0"
`)

	// GET surfaces the city override as the effective binding.
	before, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports (before): %v", err)
	}
	if got := before["tools"]; got.Source != "https://github.com/example/city-tools.git" {
		t.Fatalf("listed tools before remove = %#v; want the city override", got)
	}

	var captured map[string]config.Import
	deps := Deps{
		SyncLock: func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			captured = imports
			return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
		},
	}
	// The listed binding must be removable, not a 409.
	res, err := RemoveImportWith(fsys.OSFS{}, dir, "tools", deps)
	if err != nil {
		t.Fatalf("RemoveImportWith(tools) = %v, want peel of the city override", err)
	}
	if res.Name != "tools" {
		t.Fatalf("Name = %q, want tools", res.Name)
	}

	// The pack.toml entry survives the peel and is effective again...
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("Load(pack.toml): %v", err)
	}
	if got, ok := cfg.Imports["tools"]; !ok || got.Source != "https://github.com/example/pack-tools.git" {
		t.Fatalf("pack.toml tools after peel = %#v ok=%v; want the pack entry preserved", got, ok)
	}
	// ...and the city override is gone, so a re-list shows the pack binding.
	after, err := ListImports(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ListImports (after): %v", err)
	}
	if got := after["tools"]; got.Source != "https://github.com/example/pack-tools.git" {
		t.Fatalf("listed tools after remove = %#v; want the pack binding now effective", got)
	}
	// Lock sync must keep tools re-pointed to the pack value, not drop it.
	synced, ok := captured["pack:tools"]
	if !ok || synced.Source != "https://github.com/example/pack-tools.git" {
		t.Fatalf("synced pack:tools = %#v ok=%v; want the pack binding preserved in the lock graph", synced, ok)
	}
}

// TestRemoveRootImportKeepsSameNamedDefaultRig is the regression for the
// lock-sync deletion bug: removing a bare root import named "shared" must NOT
// also drop a same-named "default-rig:shared" binding from the synced graph.
func TestRemoveRootImportKeepsSameNamedDefaultRig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "city.toml"), `[workspace]
name = "demo"

[defaults.rig.imports.shared]
source = "https://github.com/example/dr-shared.git"
version = "^2.0"
`)
	writeFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "demo"
schema = 1

[imports.shared]
source = "https://github.com/example/root-shared.git"
version = "^1.0"
`)

	var captured map[string]config.Import
	deps := Deps{
		SyncLock: func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
			captured = imports
			return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}, nil
		},
	}
	if _, err := RemoveImportWith(fsys.OSFS{}, dir, "shared", deps); err != nil {
		t.Fatalf("RemoveImportWith(shared): %v", err)
	}

	// The removed root import is dropped from the graph...
	if _, ok := captured["pack:shared"]; ok {
		t.Fatalf("synced graph still contains removed root import: %#v", captured)
	}
	// ...but the same-named default-rig binding must survive.
	dr, ok := captured["default-rig:shared"]
	if !ok || dr.Source != "https://github.com/example/dr-shared.git" {
		t.Fatalf("default-rig:shared = %#v ok=%v; a same-named default-rig import was dropped", dr, ok)
	}
}
