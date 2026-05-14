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
	"github.com/gastownhall/gascity/internal/migrate"
)

func TestV2DeprecationChecksWarnOnLegacyPatterns(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
default_rig_includes = ["../packs/default rig"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 1

[[agent]]
name = "helper"
scope = "city"
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")
	writeDoctorFile(t, cityDir, "scripts/legacy.sh", "#!/bin/sh\necho legacy\n")

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	for _, name := range []string{
		"v2-agent-format",
		"v2-import-format",
		"v2-default-rig-import-format",
		"v2-rig-path-site-binding",
		"v2-scripts-layout",
		"v2-workspace-name",
		"v2-prompt-template-suffix",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("doctor output missing %s:\n%s", name, out)
		}
	}
	if strings.Contains(out, "gc import migrate") {
		t.Fatalf("doctor output should not point users at gc import migrate anymore:\n%s", out)
	}
	if !strings.Contains(out, "gc doctor --fix") || !strings.Contains(out, "gc doctor") {
		t.Fatalf("doctor output missing doctor migration guidance:\n%s", out)
	}
	if !strings.Contains(out, "[defaults.rig.imports.<binding>]") {
		t.Fatalf("doctor output missing rig defaults guidance:\n%s", out)
	}
	if !strings.Contains(out, ".template.md") {
		t.Fatalf("doctor output missing .template.md guidance:\n%s", out)
	}
}

func TestV2ScriptsLayoutWarnsForSymlinkOnlyDir(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "city"
schema = 2
`)
	srcFile := filepath.Join(cityDir, "assets", "scripts", "helper.sh")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scriptsDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(srcFile, filepath.Join(scriptsDir, "helper.sh")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	res := v2ScriptsLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("symlink-only scripts/ should warn as stale legacy state; got status=%v message=%q details=%v",
			res.Status, res.Message, res.Details)
	}
	if !strings.Contains(res.Message, "stale legacy symlinks") {
		t.Fatalf("symlink-only scripts/ should report stale legacy state, got %q", res.Message)
	}
}

func TestV2ScriptsLayoutWarnsForUserManagedSymlinkOnlyDir(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "city"
schema = 2
`)
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "helper.sh")
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scriptsDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(srcFile, filepath.Join(scriptsDir, "helper.sh")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	res := v2ScriptsLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("user-managed symlink-only scripts/ should still warn; got status=%v message=%q details=%v",
			res.Status, res.Message, res.Details)
	}
	if !strings.Contains(res.Message, "user-managed symlinks") {
		t.Fatalf("user-managed symlink-only scripts/ should report preserved symlink state, got %q", res.Message)
	}
}

func TestV2ScriptsLayoutTreatsTopLevelScriptsTargetsAsUserManaged(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "city"
schema = 2
`)

	scriptsDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.Join(scriptsDir, "generated", "helper.sh"), filepath.Join(scriptsDir, "helper.sh")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	res := v2ScriptsLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("top-level scripts/ symlinks should still warn; got status=%v message=%q details=%v",
			res.Status, res.Message, res.Details)
	}
	if !strings.Contains(res.Message, "user-managed symlinks") {
		t.Fatalf("top-level scripts/ symlink targets should be treated as user-managed, got %q", res.Message)
	}
}

func TestV2ScriptsLayoutTreatsRelayoutIntoAssetsScriptsAsUserManaged(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "city"
schema = 2
`)
	srcFile := filepath.Join(cityDir, "assets", "scripts", "helper.sh")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scriptsDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(srcFile, filepath.Join(scriptsDir, "custom-helper.sh")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	res := v2ScriptsLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("relayout symlink-only scripts/ should still warn; got status=%v message=%q details=%v",
			res.Status, res.Message, res.Details)
	}
	if !strings.Contains(res.Message, "user-managed symlinks") {
		t.Fatalf("relayout symlink-only scripts/ should be treated as user-managed, got %q", res.Message)
	}
}

func TestV2ScriptsLayoutWarnsOnRealFilesAlongsideSymlinks(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "city"
schema = 2
`)
	srcFile := filepath.Join(cityDir, "assets", "scripts", "resolved.sh")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(srcFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	scriptsDir := filepath.Join(cityDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(srcFile, filepath.Join(scriptsDir, "resolved.sh")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "legacy.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(legacy): %v", err)
	}

	res := v2ScriptsLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("mixed scripts/ should warn; got status=%v", res.Status)
	}
	var hasLegacy, hasResolved bool
	for _, d := range res.Details {
		if strings.Contains(d, "legacy.sh") {
			hasLegacy = true
		}
		if strings.Contains(d, "resolved.sh") {
			hasResolved = true
		}
	}
	if !hasLegacy {
		t.Errorf("warning should cite legacy.sh; details=%v", res.Details)
	}
	if hasResolved {
		t.Errorf("warning should not cite symlinked resolved.sh; details=%v", res.Details)
	}
}

func TestV2DeprecationChecksWarnAndFixLegacyRigPath(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	rigPath := filepath.Join(cityDir, "..", "frontend")
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[rigs]]
name = "frontend"
path = "`+rigPath+`"
`)

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "v2-rig-path-site-binding") {
		t.Fatalf("doctor output missing rig-path migration warning:\n%s", out)
	}
	if !strings.Contains(out, ".gc/site.toml") {
		t.Fatalf("doctor output missing site binding guidance:\n%s", out)
	}

	buf.Reset()
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, true)

	rawData, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if strings.Contains(string(rawData), "path = ") {
		t.Fatalf("city.toml should no longer store rig.path:\n%s", rawData)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != rigPath {
		t.Fatalf("binding = %+v, want frontend=%s", binding.Rigs, rigPath)
	}

	buf.Reset()
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)
	out = buf.String()
	if strings.Contains(out, "⚠ v2-rig-path-site-binding") {
		t.Fatalf("rig-path warning should clear after fix:\n%s", out)
	}
}

func TestV2DeprecationChecksWarnOnDeferredPackAgentFormat(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2

[[agent]]
name = "helper"
`)

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "⚠ v2-agent-format") {
		t.Fatalf("doctor output missing deferred pack agent warning:\n%s", out)
	}
	if !strings.Contains(out, "pack.toml") {
		t.Fatalf("doctor output missing pack.toml detail:\n%s", out)
	}
	if !strings.Contains(out, "enforcement is deferred") {
		t.Fatalf("doctor output missing deferral guidance:\n%s", out)
	}
}

func TestV2DeprecationChecksWarnOnStaleSiteBindingName(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[rigs]]
name = "frontend"
`)
	writeDoctorFile(t, cityDir, ".gc/site.toml", `
[[rig]]
name = "old-name"
path = "/tmp/frontend"
`)

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "v2-rig-path-site-binding") {
		t.Fatalf("doctor output missing stale site binding warning:\n%s", out)
	}
	if !strings.Contains(out, "old-name") {
		t.Fatalf("doctor output missing stale rig name detail:\n%s", out)
	}
}

func TestV2DeprecationChecksWarnAndFixLegacyWorkspaceIdentity(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
prefix = "lc"
`)

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "v2-workspace-name") {
		t.Fatalf("doctor output missing workspace identity warning:\n%s", out)
	}
	if !strings.Contains(out, ".gc/site.toml") {
		t.Fatalf("doctor output missing site binding guidance:\n%s", out)
	}

	buf.Reset()
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, true)

	rawData, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	if strings.Contains(string(rawData), `name = "legacy-city"`) || strings.Contains(string(rawData), `prefix = "lc"`) {
		t.Fatalf("city.toml should no longer store workspace identity:\n%s", rawData)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if binding.WorkspaceName != "legacy-city" || binding.WorkspacePrefix != "lc" {
		t.Fatalf("binding = %+v, want workspace_name=legacy-city workspace_prefix=lc", binding)
	}

	buf.Reset()
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)
	out = buf.String()
	if strings.Contains(out, "⚠ v2-workspace-name") {
		t.Fatalf("workspace identity warning should clear after fix:\n%s", out)
	}
}

func TestV2DeprecationChecksWarnOnLegacyTemplateSuffix(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md.tmpl", "Hello {{.Agent}}\n")

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "v2-prompt-template-suffix") {
		t.Fatalf("doctor output missing prompt-template warning:\n%s", out)
	}
	if !strings.Contains(out, "prompts/mayor.md.tmpl") {
		t.Fatalf("doctor output missing legacy prompt path:\n%s", out)
	}
	if !strings.Contains(out, ".template.md") {
		t.Fatalf("doctor output missing canonical suffix guidance:\n%s", out)
	}
}

func TestV2DeprecationChecksStayQuietOnMigratedLayout(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "modern-city"
schema = 1

[imports.gastown]
source = "./assets/imports/gastown"

[defaults.rig.imports.gastown]
source = "./assets/imports/gastown"
`)
	writeDoctorFile(t, cityDir, "agents/mayor/prompt.md", "Hello world\n")

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir}, &buf, false)

	out := buf.String()
	if strings.Contains(out, "⚠") {
		t.Fatalf("expected migrated layout to avoid V2 warnings, got:\n%s", out)
	}
}

func TestV2DeprecationChecksGoQuietAfterMigration(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
default_rig_includes = ["../packs/default rig"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")

	if _, err := migrate.Apply(cityDir, migrate.Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	for _, line := range []string{
		"✓ v2-agent-format",
		"✓ v2-import-format",
		"✓ v2-default-rig-import-format",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("doctor output missing %q after migration:\n%s", line, out)
		}
	}
	if strings.Contains(out, "⚠ v2-agent-format") || strings.Contains(out, "⚠ v2-import-format") || strings.Contains(out, "⚠ v2-default-rig-import-format") {
		t.Fatalf("expected migration-specific warnings to clear, got:\n%s", out)
	}
}

// TestV2DeprecationFixSurfacesMigrateWarnings guards the codex review
// finding on PR #1880: when migrate.Apply emits warnings about
// behavior-affecting fields it had to drop (e.g. legacy [[agent]] entries
// with fallback = true), doctor --fix must surface them. Without this,
// the next gc doctor run sees a green check and the manual follow-up is
// lost forever.
func TestV2DeprecationFixSurfacesMigrateWarnings(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
fallback = true
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")

	var sink bytes.Buffer
	if err := runV2PackMigration(&doctor.CheckContext{CityPath: cityDir}, &sink); err != nil {
		t.Fatalf("runV2PackMigration: %v", err)
	}

	got := sink.String()
	if !strings.Contains(got, "fallback") {
		t.Fatalf("expected migrate warnings about dropped fallback field to be surfaced; got:\n%s", got)
	}
	if !strings.Contains(got, "mayor") {
		t.Fatalf("expected the agent name to appear in the warning; got:\n%s", got)
	}
}

func TestV2DeprecationDoctorFixSurfacesMigrateWarningsInOutput(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
fallback = true
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	d.Register(v2AgentFormatCheck{})
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, true)

	got := buf.String()
	if !strings.Contains(got, "fallback") {
		t.Fatalf("expected doctor --fix output to include migrate warning; got:\n%s", got)
	}
	if !strings.Contains(got, "✓ v2-agent-format") {
		t.Fatalf("expected doctor --fix output to include fixed check result; got:\n%s", got)
	}
}

// TestV2ImportFormatCheckFixMigratesIncludes runs v2ImportFormatCheck.Fix
// in isolation against a city whose only legacy artifact is
// workspace.includes — guards the per-Check Fix entry point that the
// bundled migration test does not exercise (the chained doctor.Run already
// migrates everything via the first Fix call).
func TestV2ImportFormatCheckFixMigratesIncludes(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
`)

	check := v2ImportFormatCheck{}
	if !check.CanFix() {
		t.Fatal("v2ImportFormatCheck should advertise CanFix()=true")
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusWarning {
		t.Fatalf("pre-fix status = %v, want warning", got.Status)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; message=%q", got.Status, got.Message)
	}
}

// TestV2DefaultRigImportFormatCheckFixMigratesDefaults runs
// v2DefaultRigImportFormatCheck.Fix in isolation, mirroring the
// import-only test above for the default-rig-includes path.
func TestV2DefaultRigImportFormatCheckFixMigratesDefaults(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
default_rig_includes = ["../packs/default-rig"]
`)

	check := v2DefaultRigImportFormatCheck{}
	if !check.CanFix() {
		t.Fatal("v2DefaultRigImportFormatCheck should advertise CanFix()=true")
	}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusWarning {
		t.Fatalf("pre-fix status = %v, want warning", got.Status)
	}
	if !strings.Contains(got.FixHint, "gc doctor --fix") {
		t.Fatalf("FixHint = %q, want gc doctor --fix hint", got.FixHint)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; message=%q", got.Status, got.Message)
	}
}

// TestV2DeprecationChecksFixMigratesPackShape exercises the doctor --fix
// path for the v2 pack-shape checks (legacy [[agent]] tables,
// workspace.includes, default_rig_includes). The hint shown in warning
// states points users at "gc doctor --fix"; this test guards against the
// regression where those checks declared CanFix()=false and the hint led
// nowhere.
func TestV2DeprecationChecksFixMigratesPackShape(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]
default_rig_includes = ["../packs/default rig"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 1

[[agent]]
name = "helper"
scope = "city"
`)
	writeDoctorFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	d.Register(v2AgentFormatCheck{})
	d.Register(v2ImportFormatCheck{})
	d.Register(v2DefaultRigImportFormatCheck{})
	report := d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, true)

	if report.Fixed == 0 {
		t.Fatalf("expected at least one v2 pack check to be auto-fixed, got Fixed=0; output:\n%s", buf.String())
	}
	if report.Warned > 0 {
		t.Fatalf("expected v2 pack warnings to clear after --fix, got Warned=%d; output:\n%s", report.Warned, buf.String())
	}

	// Re-run without fix and confirm the city is now clean.
	var verify bytes.Buffer
	verifyDoctor := &doctor.Doctor{}
	verifyDoctor.Register(v2AgentFormatCheck{})
	verifyDoctor.Register(v2ImportFormatCheck{})
	verifyDoctor.Register(v2DefaultRigImportFormatCheck{})
	verifyDoctor.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &verify, false)
	out := verify.String()
	for _, line := range []string{
		"✓ v2-agent-format",
		"✓ v2-import-format",
		"✓ v2-default-rig-import-format",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("post-fix doctor output missing %q:\n%s", line, out)
		}
	}
}

func writeDoctorFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(contents, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
