package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdHookCurrentDoesNotPrintAStaleClosedStamp: the claim stamp is written
// best-effort, so it can keep naming a previous claim that is now closed while
// the session's live in_progress claim is another bead. `gc hook current
// --id-only` must not hand the closed bead back as the current claim.
func TestCmdHookCurrentDoesNotPrintAStaleClosedStamp(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	const actor = "demo/worker-1"
	t.Setenv("BEADS_ACTOR", actor)
	t.Setenv("GC_AGENT", actor)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sess, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Title:    "session",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"state": string(session.StateActive)},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	stale, err := store.Create(beads.Bead{
		Title: "previous step", Type: "task", Assignee: actor,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: sess.ID},
	})
	if err != nil {
		t.Fatalf("create stale bead: %v", err)
	}
	if err := store.Close(stale.ID); err != nil {
		t.Fatalf("close stale bead: %v", err)
	}
	live, err := store.Create(beads.Bead{
		Title: "current step", Type: "task", Assignee: actor,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: sess.ID},
	})
	if err != nil {
		t.Fatalf("create live bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(live.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark live bead in_progress: %v", err)
	}
	// The best-effort stamp still names the previous, now-closed claim.
	if err := store.SetMetadata(sess.ID, beadmeta.CurrentClaimBeadIDMetadataKey, stale.ID); err != nil {
		t.Fatalf("stamp session: %v", err)
	}
	t.Setenv("GC_SESSION_ID", sess.ID)

	var stdout, stderr bytes.Buffer
	code := cmdHookCurrent(true, &stdout, &stderr)
	got := strings.TrimSpace(stdout.String())
	if got == stale.ID {
		t.Fatalf("gc hook current --id-only printed the stale CLOSED stamp %s (exit %d); live claim is %s; stderr=%s",
			stale.ID, code, live.ID, stderr.String())
	}
	if code == 0 && got != live.ID {
		t.Fatalf("gc hook current --id-only = %q (exit 0), want the live claim %s; stderr=%s", got, live.ID, stderr.String())
	}
}
