package pgauth_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// TestPostgresEventOmitsPassword is the redaction regression test required
// by ga-yih2 / ga-5c4x §6. It locks the contract that the
// pg.credential_resolved event payload, the full event envelope, and
// execenv.RedactText all keep the resolved Postgres password out of any
// observation surface an operator or auditor can read.
//
// Each sub-test runs as its own t.Run so a regression localizes to the
// surface that broke. The canary password literal is unique per run so
// grep assertions cannot be confused with unrelated test data.
func TestPostgresEventOmitsPassword(t *testing.T) {
	canary := "redaction-canary-" + randHex(t, 16)

	// Build a real PG-backed scope on disk and exercise the resolver so
	// the test asserts on data the production resolver actually
	// produced. envMap is nil so tier 4 (scope file) wins; tiers 1-3 are
	// skipped or empty.
	scopeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	envFile := filepath.Join(scopeRoot, ".beads", ".env")
	if err := os.WriteFile(envFile, []byte("BEADS_POSTGRES_PASSWORD="+canary+"\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	// Slice 2 reads BEADS_POSTGRES_PASSWORD from os.Getenv at tier 5;
	// scrub anything inherited so tier 4 wins deterministically.
	t.Setenv("BEADS_POSTGRES_PASSWORD", "")
	t.Setenv("GC_POSTGRES_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	endpoint := pgauth.Endpoint{Host: "127.0.0.1", Port: "5433", User: "bd"}
	resolved, err := pgauth.ResolveFromEnv(nil, scopeRoot, endpoint)
	if err != nil {
		t.Fatalf("ResolveFromEnv: %v", err)
	}
	if resolved.Password != canary {
		t.Fatalf("resolver returned password %q; want canary %q", resolved.Password, canary)
	}

	// Build the payload that slice 4's emit helper would construct.
	payload := pgauth.PostgresCredentialResolvedPayload{
		ScopeKind: "rig",
		ScopeName: "pwu",
		Source:    resolved.Source.String(),
		Host:      endpoint.Host,
		Port:      endpoint.Port,
		User:      endpoint.User,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	envelope := events.Event{
		Seq:     1,
		Type:    events.PostgresCredentialResolved,
		Ts:      time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC),
		Actor:   "controller",
		Subject: "rigs/pwu",
		Payload: payloadBytes,
	}
	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	t.Run("EventPayloadOmitsPassword", func(t *testing.T) {
		if strings.Contains(string(payloadBytes), canary) {
			t.Fatalf("payload JSON leaks password literal:\n  %s", string(payloadBytes))
		}
	})

	t.Run("EventEnvelopeOmitsPassword", func(t *testing.T) {
		if strings.Contains(string(envelopeBytes), canary) {
			t.Fatalf("event envelope JSON leaks password literal:\n  %s", string(envelopeBytes))
		}
	})

	t.Run("RedactTextScrubsPassword", func(t *testing.T) {
		scrubbed := execenv.RedactText("BEADS_POSTGRES_PASSWORD=" + canary)
		if strings.Contains(scrubbed, canary) {
			t.Fatalf("RedactText leaked password: %q", scrubbed)
		}
		if !strings.Contains(scrubbed, execenv.Redacted) {
			t.Fatalf("RedactText did not insert redaction marker: %q", scrubbed)
		}
	})

	t.Run("EventCarriesExpectedSource", func(t *testing.T) {
		if payload.Source != "scope_file" {
			t.Fatalf("payload.Source = %q; want scope_file (resolver tier 4)", payload.Source)
		}
		var decoded pgauth.PostgresCredentialResolvedPayload
		if err := json.Unmarshal(payloadBytes, &decoded); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if decoded.Source != "scope_file" {
			t.Fatalf("decoded payload.Source = %q; want scope_file", decoded.Source)
		}
	})

	t.Run("EventEmitsForResolvedSource", func(t *testing.T) {
		// Negative control: assert the test setup actually produced a
		// payload whose JSON carries the expected identifying fields,
		// so the redaction assertions above are not vacuously true.
		var decoded map[string]string
		if err := json.Unmarshal(payloadBytes, &decoded); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		for _, key := range []string{"scope_kind", "scope_name", "source", "host", "port", "user"} {
			if _, ok := decoded[key]; !ok {
				t.Fatalf("payload missing required key %q: %s", key, string(payloadBytes))
			}
		}
		if _, ok := decoded["password"]; ok {
			t.Fatalf("payload has unexpected password key: %s", string(payloadBytes))
		}
		if decoded["scope_kind"] != "rig" {
			t.Fatalf("scope_kind = %q; want rig", decoded["scope_kind"])
		}
		if decoded["scope_name"] != "pwu" {
			t.Fatalf("scope_name = %q; want pwu", decoded["scope_name"])
		}
	})
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(buf)
}
