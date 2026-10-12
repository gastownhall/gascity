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

// ga-uvwxp8: demand COLLECTION, not the demand COMPARISON, is what strands an
// on-demand named session once its session bead ("phantom") closes.
//
// #5231 (ga-e70d2) fixed the comparison: namedWorkReady now accepts both the
// qualified identity and the runtime session name via
// namedSessionAssigneeMatchesSpec. But that comparison only ever sees beads that
// collectAssignedWorkBeadsWithStores actually captured, and capture runs four
// passes:
//
//   - in_progress            — List(status=in_progress), NOT assignee-scoped
//   - open assigned molecule — isOpenAssignedMoleculeWork, molecule types only
//   - open routed w/ assignee— captured but deliberately never markReadyAssigned
//   - ready-by-assignee      — Ready(Assignee: X) for each X in
//     readyAssignedWorkAssignees
//
// An OPEN, READY, non-molecule bead reaches namedWorkReady only through the last
// pass, and that pass is an EXACT-string assignee query (memstore.go:485,
// `b.Assignee != q.Assignee`). readyAssignedWorkAssignees enumerates the runtime
// name only from OPEN session beads (via session.AssigneeIdentities ->
// SessionNameMetadata); from config it adds only QualifiedName(). So once the
// phantom closes, the runtime-name form is never queried, the bead is never
// captured, and the fixed comparison never runs.
//
// #5231's own regression test does not cover this: it marks the bead
// in_progress precisely so it "bypasses the readiness gate", which also routes it
// through the non-assignee-scoped in_progress pass and past the broken collection.
//
// The fixture below models the phantom-closed state literally: a CLOSED
// gc:session bead for the identity lives in cityStore (mirroring
// TestFindClosedNamedSessionBead_ReopensOnRestart in session_beads_test.go),
// rather than simply passing nil session beads. Passing nil models "no session
// ever existed"; a closed bead in cityStore models "it existed and closed,"
// which is the actual phantom scenario findClosedNamedSessionBead is meant to
// detect and the runtime-name fallback needs to key off.
func TestNamedSessionDemand_OpenReadyWorkUnderRuntimeName_SurvivesPhantomClose(t *testing.T) {
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

	// Open + ready + assigned under the runtime name, exactly as a live city
	// records a named session's own claim (#5231: assignee=seth__seth,
	// routed=seth.seth). Deliberately NOT in_progress and NOT a molecule type,
	// so the ready-by-assignee pass is the only pass that can capture it.
	if _, err := rigStore.Create(beads.Bead{
		Title:    "patrol work left open+ready, claimed under the runtime session name",
		Type:     "task",
		Status:   "open",
		Assignee: runtimeName,
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	// The phantom: a closed named-session bead for this identity, shaped like
	// the reference fixture in TestFindClosedNamedSessionBead_ReopensOnRestart.
	// findClosedNamedSessionBead matches purely on the
	// namedSessionIdentityMetadata metadata key — Type/Labels are cosmetic,
	// mirrored here only for idiom-consistency with that existing test.
	closedSessionBead, err := cityStore.Create(beads.Bead{
		Type:   "gc:session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":               runtimeName,
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cityStore.Close(closedSessionBead.ID); err != nil {
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

	// sessionBeads (the OPEN-session snapshot) stays nil: the phantom has
	// already been recycled/closed, so there is no open session bead. The
	// closed bead instead lives in cityStore, which is what
	// findClosedNamedSessionBead must be consulted against to recover the
	// runtime-name form once the fix lands.
	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("on-demand named session %q has OPEN READY work assigned to its runtime name %q, "+
			"and a CLOSED phantom session bead recording that runtime name still exists in cityStore, "+
			"but generated no named-session demand (NamedSessionDemand=%v).\n"+
			"readyAssignedWorkAssignees adds only QualifiedName() from config, so the runtime-name "+
			"form is enumerated only while an OPEN session bead supplies it. With the phantom closed, "+
			"Ready(Assignee=%q) is never issued, the bead is never collected, and the #5231 comparison "+
			"fix never gets to run. The work keeps its assignee and nothing re-materializes a session.",
			identity, runtimeName, dsResult.NamedSessionDemand, runtimeName)
	}
}

// TestNamedSessionDemand_OpenReadyWorkUnderQualifiedIdentity_NoSessionBead is the
// control that disproves the competing hypothesis — "open (non-in_progress) work
// simply never wakes an on-demand named session, so the name form is irrelevant."
//
// Identical to the test above in every respect EXCEPT the assignee string, which
// is the qualified identity readyAssignedWorkAssignees does add from config. If
// this passes while the runtime-name case fails, the readiness gate and the
// open-vs-in_progress distinction are exonerated and the assignee FORM is the
// sole cause.
func TestNamedSessionDemand_OpenReadyWorkUnderQualifiedIdentity_NoSessionBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	const identity = "gascity/patrol"

	if _, err := rigStore.Create(beads.Bead{
		Title:    "patrol work left open+ready, claimed under the qualified identity",
		Type:     "task",
		Status:   "open",
		Assignee: identity,
		Metadata: map[string]string{"gc.routed_to": identity},
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

	dsResult := buildDesiredStateWithSessionBeads(
		"gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"gascity": rigStore}, nil, nil, io.Discard,
	)

	if !dsResult.NamedSessionDemand[identity] {
		t.Fatalf("CONTROL FAILED: open ready work assigned to the qualified identity %q produced no "+
			"named-session demand either (NamedSessionDemand=%v).\n"+
			"That refutes the runtime-name theory: the blocker would then be the open/readiness path, "+
			"not the assignee form. Re-investigate before handing off a fix spec.",
			identity, dsResult.NamedSessionDemand)
	}
}
