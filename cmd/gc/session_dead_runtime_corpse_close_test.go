package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The dead-runtime corpse cleaner reaps a tmux session whose panes are all
// dead and then CLOSES the bead that owned it, so the alias is released for a
// successor (gastownhall/gascity#2437). That reasoning holds for a row that
// asserts a live incarnation. It does not hold for a row that is dormant by
// design: `gc session kill` stops the runtime and syncs the bead to asleep
// precisely so a later `gc session wake` can start a fresh runtime on the same
// durable session, and the corpse the kill leaves behind is the EXPECTED
// residue of that sleep, not evidence the session is over.
//
// ga-f7v2ft.156: in the v59 journey the cleaner's tick landed inside that
// window and closed the killed row as dead-runtime. The wake that followed
// could never converge — a closed row can never reach BaseStateActive with a
// rotated instance token — and the leg failed at :1395 with
// `Closed:true MetadataState:dead-runtime SleepReason:killed`, three times in
// eight runs.

func newDeadRuntimeCorpseStore(t *testing.T, metadata map[string]string) (beads.Store, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:    "worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return store, bead
}

// TestCleanupDeadRuntimeSessionCorpsesKeepsAKilledAsleepRowOpen pins the
// journey shape: the corpse is reaped, the durable session survives.
func TestCleanupDeadRuntimeSessionCorpsesKeepsAKilledAsleepRowOpen(t *testing.T) {
	const name = "worker-adhoc-cf937f30dc"
	store, bead := newDeadRuntimeCorpseStore(t, map[string]string{
		"session_name":   name,
		"template":       "worker",
		"state":          string(sessionpkg.StateAsleep),
		"sleep_reason":   "killed",
		"session_origin": "manual",
	})

	sp := newDeadRuntimeArtifactProvider()
	sp.visible[name] = true
	sp.dead[name] = true

	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	var stderr bytes.Buffer
	cleanupDeadRuntimeSessionCorpses(store, nil, nil, newSessionBeadSnapshot([]beads.Bead{stored}), nil, sp, nil, &stderr)

	if sp.stopCalls[name] != 1 {
		t.Fatalf("corpse Stop calls = %d, want 1; stderr=%q", sp.stopCalls[name], stderr.String())
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read session bead after cleanup: %v", err)
	}
	if after.Status != "open" || after.Metadata["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("killed asleep row after corpse cleanup = status %q state %q (close_reason=%q), want an untouched open asleep row",
			after.Status, after.Metadata["state"], after.Metadata["close_reason"])
	}
}

// TestCleanupDeadRuntimeSessionCorpsesLeavesARebindingNameAlone is the second
// shape the campaign found, and the one a lifecycle guard alone cannot catch.
// A restart rotates the row's instance token at the pre-wake commit and only
// then starts the new incarnation, so between those two the row reads awake —
// it CLAIMS a live runtime — while the name still carries the previous
// incarnation's corpse. The pass then reaped the corpse and closed the row that
// had already been handed to the restart, and the async start result came back
// to a closed row ("ignoring stale async start result").
//
// The corpse's own identity is the fence, exactly as the keyed stop proves the
// instance token AT the destructive boundary (council R1): a runtime whose
// GC_INSTANCE_TOKEN is not the row's belongs to an incarnation this row has
// already left behind, and its death proves nothing about the row.
func TestCleanupDeadRuntimeSessionCorpsesLeavesARebindingNameAlone(t *testing.T) {
	const name = "worker-adhoc-2c4fa75693"
	store, bead := newDeadRuntimeCorpseStore(t, map[string]string{
		"session_name":   name,
		"template":       "worker",
		"state":          string(sessionpkg.StateAwake),
		"state_reason":   "creation_complete",
		"session_origin": "manual",
		"instance_token": "token-after-the-pre-wake-commit",
	})

	sp := newDeadRuntimeArtifactProvider()
	sp.visible[name] = true
	sp.dead[name] = true
	if err := sp.SetMeta(name, "GC_INSTANCE_TOKEN", "token-of-the-killed-incarnation"); err != nil {
		t.Fatalf("stamp corpse instance token: %v", err)
	}

	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	var stderr bytes.Buffer
	cleanupDeadRuntimeSessionCorpses(store, nil, nil, newSessionBeadSnapshot([]beads.Bead{stored}), nil, sp, nil, &stderr)

	if sp.stopCalls[name] != 0 {
		t.Fatalf("Stop calls on a re-binding name = %d, want 0; stderr=%q", sp.stopCalls[name], stderr.String())
	}
	after, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read session bead after cleanup: %v", err)
	}
	if after.Status != "open" {
		t.Fatalf("restarting row after corpse cleanup = status %q state %q, want the row left to its restart",
			after.Status, after.Metadata["state"])
	}
}

// TestCleanupDeadRuntimeSessionCorpsesStillClosesARowClaimingALiveRuntime is
// the teeth: the #2437 case is a row that says it is RUNNING while its runtime
// is a corpse. That row still loses its alias, which is the whole point of the
// pass.
func TestCleanupDeadRuntimeSessionCorpsesStillClosesARowClaimingALiveRuntime(t *testing.T) {
	for _, state := range []sessionpkg.State{sessionpkg.StateActive, sessionpkg.StateAwake} {
		t.Run(string(state), func(t *testing.T) {
			const name = "worker-1"
			store, bead := newDeadRuntimeCorpseStore(t, map[string]string{
				"session_name": name,
				"template":     "worker",
				"state":        string(state),
			})

			sp := newDeadRuntimeArtifactProvider()
			sp.visible[name] = true
			sp.dead[name] = true

			stored, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("read session bead: %v", err)
			}
			var stderr bytes.Buffer
			cleaned := cleanupDeadRuntimeSessionCorpses(store, nil, nil, newSessionBeadSnapshot([]beads.Bead{stored}), nil, sp, nil, &stderr)
			if cleaned != 1 || sp.stopCalls[name] != 1 {
				t.Fatalf("cleanup = %d (stops=%d), want the corpse reaped; stderr=%q", cleaned, sp.stopCalls[name], stderr.String())
			}
			after, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("read session bead after cleanup: %v", err)
			}
			if after.Status != "closed" || after.Metadata["state"] != "dead-runtime" {
				t.Fatalf("row claiming a live runtime after corpse cleanup = status %q state %q, want a dead-runtime close",
					after.Status, after.Metadata["state"])
			}
		})
	}
}
