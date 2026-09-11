package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func readScopeMetadataJSON(t *testing.T, scopeRoot string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	return string(data)
}

// D1: the topology lives in metadata.json. bd writes no dolt.mode into
// config.yaml and its own validator only accepts "server"|"embedded" there, so
// a canonical config that carries "proxied-server" is a second topology store
// with a value bd rejects. gc used to write it and then copy it back into
// metadata, which stamped proxied-server onto a legacy direct workspace whose
// metadata simply predated the field — and bd's next command would then open a
// proxy over the same data dir the managed server holds.
func TestCanonicalConfigNeverPersistsProxiedDoltMode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata string
	}{
		{name: "fresh scope", metadata: ""},
		{name: "already proxied", metadata: `{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := t.TempDir()
			if tt.metadata != "" {
				writeScopeBeadsMetadata(t, scope, tt.metadata)
			}
			state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "hq")
			if strings.TrimSpace(state.DoltMode) != "" {
				t.Fatalf("canonical config state carries dolt.mode %q for a proxied scope", state.DoltMode)
			}
		})
	}
}

// A Dolt scope whose metadata predates dolt_mode is a legacy direct server
// (init_intent's persistedTransport("")). Canonicalising it must say so
// instead of stamping the fresh proxied default over it.
func TestCanonicalConfigKeepsLegacyDoltMetadataOnServerMode(t *testing.T) {
	scope := t.TempDir()
	writeScopeBeadsMetadata(t, scope, `{"backend":"dolt","dolt_database":"hq"}`)
	state := desiredCityDoltConfigState(scope, config.DoltConfig{}, "hq")
	if got := strings.TrimSpace(state.DoltMode); got != "server" {
		t.Fatalf("legacy Dolt scope canonical dolt.mode = %q, want server", got)
	}
}

// config.yaml is a legacy compatibility input for direct/server only. It must
// never be the authority that promotes a scope to bd's proxied path: metadata
// is the single source, and a config.yaml saying otherwise is drift.
func TestConfigYAMLIsNotProxiedAuthority(t *testing.T) {
	city := t.TempDir()
	script := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"),
		[]byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, city, `{"backend":"dolt","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"),
		[]byte("issue_prefix: hq\ndolt.mode: proxied-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if scopeUsesProxiedDoltMode(city, city) {
		t.Error("config.yaml promoted a legacy direct scope to the proxied path")
	}
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, city, "hq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadata: %v", err)
	}
	mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, scopeMetadataJSONPath(city))
	if err != nil {
		t.Fatalf("ReadDoltMode: %v", err)
	}
	if ok && strings.EqualFold(strings.TrimSpace(mode), "proxied-server") {
		t.Fatalf("config.yaml stamped proxied-server into metadata: %s", readScopeMetadataJSON(t, city))
	}
}
