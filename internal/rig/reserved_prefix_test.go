package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func splitCityCfg() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Storage: &config.StorageConfig{
			Classes: config.StorageClasses{
				Work: config.StorageWorkBinding, Graph: "infra", Sessions: "infra",
				Messaging: "infra", Orders: "infra", Nudges: "infra",
			},
			Bindings: map[string]config.StorageBindingConfig{"infra": {Provider: config.StorageProviderSQLiteBeads}},
		},
	}
}

// Split-default R10, at the point a reserved prefix is INTRODUCED: rig
// provisioning (gc rig add and the API rig create) refuses a rig whose
// derived or explicit prefix falls in a reserved namespace on a split city,
// before anything is written.
func TestProvisionRefusesAReservedRigPrefixOnASplitCity(t *testing.T) {
	for _, tc := range []struct{ name, prefix, reserved string }{
		{"gas-city-gateway", "", "gcg"},
		{"alpha", "gcg-alpha", "gcg"},
		{"beta", "gco", "gco"},
	} {
		t.Run(tc.name+"/"+tc.prefix, func(t *testing.T) {
			deps := stubDeps(t.TempDir())
			deps.Cfg = splitCityCfg()
			rigPath := filepath.Join(t.TempDir(), tc.name)
			_, _, err := Provision(deps, ProvisionRequest{Name: tc.name, Path: rigPath, Prefix: tc.prefix})
			if err == nil || !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), tc.reserved) {
				t.Fatalf("Provision err = %v, want the reserved-prefix refusal naming %q", err, tc.reserved)
			}
			if _, statErr := os.Stat(rigPath); statErr == nil {
				t.Error("the refused add created the rig directory")
			}
		})
	}
}

func TestProvisionAllowsAReservedRigPrefixOnASingleStoreCity(t *testing.T) {
	deps := stubDeps(t.TempDir())
	if _, _, err := Provision(deps, ProvisionRequest{Name: "gas-city-gateway", Path: filepath.Join(t.TempDir(), "gas-city-gateway")}); err != nil && strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a single-store city refused a reserved-looking rig prefix: %v", err)
	}
}
