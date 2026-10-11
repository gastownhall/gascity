package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The adoption tier: a receipt minted from the work query alone.
//
// The fresh-claim tiers are now triple-guarded, but adoption never ran a CAS —
// it re-serves a bead the query says this session already holds — so it shipped
// an existing_assignment receipt with NO canonical readback at all. That is a
// promise made entirely on the word of whatever store answered the query.
//
// The failure is live in this fork: after a dead-assignee release the dispatcher
// re-slings a step to a fresher seat, and the old seat's work query reads a
// stale caching-store row still showing in_progress/assignee=old-seat (the
// cache-reconcile false-positive family). Every poll re-served the foreign bead,
// and the seat acted on a claim the canonical store does not honor — the
// specimen's shape exactly, minus the drain.

// staleCacheEnv is one poll whose work query returns a row this seat no longer
// owns canonically.
type staleCacheEnv struct {
	canonicalAssignee string
	readbackErr       error
	reads             []string
	stdout            bytes.Buffer
	stderr            bytes.Buffer
}

func (e *staleCacheEnv) run() int {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			// The cache still says we hold it.
			return `[{"id":"work-1","status":"in_progress","assignee":"worker-1",` +
				`"metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			e.reads = append(e.reads, beadID)
			if e.readbackErr != nil {
				return beads.Bead{}, e.readbackErr
			}
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: e.canonicalAssignee}, nil
		},
		// Keep the stamp seams inert so the test observes adoption only.
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		StampSessionClaim: func(string, string) error { return nil },
		PublishRunMap:     func(string, string, ...string) error { return nil },
	}
	return doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}, ops, &e.stdout, &e.stderr)
}

// A bead the canonical store gives to someone else is not re-served, however
// confidently the cache reports it.
func TestHookClaimAdoptionRefusesAStaleCachedOwnership(t *testing.T) {
	e := &staleCacheEnv{canonicalAssignee: "gcg-session-5a6da34c7f"}

	code := e.run()

	if strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) {
		t.Fatalf("stdout = %q, want no adoption receipt for a bead the canonical store gives to another seat", e.stdout.String())
	}
	if strings.Contains(e.stdout.String(), `"action":"work"`) {
		t.Fatalf("stdout = %q, want no work receipt at all", e.stdout.String())
	}
	// Falling through to the drain is the right answer, not exit-on-error: the
	// seat is healthy, its cache was not, and the other tiers cannot match this
	// row (ready needs open, eligible needs an empty assignee).
	if code != 1 {
		t.Fatalf("code = %d, want 1 (drained, nothing to do)", code)
	}
	if len(e.reads) != 1 || e.reads[0] != "work-1" {
		t.Fatalf("canonical reads = %v, want exactly one readback of work-1", e.reads)
	}
	if !strings.Contains(e.stderr.String(), "gcg-session-5a6da34c7f") {
		t.Errorf("stderr = %q, want the canonical owner named", e.stderr.String())
	}
}

// Control: when the canonical store agrees, adoption works exactly as before.
func TestHookClaimAdoptionServesACanonicallyOwnedBead(t *testing.T) {
	e := &staleCacheEnv{canonicalAssignee: "worker-1"}

	code := e.run()

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) {
		t.Fatalf("stdout = %q, want the adoption receipt", e.stdout.String())
	}
}

// A readback that could not be MADE is not evidence of foreign ownership.
//
// This arm is deliberately distinct from the refusal above and matches what the
// stamp path already does with an unreadable canonical bead: proceed, and let
// the missing readback suppress the durable lifecycle emission rather than
// withhold the work. Failing closed here would idle every seat behind one store
// hiccup — the same trade the F-D probe makes, for the same reason.
func TestHookClaimAdoptionProceedsWhenTheReadbackCannotBeMade(t *testing.T) {
	e := &staleCacheEnv{readbackErr: errors.New("store unreachable")}

	code := e.run()

	if code != 0 {
		t.Fatalf("code = %d, want 0 (unverified is not refused); stderr=%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) {
		t.Fatalf("stdout = %q, want the adoption receipt to still ship", e.stdout.String())
	}
	if !strings.Contains(e.stderr.String(), "without a canonical ownership readback") {
		t.Errorf("stderr = %q, want the unverified adoption named distinctly from a refusal", e.stderr.String())
	}
}
