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
