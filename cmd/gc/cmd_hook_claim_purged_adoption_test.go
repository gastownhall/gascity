package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The adoption tier against a purged bead.
//
// A seat claims work-1; the bead is later purged from every store (wisp
// retention, `gc bd delete`, a re-sling), but the caching work query keeps
// serving the dead row. The tier must not re-serve it once the city-wide
// prover confirms no store holds it, or the seat is pinned on a dead id every
// poll until its session drains.

// purgedAdoptionDeadRow is the cached row for the purged bead: still in
// progress and assigned to this seat, as far as the work query knows.
const purgedAdoptionDeadRow = `{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}`

// purgedAdoptionEnv is one `gc hook --claim` poll whose work query serves the
// dead row (plus any extra rows), with recording fakes on every seam the
// existing-assignment tier and the claim tiers touch.
type purgedAdoptionEnv struct {
	// extraRows are appended to the work query's output after the dead row.
	extraRows []string
	// missing lists the ids the canonical readback answers ErrNotFound for.
	missing map[string]bool
	// readbackErr, when set, is the readback's answer for every id.
	readbackErr error
	// prove is the absence prover; nil leaves ops.ProveInputAbsent unset.
	prove func(ids []string) (bool, error)
	// session sets GC_SESSION_ID so the session current-claim seams run.
	session bool
	// stamp and stampErr are the ReadSessionClaim fake's answer.
	stamp    string
	stampErr error
	// clearErr is returned by the StampSessionClaim spy.
	clearErr error

	reads      []string
	proofs     [][]string
	stampReads []string
	claims     []string
	sessSpy    sessionClaimSpy
	stdout     bytes.Buffer
	stderr     bytes.Buffer
}

const purgedAdoptionSessionID = "mc-sess1"

func (e *purgedAdoptionEnv) ops() hookClaimOps {
	e.sessSpy.err = e.clearErr
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return "[" + strings.Join(append([]string{purgedAdoptionDeadRow}, e.extraRows...), ",") + "]", nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			e.reads = append(e.reads, beadID)
			if e.readbackErr != nil {
				return beads.Bead{}, e.readbackErr
			}
			if e.missing[beadID] {
				return beads.Bead{}, fmt.Errorf("getting bead %q: %w", beadID, beads.ErrNotFound)
			}
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-1"}, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			e.claims = append(e.claims, id)
			return beads.Bead{
				ID: id, Status: "in_progress", Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": "worker"},
			}, true, nil
		},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		StampSessionClaim: e.sessSpy.fn,
		PublishRunMap:     noopPublishRunMap,
		ReadSessionClaim: func(sessionID string) (string, error) {
			e.stampReads = append(e.stampReads, sessionID)
			return e.stamp, e.stampErr
		},
	}
	if e.prove != nil {
		ops.ProveInputAbsent = func(_ context.Context, ids []string) (bool, error) {
			e.proofs = append(e.proofs, append([]string(nil), ids...))
			return e.prove(ids)
		}
	}
	return ops
}

func (e *purgedAdoptionEnv) run() int {
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	if e.session {
		opts.Env = []string{"GC_SESSION_ID=" + purgedAdoptionSessionID}
	}
	return doHookClaim("query", "/rig", opts, e.ops(), &e.stdout, &e.stderr)
}

// proveAbsent is a prover whose city-wide search found the ids in no store.
func proveAbsent([]string) (bool, error) { return true, nil }

// purgedAdoptionOpenRow is a second, unassigned row routed to this seat: other
// work the claim tiers can take once the dead row is dropped.
const purgedAdoptionOpenRow = `{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}`

// AC-1: a bead the readback cannot find and the prover proves absent is not
// re-served; the claim continues to the claim tiers.
func TestHookClaimPurgedAdoptionDropsAProvenMissingBead(t *testing.T) {
	t.Run("no other work drains", func(t *testing.T) {
		e := &purgedAdoptionEnv{missing: map[string]bool{"work-1": true}, prove: proveAbsent}

		code := e.run()

		if code != 1 {
			t.Fatalf("code = %d, want 1 (shared no_work drain); stdout=%s stderr=%s", code, e.stdout.String(), e.stderr.String())
		}
		if strings.Contains(e.stdout.String(), `"action":"work"`) {
			t.Fatalf("stdout = %q, want no work receipt for a bead no store holds", e.stdout.String())
		}
		if !strings.Contains(e.stderr.String(), "not re-serving work-1") {
			t.Errorf("stderr = %q, want the dropped bead named", e.stderr.String())
		}
		if len(e.proofs) != 1 || len(e.proofs[0]) != 1 || e.proofs[0][0] != "work-1" {
			t.Errorf("proofs = %v, want exactly one proof of [work-1]", e.proofs)
		}
	})

	t.Run("other offered work is claimed", func(t *testing.T) {
		e := &purgedAdoptionEnv{
			extraRows: []string{purgedAdoptionOpenRow},
			missing:   map[string]bool{"work-1": true},
			prove:     proveAbsent,
		}

		code := e.run()

		if code != 0 {
			t.Fatalf("code = %d, want 0; stdout=%s stderr=%s", code, e.stdout.String(), e.stderr.String())
		}
		if strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) {
			t.Fatalf("stdout = %q, want no adoption receipt for the purged bead", e.stdout.String())
		}
		if !strings.Contains(e.stdout.String(), `"bead_id":"work-2"`) {
			t.Fatalf("stdout = %q, want the receipt for the newly claimed work-2", e.stdout.String())
		}
		if len(e.claims) != 1 || e.claims[0] != "work-2" {
			t.Errorf("claims = %v, want exactly one claim of work-2", e.claims)
		}
	})

	// ErrIDCollision wraps ErrNotFound, so it reaches the proof like any miss:
	// only the city-wide proof decides.
	t.Run("an id collision still needs the proof", func(t *testing.T) {
		e := &purgedAdoptionEnv{
			readbackErr: fmt.Errorf("getting bead %q: %w", "work-1", beads.ErrIDCollision),
			prove:       proveAbsent,
		}

		code := e.run()

		if code != 1 || len(e.proofs) != 1 {
			t.Fatalf("code = %d proofs = %v, want the proven-missing drain after one proof; stderr=%s", code, e.proofs, e.stderr.String())
		}
	})
}

// AC-2: the session's current-claim stamp is cleared only when it names the
// dropped bead, and before any later claim in the same invocation re-stamps.
func TestHookClaimPurgedAdoptionClearsOnlyItsOwnStamp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extraRows  []string
		stamp      string
		stampErr   error
		clearErr   error
		wantCode   int
		wantWrites []string
		wantStderr string
	}{
		{name: "stamp names the dead bead", stamp: "work-1", wantCode: 1, wantWrites: []string{""}},
		{
			name: "cleared before the next claim restamps", extraRows: []string{purgedAdoptionOpenRow},
			stamp: "work-1", wantCode: 0, wantWrites: []string{"", "work-2"},
		},
		{name: "empty stamp", stamp: "", wantCode: 1, wantWrites: nil},
		{name: "stamp names other live work", stamp: "work-9", wantCode: 1, wantWrites: nil},
		{
			name: "unreadable stamp is left alone", stampErr: errors.New("session store down"),
			wantCode: 1, wantWrites: nil, wantStderr: "session store down",
		},
		{
			name: "a failed clear changes no exit code", stamp: "work-1", clearErr: errors.New("clear refused"),
			wantCode: 1, wantWrites: []string{""}, wantStderr: "clear refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &purgedAdoptionEnv{
				extraRows: tc.extraRows,
				missing:   map[string]bool{"work-1": true},
				prove:     proveAbsent,
				session:   true,
				stamp:     tc.stamp,
				stampErr:  tc.stampErr,
				clearErr:  tc.clearErr,
			}

			code := e.run()

			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stdout=%s stderr=%s", code, tc.wantCode, e.stdout.String(), e.stderr.String())
			}
			if strings.Join(e.sessSpy.beadIDs, ",") != strings.Join(tc.wantWrites, ",") || len(e.sessSpy.beadIDs) != len(tc.wantWrites) {
				t.Fatalf("session claim writes = %q, want %q", e.sessSpy.beadIDs, tc.wantWrites)
			}
			if len(e.stampReads) != 1 || e.stampReads[0] != purgedAdoptionSessionID {
				t.Errorf("stamp reads = %v, want exactly one read of %s", e.stampReads, purgedAdoptionSessionID)
			}
			if tc.wantStderr != "" && !strings.Contains(e.stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to report %q", e.stderr.String(), tc.wantStderr)
			}
		})
	}

	t.Run("no session reads no stamp", func(t *testing.T) {
		e := &purgedAdoptionEnv{missing: map[string]bool{"work-1": true}, prove: proveAbsent, stamp: "work-1"}

		if code := e.run(); code != 1 {
			t.Fatalf("code = %d, want 1; stderr=%s", code, e.stderr.String())
		}
		if len(e.stampReads) != 0 || len(e.sessSpy.beadIDs) != 0 {
			t.Fatalf("stamp reads = %v writes = %q, want none without a session id", e.stampReads, e.sessSpy.beadIDs)
		}
	})
}

// sessionCurrentClaimVia is the read half of the claim stamp, through the same
// session front door the stamp writes through.
func TestSessionCurrentClaimVia(t *testing.T) {
	t.Run("returns the stamp", func(t *testing.T) {
		front := hookCurrentFrontDoor(t, "work-1")
		got, err := sessionCurrentClaimVia(func() (*session.Store, error) { return front, nil }, hookCurrentSessionID)
		if err != nil || got != "work-1" {
			t.Fatalf("sessionCurrentClaimVia = (%q, %v), want (work-1, nil)", got, err)
		}
	})

	t.Run("wraps a front-door error", func(t *testing.T) {
		boom := errors.New("front door down")
		_, err := sessionCurrentClaimVia(func() (*session.Store, error) { return nil, boom }, hookCurrentSessionID)
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), hookCurrentSessionID) {
			t.Fatalf("err = %v, want the front-door error wrapped with the session id", err)
		}
	})

	t.Run("wraps an unreadable session", func(t *testing.T) {
		front := hookCurrentFrontDoor(t, "work-1")
		_, err := sessionCurrentClaimVia(func() (*session.Store, error) { return front, nil }, "mc-absent")
		if err == nil || !strings.Contains(err.Error(), "mc-absent") {
			t.Fatalf("err = %v, want the absent session named", err)
		}
	})

	t.Run("empty id never opens the front door", func(t *testing.T) {
		opened := false
		got, err := sessionCurrentClaimVia(func() (*session.Store, error) { opened = true; return nil, nil }, "  ")
		if err != nil || got != "" || opened {
			t.Fatalf("sessionCurrentClaimVia(\"\") = (%q, %v) opened=%v, want (\"\", nil) without opening", got, err, opened)
		}
	})
}
