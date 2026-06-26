package beads

import (
	"fmt"
	"strings"
	"testing"
)

// TestEnrichReadyProjectionForCacheSkipsMessageBeads guards the cache-reconcile
// convergence fix: message (mail) beads are never dependency-blocked ready work,
// and bd's denormalized is_blocked column flaps NULL<->false for ephemeral mail
// wisps. Feeding them through the ready projection makes the CachingStore
// reconciler re-emit bead.updated for them every cycle (an event flood that
// starved gc-hook work queries). The enrichment must leave their IsBlocked nil
// so the reconcile diff converges, while still enriching real work beads.
func TestEnrichReadyProjectionForCacheSkipsMessageBeads(t *testing.T) {
	runner := func(_, name string, args ...string) ([]byte, error) {
		joined := name + " " + strings.Join(args, " ")
		switch {
		case joined == "bd version":
			return []byte("bd version 1.1.0\n"), nil
		case len(args) > 0 && args[0] == "sql":
			// bd reports both ids as not-blocked; a buggy enrichment would
			// stamp IsBlocked=&false on the message bead too.
			return []byte(`[{"id":"mc-wisp-mail","is_blocked":false},{"id":"gcg-task","is_blocked":false}]`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}
	s := NewBdStore("/city", runner)

	items := []Bead{
		{ID: "mc-wisp-mail", Type: "message", Status: "open"}, // ephemeral mail, IsBlocked nil
		{ID: "gcg-task", Type: "task", Status: "open"},        // real work, IsBlocked nil
	}
	out, err := s.enrichReadyProjectionForCache(items)
	if err != nil {
		t.Fatalf("enrichReadyProjectionForCache: %v", err)
	}

	byID := make(map[string]Bead, len(out))
	for _, b := range out {
		byID[b.ID] = b
	}
	if got := byID["mc-wisp-mail"].IsBlocked; got != nil {
		t.Errorf("message bead IsBlocked = &%v, want nil (must be skipped so the reconcile diff converges)", *got)
	}
	if got := byID["gcg-task"].IsBlocked; got == nil || *got {
		t.Errorf("task bead IsBlocked = %v, want &false (real work must still be enriched)", got)
	}
}
