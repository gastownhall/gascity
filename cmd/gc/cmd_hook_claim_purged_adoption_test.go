package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	// deadRow, when set, replaces purgedAdoptionDeadRow as the first row.
	deadRow string
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
	// stamp is the session row's current-claim stamp the fakes keep:
	// StampSessionClaim writes it, ForgetSessionClaim clears it while it
	// names the bead.
	stamp string
	// forgetErr, when set, is ForgetSessionClaim's answer, and the stamp
	// stays.
	forgetErr error
	// front, when set, is a real session front door the two stamp seams
	// write through, by the production ...Via functions, in place of the
	// fakes.
	front *session.Store

	reads     []string
	proofs    [][]string
	stampOps  []string // "stamp <session> <bead>" and "forget <session> <bead>", in order
	claims    []string
	stopReads []string
	stdout    bytes.Buffer
	stderr    bytes.Buffer
}

const purgedAdoptionSessionID = "mc-sess1"

func (e *purgedAdoptionEnv) ops() hookClaimOps {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			dead := purgedAdoptionDeadRow
			if e.deadRow != "" {
				dead = e.deadRow
			}
			return "[" + strings.Join(append([]string{dead}, e.extraRows...), ",") + "]", nil
		},
		WorkflowStore: func(_ context.Context, _ string, _ []string, beadID, _ string) beads.Store {
			e.stopReads = append(e.stopReads, beadID)
			return nil
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
		PublishRunMap:     noopPublishRunMap,
		StampSessionClaim: func(sessionID, beadID string) error {
			e.stampOps = append(e.stampOps, "stamp "+sessionID+" "+beadID)
			e.stamp = beadID
			return nil
		},
		ForgetSessionClaim: func(sessionID, beadID string) error {
			e.stampOps = append(e.stampOps, "forget "+sessionID+" "+beadID)
			if e.forgetErr != nil {
				return e.forgetErr
			}
			if e.stamp == beadID {
				e.stamp = ""
			}
			return nil
		},
	}
	if e.front != nil {
		ops.StampSessionClaim = func(sessionID, beadID string) error {
			return stampSessionCurrentClaimVia(func() (*session.Store, error) { return e.front, nil }, sessionID, beadID)
		}
		ops.ForgetSessionClaim = func(sessionID, beadID string) error {
			return forgetSessionClaimVia(func() (*session.Store, error) { return e.front, nil }, sessionID, beadID)
		}
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

// AC-2: the dropped bead is taken back off the session's current-claim stamp,
// by a CAS on its id, before any later claim in the same invocation re-stamps.
// A stamp naming other work stays: the drop never clears blind.
func TestHookClaimPurgedAdoptionClearsOnlyItsOwnStamp(t *testing.T) {
	const forgetDead = "forget " + purgedAdoptionSessionID + " work-1"
	for _, tc := range []struct {
		name       string
		extraRows  []string
		stamp      string
		forgetErr  error
		wantCode   int
		wantOps    []string
		wantStamp  string
		wantStderr string
	}{
		{name: "stamp names the dead bead", stamp: "work-1", wantCode: 1, wantOps: []string{forgetDead}, wantStamp: ""},
		{
			name: "cleared before the next claim restamps", extraRows: []string{purgedAdoptionOpenRow}, stamp: "work-1",
			wantCode: 0, wantOps: []string{forgetDead, "stamp " + purgedAdoptionSessionID + " work-2"}, wantStamp: "work-2",
		},
		{name: "empty stamp", stamp: "", wantCode: 1, wantOps: []string{forgetDead}, wantStamp: ""},
		{name: "stamp names other live work", stamp: "work-9", wantCode: 1, wantOps: []string{forgetDead}, wantStamp: "work-9"},
		{
			name: "a failed forget changes no exit code", stamp: "work-1", forgetErr: errors.New("session store down"),
			wantCode: 1, wantOps: []string{forgetDead}, wantStamp: "work-1", wantStderr: "session store down",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &purgedAdoptionEnv{
				extraRows: tc.extraRows,
				missing:   map[string]bool{"work-1": true},
				prove:     proveAbsent,
				session:   true,
				stamp:     tc.stamp,
				forgetErr: tc.forgetErr,
			}

			code := e.run()

			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stdout=%s stderr=%s", code, tc.wantCode, e.stdout.String(), e.stderr.String())
			}
			if strings.Join(e.stampOps, ",") != strings.Join(tc.wantOps, ",") {
				t.Fatalf("session stamp ops = %q, want %q", e.stampOps, tc.wantOps)
			}
			if e.stamp != tc.wantStamp {
				t.Errorf("current claim = %q, want %q", e.stamp, tc.wantStamp)
			}
			if tc.wantStderr != "" && !strings.Contains(e.stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to report %q", e.stderr.String(), tc.wantStderr)
			}
		})
	}

	t.Run("no session forgets nothing", func(t *testing.T) {
		e := &purgedAdoptionEnv{missing: map[string]bool{"work-1": true}, prove: proveAbsent, stamp: "work-1"}

		if code := e.run(); code != 1 {
			t.Fatalf("code = %d, want 1; stderr=%s", code, e.stderr.String())
		}
		if len(e.stampOps) != 0 {
			t.Fatalf("session stamp ops = %q, want none without a session id", e.stampOps)
		}
	})
}

// purgedAdoptionSessionStore holds the seat's session row, stamped with claim.
func purgedAdoptionSessionStore(claim string) *beads.MemStore {
	return beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: purgedAdoptionSessionID, Type: session.BeadType, Status: "open", Title: "session",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"state": string(session.StateActive), beadmeta.CurrentClaimBeadIDMetadataKey: claim},
	}}, nil)
}

// sessionClaimStampRacer lands a newer claim stamp on a session row just
// before the first write to it, blind or CAS: another `gc hook --claim` of
// the same session stamping its own claim after this one read the stamp.
type sessionClaimStampRacer struct {
	*beads.MemStore
	t     *testing.T
	stamp string
	raced bool
}

func (r *sessionClaimStampRacer) SetMetadata(id, key, value string) error {
	r.landRace(id)
	return r.MemStore.SetMetadata(id, key, value)
}

func (r *sessionClaimStampRacer) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	r.landRace(id)
	return r.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
}

// landRace is the fixture's write, outside the store adapter the session
// guards look past (storeWriteMethod).
func (r *sessionClaimStampRacer) landRace(id string) {
	if r.raced {
		return
	}
	r.raced = true
	if err := r.MemStore.SetMetadata(id, beadmeta.CurrentClaimBeadIDMetadataKey, r.stamp); err != nil {
		r.t.Errorf("landing the racing stamp: %v", err)
	}
}

// The drop takes its claim back through the production session seams, over a
// real session row: the dead bead's stamp is cleared, and a claim another
// invocation of the session stamps between this one's read of the row and
// its write survives the drop.
func TestHookClaimPurgedAdoptionKeepsAClaimStampedMeanwhile(t *testing.T) {
	claimOf := func(t *testing.T, mem *beads.MemStore) string {
		t.Helper()
		row, err := mem.Get(purgedAdoptionSessionID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]
	}

	t.Run("the dead bead's stamp is cleared", func(t *testing.T) {
		mem := purgedAdoptionSessionStore("work-1")
		e := &purgedAdoptionEnv{
			missing: map[string]bool{"work-1": true},
			prove:   proveAbsent,
			session: true,
			front:   session.NewStore(beads.SessionStore{Store: mem}),
		}

		if code := e.run(); code != 1 {
			t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, e.stdout.String(), e.stderr.String())
		}
		if got := claimOf(t, mem); got != "" {
			t.Fatalf("current claim = %q, want the dead bead's stamp cleared; stderr=%s", got, e.stderr.String())
		}
	})

	t.Run("a claim stamped meanwhile survives", func(t *testing.T) {
		mem := purgedAdoptionSessionStore("work-1")
		racer := &sessionClaimStampRacer{MemStore: mem, t: t, stamp: "work-2"}
		e := &purgedAdoptionEnv{
			missing: map[string]bool{"work-1": true},
			prove:   proveAbsent,
			session: true,
			front:   session.NewStore(beads.SessionStore{Store: racer}),
		}

		if code := e.run(); code != 1 {
			t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, e.stdout.String(), e.stderr.String())
		}
		if !racer.raced {
			t.Fatal("the drop wrote nothing to the session row, so the racing stamp never landed")
		}
		if got := claimOf(t, mem); got != "work-2" {
			t.Fatalf("current claim = %q, want the claim stamped meanwhile (work-2)", got)
		}
		if strings.Contains(e.stderr.String(), "leaving session") {
			t.Errorf("stderr = %q, want a lost CAS reported as no failure", e.stderr.String())
		}
	})
}

// forgetSessionClaimVia takes a claim back off the session row through the
// session front door the stamp is written through.
func TestForgetSessionClaimVia(t *testing.T) {
	via := func(front *session.Store) sessionFrontGetter {
		return func() (*session.Store, error) { return front, nil }
	}

	t.Run("clears the stamp naming the bead", func(t *testing.T) {
		front := hookCurrentFrontDoor(t, "work-1")
		if err := forgetSessionClaimVia(via(front), hookCurrentSessionID, "work-1"); err != nil {
			t.Fatalf("forgetSessionClaimVia: %v", err)
		}
		if got, err := front.CurrentClaimBeadID(hookCurrentSessionID); err != nil || got != "" {
			t.Fatalf("current claim = (%q, %v), want it cleared", got, err)
		}
	})

	t.Run("leaves a stamp naming other work", func(t *testing.T) {
		front := hookCurrentFrontDoor(t, "work-1")
		if err := forgetSessionClaimVia(via(front), hookCurrentSessionID, "work-9"); err != nil {
			t.Fatalf("forgetSessionClaimVia: %v", err)
		}
		if got, err := front.CurrentClaimBeadID(hookCurrentSessionID); err != nil || got != "work-1" {
			t.Fatalf("current claim = (%q, %v), want work-1 left in place", got, err)
		}
	})

	t.Run("wraps a front-door error", func(t *testing.T) {
		boom := errors.New("front door down")
		err := forgetSessionClaimVia(func() (*session.Store, error) { return nil, boom }, hookCurrentSessionID, "work-1")
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), hookCurrentSessionID) {
			t.Fatalf("err = %v, want the front-door error wrapped with the session id", err)
		}
	})

	t.Run("names an unreadable session", func(t *testing.T) {
		front := hookCurrentFrontDoor(t, "work-1")
		err := forgetSessionClaimVia(via(front), "mc-absent", "work-1")
		if !errors.Is(err, beads.ErrNotFound) || !strings.Contains(err.Error(), "mc-absent") {
			t.Fatalf("err = %v, want the absent session's not-found, naming it", err)
		}
	})

	t.Run("empty id never opens the front door", func(t *testing.T) {
		opened := false
		err := forgetSessionClaimVia(func() (*session.Store, error) { opened = true; return nil, nil }, "  ", "work-1")
		if err != nil || opened {
			t.Fatalf("forgetSessionClaimVia(\"\") = %v opened=%v, want nil without opening", err, opened)
		}
	})
}

// AC-3: a not-found readback without a complete city-wide proof of absence is
// adopted exactly as today — the bead may live in a store the claim's own
// readback does not read — and the stamp is left alone.
func TestHookClaimPurgedAdoptionNeedsACompleteProof(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prove      func([]string) (bool, error)
		wantProofs int
		wantStderr string
	}{
		{name: "nil prover", prove: nil, wantProofs: 0, wantStderr: "without a canonical ownership readback"},
		{name: "found somewhere", prove: func([]string) (bool, error) { return false, nil }, wantProofs: 1, wantStderr: "without a canonical ownership readback"},
		{name: "proof failed", prove: func([]string) (bool, error) { return false, errors.New("rig store unreachable") }, wantProofs: 1, wantStderr: "rig store unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &purgedAdoptionEnv{
				missing: map[string]bool{"work-1": true},
				prove:   tc.prove,
				session: true,
				stamp:   "work-1",
			}

			code := e.run()

			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
			}
			if !strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) || !strings.Contains(e.stdout.String(), `"bead_id":"work-1"`) {
				t.Fatalf("stdout = %q, want work-1 adopted as existing_assignment", e.stdout.String())
			}
			if len(e.proofs) != tc.wantProofs {
				t.Errorf("proofs = %v, want %d", e.proofs, tc.wantProofs)
			}
			for _, op := range e.stampOps {
				if strings.HasPrefix(op, "forget ") {
					t.Fatalf("session stamp ops = %q, want no forget of an unproven adoption", e.stampOps)
				}
			}
			if e.stamp != "work-1" {
				t.Errorf("current claim = %q, want the unproven adoption's stamp kept", e.stamp)
			}
			if !strings.Contains(e.stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want %q", e.stderr.String(), tc.wantStderr)
			}
		})
	}
}

// AC-4: a readback that failed for any reason but not-found is evidence of
// nothing; the bead is served as today and the prover is never asked.
func TestHookClaimPurgedAdoptionKeepsTheFailOpenFloor(t *testing.T) {
	e := &purgedAdoptionEnv{readbackErr: errors.New("store unreachable"), prove: proveAbsent}

	code := e.run()

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) || !strings.Contains(e.stdout.String(), `"bead_id":"work-1"`) {
		t.Fatalf("stdout = %q, want work-1 served as existing_assignment", e.stdout.String())
	}
	if len(e.proofs) != 0 {
		t.Fatalf("proofs = %v, want the prover never called for a non-not-found readback error", e.proofs)
	}
}

// AC-5: dropping a proven-missing assignment re-selects over the rows left, so
// a second live assignment is adopted in the same invocation rather than the
// seat draining every tick for as long as the stale row is cached.
func TestHookClaimPurgedAdoptionReselectsTheNextAssignment(t *testing.T) {
	e := &purgedAdoptionEnv{
		extraRows: []string{`{"id":"work-2","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}`},
		missing:   map[string]bool{"work-1": true},
		prove:     proveAbsent,
	}

	code := e.run()

	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%s stderr=%s", code, e.stdout.String(), e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), `"reason":"existing_assignment"`) || !strings.Contains(e.stdout.String(), `"bead_id":"work-2"`) {
		t.Fatalf("stdout = %q, want work-2 adopted as existing_assignment", e.stdout.String())
	}
	if strings.Join(e.reads, ",") != "work-1,work-2" {
		t.Fatalf("canonical reads = %v, want [work-1 work-2]", e.reads)
	}
}

// A proven-missing row never reaches the stopped-workflow gate: there is no
// bead left to close, and the gate would only try to write to one.
func TestHookClaimPurgedAdoptionSkipsTheStopGate(t *testing.T) {
	e := &purgedAdoptionEnv{
		deadRow: `{"id":"work-1","status":"in_progress","assignee":"worker-1",` +
			`"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1"}}`,
		missing: map[string]bool{"work-1": true},
		prove:   proveAbsent,
	}

	if code := e.run(); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, e.stderr.String())
	}
	for _, id := range e.stopReads {
		if id == "work-1" {
			t.Fatalf("stop gate opened the workflow store for %v, want the missing work-1 never gated", e.stopReads)
		}
	}
}
