package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestHandoffRemoteRefusesLiveSubagentsUnlessForced(t *testing.T) {
	old := liveSubagentsForKill
	liveSubagentsForKill = func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
		return []worker.InFlightSubagent{{AgentID: "agent-42", Description: "Investigate make check slowness", StartedAt: time.Now().Add(-time.Minute)}}, nil
	}
	t.Cleanup(func() { liveSubagentsForKill = old })
	store, rec, sp := beads.NewMemStore(), events.NewFake(), runtime.NewFake()
	if err := sp.Start(context.Background(), "target", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doHandoffRemote(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want refusal", code)
	}
	if !sp.IsRunning("target") || !bytes.Contains(stderr.Bytes(), []byte("--force")) || !bytes.Contains(stderr.Bytes(), []byte("Investigate make check slowness")) || !bytes.Contains(stderr.Bytes(), []byte("agent-42")) || !bytes.Contains(stderr.Bytes(), []byte("running")) {
		t.Fatalf("refusal stderr=%q running=%v", stderr.String(), sp.IsRunning("target"))
	}
	stdout.Reset()
	stderr.Reset()
	if code := doHandoffRemoteWithForce(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, true, &stdout, &stderr); code != 0 || sp.IsRunning("target") {
		t.Fatalf("force code=%d running=%v stderr=%s", code, sp.IsRunning("target"), stderr.String())
	}
}

func TestRefuseKillForLiveSubagentsFailsOpen(t *testing.T) {
	old := liveSubagentsForKill
	t.Cleanup(func() { liveSubagentsForKill = old })
	resolverErr := errors.New("transcript unavailable")
	for _, tt := range []struct {
		name    string
		resolve sessionTargetHandleResolver
		live    func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error)
	}{
		{
			name: "target transcript cannot be resolved",
			resolve: func(string, beads.Store, runtime.Provider, *config.City, string) (worker.Handle, error) {
				return nil, resolverErr
			},
		},
		{
			name: "transcript is corrupt",
			resolve: func(string, beads.Store, runtime.Provider, *config.City, string) (worker.Handle, error) {
				return nil, nil
			},
			live: func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
				return nil, resolverErr
			},
		},
		{
			name: "no subagents",
			resolve: func(string, beads.Store, runtime.Provider, *config.City, string) (worker.Handle, error) {
				return nil, nil
			},
			live: func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
				return nil, nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.live != nil {
				liveSubagentsForKill = tt.live
			} else {
				liveSubagentsForKill = old
			}
			var out bytes.Buffer
			if refuseKillForLiveSubagents("gc session kill", tt.resolve, "", nil, nil, nil, "target", &out) {
				t.Fatalf("refuseKillForLiveSubagents() = true; output=%q", out.String())
			}
		})
	}
}

// A refusal must not leave handoff mail behind. If the mail is created before
// the guard runs, the operator's --force retry delivers a second copy.
func TestHandoffRemoteRefusalSendsNoMailAndForceSendsExactlyOne(t *testing.T) {
	old := liveSubagentsForKill
	liveSubagentsForKill = func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
		return []worker.InFlightSubagent{{AgentID: "agent-42", Description: "Long audit", StartedAt: time.Now().Add(-time.Minute)}}, nil
	}
	t.Cleanup(func() { liveSubagentsForKill = old })

	store, rec, sp := beads.NewMemStore(), events.NewFake(), runtime.NewFake()
	if err := sp.Start(context.Background(), "target", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	countMail := func() int {
		// Message beads land in the wisp tier; the default TierIssues read
		// would report a false zero regardless of what was sent.
		mail, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		return len(mail)
	}

	var stdout, stderr bytes.Buffer
	if code := doHandoffRemote(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want refusal", code)
	}
	if got := countMail(); got != 0 {
		t.Fatalf("mail after refusal = %d, want 0: a refused handoff must not send", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doHandoffRemoteWithForce(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, true, &stdout, &stderr); code != 0 {
		t.Fatalf("force code=%d stderr=%s", code, stderr.String())
	}
	if got := countMail(); got != 1 {
		t.Fatalf("mail after forced retry = %d, want exactly 1", got)
	}
}
