package validation

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestValidatePoolWorkDirIsolation_DetectsSharedRigRoot(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: rigRoot}},
		Agents: []config.Agent{
			{Name: "claude", Dir: "demo"}, // multi-session (max nil) + empty work_dir
		},
	}
	got := ValidatePoolWorkDirIsolation(cityPath, cfg)
	if len(got) != 1 {
		t.Fatalf("got %d collisions, want 1: %+v", len(got), got)
	}
	if got[0].WorkDir != rigRoot {
		t.Fatalf("WorkDir = %q, want rig root %q", got[0].WorkDir, rigRoot)
	}
}

func TestValidatePoolWorkDirIsolation_OKWhenTemplated(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{
				Name:    "claude",
				Dir:     "demo",
				WorkDir: config.ImplicitRigPoolWorkDir,
			},
		},
	}
	if got := ValidatePoolWorkDirIsolation(cityPath, cfg); len(got) != 0 {
		t.Fatalf("want no collisions, got %+v", got)
	}
}

func TestValidatePoolWorkDirIsolation_SkipsSingletons(t *testing.T) {
	cityPath := t.TempDir()
	one := 1
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "witness", Dir: "demo", MaxActiveSessions: &one},
		},
	}
	if got := ValidatePoolWorkDirIsolation(cityPath, cfg); len(got) != 0 {
		t.Fatalf("singleton should not warn, got %+v", got)
	}
}
