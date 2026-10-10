package beads_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The hook claims through `bd update <id> --claim --json` on the work store
// and, on a not-found, escalates to its graph store (DESIGN C5 G8, S24-T).
// Over http these are the two texts that decide it, verbatim from beads
// feat/http-s6bcd (cmd/bd/update.go reportUpdateFailures and the
// W-ClaimRequest.Wisp ledger row): the absent id must read as ErrNotFound,
// and a wisp the server cannot claim must be the NAMED refusal, never a
// not-found the class route would escalate on.

// bdExitWithStderr mimics classifyBDExecResult: a failed bd run's error
// carries stderr after the exit status.
func bdExitWithStderr(stderr string) error {
	return fmt.Errorf("exit status 1: %s", stderr)
}

func TestBdStoreClaimOfAnAbsentIDOverHTTPIsNotFound(t *testing.T) {
	stderr := "Issue e2e-absent9 not found\n" +
		`{"error":"1 of 1 issues failed to update","failed":[{"id":"e2e-absent9","error":"issue not found"}],"schema_version":1}`
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, bdExitWithStderr(stderr)
	}
	_, ok, err := beads.NewBdStore("/city", runner).Claim("e2e-absent9")
	if ok || !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Claim(absent) = ok %v, err %v; want ErrNotFound so the hook escalates to the graph store", ok, err)
	}
	if errors.Is(err, beads.ErrWispNotClaimable) {
		t.Fatalf("an absent id must not read as a wisp refusal: %v", err)
	}
}

func TestBdStoreClaimOfAWispTheServerCannotClaimIsANamedRefusal(t *testing.T) {
	stderr := "Error updating mc-wisp-1uajke: updateIssue: claiming a wisp is not supported by this bd serve " +
		"(the server predates claiming through its update operation, and its separate claim operation excludes the wisp plane; " +
		"upgrade bd serve, or claim this issue from a local workspace; PATCH /v0/beads/issues/{id})\n" +
		`{"error":"1 of 1 issues failed to update","failed":[{"id":"mc-wisp-1uajke","error":"updating issue: updateIssue: claiming a wisp is not supported by this bd serve"}],"schema_version":1}`
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, bdExitWithStderr(stderr)
	}
	_, ok, err := beads.NewBdStore("/city", runner).Claim("mc-wisp-1uajke")
	if ok || err == nil {
		t.Fatalf("Claim(wisp) = ok %v, err %v; want a refusal", ok, err)
	}
	var refused *beads.WispClaimRefusedError
	if !errors.As(err, &refused) || refused.ID != "mc-wisp-1uajke" {
		t.Fatalf("Claim(wisp) err = %v; want *WispClaimRefusedError naming the wisp", err)
	}
	if !errors.Is(err, beads.ErrWispNotClaimable) {
		t.Fatalf("the refusal must match ErrWispNotClaimable: %v", err)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the refusal must never read as ErrNotFound (the class route would escalate on it): %v", err)
	}
}

// TestBdStoreClaimOfAProxy404IsNotNotFound: a 404 from something in front of
// bd serve (a gateway answering for a path it does not route) carries no
// not_found code. Reading its "Not Found" text as ErrNotFound would send the
// class route looking in the graph store and skip the bead as absent
// everywhere; it is an error, said loudly.
func TestBdStoreClaimOfAProxy404IsNotNotFound(t *testing.T) {
	for _, stderr := range []string{
		"Error updating gc-7: updateIssue: bd serve at https://beads.example answered 404: Not Found\n" +
			`{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-7","error":"updating issue: updateIssue: bd serve at https://beads.example answered 404: Not Found"}],"schema_version":1}`,
		"Error updating gc-7: updateIssue: bd serve at https://beads.example answered 404: 404 page not found",
	} {
		runner := func(_, _ string, _ ...string) ([]byte, error) {
			return nil, bdExitWithStderr(stderr)
		}
		_, ok, err := beads.NewBdStore("/city", runner).Claim("gc-7")
		if ok || err == nil {
			t.Fatalf("Claim through a proxy 404 = ok %v err %v, want an error", ok, err)
		}
		if errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("a proxy 404 read as ErrNotFound (the class route would escalate on it): %v", err)
		}
	}
}
