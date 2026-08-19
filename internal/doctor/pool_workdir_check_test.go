package doctor

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPoolWorkDirIsolationCheck_WarnsOnShared(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "claude", Dir: "demo"},
		},
	}
	chk := NewPoolWorkDirIsolationCheck(cfg, cityPath)
	res := chk.Run(&CheckContext{CityPath: cityPath})
	if res.Status != StatusWarning {
		t.Fatalf("Status = %v, want warn; message=%q", res.Status, res.Message)
	}
}

func TestPoolWorkDirIsolationCheck_OKAfterImplicitIsolation(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
	}
	config.InjectImplicitAgents(cfg)
	chk := NewPoolWorkDirIsolationCheck(cfg, cityPath)
	res := chk.Run(&CheckContext{CityPath: cityPath})
	if res.Status != StatusOK {
		t.Fatalf("Status = %v, want ok; message=%q", res.Status, res.Message)
	}
}
