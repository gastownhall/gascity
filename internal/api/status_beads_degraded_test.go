package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBuildStatusBodyCarriesBeadsDegraded pins the serve side: the degraded
// verdict the controller computes at read time must reach the status body,
// not be dropped by the projection.
func TestBuildStatusBodyCarriesBeadsDegraded(t *testing.T) {
	for _, degraded := range []bool{true, false} {
		state := newFakeState(t)
		state.cityBeadsDiag = &beads.BeadsDiagnostic{
			Store:               "BdStore",
			NativeStoreEligible: false,
			Degraded:            degraded,
		}
		s := &Server{state: state}

		body := s.buildStatusBody(context.Background(), false)
		if body.Beads == nil {
			t.Fatal("Beads = nil, want diagnostic")
		}
		if body.Beads.Degraded != degraded {
			t.Fatalf("beads.degraded = %v, want %v", body.Beads.Degraded, degraded)
		}
	}
}

// TestStatusBeadsDiagnosticDecodesDegraded pins the client side. The field is
// omitempty on the wire, so an absent value must decode as false rather than
// leaving the caller with a nil-pointer question.
func TestStatusBeadsDiagnosticDecodesDegraded(t *testing.T) {
	degraded := true
	for _, tc := range []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "present", in: &degraded, want: true},
		{name: "absent", in: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusBeadsDiagnosticFromGen(&genclient.BeadsDiagnostic{
				BeadsStore: "BdStore",
				Degraded:   tc.in,
			})
			if got == nil {
				t.Fatal("statusBeadsDiagnosticFromGen() = nil")
			}
			if got.Degraded != tc.want {
				t.Fatalf("Degraded = %v, want %v", got.Degraded, tc.want)
			}
		})
	}
}
