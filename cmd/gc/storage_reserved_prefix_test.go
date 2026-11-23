package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// R10 (split-default read audit): a split city whose HQ or rig prefix is a
// reserved class prefix shares the binding's id namespace. gas-city-gateway
// derives gcg, the graph binding's own prefix. Boot only warned; it must refuse,
// before any binding is opened or created.

func TestStorageBootGateRefusesASplitCityWithAReservedPrefix(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cfg := infraSplitConfig(bindingRoot)
	cfg.Workspace.Name = "gas-city-gateway"
	cityPath := t.TempDir()
	refuseInfraMigrationSource(t)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if routes != nil {
		_ = routes.close()
	}
	if err == nil {
		t.Fatal("a split city whose HQ prefix derives the reserved gcg booted")
	}
	for _, want := range []string{"gcg", "reserved", "workspace_prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if present, _ := infraPathExists(bindingRoot); present {
		t.Error("the refused boot created the binding root; the refusal must come before any binding is touched")
	}
}

func TestStorageBootGateRefusesASplitCityWithAReservedRigPrefix(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	cfg.Workspace.Name = "my-city"
	cfg.Rigs = []config.Rig{{Name: "gas-city-sandbox", Path: t.TempDir()}}
	refuseInfraMigrationSource(t)

	var stderr bytes.Buffer
	routes, err := storageBootGate(t.TempDir(), cfg, "gc start", nil, &stderr)
	if routes != nil {
		_ = routes.close()
	}
	if err == nil || !strings.Contains(err.Error(), `rig "gas-city-sandbox"`) || !strings.Contains(err.Error(), "gcs") {
		t.Fatalf("boot = %v, want a refusal naming rig gas-city-sandbox and gcs", err)
	}
}

// A single-store city keeps today's advisory: the gate's bypass is untouched.
func TestStorageBootGateLeavesASingleStoreCityWithAReservedPrefixAlone(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "gas-city-gateway"}}
	var stderr bytes.Buffer
	routes, err := storageBootGate(t.TempDir(), cfg, "gc start", nil, &stderr)
	if err != nil || routes != nil {
		t.Errorf("a single-store city with a reserved-looking prefix: routes=%v err=%v, want the bypass", routes, err)
	}
}

func TestStorageMigrateRefusesToCreateASplitWithAReservedPrefix(t *testing.T) {
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cfg := infraSplitConfig(bindingRoot)
	cfg.Workspace.Name = "gas-city-ops"
	request := storageTestRequest(t, cfg)
	refuseInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)

	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code == 0 {
		t.Fatalf("gc storage migrate created a split for a city whose prefix derives gco; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gco") || !strings.Contains(stderr.String(), "reserved") {
		t.Errorf("stderr = %q, want the reserved-prefix refusal", stderr.String())
	}
	if present, _ := infraPathExists(bindingRoot); present {
		t.Error("the refused migration created the binding root")
	}
}

// A reload that would introduce a reserved prefix on a split city is refused
// and the booted config stays in force, the same way a changed [storage] is.
func TestCityRuntimeReloadRefusesAReservedPrefixOnASplitCity(t *testing.T) {
	cr, tomlPath, stderr := bootSplitCityForReload(t)
	binding := cr.cfg.Storage.Bindings["infra"].Path
	bootedRigs := len(cr.cfg.Rigs)

	rigDir := t.TempDir()
	writeSplitCityConfig(t, tomlPath, binding, "\n[[rigs]]\nname = \"gas-city-gateway\"\npath = \""+rigDir+"\"\n")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q; a reserved rig prefix was accepted on a split city", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "gcg") || !strings.Contains(reply.Error, "keeping old config") || !strings.Contains(reply.Error, "hook process") {
		t.Errorf("the refusal = %q, want the gcg collision and the kept config", reply.Error)
	}
	if !strings.Contains(stderr.String(), "gcg") {
		t.Errorf("the refusal was not reported to the operator: %q", stderr.String())
	}
	if len(cr.cfg.Rigs) != bootedRigs {
		t.Errorf("the refused reload applied its rigs: %d -> %d", bootedRigs, len(cr.cfg.Rigs))
	}
}
