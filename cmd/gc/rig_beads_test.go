package main

import (
	"encoding/json"
	"fmt"
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
		if routes[0].Prefix != "mc" {
			t.Errorf("self route prefix = %q, want %q", routes[0].Prefix, "mc")
		}
		// Route to frontend
		if routes[1].Prefix != "fe" {
			t.Errorf("routes[1].Prefix = %q, want %q", routes[1].Prefix, "fe")
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
		if routes[0].Prefix != "mc" {
			t.Errorf("routes[0].Prefix = %q, want %q", routes[0].Prefix, "mc")
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
	if entry.Prefix != "mc" || entry.Path != "." {
		t.Errorf("HQ self route = {%q, %q}, want {%q, %q}", entry.Prefix, entry.Path, "mc", ".")
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

// collectRigRoutes spells each scope's REAL issue prefix — the one its store
// records, not the config-derived truncation — because a route keyed by a
// shorter derived prefix dash-append-matches nothing (city-fr3t: derived "ci",
// store mints city-*).
func TestCollectRigRoutes_PrefersStoreIssuePrefix(t *testing.T) {
	orig := rigAdoptStorePrefix
	defer func() { rigAdoptStorePrefix = orig }()
	rigAdoptStorePrefix = func(cityPath, scopePath string) (string, error) {
		switch scopePath {
		case "/home/user/my-city":
			return "city", nil
		case "/home/user/rigs/gk":
			return "gk", nil
		case "/home/user/rigs/guildkeep":
			return "guildkeep", nil
		}
		return "", fmt.Errorf("no store at %s", scopePath)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Rigs: []config.Rig{
			{Name: "gk", Path: "/home/user/rigs/gk"},
			{Name: "guildkeep", Path: "/home/user/rigs/guildkeep"}, // derived would be "gu"
			{Name: "unbound", Path: "   "},                         // skipped, not collected
		},
	}

	routes := collectRigRoutes("/home/user/my-city", cfg)
	if len(routes) != 3 {
		t.Fatalf("len(routes) = %d, want 3", len(routes))
	}
	if routes[0].Prefix != "city" {
		t.Errorf("HQ prefix = %q, want the store's real prefix %q", routes[0].Prefix, "city")
	}
	if routes[1].Prefix != "gk" {
		t.Errorf("gk prefix = %q, want %q", routes[1].Prefix, "gk")
	}
	if routes[2].Prefix != "guildkeep" {
		t.Errorf("guildkeep prefix = %q, want the store's real prefix %q, not the derived truncation", routes[2].Prefix, "guildkeep")
	}
}

// A scope whose store cannot answer — cold, deferred, absent — registers under
// its derived prefix exactly as before city-fr3t; collection never fails.
func TestCollectRigRoutes_FallsBackWhenStoreSilent(t *testing.T) {
	orig := rigAdoptStorePrefix
	defer func() { rigAdoptStorePrefix = orig }()
	rigAdoptStorePrefix = func(string, string) (string, error) {
		return "", fmt.Errorf("store is cold")
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "my-city"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: "/home/user/frontend", Prefix: "fe"},
		},
	}

	routes := collectRigRoutes("/home/user/my-city", cfg)
	if len(routes) != 2 {
		t.Fatalf("len(routes) = %d, want 2", len(routes))
	}
	if routes[0].Prefix != "mc" || routes[1].Prefix != "fe" {
		t.Errorf("prefixes = [%q %q], want derived [mc fe]", routes[0].Prefix, routes[1].Prefix)
	}
}

// A provider-owned scope whose store is cold — retired on its idle timeout
// here — must keep its derived prefix WITHOUT a store read: a proxied
// `bd config get` wakes the proxy the lifecycle deliberately left down.
func TestRouteIssuePrefix_ColdScopeSkipsStoreRead(t *testing.T) {
	origCold := routeScopeCold
	origReader := rigAdoptStorePrefix
	defer func() {
		routeScopeCold = origCold
		rigAdoptStorePrefix = origReader
	}()
	routeScopeCold = func(string, string) bool { return true }
	rigAdoptStorePrefix = func(string, string) (string, error) {
		t.Fatal("route registration dialed a cold store")
		return "", nil
	}

	if got := routeIssuePrefix("/city", "/city/rigs/gk", "gk"); got != "gk" {
		t.Errorf("routeIssuePrefix = %q, want the derived fallback %q", got, "gk")
	}
}

// The real cold check passes a scope the city does not provider-own — reads
// are allowed there, and nothing about a plain scope is cold.
func TestRouteScopeCold_PlainScopeIsWarm(t *testing.T) {
	cityDir := t.TempDir()
	if routeScopeCold(cityDir, filepath.Join(cityDir, "rigs", "gk")) {
		t.Error("routeScopeCold on an unowned scope = true, want false")
	}
}

func TestRouteIssuePrefix_ReaderResultWinsOverDerived(t *testing.T) {
	orig := rigAdoptStorePrefix
	defer func() { rigAdoptStorePrefix = orig }()

	rigAdoptStorePrefix = func(string, string) (string, error) { return "city", nil }
	if got := routeIssuePrefix("/city", "/city", "ci"); got != "city" {
		t.Errorf("routeIssuePrefix = %q, want the store's real prefix %q", got, "city")
	}
	rigAdoptStorePrefix = func(string, string) (string, error) {
		return "", fmt.Errorf("bd failed")
	}
	if got := routeIssuePrefix("/city", "/city", "ci"); got != "ci" {
		t.Errorf("routeIssuePrefix = %q, want the derived fallback %q on reader error", got, "ci")
	}
	rigAdoptStorePrefix = func(string, string) (string, error) { return "  ", nil }
	if got := routeIssuePrefix("/city", "/city", "ci"); got != "ci" {
		t.Errorf("routeIssuePrefix = %q, want the derived fallback %q on empty answer", got, "ci")
	}
}

func TestRouteEntriesFor(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	routesPath := filepath.Join(dir, ".beads", "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(`{"prefix":"city","path":"."}
not json at all
{"prefix":"gk","path":"../rigs/gk"}

{"prefix":"","path":"./dropped"}
{"prefix":"fe","path":"./rigs/fe"}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, ok := routeEntriesFor(dir)
	if !ok {
		t.Fatal("routeEntriesFor = ok false, want true")
	}
	// The malformed and empty-prefix lines are skipped; the blank line is free.
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3 (%v)", len(entries), entries)
	}
	if entries[0].Prefix != "city" || entries[0].Path != "." {
		t.Errorf("entries[0] = %+v, want {city .}", entries[0])
	}
	if entries[1].Prefix != "gk" || entries[1].Path != "../rigs/gk" {
		t.Errorf("entries[1] = %+v, want {gk ../rigs/gk}", entries[1])
	}
	if entries[2].Prefix != "fe" || entries[2].Path != "./rigs/fe" {
		t.Errorf("entries[2] = %+v, want {fe ./rigs/fe}", entries[2])
	}

	if _, ok := routeEntriesFor(t.TempDir()); ok {
		t.Error("routeEntriesFor on a dir without routes.jsonl = ok true, want false")
	}
}
