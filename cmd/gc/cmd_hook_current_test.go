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

// TestDoHookCurrentReplacesClosedStampWithLiveStartedClaim is the reported
// incident (AC-2): the stamp still names a closed earlier claim X while the
// session's delivery cell records its live claim Y ahead of it. The command
// prints Y, not X, and says why only on stderr.
func TestDoHookCurrentReplacesClosedStampWithLiveStartedClaim(t *testing.T) {
	f := newHookCurrentFixture(t, "gcg-x", hookCurrentStartedCell("gcg-y", "gcg-x"),
		hookCurrentWork("gcg-x", "closed", hookCurrentSessionID, beadmeta.SessionIDMetadataKey, hookCurrentSessionID),
		hookCurrentHeld("gcg-y"))
	code, stdout, stderr := f.run(true)
	if code != 0 || stdout != "gcg-y\n" {
		t.Fatalf("--id-only = (%d, %q), want (0, %q); stderr=%s", code, stdout, "gcg-y\n", stderr)
	}
	for _, want := range []string{"gcg-x", "closed", "gcg-y"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want the substitution disclosed naming %q", stderr, want)
		}
	}
}

// TestDoHookCurrentRecoversTheOneLiveDelivery is AC-2 across every way a stamp
// is stale, and an absent one: the single recorded hook delivery the session
// still holds is printed, with the old stamp (or its absence), the reason and
// the replacement on stderr and never on stdout.
func TestDoHookCurrentRecoversTheOneLiveDelivery(t *testing.T) {
	sid := beadmeta.SessionIDMetadataKey
	for _, tc := range []struct {
		name       string
		stamp      string
		stale      []beads.Bead
		wantReason string
	}{
		{name: "closed", stamp: "gcg-old", stale: []beads.Bead{hookCurrentWork("gcg-old", "closed", hookCurrentSessionID, sid, hookCurrentSessionID)}, wantReason: string(claimStaleClosed)},
		{name: "released", stamp: "gcg-old", stale: []beads.Bead{hookCurrentWork("gcg-old", "open", "")}, wantReason: string(claimStaleReleased)},
		{name: "not started", stamp: "gcg-old", stale: []beads.Bead{hookCurrentWork("gcg-old", "blocked", hookCurrentSessionID)}, wantReason: string(claimStaleNotInProgress)},
		{name: "re-claimed by another session", stamp: "gcg-old", stale: []beads.Bead{hookCurrentWork("gcg-old", "in_progress", "mc-other", sid, "mc-other")}, wantReason: string(claimStaleOtherSession)},
		{name: "reassigned", stamp: "gcg-old", stale: []beads.Bead{hookCurrentWork("gcg-old", "in_progress", "someone-else")}, wantReason: string(claimStaleOtherAssignee)},
		{name: "gone", stamp: "gcg-old", wantReason: string(claimStaleGone)},
		{name: "no stamp", wantReason: "no claim stamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cell := hookCurrentStartedCell("gcg-live", "gcg-old")
			f := newHookCurrentFixture(t, tc.stamp, cell, append(tc.stale, hookCurrentHeld("gcg-live"))...)
			code, stdout, stderr := f.run(true)
			if code != 0 || stdout != "gcg-live\n" {
				t.Fatalf("--id-only = (%d, %q), want (0, %q); stderr=%s", code, stdout, "gcg-live\n", stderr)
			}
			if !strings.Contains(stderr, tc.wantReason) || !strings.Contains(stderr, "gcg-live") {
				t.Errorf("stderr = %q, want the reason %q and the replacement disclosed", stderr, tc.wantReason)
			}
			if tc.stamp != "" && !strings.Contains(stderr, tc.stamp) {
				t.Errorf("stderr = %q, want the stale stamp %s named", stderr, tc.stamp)
			}

			code, stdout, _ = f.run(false)
			if code != 0 || stdout != "gcg-live (claimed by session mc-sess1)\n" {
				t.Fatalf("human form = (%d, %q), want the existing format for the recovered claim", code, stdout)
			}
		})
	}
}

// TestDoHookCurrentRefusesWithoutOneVerifiedClaim is AC-3: no live recovery
// candidate, several of them, or a held stamp contradicted by a newer live
// hook delivery all exit 1 with nothing on stdout and a diagnostic naming the
// ids in conflict. Recency never breaks a tie, and work the session holds
// that no hook delivery records is not a candidate.
func TestDoHookCurrentRefusesWithoutOneVerifiedClaim(t *testing.T) {
	closed := func(id string) beads.Bead { return hookCurrentWork(id, "closed", hookCurrentSessionID) }
	for _, tc := range []struct {
		name  string
		stamp string
		cell  string
		work  []beads.Bead
		want  []string
	}{
		{
			name:  "stale stamp, no live delivery",
			stamp: "gcg-old", cell: hookCurrentStartedCell("gcg-a", "gcg-old"),
			work: []beads.Bead{closed("gcg-old"), closed("gcg-a")},
			want: []string{"gcg-old", string(claimStaleClosed), "no recorded hook delivery"},
		},
		{
			name:  "stale stamp, held work no delivery records",
			stamp: "gcg-old",
			work:  []beads.Bead{closed("gcg-old"), hookCurrentHeld("gcg-unrecorded")},
			want:  []string{"gcg-old", "no recorded hook delivery"},
		},
		{
			name: "no stamp, nothing delivered",
			want: []string{"no current claim"},
		},
		{
			name:  "several live deliveries",
			stamp: "gcg-old", cell: hookCurrentStartedCell("gcg-a", "gcg-b", "gcg-old"),
			work: []beads.Bead{closed("gcg-old"), hookCurrentHeld("gcg-a"), hookCurrentHeld("gcg-b")},
			want: []string{"gcg-a", "gcg-b", "several"},
		},
		{
			name: "no stamp, several live deliveries",
			cell: hookCurrentStartedCell("gcg-a", "gcg-b"),
			work: []beads.Bead{hookCurrentHeld("gcg-a"), hookCurrentHeld("gcg-b")},
			want: []string{"gcg-a", "gcg-b", "several"},
		},
		{
			name:  "held stamp behind a newer live delivery",
			stamp: "gcg-x", cell: hookCurrentStartedCell("gcg-y", "gcg-x"),
			work: []beads.Bead{hookCurrentHeld("gcg-x"), hookCurrentHeld("gcg-y")},
			want: []string{"gcg-x", "gcg-y"},
		},
		{
			name:  "held stamp missing from a cell with a live delivery",
			stamp: "gcg-x", cell: hookCurrentStartedCell("gcg-y"),
			work: []beads.Bead{hookCurrentHeld("gcg-x"), hookCurrentHeld("gcg-y")},
			want: []string{"gcg-x", "gcg-y"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookCurrentFixture(t, tc.stamp, tc.cell, tc.work...)
			for _, idOnly := range []bool{true, false} {
				code, stdout, stderr := f.run(idOnly)
				if code != 1 || stdout != "" {
					t.Fatalf("idOnly=%v: = (%d, %q), want exit 1 with empty stdout; stderr=%s", idOnly, code, stdout, stderr)
				}
				for _, want := range tc.want {
					if !strings.Contains(stderr, want) {
						t.Errorf("idOnly=%v: stderr = %q, want it to name %q", idOnly, stderr, want)
					}
				}
			}
		})
	}
}

// TestDoHookCurrentCountsADuplicatedDeliveryOnce: a bead the cell records
// twice is one candidate, not an ambiguity.
func TestDoHookCurrentCountsADuplicatedDeliveryOnce(t *testing.T) {
	f := newHookCurrentFixture(t, "gcg-old", hookCurrentStartedCell("gcg-a", "gcg-a", "gcg-old"),
		hookCurrentWork("gcg-old", "closed", hookCurrentSessionID), hookCurrentHeld("gcg-a"))
	if code, stdout, stderr := f.run(true); code != 0 || stdout != "gcg-a\n" {
		t.Fatalf("--id-only = (%d, %q), want (0, \"gcg-a\\n\"); stderr=%s", code, stdout, stderr)
	}
}

// TestDoHookCurrentRefusesWhatItCannotRead is AC-4: a read or decode failure
// on anything the decision needs exits 1 with nothing on stdout and names the
// failed operation and id, even after another candidate already verified. A
// claim missing from the legs while a leg could not be read is unknown, not
// stale; a claim found in the legs it could read still verifies.
func TestDoHookCurrentRefusesWhatItCannotRead(t *testing.T) {
	closed := hookCurrentWork("gcg-old", "closed", hookCurrentSessionID)
	failing := func(f *hookCurrentFixture, id string) {
		store := &ownershipFailingGetStore{Store: f.store}
		store.arm(id)
		f.ctx.legs = testWorkLegs("", f.ctx.cfg, store, nil)
	}
	for _, tc := range []struct {
		name  string
		stamp string
		cell  string
		work  []beads.Bead
		setup func(*hookCurrentFixture)
		want  []string
	}{
		{name: "malformed delivery cell", stamp: "gcg-42", cell: "{not json", work: []beads.Bead{hookCurrentHeld("gcg-42")}, want: []string{session.WorkStartedKey}},
		{name: "unknown delivery cell version", stamp: "gcg-42", cell: `{"v":9,"claims":[]}`, work: []beads.Bead{hookCurrentHeld("gcg-42")}, want: []string{session.WorkStartedKey, "version"}},
		{
			name: "stamp unreadable", stamp: "gcg-42", work: []beads.Bead{hookCurrentHeld("gcg-42")},
			setup: func(f *hookCurrentFixture) { failing(f, "gcg-42") },
			want:  []string{"reading claim gcg-42"},
		},
		{
			name: "newer delivery unreadable", stamp: "gcg-42", cell: hookCurrentStartedCell("gcg-new", "gcg-42"),
			work:  []beads.Bead{hookCurrentHeld("gcg-42"), hookCurrentHeld("gcg-new")},
			setup: func(f *hookCurrentFixture) { failing(f, "gcg-new") },
			want:  []string{"reading claim gcg-new"},
		},
		{
			name: "candidate unreadable after another verified", stamp: "gcg-old", cell: hookCurrentStartedCell("gcg-a", "gcg-b", "gcg-old"),
			work:  []beads.Bead{closed, hookCurrentHeld("gcg-a"), hookCurrentHeld("gcg-b")},
			setup: func(f *hookCurrentFixture) { failing(f, "gcg-b") },
			want:  []string{"reading claim gcg-b"},
		},
		{
			name: "stamp missing with a leg unread", stamp: "gcg-gone", cell: hookCurrentStartedCell("gcg-a", "gcg-gone"),
			work:  []beads.Bead{hookCurrentHeld("gcg-a")},
			setup: func(f *hookCurrentFixture) { f.ctx.gaps = []string{`rig "far" did not open`} },
			want:  []string{"reading claim gcg-gone", `rig "far" did not open`},
		},
		{
			name: "no usable work legs", stamp: "gcg-42", work: []beads.Bead{hookCurrentHeld("gcg-42")},
			setup: func(f *hookCurrentFixture) { f.ctx.legs = WorkLegs{} },
			want:  []string{"reading claim gcg-42"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookCurrentFixture(t, tc.stamp, tc.cell, tc.work...)
			if tc.setup != nil {
				tc.setup(f)
			}
			code, stdout, stderr := f.run(true)
			if code != 1 || stdout != "" {
				t.Fatalf("--id-only = (%d, %q), want exit 1 with empty stdout; stderr=%s", code, stdout, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to name %q", stderr, want)
				}
			}
		})
	}

	t.Run("found claim verifies with a leg unread", func(t *testing.T) {
		f := newHookCurrentFixture(t, "gcg-42", hookCurrentStartedCell("gcg-42"), hookCurrentHeld("gcg-42"))
		f.ctx.gaps = []string{`rig "far" is suspended`}
		if code, stdout, stderr := f.run(true); code != 0 || stdout != "gcg-42\n" || stderr != "" {
			t.Fatalf("--id-only = (%d, %q, %q), want the found claim verified", code, stdout, stderr)
		}
	})
}
