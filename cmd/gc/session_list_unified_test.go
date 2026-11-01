package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the values `gc session list --json` prints on each route
// now that both routes render one shape (#8453): closed rows read state
// "closed" and closed true, a never-active session prints last_active "", and
// timestamps are RFC 3339 UTC at second precision.

// sessionListUTCSecond matches an RFC 3339 UTC timestamp at second precision.
var sessionListUTCSecond = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

// seedSessionListBead creates one session bead with the given metadata and
// returns its id.
func seedSessionListBead(t *testing.T, store beads.Store, title string, meta map[string]string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("store.Create(%s): %v", title, err)
	}
	return b.ID
}

// runSessionListFallbackJSON runs the local listing directly (no routing)
// and decodes the document.
func runSessionListFallbackJSON(t *testing.T, stateFilter string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := doSessionListFallback(stateFilter, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("doSessionListFallback(%q) = %d; stderr=%s", stateFilter, code, stderr.String())
	}
	validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, stdout.Bytes())
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return doc
}

// sessionListRowByID returns the decoded row whose id is id, or nil.
func sessionListRowByID(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	for _, row := range sessionListRows(t, doc) {
		if row["id"] == id {
			return row
		}
	}
	return nil
}

// TestSessionListLocalRowsUseUnifiedValues pins the local route's values:
// last_active is "" for a never-active session, created_at is RFC 3339 UTC at
// second precision, closed is always a boolean, and no row carries the
// API-only running field.
func TestSessionListLocalRowsUseUnifiedValues(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	cityDir := writeSessionListTestCity(t)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	asleep := seedSessionListBead(t, store, "asleep", map[string]string{
		"session_name": "omega--worker", "template": "worker", "state": "asleep",
	})

	doc := runSessionListFallbackJSON(t, "")
	row := sessionListRowByID(t, doc, asleep)
	if row == nil {
		t.Fatalf("missing the asleep row: %v", doc)
	}
	if row["last_active"] != "" {
		t.Errorf("last_active = %#v, want \"\" for a never-active session", row["last_active"])
	}
	if created, _ := row["created_at"].(string); !sessionListUTCSecond.MatchString(created) {
		t.Errorf("created_at = %q, want RFC 3339 UTC at second precision", created)
	}
	if row["rig"] != "omega" || row["name"] != "omega--worker" {
		t.Errorf("rig/name = %#v/%#v, want omega/omega--worker", row["rig"], row["name"])
	}
	for _, r := range sessionListRows(t, doc) {
		if _, ok := r["closed"].(bool); !ok {
			t.Errorf("row %v: closed = %#v, want a boolean", r["id"], r["closed"])
		}
		for _, key := range []string{"running", "reason", "last_output", "program_status"} {
			if _, ok := r[key]; ok {
				t.Errorf("local row %v carries API-only %s", r["id"], key)
			}
		}
	}
	if _, ok := doc["_cache_age_s"]; ok {
		t.Errorf("local document carries _cache_age_s: %v", doc)
	}
}

// TestRenderSessionListFromAPIPrintsLocalContract pins that the API route
// prints the local document: filters and summary computed over the listed
// rows, the identity fields the wire carries (name and rig derived the local
// way), normalized timestamps, closed rows, plus the API-only running and
// _cache_age_s, and never the redacted command or session_key.
func TestRenderSessionListFromAPIPrintsLocalContract(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GC-Cache-Age-S", "3")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{
				{
					"id": "gc-1", "template": "alpha/rigbot", "state": "active", "title": "",
					"session_name": "alpha--rigbot", "provider": "claude", "created_at": "2026-10-28T10:00:00+02:00",
					"last_active": "2026-10-28T14:00:00+02:00",
					"attached":    false, "running": true, "agent_name": "alpha/rigbot", "transport": "tmux",
					"worker_dir": " /w ", "last_nudge_delivered_at": "2026-10-28T14:00:00+02:00",
				},
				{
					"id": "gc-2", "template": "worker", "state": "closed", "title": "",
					"session_name": "worker", "provider": "", "created_at": "2026-10-27T10:00:00Z",
					"attached": false, "running": false,
				},
			},
		})
	}))
	defer ts.Close()

	var stdout, stderr bytes.Buffer
	client := api.NewCityScopedClient(ts.URL, "test-city")
	if code := routeSessionList(t.TempDir(), "all", "", client, "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("routeSessionList = %d; stderr=%s", code, stderr.String())
	}
	validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, stdout.Bytes())
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if age, _ := doc["_cache_age_s"].(float64); age != 3 {
		t.Errorf("_cache_age_s = %#v, want 3", doc["_cache_age_s"])
	}
	filters, _ := doc["filters"].(map[string]any)
	if filters["state"] != "all" {
		t.Errorf("filters = %#v, want state all", doc["filters"])
	}
	summary, _ := doc["summary"].(map[string]any)
	for key, want := range map[string]float64{"total": 2, "active": 1, "suspended": 0, "closed": 1} {
		if summary[key] != want {
			t.Errorf("summary.%s = %#v, want %v", key, summary[key], want)
		}
	}

	open := sessionListRowByID(t, doc, "gc-1")
	if open == nil {
		t.Fatalf("missing gc-1: %s", stdout.String())
	}
	for key, want := range map[string]any{
		"name": "alpha--rigbot", "rig": "alpha", "agent_name": "alpha/rigbot", "transport": "tmux",
		"worker_dir": "/w", "provider": "claude", "state": "active", "closed": false, "running": true,
		"created_at": "2026-10-28T08:00:00Z", "last_active": "2026-10-28T12:00:00Z",
		"last_nudge_delivered_at": "2026-10-28T12:00:00Z",
	} {
		if open[key] != want {
			t.Errorf("gc-1 %s = %#v, want %#v", key, open[key], want)
		}
	}

	closed := sessionListRowByID(t, doc, "gc-2")
	if closed == nil {
		t.Fatalf("missing gc-2: %s", stdout.String())
	}
	for key, want := range map[string]any{"state": "closed", "closed": true, "last_active": "", "running": false, "name": "worker"} {
		if closed[key] != want {
			t.Errorf("gc-2 %s = %#v, want %#v", key, closed[key], want)
		}
	}
	for _, row := range sessionListRows(t, doc) {
		for _, key := range []string{"command", "session_key", "resume_flag", "resume_style", "resume_command"} {
			if _, ok := row[key]; ok {
				t.Errorf("API row %v carries redacted %s", row["id"], key)
			}
		}
	}
}
