package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionKill_UnknownStateRowNamesTheClose is issue #1497's kill half:
// an unnamed row in a state the controller skips, whose runtime a fresh
// liveness read confirms gone, still answers "session is not active" (the
// kill semantics are unchanged — there is no runtime to kill), but the same
// stderr line now names the raw state and the close that releases the row.
// A known-state row with a gone runtime keeps the bare message.
func TestCmdSessionKill_UnknownStateRowNamesTheClose(t *testing.T) {
	store, _, _ := newKillPokeSession(t, "s-kill-unknown-state-fixture")
	stubKillPoke(t)
	// The killHookProvider's liveness read is fresh, as tmux's is, so the
	// kill can confirm the unnamed row's never-started runtime gone.
	inner := wrapKillPokeProvider(t, &killHookProvider{})

	for name, tc := range map[string]struct {
		state string
	}{
		"unknown state": {state: "future-state"},
		"known state":   {state: "asleep"},
	} {
		t.Run(name, func(t *testing.T) {
			bead, err := store.Create(beads.Bead{
				Title:  "unnamed session",
				Type:   sessionpkg.BeadType,
				Labels: []string{sessionpkg.LabelSession, "template:worker"},
				Metadata: map[string]string{
					"template":     "worker",
					"session_name": "s-kill-unknown-state-" + tc.state,
					"state":        tc.state,
					"generation":   "3",
				},
			})
			if err != nil {
				t.Fatalf("store.Create(session bead): %v", err)
			}
			if inner.IsRunning(bead.Metadata["session_name"]) {
				t.Fatalf("premise: a runtime is running under %q", bead.Metadata["session_name"])
			}

			var stdout, stderr bytes.Buffer
			if code := cmdSessionKill([]string{bead.ID}, &stdout, &stderr); code != 1 {
				t.Fatalf("cmdSessionKill = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "session is not active") {
				t.Errorf("stderr does not keep the not-active refusal:\n%s", stderr.String())
			}
			if tc.state == "future-state" {
				if !strings.Contains(stderr.String(), `"future-state"`) {
					t.Errorf("stderr does not name the raw state:\n%s", stderr.String())
				}
				if !strings.Contains(stderr.String(), "gc session close "+bead.ID) {
					t.Errorf("stderr does not name the close that releases the row:\n%s", stderr.String())
				}
			} else if strings.Contains(stderr.String(), "controller manages") {
				t.Errorf("a known-state row's message changed:\n%s", stderr.String())
			}
		})
	}
}
