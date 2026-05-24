package main

import (
	"bytes"
	"fmt"
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
		"v2-pack-sources",
		"v2-rig-path-site-binding",
		"v2-legacy-order-layout",
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
	for _, want := range []string{
		"city.toml:3: workspace.includes includes",
		"city.toml:4: workspace.default_rig_includes includes",
		"city.toml:6: city.toml [[agent]]",
		"pack.toml:5: pack.toml [[agent]]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing source coordinate %q:\n%s", want, out)
		}
	}
}

func TestDoctorFixClearsExpandedConfigLoadAfterV2Migration(t *testing.T) {
	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "prompts/worker.md", "You are the worker.\n")
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "file")
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	var stdout, stderr bytes.Buffer
	code := doDoctor(true, false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc doctor --fix = %d, want 0; stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "✗ expanded-config-load") {
		t.Fatalf("expanded-config-load reported stale pre-fix failure:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "✓ expanded-config-load") {
		t.Fatalf("doctor output missing post-fix expanded-config-load success:\n%s", stdout.String())
	}
}

func TestV2LegacyOrderLayoutCheckReportsSchema1NestedOrder(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 1
`)
	writeDoctorFile(t, cityDir, "orders/heartbeat/order.toml", `
[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)

	got := v2LegacyOrderLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; result=%+v", got.Status, got)
	}
	for _, want := range []string{
		"unsupported PackV1 order subdirectory layouts",
		"gc doctor --fix",
		"orders/heartbeat/order.toml",
		"orders/heartbeat.toml",
	} {
		if !strings.Contains(got.Message+strings.Join(got.Details, "\n")+got.FixHint, want) {
			t.Fatalf("result %+v missing %q", got, want)
		}
	}
	if !(v2LegacyOrderLayoutCheck{}).CanFix() {
		t.Fatal("legacy order layout check should support automatic collision-free fixes")
	}
}

func TestV2LegacyOrderLayoutFixMigratesSchema1NestedOrder(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 1
`)
	writeDoctorFile(t, cityDir, "orders/heartbeat/order.toml", `
[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeDoctorFile(t, cityDir, "formulas/orders/formula-heartbeat/order.toml", `
[order]
formula = "formula-heartbeat"
trigger = "manual"
`)

	check := v2LegacyOrderLayoutCheck{}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	for _, rel := range []string{
		"orders/heartbeat.toml",
		"orders/formula-heartbeat.toml",
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("migrated %s stat: %v", rel, err)
		}
	}
	for _, rel := range []string{
		"orders/heartbeat/order.toml",
		"formulas/orders/formula-heartbeat/order.toml",
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); !os.IsNotExist(err) {
			t.Fatalf("legacy %s stat = %v, want removed", rel, err)
		}
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; result=%+v", got.Status, got)
	}
}

func TestV2LegacyOrderLayoutFixMigratesConfiguredFormulaDir(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "custom-formulas-city"

[formulas]
dir = "flows"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "custom-formulas-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "flows/orders/heartbeat/order.toml", `
[order]
formula = "heartbeat"
trigger = "manual"
`)

	check := v2LegacyOrderLayoutCheck{}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; result=%+v", got.Status, got)
	}
	resultText := got.Message + got.FixHint + strings.Join(got.Details, "\n")
	for _, want := range []string{
		"flows/orders/heartbeat/order.toml",
		"orders/heartbeat.toml",
	} {
		if !strings.Contains(resultText, want) {
			t.Fatalf("result %+v missing %q", got, want)
		}
	}

	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "orders/heartbeat.toml")); err != nil {
		t.Fatalf("migrated orders/heartbeat.toml stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "flows/orders/heartbeat/order.toml")); !os.IsNotExist(err) {
		t.Fatalf("legacy flows/orders/heartbeat/order.toml stat = %v, want removed", err)
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; result=%+v", got.Status, got)
	}
}

func TestV2LegacyOrderLayoutFixMigratesRootPackReferencedOrders(t *testing.T) {
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
includes = ["packs/root-include"]

[imports.root-import]
source = "packs/root-import"

[defaults.rig.imports.root-default]
source = "packs/root-default"
`)
	writeDoctorFile(t, cityDir, "packs/root-include/pack.toml", `
[pack]
name = "root-include"
schema = 2
`)
	writeDoctorFile(t, cityDir, "packs/root-import/pack.toml", `
[pack]
name = "root-import"
schema = 2
includes = ["../nested-import"]
`)
	writeDoctorFile(t, cityDir, "packs/root-default/pack.toml", `
[pack]
name = "root-default"
schema = 2
`)
	writeDoctorFile(t, cityDir, "packs/nested-import/pack.toml", `
[pack]
name = "nested-import"
schema = 2
`)
	for _, rel := range []string{
		"packs/root-include/orders/include-order/order.toml",
		"packs/root-import/orders/import-order/order.toml",
		"packs/root-default/orders/default-order/order.toml",
		"packs/nested-import/orders/nested-order/order.toml",
	} {
		writeDoctorFile(t, cityDir, rel, `
[order]
trigger = "manual"
`)
	}

	check := v2LegacyOrderLayoutCheck{}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; result=%+v", got.Status, got)
	}
	for _, want := range []string{
		"packs/root-include/orders/include-order/order.toml",
		"packs/root-import/orders/import-order/order.toml",
		"packs/root-default/orders/default-order/order.toml",
		"packs/nested-import/orders/nested-order/order.toml",
	} {
		if !strings.Contains(strings.Join(got.Details, "\n"), want) {
			t.Fatalf("details missing %q: %+v", want, got.Details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, rel := range []string{
		"packs/root-include/orders/include-order.toml",
		"packs/root-import/orders/import-order.toml",
		"packs/root-default/orders/default-order.toml",
		"packs/nested-import/orders/nested-order.toml",
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("migrated %s stat: %v", rel, err)
		}
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; result=%+v", got.Status, got)
	}
}

func TestV2LegacyOrderLayoutReportsRemoteImportedPackEvaluatedByLoader(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cityDir := t.TempDir()
	source := "https://github.com/example/orders-pack.git"
	repoDir := filepath.Join(t.TempDir(), "orders-pack")
	writeDoctorFile(t, repoDir, "pack.toml", `
[pack]
name = "ops"
schema = 1
`)
	writeDoctorFile(t, repoDir, "orders/nightly/order.toml", `
[order]
formula = "nightly"
trigger = "manual"
`)
	mustGitImport(t, repoDir, "init")
	mustGitImport(t, repoDir, "add", ".")
	mustGitImport(t, repoDir, "commit", "-m", "initial")
	commit := gitOutputImport(t, repoDir, "rev-parse", "HEAD")

	cacheDir := filepath.Join(homeDir, ".gc", "cache", "repos", config.RepoCacheKey(source, commit))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repoDir, cacheDir); err != nil {
		t.Fatal(err)
	}

	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "order-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "order-city"
schema = 1

[imports.ops]
source = "https://github.com/example/orders-pack.git"
version = "^1.2"
`)
	writeDoctorFile(t, cityDir, "packs.lock", fmt.Sprintf(`
schema = 1

[packs.%q]
version = "1.2.3"
commit = %q
fetched = "2026-05-20T00:00:00Z"
`, source, commit))

	_, _, loadErr := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if loadErr == nil || !strings.Contains(loadErr.Error(), "unsupported PackV1 order path") {
		t.Fatalf("LoadWithIncludes error = %v, want canonical legacy-order rejection", loadErr)
	}

	got := v2LegacyOrderLayoutCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error for remote imported legacy order; result=%+v", got.Status, got)
	}
	details := strings.Join(got.Details, "\n")
	for _, want := range []string{
		filepath.Join(cacheDir, "orders", "nightly", "order.toml"),
		"gc doctor --fix only changes files under the city",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q: %+v", want, got.Details)
		}
	}
}

func TestV2LegacyOrderLayoutFixMigratesRigPackReferencedOrders(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[rigs]]
name = "frontend"
includes = ["packs/rig-include"]

[rigs.imports.rig-import]
source = "packs/rig-import"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "packs/rig-include/pack.toml", `
[pack]
name = "rig-include"
schema = 2
`)
	writeDoctorFile(t, cityDir, "packs/rig-import/pack.toml", `
[pack]
name = "rig-import"
schema = 2
includes = ["../rig-nested"]
`)
	writeDoctorFile(t, cityDir, "packs/rig-nested/pack.toml", `
[pack]
name = "rig-nested"
schema = 2
`)
	for _, rel := range []string{
		"packs/rig-include/orders/include-order/order.toml",
		"packs/rig-import/orders/import-order/order.toml",
		"packs/rig-nested/orders/nested-order/order.toml",
	} {
		writeDoctorFile(t, cityDir, rel, `
[order]
trigger = "manual"
`)
	}

	check := v2LegacyOrderLayoutCheck{}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; result=%+v", got.Status, got)
	}
	for _, want := range []string{
		"packs/rig-include/orders/include-order/order.toml",
		"packs/rig-import/orders/import-order/order.toml",
		"packs/rig-nested/orders/nested-order/order.toml",
	} {
		if !strings.Contains(strings.Join(got.Details, "\n"), want) {
			t.Fatalf("details missing %q: %+v", want, got.Details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, rel := range []string{
		"packs/rig-include/orders/include-order.toml",
		"packs/rig-import/orders/import-order.toml",
		"packs/rig-nested/orders/nested-order.toml",
	} {
		if _, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("migrated %s stat: %v", rel, err)
		}
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; result=%+v", got.Status, got)
	}
}

func TestV2LegacyOrderLayoutFixLeavesExternalPackRefsForManualMigration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	absolutePackDir := filepath.Join(root, "shared-absolute")
	parentPackDir := filepath.Join(root, "shared-parent")
	writeDoctorFile(t, cityDir, "city.toml", fmt.Sprintf(`
[workspace]
name = "legacy-city"
includes = ["../shared-parent"]

[imports.absolute]
source = %q
`, absolutePackDir))
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "orders/local-order/order.toml", `
[order]
trigger = "manual"
`)
	for _, packDir := range []string{absolutePackDir, parentPackDir} {
		writeDoctorFile(t, packDir, "pack.toml", `
[pack]
name = "shared"
schema = 2
`)
		writeDoctorFile(t, packDir, "orders/shared-order/order.toml", `
[order]
trigger = "manual"
`)
	}

	check := v2LegacyOrderLayoutCheck{}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "orders/local-order.toml")); err != nil {
		t.Fatalf("city-local migrated order stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "orders/local-order/order.toml")); !os.IsNotExist(err) {
		t.Fatalf("city-local legacy order stat = %v, want removed", err)
	}
	for _, packDir := range []string{absolutePackDir, parentPackDir} {
		if _, err := os.Stat(filepath.Join(packDir, "orders/shared-order/order.toml")); err != nil {
			t.Fatalf("external legacy order was changed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(packDir, "orders/shared-order.toml")); !os.IsNotExist(err) {
			t.Fatalf("external flat order stat = %v, want absent", err)
		}
	}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("post-fix status = %v, want error for external manual migration; result=%+v", got.Status, got)
	}
	details := strings.Join(got.Details, "\n")
	for _, packDir := range []string{absolutePackDir, parentPackDir} {
		if !strings.Contains(details, filepath.Join(packDir, "orders/shared-order/order.toml")) {
			t.Fatalf("post-fix details missing external pack %s: %+v", packDir, got.Details)
		}
	}
}

func TestV2LegacyOrderLayoutFixLeavesExternalRigPackRefsForManualMigration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cityDir := filepath.Join(root, "city")
	includePackDir := filepath.Join(root, "shared-rig-include")
	importPackDir := filepath.Join(root, "shared-rig-import")
	writeDoctorFile(t, cityDir, "city.toml", fmt.Sprintf(`
[workspace]
name = "legacy-city"

[[rigs]]
name = "frontend"
includes = ["../shared-rig-include"]

[rigs.imports.shared]
source = %q
`, importPackDir))
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "legacy-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "orders/local-order/order.toml", `
[order]
trigger = "manual"
`)
	for _, packDir := range []string{includePackDir, importPackDir} {
		writeDoctorFile(t, packDir, "pack.toml", `
[pack]
name = "shared"
schema = 2
`)
		writeDoctorFile(t, packDir, "orders/shared-order/order.toml", `
[order]
trigger = "manual"
`)
	}

	check := v2LegacyOrderLayoutCheck{}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cityDir, "orders/local-order.toml")); err != nil {
		t.Fatalf("city-local migrated order stat: %v", err)
	}
	for _, packDir := range []string{includePackDir, importPackDir} {
		if _, err := os.Stat(filepath.Join(packDir, "orders/shared-order/order.toml")); err != nil {
			t.Fatalf("external rig pack legacy order was changed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(packDir, "orders/shared-order.toml")); !os.IsNotExist(err) {
			t.Fatalf("external rig pack flat order stat = %v, want absent", err)
		}
	}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("post-fix status = %v, want error for external manual migration; result=%+v", got.Status, got)
	}
	details := strings.Join(got.Details, "\n")
	for _, packDir := range []string{includePackDir, importPackDir} {
		if !strings.Contains(details, filepath.Join(packDir, "orders/shared-order/order.toml")) {
			t.Fatalf("post-fix details missing external rig pack %s: %+v", packDir, got.Details)
		}
	}
	if !strings.Contains(details, "gc doctor --fix only changes files under the city") {
		t.Fatalf("post-fix details missing manual external-pack guidance: %+v", got.Details)
	}
}

func TestV2LegacyOrderLayoutFixDeduplicatesRootPackSelfReference(t *testing.T) {
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
includes = ["."]
`)
	writeDoctorFile(t, cityDir, "orders/heartbeat/order.toml", `
[order]
trigger = "manual"
`)

	check := v2LegacyOrderLayoutCheck{}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "orders/heartbeat.toml")); err != nil {
		t.Fatalf("migrated order stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "orders/heartbeat/order.toml")); !os.IsNotExist(err) {
		t.Fatalf("legacy order stat = %v, want removed", err)
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; result=%+v", got.Status, got)
	}
}

func TestV2LegacyOrderLayoutFixRefusesTargetCollision(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", "[workspace]\nname = \"legacy-city\"\n")
	writeDoctorFile(t, cityDir, "orders/heartbeat/order.toml", `
[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeDoctorFile(t, cityDir, "orders/heartbeat.toml", `
[order]
exec = "scripts/existing.sh"
trigger = "manual"
`)

	err := (v2LegacyOrderLayoutCheck{}).Fix(&doctor.CheckContext{CityPath: cityDir})
	if err == nil {
		t.Fatal("Fix succeeded, want collision error")
	}
	if !strings.Contains(err.Error(), "target already exists") {
		t.Fatalf("Fix error = %v, want target collision", err)
	}
	if _, statErr := os.Stat(filepath.Join(cityDir, "orders/heartbeat/order.toml")); statErr != nil {
		t.Fatalf("legacy source was changed despite collision: %v", statErr)
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
	if !strings.Contains(out, "city.toml:6: rig \"frontend\" path") {
		t.Fatalf("doctor output missing rig path source coordinate:\n%s", out)
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

func TestV2DeprecationChecksFixLegacyRigPathRefusesOrphanSiteBinding(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	rigPath := filepath.Join(cityDir, "..", "frontend")
	orphanPath := filepath.Join(cityDir, "..", "backend")
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[rigs]]
name = "frontend"
path = "`+rigPath+`"
`)
	writeDoctorFile(t, cityDir, ".gc/site.toml", `
[[rig]]
name = "backend"
path = "`+orphanPath+`"
`)

	err := (v2RigPathSiteBindingCheck{}).Fix(&doctor.CheckContext{CityPath: cityDir})
	if err == nil {
		t.Fatal("Fix succeeded, want refusal for orphan site binding")
	}
	if !strings.Contains(err.Error(), "unknown rig names") || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("Fix error = %v, want orphan binding refusal", err)
	}

	rawData, readErr := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if readErr != nil {
		t.Fatalf("ReadFile(city.toml): %v", readErr)
	}
	if !strings.Contains(string(rawData), "path = ") {
		t.Fatalf("city.toml should retain rig.path after refused migration:\n%s", rawData)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "backend" || binding.Rigs[0].Path != orphanPath {
		t.Fatalf("site binding should remain unchanged after refused migration; binding=%+v", binding.Rigs)
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

func TestExpandedConfigLoadCheckReportsSchema2FragmentErrors(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
include = ["fragments/legacy.toml"]

[workspace]
name = "fragment-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "fragment-city"
schema = 2
`)
	writeDoctorFile(t, cityDir, "fragments/legacy.toml", `
[workspace]
includes = ["legacy-pack"]
`)

	got := expandedConfigLoadCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q", got.Status, got.Message)
	}
	for _, want := range []string{
		"expanded config load error",
		"fragments/legacy.toml",
		"unsupported PackV1 workspace.includes",
	} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message = %q, want substring %q", got.Message, want)
		}
	}
}

func TestExpandedConfigLoadCheckReportsImportedPackLegacyOrderPath(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
name = "order-city"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "order-city"
schema = 2

[imports.ops]
source = "./packs/ops"
`)
	writeDoctorFile(t, cityDir, "packs/ops/pack.toml", `
[pack]
name = "ops"
schema = 2
`)
	writeDoctorFile(t, cityDir, "packs/ops/orders/nightly/order.toml", `
[order]
formula = "nightly"
trigger = "manual"
`)

	got := expandedConfigLoadCheck{}.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error; message=%q", got.Status, got.Message)
	}
	for _, want := range []string{
		"expanded config load error",
		"unsupported PackV1 order path",
		"packs/ops/orders/nightly/order.toml",
	} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message = %q, want substring %q", got.Message, want)
		}
	}
}

func TestV2PackSourcesCheckReportsAndFixesReferencedPacks(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
includes = ["legacy"]

[packs.legacy]
source = "../packs/legacy"

[packs.unused]
source = "../packs/unused"
`)
	writeDoctorFile(t, cityDir, "pack.toml", `
[pack]
name = "pack-source-city"
schema = 2
`)

	check := v2PackSourcesCheck{}
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("pre-fix status = %v, want error", got.Status)
	}
	for _, want := range []string{
		"city.toml:4: [packs.legacy]",
		"city.toml:7: [packs.unused]",
		"gc doctor --fix",
		"manual",
	} {
		text := got.Message + "\n" + got.FixHint + "\n" + strings.Join(got.Details, "\n")
		if !strings.Contains(text, want) {
			t.Fatalf("check output missing %q:\n%+v", want, got)
		}
	}

	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusError {
		t.Fatalf("post-fix status = %v, want remaining error for unused pack", after.Status)
	}
	joined := strings.Join(after.Details, "\n")
	if strings.Contains(joined, "[packs.legacy]") {
		t.Fatalf("referenced pack should be removed after fix; details=%v", after.Details)
	}
	if !strings.Contains(joined, "[packs.unused]") {
		t.Fatalf("unused pack should remain for manual cleanup; details=%v", after.Details)
	}
}

func TestV2PackSourcesCheckFixClearsReferencedPacks(t *testing.T) {
	t.Parallel()

	cityDir := t.TempDir()
	writeDoctorFile(t, cityDir, "city.toml", `
[workspace]
includes = ["legacy"]

[packs.legacy]
source = "../packs/legacy"
`)

	check := v2PackSourcesCheck{}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if got := check.Run(&doctor.CheckContext{CityPath: cityDir}); got.Status != doctor.StatusOK {
		t.Fatalf("post-fix status = %v want OK; message=%q details=%v", got.Status, got.Message, got.Details)
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
	for _, want := range []string{
		"city.toml:2: workspace.name=legacy-city",
		"city.toml:3: workspace.prefix=lc",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing source coordinate %q:\n%s", want, out)
		}
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
	got := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if got.Status != doctor.StatusError {
		t.Fatalf("pre-fix status = %v, want error", got.Status)
	}
	if !strings.Contains(got.FixHint, "replace workspace.includes") {
		t.Fatalf("FixHint = %q, want actionable workspace.includes guidance", got.FixHint)
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
	if got.Status != doctor.StatusError {
		t.Fatalf("pre-fix status = %v, want error", got.Status)
	}
	if !strings.Contains(got.FixHint, "move each entry") {
		t.Fatalf("FixHint = %q, want actionable default rig import guidance", got.FixHint)
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
