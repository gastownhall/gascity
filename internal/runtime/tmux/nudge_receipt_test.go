package tmux

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errSessionNotFoundForTest stands in for tmux refusing every command.
var errSessionNotFoundForTest = errors.New("can't find session")

// collectReceipts installs a sink on tm and returns an accessor for what it saw.
func collectReceipts(tm *Tmux) func() []runtime.NudgeReceipt {
	var mu sync.Mutex
	var got []runtime.NudgeReceipt
	tm.cfg.NudgeReceiptSink = func(r runtime.NudgeReceipt) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	return func() []runtime.NudgeReceipt {
		mu.Lock()
		defer mu.Unlock()
		out := make([]runtime.NudgeReceipt, len(got))
		copy(out, got)
		return out
	}
}

// TestNudgeSessionEmitsReceiptForTheWholePayload covers the gp-2rq receipt: a
// nudge that reaches the terminal must leave evidence of WHAT reached it, so a
// sender that must not double-deliver (the Slack adapter's same-ts twin dedup,
// gp-32q) has something better to gate on than "the call returned nil".
//
// The payload here is a multi-line reminder of the shape that was arriving
// truncated: under the retired 4096-byte fast path it would have been typed as
// raw keys.
func TestNudgeSessionEmitsReceiptForTheWholePayload(t *testing.T) {
	const message = "<system-reminder>\nNew message in shared conversation slack/C0ASAPRETDK:\n\n- Afik (human): ship it\n</system-reminder>"

	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	if err := tm.NudgeSession("gt-receipt-session", message); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	got := receipts()
	if len(got) != 1 {
		t.Fatalf("emitted %d receipts, want exactly 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Bytes != len(message) {
		t.Errorf("receipt Bytes = %d, want %d — a receipt that under-reports the payload cannot prove the whole message landed", r.Bytes, len(message))
	}
	if want := runtime.NudgePayloadDigest(message); r.Digest != want {
		t.Errorf("receipt Digest = %q, want %q — the digest is what lets a caller several layers up name the delivery it is waiting on", r.Digest, want)
	}
	if r.Framing != runtime.NudgeFramingBracketedPaste {
		t.Errorf("receipt Framing = %q, want %q", r.Framing, runtime.NudgeFramingBracketedPaste)
	}
	if r.ID == "" {
		t.Error("receipt has no ID; gp-32q logs the receipt id to correlate a delivery")
	}
	if r.At.IsZero() {
		t.Error("receipt has no timestamp")
	}
}

// TestNudgeReceiptIDsAreUniquePerDelivery: two deliveries of identical text
// share a digest (it is content-addressed on purpose) but must not share an ID,
// or a consumer cannot tell a redelivery from the original.
func TestNudgeReceiptIDsAreUniquePerDelivery(t *testing.T) {
	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	for i := 0; i < 2; i++ {
		if err := tm.NudgeSession("gt-receipt-dupe", "same text"); err != nil {
			t.Fatalf("NudgeSession #%d: %v", i, err)
		}
	}

	got := receipts()
	if len(got) != 2 {
		t.Fatalf("emitted %d receipts, want 2", len(got))
	}
	if got[0].ID == got[1].ID {
		t.Errorf("both deliveries share receipt ID %q; a redelivery would be indistinguishable from the original", got[0].ID)
	}
	if got[0].Digest != got[1].Digest {
		t.Errorf("identical payloads produced different digests (%q vs %q); the digest must be content-addressed", got[0].Digest, got[1].Digest)
	}
}

// TestNudgeSessionEmitsNoReceiptWhenDeliveryFails: a receipt is the delivery
// claim, so it must not exist for a nudge that never reached the terminal.
func TestNudgeSessionEmitsNoReceiptWhenDeliveryFails(t *testing.T) {
	fe := &fakeExecutor{err: errSessionNotFoundForTest}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	if err := tm.NudgeSession("gt-receipt-dead", "text"); err == nil {
		t.Fatal("NudgeSession() = nil, want an error for a failing tmux")
	}

	if got := receipts(); len(got) != 0 {
		t.Fatalf("emitted %d receipts for a failed delivery, want 0: %+v", len(got), got)
	}
}

// TestSendHiddenAttachedTextFramesPayloadAsOneBracketedPaste covers the second
// transport gp-2rq had to fix. The hidden-attach client writes bytes straight
// into an attached tmux client's stdin, bypassing paste-buffer, and used to
// write the payload unframed — so a multi-line reminder arrived as a run of
// Enter presses and the TUI submitted the first line alone.
//
// Measured through `script` + `tmux attach-session` against a raw-mode TUI with
// bracketed paste enabled, the markers are forwarded to the pane intact, so
// framing here is both necessary and sufficient.
func TestSendHiddenAttachedTextFramesPayloadAsOneBracketedPaste(t *testing.T) {
	const message = "line one\nline two\nline three"

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0

	const sess = "hidden-attach-framing"
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: sink}}
	tm.hiddenAttachMu.Unlock()

	used, err := tm.sendHiddenAttachedText(sess, message)
	if err != nil {
		t.Fatalf("sendHiddenAttachedText: %v", err)
	}
	if !used {
		t.Fatal("hidden-attach branch did not run")
	}

	got := sink.written()
	// The payload must sit inside exactly one bracketed paste...
	want := bracketedPasteStart + message + bracketedPasteEnd
	if !strings.Contains(got, want) {
		t.Fatalf("hidden client received %q, want the payload framed as %q", got, want)
	}
	if n := strings.Count(got, bracketedPasteStart); n != 1 {
		t.Fatalf("payload was framed as %d pastes, want exactly 1: %q", n, got)
	}
	// ...and the submit Enter must be OUTSIDE it: inside, it is pasted text
	// rather than a keypress, and the message never submits.
	if !strings.HasSuffix(got, bracketedPasteEnd+"\r") {
		t.Fatalf("hidden client received %q, want the submit Enter after the closing paste marker", got)
	}
}

// TestSendHiddenAttachedTextEmitsReceipt: the hidden-attach transport is a
// delivery path like any other and must not be a receipt blind spot.
func TestSendHiddenAttachedTextEmitsReceipt(t *testing.T) {
	const message = "hidden payload"

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	const sess = "hidden-attach-receipt"
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &recordingWriteCloser{}}}
	tm.hiddenAttachMu.Unlock()

	if _, err := tm.sendHiddenAttachedText(sess, message); err != nil {
		t.Fatalf("sendHiddenAttachedText: %v", err)
	}

	got := receipts()
	if len(got) != 1 {
		t.Fatalf("emitted %d receipts, want 1", len(got))
	}
	if got[0].Bytes != len(message) {
		t.Errorf("receipt Bytes = %d, want %d", got[0].Bytes, len(message))
	}
	if got[0].Framing != runtime.NudgeFramingBracketedPaste {
		t.Errorf("receipt Framing = %q, want %q", got[0].Framing, runtime.NudgeFramingBracketedPaste)
	}
	if got[0].Target != sess {
		t.Errorf("receipt Target = %q, want %q", got[0].Target, sess)
	}
}
