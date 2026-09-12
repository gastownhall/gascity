package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestNamedWorkReadyMatchesRuntimeSessionNameAssignee is the ga-e70d2 repro.
//
// Work routed to a [[named_session]] is claimed under the session's *runtime*
// name — the tmux-safe form config.NamedSessionRuntimeName produces ("/" -> "--",
// "." -> "__") — not under the session's qualified identity. Live maintainer-city
// beads show both forms side by side on the same bead:
//
//	gcg-284771409691718 assignee=seth__seth routed=seth.seth status=open
//	bead.dead_assignee_reopened {"dead_assignee":"randy__randy","routed_to":"randy.randy"}
//
// namedWorkReady compared wb.Assignee against the qualified identity with a raw
// !=, so the two forms never matched and on-demand named sessions never woke for
// their own assigned work. On the live city that left the randy/seth/wendy patrol
// orders open and unstarted for 12h, and the supervisor logged ready=map[] on all
// 2623 evaluations in a day.
//
// spec.SessionName is already an accepted alias for the identity elsewhere in the
// resolver (session.ResolveNamedSessionSpecForConfigTarget matches on it), so
// honoring it here restores a convention the rest of the code already relies on.
func TestNamedWorkReadyMatchesRuntimeSessionNameAssignee(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	const identity = "gascity/patrol"
	runtimeName := agent.SanitizeQualifiedNameForSession(identity)
	if runtimeName == identity {
		t.Fatalf("test premise broken: %q sanitizes to itself, so there are not two distinct forms to confuse", identity)
	}

	// The bead is assigned under the runtime session name, exactly as the live
	// city records it, and is in_progress so it bypasses the readiness gate and
	// isolates the assignee-form comparison as the only thing under test.
	b, err := rigStore.Create(beads.Bead{
		Title:    "patrol work claimed under the runtime session name",
		Type:     "task",
		Status:   "open",
		Assignee: runtimeName,
		Metadata: map[string]string{"gc.routed_to": identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
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

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("named session %q has an in_progress bead assigned to its runtime session name %q "+
			"but generated no named-session demand (NamedSessionDemand=%v).\n"+
			"namedWorkReady compared the bead assignee against the qualified identity only, so the "+
			"runtime-session-name form never matched and the on-demand session never woke for its own work.",
			identity, runtimeName, dsResult.NamedSessionDemand)
	}
}

// TestNamedWorkReadyStillIgnoresUnrelatedAssignee is the control for the test
// above: honoring the runtime session name must not turn every assignee into a
// match. A bead assigned to some other agent entirely must still produce no
// named-session demand — otherwise the first test would pass for the wrong
// reason.
func TestNamedWorkReadyStillIgnoresUnrelatedAssignee(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	b, err := rigStore.Create(beads.Bead{
		Title:    "work belonging to a different agent",
		Type:     "task",
		Status:   "open",
		Assignee: "gascity/somebody-else",
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
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

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if dsResult.NamedSessionDemand["gascity/patrol"] {
		t.Fatalf("a bead assigned to gascity/somebody-else woke named session gascity/patrol "+
			"(NamedSessionDemand=%v); the assignee match is too loose", dsResult.NamedSessionDemand)
	}
}
