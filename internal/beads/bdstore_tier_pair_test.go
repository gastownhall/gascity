package beads_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestBdStoreTwoTierListReadsBothTiersAtOnce pins that a List over two tiers
// reads its halves at the same time. Each half is its own bd subprocess (bd list
// for the issues tier, bd query for the wisps), and read one after the other a
// two-tier List costs its caller two bd round trips in series instead of one: on
// a fresh (proxied-server) city the seat hooks' step reminder and mailbox reads
// are such Lists, and their serial round trips are most of each hook's wall time
// (ga-3js1ayv).
func TestBdStoreTwoTierListReadsBothTiersAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier beads.TierMode
	}{
		{name: "both tiers", tier: beads.TierBoth},
		{name: "wisps tier", tier: beads.TierWisps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listArrived, queryArrived := make(chan struct{}), make(chan struct{})
			var listOnce, queryOnce sync.Once
			// Each half waits, bounded, for the other to arrive: halves read at
			// once both arrive, halves read in series never do.
			awaitOther := func(other <-chan struct{}, self string) error {
				select {
				case <-other:
					return nil
				case <-time.After(testutil.GoroutineRaceTimeout):
					return fmt.Errorf("%s ran alone: the other tier's read had not started", self)
				}
			}
			runner := func(_, name string, args ...string) ([]byte, error) {
				full := name + " " + strings.Join(args, " ")
				switch {
				case strings.HasPrefix(full, "bd list "):
					listOnce.Do(func() { close(listArrived) })
					if err := awaitOther(queryArrived, "bd list"); err != nil {
						return nil, err
					}
					return []byte(`[{"id":"bd-i","title":"issue","status":"open","issue_type":"task","created_at":"2026-05-01T00:00:00Z","labels":["order-run:o"]}]`), nil
				case strings.HasPrefix(full, "bd query "):
					queryOnce.Do(func() { close(queryArrived) })
					if err := awaitOther(listArrived, "bd query"); err != nil {
						return nil, err
					}
					return []byte(`[{"id":"bd-w","title":"wisp","status":"open","issue_type":"task","created_at":"2026-05-02T00:00:00Z","ephemeral":true,"labels":["order-run:o"]}]`), nil
				}
				return nil, errors.New("unexpected: " + full)
			}
			s := beads.NewBdStore("/city", runner)
			got, err := s.List(beads.ListQuery{Label: "order-run:o", TierMode: tc.tier, Sort: beads.SortCreatedDesc})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if tc.tier == beads.TierBoth && (len(got) != 2 || got[0].ID != "bd-w" || got[1].ID != "bd-i") {
				t.Fatalf("List = %+v, want the merged [bd-w bd-i] the serial read returned", got)
			}
		})
	}
}
