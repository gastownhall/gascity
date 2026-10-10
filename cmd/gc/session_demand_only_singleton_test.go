package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// #6858: a pool agent with max_active_sessions = 1 and no [[named_session]]
// is a demand-only singleton. The controller starts its one session only when
// the pool has work, so `gc session pin` and `gc session wake` must not report
// success for such a session that is not running: the reconciler ignores both.

const demandOnlySingletonCityTOML = `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
min_active_sessions = 0
max_active_sessions = 1
`

// apiCreatedSingletonMetadata is the bead a pre-#6858 API create left behind
// for a demand-only singleton: ephemeral origin, a pending-create claim and no
// pool marker, stuck in start-pending.
func apiCreatedSingletonMetadata(now time.Time) map[string]string {
	return map[string]string{
		"template":                  "worker",
		"agent_name":                "worker",
		"session_name":              "s-gc-api",
		"state":                     string(session.StateStartPending),
		"pending_create_claim":      "true",
		"pending_create_started_at": now.UTC().Format(time.RFC3339),
		"session_origin":            "ephemeral",
	}
}

func demandOnlySingletonCfg(named bool) *config.City {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	if named {
		cfg.NamedSessions = []config.NamedSession{{Template: "worker", Mode: "on_demand"}}
	}
	return cfg
}

func TestDoSessionWake_DemandOnlySingletonReportsItWillNotStart(t *testing.T) {
	now := time.Now()
	controllerAsleep := map[string]string{
		"template":       "worker",
		"agent_name":     "worker",
		"session_name":   "worker",
		"state":          "asleep",
		"session_origin": "ephemeral",
		"pool_managed":   "true",
	}
	running := map[string]string{
		"template":       "worker",
		"agent_name":     "worker",
		"session_name":   "worker",
		"state":          "active",
		"session_origin": "ephemeral",
		"pool_managed":   "true",
	}
	starting := map[string]string{
		"template":                  "worker",
		"agent_name":                "worker",
		"session_name":              "worker",
		"state":                     string(session.StateCreating),
		"pending_create_claim":      "true",
		"pending_create_started_at": now.UTC().Format(time.RFC3339),
		"session_origin":            "ephemeral",
		"pool_managed":              "true",
	}
	manual := map[string]string{
		"template":       "worker",
		"agent_name":     "worker",
		"session_name":   "s-gc-manual",
		"state":          "asleep",
		"session_origin": "manual",
	}
	tests := []struct {
		name     string
		meta     map[string]string
		named    bool
		wantCode int
	}{
		{name: "API-created start-pending bead", meta: apiCreatedSingletonMetadata(now), wantCode: 1},
		{name: "controller pool bead asleep", meta: controllerAsleep, wantCode: 1},
		{name: "running pool bead", meta: running, wantCode: 0},
		{name: "pool bead whose start is in flight", meta: starting, wantCode: 0},
		{name: "manual session of the same template", meta: manual, wantCode: 0},
		{name: "template backed by a named session", meta: apiCreatedSingletonMetadata(now), named: true, wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Title:    "worker",
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: tt.meta,
			})
			if err != nil {
				t.Fatalf("store.Create(session bead): %v", err)
			}
			deps := sessionWakeDeps{
				store:                     store,
				cfg:                       demandOnlySingletonCfg(tt.named),
				cityPath:                  "/city",
				cityResolved:              true,
				now:                       time.Now,
				withdrawQueuedWaitNudges:  func(string, []string) error { return nil },
				cityUsesManagedReconciler: func(string) bool { return false },
				pokeController:            func(string) error { return nil },
			}

			var stdout, stderr bytes.Buffer
			code := doSessionWake(b.ID, &stdout, &stderr, false, deps)
			if code != tt.wantCode {
				t.Fatalf("doSessionWake() = %d, want %d; stdout=%s stderr=%s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if tt.wantCode == 0 {
				if got := stdout.String(); !strings.Contains(got, "wake requested") {
					t.Fatalf("stdout = %q, want wake requested", got)
				}
				return
			}
			if got := stdout.String(); strings.Contains(got, "wake requested") {
				t.Fatalf("stdout = %q, must not report success for a wake that cannot start the session", got)
			}
			for _, want := range []string{b.ID, "will not start", "max_active_sessions = 1", "sling work", "[[named_session]]"} {
				if got := stderr.String(); !strings.Contains(got, want) {
					t.Fatalf("stderr = %q, want substring %q", got, want)
				}
			}
		})
	}
}

func TestCmdSessionPin_RefusesDemandOnlySingletonSession(t *testing.T) {
	tests := []struct {
		name string
		meta func(time.Time) map[string]string
	}{
		{name: "API-created start-pending bead", meta: apiCreatedSingletonMetadata},
		{name: "running pool bead", meta: func(time.Time) map[string]string {
			return map[string]string{
				"template":       "worker",
				"agent_name":     "worker",
				"session_name":   "worker",
				"state":          "active",
				"session_origin": "ephemeral",
				"pool_managed":   "true",
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")
			t.Setenv("GC_DIR", t.TempDir())
			cityDir := t.TempDir()
			writePhase0InterfaceCity(t, cityDir, demandOnlySingletonCityTOML)
			t.Setenv("GC_CITY", cityDir)

			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt: %v", err)
			}
			b, err := store.Create(beads.Bead{
				Title:    "worker",
				Type:     session.BeadType,
				Labels:   []string{session.LabelSession},
				Metadata: tt.meta(time.Now()),
			})
			if err != nil {
				t.Fatalf("Create(session): %v", err)
			}

			var stdout, stderr bytes.Buffer
			code := cmdSessionPin([]string{b.ID}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("cmdSessionPin(%s) = 0, want nonzero; stdout=%s stderr=%s", b.ID, stdout.String(), stderr.String())
			}
			if got := stdout.String(); strings.Contains(got, "pinned") {
				t.Fatalf("stdout = %q, must not report a pin the reconciler ignores", got)
			}
			for _, want := range []string{b.ID, "max_active_sessions = 1", "sling work", "[[named_session]]"} {
				if got := stderr.String(); !strings.Contains(got, want) {
					t.Fatalf("stderr = %q, want substring %q", got, want)
				}
			}
			got := onlySessionBead(t, cityDir)
			if got.Metadata["pin_awake"] != "" {
				t.Fatalf("pin_awake = %q, want unset after a refused pin", got.Metadata["pin_awake"])
			}
		})
	}
}
