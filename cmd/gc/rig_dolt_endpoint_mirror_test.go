package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

// writeInheritedRigConfig writes a pre-churn rig .beads/config.yaml pinned to
// gc.endpoint_origin: inherited_city with an endpoint that mirrors the city,
// returning the rig root directory.
func writeInheritedRigConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "issue_prefix: gp\n" +
		"gc.endpoint_origin: inherited_city\n" +
		"gc.endpoint_status: verified\n" +
		"dolt.host: 192.168.4.230\n" +
		"dolt.port: 3308\n" +
		"dolt.user: root\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDesiredRigDoltConfigStateKeepsInheritedWhenTargetMirrorsCity guards the
// regression where a rig carrying a dolt target equal to the city canonical
// endpoint was canonicalized to `explicit` on every city start, clobbering the
// intended `inherited_city` origin and dropping the inherited dolt.user.
func TestDesiredRigDoltConfigStateKeepsInheritedWhenTargetMirrorsCity(t *testing.T) {
	cityState := contract.ConfigState{
		IssuePrefix:    "gws",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "192.168.4.230",
		DoltPort:       "3308",
		DoltUser:       "root",
	}

	t.Run("matching target stays inherited and preserves user", func(t *testing.T) {
		dir := writeInheritedRigConfig(t)
		rig := config.Rig{Name: "gascity-pack", Path: dir, Prefix: "gp", DoltHost: "192.168.4.230", DoltPort: "3308"}

		got := desiredRigDoltConfigState(dir, rig, cityState)

		if got.EndpointOrigin != contract.EndpointOriginInheritedCity {
			t.Fatalf("EndpointOrigin = %q, want %q (rig mirrors city, must not be promoted to explicit)",
				got.EndpointOrigin, contract.EndpointOriginInheritedCity)
		}
		if got.DoltUser != "root" {
			t.Fatalf("DoltUser = %q, want %q (inherited dolt.user must be preserved)", got.DoltUser, "root")
		}
		if got.DoltHost != "192.168.4.230" || got.DoltPort != "3308" {
			t.Fatalf("endpoint = %s:%s, want 192.168.4.230:3308", got.DoltHost, got.DoltPort)
		}
	})

	t.Run("genuinely different target stays explicit", func(t *testing.T) {
		dir := writeInheritedRigConfig(t)
		rig := config.Rig{Name: "gascity-pack", Path: dir, Prefix: "gp", DoltHost: "10.0.0.9", DoltPort: "3309"}

		got := desiredRigDoltConfigState(dir, rig, cityState)

		if got.EndpointOrigin != contract.EndpointOriginExplicit {
			t.Fatalf("EndpointOrigin = %q, want %q (rig target differs from city, must stay explicit)",
				got.EndpointOrigin, contract.EndpointOriginExplicit)
		}
		if got.DoltHost != "10.0.0.9" || got.DoltPort != "3309" {
			t.Fatalf("endpoint = %s:%s, want 10.0.0.9:3309", got.DoltHost, got.DoltPort)
		}
	})
}
