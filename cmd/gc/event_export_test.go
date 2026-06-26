package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// TestResolveExportCredentials_EmptyTokenFileErrors proves a configured but
// empty (or whitespace-only) token_file fails closed: the provider returns an
// error so the cursor holds and the empty credential surfaces, instead of
// silently downgrading to an unauthenticated POST.
func TestResolveExportCredentials_EmptyTokenFileErrors(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	for _, content := range []string{"", "   \n\t  "} {
		if err := os.WriteFile(tokenPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		ec := supervisor.ExportConfig{TokenFile: tokenPath, ActorSalt: "test-salt"}
		provider, _ := resolveExportCredentials(ec, dir, io.Discard)
		if provider == nil {
			t.Fatalf("configured token_file must yield a provider (content %q)", content)
		}
		tok, err := provider()
		if err == nil {
			t.Fatalf("empty token_file must error, got token %q (content %q)", tok, content)
		}
		if tok != "" {
			t.Fatalf("empty token_file must return an empty token with the error, got %q", tok)
		}
	}
}

// TestResolveExportCredentials_ValidTokenFile proves a non-empty token_file is
// read and trimmed.
func TestResolveExportCredentials_ValidTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("  s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ec := supervisor.ExportConfig{TokenFile: tokenPath, ActorSalt: "test-salt"}
	provider, _ := resolveExportCredentials(ec, dir, io.Discard)
	if provider == nil {
		t.Fatal("configured token_file must yield a provider")
	}
	tok, err := provider()
	if err != nil {
		t.Fatalf("valid token_file must not error: %v", err)
	}
	if tok != "s3cr3t" {
		t.Fatalf("token = %q, want trimmed %q", tok, "s3cr3t")
	}
}

// TestResolveExportCredentials_NoSourceIsAnonymous proves the deliberately-
// anonymous opt-out (no token and no token_file) still yields a nil provider,
// distinct from the now-error empty-token-file case.
func TestResolveExportCredentials_NoSourceIsAnonymous(t *testing.T) {
	dir := t.TempDir()
	ec := supervisor.ExportConfig{ActorSalt: "test-salt"}
	provider, salt := resolveExportCredentials(ec, dir, io.Discard)
	if provider != nil {
		t.Fatal("no token source must yield a nil provider (the anonymous opt-out)")
	}
	if string(salt) != "test-salt" {
		t.Fatalf("salt = %q, want inline ActorSalt", salt)
	}
}

// TestResolveExportCredentials_InlineToken proves an inline token is returned
// verbatim.
func TestResolveExportCredentials_InlineToken(t *testing.T) {
	dir := t.TempDir()
	ec := supervisor.ExportConfig{Token: "inline-tok", ActorSalt: "test-salt"}
	provider, _ := resolveExportCredentials(ec, dir, io.Discard)
	if provider == nil {
		t.Fatal("inline token must yield a provider")
	}
	tok, err := provider()
	if err != nil || tok != "inline-tok" {
		t.Fatalf("inline token provider = (%q, %v), want (inline-tok, nil)", tok, err)
	}
}
