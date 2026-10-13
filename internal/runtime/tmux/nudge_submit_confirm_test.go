package tmux

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// noSleep is a sleep stub so the confirm loop runs instantly under test.
func noSleep(time.Duration) {}

// TestSubmitEnterAndConfirmReEntersWhileIdle proves the ga-bwm fix: when the
// first Enter is lost (the pane stays idle with the message still drafted), the
// loop re-sends Enter, and the send that lands drives the agent busy.
func TestSubmitEnterAndConfirmReEntersWhileIdle(t *testing.T) {
	var enters int
	// Busy only becomes true once a second Enter has been sent, i.e. the first
	// Enter raced the paste and was dropped.
	busy := func() (bool, error) { return enters >= 2, nil }
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true (re-sent Enter should submit)")
	}
	if enters != 2 {
		t.Fatalf("enters = %d, want 2 (initial + one re-send)", enters)
	}
}

// TestSubmitEnterAndConfirmStopsWhenBusy proves the common case: a single Enter
// that submits is confirmed on the first poll with no wasted re-send.
func TestSubmitEnterAndConfirmStopsWhenBusy(t *testing.T) {
	var enters int
	busy := func() (bool, error) { return enters >= 1, nil }
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (no re-send once submitted)", enters)
	}
}

// TestSubmitEnterAndConfirmNoDoubleSubmitOnFastTurn proves the safety property:
// if a turn goes busy after the first send's polls but before a re-send, the
// pre-re-send busy check catches it and no second Enter is issued.
func TestSubmitEnterAndConfirmNoDoubleSubmitOnFastTurn(t *testing.T) {
	var enters int
	var busyCalls int
	busy := func() (bool, error) {
		busyCalls++
		// Idle for the baseline read and the first send's polls; busy at the
		// pre-re-send check.
		return busyCalls > 1+submitConfirmPollsPerSend, nil
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (pre-re-send busy check must prevent double-submit)", enters)
	}
}

// TestSubmitEnterAndConfirmPreCheckConfirmsShortTurn proves the pre-re-send
// check confirms on its own busy reading: a turn that is busy only at that
// check and over by the next read must not reach a later pre-check that sees
// idle and re-sends a message that was already submitted.
func TestSubmitEnterAndConfirmPreCheckConfirmsShortTurn(t *testing.T) {
	var enters int
	var busyCalls int
	busy := func() (bool, error) {
		busyCalls++
		// Busy only at send 1's pre-re-send check.
		return busyCalls == 1+submitConfirmPollsPerSend+1, nil
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true (the pre-re-send check saw the turn)")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (a finished turn must not be re-submitted)", enters)
	}
}

// TestSubmitEnterAndConfirmBestEffortWhenNeverBusy proves that a pane which
// never reports busy is delivered best-effort (bounded re-sends, no error) so
// the caller's contract (nil == delivered to tmux) is preserved.
func TestSubmitEnterAndConfirmBestEffortWhenNeverBusy(t *testing.T) {
	var enters int
	busy := func() (bool, error) { return false, nil }
	sendEnter := func() error { enters++; return nil }

	confirmed, sawIdle, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if confirmed {
		t.Fatal("confirmed = true, want false")
	}
	if !sawIdle {
		t.Fatal("sawIdle = false, want true (every reading was idle)")
	}
	if enters != submitEnterMaxSends {
		t.Fatalf("enters = %d, want %d (bounded best-effort sends)", enters, submitEnterMaxSends)
	}
}

// TestSubmitEnterAndConfirmClearsStaleSendError proves a transient first-send
// failure followed by a successful send (busy never observed) is reported as
// best-effort delivery (false, nil), not a stale error — matching the
// historical "nil == handed to tmux" contract.
func TestSubmitEnterAndConfirmClearsStaleSendError(t *testing.T) {
	var enters int
	sendEnter := func() error {
		enters++
		if enters == 1 {
			return errors.New("transient: no server yet")
		}
		return nil
	}
	busy := func() (bool, error) { return false, nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil (later send succeeded)", err)
	}
	if confirmed {
		t.Fatal("confirmed = true, want false (never busy)")
	}
	if enters != submitEnterMaxSends {
		t.Fatalf("enters = %d, want %d", enters, submitEnterMaxSends)
	}
}

// TestSubmitEnterAndConfirmIgnoresPreexistingBusyOnFirstSend proves the
// pre-send baseline fix: a pane that is ALREADY busy before this call is ever
// made (a stray earlier submit still running) must not have that pre-existing
// busy reading credited to send 0's own poll -- confirmation requires an
// actual false->true transition, an idle reading before the busy one. The
// scripted busy() sequence below stays busy through the baseline read and all
// of send 0's polls (the stale state, unrelated to this call's own submit), then
// genuinely goes idle at send 1's pre-send check (the stray turn finishes),
// then genuinely goes busy on send 1's own poll -- the real transition this
// call's submit produces. Before the fix, the very first poll of send 0 would
// have read the stale busy state and returned confirmed with enters == 1.
func TestSubmitEnterAndConfirmIgnoresPreexistingBusyOnFirstSend(t *testing.T) {
	var enters int
	var calls int
	busy := func() (bool, error) {
		calls++
		switch {
		case calls <= 1+submitConfirmPollsPerSend:
			// Baseline read (call 1) plus all of send 0's polls: the stale
			// busy state from before this call was ever made.
			return true, nil
		case calls == 1+submitConfirmPollsPerSend+1:
			// Send 1's pre-send idle check: the stray turn has finished.
			return false, nil
		default:
			// Send 1's own submit lands and the turn goes genuinely busy.
			return true, nil
		}
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true (send 1's genuine transition should confirm)")
	}
	if enters != 2 {
		t.Fatalf("enters = %d, want 2 (send 0's stale-busy reading must not short-circuit before send 1 re-sends)", enters)
	}
}

// TestSubmitEnterAndConfirmNeverCreditsPreexistingBusy proves a pane that is
// busy before the call and never reads idle (an earlier turn that outlasts the
// whole window) is left unconfirmed: no busy reading is credited without an
// idle reading before it. The running turn also never receives a re-send;
// for codex the Escape-led submit sequence would interrupt it. sawIdle stays
// false, so staged-draft recovery does not credit that busy either.
func TestSubmitEnterAndConfirmNeverCreditsPreexistingBusy(t *testing.T) {
	var enters int
	busy := func() (bool, error) { return true, nil }
	sendEnter := func() error { enters++; return nil }

	confirmed, sawIdle, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil (the first submit reached tmux)", err)
	}
	if confirmed {
		t.Fatal("confirmed = true, want false (busy was never preceded by an idle reading)")
	}
	if sawIdle {
		t.Fatal("sawIdle = true, want false (the pane never read idle)")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (no re-send into a turn that is still running)", enters)
	}
}

// TestSubmitEnterAndConfirmKeepsWatchingAfterSkippedReSend proves that skipping
// a re-send into a pre-existing busy turn does not end the window: when that
// turn finishes and the pane goes busy again within the budget, the idle->busy
// transition confirms the one submit already sent.
func TestSubmitEnterAndConfirmKeepsWatchingAfterSkippedReSend(t *testing.T) {
	var enters int
	var calls int
	busy := func() (bool, error) {
		calls++
		switch {
		case calls <= 1+submitConfirmPollsPerSend+1:
			// Baseline, send 0's polls, and send 1's pre-send check: the
			// earlier turn is still running, so send 1 must not re-send.
			return true, nil
		case calls == 1+submitConfirmPollsPerSend+2:
			// First poll after the skipped re-send: the earlier turn ends.
			return false, nil
		default:
			// The submitted message starts its turn.
			return true, nil
		}
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true (idle then busy within the window)")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (send 1 saw busy without an idle reading and must skip)", enters)
	}
}

// TestSubmitEnterAndConfirmFailedReadIsNotIdle proves a failed read never
// stands in for the idle reading a confirmation needs: when the baseline read
// fails and every later read is busy, nothing is confirmed.
func TestSubmitEnterAndConfirmFailedReadIsNotIdle(t *testing.T) {
	var enters int
	var calls int
	busy := func() (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("capture-pane failed")
		}
		return true, nil
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, sawIdle, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if confirmed {
		t.Fatal("confirmed = true, want false (a failed baseline read is not an idle reading)")
	}
	if sawIdle {
		t.Fatal("sawIdle = true, want false (a failed read is not an idle reading)")
	}
	if enters != 1 {
		t.Fatalf("enters = %d, want 1 (no re-send while the pane reads busy)", enters)
	}
}

// TestSubmitEnterAndConfirmReturnsSendError proves a genuine tmux-layer send
// failure (session gone) is surfaced, matching the pre-fix contract.
func TestSubmitEnterAndConfirmReturnsSendError(t *testing.T) {
	sendErr := errors.New("no server")
	var enters int
	sendEnter := func() error { enters++; return sendErr }
	busy := func() (bool, error) { return false, nil }

	confirmed, _, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if confirmed {
		t.Fatal("confirmed = true, want false")
	}
	if !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want sendErr chain", err)
	}
	if enters != submitEnterMaxSends {
		t.Fatalf("enters = %d, want %d", enters, submitEnterMaxSends)
	}
}

// stagedDraftPane models a codex composer holding a large pasted draft while
// the TUI is still ingesting it: the first swallow submits are eaten, the next
// one submits, and the turn then renders busy (or, with busyNever, finishes
// before any poll sees it). observe answers from one consistent snapshot, the
// way the production probe reads one capture.
type stagedDraftPane struct {
	swallow   int // submits eaten by the paste ingest before one lands
	busyNever bool
	noDraft   bool // the composer never showed a staged draft
	sends     int
	observes  int
	sleeps    []time.Duration
}

func (p *stagedDraftPane) submitted() bool { return p.sends > p.swallow }

func (p *stagedDraftPane) sendSubmit() error { p.sends++; return nil }

func (p *stagedDraftPane) observe() (paneSubmitObservation, error) {
	p.observes++
	if p.noDraft {
		return paneSubmitObservation{}, nil
	}
	if p.submitted() {
		return paneSubmitObservation{busy: !p.busyNever}, nil
	}
	return paneSubmitObservation{drafted: true}, nil
}

func (p *stagedDraftPane) sleep(d time.Duration) { p.sleeps = append(p.sleeps, d) }

// TestRecoverStagedDraftResubmitsUntilBusy is the fix for the swallowed-submit
// stall: the ordinary confirm window expired inside a large paste's ingest, so
// every Enter it sent was eaten and the draft sat staged in the composer
// forever. While the draft is visible the submit has definitively not happened,
// so recovery keeps re-sending on a slower pace, and stops the moment the pane
// goes busy: exactly one submit past the swallowed ones, never a second.
func TestRecoverStagedDraftResubmitsUntilBusy(t *testing.T) {
	pane := &stagedDraftPane{swallow: 4}

	got := recoverStagedDraft(true, pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftSubmittedBusy {
		t.Fatalf("outcome = %v, want stagedDraftSubmittedBusy", got)
	}
	if pane.sends != pane.swallow+1 {
		t.Fatalf("sends = %d, want %d (every swallowed submit re-sent, nothing after the one that landed)", pane.sends, pane.swallow+1)
	}
	for _, d := range pane.sleeps {
		if d != submitDraftRecoveryBackoff {
			t.Fatalf("sleep = %v, want the recovery pace %v", d, submitDraftRecoveryBackoff)
		}
	}
}

// TestRecoverStagedDraftClearedWithoutBusyIsDeliveredUnobserved: the draft
// cleared after a re-send but no poll saw the turn busy. The Enter reached the
// pane and the agent took the draft, so this is delivered-but-unobserved (the
// caller must not re-paste it), and recovery sends nothing more.
func TestRecoverStagedDraftClearedWithoutBusyIsDeliveredUnobserved(t *testing.T) {
	pane := &stagedDraftPane{swallow: 2, busyNever: true}

	got := recoverStagedDraft(true, pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftCleared {
		t.Fatalf("outcome = %v, want stagedDraftCleared", got)
	}
	if pane.sends != pane.swallow+1 {
		t.Fatalf("sends = %d, want %d (no submit after the draft cleared)", pane.sends, pane.swallow+1)
	}
}

// TestRecoverStagedDraftWithoutDraftSendsNothing keeps the old contract: an
// idle pane with no visible draft is the ambiguous state (the submit may have
// landed and the turn already finished), where another Enter is not provably
// safe. Recovery must not send and must leave the verdict to the caller.
func TestRecoverStagedDraftWithoutDraftSendsNothing(t *testing.T) {
	pane := &stagedDraftPane{noDraft: true}

	got := recoverStagedDraft(true, pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftUnresolved {
		t.Fatalf("outcome = %v, want stagedDraftUnresolved", got)
	}
	if pane.sends != 0 {
		t.Fatalf("sends = %d, want 0 (no draft, no extra submit)", pane.sends)
	}
}

// TestRecoverStagedDraftBusyAtEntrySendsNothing: a busy pane must never get
// another submit. When the confirm window saw the pane idle, the turn started
// right after its last poll and is this submit's. When it never did, the turn
// has been running since before the nudge, so its busy proves nothing and the
// caller's drained-composer check decides.
func TestRecoverStagedDraftBusyAtEntrySendsNothing(t *testing.T) {
	tests := []struct {
		name    string
		sawIdle bool
		want    stagedDraftOutcome
	}{
		{"turn started after the window saw idle", true, stagedDraftSubmittedBusy},
		{"turn running since before the nudge", false, stagedDraftUnresolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sends := 0
			got := recoverStagedDraft(
				tt.sawIdle,
				func() error { sends++; return nil },
				func() {},
				// Busy with the draft still on screen: busy wins, so recovery
				// never submits into a running turn.
				func() (paneSubmitObservation, error) { return paneSubmitObservation{busy: true, drafted: true}, nil },
				noSleep,
			)
			if got != tt.want {
				t.Fatalf("outcome = %v, want %v", got, tt.want)
			}
			if sends != 0 {
				t.Fatalf("sends = %d, want 0 (busy pane re-submitted)", sends)
			}
		})
	}
}

// TestRecoverStagedDraftCountsItsOwnIdleReading: a confirm window that never
// saw idle does not stop recovery from crediting a submit it watches land.
// The earlier turn ended before recovery's first capture, which shows the
// draft staged on an idle pane, so the busy that follows a re-send is this
// submit's.
func TestRecoverStagedDraftCountsItsOwnIdleReading(t *testing.T) {
	pane := &stagedDraftPane{swallow: 1}

	got := recoverStagedDraft(false, pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftSubmittedBusy {
		t.Fatalf("outcome = %v, want stagedDraftSubmittedBusy", got)
	}
	if pane.sends != pane.swallow+1 {
		t.Fatalf("sends = %d, want %d", pane.sends, pane.swallow+1)
	}
}

// TestRecoverStagedDraftIsBounded: a draft that never submits costs a bounded
// number of re-sends, then the caller's unconfirmed path takes over.
func TestRecoverStagedDraftIsBounded(t *testing.T) {
	pane := &stagedDraftPane{swallow: 1 << 30}

	got := recoverStagedDraft(true, pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftUnresolved {
		t.Fatalf("outcome = %v, want stagedDraftUnresolved", got)
	}
	if pane.sends != submitDraftRecoverySends {
		t.Fatalf("sends = %d, want exactly %d", pane.sends, submitDraftRecoverySends)
	}
}

// TestRecoverStagedDraftStopsOnProbeError: without a readable pane the draft
// cannot be proven staged, so no submit may be sent.
func TestRecoverStagedDraftStopsOnProbeError(t *testing.T) {
	sends := 0
	got := recoverStagedDraft(
		true,
		func() error { sends++; return nil },
		func() {},
		func() (paneSubmitObservation, error) { return paneSubmitObservation{}, errors.New("capture failed") },
		noSleep,
	)
	if got != stagedDraftUnresolved {
		t.Fatalf("outcome = %v, want stagedDraftUnresolved", got)
	}
	if sends != 0 {
		t.Fatalf("sends = %d, want 0", sends)
	}
}

// TestRecoverStagedDraftStopsOnSendError: a tmux-layer send failure ends
// recovery at once instead of hammering a broken server.
func TestRecoverStagedDraftStopsOnSendError(t *testing.T) {
	sends := 0
	got := recoverStagedDraft(
		true,
		func() error { sends++; return errors.New("no server") },
		func() {},
		func() (paneSubmitObservation, error) { return paneSubmitObservation{drafted: true}, nil },
		noSleep,
	)
	if got != stagedDraftUnresolved {
		t.Fatalf("outcome = %v, want stagedDraftUnresolved", got)
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// TestPaneShowsStagedDraftReadsOnlyTheLiveComposer pins where the marker is
// looked for. Only the live composer (the last prompt line and what follows
// it) counts: the same text in the transcript above, for example an agent
// printing it or an earlier message, must not trigger extra submits.
func TestPaneShowsStagedDraftReadsOnlyTheLiveComposer(t *testing.T) {
	codex := stagedDraftMarkers["codex"]
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name:  "staged paste in the composer",
			lines: []string{"• PONG", "", "› [Pasted Content 11234 chars]", "", "  gpt-5.5 low · /tmp/probe"},
			want:  true,
		},
		{
			name:  "second paste wrapped onto a continuation line",
			lines: []string{"› [Pasted Content 4096 chars]", "  [Pasted Content 1200 chars]", "  gpt-5.5 low · /tmp/probe"},
			want:  true,
		},
		{
			name:  "composer inside a box border",
			lines: []string{"│ › [Pasted Content 11234 chars] │"},
			want:  true,
		},
		{
			name:  "marker only in the transcript above an empty composer",
			lines: []string{"› [Pasted Content 11234 chars]", "• grep found \"[Pasted Content\" in tmux.go", "", "› Explain this codebase", "  gpt-5.5 low · /tmp/probe"},
			want:  false,
		},
		{
			name:  "idle empty composer",
			lines: []string{"• PONG", "› Explain this codebase", "  gpt-5.5 low · /tmp/probe"},
			want:  false,
		},
		{
			name:  "no composer on screen",
			lines: []string{"[Pasted Content 11234 chars]"},
			want:  false,
		},
		{
			name:  "empty capture",
			lines: nil,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paneShowsStagedDraft(tt.lines, codex); got != tt.want {
				t.Fatalf("paneShowsStagedDraft(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestPaneShowsStagedDraftReadsClaudesLiveComposer pins Claude's marker: its
// prompt is "❯" plus NBSP, and a submitted paste stays echoed in the transcript
// above the composer, which must not count as staged.
func TestPaneShowsStagedDraftReadsClaudesLiveComposer(t *testing.T) {
	claude := stagedDraftMarkers["claude"]
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name:  "staged paste in the composer",
			lines: []string{"● Done.", "────", "❯\u00a0[Pasted text #2 +37 lines]", "────", "  ⏵⏵ bypass permissions on"},
			want:  true,
		},
		{
			name:  "two stacked staged pastes",
			lines: []string{"❯\u00a0[Pasted text #1 +42 lines][Pasted text #2 +37 lines]", "────"},
			want:  true,
		},
		{
			name:  "earlier paste echoed above an empty composer",
			lines: []string{"❯\u00a0[Pasted text #1 +44 lines]", "● Done.", "────", "❯\u00a0", "────", "  ⏵⏵ bypass permissions on"},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paneShowsStagedDraft(tt.lines, claude); got != tt.want {
				t.Fatalf("paneShowsStagedDraft(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestStagedDraftRecoveryIsClaudeAndCodexOnly: recovery runs only for a family
// whose TUI renders a recognizable staged-draft marker and whose submit is
// already verified. Every other provider keeps the old submit contract exactly.
func TestStagedDraftRecoveryIsClaudeAndCodexOnly(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		if _, ok := stagedDraftMarkerForFamily(family); !ok {
			t.Fatalf("%s has no staged-draft marker; a swallowed submit would leave its pasted prompt staged forever", family)
		}
	}
	for _, family := range []string{"gemini", "kimi", "opencode", "grok", "", "some-unregistered-family"} {
		if _, ok := stagedDraftMarkerForFamily(family); ok {
			t.Errorf("stagedDraftMarkerForFamily(%q) = ok, want no marker (old submit contract)", family)
		}
	}
	for family := range stagedDraftMarkers {
		if !submitVerifyEligibleFamily(family) {
			t.Errorf("family %q has a staged-draft marker but is not submit-verify eligible; recovery only runs on the verified path", family)
		}
	}
}

// agentPaneExecutor answers as a pane running provider (its GC_PROVIDER) whose
// capture is pane however many submits arrive. When after is set, the capture
// switches to it once the confirm window has run out: from the first capture
// past the polls that follow the last submit Enter. attached is the attachment
// probe's client count; empty makes the probe fail.
type agentPaneExecutor struct {
	session  string
	provider string
	attached string
	pane     []string
	after    []string
	calls    [][]string
}

func (e *agentPaneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch {
	case slices.Contains(args, "#{session_name}|#{session_attached}"):
		if e.attached == "" {
			return "", errors.New("server busy")
		}
		return e.session + "|" + e.attached, nil
	case slices.Contains(args, "show-environment") && slices.Contains(args, "GC_PROVIDER"):
		return "GC_PROVIDER=" + e.provider, nil
	case slices.Contains(args, "capture-pane"):
		lines := e.pane
		if e.after != nil && e.submitEnters() == submitEnterMaxSends && e.capturesSinceLastEnter() > submitConfirmPollsPerSend {
			lines = e.after
		}
		out := strings.Join(lines, "\n")
		if !slices.Contains(args, "-e") {
			out = sgrSequence.ReplaceAllString(out, "")
		}
		return out, nil
	}
	return "", nil
}

// sgrSequence matches an SGR escape sequence, which capture-pane includes in
// its output only with -e.
var sgrSequence = regexp.MustCompile("\x1b\\[[0-9;]*m")

func (e *agentPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// submitEnters counts the submit Enters the pane received.
func (e *agentPaneExecutor) submitEnters() int {
	enters := 0
	for _, c := range e.calls {
		if isSubmitEnter(c) {
			enters++
		}
	}
	return enters
}

// capturesSinceLastEnter counts the captures taken after the last submit
// Enter, including one in progress.
func (e *agentPaneExecutor) capturesSinceLastEnter() int {
	captures := 0
	for _, c := range slices.Backward(e.calls) {
		if isSubmitEnter(c) {
			break
		}
		if slices.Contains(c, "capture-pane") {
			captures++
		}
	}
	return captures
}

// sentKeys lists, in order, the named keys the pane received: every send-keys
// call except literal text (-l) and copy-mode commands (-X).
func (e *agentPaneExecutor) sentKeys() []string {
	var keys []string
	for _, c := range e.calls {
		if slices.Contains(c, "send-keys") && !slices.Contains(c, "-l") && !slices.Contains(c, "-X") {
			keys = append(keys, c[len(c)-1])
		}
	}
	return keys
}

// isSubmitEnter reports whether args are a send-keys call that sends Enter.
func isSubmitEnter(args []string) bool {
	return slices.Contains(args, "send-keys") && args[len(args)-1] == "Enter"
}

// TestNudgeSessionClaudeStagedDraftStaysUnconfirmed: when recovery cannot
// resolve a staged Claude paste (its bound runs out, or it is skipped because a
// client is attached or the probe fails), the composer still shows the
// placeholder. That composer has not drained, so the nudge must stay
// ErrNudgeSubmitUnconfirmed and requeue. ErrNudgeSubmitDeliveredUnobserved
// would ack it and drop the message.
func TestNudgeSessionClaudeStagedDraftStaysUnconfirmed(t *testing.T) {
	tests := []struct {
		name     string
		session  string
		attached string
		enters   int
	}{
		{"detached pane exhausts recovery", "staged-claude-detached", "0", submitEnterMaxSends + submitDraftRecoverySends},
		{"attached pane skips recovery", "staged-claude-attached", "1", submitEnterMaxSends},
		{"failing attachment probe skips recovery", "staged-claude-probe-error", "", submitEnterMaxSends},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The bubble creates this session's nudge lock channel; drop it so
			// nothing outside the bubble ever touches it.
			t.Cleanup(func() { sessionNudgeLocks.Delete(tt.session) })
			synctest.Test(t, func(t *testing.T) {
				ex := &agentPaneExecutor{
					session:  tt.session,
					provider: "claude",
					attached: tt.attached,
					pane:     []string{"● Done.", "────", "❯\u00a0[Pasted text #1 +42 lines]", "────", "  ⏵⏵ bypass permissions on"},
				}
				tm := &Tmux{cfg: DefaultConfig(), exec: ex}

				err := tm.NudgeSession(tt.session, "reminder: please respond to the review")
				if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
					t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitUnconfirmed", err)
				}
				if enters := ex.submitEnters(); enters != tt.enters {
					t.Fatalf("submit Enter sends = %d, want %d", enters, tt.enters)
				}
			})
		})
	}
}

// TestNudgeSessionClaudeBusyThroughoutIsNotCredited: the pane was already
// running a turn when the nudge arrived and stayed busy through the confirm
// window. That busy belongs to the earlier turn, so it confirms nothing, no
// submit is re-sent into the running turn, and staged-draft recovery does not
// credit it either. The composer decides: while it still holds the nudge
// (typed or as a staged paste) the submit is unconfirmed and the nudge
// requeues; once Claude has queued the nudge (a plain-space "❯ " row above a
// composer showing its dim queued-message hint) the Enter reached the pane.
func TestNudgeSessionClaudeBusyThroughoutIsNotCredited(t *testing.T) {
	const message = "reminder: please respond to the review"
	turn := []string{"* Incubating… (48s · ↓ 707 tokens)", "  ⎿ \u00a0Tip: Use /btw to ask a quick side question", ""}
	footer := []string{"────", "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"}
	tests := []struct {
		name     string
		session  string
		attached string
		composer []string
		want     error
	}{
		{"detached, nudge typed in the composer", "busy-claude-typed-detached", "0", []string{"────", "❯\u00a0" + message}, ErrNudgeSubmitUnconfirmed},
		{"attached, nudge typed in the composer", "busy-claude-typed-attached", "1", []string{"────", "❯\u00a0" + message}, ErrNudgeSubmitUnconfirmed},
		{"detached, nudge staged as a paste", "busy-claude-staged-detached", "0", []string{"────", "❯\u00a0[Pasted text #1 +42 lines]"}, ErrNudgeSubmitUnconfirmed},
		{"detached, nudge queued by Claude", "busy-claude-queued-detached", "0", []string{"❯ " + message, "────", "❯\u00a0\x1b[2mPress up to edit queued messages\x1b[0m"}, ErrNudgeSubmitDeliveredUnobserved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The bubble creates this session's nudge lock channel; drop it so
			// nothing outside the bubble ever touches it.
			t.Cleanup(func() { sessionNudgeLocks.Delete(tt.session) })
			synctest.Test(t, func(t *testing.T) {
				ex := &agentPaneExecutor{
					session:  tt.session,
					provider: "claude",
					attached: tt.attached,
					pane:     slices.Concat(turn, tt.composer, footer),
				}
				tm := &Tmux{cfg: DefaultConfig(), exec: ex}

				err := tm.NudgeSession(tt.session, message)
				if !errors.Is(err, tt.want) {
					t.Fatalf("NudgeSession = %v, want %v", err, tt.want)
				}
				if enters := ex.submitEnters(); enters != 1 {
					t.Fatalf("submit Enter sends = %d, want 1 (no submit into the running turn)", enters)
				}
			})
		})
	}
}

// TestNudgeSessionCodexBusyThroughoutIsNotCredited is the codex twin of the
// Claude test above. codex's composer starts with "› ", so the drained-composer
// check must read that prompt. Through Claude's "❯ " a codex composer is never
// observed, and every nudge into a busy codex pane would requeue although codex
// consumed it, each re-paste led by an Escape that cancels the turn the last
// copy started. Every row sends exactly one Escape+Enter submit.
func TestNudgeSessionCodexBusyThroughoutIsNotCredited(t *testing.T) {
	const message = "reminder: please respond to the review"
	turn := []string{"• Ran make test", "  └ ok  ./...", "", "◦ Working (48s • esc to interrupt)", ""}
	footer := []string{"", "  gpt-5.5 low · /tmp/probe"}
	tests := []struct {
		name     string
		session  string
		attached string
		composer []string
		want     error
		keys     []string
	}{
		{"detached, composer drained", "busy-codex-drained-detached", "0", []string{"› "}, ErrNudgeSubmitDeliveredUnobserved, []string{"C-u", "Escape", "Enter"}},
		{"attached, composer drained", "busy-codex-drained-attached", "1", []string{"› "}, ErrNudgeSubmitDeliveredUnobserved, []string{"Escape", "Enter"}},
		{"detached, nudge typed in the composer", "busy-codex-typed-detached", "0", []string{"› " + message}, ErrNudgeSubmitUnconfirmed, []string{"C-u", "Escape", "Enter"}},
		{"detached, nudge staged as a paste", "busy-codex-staged-detached", "0", []string{"› [Pasted Content 1234 chars]"}, ErrNudgeSubmitUnconfirmed, []string{"C-u", "Escape", "Enter"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The bubble creates this session's nudge lock channel; drop it so
			// nothing outside the bubble ever touches it.
			t.Cleanup(func() { sessionNudgeLocks.Delete(tt.session) })
			synctest.Test(t, func(t *testing.T) {
				ex := &agentPaneExecutor{
					session:  tt.session,
					provider: "codex",
					attached: tt.attached,
					pane:     slices.Concat(turn, tt.composer, footer),
				}
				tm := &Tmux{cfg: DefaultConfig(), exec: ex}

				err := tm.NudgeSession(tt.session, message)
				if !errors.Is(err, tt.want) {
					t.Fatalf("NudgeSession = %v, want %v", err, tt.want)
				}
				if keys := ex.sentKeys(); !slices.Equal(keys, tt.keys) {
					t.Fatalf("sent keys = %q, want %q (one Escape+Enter submit, none into the running turn)", keys, tt.keys)
				}
			})
		})
	}
}

// TestPaneShowsDrainedComposerReadsTheFamilyPrompt pins that a codex composer,
// which starts with "› " rather than Claude's "❯ ", is found by its own
// family's prompt: drained while the turn runs, back to its dim hint (which
// captureInputLines strips), still holding the draft, or still staged.
func TestPaneShowsDrainedComposerReadsTheFamilyPrompt(t *testing.T) {
	const sent = "reminder: please respond to the review"
	busy := []string{"◦ Working (48s • esc to interrupt)", ""}
	footer := []string{"", "  gpt-5.5 low · /tmp/probe"}
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"drained codex composer", slices.Concat(busy, []string{"› "}, footer), true},
		{"codex composer back to its dim hint", []string{"• PONG", stripDimText("› \x1b[2mExplain this codebase\x1b[22m"), "  gpt-5.5 low · /tmp/probe"}, true},
		{"codex composer still holds the draft", slices.Concat(busy, []string{"› " + sent}, footer), false},
		{"codex composer still staged", slices.Concat(busy, []string{"› [Pasted Content 1234 chars]"}, footer), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paneShowsDrainedComposer(tt.lines); got != tt.want {
				t.Fatalf("paneShowsDrainedComposer(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}

// TestNudgeSessionClaudeTurnAfterTheWindowIsCredited: every poll of the
// confirm window read the pane idle with the nudge staged as a paste, and the
// turn started just after the last one. Recovery's first capture finds it
// running with the composer drained. The window saw the pane idle, so that
// busy is this submit's: the nudge is delivered, and nothing more is sent into
// the turn.
func TestNudgeSessionClaudeTurnAfterTheWindowIsCredited(t *testing.T) {
	const session = "claude-turn-after-window"
	// The bubble creates this session's nudge lock channel; drop it so nothing
	// outside the bubble ever touches it.
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		ex := &agentPaneExecutor{
			session:  session,
			provider: "claude",
			attached: "0",
			pane:     []string{"● Done.", "────", "❯\u00a0[Pasted text #1 +42 lines]", "────", "  ⏵⏵ bypass permissions on"},
			after:    []string{"● Done.", "❯ [Pasted text #1 +42 lines]", "", "* Incubating…", "", "────", "❯\u00a0", "────", "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"},
		}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		if err := tm.NudgeSession(session, "reminder: please respond to the review"); err != nil {
			t.Fatalf("NudgeSession = %v, want nil (the window saw the pane idle before the turn began)", err)
		}
		if enters := ex.submitEnters(); enters != submitEnterMaxSends {
			t.Fatalf("submit Enter sends = %d, want %d (no recovery submit into the running turn)", enters, submitEnterMaxSends)
		}
	})
}

// TestPaneContainsBusyIndicatorBareSpinnerRow pins the busy reading the confirm
// loop depends on for Claude Code's spinner row before its timer suffix: every
// shipped frame glyph and verb spelling reads busy, while transcript prose with
// a glyph or a word before "…" reads idle. A false busy holds WaitForIdle until
// its timeout and lets submitEnterAndConfirm confirm a submit that never landed.
func TestPaneContainsBusyIndicatorBareSpinnerRow(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"· Tinkering…", true},
		{"✢ Clauding…", true},
		{"· Quantumizing…", true},
		{"* Thinking…", true}, // outside ghostty, Linux Claude Code spins "*" in place of "✳"
		{"✳ Working…", true},
		{"✶ Dilly-dallying…", true},
		{"✻ Flambéing…", true},
		{"✽ Beboppin'…", true},
		{"- Loading…", false},
		{"> Something…", false},
		{"> Investigating…", false},
		{"> String…", false},
		{"❯ Testing…", false}, // an earlier prompt echoed in the transcript
		{"Status: Pending…", false},
		{"1. Fixing…", false},
		{"• Investigating…", false}, // codex transcript bullet
		{"• Building…", false},
		{"quoted: · Tinkering…", false},
		{"✻ Worked for 1m 49s", false},
	}
	for _, tt := range tests {
		if got := paneContainsBusyIndicator([]string{tt.line}); got != tt.want {
			t.Errorf("paneContainsBusyIndicator(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestPaneContainsBusyIndicatorHourTimer pins the busy reading of Claude Code's
// spinner once a turn passes one hour and the timer gains an hour group. The
// timer alone must read busy, not only the leading glyph and verb: a frame
// whose glyph is outside the shipped set still carries the timer. Hour-form
// idle chrome must keep reading idle, since a false busy holds WaitForIdle.
func TestPaneContainsBusyIndicatorHourTimer(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		// Live pane lines from long turns.
		{"✽ Canoodling… (5h 52m 12s · ↓ 39.5k tokens)", true},
		{"· Flibbertigibbeting… (2h 12m 40s · thinking with max effort)", true},
		{"* Orbiting… (6h 16m 34s · ↓ 443 tokens · thought for 1s)", true},
		{"* Orbiting… (6h 16m 27s)", true}, // bare frame, no separator yet
		// The timer is what makes these busy: no known glyph leads the line.
		{"⚡ Puttering… (1h 21m 34s · ↓ 45.3k tokens)", true},
		{"  (1h 2m 3s · ↓ 18.5k tokens)", true},
		{"• Waiting for background terminal (1h 05m 07s • esc to interrupt)", true},
		{"✻ Worked for 1h 2m 3s", false}, // done marker, hour form
		{"  edited (2h ago)", false},
		{"  Retrying in (5s)", false},
		{"🧠 Sonnet 4.6 | 📁 witness | ⏱️  Jun 3 20:10:09", false},
		{"  … +9 lines (ctrl+o to expand)", false},
	}
	for _, tt := range tests {
		if got := paneContainsBusyIndicator([]string{tt.line}); got != tt.want {
			t.Errorf("paneContainsBusyIndicator(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestSnapshotPaneIdleWithPrefixHourSpinner pins the pane-level verdict for a
// Claude Code seat more than an hour into a turn: the "❯" prompt stays visible
// beside the spinner, so the scan must answer busy from the spinner alone and
// idle only once the done marker replaces it.
func TestSnapshotPaneIdleWithPrefixHourSpinner(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"hour spinner above the prompt", "✽ Puttering… (1h 21m 34s · ↓ 45.3k tokens · thinking with max effort)\n\n❯ \n", false},
		{"hour spinner without a known glyph", "⚡ Puttering… (1h 21m 34s · ↓ 45.3k tokens)\n\n❯ \n", false},
		{"hour done marker above the prompt", "✻ Worked for 1h 2m 3s\n\n❯ \n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &fakeExecutor{agentPane: true, out: tt.pane}
			got, err := tm.snapshotPaneIdleWithPrefix("gc-test", "❯")
			if err != nil {
				t.Fatalf("snapshotPaneIdleWithPrefix error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("snapshotPaneIdleWithPrefix idle = %v, want %v", got, tt.want)
			}
		})
	}
}
