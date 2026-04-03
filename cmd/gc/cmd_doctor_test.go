package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestCollectPackDirsEmpty(t *testing.T) {
	cfg := &config.City{}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 0 {
		t.Errorf("expected no dirs, got %v", dirs)
	}
}

func TestCollectPackDirsCityLevel(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/a", "/b"},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirs = %v, want [/a /b]", dirs)
	}
}

func TestCollectPackDirsRigLevel(t *testing.T) {
	cfg := &config.City{
		RigPackDirs: map[string][]string{
			"rig1": {"/x", "/y"},
			"rig2": {"/z"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 3 {
		t.Fatalf("expected 3 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsDeduplicates(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/shared", "/a"},
		RigPackDirs: map[string][]string{
			"rig1": {"/shared", "/b"}, // /shared is a duplicate
		},
	}
	dirs := collectPackDirs(cfg)
	// /shared should appear only once.
	count := 0
	for _, d := range dirs {
		if d == "/shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("/shared appears %d times, want 1", count)
	}
	if len(dirs) != 3 {
		t.Fatalf("expected 3 unique dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsMixed(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/city-topo"},
		RigPackDirs: map[string][]string{
			"rig1": {"/rig-topo"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestDoctorSkipsSuspendedRigChecks(t *testing.T) {
	t.Parallel()
	activeDir := t.TempDir()
	suspendedDir := t.TempDir()

	rigs := []config.Rig{
		{Name: "active-rig", Path: activeDir},
		{Name: "suspended-rig", Path: suspendedDir, Suspended: true},
	}

	// Mirror the per-rig registration logic from doDoctor.
	d := &doctor.Doctor{}
	for _, rig := range rigs {
		if rig.Suspended {
			continue
		}
		d.Register(doctor.NewRigPathCheck(rig))
	}

	var buf bytes.Buffer
	ctx := &doctor.CheckContext{CityPath: t.TempDir()}
	d.Run(ctx, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "active-rig") {
		t.Error("expected active-rig checks to be registered")
	}
	if strings.Contains(out, "suspended-rig") {
		t.Error("suspended-rig checks should not be registered")
	}
}
