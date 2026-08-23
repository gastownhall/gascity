package tmux

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingWriteCloser captures the keystrokes gc injects into a hidden attach
// client so a test can confirm the hidden-injection branch actually ran.
type recordingWriteCloser struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriteCloser) Close() error { return nil }

func (w *recordingWriteCloser) written() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestNudgeNowHiddenAttachedRecordsPoke covers the codex-flagged residual of the
// #4187 nudge-path poke fix: NudgeNow's hidden-attached-client branch
// (sendHiddenAttachedText) injects gc's own keystrokes just like NudgeSession,
// so it must record a poke. Before the fix it returned without one, so
// GetSessionActivity counted gc's own injected input — e.g. the detached-gemini
// "/rewind" + Enter that ResetInterruptedTurn sends through a hidden client — as
// the agent responding, masking an unresponsive session.
//
// This drives Provider.NudgeNow with an injected hidden client and a fake
// executor, then verifies the recorded poke discounts a post-grace echo back to
// the genuine pre-nudge activity. It uses synthetic times (no real tmux, no
// sleeps) like the other poke unit tests, so it stays in the default lane.
func TestNudgeNowHiddenAttachedRecordsPoke(t *testing.T) {
	genuine := time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC) // last real agent turn

	// rawSessionActivity reads list-windows #{window_activity}; return the
	// genuine turn's unix seconds so pokePrior snapshots it as the poke's prior.
	fe := &fakeExecutor{out: strconv.FormatInt(genuine.Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0 // no wall-clock debounce in a unit test

	const sess = "hidden-attach-nudge"
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{
		sess: {stdin: sink},
	}
	tm.hiddenAttachMu.Unlock()

	p := &Provider{tm: tm}
	if err := p.NudgeNow(sess, runtime.TextContent("/rewind")); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}

	// The hidden-injection branch must have run (not the NudgeSession fallback).
	if got := sink.written(); !strings.Contains(got, "/rewind") || !strings.Contains(got, "\r") {
		t.Fatalf("hidden client received %q, want the /rewind text and a trailing Enter", got)
	}

	tm.pokeMu.Lock()
	pk, ok := tm.pokes[sess]
	tm.pokeMu.Unlock()
	if !ok {
		t.Fatal("NudgeNow via a hidden attached client recorded no poke; gc's own keystrokes will inflate last_active")
	}
	if !pk.prior.Equal(genuine) {
		t.Fatalf("poke prior = %v, want the genuine pre-nudge activity %v", pk.prior, genuine)
	}
	if pk.at.IsZero() {
		t.Fatal("poke was stamped with a zero time; want it stamped after delivery")
	}

	// Behavioral consequence the review requires: once the grace elapses with
	// only gc's own keystroke echo as window activity, the discount must reveal
	// the genuine pre-nudge activity, not gc's echo. Drive the pure discount with
	// the recorded poke and a synthetic now so the assertion stays deterministic.
	echo := pk.at // window_activity is only the nudge's own keystroke echo
	if got := discountPokeActivity(echo, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(genuine) {
		t.Errorf("post-grace unanswered hidden nudge resolved to %v, want the genuine prior %v", got, genuine)
	}
}

// TestSendKeysHiddenAttachedRecordsPoke covers the post-merge review finding on
// PR #4497: closing the residual NudgeNow gap poked sendHiddenAttachedText, but
// the sibling hidden-attached key path (sendHiddenAttachedKeys, driven by
// SendKeys) still injected gc's own keystrokes without recording a poke.
// ResetInterruptedTurn sends "/rewind" through the now-poked text path, then
// drives the detached Gemini rewind picker with SendKeys ("Up"/"Enter"/"Down"/
// "Enter"). That picker navigation (waitForPane + sleeps) runs longer than
// pokeEcho after the "/rewind" poke, so the stale text poke can no longer
// bracket the final confirming Enter: GetSessionActivity counted gc's own picker
// keystroke as the agent responding, masking a woken-but-stuck rewind. The
// hidden-attached key path must record its own poke (capture prior before the
// first write, stamp after the last key) so the final keystroke's echo is
// discounted.
//
// This drives Provider.SendKeys with an injected hidden client and a fake
// executor, seeds a stale "/rewind" poke to model a picker cadence that exceeds
// pokeEcho, then verifies the confirming keystroke re-stamps the poke and a
// post-grace echo discounts back to the genuine pre-rewind activity. It uses
// synthetic times (no real tmux, no sleeps) like the other poke unit tests, so
// it stays in the default lane.
func TestSendKeysHiddenAttachedRecordsPoke(t *testing.T) {
	genuine := time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC) // last real agent turn

	// rawSessionActivity reads list-windows #{window_activity}; return the
	// genuine turn's unix seconds so pokePrior snapshots it as the poke's prior.
	fe := &fakeExecutor{out: strconv.FormatInt(genuine.Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe

	const sess = "hidden-attach-picker"
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{
		sess: {stdin: sink},
	}
	tm.hiddenAttachMu.Unlock()

	// Model the picker cadence the review requires coverage for: the "/rewind"
	// poke was stamped more than pokeEcho ago (the dialog waits and sleeps in
	// ResetInterruptedTurn), so its echo window no longer covers the confirming
	// keystroke sent below.
	stale := time.Now().Add(-2 * pokeEcho)
	tm.recordPokeAt(sess, genuine, stale)

	p := &Provider{tm: tm}
	// The final confirming picker keystroke of the detached rewind flow.
	if err := p.SendKeys(sess, "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// The hidden-injection branch must have run (not the SendKeysRaw fallback).
	if got := sink.written(); !strings.Contains(got, "\r") {
		t.Fatalf("hidden client received %q, want the carriage-return Enter keystroke", got)
	}

	tm.pokeMu.Lock()
	pk, ok := tm.pokes[sess]
	tm.pokeMu.Unlock()
	if !ok {
		t.Fatal("SendKeys via a hidden attached client recorded no poke; gc's own picker keystroke will inflate last_active")
	}
	if !pk.prior.Equal(genuine) {
		t.Fatalf("poke prior = %v, want the genuine pre-rewind activity %v", pk.prior, genuine)
	}
	// The poke must be re-stamped to bracket the confirming keystroke, not left at
	// the stale "/rewind" time — otherwise the final Enter's echo lands outside the
	// pokeEcho discount window and masks a stuck rewind.
	if !pk.at.After(stale) {
		t.Fatalf("poke at = %v, want it re-stamped after the stale rewind poke %v", pk.at, stale)
	}

	// Behavioral consequence the review requires: once grace elapses with only
	// gc's own picker keystroke echo as window activity, the discount reveals the
	// genuine pre-rewind activity, not gc's echo. Drive the pure discount with the
	// recorded poke and a synthetic now so the assertion stays deterministic.
	echo := pk.at // window_activity is only the picker keystroke's own echo
	if got := discountPokeActivity(echo, pk, pk.at.Add(pokeGrace+time.Second)); !got.Equal(genuine) {
		t.Errorf("post-grace unanswered picker keystroke resolved to %v, want the genuine prior %v", got, genuine)
	}
}

// TestSendKeysHiddenAttachedUnknownKeyFallsThroughWithoutPartialWrite pins the
// other half of the sendHiddenAttachedKeys fix: every key is resolved to its
// byte sequence BEFORE anything is written, so a key hiddenAttachedKeyBytes
// cannot resolve falls the whole call through to the SendKeysRaw path with no
// partial hidden-client write and no recorded poke. Before the fix the resolve
// and write were interleaved per key, so SendKeys(sess, "Enter", <unknown>)
// wrote the Enter to the hidden client, then fell through and re-sent BOTH keys
// via SendKeysRaw — a double-delivered Enter.
func TestSendKeysHiddenAttachedUnknownKeyFallsThroughWithoutPartialWrite(t *testing.T) {
	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe

	const sess = "hidden-attach-unknown-key"
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{
		sess: {stdin: sink},
	}
	tm.hiddenAttachMu.Unlock()

	p := &Provider{tm: tm}
	// "F5" has no hiddenAttachedKeyBytes mapping; "Enter" does. The unknown key
	// must veto the hidden write for the whole sequence, not just for itself.
	if err := p.SendKeys(sess, "Enter", "F5"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	if got := sink.written(); got != "" {
		t.Fatalf("hidden client received %q, want no partial write before the unknown-key fallthrough (the Enter would be double-delivered by the raw fallback)", got)
	}

	tm.pokeMu.Lock()
	_, ok := tm.pokes[sess]
	tm.pokeMu.Unlock()
	if ok {
		t.Fatal("unknown-key fallthrough recorded a poke; nothing was delivered through the hidden client")
	}

	// The raw fallback must still deliver BOTH keys, in order, via send-keys.
	var sent []string
	for _, call := range fe.calls {
		for i, arg := range call {
			if arg == "send-keys" && len(call) > i+3 && call[i+1] == "-t" && call[i+2] == sess {
				sent = append(sent, call[i+3])
			}
		}
	}
	if len(sent) != 2 || sent[0] != "Enter" || sent[1] != "F5" {
		t.Fatalf("SendKeysRaw fallback delivered %v, want [Enter F5]", sent)
	}
}
