package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestImportStateDoctorCheckReportsOK(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, imports map[string]config.Import) (*packman.CheckReport, error) {
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("imports = %#v, want pack:tools", imports)
		}
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	result := newImportStateDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "1 remote import(s) installed") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestImportStateDoctorCheckReportsInstallHint(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       "missing-cache",
				ImportName: "pack:tools",
				Source:     "https://example.com/tools.git",
				Commit:     "abc123",
				Path:       filepath.Join(cityDir, ".gc", "cache", "repos", "abc"),
				Message:    "locked import is missing from the local repo cache",
				RepairHint: `run "gc import install"`,
			}},
		}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	result := check.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !check.CanFix() || !strings.Contains(result.FixHint, `gc doctor --fix`) || !strings.Contains(result.FixHint, `gc import install`) {
		t.Fatalf("result = %#v, want fixable doctor/import-install hint", result)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "missing-cache") {
		t.Fatalf("details = %#v", result.Details)
	}
}

func TestImportStateDoctorCheckFixRunsImportInstallPath(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevSync := syncImports
	prevInstall := installLockedImports
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
		checkInstalledImports = prevCheck
	})
	synced := false
	installed := false
	lock := &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.1.0", Commit: "new"},
		},
	}
	syncImports = func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("sync cityRoot = %q, want %q", cityRoot, cityDir)
		}
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("sync imports = %#v, want pack:tools", imports)
		}
		if mode != packman.InstallResolveIfNeeded {
			t.Fatalf("sync mode = %v, want InstallResolveIfNeeded", mode)
		}
		synced = true
		return lock, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("install cityRoot = %q, want %q", cityRoot, cityDir)
		}
		installed = true
		return lock, nil
	}
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		if !installed {
			return &packman.CheckReport{Issues: []packman.CheckIssue{{
				Severity: packman.CheckSeverityError,
				Code:     "missing-lockfile",
			}}}, nil
		}
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	before := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if before.Status != doctor.StatusError {
		t.Fatalf("before status = %v, want error", before.Status)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !synced || !installed {
		t.Fatalf("sync/install called = %v/%v, want both", synced, installed)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusOK {
		t.Fatalf("after status = %v, want OK; result=%#v", after.Status, after)
	}
}

func TestImportStateDoctorCheckReportsDurableRegistrySelectors(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.lighthouse]
source = "registry:main:lighthouse"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		t.Fatal("checkInstalledImports should not run when durable registry selectors are present")
		return nil, nil
	}

	result := newImportStateDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "command-time registry selectors") {
		t.Fatalf("message = %q", result.Message)
	}
	if !strings.Contains(result.FixHint, "concrete pack sources") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "registry-selector-source") || !strings.Contains(result.Details[0], "registry:main:lighthouse") {
		t.Fatalf("details = %#v", result.Details)
	}
	if err := newImportStateDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err == nil {
		t.Fatal("Fix succeeded for durable registry selector, want manual error")
	}
}

func TestImportStateDoctorCheckReportsLegacyPublicPackImports(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, `[workspace]
name = "demo"

[defaults.rig.imports.maintenance]
source = "examples/gastown/packs/maintenance"
`)
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.gastown]
source = ".gc/system/packs/gastown"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		t.Fatal("checkInstalledImports should not run when legacy public pack imports are present")
		return nil, nil
	}

	result := newImportStateDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "legacy public built-in pack import") {
		t.Fatalf("message = %q", result.Message)
	}
	if !strings.Contains(result.FixHint, `gc doctor --fix`) || !strings.Contains(result.FixHint, "legacy maintenance imports") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
	if len(result.Details) != 2 {
		t.Fatalf("details = %#v, want two legacy public pack details", result.Details)
	}
	for _, want := range []string{"pack:gastown", "default-rig:maintenance"} {
		found := false
		for _, detail := range result.Details {
			found = found || strings.Contains(detail, want)
		}
		if !found {
			t.Fatalf("details = %#v, missing %s", result.Details, want)
		}
	}
}

func TestLegacyPublicPackForSourceDetectsAbsolutePaths(t *testing.T) {
	cityDir := filepath.Join(string(filepath.Separator), "city")
	cases := []struct {
		name   string
		source string
		pack   string
	}{
		{
			name:   "absolute materialized gastown",
			source: filepath.Join(string(filepath.Separator), "other", ".gc", "system", "packs", "gastown"),
			pack:   "gastown",
		},
		{
			name:   "absolute example maintenance",
			source: filepath.Join(string(filepath.Separator), "repo", "examples", "gastown", "packs", "maintenance"),
			pack:   "maintenance",
		},
		{
			name:   "absolute unrelated pack",
			source: filepath.Join(string(filepath.Separator), "repo", "packs", "custom"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := legacyPublicPackForSource(cityDir, tc.source)
			if tc.pack == "" {
				if ok {
					t.Fatalf("legacyPublicPackForSource(%q) = %q, true; want false", tc.source, got)
				}
				return
			}
			if !ok || got != tc.pack {
				t.Fatalf("legacyPublicPackForSource(%q) = %q, %v; want %q, true", tc.source, got, ok, tc.pack)
			}
		})
	}
}

func TestLegacyPublicPackForSourceIgnoresRemoteSubdirectorySources(t *testing.T) {
	cityDir := filepath.Join(string(filepath.Separator), "city")
	cases := []string{
		"https://example.com/repo.git//examples/gastown/packs/gastown",
		"ssh://example.com/repo.git//.gc/system/packs/gastown",
		"git@example.com:org/repo.git//examples/gastown/packs/maintenance",
		"github.com/org/repo//examples/gastown/packs/maintenance",
		"file:///repo/examples/gastown/packs/gastown",
	}
	for _, source := range cases {
		t.Run(source, func(t *testing.T) {
			if got, ok := legacyPublicPackForSource(cityDir, source); ok {
				t.Fatalf("legacyPublicPackForSource(%q) = %q, true; want false", source, got)
			}
		})
	}
}

func TestImportStateDoctorCheckFixRewritesLegacyPublicPackImports(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, `[workspace]
name = "demo"

[[rigs]]
name = "main"
path = "rigs/main"

[rigs.imports.gastown]
source = "examples/gastown/packs/gastown"

[defaults.rig.imports.maintenance]
source = ".gc/system/packs/maintenance"
`)
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.gastown]
source = ".gc/system/packs/gastown"

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevResolve := resolveWave1PublicPackImports
	prevSync := syncImports
	prevInstall := installLockedImports
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		resolveWave1PublicPackImports = prevResolve
		syncImports = prevSync
		installLockedImports = prevInstall
		checkInstalledImports = prevCheck
	})

	targets := map[string]wave1PublicPackImportTarget{
		"gastown": {
			Binding: "gastown",
			Import:  config.Import{Source: "https://packages.example/gastown.git", Version: "^1.2"},
		},
		"maintenance": {
			Binding: "maintenance",
			Remove:  true,
		},
	}
	resolveWave1PublicPackImports = func(packNames []string) (map[string]wave1PublicPackImportTarget, error) {
		if got, want := strings.Join(packNames, ","), "gastown,maintenance"; got != want {
			t.Fatalf("resolve pack names = %q, want %q", got, want)
		}
		return targets, nil
	}
	lock := &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			targets["gastown"].Import.Source: {Version: "1.2.3", Commit: "abc"},
		},
	}
	syncImports = func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("sync cityRoot = %q, want %q", cityRoot, cityDir)
		}
		if mode != packman.InstallResolveIfNeeded {
			t.Fatalf("sync mode = %v, want InstallResolveIfNeeded", mode)
		}
		for key, target := range map[string]wave1PublicPackImportTarget{
			"pack:gastown":     targets["gastown"],
			"rig:main:gastown": targets["gastown"],
		} {
			if got := imports[key]; got.Source != target.Import.Source || got.Version != target.Import.Version {
				t.Fatalf("imports[%s] = %+v, want %s target", key, got, target.Binding)
			}
		}
		if _, ok := imports["default-rig:maintenance"]; ok {
			t.Fatalf("imports still contains maintenance, want implicit maintenance only: %#v", imports)
		}
		for key, imp := range imports {
			if strings.HasPrefix(imp.Source, ".gc/system/packs/") || strings.HasPrefix(imp.Source, "examples/gastown/packs/") {
				t.Fatalf("imports still contains legacy source at %s: %#v", key, imports)
			}
		}
		return lock, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("install cityRoot = %q, want %q", cityRoot, cityDir)
		}
		return lock, nil
	}
	checkInstalledImports = func(_ string, imports map[string]config.Import) (*packman.CheckReport, error) {
		for key, imp := range imports {
			if strings.HasPrefix(imp.Source, ".gc/system/packs/") || strings.HasPrefix(imp.Source, "examples/gastown/packs/") {
				return &packman.CheckReport{Issues: []packman.CheckIssue{{Code: "legacy-leftover", ImportName: key, Source: imp.Source}}}, nil
			}
		}
		return &packman.CheckReport{CheckedSources: 2}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusOK {
		t.Fatalf("after status = %v, want OK; result=%#v", after.Status, after)
	}

	packData, err := os.ReadFile(filepath.Join(cityDir, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	packText := string(packData)
	if !strings.Contains(packText, "[imports.gastown]") {
		t.Fatalf("pack.toml missing migrated gastown import:\n%s", packText)
	}
	if strings.Contains(packText, "maintenance") {
		t.Fatalf("pack.toml should remove legacy maintenance import because maintenance/core is implicit:\n%s", packText)
	}
	if strings.Contains(packText, ".gc/system/packs") || strings.Contains(packText, "examples/gastown/packs") {
		t.Fatalf("pack.toml still contains legacy public pack references:\n%s", packText)
	}
	cityData, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cityText := string(cityData)
	if !strings.Contains(cityText, "[[rigs]]") || !strings.Contains(cityText, "[rigs.imports.gastown]") {
		t.Fatalf("city.toml missing rig gastown import:\n%s", cityText)
	}
	if strings.Contains(cityText, ".gc/system/packs") || strings.Contains(cityText, "examples/gastown/packs") {
		t.Fatalf("city.toml still contains legacy public pack references:\n%s", cityText)
	}
	if strings.Contains(cityText, "maintenance") {
		t.Fatalf("city.toml should remove legacy maintenance default-rig import because maintenance/core is implicit:\n%s", cityText)
	}
}

func TestImportStateDoctorCheckFixRefusesToOverwriteExistingTargetImport(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.gastown]
source = "https://example.com/custom-gastown.git"
version = "^9.0"

[imports.legacy_gastown]
source = ".gc/system/packs/gastown"
`)

	prevResolve := resolveWave1PublicPackImports
	t.Cleanup(func() { resolveWave1PublicPackImports = prevResolve })
	resolveWave1PublicPackImports = func(_ []string) (map[string]wave1PublicPackImportTarget, error) {
		return map[string]wave1PublicPackImportTarget{
			"gastown": {
				Binding: "gastown",
				Import:  config.Import{Source: "https://packages.example/gastown.git", Version: "^1.2"},
			},
		}, nil
	}

	err := newImportStateDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir})
	if err == nil {
		t.Fatal("Fix succeeded despite conflicting existing target import")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite existing") {
		t.Fatalf("Fix error = %v, want overwrite refusal", err)
	}
}

func TestDoDoctorRegistersImportStateCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCityFlag := cityFlag
	prevCheck := checkInstalledImports
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		checkInstalledImports = prevCheck
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       "missing-lockfile",
				RepairHint: `run "gc import install"`,
			}},
		}, nil
	}
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, `gc import install`) {
		t.Fatalf("doctor output missing import state check:\n%s", out)
	}
}

func TestDoDoctorRunsImportStateCheckWhenImportInstallStateBroken(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, "missing-lockfile") {
		t.Fatalf("doctor output missing import-state failure for broken install state:\n%s", out)
	}
	if !strings.Contains(out, `gc import install`) {
		t.Fatalf("doctor output missing install hint:\n%s", out)
	}
}

func TestDoDoctorSkipsImportStateCheckWhenCityConfigInvalid(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevCityFlag := cityFlag
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		checkInstalledImports = prevCheck
	})
	cityFlag = cityDir
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		t.Fatal("import state check should not run when city.toml cannot load")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if strings.Contains(out, "packv2-import-state") {
		t.Fatalf("doctor output included import state check for invalid config:\n%s", out)
	}
	if !strings.Contains(out, "city-config") {
		t.Fatalf("doctor output missing city config failure:\n%s", out)
	}
}

// Regression for the ga-lurp5d follow-up review: the packv2 import-state
// rewrite re-marshals pack.toml; when pack.toml is a symlink (e.g., into a
// checked-out repo) the rewrite must write through the link instead of
// replacing it with a regular file and stranding the stale manifest in the
// checked-in target.
func TestRewriteLegacyPublicPackImportsWritesThroughPackTomlSymlink(t *testing.T) {
	t.Parallel()
	cityDir := t.TempDir()
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkoutDir, "pack.toml")
	src := `[pack]
name = "demo"
schema = 1

[imports.gastown]
source = ".gc/system/packs/gastown"
`
	if err := os.WriteFile(target, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cityDir, "pack.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	changed, err := rewriteLegacyPublicPackImportsFS(fsys.OSFS{}, cityDir, map[string]wave1PublicPackImportTarget{
		"gastown": {
			Binding: "gastown",
			Import:  config.Import{Source: "https://packages.example/gastown.git", Version: "^1.2"},
		},
	})
	if err != nil {
		t.Fatalf("rewriteLegacyPublicPackImportsFS: %v", err)
	}
	if !changed {
		t.Fatal("rewriteLegacyPublicPackImportsFS reported no change, want pack.toml rewrite")
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pack.toml symlink was replaced by a %v entry; rewrite must write through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "https://packages.example/gastown.git") {
		t.Fatalf("symlink target missing migrated import source:\n%s", text)
	}
	if strings.Contains(text, ".gc/system/packs/gastown") {
		t.Fatalf("symlink target still contains legacy import source:\n%s", text)
	}
}

// Regression for the ga-lurp5d follow-up review: the packv2 import-state
// rewrite re-marshals pack.toml through the reduced cityPackManifest struct,
// which would silently drop keys this gc binary does not recognize. A pack.toml
// carrying an unknown key must make the rewrite refuse rather than strand a
// reduced manifest at the checked-in target.
func TestRewriteLegacyPublicPackImportsRefusesPackTomlUnknownKeys(t *testing.T) {
	t.Parallel()
	cityDir := t.TempDir()
	checkoutDir := filepath.Join(cityDir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkoutDir, "pack.toml")
	src := `[pack]
name = "demo"
schema = 1

[imports.gastown]
source = ".gc/system/packs/gastown"

[future_unknown_section]
knob = "keep-me"
`
	if err := os.WriteFile(target, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cityDir, "pack.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := rewriteLegacyPublicPackImportsFS(fsys.OSFS{}, cityDir, map[string]wave1PublicPackImportTarget{
		"gastown": {
			Binding: "gastown",
			Import:  config.Import{Source: "https://packages.example/gastown.git", Version: "^1.2"},
		},
	})
	if err == nil {
		t.Fatal("rewriteLegacyPublicPackImportsFS succeeded, want refusal for unknown key")
	}
	if !strings.Contains(err.Error(), "future_unknown_section") {
		t.Fatalf("error = %v, want mention of future_unknown_section", err)
	}
	// The symlink and its content must survive an aborted rewrite.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pack.toml symlink was replaced by a %v entry", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != src {
		t.Fatalf("pack.toml was rewritten despite refusal:\n%s", data)
	}
}
