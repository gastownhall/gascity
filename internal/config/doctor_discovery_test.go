package config

import (
	"path/filepath"
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
