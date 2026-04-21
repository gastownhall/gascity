package workdir

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestSessionQualifiedNameCanonicalizesBareAndQualifiedPoolAliases(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{
		Name:              "polecat",
		Dir:               "demo",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}

	bare := SessionQualifiedName(cityPath, agent, rigs, "polecat-fenrir", "")
	qualified := SessionQualifiedName(cityPath, agent, rigs, "demo/polecat-fenrir", "")
	if bare != "demo/polecat-fenrir" {
		t.Fatalf("SessionQualifiedName(bare) = %q, want %q", bare, "demo/polecat-fenrir")
	}
	if qualified != bare {
		t.Fatalf("SessionQualifiedName(qualified) = %q, want %q", qualified, bare)
	}
}

func TestSessionQualifiedNameKeepsSingletonTemplateIdentity(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{Name: "witness", Dir: "demo", MaxActiveSessions: intPtr(1)}

	if got := SessionQualifiedName(cityPath, agent, rigs, "demo/boot", ""); got != "demo/witness" {
		t.Fatalf("SessionQualifiedName() = %q, want %q", got, "demo/witness")
	}
}

func TestSessionQualifiedNamePreservesRigQualifiedBindingIdentity(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{
		Name:              "worker",
		Dir:               "demo",
		BindingName:       "ops",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}

	if got := SessionQualifiedName(cityPath, agent, rigs, "ops.worker-1", ""); got != "demo/ops.worker-1" {
		t.Fatalf("SessionQualifiedName(bare binding) = %q, want %q", got, "demo/ops.worker-1")
	}
	if got := SessionQualifiedName(cityPath, agent, rigs, "demo/ops.worker-1", ""); got != "demo/ops.worker-1" {
		t.Fatalf("SessionQualifiedName(rig-qualified binding) = %q, want %q", got, "demo/ops.worker-1")
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

func TestExpandCommandTemplateFallsBackToCityDirBase(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	agent := config.Agent{Name: "worker"}

	got, err := ExpandCommandTemplate("echo {{.CityName}}", cityPath, "", agent, nil)
	if err != nil {
		t.Fatalf("ExpandCommandTemplate() error = %v, want nil", err)
	}
	if got != "echo demo-city" {
		t.Fatalf("ExpandCommandTemplate() = %q, want %q", got, "echo demo-city")
	}
}

func TestConfiguredRigNameMatchesSymlinkAliasPath(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	rigPath := filepath.Join(realRoot, "demo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	aliasRigPath := filepath.Join(aliasRoot, "demo")
	got := ConfiguredRigName(t.TempDir(), config.Agent{
		Name: "worker",
		Dir:  aliasRigPath,
	}, []config.Rig{{Name: "demo", Path: rigPath}})
	if got != "demo" {
		t.Fatalf("ConfiguredRigName() = %q, want %q", got, "demo")
	}
}

func TestSamePathUsesSharedPathNormalization(t *testing.T) {
	a := "/private/tmp/gc-home"
	b := "/tmp/gc-home"
	got := samePath(a, b)
	want := runtime.GOOS == "darwin"
	if got != want {
		t.Fatalf("samePath(%q, %q) = %v, want %v", a, b, got, want)
	}
}
