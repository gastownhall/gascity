package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDiscoverPackDoctors_Basic(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/git-clean/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/git-clean/help.md", "doctor help")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].Name != "git-clean" {
		t.Fatalf("Name = %q, want %q", got[0].Name, "git-clean")
	}
	if got[0].HelpFile == "" {
		t.Fatal("HelpFile = empty, want discovered help.md")
	}
}

func TestDiscoverPackDoctors_ManifestOverride(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", `
description = "Check required binaries"
run = "../../shared/check.sh"
`)
	writeTestFile(t, packDir, "shared/check.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].Description != "Check required binaries" {
		t.Fatalf("Description = %q, want %q", got[0].Description, "Check required binaries")
	}
	wantRun := filepath.Join(packDir, "shared", "check.sh")
	if got[0].RunScript != wantRun {
		t.Fatalf("RunScript = %q, want %q", got[0].RunScript, wantRun)
	}
}

func TestDoctorManifestWarmupFieldParses(t *testing.T) {
	cases := []struct {
		name       string
		manifest   string
		wantWarmup bool
	}{
		{
			name: "explicit_true",
			manifest: `
description = "Check that X exists"
run = "check-x.sh"
warmup = true
`,
			wantWarmup: true,
		},
		{
			name: "explicit_false",
			manifest: `
description = "Check that X exists"
run = "check-x.sh"
warmup = false
`,
			wantWarmup: false,
		},
		{
			name: "default_omitted",
			manifest: `
description = "Check that X exists"
run = "check-x.sh"
`,
			wantWarmup: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			packDir := filepath.Join(dir, "mypk")
			writeTestFile(t, packDir, "doctor/check-x/doctor.toml", tc.manifest)
			writeTestFile(t, packDir, "doctor/check-x/check-x.sh", "#!/bin/sh\nexit 0\n")

			got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
			if err != nil {
				t.Fatalf("DiscoverPackDoctors: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d checks, want 1", len(got))
			}
			if got[0].Warmup != tc.wantWarmup {
				t.Errorf("Warmup = %v, want %v", got[0].Warmup, tc.wantWarmup)
			}
		})
	}
}

func TestDiscoverPackDoctors_RejectsEscapingOrAbsoluteRunPaths(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	tests := []struct {
		name string
		run  string
	}{
		{name: "absolute", run: "/tmp/outside.sh"},
		{name: "escape", run: "../../../outside.sh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeTestFile(t, packDir, "doctor/binaries/doctor.toml", "run = "+`"`+tt.run+`"`+"\n")
			writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

			_, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
			if err == nil {
				t.Fatal("DiscoverPackDoctors error = nil, want containment error")
			}
		})
	}
}

func TestDiscoverPackDoctors_SkipsHiddenAndUnderscoreDirs(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/.hidden/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/_internal/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].Name != "binaries" {
		t.Fatalf("Name = %q, want %q", got[0].Name, "binaries")
	}
}

func TestDiscoverPackDoctors_NoDoctorDir(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d checks, want 0", len(got))
	}
}

func TestDiscoverPackDoctors_BadManifest(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", "description = ")
	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

	_, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err == nil {
		t.Fatal("DiscoverPackDoctors error = nil, want manifest parse error")
	}
}

func TestDiscoverPackDoctors_SiblingFixScriptAutoDiscovered(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	// Pure convention: run.sh + sibling fix.sh, no doctor.toml.
	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/binaries/fix.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	wantFix := filepath.Join(packDir, "doctor", "binaries", "fix.sh")
	if got[0].FixScript != wantFix {
		t.Fatalf("FixScript = %q, want %q (sibling convention)",
			got[0].FixScript, wantFix)
	}
}

func TestDiscoverPackDoctors_NoSiblingFixScript(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	// Only run.sh — no sibling fix.sh and no manifest.
	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].FixScript != "" {
		t.Fatalf("FixScript = %q, want empty (no sibling fix.sh)",
			got[0].FixScript)
	}
}

func TestDiscoverPackDoctors_ManifestFixScript(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", `
description = "Check required binaries"
run = "check.sh"
fix = "fix.sh"
`)
	writeTestFile(t, packDir, "doctor/binaries/check.sh", "#!/bin/sh\nexit 0\n")
	writeTestFile(t, packDir, "doctor/binaries/fix.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	wantFix := filepath.Join(packDir, "doctor", "binaries", "fix.sh")
	if got[0].FixScript != wantFix {
		t.Fatalf("FixScript = %q, want %q", got[0].FixScript, wantFix)
	}
}

func TestDiscoverPackDoctors_FixScriptAbsentWhenNotDeclared(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	// doctor.toml with only run — no fix declared.
	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", `
description = "Diagnostic only"
run = "check.sh"
`)
	writeTestFile(t, packDir, "doctor/binaries/check.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].FixScript != "" {
		t.Fatalf("FixScript = %q, want empty (no fix declared)", got[0].FixScript)
	}
}

func TestDiscoverPackDoctors_FixScriptMissingOnDisk(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", `
run = "check.sh"
fix = "fix.sh"
`)
	writeTestFile(t, packDir, "doctor/binaries/check.sh", "#!/bin/sh\nexit 0\n")

	_, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err == nil {
		t.Fatal("DiscoverPackDoctors error = nil, want missing fix script error")
	}
	if !strings.Contains(err.Error(), "doctor/binaries fix") {
		t.Fatalf("DiscoverPackDoctors error = %v, want doctor fix context", err)
	}
}

func TestDiscoverPackDoctors_RejectsFixPathEscape(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/doctor.toml", `
run = "check.sh"
fix = "../../../outside.sh"
`)
	writeTestFile(t, packDir, "doctor/binaries/check.sh", "#!/bin/sh\nexit 0\n")

	_, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err == nil {
		t.Fatal("DiscoverPackDoctors error = nil, want containment error for fix")
	}
}

func TestDiscoverPackDoctors_PreservesPackDir(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "mypk")

	writeTestFile(t, packDir, "doctor/binaries/run.sh", "#!/bin/sh\nexit 0\n")

	got, err := DiscoverPackDoctors(fsys.OSFS{}, packDir, "mypk")
	if err != nil {
		t.Fatalf("DiscoverPackDoctors: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].PackDir != packDir {
		t.Fatalf("PackDir = %q, want %q", got[0].PackDir, packDir)
	}
}

func TestDoctorConfigValidationManifest(t *testing.T) {
	for _, tc := range []struct{ name, fields, wantError string }{
		{"opted-in", "config_validate = true\nconfig_validate_formulas = [\"arbitrary-plan\"]", ""},
		{"empty-input-list", "config_validate = true", ""},
		{"ordinary", "", ""},
		{"missing-opt-in", "config_validate_formulas = [\"plan\"]", "requires config_validate"},
		{"empty-name", "config_validate = true\nconfig_validate_formulas = [\"\"]", "empty formula"},
		{"duplicate-name", "config_validate = true\nconfig_validate_formulas = [\"plan\", \"plan\"]", "repeats formula"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, discovered := range []bool{false, true} {
				dir := t.TempDir()
				writeTestFile(t, dir, "check.sh", "#!/bin/sh\nexit 0\n")
				var got []DiscoveredDoctor
				var err error
				if discovered {
					writeTestFile(t, dir, "doctor/plan/run.sh", "#!/bin/sh\nexit 0\n")
					writeTestFile(t, dir, "doctor/plan/doctor.toml", tc.fields)
					got, err = DiscoverPackDoctors(fsys.OSFS{}, dir, "tools")
				} else {
					writeTestFile(t, dir, "pack.toml", "[pack]\nname = \"tools\"\nschema = 1\n[[doctor]]\nname = \"plan\"\nscript = \"check.sh\"\n"+tc.fields)
					writeTestFile(t, dir, "city.toml", "[workspace]\nname = \"demo\"\n")
					var cfg *City
					cfg, _, err = LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
					if cfg != nil {
						got = cfg.PackDoctors
					}
				}
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("discovered=%v: error=%v, want %s", discovered, err, tc.wantError)
					}
					continue
				}
				if err != nil || len(got) != 1 {
					t.Fatalf("discovered=%v: checks=%+v err=%v", discovered, got, err)
				}
				if tc.name == "opted-in" && (!got[0].ConfigValidate || len(got[0].ConfigValidateFormulas) != 1 || got[0].ConfigValidateFormulas[0] != "arbitrary-plan") {
					t.Fatalf("lost manifest fields: %+v", got[0])
				}
				if tc.name == "ordinary" && got[0].ConfigValidate {
					t.Fatal("ordinary check opted in")
				}
			}
		})
	}
}
