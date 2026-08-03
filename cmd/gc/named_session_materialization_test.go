package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Tests for Fix B: configured named-session identities launched via
// "gc session new" get session_origin="named" and the canonical named-session
// metadata, not session_origin="manual" with no configured-identity markers.

func writeSimpleNamedSessionCityTOML(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	// pack.toml: a simple single-instance named session "kenneth"
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(`[pack]
name = "test-city"
schema = 2

[[named_session]]
template = "kenneth"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`[workspace]

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeBuiltinImportsFixture(t, dir, "core")
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte(`workspace_name = "test-city"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(.gc/site.toml): %v", err)
	}
	writeCatalogFile(t, dir, "agents/kenneth/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\n")
}

// TestCmdSessionNew_NamedSessionGetsOriginNamed verifies that launching a
// configured named session without an explicit --alias sets session_origin to
// "named" and stamps the canonical named-session metadata on the bead.
func TestCmdSessionNew_NamedSessionGetsOriginNamed(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeSimpleNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"kenneth"}, "", "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)

	if got := b.Metadata["session_origin"]; got != "named" {
		t.Errorf("session_origin = %q, want %q", got, "named")
	}
	if got := b.Metadata[session.NamedSessionMetadataKey]; got != "true" {
		t.Errorf("%s = %q, want %q", session.NamedSessionMetadataKey, got, "true")
	}
	if got := b.Metadata[session.NamedSessionIdentityMetadata]; got != "kenneth" {
		t.Errorf("%s = %q, want %q", session.NamedSessionIdentityMetadata, got, "kenneth")
	}
	if got := b.Metadata["session_name"]; got != "kenneth" {
		t.Errorf("session_name = %q, want %q", got, "kenneth")
	}
	if got := b.Metadata["alias"]; got != "kenneth" {
		t.Errorf("alias = %q, want %q", got, "kenneth")
	}
}

// TestCmdSessionNew_NonNamedSessionKeepsOriginManual verifies that a
// user-supplied --alias keeps session_origin="manual". The configured-identity
// stamp is gated on the caller supplying no alias, so an explicit --alias must
// leave the session on the pre-existing manual path even when the template does
// have a [[named_session]] entry.
func TestCmdSessionNew_NonNamedSessionKeepsOriginManual(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	// The fixture declares [[named_session]] template = "mayor", so
	// sessionNewAliasOwner does resolve a configured owner for this template.
	// That makes it the sharper case: the stamp must still not apply, because
	// it is additionally gated on the caller passing no alias. Launching
	// "mayor" with --alias my-mayor therefore exercises the pre-existing
	// user-alias path unchanged.
	writeNamedSessionCityTOML(t, cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionNew([]string{"mayor"}, "my-mayor", "", "", true, false, 0, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionNew = %d, want 0; stderr=%s", code, stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	// User-supplied alias: Fix B should NOT inject named session metadata
	// (the alias path is for user-chosen identities, not configured ones).
	if got := b.Metadata["session_origin"]; got != "manual" {
		t.Errorf("session_origin = %q, want %q (user alias should stay manual)", got, "manual")
	}
}
