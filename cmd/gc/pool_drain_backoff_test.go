package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

func TestPoolDrainBackoff_AccumulatesZeroClaimDrainsAndFires(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	for i := 0; i < drainBackoffThreshold-1; i++ {
		p.RecordDrainAck(template, false)
	}
	if suppress, _ := p.Evaluate(template, false); suppress {
		t.Fatalf("Evaluate after %d zero-claim drains: got suppress=true, want false (below threshold)", drainBackoffThreshold-1)
	}

	p.RecordDrainAck(template, false) // now at threshold
	suppress, until := p.Evaluate(template, false)
	if !suppress {
		t.Fatalf("Evaluate at threshold: got suppress=false, want true")
	}
	expectedUntil := clk.Now().Add(drainBackoffBase)
	if !until.Equal(expectedUntil) {
		t.Fatalf("Evaluate at threshold: got until=%v, want %v (now+base)", until, expectedUntil)
	}
}

func TestPoolDrainBackoff_Q6RateConditionSuppressesRaceLosers(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	// Simulate a claim race: one worker won, four lost. Race losers all
	// drain-ack with claim_count=0; the winner consumed the work.
	for i := 0; i < drainBackoffThreshold+1; i++ {
		p.RecordDrainAck(template, false)
	}

	// Q6 fires: pool-wide there WAS a claim in the window. Detector must
	// not suppress, and state should reset so it doesn't stay armed.
	suppress, _ := p.Evaluate(template, true)
	if suppress {
		t.Fatalf("Evaluate with hasRecentPoolClaim=true: got suppress=true, want false (Q6 rate condition)")
	}

	// State reset: a fresh threshold's worth of zero-claim drains must be
	// required to fire again.
	for i := 0; i < drainBackoffThreshold-1; i++ {
		p.RecordDrainAck(template, false)
	}
	if suppress, _ := p.Evaluate(template, false); suppress {
		t.Fatalf("Evaluate after reset + below-threshold drains: got suppress=true, want false")
	}
}

func TestPoolDrainBackoff_ExponentialSchedule(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	want := []time.Duration{
		drainBackoffBase,                   // streak 1: 30s
		drainBackoffBase * 2,               // streak 2: 60s
		drainBackoffBase * 4,               // streak 3: 120s
		drainBackoffBase * 8,               // streak 4: 240s
		drainBackoffCap,                    // streak >=5: capped at 5min
	}

	for i, expectedBackoff := range want {
		// Drive enough zero-claim drains to (re-)cross threshold.
		for j := 0; j < drainBackoffThreshold; j++ {
			p.RecordDrainAck(template, false)
		}
		suppress, until := p.Evaluate(template, false)
		if !suppress {
			t.Fatalf("escalation #%d: got suppress=false, want true", i+1)
		}
		got := until.Sub(clk.Now())
		if got != expectedBackoff {
			t.Fatalf("escalation #%d: got backoff=%v, want %v", i+1, got, expectedBackoff)
		}
		// Skip past this backoff so the next iteration can escalate again.
		clk.Advance(expectedBackoff + time.Second)
	}
}

func TestPoolDrainBackoff_StaleDrainsAreDropped(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	// Drain-acks just outside the window must not count toward threshold.
	for i := 0; i < drainBackoffThreshold; i++ {
		p.RecordDrainAck(template, false)
	}
	clk.Advance(drainBackoffWindow + time.Second)
	if suppress, _ := p.Evaluate(template, false); suppress {
		t.Fatalf("Evaluate after window expiry: got suppress=true, want false (stale drain-acks should not fire)")
	}
}

func TestPoolDrainBackoff_HadClaimsIsIgnored(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	// hadClaims=true means the session did productive work — never count.
	for i := 0; i < drainBackoffThreshold*3; i++ {
		p.RecordDrainAck(template, true)
	}
	if suppress, _ := p.Evaluate(template, false); suppress {
		t.Fatalf("Evaluate after only hadClaims=true drains: got suppress=true, want false")
	}
}

func TestPoolDrainBackoff_NoteClaimResetsBackoff(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)}
	p := newPoolDrainBackoff(clk)
	template := "foundations/worker"

	for i := 0; i < drainBackoffThreshold; i++ {
		p.RecordDrainAck(template, false)
	}
	if suppress, _ := p.Evaluate(template, false); !suppress {
		t.Fatal("setup: expected suppression after threshold drains")
	}

	// Observed claim resets state.
	p.NoteClaim(template)

	// Even with hasRecentPoolClaim=false at evaluate time, NoteClaim has
	// already cleared the streak/until/window — so we're back to idle.
	if suppress, until := p.Evaluate(template, false); suppress {
		t.Fatalf("Evaluate after NoteClaim: got suppress=true (until=%v), want false", until)
	}
}

func TestPoolDrainBackoff_NilSafe(t *testing.T) {
	var p *poolDrainBackoff
	p.RecordDrainAck("x", false)
	p.NoteClaim("x")
	if suppress, _ := p.Evaluate("x", false); suppress {
		t.Fatal("nil receiver: Evaluate should return suppress=false")
	}
}
