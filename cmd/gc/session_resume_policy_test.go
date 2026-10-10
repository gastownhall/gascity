package main

import (
	"bytes"
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionSubmitResumesHeldSession: `gc session submit` is an
// operator's own send (CONTRACT v5.9 D8 rule 1), so on a held session it
// resumes rather than queues. Kills the CLI submitting with the background
// policy.
func TestCmdSessionSubmitResumesHeldSession(t *testing.T) {
	const sessionName = "s-gc-submit-held"
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
	if code := cmdSessionSubmit([]string{killPokeSessionIdentity, "hello"}, sessionpkg.SubmitIntentDefault, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionSubmit = %d; stderr=%s", code, stderr.String())
	}
	final := mustGetBead(t, store, bead.ID)
	if final.Metadata["state"] != string(sessionpkg.StateActive) || !inner.IsRunning(sessionName) {
		t.Fatalf("after submit: row %v running=%v, want the held session resumed", final.Metadata, inner.IsRunning(sessionName))
	}
}
