package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// hiddenQuestionTmux returns a Tmux whose hidden attach client for session is
// a recording sink, over exec. DebounceMs is 0 so a unit test does not sleep.
func hiddenQuestionTmux(exec executor, session string) (*Tmux, *recordingWriteCloser) {
	tm := &Tmux{cfg: DefaultConfig(), exec: exec}
	tm.cfg.DebounceMs = 0
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{session: {stdin: sink}}
	tm.hiddenAttachMu.Unlock()
	return tm, sink
}

// pokeRecorded reports whether tm recorded a poke for session.
func pokeRecorded(tm *Tmux, session string) bool {
	tm.pokeMu.Lock()
	defer tm.pokeMu.Unlock()
	_, ok := tm.pokes[session]
	return ok
}

// flippingPaneExecutor serves clear for its first capture and then pane, the
// way a question dialog opens while a hidden nudge's text is being typed.
type flippingPaneExecutor struct {
	paneExecutor
	clear string
}

func (e *flippingPaneExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "capture-pane") && e.captures() == 0 {
		e.calls = append(e.calls, slices.Clone(args))
		return e.clear, nil
	}
	return e.paneExecutor.execute(args)
}

func (e *flippingPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// An open AskUserQuestion owns the pane's input: the nudge's Enter would select
// the focused option. sendHiddenAttachedText must refuse before it writes
// anything, with the kind-aware pending error, and record no poke.
func TestSendHiddenAttachedTextRefusesQuestionDialogBeforeAnyWrite(t *testing.T) {
	const sess = "hidden-auq"
	ex := &paneExecutor{pane: askUserQuestionPane, inMode: "0"}
	tm, sink := hiddenQuestionTmux(ex, sess)

	handled, err := tm.sendHiddenAttachedText(context.Background(), sess, "/rewind")
	if !handled {
		t.Fatal("handled = false; the session has a hidden attach client, so the fallback path must not run")
	}
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("err = %v, want runtime.ErrPendingInteraction", err)
	}
	for _, want := range []string{"question", "gc session attach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if got := sink.written(); got != "" {
		t.Fatalf("hidden client received %q, want zero writes while a question dialog is open", got)
	}
	if pokeRecorded(tm, sess) {
		t.Error("a refused nudge recorded a poke; gc injected nothing")
	}
}

// A capture that fails cannot rule out a dialog, so the nudge is refused as a
// dialog on screen is, never written blind.
func TestSendHiddenAttachedTextRefusesWhenPaneCaptureFails(t *testing.T) {
	const sess = "hidden-capture-fail"
	ex := &captureFailExecutor{paneExecutor: paneExecutor{pane: askUserQuestionPane, inMode: "0"}}
	tm, sink := hiddenQuestionTmux(ex, sess)

	handled, err := tm.sendHiddenAttachedText(context.Background(), sess, "/rewind")
	if !handled {
		t.Fatal("handled = false; the session has a hidden attach client")
	}
	if !errors.Is(err, errPromptLookFailed) {
		t.Fatalf("err = %v, want errPromptLookFailed", err)
	}
	if got := sink.written(); got != "" {
		t.Fatalf("hidden client received %q, want zero writes when the pane cannot be read", got)
	}
	if pokeRecorded(tm, sess) {
		t.Error("a refused nudge recorded a poke; gc injected nothing")
	}
}

// A dialog that opens while the text is being typed must not receive the
// nudge's Enter: the look is repeated before the submit key.
func TestSendHiddenAttachedTextRefusesEnterWhenQuestionOpensAfterText(t *testing.T) {
	const sess = "hidden-auq-late"
	ex := &flippingPaneExecutor{paneExecutor: paneExecutor{pane: askUserQuestionPane, inMode: "0"}}
	tm, sink := hiddenQuestionTmux(ex, sess)

	handled, err := tm.sendHiddenAttachedText(context.Background(), sess, "/rewind")
	if !handled {
		t.Fatal("handled = false; the session has a hidden attach client")
	}
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("err = %v, want runtime.ErrPendingInteraction", err)
	}
	if got := sink.written(); strings.Contains(got, "\r") {
		t.Fatalf("hidden client received %q; Enter must not reach an open question dialog", got)
	}
	if pokeRecorded(tm, sess) {
		t.Error("an undelivered nudge recorded a poke")
	}
}

// A question dialog that was answered scrolls above the composer and no longer
// owns input; the nudge is delivered, Enter included, and poked.
func TestSendHiddenAttachedTextDeliversPastAnsweredQuestionDialog(t *testing.T) {
	for name, pane := range map[string]string{
		"answered, idle composer": answeredAskUserQuestionPane,
		"answered, agent working": answeredAskUserQuestionWorkingPane,
		"empty pane":              "",
	} {
		t.Run(name, func(t *testing.T) {
			const sess = "hidden-auq-stale"
			ex := &paneExecutor{pane: pane, inMode: "0"}
			tm, sink := hiddenQuestionTmux(ex, sess)

			handled, err := tm.sendHiddenAttachedText(context.Background(), sess, "/rewind")
			if !handled || err != nil {
				t.Fatalf("sendHiddenAttachedText = (%v, %v), want (true, nil) for a pane with no open dialog", handled, err)
			}
			if got, want := sink.written(), "/rewind\r"; got != want {
				t.Fatalf("hidden client received %q, want %q", got, want)
			}
			if !pokeRecorded(tm, sess) {
				t.Error("a delivered nudge recorded no poke")
			}
		})
	}
}
