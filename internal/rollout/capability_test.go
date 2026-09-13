package rollout

import (
	"context"
	"strings"
	"testing"
)

// TestResolveCapabilityGeneral is the general-Auto acceptance artifact: a
// SYNTHETIC, non-beads capability predicate (a fake "runtime provider supports
// nudge" probe) drives every cell of the resolver using ONLY rollout types. It
// is the mechanically-checkable proof that capability-resolution is general and
// not beads-locked. (The import-boundary test guarantees this package cannot
// even reach internal/beads.)
func TestResolveCapabilityGeneral(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	capable := func(reason string) Capability {
		return func(context.Context) (bool, string) { return true, reason }
	}
	incapable := func(reason string) Capability {
		return func(context.Context) (bool, string) { return false, reason }
	}

	cases := []struct {
		name       string
		mode       Mode
		cap        Capability
		wantDec    Decision
		wantReason string
	}{
		{"unset defaults legacy", ModeUnset, incapable("unconsulted"), UseLegacy, "mode unset"},
		{"off legacy", Off, incapable("unconsulted"), UseLegacy, "mode off"},
		{"auto capable", Auto, capable("provider supports nudge"), UseNew, "provider supports nudge"},
		{"auto incapable degrades loud", Auto, incapable("provider lacks nudge"), DegradeLoud, "provider lacks nudge"},
		{"require capable", Require, capable("provider supports nudge"), UseNew, "provider supports nudge"},
		{"require incapable refuses closed", Require, incapable("provider lacks nudge"), RefuseClosed, "provider lacks nudge"},
		{"auto nil predicate vacuously capable", Auto, nil, UseNew, "no capability predicate"},
		{"require nil predicate vacuously capable", Require, nil, UseNew, "no capability predicate"},
		{"unrecognized mode fails closed to legacy", Mode("bananas"), incapable("unconsulted"), UseLegacy, "unrecognized mode"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec, reason := ResolveCapability(ctx, tc.mode, tc.cap)
			if dec != tc.wantDec {
				t.Errorf("decision = %q, want %q", dec, tc.wantDec)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want to contain %q", reason, tc.wantReason)
			}
		})
	}
}

// TestResolveCapabilityOffIsZeroCost proves Off and ModeUnset never consult the
// capability predicate — the legacy path pays nothing.
func TestResolveCapabilityOffIsZeroCost(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{Off, ModeUnset} {
		called := false
		probe := Capability(func(context.Context) (bool, string) { called = true; return true, "x" })
		if dec, _ := ResolveCapability(context.Background(), mode, probe); dec != UseLegacy {
			t.Errorf("mode %q: decision = %q, want use_legacy", mode, dec)
		}
		if called {
			t.Errorf("mode %q consulted the capability predicate; must be zero-cost", mode)
		}
	}
}
