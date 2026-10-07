package tmux

import (
	"errors"
	"testing"
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
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
		// Idle for the first send's polls; busy at the pre-re-send check.
		return busyCalls > submitConfirmPollsPerSend, nil
	}
	sendEnter := func() error { enters++; return nil }

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
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

// TestSubmitEnterAndConfirmBestEffortWhenNeverBusy proves that a pane which
// never reports busy is delivered best-effort (bounded re-sends, no error) so
// the caller's contract (nil == delivered to tmux) is preserved.
func TestSubmitEnterAndConfirmBestEffortWhenNeverBusy(t *testing.T) {
	var enters int
	busy := func() (bool, error) { return false, nil }
	sendEnter := func() error { enters++; return nil }

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if confirmed {
		t.Fatal("confirmed = true, want false")
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
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

// TestSubmitEnterAndConfirmReturnsSendError proves a genuine tmux-layer send
// failure (session gone) is surfaced, matching the pre-fix contract.
func TestSubmitEnterAndConfirmReturnsSendError(t *testing.T) {
	sendErr := errors.New("no server")
	var enters int
	sendEnter := func() error { enters++; return sendErr }
	busy := func() (bool, error) { return false, nil }

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, noSleep)
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

	got := recoverStagedDraft(pane.sendSubmit, func() {}, pane.observe, pane.sleep)
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

	got := recoverStagedDraft(pane.sendSubmit, func() {}, pane.observe, pane.sleep)
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

	got := recoverStagedDraft(pane.sendSubmit, func() {}, pane.observe, pane.sleep)
	if got != stagedDraftUnresolved {
		t.Fatalf("outcome = %v, want stagedDraftUnresolved", got)
	}
	if pane.sends != 0 {
		t.Fatalf("sends = %d, want 0 (no draft, no extra submit)", pane.sends)
	}
}

// TestRecoverStagedDraftBusyAtEntrySendsNothing: the turn started right after
// the ordinary window's last poll. A busy pane must never get another submit.
func TestRecoverStagedDraftBusyAtEntrySendsNothing(t *testing.T) {
	sends := 0
	got := recoverStagedDraft(
		func() error { sends++; return nil },
		func() {},
		// Busy with the draft still on screen: busy wins, as it does in
		// submitEnterAndConfirm.
		func() (paneSubmitObservation, error) { return paneSubmitObservation{busy: true, drafted: true}, nil },
		noSleep,
	)
	if got != stagedDraftSubmittedBusy {
		t.Fatalf("outcome = %v, want stagedDraftSubmittedBusy", got)
	}
	if sends != 0 {
		t.Fatalf("sends = %d, want 0 (busy pane re-submitted)", sends)
	}
}

// TestRecoverStagedDraftIsBounded: a draft that never submits costs a bounded
// number of re-sends, then the caller's unconfirmed path takes over.
func TestRecoverStagedDraftIsBounded(t *testing.T) {
	pane := &stagedDraftPane{swallow: 1 << 30}

	got := recoverStagedDraft(pane.sendSubmit, func() {}, pane.observe, pane.sleep)
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
