package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// A rig that already carries a persisted dolt_mode keeps it — that is what the
// embedded and legacy shapes need — but keeping the mode is not a reason to
// drop the endpoint it inherits.
//
// Under a city bound to a Dolt server somebody else runs, an inherited rig
// config with no dolt.host and no dolt.port is invalid by the canonical config
// contract's own rule. `gc rig add` wrote exactly that, and from then on the
// whole city refused to start: "canonical inherited rig config requires both
// dolt.host and dolt.port", with `gc doctor` reporting the rig's store, its
// server and the city's topology as failures.
func writeRigMetadataWithMode(t *testing.T, rigPath, mode string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigPath, ".beads", "metadata.json"),
		contract.MetadataState{Database: "dolt", Backend: "dolt", DoltMode: mode, DoltDatabase: "fe"}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
}

func TestDesiredRigDoltConfigStateInheritsTheExternalEndpointItKeepsAModeFor(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	writeRigMetadataWithMode(t, rigPath, "server")

	cityState := desiredCityDoltConfigState(cityPath, config.DoltConfig{Host: "db.example.com", Port: 4406}, "gc")
	if cityState.EndpointOrigin != contract.EndpointOriginCityCanonical {
		t.Fatalf("fixture city origin = %q, want city_canonical", cityState.EndpointOrigin)
	}

	rigState := desiredRigDoltConfigState(cityPath, config.Rig{Name: "frontend", Path: rigPath, Prefix: "fe"}, cityState)

	if rigState.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Errorf("rig origin = %q, want inherited_city", rigState.EndpointOrigin)
	}
	if rigState.DoltMode != "server" {
		t.Errorf("rig dolt_mode = %q, want the persisted server mode", rigState.DoltMode)
	}
	if rigState.DoltHost != "db.example.com" || rigState.DoltPort != "4406" {
		t.Fatalf("rig endpoint = %s:%s, want the city's db.example.com:4406 — an inherited rig under a "+
			"canonical city needs both, and gc start refuses the city without them",
			rigState.DoltHost, rigState.DoltPort)
	}
}

// The same rig under a managed city inherits no endpoint, because there is none
// to inherit: the city's own runtime state is the record of where its server is.
func TestDesiredRigDoltConfigStateKeepsAModeWithoutInventingAnEndpoint(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	writeRigMetadataWithMode(t, rigPath, "embedded")

	cityState := desiredCityDoltConfigState(cityPath, config.DoltConfig{}, "gc")
	rigState := desiredRigDoltConfigState(cityPath, config.Rig{Name: "frontend", Path: rigPath, Prefix: "fe"}, cityState)

	if rigState.DoltMode != "embedded" {
		t.Errorf("rig dolt_mode = %q, want the persisted embedded mode preserved", rigState.DoltMode)
	}
	if rigState.DoltHost != "" || rigState.DoltPort != "" {
		t.Errorf("rig endpoint = %s:%s, want none under a managed city", rigState.DoltHost, rigState.DoltPort)
	}
	if rigState.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Errorf("rig origin = %q, want inherited_city", rigState.EndpointOrigin)
	}
}
