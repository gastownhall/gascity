package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestGcBdPassesCanonicalSessionActorForFreshClaim(t *testing.T) {
	actorCapture := filepath.Join(t.TempDir(), "actor")
	managedDoltTestSetup(t, `#!/bin/sh
set -eu
printf '%s' "${BEADS_ACTOR:-}" > "${ACTOR_CAPTURE}"
printf '{"id":"demo-abc","status":"in_progress"}\n'
`)
	t.Setenv("ACTOR_CAPTURE", actorCapture)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "gcg-session-abc123")
	t.Setenv("GC_SESSION_NAME", "demo--mechanic-4-pool")
	t.Setenv("GC_AGENT", "demo--mechanic-4-pool")
	t.Setenv("BEADS_ACTOR", "demo--mechanic-4-pool")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"update", "demo-abc", "--claim"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd claim = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	got, err := os.ReadFile(actorCapture)
	if err != nil {
		t.Fatalf("read captured actor: %v", err)
	}
	if strings.TrimSpace(string(got)) != "gcg-session-abc123" {
		t.Fatalf("bd received BEADS_ACTOR=%q, want durable session id", strings.TrimSpace(string(got)))
	}
}

func TestProjectBdActorForMutationUsesOwnedIdentitySet(t *testing.T) {
	const (
		sessionID = "gcg-session-abc123"
		slot      = "city--mechanic-4-pool"
	)
	identities := []string{sessionID, slot}

	tests := []struct {
		name         string
		args         []string
		ambientActor string
		claimActor   string
		targets      map[string]beads.Bead
		want         string
	}{
		{
			name:         "fresh claim uses the canonical hook actor",
			args:         []string{"update", "work-1", "--claim"},
			ambientActor: slot,
			claimActor:   sessionID,
			targets:      map[string]beads.Bead{"work-1": {ID: "work-1"}},
			want:         sessionID,
		},
		{
			name:         "close projects the owned session identity",
			args:         []string{"close", "work-1"},
			ambientActor: slot,
			targets:      map[string]beads.Bead{"work-1": {ID: "work-1", Assignee: sessionID}},
			want:         sessionID,
		},
		{
			name:         "claim keeps an owned slot spelling for adoption",
			args:         []string{"update", "work-1", "--claim"},
			ambientActor: slot,
			claimActor:   sessionID,
			targets:      map[string]beads.Bead{"work-1": {ID: "work-1", Assignee: slot}},
			want:         slot,
		},
		{
			name:         "heartbeat projects the owned session identity",
			args:         []string{"heartbeat", "work-1"},
			ambientActor: slot,
			targets:      map[string]beads.Bead{"work-1": {ID: "work-1", Assignee: sessionID}},
			want:         sessionID,
		},
		{
			name:         "foreign owner stays foreign",
			args:         []string{"close", "work-1"},
			ambientActor: slot,
			targets:      map[string]beads.Bead{"work-1": {ID: "work-1", Assignee: "other-session"}},
			want:         slot,
		},
		{
			name:         "mixed owned identities do not impersonate a batch",
			args:         []string{"close", "work-1", "work-2"},
			ambientActor: slot,
			targets: map[string]beads.Bead{
				"work-1": {ID: "work-1", Assignee: sessionID},
				"work-2": {ID: "work-2", Assignee: slot},
			},
			want: slot,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectBdActorForMutation(tc.args, tc.ambientActor, tc.claimActor, identities, tc.targets); got != tc.want {
				t.Fatalf("projectBdActorForMutation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBdSessionIdentityCandidatesPreserveNamedAndPoolForms(t *testing.T) {
	env := []string{
		"GC_SESSION_ID=gcg-session-abc123",
		"GC_SESSION_NAME=city--mechanic-4-pool",
		"GC_ALIAS=",
		"GC_AGENT=city--mechanic-4-pool",
		"BEADS_ACTOR=city--mechanic-4-pool",
	}
	got := bdSessionIdentityCandidates(env)
	for _, want := range []string{
		"gcg-session-abc123",
		"city--mechanic-4-pool",
	} {
		if !containsString(got, want) {
			t.Fatalf("bdSessionIdentityCandidates() = %q, missing %q", got, want)
		}
	}

	named := []string{
		"GC_SESSION_ID=gcg-session-mayor",
		"GC_SESSION_NAME=city--mayor",
		"GC_ALIAS=city/mayor",
		"GC_AGENT=city/mayor",
		"BEADS_ACTOR=city/mayor",
	}
	if got := bdSessionIdentityCandidates(named); !containsString(got, "city/mayor") {
		t.Fatalf("named bdSessionIdentityCandidates() = %q, missing configured alias", got)
	}
}
