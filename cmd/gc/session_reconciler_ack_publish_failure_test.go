package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ackAtPublishFailureProvider fails the reconciler's publication of one
// marker key and publishes a complete agent acknowledgment at the first
// removal the failed publication's cleanup issues — an agent's `gc runtime
// drain-ack` landing between the reconciler's partial write and its cleanup.
// The shape of Cherub's second source-level finding on
// gastownhall/gascity#6178 at 112dab235.
type ackAtPublishFailureProvider struct {
	runtime.Provider
	failKey string
	failed  bool
	fired   bool
	ackErr  error
	keys    []string
}

func (p *ackAtPublishFailureProvider) SetMeta(name, key, value string) error {
	if key == p.failKey && !p.failed {
		p.failed = true
		return errors.New("injected publish failure")
	}
	return p.Provider.SetMeta(name, key, value)
}

func (p *ackAtPublishFailureProvider) RemoveMeta(name, key string) error {
	p.keys = append(p.keys, key)
	if p.failed && !p.fired {
		p.fired = true
		p.ackErr = newDrainOps(p.Provider).setDrainAck(name)
		if p.ackErr != nil {
			return p.ackErr
		}
	}
	return p.Provider.RemoveMeta(name, key)
}

// TestWoodhouseReconcilerAckPublishFailureCleanupPreservesAgentAck: when the
// reconciler's ack publication fails part-way (reason or generation write),
// its cleanup must remove only the reconciler's own keys — an agent ack that
// completed before the cleanup's removal survives and still reads as an ack,
// and the partial marker never reads as a reconciler ack.
func TestWoodhouseReconcilerAckPublishFailureCleanupPreservesAgentAck(t *testing.T) {
	for _, failKey := range []string{reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
		t.Run(failKey, func(t *testing.T) {
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			provider := &ackAtPublishFailureProvider{Provider: sp, failKey: failKey}
			err := setReconcilerDrainAckMetadata(provider, "worker", &drainState{reason: "orphaned", generation: 1})
			if err == nil {
				t.Fatal("publication must report the injected failure")
			}
			if !provider.fired || provider.ackErr != nil {
				t.Fatalf("agent acknowledgment did not complete at the cleanup boundary: fired=%v error=%v removals=%v", provider.fired, provider.ackErr, provider.keys)
			}
			for _, key := range provider.keys {
				if key == "GC_DRAIN_ACK" {
					t.Fatalf("the publication-failure cleanup removed GC_DRAIN_ACK, the agent's key; removals=%v", provider.keys)
				}
			}
			if v, _ := sp.GetMeta("worker", "GC_DRAIN_ACK"); v != "1" {
				t.Fatalf("agent ack erased by the publication-failure cleanup: GC_DRAIN_ACK=%q removals=%v", v, provider.keys)
			}
			acked, err := newDrainOps(sp).isDrainAcked("worker")
			if err != nil {
				t.Fatal(err)
			}
			if !acked {
				t.Fatal("surviving agent ack does not read as an ack")
			}
			if source, _ := sp.GetMeta("worker", reconcilerDrainAckSourceKey); source == reconcilerDrainAckSourceValue {
				t.Fatalf("partial reconciler marker left behind: %s=%q", reconcilerDrainAckSourceKey, source)
			}
		})
	}
}
