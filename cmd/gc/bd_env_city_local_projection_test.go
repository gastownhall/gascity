package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

func nativeDirectCityProjectionFixture(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	city, _, preserved := nativeDirectAdoptFixture(t)
	for name, contents := range preserved {
		path := filepath.Join(city, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	local, err := scopeBindingIsProviderOwnedDirectLocal(city, city)
	if err != nil || !local {
		t.Fatalf("captured-shape physical native city local=%v err=%v", local, err)
	}
	return city, preserved
}

func TestBdRuntimeCityNativeLocalKeepsPhysicalIdentityWithoutCityFallback(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	city, before := nativeDirectCityProjectionFixture(t)
	t.Setenv("GC_DOLT_HOST", "foreign-city.example")
	t.Setenv("GC_DOLT_PORT", "43361")
	t.Setenv("GC_DOLT_DATA_DIR", "/foreign/city")
	for _, records := range []bool{true, false} {
		if !records {
			for _, name := range []string{".beads/dolt-server.pid", ".beads/dolt-server.port"} {
				if err := os.Remove(filepath.Join(city, name)); err != nil {
					t.Fatal(err)
				}
				delete(before, name)
			}
		}
		for _, allowRecovery := range []bool{true, false} {
			env, err := bdRuntimeEnvWithErrorRecovery(city, allowRecovery)
			if err != nil {
				t.Fatalf("records=%v recovery=%v: %v", records, allowRecovery, err)
			}
			if env["BEADS_DIR"] != filepath.Join(city, ".beads") || env["GC_BEADS_BACKEND"] != "dolt" || env["BEADS_BACKEND"] != "dolt" {
				t.Fatalf("native city binding changed: %v", env)
			}
			for _, key := range append(append([]string{}, projectedDoltEnvKeys...), "GC_DOLT_DATA_DIR", "BEADS_DOLT_AUTO_START") {
				if env[key] != "" {
					t.Fatalf("records=%v recovery=%v: foreign city %s=%q projected", records, allowRecovery, key, env[key])
				}
			}
			assertNativeDirectPreserved(t, city, before)
		}
	}
}

func TestBdRuntimeCityProjectionRefusesUnknownNativeIdentityWithoutRecovery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	city := t.TempDir()
	writeSchema2RigCity(t, city, "control-city-error", "[workspace]\n[beads]\nprovider=\"bd\"\n", "")
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"unknown"}`)
	writeCanonicalScopeConfig(t, city, contract.ConfigState{IssuePrefix: "unknown"})
	if !cityUsesBdStoreContract(city) {
		t.Fatal("fixture must select the actual bd contract")
	}
	before := snapshotEndpointRefusalArtifacts(t, city, city)
	for _, allowRecovery := range []bool{true, false} {
		if _, err := bdRuntimeEnvWithErrorRecovery(city, allowRecovery); err == nil {
			t.Fatalf("recovery=%v: unknown native city became successful environment", allowRecovery)
		}
		if after := snapshotEndpointRefusalArtifacts(t, city, city); !reflect.DeepEqual(before, after) {
			t.Fatal("unknown native city metadata/config changed")
		}
	}
}

func TestBdRuntimeCityProjectionRetainsNonBdAndDoltliteBoundaries(t *testing.T) {
	for _, provider := range []string{"file", "exec:/custom/non-bd", "bd"} {
		t.Run(provider, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			city := t.TempDir()
			writeSchema2RigCity(t, city, "other-city-provider", "[workspace]\n", "")
			t.Setenv("GC_BEADS", provider)
			metadata := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"unknown"}`
			if provider == "bd" {
				metadata = `{"backend":"doltlite"}`
			}
			writeScopeBeadsMetadata(t, city, metadata)
			before := snapshotEndpointRefusalArtifacts(t, city, city)
			env, err := bdRuntimeEnvWithErrorNoRecovery(city)
			if err != nil || env["BEADS_DIR"] != filepath.Join(city, ".beads") {
				t.Fatalf("provider=%s env=%v err=%v", provider, env, err)
			}
			if provider == "bd" && env["BEADS_BACKEND"] != "doltlite" {
				t.Fatalf("doltlite projection changed: %v", env)
			}
			if after := snapshotEndpointRefusalArtifacts(t, city, city); !reflect.DeepEqual(before, after) {
				t.Fatal("nonlocal branch changed native artifacts")
			}
		})
	}
}

func TestBdRuntimeCityNativeLocalPreservesCanceledContext(t *testing.T) {
	clearGCEnv(t)
	city, before := nativeDirectCityProjectionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, city, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled native city context error=%v", err)
	}
	assertNativeDirectPreserved(t, city, before)
}
