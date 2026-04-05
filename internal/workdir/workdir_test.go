package workdir

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func intPtr(n int) *int { return &n }

func TestResolveWorkDirPathUsesWorkDirTemplate(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "gastown"
	cfg := &config.City{
		Workspace: config.Workspace{Name: cityName},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
	}
	agent := config.Agent{
		Name:    "refinery",
		Dir:     "demo",
		WorkDir: ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
	}

	got := ResolveWorkDirPath(cityPath, cityName, "demo/refinery", agent, cfg.Rigs)
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "refinery")
	if got != want {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, want)
	}
}

func TestResolveWorkDirPathDefaultsRigScopedAgentsToRigRoot(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo-repo")
	got := ResolveWorkDirPath(cityPath, "gastown", "demo/refinery", config.Agent{
		Name: "refinery",
		Dir:  "demo",
	}, []config.Rig{{Name: "demo", Path: rigRoot}})
	if got != rigRoot {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, rigRoot)
	}
}

func TestResolveWorkDirPathUsesPoolInstanceBase(t *testing.T) {
	cityPath := t.TempDir()
	got := ResolveWorkDirPath(cityPath, "gastown", "demo/polecat-2", config.Agent{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}, []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}})
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-2")
	if got != want {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, want)
	}
}

func TestCityNameFallsBackToCityDirBase(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city-root")
	got := CityName(cityPath, &config.City{})
	if got != "city-root" {
		t.Fatalf("CityName() = %q, want %q", got, "city-root")
	}
}

func TestResolveWorkDirPathStrictRejectsInvalidTemplate(t *testing.T) {
	cityPath := t.TempDir()
	_, err := ResolveWorkDirPathStrict(cityPath, "gastown", "demo/refinery", config.Agent{
		Name:    "refinery",
		Dir:     "demo",
		WorkDir: ".gc/worktrees/{{.RigName}}/refinery",
	}, []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}})
	if err == nil {
		t.Fatal("ResolveWorkDirPathStrict() error = nil, want invalid template error")
	}
}
