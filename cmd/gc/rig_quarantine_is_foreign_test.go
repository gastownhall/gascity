//go:build !windows

package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// A quarantined rig's roster is unknown, not empty: an identity on it gets
// "cannot answer" even when a local session record would vouch for it, so the
// orphan sweep protects that rig's claims the way it protects the whole city
// when the config does not load at all. A healthy rig's identity still
// resolves from its roster.
func TestAgentIsForeignQuarantinedRigIdentityIsUnknown(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	c := newRigQuarantineCity(t)
	store, err := openStoreAtForCityWithConfig(c.cityDir, c.cityDir, nil)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:    "session record",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "bad/worker"},
	})
	if err != nil {
		t.Fatalf("seed session record: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("close session record: %v", err)
	}

	code, out, raw, stderr := runIsForeign(t, isForeignArgs(c.cityDir, "bad/worker")...)
	if code != agentIsForeignExitUnknown {
		t.Fatalf("is-foreign bad/worker exit = %d, want %d\nstdout: %s\nstderr: %s", code, agentIsForeignExitUnknown, raw, stderr)
	}
	if out.Verdict != agentIsForeignVerdictUnknown || out.Reason != string(claimReasonRigQuarantined) {
		t.Fatalf("verdict/reason = %q/%q, want %q/%q (raw: %s)", out.Verdict, out.Reason, agentIsForeignVerdictUnknown, claimReasonRigQuarantined, raw)
	}

	code, out, raw, stderr = runIsForeign(t, isForeignArgs(c.cityDir, "good/worker")...)
	if code != agentIsForeignExitLocal {
		t.Fatalf("is-foreign good/worker exit = %d, want %d\nstdout: %s\nstderr: %s", code, agentIsForeignExitLocal, raw, stderr)
	}
	if out.Reason != string(claimReasonRoster) {
		t.Errorf("good/worker reason = %q, want %q", out.Reason, claimReasonRoster)
	}
}
