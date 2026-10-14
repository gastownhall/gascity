package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// mc-58gr9 (staging J11, legacy): `gc session close scribe` landed between
// the tick's session snapshot, which still held scribe's canonical bead, and
// that tick's syncSessionBeads. The on_demand session was desired only
// because of that bead, so sync found no open row and minted a new one in
// state active (the runtime's cached liveness), which then sat open and
// asleep answering to the name.

func onDemandNamedCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "scribe", StartCommand: "true", MaxActiveSessions: intPtr(1), WorkQuery: "printf ''"}},
		NamedSessions: []config.NamedSession{{
			Template: "scribe",
			Mode:     "on_demand",
		}},
	}
}

// canonicalNamedBead creates scribe's canonical bead in store, in state.
func canonicalNamedBead(t *testing.T, store beads.Store, cfg *config.City, state string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "scribe",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "scribe"),
			"alias":                      "scribe",
			"template":                   "scribe",
			"state":                      state,
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "scribe",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create canonical bead: %v", err)
	}
	return b
}

// buildDesiredState marks an on_demand named session that only its canonical
// bead makes desired, and not one that assigned work wants too. Kills the
// flag never set, and set for work-wanted sessions (which must still be
// recreated after a close).
func TestBuildDesiredState_OnDemandNamedSessionMarksCanonicalOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		work bool
		want bool
	}{
		{"canonical bead only", false, true},
		{"assigned work too", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := onDemandNamedCity()
			store := beads.NewMemStore()
			canonicalNamedBead(t, store, cfg, "asleep")
			if tc.work {
				if _, err := store.Create(beads.Bead{Title: "scribe work", Type: "task", Status: "open", Assignee: "scribe"}); err != nil {
					t.Fatal(err)
				}
			}
			ds := buildDesiredState("test-city", t.TempDir(), time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
			found := false
			for _, tp := range ds.State {
				if tp.ConfiguredNamedIdentity != "scribe" {
					continue
				}
				found = true
				if tp.NamedCanonicalOnly != tc.want {
					t.Fatalf("NamedCanonicalOnly = %v, want %v", tp.NamedCanonicalOnly, tc.want)
				}
			}
			if !found {
				t.Fatal("scribe is not desired; want it materialized from its canonical bead")
			}
		})
	}
}

// A close that wins the race leaves no open row: sync neither reopens the
// closed bead nor mints a new one for a session desired only by that bead,
// even with its runtime still reading as running. A session that work wants
// is still recreated. Kills the base code (a fresh row in state active).
func TestSyncSessionBeads_OnDemandNamedSessionClosedMidTickIsNotRecreated(t *testing.T) {
	for _, tc := range []struct {
		name          string
		canonicalOnly bool
		wantOpen      int
	}{
		{"desired only by the closed bead", true, 0},
		{"desired by work", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := onDemandNamedCity()
			store := beads.NewMemStore()
			closed := canonicalNamedBead(t, store, cfg, "awake")
			if err := store.Close(closed.ID); err != nil {
				t.Fatalf("close canonical bead: %v", err)
			}
			sessionName := closed.Metadata["session_name"]
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil { // the cached liveness the race read
				t.Fatalf("start: %v", err)
			}
			ds := map[string]TemplateParams{sessionName: {
				TemplateName:            "scribe",
				InstanceName:            "scribe",
				Alias:                   "scribe",
				Command:                 "true",
				SessionName:             sessionName,
				ConfiguredNamedIdentity: "scribe",
				ConfiguredNamedMode:     "on_demand",
				NamedCanonicalOnly:      tc.canonicalOnly,
			}}
			var stderr bytes.Buffer
			clk := &clock.Fake{Time: time.Date(2026, 10, 14, 3, 58, 50, 0, time.UTC)}
			syncSessionBeads(t.TempDir(), store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)
			all, err := store.ListByLabel(sessionBeadLabel, 0)
			if err != nil {
				t.Fatalf("listing beads: %v", err)
			}
			open := 0
			for _, b := range all {
				if b.Status != "closed" {
					open++
				}
			}
			if open != tc.wantOpen {
				t.Fatalf("open session beads = %d, want %d (beads: %+v; stderr: %s)", open, tc.wantOpen, all, stderr.String())
			}
		})
	}
}
