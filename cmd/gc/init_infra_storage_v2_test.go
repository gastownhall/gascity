package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A lost final witness write leaves all other durable files exactly as a
// successful initialization wrote them, but its witness is still pending.
// Recovery must use those files rather than require the old process to survive.
func TestNativeInfrastructurePendingWitnessRecoversReadyOwnership(t *testing.T) {
	for _, originalConfig := range []bool{false, true} {
		name := "ready-config"
		if originalConfig {
			name = "original-config"
		}
		t.Run(name, func(t *testing.T) {
			city, raw, plan := nativeInfrastructureFixture(t)
			if err := resumeInitInfrastructureWithOps(context.Background(), city, nativeInfrastructureFakeOps(t, plan)); err != nil {
				t.Fatal(err)
			}
			pending, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
			if err != nil || pending == nil || !pending.Ready {
				t.Fatalf("completed plan=%+v err=%v", pending, err)
			}
			pending.Ready = false
			if err := saveInitInfrastructurePlan(fsys.OSFS{}, city, *pending); err != nil {
				t.Fatal(err)
			}
			if originalConfig {
				if err := os.WriteFile(filepath.Join(city, "city.toml"), raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			probes := 0
			resumed := nativeInfrastructureFakeOps(t, *pending)
			resumed.Verify = func(_ context.Context, _ string, recovered initInfrastructurePlan) error {
				probes++
				if recovered.ProjectID != pending.ProjectID || recovered.Database != pending.Database {
					t.Fatalf("recovery changed database identity: %+v", recovered)
				}
				return nil
			}
			if err := resumeInitInfrastructureWithOps(context.Background(), city, resumed); err != nil {
				t.Fatalf("ready ownership must not strand a pending witness: %v", err)
			}
			ready, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
			if err != nil || ready == nil || !ready.Ready || probes != 1 {
				t.Fatalf("ready=%+v probes=%d err=%v", ready, probes, err)
			}
			scope := filepath.Join(city, ".gc", "storage", "infra")
			entry, owned, err := providerScopeOwnership(city, scope)
			if err != nil || !owned || entry.State != providerScopeReady {
				t.Fatalf("ownership=%+v owned=%t err=%v", entry, owned, err)
			}
		})
	}
}

func TestNativeInfrastructureReadyOwnershipStillRefusesIdentityChanges(t *testing.T) {
	for _, mutation := range []string{"project", "database", "workspace-owner", "config", "pack"} {
		t.Run(mutation, func(t *testing.T) {
			city, _, plan := nativeInfrastructureFixture(t)
			if err := resumeInitInfrastructureWithOps(context.Background(), city, nativeInfrastructureFakeOps(t, plan)); err != nil {
				t.Fatal(err)
			}
			pending, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
			if err != nil || pending == nil {
				t.Fatalf("plan=%+v err=%v", pending, err)
			}
			pending.Ready = false
			if err := saveInitInfrastructurePlan(fsys.OSFS{}, city, *pending); err != nil {
				t.Fatal(err)
			}
			workspace := filepath.Join(city, ".gc", "storage", "infra")
			switch mutation {
			case "project", "database":
				md := initInfrastructureMetadata{Backend: "dolt", Mode: "server", Database: plan.Database, ProjectID: pending.ProjectID}
				if mutation == "project" {
					md.ProjectID = "foreign-project"
				} else {
					md.Database = "foreign-database"
				}
				b, _ := json.Marshal(md)
				if err := os.WriteFile(filepath.Join(workspace, ".beads", "metadata.json"), b, 0o600); err != nil {
					t.Fatal(err)
				}
			case "workspace-owner":
				if err := os.WriteFile(filepath.Join(workspace, initInfrastructureOwnerFile), []byte(strings.Repeat("f", 32)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "config", "pack":
				filename := "city.toml"
				if mutation == "pack" {
					filename = "pack.toml"
				}
				f, err := os.OpenFile(filepath.Join(city, filename), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("\n# edited after interruption\n"); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(city, "city.toml"))
			ops := nativeInfrastructureFakeOps(t, *pending)
			ops.Run = func(context.Context, string, []string, ...string) error {
				t.Error("identity mismatch reached a provider command")
				return nil
			}
			ops.Verify = func(context.Context, string, initInfrastructurePlan) error {
				t.Error("identity mismatch reached a native probe")
				return nil
			}
			if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil {
				t.Fatal("changed identity was accepted")
			}
			after, _ := os.ReadFile(filepath.Join(city, "city.toml"))
			if !bytes.Equal(before, after) {
				t.Fatal("identity refusal replaced city configuration")
			}
			stored, _ := readInitInfrastructurePlan(fsys.OSFS{}, city)
			if stored.Ready {
				t.Fatal("identity mismatch marked ready")
			}
		})
	}
}

// These unit fixtures exercise actual authored scaffold and composition logic,
// not provider readiness. Keep GC_DOLT unset through intent recording; defer only
// finalization's external database work when its dependency seam is reached.
func nativeInfrastructureDeferAuthoredFinalization(t *testing.T, city string) {
	t.Helper()
	stubInitDependencyChecks(t)
	lookup := initLookPath
	reached := false
	initLookPath = func(name string) (string, error) {
		if !reached {
			if _, err := os.Stat(filepath.Join(city, "city.toml")); err != nil {
				t.Fatalf("prerequisite seam ran before the authored scaffold existed: %v", err)
			}
			reached = true
			t.Setenv("GC_DOLT", "skip")
		}
		return lookup(name)
	}
	t.Cleanup(func() {
		if !reached {
			t.Error("fixture never reached authored finalization dependency checks")
		}
	})
}

func TestNativeInfrastructureAuthoredScaffoldNeverCreatesSplitIntent(t *testing.T) {
	for _, mode := range []string{"file", "directory"} {
		for _, policy := range []string{"plain", "include-native-off", "include-ops", "agent-defaults"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				t.Setenv("GC_BEADS", "bd")
				t.Setenv("GC_DOLT", "")
				t.Setenv("GC_HOME", t.TempDir())
				src := t.TempDir()
				city := filepath.Join(t.TempDir(), "authored-city")
				nativeInfrastructureDeferAuthoredFinalization(t, city)
				content := "[workspace]\nprovider=\"codex\"\n[beads]\nprovider=\"bd\"\n[providers.codex]\nbase=\"builtin:codex\"\n"
				fragment := ""
				fragmentName := ""
				if strings.HasPrefix(policy, "include-") {
					// --file preserves its authored include reference; --from copies the
					// directory. Both references must resolve in their actual destination.
					ref := "policy.toml"
					if mode == "file" {
						ref = filepath.Join(src, ref)
					}
					content = fmt.Sprintf("include=[%q]\n", ref) + content
					fragment = "[beads]\nnative_transport=\"off\"\n"
					if policy == "include-ops" {
						fragment = "[ops]\nauthoritative=[]\n"
					}
					fragmentName = filepath.Join(src, "policy.toml")
					if err := os.WriteFile(fragmentName, []byte(fragment), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if policy == "agent-defaults" {
					content += "\n[agent_defaults]\nmodel=\"gpt-6.1-sol\"\n"
				}
				if err := os.WriteFile(filepath.Join(src, "city.toml"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(src, "pack.toml"), []byte("[pack]\nname=\"authored\"\nschema=2\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				var code int
				if mode == "file" {
					code = cmdInitFromTOMLFileWithOptionsInternal(fsys.OSFS{}, filepath.Join(src, "city.toml"), city, "", &stdout, &stderr, true, false, true)
				} else {
					code = doInitFromDirWithOptionsFSInternal(fsys.OSFS{}, src, city, "", &stdout, &stderr, true, true, hostedDoltInitOptions{})
				}
				if code != 0 {
					t.Fatalf("valid authored scaffold failed: code=%d; %s", code, stderr.String())
				}
				cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
				if err != nil {
					t.Fatalf("authored configuration did not resolve: %v", err)
				}
				if cfg.Storage != nil {
					t.Fatalf("authored configuration acquired storage policy: %+v", cfg.Storage)
				}
				if policy == "include-native-off" && cfg.Beads.NativeTransport != "off" {
					t.Fatalf("lost native transport policy: %+v", cfg.Beads)
				}
				if policy == "include-ops" && len(cfg.Ops.Authoritative) != 0 {
					t.Fatalf("lost explicit operations policy: %+v", cfg.Ops)
				}
				if policy == "agent-defaults" && cfg.AgentDefaults.Model != "gpt-6.1-sol" {
					t.Fatalf("lost authored agent defaults: %+v", cfg.AgentDefaults)
				}
				if fragment != "" {
					if mode == "directory" {
						fragmentName = filepath.Join(city, "policy.toml")
					}
					got, err := os.ReadFile(fragmentName)
					if err != nil || string(got) != fragment {
						t.Fatalf("authored policy file changed: %q %v", got, err)
					}
				}
				if _, err := os.Lstat(initInfrastructurePath(city)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("authored policy acquired a native split intent: %v; %s", err, stderr.String())
				}
				if _, err := os.Lstat(filepath.Join(city, ".gc", "storage", "infra")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("authored policy provisioned infrastructure: %v", err)
				}
			})
		}
	}
}

func TestNativeInfrastructureComposedPolicyDoesNotCreateIntent(t *testing.T) {
	for _, policy := range []string{"include-native-off", "include-ops", "explicit-native-off", "explicit-ops", "pack-include"} {
		t.Run(policy, func(t *testing.T) {
			city, raw, _ := nativeInfrastructureFixture(t)
			if err := os.Remove(initInfrastructurePath(city)); err != nil {
				t.Fatal(err)
			}
			fragment := "[beads]\nnative_transport=\"off\"\n"
			if strings.HasSuffix(policy, "ops") {
				fragment = "[ops]\nauthoritative=[]\n"
			}
			switch {
			case strings.HasPrefix(policy, "include-"):
				if err := os.WriteFile(filepath.Join(city, "policy.toml"), []byte(fragment), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(city, "city.toml"), append([]byte("include=[\"policy.toml\"]\n"), raw...), 0o600); err != nil {
					t.Fatal(err)
				}
			case policy == "explicit-native-off":
				// The existing beads table remains the owner; do not write a duplicate.
				if err := os.WriteFile(filepath.Join(city, "city.toml"), append(raw, []byte("native_transport=\"off\"\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			case policy == "explicit-ops":
				if err := os.WriteFile(filepath.Join(city, "city.toml"), append(raw, []byte(fragment)...), 0o600); err != nil {
					t.Fatal(err)
				}
			case policy == "pack-include":
				// A pack can compose portable agent/provider policy; beads and
				// ops belong to city config and are intentionally invalid in pack.toml.
				pack := newInitPackConfig("native-fixture")
				factory := config.GascityCityWithProviders("native-fixture", "codex", []string{"codex"})
				pack.Imports, pack.Providers = factory.Imports, factory.Providers
				pack.Pack.Includes = []string{"custom-policy"}
				addBuiltinImportsToInitPack(&pack, "bd")
				if _, err := writeInitPackTomlOpts(fsys.OSFS{}, city, pack, false); err != nil {
					t.Fatal(err)
				}
				nested := filepath.Join(city, "custom-policy")
				if err := os.MkdirAll(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "pack.toml"), []byte("[pack]\nname=\"custom-policy\"\nschema=2\n[[agent]]\nname=\"policy-sentinel\"\nscope=\"city\"\nprovider=\"codex\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Prove the fixture is supported composed config, not an unknown field
			// that happens to make initialization refuse before reaching its subject.
			cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatalf("invalid policy fixture: %v", err)
			}
			if strings.HasSuffix(policy, "native-off") && cfg.Beads.NativeTransport != "off" {
				t.Fatalf("native policy did not compose: %+v", cfg.Beads)
			}
			if policy == "pack-include" {
				found := false
				for _, agent := range cfg.Agents {
					if agent.Name == "policy-sentinel" && agent.Provider == "codex" {
						found = true
					}
				}
				if !found {
					t.Fatalf("portable pack policy did not compose: %+v", cfg.Agents)
				}
			}
			beforeCity, err := os.ReadFile(filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			beforePack, err := os.ReadFile(filepath.Join(city, "pack.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := recordFreshInitInfrastructure(fsys.OSFS{}, city, true); err != nil {
				t.Fatal(err)
			}
			afterCity, err := os.ReadFile(filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			afterPack, err := os.ReadFile(filepath.Join(city, "pack.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeCity, afterCity) || !bytes.Equal(beforePack, afterPack) {
				t.Fatal("policy refusal rewrote authored config")
			}
			if _, err := os.Lstat(initInfrastructurePath(city)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("composed policy acquired a native intent: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(city, ".gc", "storage", "infra")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("composed policy provisioned infrastructure: %v", err)
			}
		})
	}
}
