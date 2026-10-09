package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionCloseReleasesAssignedWorkBeads is a regression for
// gastownhall/gascity#2625. After `gc session close`, any work bead still
// assigned to the closed session must be released (Assignee cleared, Status
// reset to open) so the pool scale-check picks up the freed demand on the
// next reconcile tick. Without it, Source-1 CachedReady stays stale, the
// pool scale-check sees scaleCount=0, and no fresh worker spawns even
// though the demand is admittable.
func TestCmdSessionCloseReleasesAssignedWorkBeads(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Title:  "stranded worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-stranded",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "admittable demand",
		Type:     "task",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr); code != 0 {
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
	if gotWork.Assignee != "" {
		t.Errorf("work bead Assignee = %q, want empty (released after session close)", gotWork.Assignee)
	}
	if gotWork.Status != "open" {
		t.Errorf("work bead Status = %q, want open (reset so the routed queue can re-pick it)", gotWork.Status)
	}
}

// TestCmdSessionCloseClearsClaimInTheSessionStoreOnAMigratedCity pins the store
// `gc session close` hands the claim clear. On a migrated city the session bead
// lives in the sessions-class binding while the release sweep leads with the
// retained work store; clearing the claim back-channel through the work store
// looked the session bead up where it does not live, logged "clearing current
// claim on retired session ...: bead not found", and left current_claim_bead_id
// stamped on the closed session. The helper's own split-store contract is
// TestUnclaimWorkAssignedToRetiredSessionClearsClaimInTheSessionStore; this row
// is the command's call site, which that helper test cannot see.
func TestCmdSessionCloseClearsClaimInTheSessionStoreOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	t.Setenv("GC_SESSION", "fake")

	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)

	sessionBead, err := sessions.Create(beads.Bead{
		Title:  "claiming worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-claiming",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead in the session store: %v", err)
	}
	claimed, err := work.Create(beads.Bead{
		Title:    "claimed step",
		Type:     "task",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := work.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	if err := sessions.SetMetadata(sessionBead.ID, beadmeta.CurrentClaimBeadIDMetadataKey, claimed.ID); err != nil {
		t.Fatalf("seeding the claim stamp: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "clearing current claim") {
		t.Errorf("stderr = %q, want no claim-clear failure: the close must clear the claim in the session store", stderr.String())
	}

	// The funnel's own handle goes first, so the reads below see durable bytes
	// rather than state an open connection is holding.
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	closed, err := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg)).Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("reading the session bead back from the binding: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("session bead status = %q, want closed", closed.Status)
	}
	if got := closed.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]; got != "" {
		t.Errorf("current claim = %q on the closed session, want cleared in the session store", got)
	}

	reopened, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("reopening the work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(reopened) })
	released, err := reopened.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if released.Assignee != "" || released.Status != "open" {
		t.Errorf("work bead = (assignee %q, status %q), want released to (\"\", open) from the work store", released.Assignee, released.Status)
	}
}
