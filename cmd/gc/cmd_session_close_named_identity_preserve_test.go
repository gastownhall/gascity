package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionCloseOnConfiguredNamedSessionPreservesIdentityAssignedWork is
// a regression for gastownhall/gascity#8179. Closing one incarnation of a
// STILL-CONFIGURED named session must not release work addressed to the
// configured identity: the controller re-materializes the identity right
// away, and the identity's assignments are demand for the identity, not for
// the dying bead. Before the fix, `cmdSessionClose` released every claim in
// releaseScopeOwn's scope unconditionally — which includes the configured
// identity — stripping the assignee and leaving the work to be mis-recovered
// as an orphan (the exact cascade reported in gastownhall/gascity-packs#561).
//
// This mirrors the reconciler's own recyclableDeadConfiguredNamePhantom guard
// (session_reconciler.go), which preserves the same identity for the same
// reason when IT closes a bead squatting a configured identity.
func TestCmdSessionCloseOnConfiguredNamedSessionPreservesIdentityAssignedWork(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "refinery"
start_command = "true"
max_active_sessions = 1

[[named_session]]
name = "refinery"
template = "refinery"
mode = "on_demand"
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	const identity = "refinery" // city-scoped named session: QualifiedName() == IdentityName()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              "refinery",
			"template":                  "refinery",
			"state":                     "active",
			"configured_named_session":  "true",
			"configured_named_identity": identity,
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	// A completed polecat->refinery handoff: the work bead is addressed to
	// the configured identity, open (not in_progress), with no route of its
	// own — exactly the issue's repro shape.
	work, err := store.Create(beads.Bead{
		Title:    "handed-off work",
		Type:     "task",
		Assignee: identity,
		Metadata: map[string]string{"gc.routed_to": ""},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}

	gotSession, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if gotSession.Status != "closed" {
		t.Errorf("session bead status = %q, want closed", gotSession.Status)
	}

	gotWork, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if gotWork.Assignee != identity {
		t.Errorf("work bead Assignee = %q, want preserved at the configured identity %q (gastownhall/gascity#8179)", gotWork.Assignee, identity)
	}
	if gotWork.Status != "open" {
		t.Errorf("work bead Status = %q, want unchanged open (it was never in_progress)", gotWork.Status)
	}
}
