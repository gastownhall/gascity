package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The identity_match probe must dial with the same credential the native
// store open resolves for the scope, not only the ambient GC_DOLT_PASSWORD.
// Otherwise every credential-less in-process open of an external rig store
// emits one rejected authentication at the Dolt server (gascity#5965).
func TestPreflightProbePasswordResolvesScopeLocalCredential(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent"))

	cityPath := t.TempDir()
	scope := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=scope-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := contract.DoltConnectionTarget{
		Host:           "100.64.0.9",
		Port:           "3307",
		Database:       "rig",
		User:           "bd",
		EndpointOrigin: contract.EndpointOriginExplicit,
		External:       true,
	}

	if got := preflightProbePassword(cityPath, scope, target); got != "scope-secret" {
		t.Fatalf("preflightProbePassword() = %q, want scope-local .beads/.env password", got)
	}

	// The ambient variable still wins when present (unchanged operator override).
	t.Setenv("GC_DOLT_PASSWORD", "ambient-secret")
	if got := preflightProbePassword(cityPath, scope, target); got != "ambient-secret" {
		t.Fatalf("preflightProbePassword() = %q, want ambient GC_DOLT_PASSWORD", got)
	}
}

func TestPreflightProbePasswordReadsCredentialsFileForTarget(t *testing.T) {
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")

	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentials, []byte("[100.64.0.9:3307]\npassword = file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentials)

	cityPath := t.TempDir()
	target := contract.DoltConnectionTarget{
		Host:           "100.64.0.9",
		Port:           "3307",
		Database:       "hq",
		User:           "bd",
		EndpointOrigin: contract.EndpointOriginExplicit,
		External:       true,
	}
	if got := preflightProbePassword(cityPath, cityPath, target); got != "file-secret" {
		t.Fatalf("preflightProbePassword() = %q, want credentials-file password for host:port", got)
	}
}
