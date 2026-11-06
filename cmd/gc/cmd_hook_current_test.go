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
	store := beads.NewMemStoreFrom(1, rows, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "hook-current-city"}}
	return hookCurrentContext{
		sessFront: session.NewStore(beads.SessionStore{Store: store}),
		cfg:       cfg,
		legs:      testWorkLegs("", cfg, store, nil),
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
