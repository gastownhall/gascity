package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeHostedK8SScope(t *testing.T, root, id, prefix, database, host string, port int, origin string) {
	t.Helper()
	beadsDir := filepath.Join(root, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, root, id); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q,"project_id":%q,"dolt_server_host":%q,"dolt_server_port":%d}`+"\n", database, id, host, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf("issue_prefix: %s\ngc.endpoint_origin: %s\ngc.endpoint_status: verified\ndolt.host: %s\ndolt.port: %d\ndolt.mode: server\ndolt.auto-start: false\n", prefix, origin, host, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestK8SSessionStoreIdentityUsesSelectedScope(t *testing.T) {
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "frontend")
	writeHostedK8SScope(t, cityPath, "hq-project", "hq", "bd_hq_project", "hq.dolt.example", 4406, "city_canonical")
	writeHostedK8SScope(t, rigPath, "frontend-project", "fe", "bd_frontend_project", "frontend.dolt.example", 4407, "explicit")
	cfg := &config.City{Rigs: []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fe"}}}

	t.Run("Withheld managed-local target still resolves identity", func(t *testing.T) {
		got, err := k8sSessionStoreIdentityEnv(cityPath, cityPath, cfg, map[string]string{"GC_DOLT_MANAGED_LOCAL": "1"})
		if err != nil {
			t.Fatalf("managed-local identity: %v", err)
		}
		if got["GC_BEADS_PROJECT_ID"] != "hq-project" || got["GC_DOLT_DATABASE"] != "bd_hq_project" || got["GC_BEADS_PREFIX"] != "hq" {
			t.Fatalf("managed-local identity = %#v, want the HQ store identity", got)
		}
	})

	t.Run("City scope", func(t *testing.T) {
		got, err := k8sSessionStoreIdentityEnv(cityPath, cityPath, cfg, map[string]string{
			"GC_DOLT_HOST": "hq.dolt.example", "GC_DOLT_PORT": "4406",
		})
		if err != nil {
			t.Fatalf("City scoped hosted identity: %v", err)
		}
		for key, want := range map[string]string{
			"GC_BEADS_PROJECT_ID": "hq-project",
			"GC_BEADS_PREFIX":     "hq",
			"GC_DOLT_DATABASE":    "bd_hq_project",
			"GC_STORE_ROOT":       cityPath,
			"GC_STORE_SCOPE":      "city",
		} {
			if got[key] != want {
				t.Errorf("%s = %q, want %q", key, got[key], want)
			}
		}
	})

	t.Run("rig scope remains distinct from HQ", func(t *testing.T) {
		got, err := k8sSessionStoreIdentityEnv(cityPath, rigPath, cfg, map[string]string{
			"GC_DOLT_HOST": "frontend.dolt.example", "GC_DOLT_PORT": "4407",
		})
		if err != nil {
			t.Fatalf("rig scoped hosted identity: %v", err)
		}
		for key, want := range map[string]string{
			"GC_BEADS_PROJECT_ID": "frontend-project",
			"GC_BEADS_PREFIX":     "fe",
			"GC_DOLT_DATABASE":    "bd_frontend_project",
			"GC_STORE_ROOT":       rigPath,
			"GC_STORE_SCOPE":      "rig",
		} {
			if got[key] != want {
				t.Errorf("%s = %q, want %q", key, got[key], want)
			}
		}
		if got["GC_BEADS_PROJECT_ID"] == "hq-project" || got["GC_BEADS_PREFIX"] == "hq" {
			t.Fatalf("rig scope inherited City identity: %+v", got)
		}
	})
}

func TestResolveTemplateCarriesSelectedAndCityK8SIdentities(t *testing.T) {
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "frontend")
	writeHostedK8SScope(t, cityPath, "hq-project", "hq", "bd_hq_project", "hq.dolt.example", 4406, "city_canonical")
	writeHostedK8SScope(t, rigPath, "frontend-project", "fe", "bd_frontend_project", "frontend.dolt.example", 4407, "explicit")
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "test"},
		Providers: map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigPath, Prefix: "fe"}},
	}
	params := func() *agentBuildParams {
		return &agentBuildParams{
			city: cfg, cityName: "city", cityPath: cityPath, workspace: &cfg.Workspace,
			providers: cfg.Providers, lookPath: func(string) (string, error) { return "/bin/echo", nil },
			fs: fsys.OSFS{}, rigs: cfg.Rigs, beaconTime: time.Unix(0, 0),
			beadNames: make(map[string]string), stderr: io.Discard, sessionProvider: "k8s",
		}
	}

	t.Run("hybrid city provider receives pod store identity", func(t *testing.T) {
		agent := &config.Agent{Name: "mayor", Scope: "city", Provider: "test"}
		p := params()
		p.sessionProvider = "hybrid"
		tp, err := resolveTemplate(p, agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		if got := tp.Env["GC_BEADS_PROJECT_ID"]; got != "hq-project" {
			t.Errorf("GC_BEADS_PROJECT_ID = %q, want hq-project", got)
		}
		if got := tp.Env["GC_K8S_CITY_BEADS_PROJECT_ID"]; got != "hq-project" {
			t.Errorf("GC_K8S_CITY_BEADS_PROJECT_ID = %q, want hq-project", got)
		}
	})

	t.Run("HQ worker receives HQ identity", func(t *testing.T) {
		agent := &config.Agent{Name: "mayor", Scope: "city", Provider: "test"}
		tp, err := resolveTemplate(params(), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate(mayor): %v", err)
		}
		for key, want := range map[string]string{
			"GC_BEADS_PROJECT_ID":          "hq-project",
			"GC_BEADS_PREFIX":              "hq",
			"GC_DOLT_DATABASE":             "bd_hq_project",
			"GC_DOLT_HOST":                 "hq.dolt.example",
			"GC_DOLT_PORT":                 "4406",
			"GC_K8S_CITY_BEADS_PROJECT_ID": "hq-project",
			"GC_K8S_CITY_BEADS_PREFIX":     "hq",
			"GC_K8S_CITY_DOLT_DATABASE":    "bd_hq_project",
		} {
			if tp.Env[key] != want {
				t.Errorf("%s = %q, want %q", key, tp.Env[key], want)
			}
		}
	})

	t.Run("rig worker receives rig identity and separate HQ init identity", func(t *testing.T) {
		agent := &config.Agent{Name: "worker", Dir: "frontend", Scope: "rig", Provider: "test"}
		tp, err := resolveTemplate(params(), agent, "frontend/worker", nil)
		if err != nil {
			t.Fatalf("resolveTemplate(frontend/worker): %v", err)
		}
		for key, want := range map[string]string{
			"GC_BEADS_PROJECT_ID":          "frontend-project",
			"GC_BEADS_PREFIX":              "fe",
			"GC_DOLT_DATABASE":             "bd_frontend_project",
			"GC_DOLT_HOST":                 "frontend.dolt.example",
			"GC_DOLT_PORT":                 "4407",
			"GC_STORE_ROOT":                rigPath,
			"GC_STORE_SCOPE":               "rig",
			"GC_K8S_CITY_BEADS_PROJECT_ID": "hq-project",
			"GC_K8S_CITY_BEADS_PREFIX":     "hq",
			"GC_K8S_CITY_DOLT_DATABASE":    "bd_hq_project",
			"GC_K8S_CITY_DOLT_HOST":        "hq.dolt.example",
			"GC_K8S_CITY_DOLT_PORT":        "4406",
		} {
			if tp.Env[key] != want {
				t.Errorf("%s = %q, want %q", key, tp.Env[key], want)
			}
		}
		if tp.Env["GC_BEADS_PROJECT_ID"] == tp.Env["GC_K8S_CITY_BEADS_PROJECT_ID"] {
			t.Fatal("rig worker store identity collapsed to the City identity")
		}
	})
}

func TestK8SSessionStoreIdentityFailsClosedOnMissingOrConflictingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity string
		metadata string
		wantErr  string
		wantPath bool
	}{
		{name: "missing canonical identity", metadata: "project", wantErr: "missing project identity at", wantPath: true},
		{name: "identity mismatch", identity: "scope-id", metadata: "other-id", wantErr: "inconsistent project identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			beadsDir := filepath.Join(cityPath, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.identity != "" {
				if err := contract.WriteProjectIdentity(fsys.OSFS{}, cityPath, tc.identity); err != nil {
					t.Fatal(err)
				}
			}
			metadata := fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"bd_project","project_id":%q}`, tc.metadata)
			if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: pr\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: db.example\ndolt.port: 4406\ndolt.mode: server\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := k8sSessionStoreIdentityEnv(cityPath, cityPath, &config.City{}, map[string]string{
				"GC_DOLT_HOST": "db.example", "GC_DOLT_PORT": "4406",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("identity error = %v, want containing %q", err, tc.wantErr)
			}
			if tc.wantPath && !strings.Contains(err.Error(), contract.ProjectIdentityPath(cityPath)) {
				t.Fatalf("identity error = %v, want canonical path %q", err, contract.ProjectIdentityPath(cityPath))
			}
		})
	}
}
