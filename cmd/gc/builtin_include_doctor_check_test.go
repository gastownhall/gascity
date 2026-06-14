package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/migrate"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestLegacySystemPacksInclude(t *testing.T) {
	cityPath := "/city"
	for _, tt := range []struct {
		include string
		want    bool
	}{
		{include: ".gc/system/packs/core", want: true},
		{include: ".gc/system/packs/maintenance", want: true},
		{include: "./.gc/system/packs/bd", want: true},
		{include: "/city/.gc/system/packs/core", want: true},
		{include: "packs/maintenance", want: false},
		{include: "rigs/demo/pack", want: false},
		{include: "", want: false},
	} {
		if got := legacySystemPacksInclude(cityPath, tt.include); got != tt.want {
			t.Errorf("legacySystemPacksInclude(%q) = %v, want %v", tt.include, got, tt.want)
		}
	}
}

func writeBuiltinImportTestCity(t *testing.T, cityToml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuiltinImportDoctorCheck_AddsMissingImports(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n")

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for missing core import; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "missing-builtin-import | core") {
		t.Fatalf("Run() details = %v, want missing-builtin-import for core", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core]:\n%s", packData)
	}
	lockData, err := os.ReadFile(filepath.Join(dir, "packs.lock"))
	if err != nil {
		t.Fatalf("packs.lock after fix: %v", err)
	}
	if !strings.Contains(string(lockData), "commit") {
		t.Fatalf("packs.lock after fix has no entries:\n%s", lockData)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinImportDoctorCheck_MigratesLegacySystemPacksIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/maintenance", ".gc/system/packs/core", "rigs/demo/pack"]

[beads]
provider = "file"
`)
	if err := os.MkdirAll(filepath.Join(dir, "rigs", "demo", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rigs", "demo", "pack", "pack.toml"), []byte("[pack]\nname = \"demo-pack\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for legacy includes; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "legacy-system-packs-include") {
		t.Fatalf("Run() details = %v, want legacy-system-packs-include entry", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after fix still references .gc/system/packs:\n%s", cityData)
	}
	if !strings.Contains(string(cityData), "rigs/demo/pack") {
		t.Fatalf("city.toml after fix lost the non-builtin include:\n%s", cityData)
	}
	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core]:\n%s", packData)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestBuiltinImportDoctorCheck_MigratesOptionalCanonicalWorkspaceIncludes
// pins the conversion half of the workspace.includes migration for packs
// outside the required set: a legacy workspace include of an optional
// canonical pack (gastown) must land a pinned canonical-source import in
// pack.toml before the include is stripped, so the composed pack set
// never narrows — stripping without landing would silently drop the pack
// and let the boundary prune delete its only composition route.
func TestBuiltinImportDoctorCheck_MigratesOptionalCanonicalWorkspaceIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/gastown"]

[beads]
provider = "file"
`)
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "gastown")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(dir, io.Discard)
	if err != nil {
		t.Fatalf("loading fixture config: %v", err)
	}
	if !config.ReachablePackNames(cfg, fsys.OSFS{}, dir)["gastown"] {
		t.Fatal("fixture does not compose gastown through the legacy include")
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for the legacy include; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "legacy-system-packs-include | .gc/system/packs/gastown") {
		t.Fatalf("Run() details = %v, want legacy-system-packs-include entry for gastown", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after fix still references .gc/system/packs:\n%s", cityData)
	}
	gastownSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	packManifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("re-reading pack.toml manifest: %v", err)
	}
	wantGastown := config.Import{Source: gastownSource, Version: config.BundledSourcePinnedVersion(gastownSource)}
	if got := packManifest.Imports["gastown"]; got != wantGastown {
		t.Errorf("pack.toml import gastown after fix = %+v, want %+v (the stripped include's replacement must land)", got, wantGastown)
	}

	cfg, err = loadCityConfigWithoutBuiltinPackRefresh(dir, io.Discard)
	if err != nil {
		t.Fatalf("loading migrated config: %v", err)
	}
	if !config.ReachablePackNames(cfg, fsys.OSFS{}, dir)["gastown"] {
		t.Fatal("gastown is no longer reachable after Fix — the migration narrowed the composed pack set")
	}

	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
}

// TestMigrateThenDoctorCompletesBuiltinSystemPackConversion documents and pins
// the intended two-step migration for the retired .gc/system/packs surface:
// `gc migrate` deliberately preserves canonical builtin system-pack includes
// so a city authored by an older binary keeps composing through the migrate
// step, and the follow-up `gc doctor --fix` converts each one to a pinned
// [imports] entry and prunes the retired tree. It guards the contract
// described in config.IsBuiltinSystemPackInclude and internal/migrate against
// drifting back to either "migrate alone fully migrates" or "migrate strips
// builtin includes".
func TestMigrateThenDoctorCompletesBuiltinSystemPackConversion(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/gastown"]

[beads]
provider = "file"
`)
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "gastown")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Step 1: `gc migrate` preserves the builtin system-pack include — it does
	// not convert it — so the migrated city still references .gc/system/packs
	// and stays composable until the doctor step runs.
	if _, err := migrate.Apply(dir, migrate.Options{}); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cityData), ".gc/system/packs/gastown") {
		t.Fatalf("migrate must preserve the builtin system-pack include for the doctor step; city.toml:\n%s", cityData)
	}

	// Step 2: the follow-up `gc doctor --fix` completes the conversion to a
	// pinned [imports] entry and prunes the retired tree.
	check := newBuiltinImportDoctorCheck(dir)
	if err := check.Fix(nil); err != nil {
		t.Fatalf("doctor Fix(): %v", err)
	}
	cityData, err = os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after doctor --fix still references .gc/system/packs:\n%s", cityData)
	}
	gastownSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	packManifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("re-reading pack.toml manifest: %v", err)
	}
	wantGastown := config.Import{Source: gastownSource, Version: config.BundledSourcePinnedVersion(gastownSource)}
	if got := packManifest.Imports["gastown"]; got != wantGastown {
		t.Errorf("pack.toml import gastown after the migrate→doctor two-step = %+v, want %+v", got, wantGastown)
	}

	// The two-step output must be self-consistent: the doctor check is clean
	// on the fully converted city.
	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("doctor Run() after the two-step status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestBuiltinImportDoctorCheck_KeepsNonBuiltinWorkspaceIncludes pins the
// preservation half of the workspace.includes migration: a workspace
// include of a pack under the retired tree with no canonical bundled
// import has no automatic modern form, so Run must report it as a manual
// migration (not promise an auto-fix), Fix must keep the include, and the
// kept reference must hold the prune gate closed so the user-authored
// pack content is never deleted out from under its only composition
// route.
func TestBuiltinImportDoctorCheck_KeepsNonBuiltinWorkspaceIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/mypack"]

[beads]
provider = "file"
`)
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "mypack")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"mypack\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stale, "user-content.md")
	if err := os.WriteFile(marker, []byte("hand-authored content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for the non-builtin include; message=%s", r.Status, r.Message)
	}
	details := strings.Join(r.Details, "\n")
	if !strings.Contains(details, "legacy-system-packs-manual | city.toml workspace.includes | .gc/system/packs/mypack") {
		t.Fatalf("Run() details do not report the non-builtin include as manual:\n%s", details)
	}
	if !strings.Contains(details, "references a non-builtin pack") {
		t.Fatalf("Run() details missing the non-builtin manual instruction:\n%s", details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cityData), ".gc/system/packs/mypack") {
		t.Fatalf("city.toml after fix lost the non-builtin include — Fix stripped a route it cannot replace:\n%s", cityData)
	}
	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if strings.Contains(string(packData), "mypack") {
		t.Fatalf("pack.toml after fix imports the non-builtin pack:\n%s", packData)
	}

	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(dir, io.Discard)
	if err != nil {
		t.Fatalf("loading config after fix: %v", err)
	}
	if !config.ReachablePackNames(cfg, fsys.OSFS{}, dir)["mypack"] {
		t.Fatal("mypack is no longer reachable after Fix")
	}

	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("user-authored pack content deleted by prune while still referenced (stat err = %v)", err)
	}

	if r := check.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("Run() after fix status = %v, want error while the manual reference remains; message=%s", r.Status, r.Message)
	}
}

// TestBuiltinImportDoctorCheck_MigratesLegacyRigImportRoutes pins the
// automatic migration for the city.toml composition routes beyond
// workspace.includes: rig import sources written as
// .gc/system/packs/<name> (the canonical form released binaries wrote for
// `gc rig add --include` until this branch), legacy rig includes, and
// legacy workspace.default_rig_includes. All three must be detected by
// Run with actionable details and rewritten by Fix to pinned canonical
// bundled imports, after which the retired tree prunes.
func TestBuiltinImportDoctorCheck_MigratesLegacyRigImportRoutes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
default_rig_includes = [".gc/system/packs/core"]

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = "demo"
includes = [".gc/system/packs/bd"]

[rigs.imports.core]
source = ".gc/system/packs/core"
`)
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for legacy rig routes; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{
		"workspace.default_rig_includes",
		"rigs[demo].includes",
		"rigs[demo].imports.core.source",
	} {
		if !strings.Contains(details, want) {
			t.Errorf("Run() details missing %q:\n%s", want, details)
		}
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after fix still references .gc/system/packs:\n%s", cityData)
	}
	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core] — Fix must ensure required builtin imports in the pack.toml manifest, the surface the lockfile collection reads:\n%s", packData)
	}

	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	bdSource, ok := builtinpacks.CanonicalImportSource("bd")
	if !ok {
		t.Fatal("bundled bd pack not registered")
	}
	after, err := loadCityImportManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("re-reading city.toml manifest: %v", err)
	}
	if len(after.Rigs) != 1 {
		t.Fatalf("city.toml after fix has %d rigs, want 1", len(after.Rigs))
	}
	wantCore := config.Import{Source: coreSource, Version: config.BundledSourcePinnedVersion(coreSource)}
	wantBd := config.Import{Source: bdSource, Version: config.BundledSourcePinnedVersion(bdSource)}
	if got := after.Rigs[0].Imports["core"]; got != wantCore {
		t.Errorf("rig import core after fix = %+v, want %+v", got, wantCore)
	}
	if got := after.Rigs[0].Imports["bd"]; got != wantBd {
		t.Errorf("rig include bd was not converted to a pinned bundled import: got %+v, want %+v", got, wantBd)
	}
	if len(after.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes after fix = %v, want none", after.Rigs[0].Includes)
	}
	if got := after.Defaults.Rig.Imports["core"]; got != wantCore {
		t.Errorf("default-rig import core after fix = %+v, want %+v", got, wantCore)
	}
	if got := after.Workspace.LegacyDefaultRigIncludes(); len(got) != 0 {
		t.Errorf("workspace.default_rig_includes after fix = %v, want none", got)
	}

	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
}

// TestBuiltinImportDoctorCheck_MigrationPreservesCollidingImportBindings
// pins the collision contract for the automatic include migration: when a
// legacy default-rig or rig include migrates to a bundled import whose
// natural binding is already occupied by a different import, the bundled
// import must land under a fresh unique binding — the same uniquification
// policy normal composition applies to colliding legacy includes
// (config.AddOrderedLegacyImports) — instead of being skipped. Stripping
// the include without landing its replacement import would silently
// narrow the composed pack set.
func TestBuiltinImportDoctorCheck_MigrationPreservesCollidingImportBindings(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
default_rig_includes = [".gc/system/packs/core"]

[beads]
provider = "file"

[defaults.rig.imports.core]
source = "./packs/altcore"

[[rigs]]
name = "demo"
path = "demo"
includes = [".gc/system/packs/bd"]

[rigs.imports.bd]
source = "./packs/altbd"
`)
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, alt := range []string{"altcore", "altbd"} {
		altDir := filepath.Join(dir, "packs", alt)
		if err := os.MkdirAll(altDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(altDir, "pack.toml"), []byte("[pack]\nname = \""+alt+"\"\nschema = 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, stale := range []string{"core", "bd"} {
		staleDir := filepath.Join(dir, citylayout.SystemPacksRoot, stale)
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "pack.toml"), []byte("[pack]\nname = \""+stale+"\"\nschema = 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	check := newBuiltinImportDoctorCheck(dir)
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	after, err := loadCityImportManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("re-reading city.toml manifest: %v", err)
	}
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	bdSource, ok := builtinpacks.CanonicalImportSource("bd")
	if !ok {
		t.Fatal("bundled bd pack not registered")
	}
	wantCore := config.Import{Source: coreSource, Version: config.BundledSourcePinnedVersion(coreSource)}
	wantBd := config.Import{Source: bdSource, Version: config.BundledSourcePinnedVersion(bdSource)}

	if got := after.Workspace.LegacyDefaultRigIncludes(); len(got) != 0 {
		t.Errorf("workspace.default_rig_includes after fix = %v, want none", got)
	}
	if got, want := after.Defaults.Rig.Imports["core"], (config.Import{Source: "./packs/altcore"}); got != want {
		t.Errorf("occupied default-rig binding core after fix = %+v, want untouched %+v", got, want)
	}
	if got := after.Defaults.Rig.Imports["core-2"]; got != wantCore {
		t.Errorf("default-rig import core-2 after fix = %+v, want %+v (migrated include must land on a unique binding)", got, wantCore)
	}
	if len(after.Rigs) != 1 {
		t.Fatalf("city.toml after fix has %d rigs, want 1", len(after.Rigs))
	}
	if got := after.Rigs[0].Includes; len(got) != 0 {
		t.Errorf("rig includes after fix = %v, want none", got)
	}
	if got, want := after.Rigs[0].Imports["bd"], (config.Import{Source: "./packs/altbd"}); got != want {
		t.Errorf("occupied rig binding bd after fix = %+v, want untouched %+v", got, want)
	}
	if got := after.Rigs[0].Imports["bd-2"]; got != wantBd {
		t.Errorf("rig import bd-2 after fix = %+v, want %+v (migrated include must land on a unique binding)", got, wantBd)
	}

	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
}

// TestBuiltinImportDoctorCheck_FixLandsRequiredImportOnCollidingPackBinding
// pins the collision contract for the required-import merge (Fix step 1),
// the conversion route for legacy workspace includes: when a missing
// required builtin import's natural pack.toml binding is already occupied
// by a different source, the bundled import must land under a fresh
// unique binding — exactly like the city.toml include conversions — never
// be skipped. Skipping while step 2 strips the legacy workspace include
// unconditionally would destroy the include's composition route without
// landing its replacement, leaving Run red forever while every re-run of
// Fix no-ops and claims success.
func TestBuiltinImportDoctorCheck_FixLandsRequiredImportOnCollidingPackBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/core"]

[beads]
provider = "file"
`)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "demo"
schema = 2

[imports.core]
source = "./packs/standards"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	altDir := filepath.Join(dir, "packs", "standards")
	if err := os.MkdirAll(altDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(altDir, "pack.toml"), []byte("[pack]\nname = \"standards\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after fix still references .gc/system/packs:\n%s", cityData)
	}
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	packManifest, err := loadCityPackManifestFS(fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("re-reading pack.toml manifest: %v", err)
	}
	if got, want := packManifest.Imports["core"], (config.Import{Source: "./packs/standards"}); got != want {
		t.Errorf("occupied pack.toml binding core after fix = %+v, want untouched %+v", got, want)
	}
	wantCore := config.Import{Source: coreSource, Version: config.BundledSourcePinnedVersion(coreSource)}
	if got := packManifest.Imports["core-2"]; got != wantCore {
		t.Errorf("pack.toml import core-2 after fix = %+v, want %+v (required import must land on a unique binding, not be skipped)", got, wantCore)
	}

	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK after one Fix; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
}

// TestBuiltinImportDoctorCheck_MigrationDedupsSameSourceImports pins the
// no-op half of the collision contract and its semantic-equivalence
// limits: a legacy include whose pack is already imported from the
// canonical bundled source — under any binding, including a legacy-source
// binding the same Fix pass rewrites — is a duplicate composition route,
// so the include is stripped and no second import lands. Only a binding
// with default option semantics can stand in for the converted import: a
// non-transitive (or exported / shadow-silenced) same-source import
// composes something narrower or different, so the bundled import must
// still land. A versionless same-source binding gains the canonical pin
// in place — a conversion must never leave a bundled source unpinned.
func TestBuiltinImportDoctorCheck_MigrationDedupsSameSourceImports(t *testing.T) {
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	corePin := config.BundledSourcePinnedVersion(coreSource)
	canonical := config.Import{Source: coreSource, Version: corePin}
	for _, tt := range []struct {
		name        string
		importBlock string
		wantImports map[string]config.Import
	}{
		{
			name:        "canonical-source-under-other-binding",
			importBlock: "[defaults.rig.imports.mycore]\nsource = \"" + coreSource + "\"\nversion = \"" + corePin + "\"\n",
			wantImports: map[string]config.Import{"mycore": canonical},
		},
		{
			name:        "legacy-source-on-natural-binding",
			importBlock: "[defaults.rig.imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantImports: map[string]config.Import{"core": canonical},
		},
		{
			name:        "transitive-disabled-same-source-still-lands-bundled-import",
			importBlock: "[defaults.rig.imports.mycore]\nsource = \"" + coreSource + "\"\nversion = \"" + corePin + "\"\ntransitive = false\n",
			wantImports: map[string]config.Import{
				"mycore": {Source: coreSource, Version: corePin, Transitive: boolPtr(false)},
				"core":   canonical,
			},
		},
		{
			name:        "versionless-same-source-gains-canonical-pin-in-place",
			importBlock: "[defaults.rig.imports.mycore]\nsource = \"" + coreSource + "\"\n",
			wantImports: map[string]config.Import{"mycore": canonical},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
default_rig_includes = [".gc/system/packs/core"]

[beads]
provider = "file"

`+tt.importBlock)
			stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
			if err := os.MkdirAll(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			check := newBuiltinImportDoctorCheck(dir)
			if err := check.Fix(nil); err != nil {
				t.Fatalf("Fix(): %v", err)
			}
			after, err := loadCityImportManifestFS(fsys.OSFS{}, dir)
			if err != nil {
				t.Fatalf("re-reading city.toml manifest: %v", err)
			}
			if got := after.Workspace.LegacyDefaultRigIncludes(); len(got) != 0 {
				t.Errorf("workspace.default_rig_includes after fix = %v, want none", got)
			}
			assertImportMapEqual(t, after.Defaults.Rig.Imports, tt.wantImports)
		})
	}
}

// TestBuiltinImportDoctorCheck_FixSurfacesSameSourcePinConflict pins the
// refusal half of the conversion contract: when a legacy include's
// bundled import would land beside a same-source binding that has
// non-default option semantics AND a different explicit pin, Fix must
// fail with a targeted error naming the conflicting binding BEFORE
// mutating city.toml — landing the second pin would wedge every later
// sync on "incompatible pinned versions" with the legacy include already
// stripped, a state only a hand edit can unwind.
func TestBuiltinImportDoctorCheck_FixSurfacesSameSourcePinConflict(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	cityToml := `[workspace]
name = "demo"
default_rig_includes = [".gc/system/packs/core"]

[beads]
provider = "file"

[defaults.rig.imports.mycore]
source = "` + coreSource + `"
version = "sha:4242424242424242424242424242424242424242"
transitive = false
`
	dir := writeBuiltinImportTestCity(t, cityToml)
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	err := check.Fix(nil)
	if err == nil {
		t.Fatal("Fix() = nil, want a same-source pin-conflict error")
	}
	if !strings.Contains(err.Error(), "mycore") || !strings.Contains(err.Error(), "two pins") {
		t.Fatalf("Fix() error = %v, want it to name the conflicting binding and the two-pin state", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cityData) != cityToml {
		t.Fatalf("Fix mutated city.toml despite the conflict:\n%s", cityData)
	}
}

// assertImportMapEqual compares import maps field-wise: Transitive is a
// pointer, so plain struct equality would compare pointer identity
// instead of the configured value.
func assertImportMapEqual(t *testing.T, got, want map[string]config.Import) {
	t.Helper()
	for binding, w := range want {
		g, ok := got[binding]
		if !ok {
			t.Errorf("import binding %q missing; got %+v", binding, got)
			continue
		}
		if g.Source != w.Source || g.Version != w.Version || g.Export != w.Export ||
			g.ImportIsTransitive() != w.ImportIsTransitive() ||
			strings.TrimSpace(g.Shadow) != strings.TrimSpace(w.Shadow) {
			t.Errorf("import %q = %+v, want %+v", binding, g, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("imports = %+v, want exactly %d binding(s): %+v", got, len(want), want)
	}
}

// TestEnsureBundledImportBindingSemanticEquivalence pins the dedup
// predicate directly: only a same-source binding with the default option
// semantics composition's reuse policy requires
// (config.Import.HasDefaultOptionSemantics) can stand in for the
// converted bundled import. The version policy: an explicit user pin on
// the canonical source is respected as the duplicate route (packs.lock
// keys by source, so the same source must never land at a second pin),
// and a versionless binding gains the canonical pin in place. A
// same-source binding with NON-default options at a different pin is
// refused with a targeted conflict error — landing beside it would put
// one source at two pins, which every later sync rejects — unless a
// pinned default-semantics binding also exists, in which case the dedup
// wins and nothing lands. Everything else falls through to
// natural-or-unique binding allocation.
func TestEnsureBundledImportBindingSemanticEquivalence(t *testing.T) {
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	corePin := config.BundledSourcePinnedVersion(coreSource)
	bundled := config.Import{Source: coreSource, Version: corePin}
	userPin := "sha:4242424242424242424242424242424242424242"
	for _, tt := range []struct {
		name    string
		imports map[string]config.Import
		want    map[string]config.Import
		wantErr string
	}{
		{
			name:    "nil-map-lands-natural-binding",
			imports: nil,
			want:    map[string]config.Import{"core": bundled},
		},
		{
			name:    "different-source-occupies-natural-binding",
			imports: map[string]config.Import{"core": {Source: "./packs/alt"}},
			want: map[string]config.Import{
				"core":   {Source: "./packs/alt"},
				"core-2": bundled,
			},
		},
		{
			name:    "canonical-pin-dedups",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: corePin}},
			want:    map[string]config.Import{"mycore": {Source: coreSource, Version: corePin}},
		},
		{
			name:    "explicit-user-pin-dedups",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: userPin}},
			want:    map[string]config.Import{"mycore": {Source: coreSource, Version: userPin}},
		},
		{
			name:    "versionless-gains-canonical-pin-in-place",
			imports: map[string]config.Import{"mycore": {Source: coreSource}},
			want:    map[string]config.Import{"mycore": bundled},
		},
		{
			name:    "exported-same-source-lands-bundled-import",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: corePin, Export: true}},
			want: map[string]config.Import{
				"mycore": {Source: coreSource, Version: corePin, Export: true},
				"core":   bundled,
			},
		},
		{
			name:    "transitive-disabled-same-source-lands-bundled-import",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: corePin, Transitive: boolPtr(false)}},
			want: map[string]config.Import{
				"mycore": {Source: coreSource, Version: corePin, Transitive: boolPtr(false)},
				"core":   bundled,
			},
		},
		{
			name:    "shadow-silent-same-source-lands-bundled-import",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: corePin, Shadow: "silent"}},
			want: map[string]config.Import{
				"mycore": {Source: coreSource, Version: corePin, Shadow: "silent"},
				"core":   bundled,
			},
		},
		{
			name:    "shadow-warn-same-source-dedups",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: corePin, Shadow: "warn"}},
			want:    map[string]config.Import{"mycore": {Source: coreSource, Version: corePin, Shadow: "warn"}},
		},
		{
			name:    "transitive-disabled-different-pin-conflicts",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Version: userPin, Transitive: boolPtr(false)}},
			want:    map[string]config.Import{"mycore": {Source: coreSource, Version: userPin, Transitive: boolPtr(false)}},
			wantErr: "mycore",
		},
		{
			name:    "transitive-disabled-versionless-lands-bundled-import",
			imports: map[string]config.Import{"mycore": {Source: coreSource, Transitive: boolPtr(false)}},
			want: map[string]config.Import{
				"mycore": {Source: coreSource, Transitive: boolPtr(false)},
				"core":   bundled,
			},
		},
		{
			name: "pinned-default-binding-dedups-over-conflicting-pin",
			imports: map[string]config.Import{
				"mycore":  {Source: coreSource, Version: corePin},
				"shallow": {Source: coreSource, Version: userPin, Transitive: boolPtr(false)},
			},
			want: map[string]config.Import{
				"mycore":  {Source: coreSource, Version: corePin},
				"shallow": {Source: coreSource, Version: userPin, Transitive: boolPtr(false)},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ensureBundledImportBinding(tt.imports, "core", bundled)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ensureBundledImportBinding() err = %v, want a pin-conflict error naming %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("ensureBundledImportBinding(): %v", err)
			}
			assertImportMapEqual(t, got, tt.want)
		})
	}
}

// TestDoDoctorFixConvergesWave1CityRootImportsThroughImportState pins the
// doctor-check handoff for city.toml's top-level [imports] table: the
// builtin-pack-imports check runs before packv2-import-state, but wave-1
// public-pack sources there are still owned by packv2-import-state. The first
// check must not report them as manual or it can block the later automatic
// rewrite.
func TestDoDoctorFixConvergesWave1CityRootImportsThroughImportState(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("core builtin source missing")
	}
	coreVersion := config.BundledSourcePinnedVersion(coreSource)
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"

[beads]
provider = "file"

[imports.core]
source = "`+coreSource+`"
version = "`+coreVersion+`"

[imports.legacy-gastown]
source = ".gc/system/packs/gastown"

[imports.legacy-maintenance]
source = ".gc/system/packs/maintenance"
`)
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 2
`)
	for _, stale := range []string{"gastown", "maintenance"} {
		staleDir := filepath.Join(dir, citylayout.SystemPacksRoot, stale)
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "pack.toml"), []byte("[pack]\nname = \""+stale+"\"\nschema = 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prevCityFlag := cityFlag
	prevResolve := resolveWave1PublicPackImports
	prevSync := syncImports
	prevInstall := installLockedImports
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		resolveWave1PublicPackImports = prevResolve
		syncImports = prevSync
		installLockedImports = prevInstall
		checkInstalledImports = prevCheck
	})
	cityFlag = dir
	t.Setenv("GC_CITY_PATH", dir)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	targets := map[string]wave1PublicPackImportTarget{
		"gastown": {
			Binding: "legacy-gastown",
			Import:  config.Import{Source: "https://packages.example/gastown.git", Version: "^1.2"},
		},
		"maintenance": {Binding: "legacy-maintenance", Remove: true},
	}
	resolveWave1PublicPackImports = func(_ []string) (map[string]wave1PublicPackImportTarget, error) {
		return targets, nil
	}
	syncImports = func(_ string, imports map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		packs := make(map[string]packman.LockedPack, len(imports))
		for _, imp := range imports {
			source := strings.TrimSpace(imp.Source)
			if source == "" {
				continue
			}
			packs[source] = packman.LockedPack{Version: "1.2.3", Commit: "abc123"}
		}
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: packs}, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		return packman.ReadLockfile(fsys.OSFS{}, cityRoot)
	}
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doDoctor(true, false, false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc doctor --fix = %d, want 0; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	output := stdout.String() + stderr.String()
	if strings.Contains(output, "legacy-system-packs-manual | city.toml imports.") {
		t.Fatalf("builtin-pack-imports reported city-root imports as manual before import-state could fix them:\n%s", output)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cityText := string(cityData)
	if strings.Contains(cityText, ".gc/system/packs/") {
		t.Fatalf("city.toml still references the retired system-packs tree after doctor --fix:\n%s", cityText)
	}
	if !strings.Contains(cityText, "https://packages.example/gastown.git") {
		t.Fatalf("city.toml root gastown import was not rewritten by packv2-import-state:\n%s", cityText)
	}
	if strings.Contains(cityText, "legacy-maintenance") {
		t.Fatalf("city.toml root maintenance import was not removed by packv2-import-state:\n%s", cityText)
	}
	if r := newImportStateDoctorCheck(dir).Run(&doctor.CheckContext{CityPath: dir}); r.Status != doctor.StatusOK {
		t.Fatalf("packv2-import-state after doctor --fix = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestBuiltinImportDoctorCheck_ReportsWave1FragmentImportSourcesAsManual
// pins the fragment half of the wave-1 reporting hand-off: the
// packv2-import-state check's surface readers are single-file loaders
// that never merge config fragments, so wave-1 public-pack import sources
// declared in fragments must be reported here as manual migrations — like
// the city.toml top-level [imports] table, they keep the retired tree
// preserved, and no other check names them. Fix must not rewrite
// fragments.
func TestBuiltinImportDoctorCheck_ReportsWave1FragmentImportSourcesAsManual(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `include = ["legacy-wave1.toml"]

[workspace]
name = "demo"

[beads]
provider = "file"
`)
	fragment := `[[rigs]]
name = "demo"
path = "demo"

[rigs.imports.gt]
source = ".gc/system/packs/gastown"

[rigs.imports.legacy-maintenance]
source = ".gc/system/packs/maintenance"
`
	fragPath := filepath.Join(dir, "legacy-wave1.toml")
	if err := os.WriteFile(fragPath, []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"gastown", "maintenance"} {
		staleDir := filepath.Join(dir, citylayout.SystemPacksRoot, stale)
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "pack.toml"), []byte("[pack]\nname = \""+stale+"\"\nschema = 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for wave-1 fragment import sources; message=%s details=%v", r.Status, r.Message, r.Details)
	}
	details := strings.Join(r.Details, "\n")
	if !strings.Contains(details, "legacy-system-packs-manual | legacy-wave1.toml rigs[demo].imports.gt.source") {
		t.Errorf("Run() details missing manual entry for the fragment gastown import source:\n%s", details)
	}
	if !strings.Contains(details, "public gascity-packs source") {
		t.Errorf("Run() details missing public-source guidance for the fragment gastown import:\n%s", details)
	}
	if !strings.Contains(details, "legacy-system-packs-manual | legacy-wave1.toml rigs[demo].imports.legacy-maintenance.source") {
		t.Errorf("Run() details missing manual entry for the fragment maintenance import source:\n%s", details)
	}
	if !strings.Contains(details, "folded into the bundled core pack") {
		t.Errorf("Run() details missing maintenance removal guidance:\n%s", details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}
	fragData, err := os.ReadFile(fragPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(fragData) != fragment {
		t.Fatalf("Fix rewrote the user-authored fragment:\n%s", fragData)
	}
}

// TestLegacyCityUpgradeWindowFragmentIncludeKeepsBuiltins mirrors
// TestLegacyCityUpgradeWindowKeepsBuiltinsUntilDoctorFix for a legacy
// system-pack include declared in a config fragment. Fragments are
// user-authored files the doctor must not rewrite (a decode/re-marshal
// round trip drops comments and unknown content), so the migration story
// is detect-and-instruct: the gate preserves the tree, `gc doctor` names
// the fragment and the edit, `gc doctor --fix` ensures the required
// imports up front — even while the pack is still reachable through the
// preserved tree — so the instructed edit is safe whenever the user makes
// it, and the first boundary pass after the edit prunes the tree.
func TestLegacyCityUpgradeWindowFragmentIncludeKeepsBuiltins(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `include = ["legacy-builtin.toml"]

[workspace]
name = "demo"

[beads]
provider = "file"
`)
	fragPath := filepath.Join(dir, "legacy-builtin.toml")
	if err := os.WriteFile(fragPath, []byte("[workspace]\nincludes = [\".gc/system/packs/core\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First contact with the new binary: the boundary preserves the tree
	// because the fragment still composes through it.
	var warnings bytes.Buffer
	if err := EnsureBuiltinRuntimeAssets(dir, &warnings); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("legacy tree pruned despite the fragment-hosted include (stat err = %v)", err)
	}
	if !strings.Contains(warnings.String(), `run "gc doctor --fix"`) {
		t.Errorf("upgrade warning does not point at the doctor migration: %q", warnings.String())
	}

	// The composed config still reaches core through the preserved
	// fragment include — no silent degraded window.
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(dir, io.Discard)
	if err != nil {
		t.Fatalf("loading legacy fragment city config: %v", err)
	}
	if missing := missingRequiredBuiltinImports(fsys.OSFS{}, cfg, dir); len(missing) != 0 {
		t.Fatalf("fragment city composes without %v during the upgrade window", missing)
	}

	// The doctor names the fragment and the manual edit.
	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for the fragment-hosted include; message=%s", r.Status, r.Message)
	}
	details := strings.Join(r.Details, "\n")
	if !strings.Contains(details, "legacy-system-packs-manual | legacy-builtin.toml workspace.includes") {
		t.Fatalf("Run() details do not name the fragment edit:\n%s", details)
	}

	// --fix ensures the core import even though core is still reachable
	// through the preserved tree (reachability through legacy routes must
	// not mask the missing import), and leaves the fragment untouched.
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}
	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core] (legacy reachability masked the missing import):\n%s", packData)
	}
	fragData, err := os.ReadFile(fragPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fragData), ".gc/system/packs/core") {
		t.Fatalf("doctor rewrote the user-authored fragment:\n%s", fragData)
	}

	// Until the user edits the fragment, the check keeps instructing and
	// the tree stays preserved.
	if r := check.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("Run() after fix status = %v, want error while the fragment reference remains; message=%s", r.Status, r.Message)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("tree pruned while the fragment still composes through it (stat err = %v)", err)
	}

	// The user performs the instructed edit; the next boundary pass
	// prunes, and the check reports healthy.
	if err := os.WriteFile(fragPath, []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after the fragment edit", citylayout.SystemPacksRoot, err)
	}
	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fragment edit status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestLegacyCityUpgradeWindowKeepsBuiltinsUntilDoctorFix pins the composed
// legacy upgrade story end to end: a city whose city.toml still lists
// .gc/system/packs includes meets the new binary. The config-load boundary
// must preserve the materialized tree (so the city keeps composing the
// builtin packs it references), `gc doctor --fix` migrates the includes to
// pinned [imports], and only then is the retired tree pruned.
func TestLegacyCityUpgradeWindowKeepsBuiltinsUntilDoctorFix(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/core"]

[beads]
provider = "file"
`)
	stale := filepath.Join(dir, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First contact with the new binary: the boundary preserves the tree
	// because the manifest still composes through it, and the warning points
	// at the doctor migration.
	var warnings bytes.Buffer
	if err := EnsureBuiltinRuntimeAssets(dir, &warnings); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("legacy tree pruned before the include migration (stat err = %v)", err)
	}
	if !strings.Contains(warnings.String(), `run "gc doctor --fix"`) {
		t.Errorf("upgrade warning does not point at the doctor migration: %q", warnings.String())
	}

	// The composed config still reaches core through the preserved include —
	// the city never enters the degraded composes-without-core window.
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(dir, io.Discard)
	if err != nil {
		t.Fatalf("loading legacy city config: %v", err)
	}
	if missing := missingRequiredBuiltinImports(fsys.OSFS{}, cfg, dir); len(missing) != 0 {
		t.Fatalf("legacy city composes without %v during the upgrade window", missing)
	}

	// gc doctor --fix migrates the includes to pinned imports.
	check := newBuiltinImportDoctorCheck(dir)
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	// The migrated manifest no longer composes through the tree: the next
	// boundary pass prunes it, and the doctor check reports healthy.
	pruneRetiredSystemPacks(dir, io.Discard)
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
	if r := check.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("Run() after migration status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinImportDoctorCheck_OKAfterInit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc init = %d; stderr=%s", code, stderr.String())
	}
	r := newBuiltinImportDoctorCheck(dir).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestStatusWarnsOnMissingBuiltinImports pins the user-visible migration
// warning end to end: a city.toml without the builtin imports must surface
// the once-per-city warning on a real command's stderr, even though earlier
// silent config pre-loads (io.Discard writers) run first in the same process
// and must not consume the warning slot.
func TestStatusWarnsOnMissingBuiltinImports(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte("workspace_name = \"legacy\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc status = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not import required builtin pack(s) core") {
		t.Fatalf("stderr missing builtin-import warning: %q", stderr.String())
	}
}

// TestRewriteLegacyBundledImportSourcesPreservesOptions guards the
// direct-import migration finding: rewriting a legacy .gc/system/packs import
// source to its canonical bundled equivalent must change only the source and
// pin, never the import's load-bearing composition options (export,
// transitive, shadow). Dropping them would silently alter composition during
// "gc doctor --fix".
func TestRewriteLegacyBundledImportSourcesPreservesOptions(t *testing.T) {
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	coreVersion := config.BundledSourcePinnedVersion(coreSource)
	transitiveFalse := false

	for _, tc := range []struct {
		name string
		imp  config.Import
	}{
		{name: "exported", imp: config.Import{Source: ".gc/system/packs/core", Export: true}},
		{name: "non_transitive", imp: config.Import{Source: ".gc/system/packs/core", Transitive: &transitiveFalse}},
		{name: "shadow_silent", imp: config.Import{Source: ".gc/system/packs/core", Shadow: "silent"}},
		{name: "all_options", imp: config.Import{Source: ".gc/system/packs/core", Export: true, Transitive: &transitiveFalse, Shadow: "silent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			imports := map[string]config.Import{"core": tc.imp}
			if !rewriteLegacyBundledImportSources(cityPath, imports) {
				t.Fatal("rewriteLegacyBundledImportSources reported no change for a legacy bundled source")
			}
			got := imports["core"]
			if got.Source != coreSource {
				t.Errorf("source = %q, want canonical %q", got.Source, coreSource)
			}
			if got.Version != coreVersion {
				t.Errorf("version = %q, want canonical pin %q", got.Version, coreVersion)
			}
			if got.Export != tc.imp.Export {
				t.Errorf("export = %v, want preserved %v", got.Export, tc.imp.Export)
			}
			switch {
			case (got.Transitive == nil) != (tc.imp.Transitive == nil):
				t.Errorf("transitive = %v, want preserved %v", got.Transitive, tc.imp.Transitive)
			case got.Transitive != nil && *got.Transitive != *tc.imp.Transitive:
				t.Errorf("transitive = %v, want preserved %v", *got.Transitive, *tc.imp.Transitive)
			}
			if got.Shadow != tc.imp.Shadow {
				t.Errorf("shadow = %q, want preserved %q", got.Shadow, tc.imp.Shadow)
			}
		})
	}
}

// TestMigrateLegacySystemPacksManifestPreservesImportOptions confirms the
// option-preserving rewrite reaches every import surface the migration
// touches — city, default-rig, and rig — so a non-default option on any of
// them survives "gc doctor --fix".
func TestMigrateLegacySystemPacksManifestPreservesImportOptions(t *testing.T) {
	coreSource, ok := builtinpacks.CanonicalImportSource("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	coreVersion := config.BundledSourcePinnedVersion(coreSource)
	transitiveFalse := false

	cityPath := t.TempDir()
	manifest := &config.City{
		Imports: map[string]config.Import{
			"core": {Source: ".gc/system/packs/core", Export: true},
		},
		Defaults: config.PackDefaults{Rig: config.PackRigDefaults{Imports: map[string]config.Import{
			"core": {Source: ".gc/system/packs/core", Transitive: &transitiveFalse},
		}}},
		Rigs: []config.Rig{{
			Name:    "demo",
			Imports: map[string]config.Import{"core": {Source: ".gc/system/packs/core", Shadow: "silent"}},
		}},
	}

	changed, err := migrateLegacySystemPacksManifest(cityPath, manifest)
	if err != nil {
		t.Fatalf("migrateLegacySystemPacksManifest: %v", err)
	}
	if !changed {
		t.Fatal("expected the manifest to report a change")
	}

	city := manifest.Imports["core"]
	if city.Source != coreSource || city.Version != coreVersion || !city.Export {
		t.Errorf("city import = %+v, want source %q version %q export=true", city, coreSource, coreVersion)
	}
	def := manifest.Defaults.Rig.Imports["core"]
	if def.Source != coreSource || def.Version != coreVersion || def.Transitive == nil || *def.Transitive {
		t.Errorf("default-rig import = %+v, want source %q version %q transitive=false", def, coreSource, coreVersion)
	}
	rig := manifest.Rigs[0].Imports["core"]
	if rig.Source != coreSource || rig.Version != coreVersion || rig.Shadow != "silent" {
		t.Errorf("rig import = %+v, want source %q version %q shadow=silent", rig, coreSource, coreVersion)
	}
}

// TestBuiltinImportDoctorCheck_FixSkipsResyncWhenNoOwnedMutation is a
// regression for the attempt-1 review (claude, major): when the only detected
// condition is one this Fix does not own — here a wave-1 public-pack import in
// a user-authored config fragment — steps 1-2 make no mutation, so the step-3
// lockfile/cache resync must be skipped. Resyncing every declared import for
// work this check did not own would turn an advisory warning into a hard
// "gc doctor --fix" failure whenever an unrelated import is momentarily
// unresolvable.
func TestBuiltinImportDoctorCheck_FixSkipsResyncWhenNoOwnedMutation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc init = %d; stderr=%s", code, stderr.String())
	}
	// A freshly initialized city already imports every required builtin, so
	// this check owns no add/migrate mutation.
	if r := newBuiltinImportDoctorCheck(dir).Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("precondition: builtin-import status after init = %v, want OK; details=%v", r.Status, r.Details)
	}

	// Add a wave-1 public-pack fragment import plus its retired-tree pack so
	// Run reports a manual migration without giving this Fix anything to add
	// or rewrite. Root city.toml imports are owned by packv2-import-state.
	cityPath := filepath.Join(dir, "city.toml")
	cityData, err := os.ReadFile(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cityPath, append([]byte("include = [\"legacy-wave1.toml\"]\n"), cityData...), 0o644); err != nil {
		t.Fatal(err)
	}
	fragment := `[imports.legacy-gastown]
source = ".gc/system/packs/gastown"
`
	fragPath := filepath.Join(dir, "legacy-wave1.toml")
	if err := os.WriteFile(fragPath, []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Join(dir, citylayout.SystemPacksRoot, "gastown")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "pack.toml"), []byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	if r := check.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for the wave-1 manual import; details=%v", r.Status, r.Details)
	}

	prevSync := syncImports
	prevInstall := installLockedImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
	})
	syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
		t.Fatal("Fix resynced the lockfile despite owning no mutation (steps 1-2 were no-ops)")
		return nil, nil
	}
	installLockedImports = func(_ string) (*packman.Lockfile, error) {
		t.Fatal("Fix reinstalled locked imports despite owning no mutation (steps 1-2 were no-ops)")
		return nil, nil
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	// The fragment import needs a user edit; this Fix must leave it.
	after, err := os.ReadFile(fragPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != fragment {
		t.Fatalf("Fix rewrote the wave-1 fragment import:\n%s", after)
	}
}
