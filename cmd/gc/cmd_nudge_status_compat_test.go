package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestListQueuedNudgesStatusConsumerReadsAllBucketsForExactAgent(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().UTC()

	seedNudgeQueueState(t, dir, nudgeQueueState{
		Pending: []queuedNudge{
			statusCompatPendingNudge("n-worker-pending", "worker", now),
			statusCompatPendingNudge("n-other-pending", "other", now),
		},
		InFlight: []queuedNudge{
			statusCompatInFlightNudge("n-worker-inflight", "worker", now),
			statusCompatInFlightNudge("n-other-inflight", "other", now),
		},
		Dead: []queuedNudge{
			statusCompatDeadNudge("n-worker-dead", "worker", now),
			statusCompatDeadNudge("n-other-dead", "other", now),
		},
	})

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}

	assertQueuedNudgeIDs(t, pending, "n-worker-pending")
	assertQueuedNudgeIDs(t, inFlight, "n-worker-inflight")
	assertQueuedNudgeIDs(t, dead, "n-worker-dead")

	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 2 || len(state.InFlight) != 2 || len(state.Dead) != 2 {
		t.Fatalf("status list mutated nonterminal queue state: pending=%d inFlight=%d dead=%d, want 2/2/2",
			len(state.Pending), len(state.InFlight), len(state.Dead))
	}
}

func TestListQueuedNudgesForTargetStatusConsumerMatchesTargetKeys(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().UTC()

	seedNudgeQueueState(t, dir, nudgeQueueState{
		Pending: []queuedNudge{
			statusCompatPendingNudge("n-current-alias", "sky", now),
			statusCompatPendingNudge("n-session-id", "gc-1", now),
			statusCompatPendingNudge("n-unrelated-pending", "other", now),
		},
		InFlight: []queuedNudge{
			statusCompatInFlightNudge("n-qualified-agent", "myrig/witness", now),
			statusCompatInFlightNudge("n-unrelated-inflight", "other", now),
		},
		Dead: []queuedNudge{
			statusCompatDeadNudge("n-old-alias", "mayor", now),
			statusCompatDeadNudge("n-unrelated-dead", "other", now),
		},
	})

	target := nudgeTarget{
		alias:        "sky",
		aliasHistory: []string{"mayor"},
		sessionID:    "gc-1",
		identity:     "myrig/witness",
		agent:        config.Agent{Name: "witness", Dir: "myrig"},
		sessionName:  "sess-sky",
	}
	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, now)
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}

	assertQueuedNudgeIDs(t, pending, "n-current-alias", "n-session-id")
	assertQueuedNudgeIDs(t, inFlight, "n-qualified-agent")
	assertQueuedNudgeIDs(t, dead, "n-old-alias")
}

func TestCmdNudgeStatusTableOutputShape(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	now := time.Now().Add(-time.Minute)
	item := newQueuedNudgeWithOptions("mayor", "review queued work", "session", now, queuedNudgeOptions{ID: "n-status-table"})
	if err := enqueueQueuedNudge(cityDir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdNudgeStatus([]string{"mayor"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdNudgeStatus = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"AGENT",
		"PENDING",
		"IN_FLIGHT",
		"DEAD",
		"SESSION",
		"mayor",
		"pending  n-status-table",
		"review queued work",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status table missing %q:\n%s", want, out)
		}
	}
}

func TestClaimDueQueuedNudgesMatchingKeepsPollerMaintenanceUnbounded(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().UTC()

	var state nudgeQueueState
	for i := 0; i < 20; i++ {
		pending := statusCompatPendingNudge(fmt.Sprintf("n-expired-pending-%02d", i), "worker", now)
		pending.ExpiresAt = now.Add(-time.Hour)
		state.Pending = append(state.Pending, pending)

		inFlight := statusCompatInFlightNudge(fmt.Sprintf("n-expired-inflight-%02d", i), "worker", now)
		inFlight.ExpiresAt = now.Add(-time.Hour)
		inFlight.LeaseUntil = now.Add(-time.Hour)
		state.InFlight = append(state.InFlight, inFlight)
	}
	seedNudgeQueueState(t, dir, state)

	// Poller/dispatcher maintenance is intentionally unbounded: unlike
	// foreground status reads, non-foreground callers own convergence and must
	// drain the whole expired backlog they choose to inspect.
	claimed, err := claimDueQueuedNudgesMatching(dir, now, noMaintenanceDeadline(), func(queuedNudge) bool { return false })
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesMatching: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed = %d, want 0", len(claimed))
	}

	updated, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(updated.Pending) != 0 || len(updated.InFlight) != 0 {
		t.Fatalf("expired backlog not fully drained: pending=%d inFlight=%d", len(updated.Pending), len(updated.InFlight))
	}
	if len(updated.Dead) != 40 {
		t.Fatalf("dead = %d, want 40 expired items", len(updated.Dead))
	}
}

func seedNudgeQueueState(t *testing.T, dir string, state nudgeQueueState) {
	t.Helper()
	if err := nudgequeue.WithState(dir, func(current *nudgeQueueState) error {
		*current = state
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
}

func statusCompatPendingNudge(id, agent string, now time.Time) queuedNudge {
	item := newQueuedNudgeWithOptions(agent, id, "session", now.Add(-time.Minute), queuedNudgeOptions{ID: id})
	item.DeliverAfter = now.Add(-time.Minute).UTC()
	item.ExpiresAt = now.Add(time.Hour).UTC()
	return item
}

func statusCompatInFlightNudge(id, agent string, now time.Time) queuedNudge {
	item := statusCompatPendingNudge(id, agent, now)
	item.ClaimedAt = now.Add(-30 * time.Second).UTC()
	item.LeaseUntil = now.Add(time.Minute).UTC()
	return item
}

func statusCompatDeadNudge(id, agent string, now time.Time) queuedNudge {
	item := statusCompatPendingNudge(id, agent, now)
	item.LastError = "dead-letter"
	item.DeadAt = now.Add(-time.Minute).UTC()
	return item
}

func assertQueuedNudgeIDs(t *testing.T, got []queuedNudge, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("nudge count = %d, want %d; got=%#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("nudge IDs = %#v, want %#v", queuedNudgeIDs(got), want)
		}
	}
}
