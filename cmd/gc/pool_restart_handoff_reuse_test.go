package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// restartHandedOffPoolInfo is a persistent pool row exactly as the restart
// finalize leaves it: restartHandoff's patch (RestartRequestPatchAt plus
// state=asleep) over an awake row, so no sleep_reason and no slept_at.
func restartHandedOffPoolInfo(t *testing.T, template, sessionName string, now time.Time) session.Info {
	t.Helper()
	patch := restartHandoff("", true, finalizeFacts{Now: now})
	return session.Info{
		ID:                       "session-restart-handoff",
		Template:                 template,
		SessionNameMetadata:      sessionName,
		PoolManaged:              true,
		PoolSlot:                 "1",
		MetadataState:            patch["state"],
		ContinuationResetPending: patch["continuation_reset_pending"],
		ResetCommittedAt:         patch[session.ResetCommittedAtKey],
	}
}

// TestReusablePoolSessionInfo_PersistentRestartHandoffIsReusable pins the
// predicate half of the post-restart dispatcher outage: a supervisor restart
// stops a min_active_sessions=1 pool row through the restart handoff, which
// parks it asleep with its continuation reset committed and stamps no
// sleep_reason. Rejecting it as an ordinary asleep row left the pool with no
// reusable row and a fresh create that could not take the runtime name the
// row still held, so the pool stalled until the session sweep closed it.
func TestReusablePoolSessionInfo_PersistentRestartHandoffIsReusable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 16, 18, 12, 37, 0, time.UTC)
	template := "myrig/core.control-dispatcher"
	cfgAgent := &config.Agent{Name: "core.control-dispatcher", Dir: "myrig"}
	handedOff := restartHandedOffPoolInfo(t, template, "core__control-dispatcher-gc-1", now)

	if !reusablePoolSessionInfo(&agentBuildParams{}, cfgAgent, template, handedOff, nil) {
		t.Fatalf("reusablePoolSessionInfo(persistent, restart handoff) = false; want true: " +
			"the reset-pending wake is the only way back for this row")
	}

	// Control: a persistent asleep row with no reset pending keeps today's
	// behavior and is never reused.
	plainAsleep := handedOff
	plainAsleep.ContinuationResetPending = ""
	plainAsleep.ResetCommittedAt = ""
	if reusablePoolSessionInfo(&agentBuildParams{}, cfgAgent, template, plainAsleep, nil) {
		t.Fatalf("reusablePoolSessionInfo(persistent, asleep, no reset pending) = true; want false")
	}

	// Control: a fresh-wake drain completion also sets the reset marker, but
	// it is a sleep (sleep_reason, slept_at, no reset_committed_at) and stays
	// under the ordinary asleep rules.
	drainedFresh := plainAsleep
	drainedFresh.ContinuationResetPending = "true"
	drainedFresh.SleepReason = "idle"
	drainedFresh.SleptAt = now.Format(time.RFC3339)
	if reusablePoolSessionInfo(&agentBuildParams{}, cfgAgent, template, drainedFresh, nil) {
		t.Fatalf("reusablePoolSessionInfo(persistent, fresh-wake drain sleep) = true; want false")
	}

	// Control: a sleep reason stamped after the handoff (a quarantine on a
	// failed wake) puts the row back under the ordinary asleep rules.
	quarantined := handedOff
	quarantined.SleepReason = "quarantine"
	if reusablePoolSessionInfo(&agentBuildParams{}, cfgAgent, template, quarantined, nil) {
		t.Fatalf("reusablePoolSessionInfo(persistent, restart handoff then quarantine) = true; want false")
	}

	// Control: a handed-off row that still holds assigned work is not taken
	// by the generic reuse path.
	withWork := &agentBuildParams{assignedWorkBeads: []beads.Bead{{
		ID:       "work-in-progress",
		Status:   "in_progress",
		Assignee: handedOff.SessionNameMetadata,
	}}}
	if reusablePoolSessionInfo(withWork, cfgAgent, template, handedOff, nil) {
		t.Fatalf("reusablePoolSessionInfo(persistent, restart handoff, assigned work) = true; want false")
	}
}

// TestBuildDesiredState_MinFloorCanonicalPoolRowAfterRestartHandoffIsReused is
// the end-to-end half: the next tick after the restart handoff keeps the same
// row in the desired set, so the awake set's reset-pending arm starts it, and
// the canonical name is not read as held by a stalled slot.
func TestBuildDesiredState_MinFloorCanonicalPoolRowAfterRestartHandoffIsReused(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	now := time.Now().UTC()
	row, err := store.Create(beads.Bead{
		Title:  "myrig/control-dispatcher",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:myrig/control-dispatcher", "template:myrig/control-dispatcher"},
		Metadata: map[string]string{
			"template":             "myrig/control-dispatcher",
			"agent_name":           "myrig/control-dispatcher",
			"alias":                "myrig/control-dispatcher",
			"session_name":         "control-dispatcher-canonical",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range restartHandoff("", true, finalizeFacts{Now: now.Add(-time.Minute)}) {
		if err := store.SetMetadata(row.ID, k, v); err != nil {
			t.Fatalf("SetMetadata(%s, %s): %v", row.ID, k, err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "control-dispatcher",
			Dir:               "myrig",
			StartCommand:      "true",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(1),
		}},
	}

	var stderr bytes.Buffer
	for tick := 1; tick <= 2; tick++ {
		got := buildDesiredState("test-city", cityPath, now, cfg, runtime.NewFake(), store, &stderr)
		if _, ok := got.State["control-dispatcher-canonical"]; !ok {
			t.Fatalf("tick %d: desired state missing the restart-handed-off row; keys=%v stderr=%q", tick, mapKeys(got.State), stderr.String())
		}
	}
	if bytes.Contains(stderr.Bytes(), []byte("slot stalled on its own runtime name")) {
		t.Fatalf("stderr = %q, want no stalled-slot line for a restart-handed-off row", stderr.String())
	}
	sessionBeads, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	if len(sessionBeads) != 1 {
		t.Fatalf("session beads = %d, want 1: the row is reused, not replaced", len(sessionBeads))
	}
}
