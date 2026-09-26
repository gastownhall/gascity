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
// command-drift gate: a start is stale only when the persisted command changed
// between enqueue (prepared.candidate.info) and commit, and changed to something
// other than the prepared command or a prefix-extension of it (the worker
// boundary's augmented form, shouldPreserveStoredRuntimeCommand). A difference
// that already existed at enqueue cannot be repaired by discarding the start
// (ga-k88yuh, ga-2ygo4s).
func TestAsyncStartCommandGate_OnlyAChangeDuringStartupIsStale(t *testing.T) {
	const (
		prepared  = "claude-fallback --dangerously-skip-permissions --model claude-sonnet-5"
		augmented = prepared + ` --settings "/city/.gc/settings.json"`
		older     = "claude-fallback --dangerously-skip-permissions --model claude-sonnet-4 --effort max"
		other     = "claude-fallback --dangerously-skip-permissions --model claude-opus-5"
	)
	cases := []struct {
		name                      string
		prepared, enqueue, commit string
		wantStale                 bool
	}{
		{"unchanged and equal", prepared, prepared, prepared, false},
		{"stale at enqueue, unchanged (ga-k88yuh)", prepared, older, older, false},
		{"augmented at enqueue, unchanged (ga-2ygo4s)", prepared, augmented, augmented, false},
		{"empty at enqueue, augmented at commit (ga-2ygo4s)", prepared, "", augmented, false},
		{"changed to the augmented prepared command", prepared, prepared, augmented, false},
		{"changed to the prepared command", prepared, older, prepared, false},
		{"empty prepared command", "", prepared, other, false},
		{"empty persisted command", prepared, older, "", false},
		{"changed during startup to a different command", prepared, prepared, other, true},
		{"changed during startup from empty to a different command", prepared, "", other, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := preparedStart{candidate: startCandidate{
				tp:   TemplateParams{Command: tc.prepared},
				info: sessionpkg.Info{Command: tc.enqueue},
			}}
			if got := asyncStartPreparedCommandStaleInfo(start, sessionpkg.Info{Command: tc.commit}); got != tc.wantStale {
				t.Fatalf("stale = %v, want %v (prepared=%q enqueue=%q commit=%q)", got, tc.wantStale, tc.prepared, tc.enqueue, tc.commit)
			}
		})
	}
}

// TestCommitAsyncStartResult_CommitsWhenStoredCommandUnchangedDuringStartup is
// the ga-k88yuh deadlock: the session bead's persisted command was a stale
// snapshot of an older resolution (NOT a prefix-extension of the current one),
// nothing changed it during startup, and every wave was discarded because the
// gate compared it with the freshly resolved command.
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
			"session_name":         name,
			"template":             "gascity/deployer",
			"generation":           "283",
			"continuation_epoch":   "30",
			"instance_token":       "tok-deployer",
			"pending_create_claim": "true",
			"last_woke_at":         clk.Now().Format(time.RFC3339),
			"command":              stale,
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
