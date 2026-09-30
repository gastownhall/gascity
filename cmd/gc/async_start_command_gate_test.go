package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestAsyncStartCommandGate_OnlyAChangeDuringStartupIsStale pins the async-start
// command-drift gate by row class. For every row, a persisted command equal to
// the prepared command or a prefix-extension of it (the worker boundary's
// augmented form, shouldPreserveStoredRuntimeCommand) is the same command and
// never stale (ga-2ygo4s). For a wake of an already-committed row (no
// pending_create_claim, state queued or creating) a difference that already
// existed at enqueue is not startup drift either: nothing on the commit path
// repairs it, so discarding would repeat every wave forever (ga-k88yuh). A
// pending create keeps the discard so the pending-create rollback
// (asyncStartDriftRollbackEligibleInfo) can release its alias, and any other
// row keeps it too.
func TestAsyncStartCommandGate_OnlyAChangeDuringStartupIsStale(t *testing.T) {
	const (
		prepared  = "claude-fallback --dangerously-skip-permissions --model claude-sonnet-5"
		augmented = prepared + ` --settings "/city/.gc/settings.json"`
		older     = "claude-fallback --dangerously-skip-permissions --model claude-sonnet-4 --effort max"
		other     = "claude-fallback --dangerously-skip-permissions --model claude-opus-5"
		creating  = string(sessionpkg.StateCreating)
		active    = string(sessionpkg.StateActive)
	)
	cases := []struct {
		name                      string
		prepared, enqueue, commit string
		claim                     bool
		state                     string
		wantStale                 bool
	}{
		{"wake, unchanged and equal", prepared, prepared, prepared, false, creating, false},
		{"wake, stale at enqueue, unchanged (ga-k88yuh)", prepared, older, older, false, creating, false},
		{"pending create, stale at enqueue, unchanged (rollback owns it)", prepared, older, older, true, creating, true},
		{"active row, stale at enqueue, unchanged", prepared, older, older, false, active, true},
		{"wake, augmented at enqueue, unchanged (ga-2ygo4s)", prepared, augmented, augmented, false, creating, false},
		{"pending create, augmented at enqueue, unchanged (ga-2ygo4s)", prepared, augmented, augmented, true, creating, false},
		{"wake, empty at enqueue, augmented at commit (ga-2ygo4s)", prepared, "", augmented, false, creating, false},
		{"wake, changed to the augmented prepared command", prepared, prepared, augmented, false, creating, false},
		{"wake, changed to the prepared command", prepared, older, prepared, false, creating, false},
		{"empty prepared command", "", prepared, other, false, creating, false},
		{"empty persisted command", prepared, older, "", false, creating, false},
		{"wake, changed during startup to a different command", prepared, prepared, other, false, creating, true},
		{"wake, changed during startup from empty to a different command", prepared, "", other, false, creating, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := preparedStart{candidate: startCandidate{
				tp:   TemplateParams{Command: tc.prepared},
				info: sessionpkg.Info{Command: tc.enqueue},
			}}
			current := sessionpkg.Info{Command: tc.commit, PendingCreateClaim: tc.claim, MetadataState: tc.state}
			if got := asyncStartPreparedCommandStaleInfo(start, current); got != tc.wantStale {
				t.Fatalf("stale = %v, want %v (prepared=%q enqueue=%q commit=%q claim=%v state=%q)", got, tc.wantStale, tc.prepared, tc.enqueue, tc.commit, tc.claim, tc.state)
			}
		})
	}
}

// TestCommitAsyncStartResult_CommitsWhenStoredCommandUnchangedDuringStartup is
// the ga-k88yuh deadlock on a wake of an already-committed row (state creating,
// no pending_create_claim, the shape production shows): the session bead's
// persisted command was a stale snapshot of an older resolution (NOT a
// prefix-extension of the current one), nothing changed it during startup, and
// every wave was discarded because the gate compared it with the freshly
// resolved command.
func TestCommitAsyncStartResult_CommitsWhenStoredCommandUnchangedDuringStartup(t *testing.T) {
	const (
		name  = "gascity--deployer-pool"
		stale = "codex-strip --dangerously-skip-permissions --model claude-sonnet-5 --dangerously-bypass-approvals-and-sandbox --model gpt-5.6-sol -c model_reasoning_effort=xhigh"
		fresh = "codex-strip --dangerously-skip-permissions --model claude-sonnet-5 --dangerously-bypass-approvals-and-sandbox --model gpt-5.5 -c model_reasoning_effort=xhigh"
	)
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 9, 24, 20, 25, 0, 0, time.UTC)}
	session, err := store.Create(beads.Bead{
		ID:     "gc-deployer",
		Title:  "gascity/deployer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":       name,
			"template":           "gascity/deployer",
			"generation":         "283",
			"continuation_epoch": "30",
			"instance_token":     "tok-deployer",
			"last_woke_at":       clk.Now().Format(time.RFC3339),
			"command":            stale,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), name, runtime.Config{Command: fresh}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GC_SESSION_ID": session.ID, "GC_INSTANCE_TOKEN": "tok-deployer", "GC_RUNTIME_EPOCH": "283"} {
		if err := sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, session),
				tp:   TemplateParams{Command: fresh, SessionName: name, TemplateName: "gascity/deployer"},
			},
			coreHash: "core-v1",
			liveHash: "live-v1",
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}

	if !commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("async start discarded although the persisted command did not change during startup")
	}
	if !sp.IsRunning(name) {
		t.Fatal("the started runtime was stopped")
	}
	updated, err := store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Metadata["started_config_hash"]; got != "core-v1" {
		t.Fatalf("started_config_hash = %q, want the committed start's core-v1", got)
	}
}
