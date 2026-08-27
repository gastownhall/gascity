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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, nil, noSleep)
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

// TestSubmitEnterAndConfirmRecoversAStagedDraft pins ga-wr4ft: the ordinary
// confirm window can expire entirely inside a large paste's ingest with every
// Enter swallowed. While the composer visibly holds the staged draft the
// submit has definitively not happened, so the recovery loop keeps re-sending
// on a generous pace and succeeds when the draft finally clears.
func TestSubmitEnterAndConfirmRecoversAStagedDraft(t *testing.T) {
	sends := 0
	// The first 5 Enters are swallowed by paste ingest; the 6th submits.
	sendEnter := func() error { sends++; return nil }
	busy := func() (bool, error) { return sends >= 6 && sends > 0, nil }
	drafted := func() (bool, error) { return sends < 6, nil }
	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, drafted, func(time.Duration) {})
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%t err=%v, want the recovery loop to land the submit", confirmed, err)
	}
	if sends < 6 {
		t.Fatalf("sends=%d, want the loop to persist past the swallowed Enters", sends)
	}
}

// TestSubmitEnterAndConfirmDraftClearWithoutBusyCountsAsSubmitted: a short
// turn can complete between polls — the draft clearing is itself the proof
// the submit landed.
func TestSubmitEnterAndConfirmDraftClearWithoutBusyCountsAsSubmitted(t *testing.T) {
	sends := 0
	sendEnter := func() error { sends++; return nil }
	busy := func() (bool, error) { return false, nil }
	drafted := func() (bool, error) { return sends < 4, nil }
	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, drafted, func(time.Duration) {})
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%t err=%v, want draft-clear to count as submitted", confirmed, err)
	}
}

// TestSubmitEnterAndConfirmNoDraftKeepsTheOldContract: without a visible
// draft the recovery loop must not fire — an idle pane with no draft is the
// ambiguous state where extra Enters are NOT provably safe.
func TestSubmitEnterAndConfirmNoDraftKeepsTheOldContract(t *testing.T) {
	sends := 0
	sendEnter := func() error { sends++; return nil }
	busy := func() (bool, error) { return false, nil }
	drafted := func() (bool, error) { return false, nil }
	confirmed, err := submitEnterAndConfirm(sendEnter, func() {}, busy, drafted, func(time.Duration) {})
	if err != nil || confirmed {
		t.Fatalf("confirmed=%t err=%v, want the old unconfirmed outcome", confirmed, err)
	}
	if sends != submitEnterMaxSends {
		t.Fatalf("sends=%d, want exactly the ordinary window %d", sends, submitEnterMaxSends)
	}
}
