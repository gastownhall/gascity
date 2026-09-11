package featureflags_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
)

func boolPtr(v bool) *bool { return &v }

// restoreGlobals captures the process-global flag state and restores it after
// the test, so a test that mutates the globals cannot leak into siblings.
func restoreGlobals(t *testing.T) {
	t.Helper()
	prev := featureflags.Snapshot()
	t.Cleanup(func() { featureflags.Apply(prev) })
}

func TestFromConfigNilIsDisabled(t *testing.T) {
	// A nil config yields the all-disabled state, matching the API server's
	// historical `cfg != nil && …` nil-guard in syncFeatureFlags.
	got := featureflags.FromConfig(nil)
	if got.FormulaV2 || got.GraphApply {
		t.Fatalf("FromConfig(nil) = %+v, want all-disabled", got)
	}
}

func TestFromConfigDefaultsEnabled(t *testing.T) {
	// A non-nil config with an absent [daemon] formula_v2 is enabled by
	// default (config.DaemonConfig.FormulaV2Enabled), and both flags follow.
	got := featureflags.FromConfig(&config.City{})
	if !got.FormulaV2 || !got.GraphApply {
		t.Fatalf("FromConfig(&City{}) = %+v, want both enabled (default-on)", got)
	}
}

func TestFromConfigDerivesBothFromDaemonInLockstep(t *testing.T) {
	for _, tc := range []struct {
		name string
		v2   bool
	}{{"explicit-enabled", true}, {"explicit-disabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{}
			cfg.Daemon.FormulaV2 = boolPtr(tc.v2)
			got := featureflags.FromConfig(cfg)
			if got.FormulaV2 != tc.v2 || got.GraphApply != tc.v2 {
				t.Fatalf("FromConfig(formula_v2=%v) = %+v, want both %v", tc.v2, got, tc.v2)
			}
		})
	}
}

func TestApplySnapshotRoundTripsBothFlagsIndependently(t *testing.T) {
	restoreGlobals(t)
	// Apply/Snapshot treat the two flags independently even though FromConfig
	// only ever sets them in lockstep — proves the mechanism, not the policy.
	for _, want := range []featureflags.Flags{
		{FormulaV2: true, GraphApply: false},
		{FormulaV2: false, GraphApply: true},
		{FormulaV2: true, GraphApply: true},
		{FormulaV2: false, GraphApply: false},
	} {
		featureflags.Apply(want)
		if got := featureflags.Snapshot(); got != want {
			t.Fatalf("after Apply(%+v), Snapshot() = %+v", want, got)
		}
	}
}

func TestWithScopedAppliesThenRestores(t *testing.T) {
	restoreGlobals(t)
	baseline := featureflags.Flags{FormulaV2: false, GraphApply: false}
	featureflags.Apply(baseline)

	scoped := featureflags.Flags{FormulaV2: true, GraphApply: true}
	ran := false
	featureflags.WithScoped(scoped, func() {
		ran = true
		if got := featureflags.Snapshot(); got != scoped {
			t.Fatalf("inside WithScoped, Snapshot() = %+v, want %+v", got, scoped)
		}
	})
	if !ran {
		t.Fatal("WithScoped did not invoke fn")
	}
	if got := featureflags.Snapshot(); got != baseline {
		t.Fatalf("after WithScoped, Snapshot() = %+v, want restored baseline %+v", got, baseline)
	}
}
