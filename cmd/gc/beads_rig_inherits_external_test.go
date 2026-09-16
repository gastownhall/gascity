package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A rig added to a city bound to a Dolt server somebody else runs has to be
// initialized against that same server. It was not: the city's binding lives
// only in the metadata bd persisted — a provider-owned scope carries no gc
// endpoint keys to say so instead — and nothing read it, so the rig inherited
// "local" and bd gave it a store of its own on this machine. The operator's
// beads ended up split across two servers, which the topology matrix caught as
// a local sql-server appearing under a rig that is supposed to have none.

func writeBdOwnedDirectExternalCity(t *testing.T, cityPath, host, port string) {
	t.Helper()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"database":"dolt","backend":"dolt","dolt_mode":"server",` +
		`"dolt_server_host":"` + host + `","dolt_server_port":` + port + `,"dolt_database":"hosted"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	// bd's own template: a prefix and nothing gc wrote. gc does not
	// canonicalize a scope the provider owns.
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: gc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderOwnershipIntentFromPersistedCityReadsBdsExternalBinding(t *testing.T) {
	cityPath := t.TempDir()
	writeBdOwnedDirectExternalCity(t, cityPath, "db.example", "4406")

	intent, err := providerOwnershipIntentFromPersistedCity(cityPath)
	if err != nil {
		t.Fatalf("providerOwnershipIntentFromPersistedCity: %v", err)
	}
	if intent.Transport != "direct" || intent.Target != "external" {
		t.Fatalf("inherited intent = %+v, want direct/external — the city is bound to a server it does not run", intent)
	}
}

func TestProviderOwnershipIntentFromPersistedCityKeepsLocalWithoutABinding(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: gc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	intent, err := providerOwnershipIntentFromPersistedCity(cityPath)
	if err != nil {
		t.Fatalf("providerOwnershipIntentFromPersistedCity: %v", err)
	}
	if intent.Transport != "direct" || intent.Target != "local" {
		t.Fatalf("inherited intent = %+v, want direct/local for a city bd runs a server for here", intent)
	}
}

// Knowing the target is external is only half of it: the rig's init has to be
// handed the endpoint, or bd falls back to its own default and starts a local
// server anyway.
func TestInheritedProviderExternalEndpointEnvReadsBdsExternalBinding(t *testing.T) {
	cityPath := t.TempDir()
	writeBdOwnedDirectExternalCity(t, cityPath, "db.example", "4406")

	env, err := inheritedProviderExternalEndpointEnv(cityPath, providerScopeIntent{Transport: "direct", Target: "external"})
	if err != nil {
		t.Fatalf("inheritedProviderExternalEndpointEnv: %v", err)
	}
	if env["GC_DOLT_HOST"] != "db.example" || env["GC_DOLT_PORT"] != "4406" {
		t.Fatalf("inherited endpoint env = %+v, want the city's db.example:4406", env)
	}
}

func TestInheritedProviderExternalEndpointEnvRefusesACityWithNoBinding(t *testing.T) {
	cityPath := t.TempDir()
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue_prefix: gc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := inheritedProviderExternalEndpointEnv(cityPath, providerScopeIntent{Transport: "direct", Target: "external"}); err == nil {
		t.Fatal("inheritedProviderExternalEndpointEnv invented an endpoint for a city that has none")
	}
}
