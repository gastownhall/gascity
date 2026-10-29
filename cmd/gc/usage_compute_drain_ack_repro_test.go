package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// TestDrainAckClosedSessionStillEmitsComputeFact reproduces issue #6672: a pool
// session that drain-acks with no assigned work is closed straight from active by
// finalizeDrainAckStoppedSession. Its awake interval must still be accounted (one
// compute fact) by the reconcile tick's usage pass that follows the close.
func TestDrainAckClosedSessionStillEmitsComputeFact(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	b := env.createSessionBead("worker", "worker")
	start := env.clk.Now().UTC().Add(-10 * time.Minute)
	env.setSessionMetadata(&b, map[string]string{
		"state":            "active",
		"session_key":      "01a0d2d9-ab82-7253-80d2-9442255bce55",
		"awake_started_at": start.Format(time.RFC3339Nano),
		"provider":         "codex",
		"builtin_ancestor": "codex",
	})
	env.setSessionMetadata(&b, session.DrainAckStopPendingPatch(env.clk.Now().UTC()))
	finalizeDrainAckStoppedSession(env.cfg, env.store, testSeatWork(env.city, env.cfg, env.store, nil), env.sessionInfo(b.ID), "worker", true, newFakeDrainOps(), env.dt, env.clk, events.Discard, io.Discard)
	closed, err := env.store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" {
		t.Fatalf("precondition: drain-ack finalize left status=%q, want closed", closed.Status)
	}

	sinkPath := filepath.Join(env.city, ".gc", "usage.jsonl")
	cs := &controllerState{cityBeadStore: env.store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: env.city}
	cr := &CityRuntime{cs: cs, cfg: env.cfg, sp: env.sp, cityName: "demo", cityPath: env.city, stderr: io.Discard}
	// The next reconcile tick's usage pass, fed exactly as the tick feeds it.
	snap, err := loadSessionBeadSnapshot(env.store)
	if err != nil {
		t.Fatal(err)
	}
	cr.emitDueComputeFacts(context.Background(), snap.OpenInfos(), false)

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := kindCount(facts, usage.KindCompute); n != 1 {
		got, _ := env.store.Get(b.ID)
		t.Fatalf("compute facts for the drain-acked session = %d, want 1 (state=%q usage_compute_emitted_at=%q usage_model_swept_at=%q)",
			n, got.Metadata["state"], got.Metadata[usageComputeEmittedAtKey], got.Metadata[usageModelSweptAtKey])
	}
}
