package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCurrentSessionID is the session bead id every case in this file resolves;
// the absent-session case deliberately asks for a different one.
const hookCurrentSessionID = "mc-sess1"

// hookCurrentSessionBead is the session row every case reads: active, named,
// claimable under its own bead id — the first identity of every release scope,
// and the one an unaliased pool claim is recorded under. claim is the stamped
// current claim ("" ⇒ nothing stamped); claims are the work_started cell's
// entries, newest first.
func hookCurrentSessionBead(claim string, claims ...session.WorkStartedEntry) beads.Bead {
	meta := map[string]string{
		"state":        string(session.StateActive),
		"session_name": "mc-worker-1",
	}
	if claim != "" {
		meta[beadmeta.CurrentClaimBeadIDMetadataKey] = claim
	}
	if len(claims) > 0 {
		meta[session.WorkStartedKey] = session.EncodeWorkStarted(session.WorkStarted{Claims: claims})
	}
	return beads.Bead{
		ID:       hookCurrentSessionID,
		Type:     session.BeadType,
		Status:   "open",
		Title:    "session",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

// hookCurrentEntry is one recorded hook delivery of bead id.
func hookCurrentEntry(id string) session.WorkStartedEntry {
	return session.WorkStartedEntry{Bead: id, At: time.Now().UTC(), Via: session.WorkStartedViaClaim}
}

// hookCurrentWorkBead is a qualifying current claim: a work row in progress,
// held by the session, and assigned under the session's bead id.
func hookCurrentWorkBead(id string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     "task",
		Status:   "in_progress",
		Title:    "step " + id,
		Assignee: hookCurrentSessionID,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: hookCurrentSessionID},
	}
}

// hookCurrentTestContext is the verified read context over one in-memory
// single-store city holding rows: the session front door over the store, and
// the work legs a controller of that city would mint over it.
func hookCurrentTestContext(t *testing.T, rows ...beads.Bead) hookCurrentContext {
	t.Helper()
	return hookCurrentContextOver(t, beads.NewMemStoreFrom(1, rows, nil))
}

// hookCurrentContextOver is the verified read context over one already-built
// store, for the cases that wrap the store to fail a chosen read.
func hookCurrentContextOver(t *testing.T, store beads.Store) hookCurrentContext {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "hook-current-city"}}
	return hookCurrentContext{
		sessFront: session.NewStore(beads.SessionStore{Store: store}),
		cfg:       cfg,
		legs:      testWorkLegs("", cfg, store, nil),
	}
}

// hookCurrentFailingGetStore serves every read from the embedded store except
// the ids in fail, whose Get reports the mapped error: the live read path
// (WorkLegs.readLive reads through HandlesFor(...).Live, whose logical reader
// delegates to Get) then errors for exactly those ids.
type hookCurrentFailingGetStore struct {
	beads.Store
	fail map[string]error
}

// Get serves every id but fail's, which reports its mapped error.
func (s hookCurrentFailingGetStore) Get(id string) (beads.Bead, error) {
	if err, ok := s.fail[id]; ok {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

// hookCurrentSplitContext is the verified read context over a splitEnv city:
// the session front door over the city's real sessions-class front door, and
// the work legs a controller of that city would mint (the fixture registers
// its routes, so testWorkLegs resolves the same residency production does).
func hookCurrentSplitContext(t *testing.T, e splitEnv) hookCurrentContext {
	t.Helper()
	return hookCurrentContext{
		sessFront: session.NewStore(beads.SessionStore{Store: e.sessionsStore()}),
		cfg:       e.cfg,
		legs:      testWorkLegs(e.cityPath, e.cfg, e.store, nil),
	}
}

// hookCurrentSeedFrozenLedgerCopy plants a pre-migration copy of id under the
// work ledger's policy fence, the way `gc storage migrate` leaves one behind:
// a forced foreign-id create the ledger's own policy would refuse.
func hookCurrentSeedFrozenLedgerCopy(t *testing.T, e splitEnv, row beads.Bead) {
	t.Helper()
	ledger := e.work
	if policy, wrapped := ledger.(*beadPolicyStore); wrapped {
		ledger = policy.Store // the forced create sits below the policy fence, as gc storage migrate's does
	}
	forcer, ok := ledger.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("the work store %T cannot model a retained pre-migration copy", e.work)
	}
	if _, err := forcer.CreateWithForeignID(row); err != nil {
		t.Fatalf("seeding the stale ledger copy: %v", err)
	}
}

// TestDoHookCurrentPrintsTheStampedClaim is the primary read-back test: a
// stamp the live ledger still backs — the row is in progress, held by this
// session, and assigned to it — prints exactly, and --id-only prints the bare
// id so `$(gc hook current --id-only)` substitutes cleanly into a shell
// derivation. A verified stamp needs no disclosure: stderr stays empty.
func TestDoHookCurrentPrintsTheStampedClaim(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-42", hookCurrentEntry("gcg-42")),
		hookCurrentWorkBead("gcg-42"),
	)

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent(--id-only) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Fatalf("--id-only stdout = %q, want exactly the bead id", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("--id-only stderr = %q, want empty: a verified stamp needs no disclosure", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doHookCurrent(ctx, hookCurrentSessionID, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent = %d, want 0; stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "gcg-42") || !strings.Contains(got, hookCurrentSessionID) {
		t.Fatalf("stdout = %q, want the bead id plus the session it was claimed by", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// TestDoHookCurrentRefusesAStampTheLedgerContradicts pins why the stamp is
// verified at all: the stamp write is best-effort, so a stamp can keep naming
// a claim the ledger has since closed. A row that is not still this session's
// live in-progress work is not printed, whatever the stamp says — the printed
// id is a close target, and a closed bead is not one.
func TestDoHookCurrentRefusesAStampTheLedgerContradicts(t *testing.T) {
	closed := hookCurrentWorkBead("gcg-old")
	closed.Status = "closed"
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-old", hookCurrentEntry("gcg-old")),
		closed,
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (stale stamp) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed for a contradicted stamp", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg-old") {
		t.Errorf("stderr = %q, want the refused stamp named", stderr.String())
	}
}

// TestDoHookCurrentReplacesClosedStampWithLiveStartedClaim is the recovery
// case the work_started cell exists for: the stamp names a claim the ledger
// has since closed, but the session's recorded deliveries still name exactly
// one live claim. That claim is the session's current work — it is what the
// step must close — so it prints (stdout stays the machine-clean id for
// --id-only) and the substitution is disclosed on stderr only.
func TestDoHookCurrentReplacesClosedStampWithLiveStartedClaim(t *testing.T) {
	closed := hookCurrentWorkBead("gcg-old")
	closed.Status = "closed"
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-old",
			hookCurrentEntry("gcg-live"),
			hookCurrentEntry("gcg-old"),
		),
		closed,
		hookCurrentWorkBead("gcg-live"),
	)

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent (stale stamp, one live recorded claim) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-live\n" {
		t.Fatalf("--id-only stdout = %q, want the recovered live claim id", got)
	}
	errText := stderr.String()
	if !strings.Contains(errText, "gcg-old") || !strings.Contains(errText, "gcg-live") {
		t.Errorf("stderr = %q, want the refused stamp and the printed claim both disclosed", errText)
	}
}

// TestDoHookCurrentPrintsTheOnlyRecordedClaimWhenNothingIsStamped covers the
// stamp write that never landed: nothing is stamped, but the session's
// recorded deliveries name exactly one live claim. The claim is real and
// provably the session's, so it prints with the absence disclosed on stderr;
// an unstamped session is no longer a blanket refusal.
func TestDoHookCurrentPrintsTheOnlyRecordedClaimWhenNothingIsStamped(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("", hookCurrentEntry("gcg-solo")),
		hookCurrentWorkBead("gcg-solo"),
	)

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent (no stamp, one live recorded claim) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-solo\n" {
		t.Fatalf("--id-only stdout = %q, want the recorded claim id", got)
	}
	if !strings.Contains(stderr.String(), "gcg-solo") {
		t.Errorf("stderr = %q, want the recovered claim disclosed", stderr.String())
	}
}

// TestDoHookCurrentRefusesAStampContradictedByANewerLiveDelivery is the
// conflict arm: the stamp's own row still qualifies, but the cell recorded a
// NEWER delivery (listed before the stamp, newest first) whose work is also
// still live. The stamp is then not the session's most recent live claim —
// printing it could close superseded work while its successor goes unclaimed
// — so the command refuses, naming both.
func TestDoHookCurrentRefusesAStampContradictedByANewerLiveDelivery(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-stamp",
			hookCurrentEntry("gcg-newer"),
			hookCurrentEntry("gcg-stamp"),
		),
		hookCurrentWorkBead("gcg-stamp"),
		hookCurrentWorkBead("gcg-newer"),
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (stamp contradicted by newer live delivery) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed for a contradicted stamp", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "gcg-stamp") || !strings.Contains(errText, "gcg-newer") {
		t.Errorf("stderr = %q, want both the stamp and the newer claim named", errText)
	}
}

// TestDoHookCurrentPrintsAStampNoNewerDeliveryContradicts is the no-false-
// positive arm of the conflict scan: a newer recorded delivery exists, but its
// work is no longer live (closed here), so nothing contradicts the stamp and
// it prints with no disclosure — a verified stamp stays silent on stderr.
func TestDoHookCurrentPrintsAStampNoNewerDeliveryContradicts(t *testing.T) {
	finished := hookCurrentWorkBead("gcg-newer")
	finished.Status = "closed"
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-stamp",
			hookCurrentEntry("gcg-newer"),
			hookCurrentEntry("gcg-stamp"),
		),
		hookCurrentWorkBead("gcg-stamp"),
		finished,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent (newer delivery not live) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-stamp\n" {
		t.Fatalf("--id-only stdout = %q, want the stamp", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty: nothing contradicts a verified stamp", stderr.String())
	}
}

// TestDoHookCurrentNamesTheClaimsWhenSeveralRemain pins the ambiguity
// diagnostic: several live recorded claims is a state no automation may
// silently resolve, so the refusal names every conflicting id for the
// operator the cell left holding all of them.
func TestDoHookCurrentNamesTheClaimsWhenSeveralRemain(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("",
			hookCurrentEntry("gcg-y"),
			hookCurrentEntry("gcg-z"),
		),
		hookCurrentWorkBead("gcg-y"),
		hookCurrentWorkBead("gcg-z"),
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (several live claims) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed while ambiguous", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "gcg-y") || !strings.Contains(errText, "gcg-z") {
		t.Errorf("stderr = %q, want every live claim named", errText)
	}
}

// TestDoHookCurrentExitsOneWhenNothingIsStamped is the fail-loud half of the
// contract: a session with nothing stamped AND nothing recorded live must NOT
// print an empty line and exit 0, or a caller substituting the output would
// silently derive an empty bead id — the exact fail-open the back-channel
// exists to close.
func TestDoHookCurrentExitsOneWhenNothingIsStamped(t *testing.T) {
	ctx := hookCurrentTestContext(t, hookCurrentSessionBead(""))

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (unstamped) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed when there is no claim", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no current claim") {
		t.Errorf("stderr = %q, want a message naming the missing claim", stderr.String())
	}
}

// TestDoHookCurrentRefusesAMalformedWorkStartedCell pins the decode arm: the
// cell is read before any decision, and a cell that cannot decode refuses —
// it may hold the only record of the session's live claim, so a caller must
// never receive an id decided over an unreadable cell.
func TestDoHookCurrentRefusesAMalformedWorkStartedCell(t *testing.T) {
	sess := hookCurrentSessionBead("gcg-42", hookCurrentEntry("gcg-42"))
	sess.Metadata[session.WorkStartedKey] = "{not a work_started cell"
	ctx := hookCurrentTestContext(t, sess, hookCurrentWorkBead("gcg-42"))

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (malformed cell) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed over an undecodable cell", stdout.String())
	}
	if !strings.Contains(stderr.String(), hookCurrentSessionID) {
		t.Errorf("stderr = %q, want the session named", stderr.String())
	}
}

// TestDoHookCurrentExitsOneNamingTheClaimAReadFailed pins the read-error arm:
// a live read that errors is an error, not a miss and not a print — the id
// this command hands out is a close target, and an unreadable claim must not
// be guessed at. The refusal names the id it failed to read.
func TestDoHookCurrentExitsOneNamingTheClaimAReadFailed(t *testing.T) {
	ctx := hookCurrentContextOver(t, hookCurrentFailingGetStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{
			hookCurrentSessionBead("gcg-42", hookCurrentEntry("gcg-42")),
			hookCurrentWorkBead("gcg-42"),
		}, nil),
		fail: map[string]error{"gcg-42": errors.New("dolt: connection refused")},
	})

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (unreadable claim) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed for an unreadable claim", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "reading claim gcg-42") || !strings.Contains(errText, "connection refused") {
		t.Errorf("stderr = %q, want the failed read named with its id and cause", errText)
	}
}

// TestDoHookCurrentRefusesAMissingClaimWhileCoverageIsIncomplete is the
// coverage fence: a claim id no leg can find is NOT "gone" while a bound rig
// store failed to open — the unopened rig may hold the row, so absence is
// unprovable and the command refuses naming the short rig, rather than
// telling a caller its claim vanished.
func TestDoHookCurrentRefusesAMissingClaimWhileCoverageIsIncomplete(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-missing", hookCurrentEntry("gcg-missing")),
	)
	ctx.short = []rigStoreOpenFailure{{rig: "rig-1", err: errors.New("dolt server down")}}

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (missing claim, short coverage) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "reading claim gcg-missing") || !strings.Contains(errText, "rig-1") || !strings.Contains(errText, "cannot be proven") {
		t.Errorf("stderr = %q, want the missing claim, the short rig, and the unprovability named", errText)
	}
	if strings.Contains(errText, "no longer held") {
		t.Errorf("stderr = %q, a miss under incomplete coverage must not read as a stale verdict", errText)
	}
}

// TestDoHookCurrentReadsAMissingClaimAsGoneUnderFullCoverage is the fence's
// complement: with every bound rig open, a claim no leg can find is provably
// absent — a stale verdict ("gone"), not an error, and the command refuses on
// the stale arm instead of blaming the stores.
func TestDoHookCurrentReadsAMissingClaimAsGoneUnderFullCoverage(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-missing", hookCurrentEntry("gcg-missing")),
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (missing claim, full coverage) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gone") {
		t.Errorf("stderr = %q, want the miss read as gone, not as a store failure", stderr.String())
	}
}

// TestDoHookCurrentAbortsTheScanWhenALaterEntryFailsToRead pins the abort: a
// candidate found early in the cell does not print when a later entry's read
// errors — the entries never reached could have made the count two, and
// printing the first would be the guess the command exists to avoid.
func TestDoHookCurrentAbortsTheScanWhenALaterEntryFailsToRead(t *testing.T) {
	ctx := hookCurrentContextOver(t, hookCurrentFailingGetStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{
			hookCurrentSessionBead("",
				hookCurrentEntry("gcg-ok"),
				hookCurrentEntry("gcg-bad"),
			),
			hookCurrentWorkBead("gcg-ok"),
		}, nil),
		fail: map[string]error{"gcg-bad": errors.New("dolt: connection refused")},
	})

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (later entry unreadable) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed: an aborted scan has no sole claim", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg-bad") {
		t.Errorf("stderr = %q, want the unreadable entry named", stderr.String())
	}
}

// TestDoHookCurrentPrintsARelocatedClaimThroughItsBinding runs the verified
// print over both topologies (AC-5): the claimed step is graph class, so a
// split city keeps it in the class binding, and the binding's answer decides.
// On the split city the work ledger still holds the frozen pre-migration copy
// `gc storage migrate` left behind, marked closed — it must not be consulted,
// and the binding's live in_progress row prints through.
func TestDoHookCurrentPrintsARelocatedClaimThroughItsBinding(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		sess, err := e.sessionsStore().Create(beads.Bead{
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"state":        string(session.StateActive),
				"session_name": "hc-worker-1",
			},
		})
		if err != nil {
			t.Fatalf("creating the session bead: %v", err)
		}
		step := mintDurableGraphBead(t, e, "routed step", "")
		if err := e.graphStore().Update(step.ID, beads.UpdateOpts{
			Status:   ptrString("in_progress"),
			Assignee: ptrString(sess.ID),
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: sess.ID},
		}); err != nil {
			t.Fatalf("claiming the step: %v", err)
		}
		// The stamp alone drives both directions here: AC-5's lens is which
		// store answers for the claim row, not the cell (whose recovery is
		// covered in the single-store suite above). SetCurrentClaim is the
		// claim path's own marker write and needs no conditional writer.
		front := session.NewStore(beads.SessionStore{Store: e.sessionsStore()})
		if ok, err := front.SetCurrentClaim(sess.ID, step.ID); err != nil || !ok {
			t.Fatalf("stamping the claim: ok=%v err=%v", ok, err)
		}
		if e.split {
			hookCurrentSeedFrozenLedgerCopy(t, e, beads.Bead{ID: step.ID, Title: "frozen copy", Type: "task", Status: "closed"})
		}

		ctx := hookCurrentSplitContext(t, e)
		var stdout, stderr bytes.Buffer
		if code := doHookCurrent(ctx, sess.ID, true, &stdout, &stderr); code != 0 {
			t.Fatalf("split=%v: doHookCurrent = %d, want 0; stderr=%s", e.split, code, stderr.String())
		}
		if got := stdout.String(); got != step.ID+"\n" {
			t.Fatalf("split=%v: stdout = %q, want the binding's live claim %s", e.split, got, step.ID)
		}
	})
}

// TestDoHookCurrentLetsAFrozenCopyEstablishNothing is AC-5's other direction:
// the binding's copy has moved on (closed), while the work ledger's frozen
// pre-migration copy still carries the claimed shape. A copy the migration
// left behind cannot establish ownership, so the command refuses on the
// binding's verdict — the frozen row is never an answer.
func TestDoHookCurrentLetsAFrozenCopyEstablishNothing(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		sess, err := e.sessionsStore().Create(beads.Bead{
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"state":        string(session.StateActive),
				"session_name": "hc-worker-1",
			},
		})
		if err != nil {
			t.Fatalf("creating the session bead: %v", err)
		}
		step := mintDurableGraphBead(t, e, "routed step", "")
		if err := e.graphStore().Update(step.ID, beads.UpdateOpts{
			Status:   ptrString("closed"),
			Assignee: ptrString(sess.ID),
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: sess.ID},
		}); err != nil {
			t.Fatalf("closing the step in its owner: %v", err)
		}
		// The stamp alone drives both directions here: AC-5's lens is which
		// store answers for the claim row, not the cell (whose recovery is
		// covered in the single-store suite above). SetCurrentClaim is the
		// claim path's own marker write and needs no conditional writer.
		front := session.NewStore(beads.SessionStore{Store: e.sessionsStore()})
		if ok, err := front.SetCurrentClaim(sess.ID, step.ID); err != nil || !ok {
			t.Fatalf("stamping the claim: ok=%v err=%v", ok, err)
		}
		if e.split {
			// The frozen copy still carries the claimed shape; only its owner
			// has moved on.
			hookCurrentSeedFrozenLedgerCopy(t, e, beads.Bead{
				ID: step.ID, Title: "frozen copy", Type: "task", Status: "in_progress",
				Assignee:  sess.ID,
				Metadata:  map[string]string{beadmeta.SessionIDMetadataKey: sess.ID},
				Ephemeral: false,
			})
		}

		ctx := hookCurrentSplitContext(t, e)
		var stdout, stderr bytes.Buffer
		if code := doHookCurrent(ctx, sess.ID, true, &stdout, &stderr); code != 1 {
			t.Fatalf("split=%v: doHookCurrent = %d, want 1: the binding closed the claim", e.split, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("split=%v: stdout = %q, want nothing printed from a frozen copy's shape", e.split, stdout.String())
		}
		if !strings.Contains(stderr.String(), "closed") {
			t.Errorf("split=%v: stderr = %q, want the owner's closed verdict named", e.split, stderr.String())
		}
	})
}

// TestDoHookCurrentRecoversASessionlessDeliveryUnderScopeIdentities is AC-6's
// positive arm: a control delivery whose row carries no session metadata at
// all — a pre-session-id claim shape, or a writer that never stamps it — is
// still provably this session's when its assignee is one of the identities
// the session releases work under (here its session_name, not its bead id),
// so the recovery prints it. Actor env (BEADS_ACTOR, GC_AGENT) has no vote:
// the verdict is ledger state, not who is asking.
func TestDoHookCurrentRecoversASessionlessDeliveryUnderScopeIdentities(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "garbage-actor")
	t.Setenv("GC_AGENT", "garbage-agent")
	row := hookCurrentWorkBead("gcg-ctl")
	row.Assignee = "mc-worker-1"       // the session's session_name identity, not its bead id
	row.Metadata = map[string]string{} // no gc.session_id: the assignee alone carries the ownership
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("", hookCurrentEntry("gcg-ctl")),
		row,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent (sessionless delivery, scope assignee) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-ctl\n" {
		t.Fatalf("--id-only stdout = %q, want the recovered control delivery", got)
	}
}

// TestDoHookCurrentRefusesARowAnotherSessionOwns is AC-6's foreign-metadata
// arm: the row is in progress and assigned to this session's bead id, but its
// gc.session_id names ANOTHER session — the shared fence's stale-stamp case.
// Whatever the assignee says, the row is not this session's to close.
func TestDoHookCurrentRefusesARowAnotherSessionOwns(t *testing.T) {
	row := hookCurrentWorkBead("gcg-x")
	row.Metadata[beadmeta.SessionIDMetadataKey] = "mc-other"
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-x", hookCurrentEntry("gcg-x")),
		row,
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (foreign gc.session_id) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed for another session's row", stdout.String())
	}
	if !strings.Contains(stderr.String(), "claimed by another session") {
		t.Errorf("stderr = %q, want the foreign-session refusal named", stderr.String())
	}
}

// TestDoHookCurrentRefusesARowAssignedOutsideTheScope is AC-6's outside-
// assignee arm: the row carries this session's gc.session_id but is assigned
// to an identity outside its release scope. The drain veto would hold this
// row open; the id this command prints is a close target, so its bar is
// higher — an assignee the session cannot release under is not its claim.
func TestDoHookCurrentRefusesARowAssignedOutsideTheScope(t *testing.T) {
	row := hookCurrentWorkBead("gcg-x")
	row.Assignee = "someone-else"
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-x", hookCurrentEntry("gcg-x")),
		row,
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, hookCurrentSessionID, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (assignee outside the scope) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed for an out-of-scope assignee", stdout.String())
	}
	if !strings.Contains(stderr.String(), "assigned to another identity") {
		t.Errorf("stderr = %q, want the out-of-scope refusal named", stderr.String())
	}
}

// TestDoHookCurrentExitsOneOnAnUnreadableSession covers the store arm: an id the
// session store cannot resolve is an error, not "nothing claimed", and must not
// print a bead id.
func TestDoHookCurrentExitsOneOnAnUnreadableSession(t *testing.T) {
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-42", hookCurrentEntry("gcg-42")),
		hookCurrentWorkBead("gcg-42"),
	)

	var stdout, stderr bytes.Buffer
	code := doHookCurrent(ctx, "mc-missing", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doHookCurrent (absent session) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc hook current:") {
		t.Errorf("stderr = %q, want a gc hook current diagnostic", stderr.String())
	}
}

// TestCmdHookCurrentRequiresASessionIdentity pins the env arm: outside a session
// there is no bead to read, so the command refuses instead of reporting an empty
// claim, and never opens a store.
func TestCmdHookCurrentRequiresASessionIdentity(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "")
	opened := false
	restore := hookCurrentReadContext
	t.Cleanup(func() { hookCurrentReadContext = restore })
	hookCurrentReadContext = func() (hookCurrentContext, error) {
		opened = true
		return hookCurrentContext{}, errors.New("must not be reached")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdHookCurrent(true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdHookCurrent (no GC_SESSION_ID) = %d, want 1", code)
	}
	if opened {
		t.Error("cmdHookCurrent opened the city's stores without a session identity")
	}
	if !strings.Contains(stderr.String(), "no session identity") {
		t.Errorf("stderr = %q, want the missing-identity message", stderr.String())
	}
}

// TestCmdHookCurrentReadsTheCallingSessionFromEnv proves the command resolves the
// CALLING session from GC_SESSION_ID — the same variable the claim path stamped
// under — rather than any other identity variable, and prints through the
// verified read context it opened.
func TestCmdHookCurrentReadsTheCallingSessionFromEnv(t *testing.T) {
	t.Setenv("GC_SESSION_ID", hookCurrentSessionID)
	ctx := hookCurrentTestContext(t,
		hookCurrentSessionBead("gcg-42", hookCurrentEntry("gcg-42")),
		hookCurrentWorkBead("gcg-42"),
	)
	restore := hookCurrentReadContext
	t.Cleanup(func() { hookCurrentReadContext = restore })
	hookCurrentReadContext = func() (hookCurrentContext, error) { return ctx, nil }

	var stdout, stderr bytes.Buffer
	if code := cmdHookCurrent(true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookCurrent = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Fatalf("stdout = %q, want gcg-42", got)
	}
}

// TestHookCurrentIsWiredIntoTheHookCommandFamily proves the subcommand is
// reachable as `gc hook current` with an --id-only flag; a helper that only the
// formula calls is worthless if the CLI never registers it.
func TestHookCurrentIsWiredIntoTheHookCommandFamily(t *testing.T) {
	var stdout, stderr bytes.Buffer
	hook := newHookCmd(&stdout, &stderr)
	sub, _, err := hook.Find([]string{"current"})
	if err != nil || sub == nil || sub.Name() != "current" {
		t.Fatalf("gc hook current not registered: sub=%v err=%v", sub, err)
	}
	if sub.Flags().Lookup("id-only") == nil {
		t.Error("gc hook current has no --id-only flag")
	}
	if sub.Args == nil {
		t.Error("gc hook current accepts arbitrary args; it is scoped to the calling session")
	}
}

// TestHookCurrentDocIdiomPrefersClaimOverTriggerHint pins the documented close
// idiom in the Long help text: it must resolve to the session's own claim
// before ever consulting $GC_TRIGGER_BEAD_ID. That variable is frozen in the
// process environment at exec and is frequently a foreign bead on named
// singleton sessions (mayor, deacon) that also carry it — a step following
// the old chain would close someone else's work. This guards the doc text
// against silently regressing back to preferring the frozen hint.
func TestHookCurrentDocIdiomPrefersClaimOverTriggerHint(t *testing.T) {
	long := newHookCurrentCmd(io.Discard, io.Discard).Long
	if !strings.Contains(long, `BEAD_ID="${GC_BEAD_ID:-$(gc hook current --id-only)}"`) {
		t.Errorf("documented idiom does not prefer the authoritative claim; got:\n%s", long)
	}
	if strings.Contains(long, "${GC_TRIGGER_BEAD_ID:-$(gc hook current") {
		t.Error("documented idiom still consults the frozen GC_TRIGGER_BEAD_ID hint before the claim")
	}
}

// TestHookCurrentDocNamesNoTriggerEnv pins that the Long help text no longer
// documents $GC_TRIGGER_BEAD_ID: no session shell carries a trigger id
// (#5738), so the help must not describe one as present on any seat.
func TestHookCurrentDocNamesNoTriggerEnv(t *testing.T) {
	long := newHookCurrentCmd(io.Discard, io.Discard).Long
	if strings.Contains(long, "GC_TRIGGER_") {
		t.Errorf("help text still documents a GC_TRIGGER_* shell variable; got:\n%s", long)
	}
}

// TestHookCurrentDocNamesTheVerification pins that the Long help text says the
// stamp is verified against the live ledger: the command's contract is no
// longer "print the stamp", and an operator reading the help must learn what
// the exit-1 arms are for.
func TestHookCurrentDocNamesTheVerification(t *testing.T) {
	long := newHookCurrentCmd(io.Discard, io.Discard).Long
	if !strings.Contains(long, "verified") {
		t.Errorf("help text does not say the stamp is verified; got:\n%s", long)
	}
}
