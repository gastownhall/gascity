package main

import (
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

func TestControlBdRunnerRefusesUnprovenScopeBeforeOverlayOrExecution(t *testing.T) {
	for _, kind := range []string{"city", "rig"} {
		t.Run(kind, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			city := t.TempDir()
			writeSchema2RigCity(t, city, "control-environment", "[workspace]\n", "")
			scope := city
			if kind == "rig" {
				scope = filepath.Join(city, "rigs", "removed")
			}
			writeScopeBeadsMetadata(t, scope, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"unknown"}`)
			writeCanonicalScopeConfig(t, scope, contract.ConfigState{IssuePrefix: "unknown"})
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("BEADS_ACTOR", "")
			before := snapshotEndpointRefusalArtifacts(t, city, scope)
			calls := 0
			previous := beadsExecCommandRunnerWithEnv
			beadsExecCommandRunnerWithEnv = func(map[string]string) beads.CommandRunner {
				calls++
				return func(string, string, ...string) ([]byte, error) {
					t.Fatal("unproven scope reached native bd execution")
					return nil, nil
				}
			}
			t.Cleanup(func() { beadsExecCommandRunnerWithEnv = previous })
			runner := controlBdCommandRunnerForCity(city)
			if kind == "rig" {
				runner = controlBdCommandRunnerForRig(city, &config.City{}, scope)
			}
			var failure error
			var panicValue any
			func() {
				defer func() { panicValue = recover() }()
				_, failure = runner(scope, "bd", "list", "--json")
			}()
			if panicValue != nil {
				t.Errorf("control bd scope refusal panicked before propagating its error: %v", panicValue)
			}
			if failure == nil || !strings.Contains(failure.Error(), "no proven project identity") {
				t.Errorf("control bd runner error=%v, want exact unproven binding refusal", failure)
			}
			if calls != 0 {
				t.Fatalf("native command factory calls=%d, want none", calls)
			}
			if after := snapshotEndpointRefusalArtifacts(t, city, scope); !reflect.DeepEqual(before, after) {
				t.Fatalf("scope error changed named native artifacts: before=%v after=%v", before, after)
			}
		})
	}
}

func TestControlBdRunnerPreservesValidLegacyEndpointAndControllerOverlay(t *testing.T) {
	for _, kind := range []string{"city", "rig"} {
		t.Run(kind, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			city := t.TempDir()
			writeSchema2RigCity(t, city, "control-environment", "[workspace]\n", "")
			scope := city
			if kind == "rig" {
				scope = filepath.Join(city, "rigs", "removed")
			}
			writeScopeBeadsMetadata(t, scope, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"known"}`)
			writeCanonicalScopeConfig(t, scope, contract.ConfigState{IssuePrefix: "known", DoltHost: "old-db.example.com", DoltPort: "3307"})
			assertCompleteLegacyEndpointAuthority(t, city, scope, "known")
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("BEADS_ACTOR", "")
			var environments []map[string]string
			previous := beadsExecCommandRunnerWithEnv
			beadsExecCommandRunnerWithEnv = func(env map[string]string) beads.CommandRunner {
				environments = append(environments, maps.Clone(env))
				return func(_ string, name string, _ ...string) ([]byte, error) {
					if name != "bd" {
						t.Fatalf("unexpected native command %q", name)
					}
					return []byte("[]"), nil
				}
			}
			t.Cleanup(func() { beadsExecCommandRunnerWithEnv = previous })
			runner := controlBdCommandRunnerForCity(city)
			if kind == "rig" {
				runner = controlBdCommandRunnerForRig(city, &config.City{}, scope)
			}
			if _, err := runner(scope, "bd", "list", "--json"); err != nil {
				t.Fatal(err)
			}
			if len(environments) != 1 {
				t.Fatalf("native command environments=%v, want one", environments)
			}
			env := environments[0]
			if env["BD_EXPORT_AUTO"] != "false" || env["BEADS_ACTOR"] != "controller" || env["BEADS_DIR"] != filepath.Join(scope, ".beads") {
				t.Fatalf("controller overlay changed or retargeted scope: %v", env)
			}
			if env["BEADS_DOLT_SERVER_HOST"] != "old-db.example.com" || env["BEADS_DOLT_SERVER_PORT"] != "3307" {
				t.Fatalf("legacy endpoint was not preserved: %v", env)
			}
		})
	}
}
