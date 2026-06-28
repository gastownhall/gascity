package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/transcriptmeta"
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

// TestStartEventExport_CorruptCursorLeavesSidecarDisabled is the regression for
// the release-safety finding: startEventExport must not arm the process-wide
// transcript sidecar gate when it refuses to start. A corrupt or unreadable
// durable cursor makes the exporter fail closed; were the sidecar gate armed
// first, later successful turns would write .gcmeta correlation files with no
// event stream to join.
func TestStartEventExport_CorruptCursorLeavesSidecarDisabled(t *testing.T) {
	transcriptmeta.SetEnabled(false)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	home := t.TempDir()
	// Seed a corrupt cursor so eventexport.LoadCursors errors and
	// startEventExport returns before launching the exporter.
	cursorPath := filepath.Join(home, "events-export-cursor.json")
	if err := os.WriteFile(cursorPath, []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ec := supervisor.ExportConfig{Endpoint: "https://example.invalid/ingest", ActorSalt: "test-actor-salt-ok"}
	providers := func() map[string]events.Provider { return map[string]events.Provider{} }

	startEventExport(ctx, ec, providers, home, io.Discard)

	if transcriptmeta.Enabled() {
		t.Fatal("sidecar gate must stay disabled when the exporter refuses to start on a corrupt cursor")
	}
}

// TestStartEventExport_SuccessfulStartupArmsSidecar proves the gate is still
// armed on the happy path (no cursor file -> first-run start), so deferring the
// arm past the fail-closed cursor load did not silently disable the feature.
func TestStartEventExport_SuccessfulStartupArmsSidecar(t *testing.T) {
	transcriptmeta.SetEnabled(false)
	t.Cleanup(func() { transcriptmeta.SetEnabled(false) })

	home := t.TempDir() // no cursor file: LoadCursors returns an empty map, no error

	// A pre-canceled context lets the exporter goroutines exit immediately; the
	// gate is set synchronously before they launch, so its state is unaffected.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ec := supervisor.ExportConfig{Endpoint: "https://example.invalid/ingest", ActorSalt: "test-actor-salt-ok"}
	providers := func() map[string]events.Provider { return map[string]events.Provider{} }

	wg := startEventExport(ctx, ec, providers, home, io.Discard)
	// The exporter and cursor-persist goroutines write under home (e.g. the
	// shutdown cursor flush in persistExportCursors). Drain them before the test
	// returns so t.TempDir cleanup does not race those writes ("directory not
	// empty"). On this happy path the goroutines were launched, so wg is non-nil.
	if wg == nil {
		t.Fatal("happy-path startEventExport must return a drain handle")
	}
	wg.Wait()

	if !transcriptmeta.Enabled() {
		t.Fatal("sidecar gate must be armed once the exporter clears its fail-closed startup")
	}
}
