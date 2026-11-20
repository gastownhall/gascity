package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/rig"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Registration must expose canonical project hooks before setup review and
// input pinning. Startup then consumes exactly those reviewed bytes.
func TestRigPostProvisionMaterializesProviderHooksBeforeStartup(t *testing.T) {
	for _, provider := range []string{"codex", "fast-codex"} {
		for _, existing := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/existing=%t/explicit=%t", provider, existing, explicit), func(t *testing.T) {
					city := t.TempDir()
					repo := filepath.Join(t.TempDir(), "project")
					if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o755); err != nil {
						t.Fatal(err)
					}
					hook := filepath.Join(repo, ".codex", "hooks.json")
					if existing {
						userHook := "{\"hooks\":{\"Stop\":[{\"matcher\":\"\",\"hooks\":[{\"type\":\"command\",\"command\":\"user-hook --keep\"}]}],\"SessionStart\":[{\"matcher\":\"\",\"hooks\":[{\"type\":\"command\",\"command\":\"user-start --keep\"}]}],\"UserPromptSubmit\":[{\"matcher\":\"\",\"hooks\":[{\"type\":\"command\",\"command\":\"user-prompt --keep\"}]}]},\"operator_number\":9007199254740993}"
						if err := os.WriteFile(hook, []byte(userHook), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					overlay := filepath.Join(city, "project-overlay")
					custom := filepath.Join(overlay, "per-provider", provider, ".codex", "hooks.json")
					if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(custom, []byte("{\"hooks\":{\"UserPromptSubmit\":[{\"matcher\":\"\",\"hooks\":[{\"type\":\"command\",\"command\":\"configured-project-hook --keep\"}]}]}}"), 0o644); err != nil {
						t.Fatal(err)
					}
					spec := config.BuiltinProviderAlias("codex")
					spec.Command = "/bin/echo"
					spec.PathCheck = "/bin/echo"
					spec.ResumeCommand = "/bin/echo resume {{.SessionKey}}"
					cfg := config.City{
						Workspace: config.Workspace{Provider: provider},
						Providers: map[string]config.ProviderSpec{provider: spec},
						Rigs:      []config.Rig{{Name: "project", Path: repo}},
						Agents:    []config.Agent{{Name: "worker", Scope: "rig", Dir: "project", OverlayDir: overlay}},
					}
					reviewOverlay := filepath.Join(city, "review-overlay")
					reviewFile := filepath.Join(reviewOverlay, "per-provider", provider, ".codex", "hooks.json")
					if err := os.MkdirAll(filepath.Dir(reviewFile), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(reviewFile, []byte("{\"hooks\":{\"Stop\":[{\"type\":\"command\",\"command\":\"configured-review-hook --keep\"}]}}"), 0o644); err != nil {
						t.Fatal(err)
					}
					cfg.Agents = append(cfg.Agents, config.Agent{Name: "reviewer", Scope: "rig", Dir: "project", OverlayDir: reviewOverlay})
					if explicit {
						cfg.Workspace.InstallAgentHooks = []string{provider}
					}
					agents := append([]config.Agent(nil), cfg.Agents...)
					cityTOML := fmt.Sprintf("[workspace]\nprovider = %q\n", provider)
					if explicit {
						cityTOML += fmt.Sprintf("install_agent_hooks = [%q]\n", provider)
					}
					cityTOML += fmt.Sprintf("\n[providers.%s]\nbase = \"builtin:codex\"\ncommand = \"/bin/echo\"\npath_check = \"/bin/echo\"\nresume_command = \"/bin/echo resume {{.SessionKey}}\"\n\n[[rigs]]\nname = \"project\"\n", provider)
					siteTOML := fmt.Sprintf("workspace_name = \"test-city\"\n\n[[rig]]\nname = \"project\"\npath = %q\n", repo)
					writeSchema2RigCity(t, city, "test-city", cityTOML, siteTOML)
					var data []byte

					for _, agent := range agents {
						agentFile := filepath.Join(city, "agents", agent.Name, "agent.toml")
						if err := os.MkdirAll(filepath.Dir(agentFile), 0o755); err != nil {
							t.Fatal(err)
						}
						text := fmt.Sprintf("scope = \"rig\"\noverlay_dir = %q\n", agent.OverlayDir)
						if err := os.WriteFile(agentFile, []byte(text), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					effective, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
					if err != nil {
						t.Fatal(err)
					}
					agent := config.FindAgent(effective, "project/worker")
					if agent == nil {
						t.Fatal("composed fixture has no project/worker")
					}
					home, err := resolveConfiguredWorkDirPath(city, "test-city", agent.QualifiedName(), agent, effective.Rigs)
					if err != nil || filepath.Clean(home) != filepath.Clean(repo) {
						t.Fatalf("fixture agent does not target registered rig: home=%q err=%v", home, err)
					}
					cs := &controllerState{cityPath: city}
					cs.rigPostProvisionLocal("project", rig.ProvisionContext{RigPath: repo, Rig: cfg.Rigs[0], Cfg: &cfg})
					data, err = os.ReadFile(hook)
					if err != nil {
						t.Fatalf("hooks must exist at registration: %v", err)
					}
					if !strings.Contains(string(data), "GC_BIN") || !strings.Contains(string(data), "configured-project-hook --keep") || !strings.Contains(string(data), "configured-review-hook --keep") {
						t.Fatalf("registration did not compose managed and configured hooks: %s", data)
					}
					if existing {
						for _, text := range []string{"user-hook --keep", "user-start --keep", "user-prompt --keep", "9007199254740993"} {
							if !strings.Contains(string(data), text) {
								t.Fatalf("registration lost operator hook field %q: %s", text, data)
							}
						}
					}
					if (len(cfg.Workspace.InstallAgentHooks) > 0) != explicit || (explicit && cfg.Workspace.InstallAgentHooks[0] != provider) {
						t.Fatal("registration changed install_agent_hooks")
					}
					bp := newAgentBuildParams("test-city", city, effective, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
					for _, name := range []string{"project/worker", "project/reviewer"} {
						agent := config.FindAgent(effective, name)
						if agent == nil {
							t.Fatalf("composed fixture has no %s", name)
						}
						ownership := prepareTemplateResolution(bp, agent, agent.QualifiedName(), io.Discard)
						if ownership[materialize.CodexManagedMergeablePath] == "" {
							t.Fatal("startup did not verify canonical configured-home ownership")
						}
						hints := runtime.Config{WorkDir: repo}
						resolved, err := config.ResolveProvider(agent, &effective.Workspace, effective.Providers, exec.LookPath)
						if err != nil {
							t.Fatal(err)
						}
						materialize.ApplyConfiguredSessionOverlayHints(city, effective, agent.QualifiedName(), resolved, &hints)
						materialize.ApplyVerifiedMergeableOwnership(&hints, ownership)
						if err := runtime.StageSessionWorkDir(hints); err != nil {
							t.Fatal(err)
						}
						after, err := os.ReadFile(hook)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(data, after) {
							t.Fatalf("startup changed reviewed hook bytes:\nbefore=%s\nafter=%s", data, after)
						}
					}
				})
			}
		}
	}
}

func TestRigAgentHookProfileAllowsAgentOverrideOfWorkspaceOverlay(t *testing.T) {
	city, repo, base, agentOverlay := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for dir, value := range map[string]string{base: "base", agentOverlay: "agent"} {
		file := filepath.Join(dir, ".codex", "hooks.json")
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf("{\"operator_policy\":%q}", value)
		if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := config.BuiltinProviderAlias("codex")
	spec.Command, spec.PathCheck, spec.ResumeCommand = "/bin/echo", "/bin/echo", "/bin/echo resume {{.SessionKey}}"
	cfg := &config.City{
		Workspace:       config.Workspace{Provider: "codex"},
		Providers:       map[string]config.ProviderSpec{"codex": spec},
		Rigs:            []config.Rig{{Name: "project", Path: repo}},
		PackOverlayDirs: []string{base},
		Agents:          []config.Agent{{Name: "worker", Scope: "rig", Dir: "project", OverlayDir: agentOverlay}},
	}
	if err := installRigAgentHooks(fsys.OSFS{}, city, "project", repo, cfg); err != nil {
		t.Fatalf("valid agent override refused: %v", err)
	}
	agent := &cfg.Agents[0]
	resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		t.Fatal(err)
	}
	hints := runtime.Config{}
	materialize.ApplyConfiguredSessionOverlayHints(city, cfg, agent.QualifiedName(), resolved, &hints)
	layers, err := materialize.ReadConfiguredCodexHookOverlayLayers(hints)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"operator_policy\": \"agent\"") || !hooks.CodexHooksAreConvergedWithOverlays(data, city, layers) {
		t.Fatalf("agent override is not stable: %s", data)
	}
}

func TestRigAgentHookPreparationDoesNotCreateOtherAgentHomes(t *testing.T) {
	city, repo := t.TempDir(), t.TempDir()
	unrelated := filepath.Join(t.TempDir(), "do-not-create")
	spec := config.BuiltinProviderAlias("codex")
	spec.Command, spec.PathCheck, spec.ResumeCommand = "/bin/echo", "/bin/echo", "/bin/echo resume {{.SessionKey}}"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "codex"},
		Providers: map[string]config.ProviderSpec{"codex": spec},
		Rigs:      []config.Rig{{Name: "project", Path: repo}},
		Agents:    []config.Agent{{Name: "unrelated", Scope: "city", WorkDir: unrelated}},
	}
	if err := installRigAgentHooks(fsys.OSFS{}, city, "project", repo, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unrelated); !os.IsNotExist(err) {
		t.Fatalf("registration materialized another agent's home: %v", err)
	}
}

func TestRigAgentHookPreparationUsesExplicitRigRolePrecedence(t *testing.T) {
	for _, selected := range []string{"claude", "codex"} {
		t.Run(selected, func(t *testing.T) {
			city, repo, rootOverlay, boundOverlay := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			for dir, policy := range map[string]string{rootOverlay: "root", boundOverlay: "bound"} {
				file := filepath.Join(dir, ".codex", "hooks.json")
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(fmt.Sprintf("{\"operator_policy\":%q}", policy)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			providers := make(map[string]config.ProviderSpec)
			for _, name := range []string{"claude", "codex"} {
				spec := config.BuiltinProviderAlias(name)
				spec.Command, spec.PathCheck, spec.ResumeCommand = "/bin/echo", "/bin/echo", "/bin/echo resume {{.SessionKey}}"
				providers[name] = spec
			}
			cfg := &config.City{
				Workspace: config.Workspace{Provider: "claude"}, Providers: providers,
				Rigs: []config.Rig{{Name: "project", Path: repo}},
				Agents: []config.Agent{
					{Name: "worker", Scope: "rig", Provider: "codex", OverlayDir: rootOverlay},
					{Name: "worker", Scope: "rig", Dir: "project", Provider: selected, OverlayDir: boundOverlay},
				},
			}
			if agent := config.FindAgent(cfg, "project/worker"); agent == nil || agent.Provider != selected {
				t.Fatal("fixture does not select explicit rig role")
			}
			if err := installRigAgentHooks(fsys.OSFS{}, city, "project", repo, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(repo, ".codex", "hooks.json"))
			if selected == "claude" {
				if !os.IsNotExist(err) {
					t.Fatalf("shadowed Codex template materialized hooks: data=%s err=%v", data, err)
				}
			} else if err != nil || !strings.Contains(string(data), "\"operator_policy\": \"bound\"") {
				t.Fatalf("selected Codex role did not own its configuration: data=%s err=%v", data, err)
			}
			if cfg.Agents[0].Dir != "" {
				t.Fatal("registration changed the root template")
			}
		})
	}
}
