package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The ga-ylpbsy.30 split city, end to end through a real NativeDoltStore work
// leg: the work store is a remote bd serve, so its claimer's miss is the wire's
// not_found problem — an error that renders the client's own text and unwraps
// to issueops.ErrNotFound — and the bead lives only in the relocated infra
// binding the federated reader served it from.

// wireNotFoundProblem renders a bd serve 404 the way the beads http client does:
// its own text, with the canonical sentinel kept in the chain.
type wireNotFoundProblem struct{ op string }

func (e wireNotFoundProblem) Error() string {
	return e.op + ": bd serve at https://bdserve.example.test:30635 answered 404 not_found: no issue or wisp with that id"
}

func (e wireNotFoundProblem) Unwrap() error { return issueops.ErrNotFound }

// wireMissStorage is a remote work store that holds no row at all: its claimer
// and its reader both answer with the wire's not_found. Every other storage
// method is the embedded nil interface, so a claim that reaches past these two
// roles panics rather than passing by accident.
type wireMissStorage struct {
	beadslib.Storage
	claims, gets int
}

func (s *wireMissStorage) IssueClaimer() (issueops.Claimer, error) { return wireMissClaimer{s}, nil }
func (s *wireMissStorage) IssueReader() (issueops.Reader, error)   { return wireMissReader{s: s}, nil }

type wireMissClaimer struct{ s *wireMissStorage }

func (c wireMissClaimer) Claim(context.Context, issueops.ClaimRequest) (issueops.ClaimResult, error) {
	c.s.claims++
	return issueops.ClaimResult{}, wireNotFoundProblem{op: "claimIssue"}
}

type wireMissReader struct {
	issueops.Reader
	s *wireMissStorage
}

func (r wireMissReader) Get(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.s.gets++
	return nil, wireNotFoundProblem{op: "getIssue"}
}

// TestClassRoutedClaimTakesAnInfraBindingWispOverAWireWorkStore is the
// incident, replayed: before the fix the work leg's claim error was
// unclassified, the class route never escalated, and the seat skipped the wisp
// and drained claims_errored while the controller kept counting it as demand.
func TestClassRoutedClaimTakesAnInfraBindingWispOverAWireWorkStore(t *testing.T) {
	const wisp = "gcg--9223372036838841776"
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, wisp, map[string]string{"gc.routed_to": "bd.dog"})

	storage := &wireMissStorage{}
	work := beads.NewNativeDoltStoreOverStorageForTest(storage)
	ops := classRoutedHookClaimOps(hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return hookClaimThroughStore(beadID, assignee,
				func() (beads.Bead, bool, error) { return work.Claim(beadID, assignee) },
				work.Get)
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return work.Get(beadID)
		},
	}, newClaimRouteFor(t, class))

	claimed, ok, err := ops.Claim(context.Background(), "/data/projects/maintainer-city", nil, wisp, "bd.dog-1")
	if err != nil || !ok {
		t.Fatalf("claim of %s = (ok=%v err=%v), want it claimed through the infra binding the reader served it from", wisp, ok, err)
	}
	if strings.TrimSpace(claimed.Assignee) != "bd.dog-1" {
		t.Fatalf("claimed assignee = %q, want bd.dog-1", claimed.Assignee)
	}
	held, err := class.Get(wisp)
	if err != nil || strings.TrimSpace(held.Assignee) != "bd.dog-1" || !strings.EqualFold(held.Status, "in_progress") {
		t.Fatalf("infra binding holds %s as status=%q assignee=%q (err=%v), want in_progress owned by bd.dog-1", wisp, held.Status, held.Assignee, err)
	}
	if storage.claims != 1 {
		t.Fatalf("the work store's claimer ran %d times, want once: the work leg answers first", storage.claims)
	}
	// The typed not-found is the proof, so no settling read of the work leg is
	// paid beyond the store's own wisp disambiguation.
	if storage.gets != 1 {
		t.Fatalf("the work store was read %d times, want 1 (Claim's own disambiguating read)", storage.gets)
	}
}
