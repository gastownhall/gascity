package api

import (
	"testing"
	"time"
)

// TestDefaultClientTimeoutAccommodatesFederatedReads guards the ceiling that
// governs the control-plane read paths. ListBeads/GetBead/GetStatus/
// ListMailInbox pass context.Background(), so the HTTP client's overall timeout
// is their only deadline. Those endpoints federate the city store plus every
// rig store, and a dolt-backed rig store can take several seconds; a too-tight
// ceiling false-times-out healthy-but-slow federated reads. 10s was too tight
// once a federated read measured ~10s — keep meaningful headroom over the
// federated read cost.
func TestDefaultClientTimeoutAccommodatesFederatedReads(t *testing.T) {
	const minFederatedReadBudget = 30 * time.Second
	if defaultClientTimeout < minFederatedReadBudget {
		t.Fatalf("defaultClientTimeout = %v, want >= %v to cover federated multi-store control-plane reads",
			defaultClientTimeout, minFederatedReadBudget)
	}
}
