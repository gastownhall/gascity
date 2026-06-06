package coordstore_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/authorcore"
)

// TestSteadyStateWorkloadPlateausWisps is the ga-w08fz / ga-sftyt regression
// guard. The steady-state design keeps wisp deletes ≈ wisp creates, so the
// ephemeral population PLATEAUS; with the lifecycle ops disabled (the legacy
// net-create-only workload) the ephemeral population grows without bound. A
// soak against an in-memory backend would otherwise just benchmark each
// backend's compression of an ever-growing dataset rather than real
// steady-state coordination work.
//
// Note: per the design, the MAIN tier is intentionally NOT fully plateaued
// (CloseRate ≪ create rate; tasks are long-lived) — only wisps plateau. So this
// asserts the ephemeral population, which is the high-churn tier the invariant
// targets.
func TestSteadyStateWorkloadPlateausWisps(t *testing.T) {
	if testing.Short() {
		t.Skip("timed workload; skipped in -short")
	}

	base := coordstore.WorkloadConfig{
		Name:            "steady-test",
		MainOpenCount:   200,
		MainClosedCount: 100,
		WispOpenCount:   200, // > 100 so the lifecycle floor guard lets deletes run
		// Heavy, create-dominant load so net-create-only growth is unmistakable.
		MailPollRate: 10,
		CreateRate:   200, // ≈ half land in the ephemeral tier
		Duration:     3 * time.Second,
		Concurrency:  8,
	}

	run := func(lifecycle bool) (trackerWisps, storeWisps int64) {
		wl := base
		if lifecycle {
			wl.WispDeleteRate = 100 // ≈ wisp create rate → ephemeral plateau
			wl.CloseRate = 5        // main closes (main still grows, by design)
			wl.PurgeExpiredRate = 1 // TTL sweeps
		}
		ctx := context.Background()
		a := authorcore.New()
		if err := a.Open(ctx, coordstore.Config{}); err != nil {
			t.Fatalf("open: %v", err)
		}
		seed, err := coordstore.NewSeeder(1).Seed(ctx, a, wl)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		runner := coordstore.NewRunner(a, wl, seed)
		if _, err := runner.Run(ctx, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		snap := runner.SeedSnapshot()
		return int64(len(snap.WispOpenIDs)), a.Stats(ctx)["ephemeral_records"]
	}

	const seededWisps = 200

	onTracker, onStore := run(true)
	offTracker, offStore := run(false)
	t.Logf("seeded wisps=%d | lifecycle-ON tracker=%d store=%d | create-only tracker=%d store=%d",
		seededWisps, onTracker, onStore, offTracker, offStore)

	// Plateau: with balanced deletes the ephemeral population stays near seed.
	if onStore > seededWisps*3 {
		t.Errorf("ephemeral population did not plateau with lifecycle: store=%d, want <= %d (seeded %d)",
			onStore, seededWisps*3, seededWisps)
	}
	// Doesn't drain to zero (the floor guard + create refill hold it up).
	if onTracker < 50 {
		t.Errorf("ephemeral tracker drained too far: tracker=%d (seeded %d)", onTracker, seededWisps)
	}
	// Sanity: the create-only workload must visibly grow the ephemeral tier.
	if offStore < seededWisps*4 {
		t.Errorf("create-only workload did not grow ephemeral tier enough to be meaningful: store=%d (seeded %d)",
			offStore, seededWisps)
	}
	// The whole point: lifecycle keeps the ephemeral tier far smaller.
	if onStore*2 >= offStore {
		t.Errorf("lifecycle ephemeral (%d) not meaningfully smaller than create-only (%d)",
			onStore, offStore)
	}
}
