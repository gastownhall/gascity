package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-0o3uby: a bd-deferred bead assigned to an on-demand named session shows up
// in the assigned-work snapshot dump as status=open (mapBdStatus collapses
// deferred into open), but it must generate no named-session demand. This pins
// both halves: the dump marks the deferral, and namedWorkReady stays false. The
// non-deferred control keeps the demand assertion from passing vacuously.
func TestNamedSessionDemand_DeferredAssignedBead_NoDemandAndDumpMarksIt(t *testing.T) {
	const identity = "gascity/patrol"

	build := func(t *testing.T, deferred bool) (demand bool, dump string) {
		t.Helper()
		t.Setenv("GC_DEBUG", "1")
		cityPath := t.TempDir()
		rigPath := filepath.Join(cityPath, "gascity")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		cityStore := beads.NewMemStore()
		rigStore := beads.NewMemStore()
		if _, err := rigStore.Create(beads.Bead{
			Title:                "patrol work",
			Type:                 "task",
			Assignee:             identity,
			Metadata:             map[string]string{"gc.routed_to": identity},
			IndefinitelyDeferred: deferred,
		}); err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "gc"},
			Rigs:      []config.Rig{{Name: "gascity", Path: rigPath}},
			Agents: []config.Agent{{
				Name:         "patrol",
				Dir:          "gascity",
				StartCommand: "true",
				WorkQuery:    "printf ''",
			}},
			NamedSessions: []config.NamedSession{{
				Template: "patrol",
				Dir:      "gascity",
				Mode:     "on_demand",
			}},
		}
		var stderr bytes.Buffer
		ds := buildDesiredStateWithSessionBeads(
			"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
			cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, &stderr,
		)
		return ds.NamedSessionDemand[identity], stderr.String()
	}

	if demand, dump := build(t, false); !demand {
		t.Fatalf("control: an open ready bead assigned to %q generated no named-session demand; dump:\n%s", identity, dump)
	}

	demand, dump := build(t, true)
	if demand {
		t.Errorf("a bd-deferred bead assigned to %q generated named-session demand", identity)
	}
	if !strings.Contains(dump, "status=open deferred=true") {
		t.Errorf("assigned-work dump does not mark the deferred bead; dump:\n%s", dump)
	}
}
