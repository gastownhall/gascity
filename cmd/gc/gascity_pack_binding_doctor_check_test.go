package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// v14GascityPackToml is the pack.toml shape gc v1.4.x `gc init` (default /
// --template gascity) wrote: the public Gas City pack bound as "gascity".
func v14GascityPackToml() string {
	return fmt.Sprintf(`[pack]
name = "bright-lights"
schema = 2

[imports.core]
source = "https://github.com/gastownhall/gascity/tree/main/internal/bootstrap/packs/core"
version = %q

[imports.gascity]
source = %q
version = %q

[[named_session]]
template = "mayor"
mode = "always"
`, config.BundledPackImportVersion, config.PublicGascityPackSource, config.PublicGascityPackVersion)
}

// v14GascityCityToml carries the gc-roles default rig import (already bound
// "gc" in v1.4.x) and a rig that imports the public pack under its own key:
// neither may be touched by the check.
func v14GascityCityToml() string {
	return fmt.Sprintf(`[workspace]
name = "bright-lights"

[[rigs]]
name = "proj"

[rigs.imports.gascity]
source = %q
version = %q

[defaults.rig.imports.gc]
source = %q
version = %q
`, config.PublicGascityPackSource, config.PublicGascityPackVersion,
		config.PublicGascityRolesPackSource, config.PublicGascityPackVersion)
}

func setupGascityBindingCity(t *testing.T, packToml, cityToml string) string {
	t.Helper()
	clearGCEnv(t)
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	cityDir := filepath.Join(root, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	site := "workspace_name = \"bright-lights\"\n\n[[rig]]\nname = \"proj\"\npath = \"proj\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, cityToml)
	writePackToml(t, cityDir, packToml)
	return cityDir
}

func runGascityBindingCheck(t *testing.T, cityDir string) *doctor.CheckResult {
	t.Helper()
	return newGascityPackBindingDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
}

func readCityFile(t *testing.T, cityDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

type gascityBindingLoadSummary struct {
	agents   []string
	formulas []string
	skills   []string
}

// loadGascityBindingSummary loads the city the way gc commands do and
// records its agents, city formula layers, and the binding-qualified skill
// names contributed by the public Gas City pack.
func loadGascityBindingSummary(t *testing.T, cityDir string) gascityBindingLoadSummary {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	var s gascityBindingLoadSummary
	for _, a := range cfg.Agents {
		s.agents = append(s.agents, a.QualifiedName())
	}
	for _, dir := range cfg.FormulaLayers.City {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			s.formulas = append(s.formulas, e.Name())
		}
	}
	for _, cat := range cfg.PackSkills {
		if cat.PackName != "gascity" {
			continue
		}
		entries, err := os.ReadDir(cat.SourceDir)
		if err != nil {
			t.Fatalf("reading skills dir %s: %v", cat.SourceDir, err)
		}
		for _, e := range entries {
			s.skills = append(s.skills, cat.BindingName+"."+e.Name())
		}
	}
	sort.Strings(s.agents)
	sort.Strings(s.formulas)
	sort.Strings(s.skills)
	return s
}

func TestGascityPackBindingV14CityWarnsFixesAndStaysLoadable(t *testing.T) {
	cityDir := setupGascityBindingCity(t, v14GascityPackToml(), v14GascityCityToml())
	before := loadGascityBindingSummary(t, cityDir)
	if !containsString(before.skills, "gascity.mayor") {
		t.Fatalf("v1.4 city skills = %v, want gascity.mayor", before.skills)
	}
	lockPath := filepath.Join(cityDir, "packs.lock")
	lockBefore := fmt.Sprintf(`schema = 1

[packs.%q]
version = %q
commit = %q
fetched = "2026-09-26T21:59:25Z"
`, config.PublicGascityPackSource, config.PublicGascityPackVersion, strings.TrimPrefix(config.PublicGascityPackVersion, "sha:"))
	if err := os.WriteFile(lockPath, []byte(lockBefore), 0o644); err != nil {
		t.Fatal(err)
	}

	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning; result=%#v", result.Status, result)
	}
	for _, want := range []string{"[imports.gascity]", "[imports.gc]", "gc.mayor", "gc gc claim", "0.1.6"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message %q missing %q", result.Message, want)
		}
	}
	if !strings.Contains(result.FixHint, `gc doctor --fix`) || !strings.Contains(result.FixHint, "[imports.gc]") {
		t.Errorf("fix hint = %q, want doctor --fix rename hint", result.FixHint)
	}

	cityBefore := readCityFile(t, cityDir, "city.toml")
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := readCityFile(t, cityDir, "city.toml"); got != cityBefore {
		t.Fatalf("Fix rewrote city.toml (rig and default-rig imports must be untouched):\n%s", got)
	}
	packCfg, err := config.Parse([]byte(readCityFile(t, cityDir, "pack.toml")))
	if err != nil {
		t.Fatalf("parsing fixed pack.toml: %v", err)
	}
	if _, ok := packCfg.Imports["gascity"]; ok {
		t.Fatalf("fixed pack.toml still imports gascity: %#v", packCfg.Imports)
	}
	gc := packCfg.Imports["gc"]
	if gc.Source != config.PublicGascityPackSource || gc.Version != config.PublicGascityPackVersion {
		t.Fatalf("fixed gc import = %#v, want public gascity source and pin preserved", gc)
	}
	if core := packCfg.Imports["core"]; core.Version != config.BundledPackImportVersion {
		t.Fatalf("core import changed: %#v", core)
	}
	if len(packCfg.NamedSessions) != 1 || packCfg.NamedSessions[0].Template != "mayor" {
		t.Fatalf("named sessions not preserved: %#v", packCfg.NamedSessions)
	}
	if got := readCityFile(t, cityDir, "packs.lock"); got != lockBefore {
		t.Fatalf("packs.lock changed across the rename (it is keyed by source):\n%s", got)
	}

	if second := runGascityBindingCheck(t, cityDir); second.Status != doctor.StatusOK {
		t.Fatalf("second run status = %v, want OK; result=%#v", second.Status, second)
	}
	packAfterFix := readCityFile(t, cityDir, "pack.toml")
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("second Fix: %v", err)
	}
	if got := readCityFile(t, cityDir, "pack.toml"); got != packAfterFix {
		t.Fatalf("second Fix was not idempotent:\n%s", got)
	}

	after := loadGascityBindingSummary(t, cityDir)
	if strings.Join(after.agents, ",") != strings.Join(before.agents, ",") {
		t.Errorf("agents changed across rename:\nbefore=%v\nafter=%v", before.agents, after.agents)
	}
	if len(after.formulas) == 0 || strings.Join(after.formulas, ",") != strings.Join(before.formulas, ",") {
		t.Errorf("formulas changed across rename:\nbefore=%v\nafter=%v", before.formulas, after.formulas)
	}
	if !containsString(after.skills, "gc.mayor") || containsString(after.skills, "gascity.mayor") {
		t.Errorf("skills after rename = %v, want gc.mayor and no gascity.mayor", after.skills)
	}
}

func TestGascityPackBindingThroughDoctorFixLoop(t *testing.T) {
	cityDir := setupGascityBindingCity(t, v14GascityPackToml(), "[workspace]\nname = \"bright-lights\"\n")
	d := &doctor.Doctor{}
	d.Register(newGascityPackBindingDoctorCheck(cityDir))
	var out strings.Builder
	report := d.Run(&doctor.CheckContext{CityPath: cityDir}, &out, true)
	if report.Fixed != 1 || report.Warned != 0 {
		t.Fatalf("report = %+v, want one fixed check; output:\n%s", report, out.String())
	}
	if !strings.Contains(readCityFile(t, cityDir, "pack.toml"), "[imports.gc]") {
		t.Fatalf("pack.toml not renamed:\n%s", readCityFile(t, cityDir, "pack.toml"))
	}
}

func TestGascityPackBindingRefusesConflictingGCKey(t *testing.T) {
	pack := v14GascityPackToml() + `
[imports.gc]
source = "https://github.com/example/other-pack.git"
version = "^1"
`
	cityDir := setupGascityBindingCity(t, pack, "[workspace]\nname = \"bright-lights\"\n")

	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning", result.Status)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "already bound to a different source") {
		t.Fatalf("details = %v, want conflict explanation", result.Details)
	}
	err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir})
	if err == nil || !strings.Contains(err.Error(), "already bound to a different source") || !strings.Contains(err.Error(), "other-pack") {
		t.Fatalf("Fix error = %v, want refusal naming the conflicting source", err)
	}
	if got := readCityFile(t, cityDir, "pack.toml"); got != pack {
		t.Fatalf("refused Fix changed pack.toml:\n%s", got)
	}
}

func TestGascityPackBindingRemovesIdenticalDuplicate(t *testing.T) {
	pack := v14GascityPackToml() + fmt.Sprintf(`
[imports.gc]
source = %q
version = %q
`, config.PublicGascityPackSource, config.PublicGascityPackVersion)
	cityDir := setupGascityBindingCity(t, pack, "[workspace]\nname = \"bright-lights\"\n")

	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(result.FixHint, "duplicate gascity") {
		t.Fatalf("result = %#v, want duplicate-removal warning", result)
	}
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	packCfg, err := config.Parse([]byte(readCityFile(t, cityDir, "pack.toml")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := packCfg.Imports["gascity"]; ok {
		t.Fatalf("duplicate gascity import not removed: %#v", packCfg.Imports)
	}
	if gc := packCfg.Imports["gc"]; gc.Source != config.PublicGascityPackSource || gc.Version != config.PublicGascityPackVersion {
		t.Fatalf("gc import = %#v, want unchanged", gc)
	}
	if second := runGascityBindingCheck(t, cityDir); second.Status != doctor.StatusOK {
		t.Fatalf("second run = %#v, want OK", second)
	}
}

func TestGascityPackBindingReportsDifferingDuplicate(t *testing.T) {
	pack := v14GascityPackToml() + fmt.Sprintf(`
[imports.gc]
source = %q
version = "^0.1"
`, config.PublicGascityPackSource)
	cityDir := setupGascityBindingCity(t, pack, "[workspace]\nname = \"bright-lights\"\n")

	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), "second time with different settings") {
		t.Fatalf("result = %#v, want differing-duplicate report", result)
	}
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err == nil {
		t.Fatal("Fix succeeded, want refusal for a duplicate with different settings")
	}
	if got := readCityFile(t, cityDir, "pack.toml"); got != pack {
		t.Fatalf("refused Fix changed pack.toml:\n%s", got)
	}
}

func TestGascityPackBindingIgnoresOtherPacksAndScopes(t *testing.T) {
	pack := `[pack]
name = "bright-lights"
schema = 2

[imports.gascity]
source = "https://github.com/example/gascity.git"

[imports.gt]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"

[imports.roles]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity/roles"
`
	cityDir := setupGascityBindingCity(t, pack, v14GascityCityToml())
	if result := runGascityBindingCheck(t, cityDir); result.Status != doctor.StatusOK {
		t.Fatalf("result = %#v, want OK: third-party/other public packs and rig imports are out of scope", result)
	}
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := readCityFile(t, cityDir, "pack.toml"); got != pack {
		t.Fatalf("Fix changed pack.toml:\n%s", got)
	}
}

func TestGascityPackBindingRenamesCityTomlOverride(t *testing.T) {
	cityToml := fmt.Sprintf(`[workspace]
name = "bright-lights"

[imports.planning]
source = "https://github.com/gastownhall/gascity-packs.git//gascity"
version = %q
`, config.PublicGascityPackVersion)
	pack := "[pack]\nname = \"bright-lights\"\nschema = 2\n"
	cityDir := setupGascityBindingCity(t, pack, cityToml)

	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(result.Message, "[imports.planning]") {
		t.Fatalf("result = %#v, want warning for the //subpath spelling under planning", result)
	}
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	cfg, err := config.Parse([]byte(readCityFile(t, cityDir, "city.toml")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Imports["planning"]; ok {
		t.Fatalf("city.toml still imports planning: %#v", cfg.Imports)
	}
	if gc := cfg.Imports["gc"]; gc.Source != "https://github.com/gastownhall/gascity-packs.git//gascity" {
		t.Fatalf("gc import = %#v, want source spelling preserved", gc)
	}
}

func TestGascityPackBindingRefusesWhenBindingQualifiedNamesAreReferenced(t *testing.T) {
	pack := v14GascityPackToml() + `
[[patches.agent]]
name = "gascity.mayor"
suspended = true
`
	cityDir := setupGascityBindingCity(t, pack, "[workspace]\nname = \"bright-lights\"\n")
	result := runGascityBindingCheck(t, cityDir)
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), `pack.toml patches.agent[0].name = "gascity.mayor"`) {
		t.Fatalf("result = %#v, want reference report", result)
	}
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err == nil {
		t.Fatal("Fix succeeded, want refusal while gascity.* names are referenced")
	}
	if got := readCityFile(t, cityDir, "pack.toml"); got != pack {
		t.Fatalf("refused Fix changed pack.toml:\n%s", got)
	}
}

func TestGascityPackBindingReferenceScanIgnoresCommentsAndNonNames(t *testing.T) {
	pack := strings.Replace(v14GascityPackToml(), "[[named_session]]", `# old skill name was "gascity.mayor"
[[named_session]]`, 1)
	cityToml := `[workspace]
name = "bright-lights"
# patches used to target "gascity.planner"

[beads]
host = "gascity.example.com"
`
	cityDir := setupGascityBindingCity(t, pack, cityToml)
	if err := newGascityPackBindingDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v (comments and hostnames must not block the rename)", err)
	}
	if !strings.Contains(readCityFile(t, cityDir, "pack.toml"), "[imports.gc]") {
		t.Fatalf("pack.toml not renamed:\n%s", readCityFile(t, cityDir, "pack.toml"))
	}
}

func TestIsPublicGascityPackSource(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{config.PublicGascityPackSource, true},
		{"https://github.com/gastownhall/gascity-packs/tree/main/gascity/", true},
		{"https://github.com/gastownhall/gascity-packs.git//gascity", true},
		{"github.com/gastownhall/gascity-packs//gascity", true},
		{"git@github.com:gastownhall/gascity-packs.git//gascity", true},
		{"ssh://git@github.com/gastownhall/gascity-packs.git//gascity", true},
		{"http://github.com/gastownhall/gascity-packs/tree/main/gascity", true},
		{"https://GitHub.com/GastownHall/Gascity-Packs/tree/main/gascity", true},
		{"git@github.com:GastownHall/gascity-packs.git//gascity#v0.1.6", true},
		{"git@github.com:someone/gascity-packs.git//gascity", false},
		{"https://gitlab.com/gastownhall/gascity-packs/-/tree/main/gascity", false},
		{"git@gitlab.com:gastownhall/gascity-packs.git//gascity", false},
		{"https://github.com/gastownhall/gascity.git//gascity", false},
		{"git@github.com:gastownhall/gascity-packs.git//gascity/roles", false},
		{config.PublicGascityRolesPackSource, false},
		{config.PublicGastownPackSource, false},
		{"https://github.com/example/gascity.git", false},
		{"./packs/gascity", false},
	} {
		if got := isPublicGascityPackSource(tc.source); got != tc.want {
			t.Errorf("isPublicGascityPackSource(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}
