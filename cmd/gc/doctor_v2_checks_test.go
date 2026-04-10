package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
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
	if err := os.MkdirAll(filepath.Join(cityDir, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(scripts): %v", err)
	}

	var buf bytes.Buffer
	d := &doctor.Doctor{}
	registerV2DeprecationChecks(d)
	d.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true}, &buf, false)

	out := buf.String()
	for _, name := range []string{
		"v2-agent-format",
		"v2-import-format",
		"v2-default-rig-import-format",
		"v2-scripts-layout",
		"v2-workspace-name",
		"v2-prompt-template-suffix",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("doctor output missing %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "gc import migrate") {
		t.Fatalf("doctor output missing migrate hint:\n%s", out)
	}
	if !strings.Contains(out, ".md.tmpl") {
		t.Fatalf("doctor output missing .md.tmpl guidance:\n%s", out)
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
