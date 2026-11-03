package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The contracts in this file cover the ledger-verified read-back: the stamp
// the claim protocol wrote is a hint until the work ledger agrees it is still
// the calling session's live claim. The original failure (gk-gw1b6): a stamp
// naming a CLOSED step printed as the current claim, and the submit-and-exit
// drain guard, which re-validates what this command prints, refused to let a
// finished session drain. The opposite direction — a stamp naming a live but
// not-this-session's claim, then a close aimed at it — is the hazard the
// ambiguity refusal protects against.

// hookCurrentLedgerStub serves fixed ledger answers and records the calls, so
// a test can assert both the printed verdict and which reads produced it.
type hookCurrentLedgerStub struct {
	readID    string
	work      beads.Bead
	readErr   error
	listActor string
	live      []beads.Bead
	listErr   error
}

func (s *hookCurrentLedgerStub) ledger() *hookCurrentLedger {
	return &hookCurrentLedger{
		readClaim: func(beadID string) (beads.Bead, error) {
			s.readID = beadID
			return s.work, s.readErr
		},
		listLiveClaims: func(actor string) ([]beads.Bead, error) {
			s.listActor = actor
			return s.live, s.listErr
		},
	}
}

// hookCurrentWorkBead builds the work-ledger row a stamp or a live-candidate
// read returns. ownerSession is the gc.session_id claim stamp on the bead
// ("" = none); assignee is the actor identity bd lists by.
func hookCurrentWorkBead(id, status, assignee, ownerSession string) beads.Bead {
	meta := map[string]string{}
	if ownerSession != "" {
		meta[beadmeta.SessionIDMetadataKey] = ownerSession
	}
	return beads.Bead{
		ID:       id,
		Title:    "step",
		Status:   status,
		Type:     "task",
		Assignee: assignee,
		Metadata: meta,
	}
}

const hookCurrentActor = "gk/gastown.furiosa"

// TestHookCurrentPrintsAStampTheLedgerStillHolds is the fast path: a stamp
// whose bead the ledger still holds as this session's claim prints unchanged,
// with clean stderr (the original tests pin empty stderr on a good stamp) and
// no fallback list.
func TestHookCurrentPrintsAStampTheLedgerStillHolds(t *testing.T) {
	stub := &hookCurrentLedgerStub{work: hookCurrentWorkBead("gcg-42", "in_progress", hookCurrentActor, hookCurrentSessionID)}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Errorf("stdout = %q, want exactly the stamped id", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on a live stamp", stderr.String())
	}
	if stub.readID != "gcg-42" {
		t.Errorf("ledger read = %q, want the stamped id", stub.readID)
	}
	if stub.listActor != "" {
		t.Errorf("fallback list ran for actor %q; a live stamp must not consult it", stub.listActor)
	}
}

// TestHookCurrentCorrectsAStaleStampFromTheLedger is the fix's core: every
// way the stamp stops being this session's claim resolves to the ledger's one
// live claim rather than printing the stale id — including the original
// bug's closed-step stamp.
func TestHookCurrentCorrectsAStaleStampFromTheLedger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		work    beads.Bead
		readErr error
	}{
		{name: "step closed by its own session", work: hookCurrentWorkBead("gcg-42", "closed", hookCurrentActor, hookCurrentSessionID)},
		{name: "released back to the queue", work: hookCurrentWorkBead("gcg-42", "open", "", "")},
		{name: "re-claimed by another session", work: hookCurrentWorkBead("gcg-42", "in_progress", hookCurrentActor, "mc-other")},
		{name: "assigned to another actor", work: hookCurrentWorkBead("gcg-42", "in_progress", "gk/other.polecat", "")},
		{name: "gone from the ledger", readErr: beads.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &hookCurrentLedgerStub{
				work:    tc.work,
				readErr: tc.readErr,
				live:    []beads.Bead{hookCurrentWorkBead("gcg-43", "in_progress", hookCurrentActor, hookCurrentSessionID)},
			}
			var stdout, stderr bytes.Buffer
			code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
			}
			if got := stdout.String(); got != "gcg-43\n" {
				t.Errorf("stdout = %q, want the ledger's live claim, not the stale stamp", got)
			}
			if !strings.Contains(stderr.String(), "stale") {
				t.Errorf("stderr = %q, want the correction disclosed", stderr.String())
			}
			if stub.listActor != hookCurrentActor {
				t.Errorf("fallback list actor = %q, want the session's actor identity", stub.listActor)
			}
		})
	}
}

// TestHookCurrentPrintsAStampItCannotReadUnverified keeps the command from
// failing a working session on a store hiccup: an unreadable stamp (as
// opposed to a stale one) prints the stamp with a warning, exactly the
// answer the command gave before it could verify.
func TestHookCurrentPrintsAStampItCannotReadUnverified(t *testing.T) {
	stub := &hookCurrentLedgerStub{readErr: errors.New("dolt: deadlock on write")}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Errorf("stdout = %q, want the stamp printed unverified", got)
	}
	if !strings.Contains(stderr.String(), "unverified") {
		t.Errorf("stderr = %q, want the uncertainty disclosed", stderr.String())
	}
	if stub.listActor != "" {
		t.Error("a transient read error must not fall through to the fallback list")
	}
}

// TestHookCurrentResolvesALiveClaimWhenNothingIsStamped covers the other
// write-side failure shape: the claim recorded no stamp at all (or a rollback
// cleared it). With exactly one live claim, the command answers from the
// ledger instead of failing the session.
func TestHookCurrentResolvesALiveClaimWhenNothingIsStamped(t *testing.T) {
	stub := &hookCurrentLedgerStub{
		live: []beads.Bead{hookCurrentWorkBead("gcg-44", "in_progress", hookCurrentActor, hookCurrentSessionID)},
	}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, ""), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-44\n" {
		t.Errorf("stdout = %q, want the ledger's live claim", got)
	}
	if stub.readID != "" {
		t.Errorf("readClaim(%q) ran with no stamp; the ledger read must follow the stamp", stub.readID)
	}
	if !strings.Contains(stderr.String(), "no claim was stamped") {
		t.Errorf("stderr = %q, want the unstamped resolution disclosed", stderr.String())
	}
}

// TestHookCurrentExitsOneWhenNeitherStampNorLedgerNamesAClaim keeps the
// fail-loud contract: nothing stamped and nothing live is an error, never 0
// with an empty line.
func TestHookCurrentExitsOneWhenNeitherStampNorLedgerNamesAClaim(t *testing.T) {
	stub := &hookCurrentLedgerStub{}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, ""), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no current claim") {
		t.Errorf("stderr = %q, want the no-claim message", stderr.String())
	}
}

// TestHookCurrentExitsOneOnAmbiguousLiveClaims pins the conservative half:
// two live claims is not a cue to pick the newest. Auto-picking would hand a
// close step an id this session never claimed — the wrong-work hazard.
func TestHookCurrentExitsOneOnAmbiguousLiveClaims(t *testing.T) {
	stub := &hookCurrentLedgerStub{
		work: hookCurrentWorkBead("gcg-42", "closed", hookCurrentActor, hookCurrentSessionID),
		live: []beads.Bead{
			hookCurrentWorkBead("gcg-43", "in_progress", hookCurrentActor, ""),
			hookCurrentWorkBead("gcg-44", "in_progress", hookCurrentActor, hookCurrentSessionID),
		},
	}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on an ambiguous ledger", stdout.String())
	}
	for _, want := range []string{"gcg-43", "gcg-44", "refusing"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to name %q", stderr.String(), want)
		}
	}
}

// TestHookCurrentFallbackFiltersClaimsStampedToAnotherSession keeps a
// same-actor sibling session's claim out of the candidate set: assignee is a
// per-actor filter, and only the gc.session_id stamp separates two live
// claims held under the same agent identity.
func TestHookCurrentFallbackFiltersClaimsStampedToAnotherSession(t *testing.T) {
	stub := &hookCurrentLedgerStub{
		live: []beads.Bead{
			hookCurrentWorkBead("gcg-43", "in_progress", hookCurrentActor, "mc-other"),
			hookCurrentWorkBead("gcg-44", "in_progress", hookCurrentActor, ""),
		},
	}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, ""), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-44\n" {
		t.Errorf("stdout = %q, want only the candidate not stamped to another session", got)
	}
}

// TestHookCurrentExitsOneWhenTheLedgerListFails is the honest-error arm: a
// stale stamp plus an unreadable ledger cannot be answered, and printing the
// stale stamp instead would reintroduce the original bug.
func TestHookCurrentExitsOneWhenTheLedgerListFails(t *testing.T) {
	stub := &hookCurrentLedgerStub{
		work:    hookCurrentWorkBead("gcg-42", "closed", hookCurrentActor, hookCurrentSessionID),
		listErr: errors.New("ledger offline"),
	}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "could not be read") {
		t.Errorf("stderr = %q, want the failed ledger read named", stderr.String())
	}
}

// TestHookCurrentStaleStampWithoutAnActorIdentityCannotResolve pins the no
// -identity arm: the stamp was verifiably stale, but without an actor there
// is no assignee-scoped list to repair it from, so the command refuses rather
// than printing either a stale id or a guess.
func TestHookCurrentStaleStampWithoutAnActorIdentityCannotResolve(t *testing.T) {
	stub := &hookCurrentLedgerStub{work: hookCurrentWorkBead("gcg-42", "closed", hookCurrentActor, hookCurrentSessionID)}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, "", stub.ledger(), true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "BEADS_ACTOR") {
		t.Errorf("stderr = %q, want the missing identity named", stderr.String())
	}
	if stub.listActor != "" {
		t.Error("no actor identity means no fallback list")
	}
}

// TestHookCurrentValidStampWithoutActorSkipsOnlyTheAssigneeFence: an actor
// -less shell can still verify status and session ownership; it must not fail
// a stamp it cannot list for.
func TestHookCurrentValidStampWithoutActorSkipsOnlyTheAssigneeFence(t *testing.T) {
	stub := &hookCurrentLedgerStub{work: hookCurrentWorkBead("gcg-42", "in_progress", "any/actor", hookCurrentSessionID)}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, "", stub.ledger(), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Errorf("stdout = %q, want the stamp", got)
	}
}

// TestDoHookCurrentWiresActorAndProbeFromEnv pins the doHookCurrent wiring:
// the actor comes from BEADS_ACTOR (GC_AGENT when that is unset) and the
// ledger from the probe seam — so the production command, not only the
// injected resolver, gets the verified behavior.
func TestDoHookCurrentWiresActorAndProbeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		actor string
	}{
		{name: "BEADS_ACTOR wins", env: map[string]string{"BEADS_ACTOR": hookCurrentActor, "GC_AGENT": "other"}, actor: hookCurrentActor},
		{name: "GC_AGENT is the fallback", env: map[string]string{"BEADS_ACTOR": "", "GC_AGENT": hookCurrentActor}, actor: hookCurrentActor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			stub := &hookCurrentLedgerStub{
				readErr: beads.ErrNotFound,
				live:    []beads.Bead{hookCurrentWorkBead("gcg-50", "in_progress", tc.actor, hookCurrentSessionID)},
			}
			restore := hookCurrentLedgerProbe
			t.Cleanup(func() { hookCurrentLedgerProbe = restore })
			hookCurrentLedgerProbe = func() *hookCurrentLedger { return stub.ledger() }

			var stdout, stderr bytes.Buffer
			if code := doHookCurrent(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
				t.Fatalf("doHookCurrent = %d, want 0; stderr=%s", code, stderr.String())
			}
			if got := stdout.String(); got != "gcg-50\n" {
				t.Errorf("stdout = %q, want the resolved live claim", got)
			}
			if stub.listActor != tc.actor {
				t.Errorf("fallback list actor = %q, want %q", stub.listActor, tc.actor)
			}
		})
	}
}

// TestHookCurrentFullOutputNamesTheResolvingSession pins the non-id-only
// shape after a ledger resolution: the id plus the session, so a human reader
// sees the same claim contract the shell derivation consumes.
func TestHookCurrentFullOutputNamesTheResolvingSession(t *testing.T) {
	stub := &hookCurrentLedgerStub{
		work: hookCurrentWorkBead("gcg-42", "closed", hookCurrentActor, hookCurrentSessionID),
		live: []beads.Bead{hookCurrentWorkBead("gcg-43", "in_progress", hookCurrentActor, hookCurrentSessionID)},
	}
	var stdout, stderr bytes.Buffer
	code := resolveHookCurrentClaim(hookCurrentFrontDoor(t, "gcg-42"), hookCurrentSessionID, hookCurrentActor, stub.ledger(), false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resolveHookCurrentClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-43 (claimed by session "+hookCurrentSessionID+")\n" {
		t.Errorf("stdout = %q, want the full claim line", got)
	}
}
