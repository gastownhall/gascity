package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// runPoolIsolationDoctorCheck runs the pool-isolation check gc doctor
// registers for cfg and returns its result.
func runPoolIsolationDoctorCheck(t *testing.T, city string, cfg *config.City) *doctor.CheckResult {
	t.Helper()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	t.Cleanup(func() { doctorBeadStorePreflight = old })
	for _, c := range buildDoctorChecks(city, cfg, nil, buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true}) {
		if c.Name() == "pool-isolation" {
			return c.Run(&doctor.CheckContext{CityPath: city})
		}
	}
	t.Fatalf("gc doctor registers no pool-isolation check")
	return nil
}

func TestDoctorWarnsWhenPoolInstancesShareADirectory(t *testing.T) {
	explicitPool := func(workDir string) func(t *testing.T, cfg *config.City, rig string) {
		return func(_ *testing.T, cfg *config.City, _ string) {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: "builder", Dir: "app", Provider: "claude", MaxActiveSessions: intPtr(3), WorkDir: workDir})
		}
	}
	for _, tc := range []struct {
		name    string
		runtime string
		prep    func(t *testing.T, cfg *config.City, rig string)
		agent   string
		reason  string
	}{
		{name: "isolated", runtime: "tmux", prep: explicitPool(".gc/worktrees/{{.Rig}}/{{.AgentBase}}")},
		{
			name: "implicit_isolation_none", runtime: "tmux", agent: "app/claude", reason: `workspace.implicit_isolation = "none"`,
			prep: func(_ *testing.T, cfg *config.City, _ string) {
				cfg.Workspace.ImplicitIsolation = config.ImplicitIsolationNone
			},
		},
		{
			name: "rig_not_a_git_repository", runtime: "tmux", agent: "app/claude", reason: "is not a git repository",
			prep: func(t *testing.T, _ *config.City, rig string) {
				if err := os.RemoveAll(filepath.Join(rig, ".git")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "remote_runtime", runtime: "k8s", agent: "app/claude", reason: `session runtime "k8s" is not host-local`},
		{
			name: "explicit_pool_work_dir_not_per_instance", runtime: "tmux", agent: "app/builder", reason: "{{.AgentBase}}",
			prep: explicitPool(".gc/shared/builder"),
		},
		{
			name: "unborn_head", runtime: "tmux", agent: "app/claude", reason: "has no commit at HEAD",
			prep: func(t *testing.T, _ *config.City, rig string) {
				if err := os.RemoveAll(filepath.Join(rig, ".git")); err != nil {
					t.Fatal(err)
				}
				mustGit(t, rig, "init", "-q")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_SESSION", tc.runtime)
			city, _, cfg, _ := implicitSlotCityPrepared(t, func(cfg *config.City, rig string) {
				if tc.prep != nil {
					tc.prep(t, cfg, rig)
				}
			})
			res := runPoolIsolationDoctorCheck(t, city, cfg)
			text := res.Message + "\n" + strings.Join(res.Details, "\n")
			if tc.agent == "" {
				if res.Status != doctor.StatusOK {
					t.Fatalf("status = %v, want OK when every pool is isolated:\n%s", res.Status, text)
				}
				return
			}
			if res.Status != doctor.StatusWarning {
				t.Fatalf("status = %v, want a warning:\n%s", res.Status, text)
			}
			if !strings.Contains(res.Message, tc.agent) {
				t.Errorf("message %q does not name agent %q", res.Message, tc.agent)
			}
			var named bool
			for _, line := range res.Details {
				if strings.Contains(line, tc.agent) && strings.Contains(line, tc.reason) {
					named = true
				}
			}
			if !named {
				t.Errorf("no detail names agent %q with reason %q:\n%s", tc.agent, tc.reason, text)
			}
		})
	}
}
