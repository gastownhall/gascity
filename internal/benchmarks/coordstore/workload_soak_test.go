package coordstore

import (
	"math"
	"testing"
	"time"
)

func TestSoakConfigScaledWorkload(t *testing.T) {
	base := WorkloadConfig{
		Name:            "tiny",
		MainOpenCount:   10,
		MainClosedCount: 20,
		WispOpenCount:   30,
		DepEdgeCount:    3,
		MailPollRate:    1.5,
		PointReadRate:   2.5,
		FilterScanRate:  0.25,
		CreateRate:      0.5,
		UpdateRate:      0.75,
		SetMetadataRate: 1.25,
		BatchGetRate:    1.75,
		ReadyRate:       0.2,
		DepOpRate:       0.1,
		RecentScanRate:  0.05,
		Duration:        5 * time.Second,
		Concurrency:     4,
	}
	cfg := SoakConfig{
		SoakPhase:      SoakPhaseA,
		SoakDuration:   time.Minute,
		KillCadence:    10 * time.Second,
		SampleInterval: 250 * time.Millisecond,
		ResultsDir:     "/tmp/coordstore-soak-test",
		ScaleFactor:    2.5,
	}

	got := cfg.ScaledWorkload(base)

	if got.Name != base.Name {
		t.Fatalf("Name = %q, want %q", got.Name, base.Name)
	}
	if got.Duration != cfg.SoakDuration {
		t.Fatalf("Duration = %s, want %s", got.Duration, cfg.SoakDuration)
	}
	if got.MainOpenCount != 25 || got.MainClosedCount != 50 || got.WispOpenCount != 75 || got.DepEdgeCount != 8 {
		t.Fatalf("scaled counts = main open %d closed %d wisps %d deps %d",
			got.MainOpenCount, got.MainClosedCount, got.WispOpenCount, got.DepEdgeCount)
	}
	if got.Concurrency != 10 {
		t.Fatalf("Concurrency = %d, want 10", got.Concurrency)
	}
	if !nearly(got.MailPollRate, 3.75) ||
		!nearly(got.PointReadRate, 6.25) ||
		!nearly(got.FilterScanRate, 0.625) ||
		!nearly(got.CreateRate, 1.25) ||
		!nearly(got.UpdateRate, 1.875) ||
		!nearly(got.SetMetadataRate, 3.125) ||
		!nearly(got.BatchGetRate, 4.375) ||
		!nearly(got.ReadyRate, 0.5) ||
		!nearly(got.DepOpRate, 0.25) ||
		!nearly(got.RecentScanRate, 0.125) {
		t.Fatalf("scaled rates = %#v", got)
	}
	if base.Duration != 5*time.Second || base.MainOpenCount != 10 {
		t.Fatalf("ScaledWorkload mutated base: %#v", base)
	}
}

func TestSoakConfigScaledWorkloadDefaultsScaleToOne(t *testing.T) {
	base := WorkloadConfig{
		Name:          "default-scale",
		MainOpenCount: 3,
		CreateRate:    0.5,
		Duration:      time.Second,
		Concurrency:   2,
	}
	got := (SoakConfig{}).ScaledWorkload(base)

	if got.MainOpenCount != base.MainOpenCount || got.CreateRate != base.CreateRate {
		t.Fatalf("zero ScaleFactor should preserve workload, got %#v from %#v", got, base)
	}
	if got.Duration != base.Duration {
		t.Fatalf("zero SoakDuration should preserve duration, got %s want %s", got.Duration, base.Duration)
	}
	if got.Concurrency != base.Concurrency {
		t.Fatalf("Concurrency = %d, want %d", got.Concurrency, base.Concurrency)
	}
}

func nearly(got, want float64) bool {
	return math.Abs(got-want) < 0.000001
}
