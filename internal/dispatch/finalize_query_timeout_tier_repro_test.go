package dispatch

import (
	"errors"
	"testing"
)

func TestFinalizeStoreTimeoutIsAvailabilityTier(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			name: "reported finalize outcome read",
			msg:  "contour-5d1: resolving workflow outcome: bd list both tiers: bd query: bd query (wisps): timed out after 30s",
		},
		{
			name: "this repo's own finalize wrapping",
			msg:  "ga-0ow: resolving workflow outcome: timed out after 30s",
		},
		{
			name: "work query read (already handled)",
			msg:  "running work query: bd query (wisps): timed out after 30s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyControllerError(errors.New(tc.msg))
			if got != TierAvailability {
				t.Errorf("ClassifyControllerError(%q) = %s, want availability\n"+
					"a store timeout must retry, not quarantine the finalizer on first refusal", tc.msg, got)
			}
		})
	}
}
