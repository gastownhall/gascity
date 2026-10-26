package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The ga-ylpbsy.30 split city, end to end: the work store is a remote bd serve
// whose answer to a claim of a bead it does not hold is the wire's not_found
// problem, and the bead lives only in the relocated infra binding the federated
// reader served it from. Two work legs reach that answer. An in-process native
// door keeps the problem's typed sentinel, so the write alone proves the
// absence. The bd subprocess `gc hook --claim` claims through in production
// gets only its text, so the settling read of the same leg proves it.

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

func (s *wireMissStorage) IssueReader() (issueops.Reader, error) { return wireMissReader{s: s}, nil }

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
// incident's shape through an in-process native door: before the fix the
// door's claim error was unclassified, the class route never escalated, and
// the seat skipped the wisp and drained claims_errored while the controller
// kept counting it as demand.
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

// TestClassRoutedClaimTakesAnInfraBindingWispOverABdSubprocessWorkLeg is the
// incident on its production path: the hook's own bd seams
// (hookClaimWithBdStore, hookReadClaimedBeadWithBdStore) over bd's real
// replies. No sentinel crosses the subprocess boundary, so the claim's miss is
// text the not-found predicate does not recognize, and it is the settling read
// (`bd show`, legProvedAbsent) that proves the work leg holds no such row and
// lands the claim in the binding. Without that read every seat spawned for the
// wisp skips it and drains.
func TestClassRoutedClaimTakesAnInfraBindingWispOverABdSubprocessWorkLeg(t *testing.T) {
	const (
		wisp     = "gcg--9223372036838841776"
		wireMiss = "claimIssue: bd serve at https://bdserve.example.test:30635 answered 404 not_found: no issue or wisp with that id"
	)
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, wisp, map[string]string{"gc.routed_to": "bd.dog"})

	claimCmd := "update " + wisp + " --claim --json"
	showCmd := "show --json " + wisp
	wispQueryCmd := "query --json ephemeral=true AND id=" + wisp + " --all --limit 1"
	// The remote store's 404 on the claim, and bd show's miss: stderr naming the
	// bead, the not-found envelope on stdout.
	replies := map[string]struct{ out, err string }{
		claimCmd:     {out: `{"error": "` + wireMiss + `", "schema_version": 1}`, err: "exit status 1: Error: " + wireMiss},
		showCmd:      {out: `{"error": "no issues found matching the provided IDs", "schema_version": 1}`, err: "exit status 1: Issue " + wisp + " not found"},
		wispQueryCmd: {out: "[]"},
	}
	var ran []string
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_, name string, args ...string) ([]byte, error) {
			cmd := strings.Join(args, " ")
			ran = append(ran, cmd)
			reply, ok := replies[cmd]
			if name != "bd" || !ok {
				t.Errorf("unexpected command: %s %s", name, cmd)
				return nil, fmt.Errorf("unexpected command: %s %s", name, cmd)
			}
			if reply.err != "" {
				return []byte(reply.out), errors.New(reply.err)
			}
			return []byte(reply.out), nil
		}
	}
	cityDir := t.TempDir()
	if _, _, err := hookClaimWithBdStore(context.Background(), cityDir, nil, wisp, "bd.dog-1"); err == nil || hookClaimBeadIsElsewhere(err) {
		t.Fatalf("fixture: the subprocess claim answered %v, want an error that names no miss", err)
	}
	ran = nil

	ops := classRoutedHookClaimOps(hookClaimOps{Claim: hookClaimWithBdStore, ReadWorkMeta: hookReadClaimedBeadWithBdStore}, newClaimRouteFor(t, class))
	claimed, ok, err := ops.Claim(context.Background(), cityDir, nil, wisp, "bd.dog-1")
	if err != nil || !ok || strings.TrimSpace(claimed.Assignee) != "bd.dog-1" {
		t.Fatalf("claim of %s = (%+v ok=%v err=%v), want it claimed through the infra binding the reader served it from", wisp, claimed, ok, err)
	}
	held, err := class.Get(wisp)
	if err != nil || strings.TrimSpace(held.Assignee) != "bd.dog-1" || !strings.EqualFold(held.Status, "in_progress") {
		t.Fatalf("infra binding holds %s as status=%q assignee=%q (err=%v), want in_progress owned by bd.dog-1", wisp, held.Status, held.Assignee, err)
	}
	if want := []string{claimCmd, showCmd, wispQueryCmd}; !slices.Equal(ran, want) {
		t.Fatalf("bd ran %q, want the claim, then one settling read of the same leg %q", ran, want)
	}
}
