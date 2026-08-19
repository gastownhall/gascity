package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSetTemplateEnvIdentityAlignsBeadsActorWithRuntimeAlias is the #5048
// write-side repro.
//
// Pool/cron workers claim via BEADS_ACTOR. Before this fix, setTemplateEnvIdentity
// stamped GC_ALIAS/GC_AGENT with the stable pool identity (bd.dog-1) while
// leaving BEADS_ACTOR on the sanitized tmux session name seeded by
// resolveTemplate (bd__dog-<beadID>). Claims therefore landed under a form
// later respawns no longer export in $GC_ALIAS, stranding healthy idle dogs.
func TestSetTemplateEnvIdentityAlignsBeadsActorWithRuntimeAlias(t *testing.T) {
	const (
		sanitizedSessionName = "bd__dog-ma-wisp-dw8d64"
		runtimeIdentity      = "bd.dog-1"
	)
	if !strings.Contains(sanitizedSessionName, "__") {
		t.Fatal("test premise broken: sanitized form must differ from dotted alias")
	}
	if sanitizedSessionName == runtimeIdentity {
		t.Fatal("test premise broken: sanitized session name and runtime identity must differ")
	}

	tp := &TemplateParams{
		Env: map[string]string{
			"GC_SESSION_NAME": sanitizedSessionName,
			"GC_AGENT":        sanitizedSessionName,
			"GC_ALIAS":        "bd.dog",
			"BEADS_ACTOR":     sanitizedSessionName,
		},
	}
	setTemplateEnvIdentity(tp, runtimeIdentity)

	if got := tp.Env["GC_ALIAS"]; got != runtimeIdentity {
		t.Fatalf("GC_ALIAS = %q, want runtime identity %q", got, runtimeIdentity)
	}
	if got := tp.Env["GC_AGENT"]; got != runtimeIdentity {
		t.Fatalf("GC_AGENT = %q, want runtime identity %q", got, runtimeIdentity)
	}
	if got := tp.Env["BEADS_ACTOR"]; got != runtimeIdentity {
		t.Fatalf("BEADS_ACTOR = %q, want runtime identity %q (claim assignee must match $GC_ALIAS)", got, runtimeIdentity)
	}
	if got := tp.Env["GC_SESSION_NAME"]; got != sanitizedSessionName {
		t.Fatalf("GC_SESSION_NAME = %q, want preserved sanitized handle %q", got, sanitizedSessionName)
	}
	if !tp.EnvIdentityStamped {
		t.Fatal("EnvIdentityStamped = false, want true")
	}
}

// TestNamedWorkReadyMatchesRuntimeSessionNameAssignee covers the read-side of
// the same sanitized-vs-runtime identity mismatch: work claimed under the
// tmux-safe session name must still wake the on-demand named session whose
// qualified identity differs only by separator encoding.
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
