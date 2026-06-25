package api

import (
	"testing"
	"time"
)

// TestDefaultClientTimeoutAccommodatesFederatedReads guards the ceiling that
// governs the worker claim path. EphemeralBeads/ReadyBeads/ClaimBead pass
// context.Background(), so the HTTP client's overall timeout is their only
// deadline. Those endpoints federate the city store plus every rig store, and a
// dolt-backed rig store can take several seconds; a too-tight ceiling times out
// gc hook --claim and operators cannot claim work. 10s was too tight once the
// ephemeral read measured ~10s — keep meaningful headroom over the federated
// read cost.
func TestDefaultClientTimeoutAccommodatesFederatedReads(t *testing.T) {
	const minFederatedReadBudget = 30 * time.Second
	if defaultClientTimeout < minFederatedReadBudget {
		t.Fatalf("defaultClientTimeout = %v, want >= %v to cover federated multi-store reads on the claim path",
			defaultClientTimeout, minFederatedReadBudget)
	}
}
