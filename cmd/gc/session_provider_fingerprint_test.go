package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestWithSessionProviderFingerprintRequiresOptInMetadata(t *testing.T) {
	cfg := runtime.Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "1"}}
	tp := TemplateParams{SessionProvider: "tmux"}

	got := withSessionProviderFingerprint(cfg, beads.Bead{Metadata: map[string]string{}}, tp)
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("without metadata got %#v, want unchanged %#v", got, cfg)
	}
}

func TestWithSessionProviderFingerprintAddsExecProviderForLegacyBeads(t *testing.T) {
	cfg := runtime.Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "1"}}
	tp := TemplateParams{SessionProvider: "exec:/tmp/remote-worker", SessionProviderProfile: remoteWorkerProfile}

	got := withSessionProviderFingerprint(cfg, beads.Bead{Metadata: map[string]string{}}, tp)
	if got.FingerprintExtra[sessionpkg.MetadataSessionProvider] != tp.SessionProvider {
		t.Fatalf("session provider fingerprint = %q, want %q", got.FingerprintExtra[sessionpkg.MetadataSessionProvider], tp.SessionProvider)
	}
	if got.FingerprintExtra[sessionpkg.MetadataSessionProviderProfile] != tp.SessionProviderProfile {
		t.Fatalf("session provider profile fingerprint = %q, want %q", got.FingerprintExtra[sessionpkg.MetadataSessionProviderProfile], tp.SessionProviderProfile)
	}
}

func TestWithSessionProviderFingerprintAddsProviderForOwnedBeads(t *testing.T) {
	cfg := runtime.Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "1"}}
	tp := TemplateParams{SessionProvider: "exec:/tmp/remote-worker", SessionProviderProfile: remoteWorkerProfile}
	bead := beads.Bead{Metadata: map[string]string{sessionpkg.MetadataSessionProvider: "tmux"}}

	got := withSessionProviderFingerprint(cfg, bead, tp)
	if got.FingerprintExtra["pool.max"] != "1" {
		t.Fatalf("pool.max = %q, want 1", got.FingerprintExtra["pool.max"])
	}
	if got.FingerprintExtra[sessionpkg.MetadataSessionProvider] != tp.SessionProvider {
		t.Fatalf("session provider fingerprint = %q, want %q", got.FingerprintExtra[sessionpkg.MetadataSessionProvider], tp.SessionProvider)
	}
	if got.FingerprintExtra[sessionpkg.MetadataSessionProviderProfile] != tp.SessionProviderProfile {
		t.Fatalf("session provider profile fingerprint = %q, want %q", got.FingerprintExtra[sessionpkg.MetadataSessionProviderProfile], tp.SessionProviderProfile)
	}
	if cfg.FingerprintExtra[sessionpkg.MetadataSessionProvider] != "" {
		t.Fatal("withSessionProviderFingerprint mutated input FingerprintExtra")
	}
}
