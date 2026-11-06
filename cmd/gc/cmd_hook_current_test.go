package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCurrentSessionID is the session bead id every case in this file resolves;
// the absent-session case deliberately asks for a different one.
const hookCurrentSessionID = "mc-sess1"

// hookCurrentFixture is a single-store city holding one session bead and the
// work it may have claimed, with the verified context `gc hook current` reads.
type hookCurrentFixture struct {
	store *beads.MemStore
	ctx   hookCurrentContext
}

// hookCurrentWork is a work bead with status and assignee, carrying the
// metadata key/value pairs in kv.
func hookCurrentWork(id, status, assignee string, kv ...string) beads.Bead {
	meta := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		meta[kv[i]] = kv[i+1]
	}
	return beads.Bead{ID: id, Type: "task", Title: "step " + id, Status: status, Assignee: assignee, Metadata: meta}
}

// hookCurrentHeld is a work bead this fixture's session holds: in_progress,
// assigned to the session, stamped with its gc.session_id.
func hookCurrentHeld(id string) beads.Bead {
	return hookCurrentWork(id, "in_progress", hookCurrentSessionID, beadmeta.SessionIDMetadataKey, hookCurrentSessionID)
}

// hookCurrentStartedCell is a work_started cell recording beadIDs newest
// first, as the claim path writes it.
func hookCurrentStartedCell(beadIDs ...string) string {
	var w session.WorkStarted
	for _, id := range beadIDs {
		w.Claims = append(w.Claims, session.WorkStartedEntry{Bead: id, Via: session.WorkStartedViaClaim, Gen: 1})
	}
	return session.EncodeWorkStarted(w)
}

// newHookCurrentFixture builds the city: the session bead carries claim as its
// stamp (empty ⇒ nothing stamped) and startedCell as its work_started cell
// (empty ⇒ no cell), beside work.
func newHookCurrentFixture(t *testing.T, claim, startedCell string, work ...beads.Bead) *hookCurrentFixture {
	t.Helper()
	meta := map[string]string{"state": string(session.StateActive), "session_name": "worker-1"}
	if claim != "" {
		meta[beadmeta.CurrentClaimBeadIDMetadataKey] = claim
	}
	if startedCell != "" {
		meta[session.WorkStartedKey] = startedCell
	}
	sess := beads.Bead{
		ID:       hookCurrentSessionID,
		Type:     session.BeadType,
		Status:   "open",
		Title:    "session",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
	store := beads.NewMemStoreFrom(100, append([]beads.Bead{sess}, work...), nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	return &hookCurrentFixture{store: store, ctx: hookCurrentContext{
		sessFront: session.NewStore(beads.SessionStore{Store: store}),
		cfg:       cfg,
		legs:      testWorkLegs("", cfg, store, nil),
	}}
}

// hookCurrentFrontDoor is the fixture context for a session stamped with a
// claim it still holds (empty claim ⇒ nothing stamped, no work).
func hookCurrentFrontDoor(t *testing.T, claim string) hookCurrentContext {
	t.Helper()
	if claim == "" {
		return newHookCurrentFixture(t, "", "").ctx
	}
	return newHookCurrentFixture(t, claim, hookCurrentStartedCell(claim), hookCurrentHeld(claim)).ctx
}

// run runs doHookCurrent over the fixture.
func (f *hookCurrentFixture) run(idOnly bool) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = doHookCurrent(f.ctx, hookCurrentSessionID, idOnly, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestDoHookCurrentPrintsTheStampedClaim is the primary read-back test: a session
// whose bead carries the claim stamp prints it, and --id-only prints the bare id
// so `$(gc hook current --id-only)` substitutes cleanly into a shell derivation.
func TestDoHookCurrentPrintsTheStampedClaim(t *testing.T) {
	sessFront := hookCurrentFrontDoor(t, "gcg-42")

	var stdout, stderr bytes.Buffer
	if code := doHookCurrent(sessFront, "mc-sess1", true, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent(--id-only) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "gcg-42\n" {
		t.Fatalf("--id-only stdout = %q, want exactly the bead id", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("--id-only stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := doHookCurrent(sessFront, "mc-sess1", false, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookCurrent = %d, want 0; stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "gcg-42") || !strings.Contains(got, "mc-sess1") {
		t.Fatalf("stdout = %q, want the bead id plus the session it was claimed by", got)
	}
}

// TestDoHookCurrentExitsOneWhenNothingIsStamped is the fail-loud half of the
// contract: an unstamped session must NOT print an empty line and exit 0, or a
// caller substituting the output would silently derive an empty bead id — the
// exact fail-open the back-channel exists to close.
func TestDoHookCurrentExitsOneWhenNothingIsStamped(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doHookCurrent(hookCurrentFrontDoor(t, ""), "mc-sess1", true, &stdout, &stderr)
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
	var stdout, stderr bytes.Buffer
	code := doHookCurrent(hookCurrentFrontDoor(t, "gcg-42"), "mc-missing", true, &stdout, &stderr)
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
	restore := hookCurrentOpen
	t.Cleanup(func() { hookCurrentOpen = restore })
	hookCurrentOpen = func() (hookCurrentContext, error) {
		opened = true
		return hookCurrentContext{}, errors.New("must not be reached")
	}

	var stdout, stderr bytes.Buffer
	if code := cmdHookCurrent(true, &stdout, &stderr); code != 1 {
		t.Fatalf("cmdHookCurrent (no GC_SESSION_ID) = %d, want 1", code)
	}
	if opened {
		t.Error("cmdHookCurrent opened the session store without a session identity")
	}
	if !strings.Contains(stderr.String(), "no session identity") {
		t.Errorf("stderr = %q, want the missing-identity message", stderr.String())
	}
}

// TestCmdHookCurrentReadsTheCallingSessionFromEnv proves the command resolves the
// CALLING session from GC_SESSION_ID — the same variable the claim path stamped
// under — rather than any other identity variable.
func TestCmdHookCurrentReadsTheCallingSessionFromEnv(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	restore := hookCurrentOpen
	t.Cleanup(func() { hookCurrentOpen = restore })
	hookCurrentOpen = func() (hookCurrentContext, error) {
		return hookCurrentFrontDoor(t, "gcg-42"), nil
	}

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

// TestDoHookCurrentPrintsAQualifyingStampWithCleanStderr is AC-1: a stamp the
// session still holds by the live ledger (in_progress, its own gc.session_id,
// assigned to one of its identities), with no newer live delivery recorded
// ahead of it, prints exactly the id and a newline with nothing on stderr.
// Older live entries behind the stamp in the delivery cell are not read: the
// stamp is the most recent claim.
func TestDoHookCurrentPrintsAQualifyingStampWithCleanStderr(t *testing.T) {
	for _, tc := range []struct {
		name string
		cell string
		work []beads.Bead
	}{
		{name: "no delivery cell", work: []beads.Bead{hookCurrentHeld("gcg-42")}},
		{name: "stamp heads the cell", cell: hookCurrentStartedCell("gcg-42"), work: []beads.Bead{hookCurrentHeld("gcg-42")}},
		{
			name: "older live delivery behind the stamp",
			cell: hookCurrentStartedCell("gcg-42", "gcg-7"),
			work: []beads.Bead{hookCurrentHeld("gcg-42"), hookCurrentHeld("gcg-7")},
		},
		{
			name: "newer delivery no longer held",
			cell: hookCurrentStartedCell("gcg-50", "gcg-42"),
			work: []beads.Bead{hookCurrentHeld("gcg-42"), hookCurrentWork("gcg-50", "closed", hookCurrentSessionID)},
		},
		{
			name: "assigned by session name",
			cell: hookCurrentStartedCell("gcg-42"),
			work: []beads.Bead{hookCurrentWork("gcg-42", "in_progress", "worker-1", beadmeta.SessionIDMetadataKey, hookCurrentSessionID)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookCurrentFixture(t, "gcg-42", tc.cell, tc.work...)
			code, stdout, stderr := f.run(true)
			if code != 0 || stdout != "gcg-42\n" || stderr != "" {
				t.Fatalf("--id-only = (%d, %q, %q), want (0, %q, \"\")", code, stdout, stderr, "gcg-42\n")
			}
		})
	}
}
