package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludes_ComposesImportedPackCommandsAndDoctors(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "helper")
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "helper"
schema = 1
`)
	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/repo/sync/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.helper]
source = "../helper"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackCommands) != 2 {
		t.Fatalf("got %d PackCommands, want 2", len(cfg.PackCommands))
	}

	foundStatus := false
	foundRepoSync := false
	for _, cmd := range cfg.PackCommands {
		if reflect.DeepEqual(cmd.Command, []string{"status"}) {
			foundStatus = true
			if cmd.BindingName != "helper" {
				t.Fatalf("status BindingName = %q, want %q", cmd.BindingName, "helper")
			}
		}
		if reflect.DeepEqual(cmd.Command, []string{"repo", "sync"}) {
			foundRepoSync = true
			if cmd.BindingName != "helper" {
				t.Fatalf("repo sync BindingName = %q, want %q", cmd.BindingName, "helper")
			}
		}
	}
	if !foundStatus {
		t.Fatal("missing imported status command")
	}
	if !foundRepoSync {
		t.Fatal("missing imported repo sync command")
	}

	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].BindingName != "helper" {
		t.Fatalf("doctor BindingName = %q, want %q", cfg.PackDoctors[0].BindingName, "helper")
	}
}

func TestLoadWithIncludes_CityPackCommandsUsePackNameBinding(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "helper")
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "helper"
schema = 1
`)
	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../helper"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.PackCommands) != 1 {
		t.Fatalf("got %d PackCommands, want 1", len(cfg.PackCommands))
	}
	if cfg.PackCommands[0].BindingName != "helper" {
		t.Fatalf("BindingName = %q, want %q", cfg.PackCommands[0].BindingName, "helper")
	}
}

func TestLoadWithIncludes_RootPackCommandsAndDoctorsCompose(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "backstage"
schema = 2
`)
	writeTestFile(t, cityDir, "commands/hello/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, cityDir, "doctor/check/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackCommands) != 1 {
		t.Fatalf("got %d PackCommands, want 1", len(cfg.PackCommands))
	}
	if !reflect.DeepEqual(cfg.PackCommands[0].Command, []string{"hello"}) {
		t.Fatalf("command words = %#v, want %#v", cfg.PackCommands[0].Command, []string{"hello"})
	}
	if cfg.PackCommands[0].BindingName != "backstage" {
		t.Fatalf("BindingName = %q, want %q", cfg.PackCommands[0].BindingName, "backstage")
	}

	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].Name != "check" {
		t.Fatalf("doctor Name = %q, want %q", cfg.PackDoctors[0].Name, "check")
	}
}

func TestLoadWithIncludes_RootPackAgentsCompose(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, cityDir, "pack.toml", `
[pack]
name = "backstage"
schema = 2
`)
	writeTestFile(t, cityDir, "agents/worker/prompt.template.md", "You are the worker.\n")
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	explicit := explicitAgents(cfg.Agents)
	for _, a := range explicit {
		if a.Name == "worker" {
			if !strings.HasSuffix(a.PromptTemplate, "agents/worker/prompt.template.md") {
				t.Fatalf("worker PromptTemplate = %q, want scaffold path", a.PromptTemplate)
			}
			return
		}
	}
	t.Fatal("worker agent not discovered from root agents/ directory")
}

func TestLoadWithIncludes_LegacyPackTomlCommandsAndDoctorsStillCompose(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "legacy")
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "legacy"
schema = 1

[[doctor]]
name = "check-legacy"
script = "doctor/check-legacy.sh"
description = "Legacy doctor"

[[commands]]
name = "status"
description = "Legacy status"
long_description = "commands/status-help.txt"
script = "commands/status.sh"
`)
	writeTestFile(t, packDir, "doctor/check-legacy.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/status.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "commands/status-help.txt", "legacy help")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"
includes = ["../legacy"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackCommands) != 1 {
		t.Fatalf("got %d PackCommands, want 1", len(cfg.PackCommands))
	}
	if !reflect.DeepEqual(cfg.PackCommands[0].Command, []string{"status"}) {
		t.Fatalf("command words = %#v, want %#v", cfg.PackCommands[0].Command, []string{"status"})
	}
	if cfg.PackCommands[0].BindingName != "legacy" {
		t.Fatalf("BindingName = %q, want %q", cfg.PackCommands[0].BindingName, "legacy")
	}

	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].Name != "check-legacy" {
		t.Fatalf("doctor Name = %q, want %q", cfg.PackDoctors[0].Name, "check-legacy")
	}
}

func TestLoadWithIncludes_TransitiveFalseFiltersNestedCommandsAndDoctors(t *testing.T) {
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "parent")
	childDir := filepath.Join(dir, "child")
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, childDir, "pack.toml", `
[pack]
name = "child"
schema = 1
`)
	writeTestFile(t, childDir, "commands/repo/sync/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, childDir, "doctor/child-check/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, parentDir, "pack.toml", `
[pack]
name = "parent"
schema = 1

[imports.child]
source = "../child"
`)
	writeTestFile(t, parentDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, parentDir, "doctor/parent-check/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.ops]
source = "../parent"
transitive = false
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackCommands) != 1 {
		t.Fatalf("got %d PackCommands, want 1", len(cfg.PackCommands))
	}
	if !reflect.DeepEqual(cfg.PackCommands[0].Command, []string{"status"}) {
		t.Fatalf("command words = %#v, want %#v", cfg.PackCommands[0].Command, []string{"status"})
	}
	if cfg.PackCommands[0].BindingName != "ops" {
		t.Fatalf("command BindingName = %q, want %q", cfg.PackCommands[0].BindingName, "ops")
	}

	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].Name != "parent-check" {
		t.Fatalf("doctor Name = %q, want %q", cfg.PackDoctors[0].Name, "parent-check")
	}
}

func TestExpandPacks_RigImportsContributeDoctorsButNotCommands(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "helper")
	cityDir := filepath.Join(dir, "city")

	writeTestFile(t, packDir, "pack.toml", `
[pack]
name = "helper"
schema = 1
`)
	writeTestFile(t, packDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "frontend"
path = "../rig"

[rigs.imports.helper]
source = "../helper"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackCommands) != 0 {
		t.Fatalf("got %d PackCommands, want 0 for rig import commands", len(cfg.PackCommands))
	}
	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1 for rig import doctors", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].Name != "binaries" {
		t.Fatalf("doctor Name = %q, want %q", cfg.PackDoctors[0].Name, "binaries")
	}
}

func TestLoadWithIncludes_DiamondImportDedupsCommandsAndDoctors(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	aDir := filepath.Join(dir, "a")
	bDir := filepath.Join(dir, "b")
	cDir := filepath.Join(dir, "c")
	dDir := filepath.Join(dir, "d")

	writeTestFile(t, dDir, "pack.toml", `
[pack]
name = "shared"
schema = 1
`)
	writeTestFile(t, dDir, "commands/status/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, dDir, "doctor/shared-check/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, bDir, "pack.toml", `
[pack]
name = "b"
schema = 1

[imports.shared]
source = "../d"
`)
	writeTestFile(t, cDir, "pack.toml", `
[pack]
name = "c"
schema = 1

[imports.shared]
source = "../d"
`)
	writeTestFile(t, aDir, "pack.toml", `
[pack]
name = "a"
schema = 1

[imports.b]
source = "../b"

[imports.c]
source = "../c"
`)
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test"

[imports.ops]
source = "../a"
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	commandCount := 0
	doctorCount := 0
	for _, cmd := range cfg.PackCommands {
		if reflect.DeepEqual(cmd.Command, []string{"status"}) && cmd.BindingName == "shared" {
			commandCount++
		}
	}
	for _, check := range cfg.PackDoctors {
		if check.Name == "shared-check" && check.BindingName == "shared" {
			doctorCount++
		}
	}
	if commandCount != 1 {
		t.Fatalf("got %d shared status commands, want 1", commandCount)
	}
	if doctorCount != 1 {
		t.Fatalf("got %d shared doctor checks, want 1", doctorCount)
	}
}

func TestLoadWithIncludes_ImplicitImportsComposeCommandsAndDoctors(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cacheDir := GlobalRepoCachePath(gcHome, "github.com/gastownhall/gc-import", "abc123")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, cacheDir, "pack.toml", `
[pack]
name = "gc-import"
schema = 1
`)
	writeTestFile(t, cacheDir, "commands/list/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, cacheDir, "doctor/cache/run.sh", "#!/bin/sh\nexit 0\n")

	writeTestFile(t, gcHome, "implicit-import.toml", `
schema = 1

[imports.import]
source = "github.com/gastownhall/gc-import"
version = "0.2.0"
commit = "abc123"
`)

	cityDir := t.TempDir()
	writeTestFile(t, cityDir, "city.toml", `
[workspace]
name = "test-city"
`)

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if got := prov.Imports["import"]; got != "(implicit)" {
		t.Fatalf("prov.Imports[import] = %q, want %q", got, "(implicit)")
	}

	if len(cfg.PackCommands) != 1 {
		t.Fatalf("got %d PackCommands, want 1", len(cfg.PackCommands))
	}
	if !reflect.DeepEqual(cfg.PackCommands[0].Command, []string{"list"}) {
		t.Fatalf("command words = %#v, want %#v", cfg.PackCommands[0].Command, []string{"list"})
	}
	if cfg.PackCommands[0].BindingName != "import" {
		t.Fatalf("command BindingName = %q, want %q", cfg.PackCommands[0].BindingName, "import")
	}

	if len(cfg.PackDoctors) != 1 {
		t.Fatalf("got %d PackDoctors, want 1", len(cfg.PackDoctors))
	}
	if cfg.PackDoctors[0].Name != "cache" {
		t.Fatalf("doctor Name = %q, want %q", cfg.PackDoctors[0].Name, "cache")
	}
	if cfg.PackDoctors[0].BindingName != "import" {
		t.Fatalf("doctor BindingName = %q, want %q", cfg.PackDoctors[0].BindingName, "import")
	}
}
