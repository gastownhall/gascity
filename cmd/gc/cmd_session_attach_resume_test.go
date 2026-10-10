package main

import (
	"bytes"
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionAttachResumesHeldSession: `gc session attach` on a
// session an operator suspended resumes it and consumes the hold (D8 B3),
// entering through the CLI so the production Actor is the one under test.
func TestCmdSessionAttachResumesHeldSession(t *testing.T) {
	const sessionName = "s-gc-attach-held"
	store, bead, _ := newKillPokeSession(t, sessionName)
	inner := wrapKillPokeProvider(t, &killHookProvider{})
	if err := inner.Stop(sessionName); err != nil {
		t.Fatal(err)
	}
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{
		"state":        string(sessionpkg.StateSuspended),
		"held_until":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"sleep_intent": "user-hold",
		"command":      "echo",
	})
	var stdout, stderr bytes.Buffer
	if code := cmdSessionAttach([]string{killPokeSessionIdentity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionAttach = %d; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	if final.Metadata["state"] != string(sessionpkg.StateActive) || final.Metadata["sleep_intent"] != "" || !inner.IsRunning(sessionName) {
		t.Fatalf("after attach: row %v running=%v, want the held session resumed and its hold consumed", final.Metadata, inner.IsRunning(sessionName))
	}
}
