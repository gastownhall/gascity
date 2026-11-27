package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestGenerateRoutesFor(t *testing.T) {
	all := []rigRoute{
		{Prefix: "mc", AbsDir: "/home/user/my-city"},
		{Prefix: "fe", AbsDir: "/home/user/projects/my-frontend"},
		{Prefix: "be", AbsDir: "/home/user/projects/my-backend"},
	}

	t.Run("from HQ", func(t *testing.T) {
		routes, err := generateRoutesFor(all[0], all)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(routes) != 3 {
			t.Fatalf("len(routes) = %d, want 3", len(routes))
		}
		// Self-route should be "."
		if routes[0].Path != "." {
			t.Errorf("self route path = %q, want %q", routes[0].Path, ".")
		}
		if routes[0].Prefix != "mc-" {
			t.Errorf("self route prefix = %q, want %q", routes[0].Prefix, "mc-")
		}
		// Route to frontend
		if routes[1].Prefix != "fe-" {
			t.Errorf("routes[1].Prefix = %q, want %q", routes[1].Prefix, "fe-")
		}
		if routes[1].Path != "../projects/my-frontend" {
			t.Errorf("routes[1].Path = %q, want %q", routes[1].Path, "../projects/my-frontend")
		}
	})

	t.Run("from frontend", func(t *testing.T) {
		routes, err := generateRoutesFor(all[1], all)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(routes) != 3 {
			t.Fatalf("len(routes) = %d, want 3", len(routes))
		}
		// Route to HQ
		if routes[0].Prefix != "mc-" {
			t.Errorf("routes[0].Prefix = %q, want %q", routes[0].Prefix, "mc-")
		}
		if routes[0].Path != "../../my-city" {
			t.Errorf("routes[0].Path = %q, want %q", routes[0].Path, "../../my-city")
		}
		// Self-route
		if routes[1].Path != "." {
			t.Errorf("self route path = %q, want %q", routes[1].Path, ".")
		}
		// Route to backend (sibling)
		if routes[2].Path != "../my-backend" {
			t.Errorf("routes[2].Path = %q, want %q", routes[2].Path, "../my-backend")
		}
	})

	t.Run("prefix normalization avoids double hyphen", func(t *testing.T) {
		got, err := generateRoutesFor(
			rigRoute{Prefix: "hq", AbsDir: "/home/user/city"},
			[]rigRoute{
				{Prefix: "hq-", AbsDir: "/home/user/city"},
				{Prefix: "fx", AbsDir: "/home/user/city/rigs/fx"},
				{Prefix: "", AbsDir: "/home/user/city/rigs/empty"},
			},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got[0].Prefix != "hq-" {
			t.Errorf("got[0].Prefix = %q, want %q", got[0].Prefix, "hq-")
		}
		if got[1].Prefix != "fx-" {
			t.Errorf("got[1].Prefix = %q, want %q", got[1].Prefix, "fx-")
		}
		if got[2].Prefix != "" {
			t.Errorf("got[2].Prefix = %q, want empty", got[2].Prefix)
		}
	})
}

func TestWriteAllRoutes(t *testing.T) {
	dir := t.TempDir()
	hqDir := filepath.Join(dir, "my-city")
	feDir := filepath.Join(dir, "projects", "frontend")

	if err := os.MkdirAll(hqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(feDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rigs := []rigRoute{
		{Prefix: "mc", AbsDir: hqDir},
		{Prefix: "fe", AbsDir: feDir},
	}

	if err := writeAllRoutes(rigs); err != nil {
		t.Fatalf("writeAllRoutes() error = %v", err)
	}

	// Check HQ routes file exists and has correct content.
	hqRoutes := filepath.Join(hqDir, ".beads", "routes.jsonl")
	data, err := os.ReadFile(hqRoutes)
	if err != nil {
		t.Fatalf("reading HQ routes: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("HQ routes has %d lines, want 2", len(lines))
	}

	var entry routeEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("parsing HQ route line 0: %v", err)
	}
	if entry.Prefix != "mc-" || entry.Path != "." {
		t.Errorf("HQ self route = {%q, %q}, want {%q, %q}", entry.Prefix, entry.Path, "mc-", ".")
	}

	// Check frontend routes file.
	feRoutes := filepath.Join(feDir, ".beads", "routes.jsonl")
	data, err = os.ReadFile(feRoutes)
	if err != nil {
		t.Fatalf("reading frontend routes: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("frontend routes has %d lines, want 2", len(lines))
	}
}

func TestWriteAllRoutes_Idempotent(t *testing.T) {
	dir := t.TempDir()
	rigs := []rigRoute{
		{Prefix: "mc", AbsDir: dir},
	}

	// Write twice — second should overwrite cleanly.
	if err := writeAllRoutes(rigs); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeAllRoutes(rigs); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".beads", "routes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 route line after idempotent write, got %d", len(lines))
	}
}

func TestWriteRoutesFile_Atomic(t *testing.T) {
	dir := t.TempDir()
	routes := []routeEntry{{Prefix: "mc", Path: "."}}

	if err := writeRoutesFile(dir, routes); err != nil {
		t.Fatalf("writeRoutesFile() error = %v", err)
	}

	// Verify no temp files left behind (atomic rename cleans up).
	// Temp files use a PID+nonce suffix for concurrent safety.
	beadsDir := filepath.Join(dir, ".beads")
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file should not exist after successful write: %s", e.Name())
		}
	}

	// Verify actual file exists and is valid JSONL.
	data, err := os.ReadFile(filepath.Join(beadsDir, "routes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry routeEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("routes.jsonl is not valid JSON: %v", err)
	}
}

func TestCollectRigRoutes_UsesEffectivePrefix(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: "/home/user/frontend", Prefix: "fe"},
			{Name: "backend", Path: "/home/user/backend"}, // derived
		},
	}

	routes := collectRigRoutes("/home/user/my-city", cfg)
	if len(routes) != 3 {
		t.Fatalf("len(routes) = %d, want 3", len(routes))
	}
	// HQ — derived from city name.
	if routes[0].Prefix != "mc" {
		t.Errorf("HQ prefix = %q, want %q", routes[0].Prefix, "mc")
	}
	// Frontend — explicit prefix.
	if routes[1].Prefix != "fe" {
		t.Errorf("frontend prefix = %q, want %q", routes[1].Prefix, "fe")
	}
	// Backend — derived prefix.
	if routes[2].Prefix != "ba" {
		t.Errorf("backend prefix = %q, want %q", routes[2].Prefix, "ba")
	}
}
