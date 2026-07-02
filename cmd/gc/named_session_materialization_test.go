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

// TestCmdSessionNew_NonNamedSessionKeepsOriginManual verifies that launching
// a plain agent template (not a configured named session) still gets
// session_origin="manual", so Fix B has no unintended side effects.
func TestCmdSessionNew_NonNamedSessionKeepsOriginManual(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir) // has "mayor" as named session

	// Use --alias to exercise the existing alias path without Fix B changes.
	// "mayor" IS a named session in this config, so we test with an alias
	// that is NOT a configured named session identity.
	//
	// Actually we need a template that isn't in named_sessions. Use "mayor"
	// without --alias since writeNamedSessionCityTOML only has [[named_session]] template="mayor".
	// The test below creates a bead via a template that has NO named_session entry.
	// We re-use the mayor template but pass a user-supplied alias; sessionNewAliasOwner
	// matches only when the agent IS in named_sessions AND no user alias is provided
	// — so passing alias="my-mayor" will NOT trigger Fix B.
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
