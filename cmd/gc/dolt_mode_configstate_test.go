package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

// TestConfigStateConstructorsSetDoltModeServer verifies that the managed-scope
// ConfigState constructors set DoltMode:"server" so EnsureCanonicalConfig writes
// dolt.mode: server into .beads/config.yaml. Without it, `bd context` reports
// dolt_mode=embedded and the preflight dolt_mode_safe gate
// (internal/beads/contract/preflight_checker.go checkDoltModeSafe) cannot confirm
// server mode on the Darwin managed-city on-ramp.
//
// This re-lands the reviewed+approved fix from commit cd83f6da8 (ga-yqn5py Slice 2,
// review bead ga-94h3qv "pass"), which was lost during branch churn and never
// merged to main (PR #3722 carried a now-stale copy against pre-refactor structure).
func TestConfigStateConstructorsSetDoltModeServer(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rig")

	// Managed city (gc runs the Dolt sql-server; no external host/port).
	managedCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{}, "gc")
	if managedCity.DoltMode != "server" {
		t.Errorf("desiredCityDoltConfigState (managed city): DoltMode = %q, want %q", managedCity.DoltMode, "server")
	}

	// External city (explicit host/port endpoint).
	externalCity := desiredCityDoltConfigState(cityPath, config.DoltConfig{Host: "db.example.com", Port: 3306}, "gc")
	if externalCity.DoltMode != "server" {
		t.Errorf("desiredCityDoltConfigState (external city): DoltMode = %q, want %q", externalCity.DoltMode, "server")
	}

	// Explicit rig (own dolt host/port override).
	explicitRig := desiredRigDoltConfigState(cityPath, config.Rig{
		Name: "rig", Path: rigPath, Prefix: "rig", DoltHost: "db.example.com", DoltPort: "3306",
	}, managedCity)
	if explicitRig.DoltMode != "server" {
		t.Errorf("desiredRigDoltConfigState (explicit rig): DoltMode = %q, want %q", explicitRig.DoltMode, "server")
	}

	// Inherited rig propagates the city's DoltMode.
	inheritedRig := inheritedRigDoltConfigState(rigPath, "rig", managedCity)
	if inheritedRig.DoltMode != managedCity.DoltMode {
		t.Errorf("inheritedRigDoltConfigState: DoltMode = %q, want %q (inherited from city)", inheritedRig.DoltMode, managedCity.DoltMode)
	}

	// Requested rig endpoint (self path — gc manages this rig's Dolt).
	selfEndpoint := requestedRigEndpointState(
		config.Rig{Name: "rig", Path: rigPath, Prefix: "rig"},
		contract.ConfigState{}, managedCity,
		rigEndpointOptions{Self: true, Port: "3306"},
	)
	if selfEndpoint.DoltMode != "server" {
		t.Errorf("requestedRigEndpointState (self): DoltMode = %q, want %q", selfEndpoint.DoltMode, "server")
	}

	// Requested rig endpoint (explicit host/port path).
	externalEndpoint := requestedRigEndpointState(
		config.Rig{Name: "rig", Path: rigPath, Prefix: "rig"},
		contract.ConfigState{}, managedCity,
		rigEndpointOptions{Host: "db.example.com", Port: "3306", User: "bd"},
	)
	if externalEndpoint.DoltMode != "server" {
		t.Errorf("requestedRigEndpointState (external): DoltMode = %q, want %q", externalEndpoint.DoltMode, "server")
	}
}
