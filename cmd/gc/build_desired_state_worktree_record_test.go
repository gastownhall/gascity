package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestBindPoolSessionTriggerBead_PreservesLauncherWorktreePath is the
// regression guard for sc-s5mfl3: the recorded gc.work_dir must match the
// worktree the launcher actually created, including the work-bead title slug
// suffix (e.g. "dip-<bead>-implement-compound-work-item").
//
// The launcher derives the worktree name from the work bead's id+title slug
// when the session is first created ("new" tier, title known). On a subsequent
// reconcile the same session is re-bound to the same trigger bead via a
// resume/wake request that does NOT carry the title (WorkBeadTitle == ""). The
// pre-fix code unconditionally re-derived the work_dir from the incomplete
// request, producing the suffix-less "dip-<bead>" form and clobbering the
// launcher-created path that was already recorded. The build-artifact-valid
// gate then chdirs into a path that never existed.
//
// The fix makes the recorded launcher path the single source of truth: for an
// unchanged trigger bead, the already-recorded work_dir is preserved rather
// than re-derived from a request that may be missing the title.
func TestBindPoolSessionTriggerBead_PreservesLauncherWorktreePath(t *testing.T) {
	const (
		workBead = "dip-42"
		title    = "implement compound work item"
	)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "dip"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			WorkDir:           ".gc/workspaces/{{.AgentBase}}",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	var stderr bytes.Buffer
	store := beads.NewMemStore()
	bp := newAgentBuildParams("dip", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)

	// The base workspace root the launcher joins the trigger slug under.
	base := filepath.Join(bp.cityPath, ".gc", "workspaces", "worker")
	// What the launcher actually creates the first time, title in hand.
	launcherCreated := filepath.Join(base, "dip-42-implement-compound-work-item")

	// 1. First bind: "new" tier carries the title. This mirrors the launcher's
	//    own path derivation, so the recorded work_dir == the created worktree.
	created, err := store.Create(beads.Bead{ID: "sess-1", Type: "session"})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(created.ID)
	if err != nil {
		t.Fatalf("get session info: %v", err)
	}
	bound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, SessionRequest{
		Tier:          "new",
		WorkBeadID:    workBead,
		WorkBeadTitle: title,
	})
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	recorded := bound.WorkDirCanonical
	if recorded != launcherCreated {
		t.Fatalf("first-bind work_dir = %q, want launcher-created %q", recorded, launcherCreated)
	}

	// 2. Re-bind on the next reconcile: a resume/wake request for the SAME
	//    trigger bead, but WITHOUT the title (the resume/wake tiers never
	//    populate WorkBeadTitle). The recorded launcher path must survive.
	reBound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", bound, SessionRequest{
		Tier:       "wake-known-identity",
		WorkBeadID: workBead,
		// WorkBeadTitle intentionally empty.
	})
	if err != nil {
		t.Fatalf("re-bind: %v", err)
	}

	got := reBound.WorkDirCanonical
	if got != launcherCreated {
		t.Fatalf("re-bind dropped the title suffix:\n  recorded = %q\n  created  = %q\nrecorded path never existed", got, launcherCreated)
	}
	if reBound.WorkDir != launcherCreated {
		t.Fatalf("re-bind legacy work_dir = %q, want %q", reBound.WorkDir, launcherCreated)
	}

	// The store copy must agree with the returned bead.
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := persisted.Metadata[beadmeta.WorkDirMetadataKey]; got != launcherCreated {
		t.Fatalf("persisted work_dir = %q, want %q", got, launcherCreated)
	}
	if got := persisted.Metadata[beadmeta.LegacyWorkDirMetadataKey]; got != launcherCreated {
		t.Fatalf("persisted legacy work_dir = %q, want %q", got, launcherCreated)
	}
}
