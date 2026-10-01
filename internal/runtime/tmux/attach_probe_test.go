package tmux

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// attachProbeArgs is the exact argv of the attachment probe for worker-1: an
// exact pane target plus the session-name echo the answer is checked against.
var attachProbeArgs = []string{"-u", "display-message", "-t", "=worker-1:", "-p", "#{session_name}|#{session_attached}"}

func attachProbeTmux(out string, err error) (*Tmux, *fakeExecutor) {
	ex := &fakeExecutor{out: out, err: err}
	return &Tmux{exec: ex}, ex
}

// #{session_attached} is a client count: a human attached next to gc's
// hidden attach client makes it 2, which "== 1" read as detached.
func TestSessionAttachedWithErrorCountsClients(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"worker-1|0", false},
		{"worker-1|1", true},
		{"worker-1|2", true},
		{"worker-1|3\n", true},
	} {
		t.Run(tc.out, func(t *testing.T) {
			tm, ex := attachProbeTmux(tc.out, nil)
			got, err := tm.SessionAttachedWithError("worker-1")
			if err != nil || got != tc.want {
				t.Fatalf("SessionAttachedWithError = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			if len(ex.calls) != 1 || !slices.Equal(ex.calls[0], attachProbeArgs) {
				t.Fatalf("calls = %q, want [%q]", ex.calls, attachProbeArgs)
			}
			if got := tm.IsSessionAttached("worker-1"); got != tc.want {
				t.Errorf("IsSessionAttached = %v, want %v", got, tc.want)
			}
			p := &Provider{tm: tm}
			if got, err := p.IsAttachedWithError("worker-1"); err != nil || got != tc.want {
				t.Errorf("Provider.IsAttachedWithError = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
			// The bool path legacy callers read must stay on the probe.
			if got := p.IsAttached("worker-1"); got != tc.want {
				t.Errorf("Provider.IsAttached = %v, want %v", got, tc.want)
			}
		})
	}
}

// tmux 3.4 answers display-message for a missing exact target with rc 0 and
// an empty expansion, so the echo is the only not-found signal.
func TestSessionAttachedWithErrorMissingSessionIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{"empty expansion", "", nil},
		{"empty fields", "|", nil},
		{"empty fields with newline", "|\n", nil},
		{"can't find session", "", wrapError(errors.New("exit status 1"), "can't find session: worker-1", []string{"display-message"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm, _ := attachProbeTmux(tc.out, tc.err)
			got, err := tm.SessionAttachedWithError("worker-1")
			if got || !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("SessionAttachedWithError = (%v, %v), want (false, runtime.ErrSessionNotFound)", got, err)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("not-found error %v also wraps ErrRuntimeUnavailable", err)
			}
		})
	}
}

// An empty -t answers for the current or most recent session, which is not
// the one asked about, so a blank target must not reach tmux.
func TestSessionAttachedWithErrorBlankTargetIsNotFound(t *testing.T) {
	for _, target := range []string{"", " "} {
		t.Run(fmt.Sprintf("%q", target), func(t *testing.T) {
			tm, ex := attachProbeTmux("worker-1|1", nil)
			got, err := tm.SessionAttachedWithError(target)
			if got || !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("SessionAttachedWithError = (%v, %v), want (false, runtime.ErrSessionNotFound)", got, err)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("not-found error %v also wraps ErrRuntimeUnavailable", err)
			}
			if len(ex.calls) != 0 {
				t.Errorf("calls = %q, want none", ex.calls)
			}
		})
	}
}

// A probe that could not answer must say so. Callers gating a destructive
// action treat that error as attached; false-with-nil would let them proceed.
func TestSessionAttachedWithErrorProbeFailureIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{"no server", "", ErrNoServer},
		{"no current target", "", ErrNoCurrentTarget},
		{"executor failure", "", fmt.Errorf("tmux display-message: %w", errors.New("signal: killed"))},
		{"unparsable count", "worker-1|x", nil},
		{"missing count", "worker-1", nil},
		{"negative count", "worker-1|-1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm, _ := attachProbeTmux(tc.out, tc.err)
			got, err := tm.SessionAttachedWithError("worker-1")
			if got || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("SessionAttachedWithError = (%v, %v), want (false, runtime.ErrRuntimeUnavailable)", got, err)
			}
			if errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("probe failure %v reads as not-found", err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error %v dropped the executor cause %v", err, tc.err)
			}
			// ErrNoServer's text "no tmux server running" matches
			// IsSessionGone; an unreachable server is not a gone session.
			if runtime.IsSessionGone(err) {
				t.Errorf("probe failure %q reads as gone to runtime.IsSessionGone", err)
			}
		})
	}
}

// A pane id or a qualified target names no single session, so the echo is
// not compared and the count is read after the last "|".
func TestSessionAttachedWithErrorPassesThroughNonNameTargets(t *testing.T) {
	for _, tc := range []struct {
		target string
		out    string
		want   bool
	}{
		{"%5", "worker-1|1", true},
		{"%5", "worker-1|0", false},
		{"sess:1.0", "sess|2", true},
		{"%7", "odd|name|2", true},
	} {
		t.Run(tc.target+" "+tc.out, func(t *testing.T) {
			tm, ex := attachProbeTmux(tc.out, nil)
			got, err := tm.SessionAttachedWithError(tc.target)
			if err != nil || got != tc.want {
				t.Fatalf("SessionAttachedWithError(%q) = (%v, %v), want (%v, nil)", tc.target, got, err, tc.want)
			}
			want := []string{"-u", "display-message", "-t", tc.target, "-p", "#{session_name}|#{session_attached}"}
			if len(ex.calls) != 1 || !slices.Equal(ex.calls[0], want) {
				t.Fatalf("calls = %q, want [%q]", ex.calls, want)
			}
		})
	}
}

// An answer for another session is not an answer for this one.
func TestSessionAttachedWithErrorRejectsForeignName(t *testing.T) {
	tm, _ := attachProbeTmux("worker-10|1", nil)
	got, err := tm.SessionAttachedWithError("worker-1")
	if got || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("SessionAttachedWithError = (%v, %v), want (false, runtime.ErrRuntimeUnavailable)", got, err)
	}
	if tm.IsSessionAttached("worker-1") {
		t.Error("IsSessionAttached = true for an answer about worker-10")
	}
}

func TestSessionRosterCountsMultipleClients(t *testing.T) {
	ex := &fakeExecutor{outs: []string{
		"multi|2\nnone|0\nsingle|1\nodd|name|2", // list-sessions
		"100",                                   // list-windows multi
		"100",                                   // list-windows none
		"100",                                   // list-windows odd|name
		"100",                                   // list-windows single
	}}
	tm := &Tmux{exec: ex}
	roster, err := tm.SessionRoster()
	if err != nil {
		t.Fatalf("SessionRoster: %v", err)
	}
	for name, want := range map[string]bool{"multi": true, "none": false, "single": true, "odd|name": true} {
		if got := roster[name].Attached; got != want {
			t.Errorf("roster[%q].Attached = %v, want %v", name, got, want)
		}
	}
}

func TestGetSessionInfoCountsMultipleClients(t *testing.T) {
	for _, tc := range []struct {
		clients string
		want    bool
	}{
		{"0", false},
		{"1", true},
		{"2", true},
	} {
		tm := &Tmux{exec: &fakeExecutor{out: "worker-1|1|1700000000|" + tc.clients + "|1700000001|1700000002"}}
		info, err := tm.GetSessionInfo("worker-1")
		if err != nil {
			t.Fatalf("GetSessionInfo: %v", err)
		}
		if info.Attached != tc.want {
			t.Errorf("GetSessionInfo with %s clients: Attached = %v, want %v", tc.clients, info.Attached, tc.want)
		}
	}
}

// seamBackedProvider routes IsAttached through the seams but carries the
// error-bearing probe by embedding: a wrapper that shadowed it with the seam
// bool would turn every probe failure back into "not attached".
func TestSeamBackedProviderPromotesAttachmentObserver(t *testing.T) {
	tm, ex := attachProbeTmux("", ErrNoServer)
	// A live cache entry lets the seam route reach its own attachment read,
	// so a shadowing wrapper fails on the lost error rather than on setup.
	raw := &Provider{tm: tm, cache: NewStateCache(&mockFetcher{sessions: map[string]bool{"worker-1": true}}, time.Hour)}
	var sp runtime.Provider = &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(raw.Seams())}

	if _, ok := sp.(runtime.AttachmentObserverWithError); !ok {
		t.Fatal("seam-backed tmux provider does not implement AttachmentObserverWithError")
	}
	got, err := runtime.IsAttachedWithError(sp, "worker-1")
	if got || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("IsAttachedWithError = (%v, %v), want (false, runtime.ErrRuntimeUnavailable)", got, err)
	}
	if len(ex.calls) != 1 || !slices.Equal(ex.calls[0], attachProbeArgs) {
		t.Fatalf("calls = %q, want only the attachment probe %q", ex.calls, attachProbeArgs)
	}
}

// attachProbeFailsExecutor fails only the attachment probe. Every other tmux
// call answers as a detached codex pane holding a staged paste in its
// composer: idle, draft visible, so both input-clearing gates in nudgeSession
// are reachable.
type attachProbeFailsExecutor struct {
	calls [][]string
}

func (e *attachProbeFailsExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	switch {
	case slices.Contains(args, "#{session_name}|#{session_attached}"):
		return "", errors.New("server busy")
	case slices.Contains(args, "show-environment") && slices.Contains(args, "GC_PROVIDER"):
		return "GC_PROVIDER=codex", nil
	case slices.Contains(args, "capture-pane"):
		return "› [Pasted Content 120 chars]", nil
	}
	return "", nil
}

func (e *attachProbeFailsExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// A failed attachment probe cannot tell whether a human is composing, so
// nudgeSession must neither clear the line (C-u) nor re-send the submit over a
// staged draft (#5192). Reading the failure as "detached" wipes or sends the
// operator's draft.
func TestNudgeSessionAttachProbeErrorKeepsInput(t *testing.T) {
	ex := &attachProbeFailsExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("worker-1", "hello world")
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitUnconfirmed", err)
	}
	enters := 0
	for _, c := range ex.calls {
		if !slices.Contains(c, "send-keys") {
			continue
		}
		if slices.Contains(c, "C-u") {
			t.Fatalf("sent C-u with the attachment probe failing: %q", c)
		}
		if c[len(c)-1] == "Enter" {
			enters++
		}
	}
	// Only the confirm window's submits; staged-draft recovery adds more.
	if enters != submitEnterMaxSends {
		t.Fatalf("submit Enter sends = %d, want %d (no staged-draft resubmit)", enters, submitEnterMaxSends)
	}
}
