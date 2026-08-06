package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdStoreBridgeBead is the PRODUCER that feeds the exec store's beadWire
// consumer: `gc bd-store-bridge` reads through BdStore (which sees real bd
// JSON) and re-marshals into this narrower envelope, so a field the envelope
// omits is gone before the consumer decodes anything. That makes a consumer
// test fed hand-written JSON worthless as evidence for this path — the JSON
// already contains the keys the producer never emits, so it passes either way.
//
// The two tests below close that gap from both ends: the first pins the wire
// KEYS the mapping emits, the second drives the real `get` op end to end with a
// bd that emits the plain columns and asserts they survive to stdout.

// TestBridgeBeadCarriesPlainColumns pins the mapping itself.
func TestBridgeBeadCarriesPlainColumns(t *testing.T) {
	in := beads.Bead{
		ID: "bd-1", Title: "Gate: gh:pr 4912", Status: "open", Type: "gate",
		AwaitType: "gh:pr",
		AwaitID:   "4912",
		CreatedBy: "seeder",
		Owner:     "owner@example.com",
		Notes:     "waiting on a PR",
	}
	got := bridgeBead(in)
	for _, tc := range []struct{ field, got, want string }{
		{"AwaitType", got.AwaitType, in.AwaitType},
		{"AwaitID", got.AwaitID, in.AwaitID},
		{"CreatedBy", got.CreatedBy, in.CreatedBy},
		{"Owner", got.Owner, in.Owner},
		{"Notes", got.Notes, in.Notes},
	} {
		if tc.got != tc.want {
			t.Errorf("bridgeBead dropped %s: got %q, want %q", tc.field, tc.got, tc.want)
		}
	}

	// Assert the emitted KEYS, not just the Go fields: the consumer on the far
	// side of stdout matches by json tag, so a tag that differs from bd's key
	// round trips perfectly within this file and still reaches nobody.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal bridge bead: %v", err)
	}
	var keyed map[string]any
	if err := json.Unmarshal(raw, &keyed); err != nil {
		t.Fatalf("unmarshal bridge bead: %v", err)
	}
	for _, key := range []string{"await_type", "await_id", "created_by", "owner", "notes"} {
		if _, ok := keyed[key]; !ok {
			t.Errorf("bridge JSON has no %q key; the exec store's beadWire decodes by that name", key)
		}
	}
}

// TestBdStoreBridgeGetCarriesPlainColumnsEndToEnd drives the real bridge op
// against a bd that emits the plain columns, so the assertion covers the whole
// producer chain — bd JSON, bdIssue.toBead, bridgeBead, the stdout envelope —
// rather than a hand-written payload that already contains the answer.
func TestBdStoreBridgeGetCarriesPlainColumnsEndToEnd(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
cat <<'JSON'
[{"id":"bd-1","title":"Gate: gh:pr 4912","status":"open","issue_type":"gate",
  "created_at":"2026-02-27T10:00:00Z","await_type":"gh:pr","await_id":"4912",
  "created_by":"seeder","owner":"owner@example.com","notes":"waiting on a PR"}]
JSON
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout bytes.Buffer
	if err := runBdStoreBridge("get", []string{"bd-1"}, scopeDir, "db.example.internal", "3317", "root", bytes.NewReader(nil), &stdout); err != nil {
		t.Fatalf("runBdStoreBridge(get): %v", err)
	}

	var keyed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &keyed); err != nil {
		t.Fatalf("bridge stdout JSON: %v\n%s", err, stdout.String())
	}
	for _, tc := range []struct{ key, want string }{
		{"await_type", "gh:pr"},
		{"await_id", "4912"},
		{"created_by", "seeder"},
		{"owner", "owner@example.com"},
		{"notes", "waiting on a PR"},
	} {
		got, ok := keyed[tc.key].(string)
		if !ok || got != tc.want {
			t.Errorf("bridge stdout %q = %v, want %q; an exec-backed city loses it here\n%s", tc.key, keyed[tc.key], tc.want, stdout.String())
		}
	}
}
