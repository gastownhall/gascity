package beads

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

// wireProblem mirrors how a beads http client reports a non-2xx answer from
// bd serve: the error renders ITS OWN text ("<op>: bd serve at <url> answered
// <status> <code>: <detail>") and unwraps to the canonical sentinel the problem
// code maps to. The wire's `not_found` code maps to issueops.ErrNotFound, but
// the sentinel's own text never appears in the rendering, so a classifier that
// reads the message instead of the chain cannot see the miss.
type wireProblem struct {
	op     string
	status int
	code   string
	detail string
	err    error
}

func (e *wireProblem) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: bd serve at https://bdserve.example.test:30635 answered %d", e.op, e.status)
	if e.code != "" {
		fmt.Fprintf(&b, " %s", e.code)
	}
	if e.detail != "" {
		fmt.Fprintf(&b, ": %s", e.detail)
	}
	return b.String()
}

func (e *wireProblem) Unwrap() error { return e.err }

// errWireBadRequest stands in for the http client's status-class default: a
// 404 with no problem code (a proxy answering for a path it does not route,
// with an html body or none, so no detail either) is a bad request, never the
// not_found sentinel.
var errWireBadRequest = errors.New("bd serve rejected the request")

// wireNotFound is bd serve's own answer for an id neither plane holds — the
// exact shape maintainer-city's bd.dog seats hit (ga-ylpbsy.30).
func wireNotFound(op string) error {
	return &wireProblem{op: op, status: 404, code: "not_found", detail: "no issue or wisp with that id", err: issueops.ErrNotFound}
}

// TestClaimMapsTheWireNotFoundProblemToErrNotFound is the ga-ylpbsy.30 pin.
//
// Over a remote bd serve the claimer's miss is the wire's not_found problem:
// errors.Is(err, issueops.ErrNotFound) holds, but its text is the client's
// own rendering. Claim must still report it as beads.ErrNotFound — the
// documented contract every Store.Claim shares — because that is the only
// signal on which `gc hook --claim`'s class route goes looking for the bead
// in the relocated binding. Classifying by message alone answered an
// unrecognized error instead, the route never escalated, and every seat the
// controller spawned for a binding-resident wisp skipped it and drained.
func TestClaimMapsTheWireNotFoundProblemToErrNotFound(t *testing.T) {
	spy := &claimRoleSpy{err: wireNotFound("claimIssue"), getErr: wireNotFound("getIssue")}
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gcg--9223372036838841776", "bd.dog-1")
	if claimed {
		t.Fatal("claimed = true for an id the server does not hold")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Claim over the wire = %v, want it to wrap beads.ErrNotFound so the class route escalates", err)
	}
	if errors.Is(err, ErrWispNotClaimable) {
		t.Fatalf("Claim over the wire = %v, want no wisp refusal for an id no plane holds", err)
	}
	if !strings.Contains(err.Error(), "answered 404 not_found") {
		t.Fatalf("Claim over the wire = %q, want the server's own answer kept in the text", err)
	}
}

// TestNativeStoreErrorClassifiesTheTypedNotFoundSentinel pins the shared
// translation every native door uses, over every shape a miss arrives in.
func TestNativeStoreErrorClassifiesTheTypedNotFoundSentinel(t *testing.T) {
	transport := errors.New("dial tcp 10.0.0.1:30635: connect: connection refused")
	cases := []struct {
		name         string
		err          error
		wantNotFound bool
	}{
		{name: "wire not_found problem", err: wireNotFound("claimIssue"), wantNotFound: true},
		{name: "wire not_found wrapped by a caller", err: fmt.Errorf("claiming: %w", wireNotFound("claimIssue")), wantNotFound: true},
		{name: "local role miss", err: issueops.ErrNotFound, wantNotFound: true},
		{name: "local engine text", err: errors.New("issue not found: gc-1"), wantNotFound: true},
		{name: "codeless proxy 404", err: &wireProblem{op: "claimIssue", status: 404, err: errWireBadRequest}},
		{name: "transport failure", err: transport},
		{name: "server fault", err: &wireProblem{op: "claimIssue", status: 500, code: "internal", detail: "internal error", err: errors.New("bd serve fault")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nativeStoreError("gc-1", tc.err)
			if errors.Is(got, ErrNotFound) != tc.wantNotFound {
				t.Fatalf("nativeStoreError(%v) = %v; errors.Is(ErrNotFound) = %v, want %v", tc.err, got, errors.Is(got, ErrNotFound), tc.wantNotFound)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("nativeStoreError(%v) = %v, want the original error kept in the chain", tc.err, got)
			}
		})
	}
	if got := nativeStoreError("gc-1", nil); got != nil {
		t.Fatalf("nativeStoreError(nil) = %v, want nil", got)
	}
}
