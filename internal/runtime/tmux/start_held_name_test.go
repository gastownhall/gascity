package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// heldNameToken is the GC_INSTANCE_TOKEN every start in this file supplies. It
// is per bead, so two attempts at one bead carry the same value and a name held
// by one attempt reads as "mine" to the other.
const heldNameToken = "token-of-this-bead"

// heldNameTmux scripts a tmux server for a Provider.Start that may find its
// session name already held, answering by tmux verb and recording every call.
//
// Answering by verb keeps a change in how many probes Start makes from shifting
// an answer onto the wrong command. No answer carries a pane PID:
// KillSessionWithProcesses builds its kill plan from the host process table, so
// a numeric PID would let a test signal whatever real process owns it. The
// pane-state read behind that plan fails instead, which still lets the
// kill-session itself through.
type heldNameTmux struct {
	calls [][]string

	// createErr answers the create verb; nil means this attempt created the
	// session.
	createErr error
	// onCreate runs as the create verb arrives, before it is answered.
	onCreate func()
	// hasSessionErrs answers has-session in order. Once spent, the name is held.
	hasSessionErrs []error
	// paneCommand is the held session's pane_current_command.
	paneCommand string
	// liveToken is the GC_INSTANCE_TOKEN the session reports to show-environment.
	liveToken string
	// listRows answers list-panes -a.
	listRows string
}

func (h *heldNameTmux) execute(args []string) (string, error) {
	h.calls = append(h.calls, slices.Clone(args))
	switch tmuxSubcommand(args) {
	case "start-server", "new-session":
		// GC_INSTANCE_TOKEN is not argv-safe, so the create reaches tmux as
		// `start-server ; source-file <private file>` rather than as new-session.
		if h.onCreate != nil {
			h.onCreate()
		}
		return "", h.createErr
	case "has-session":
		if len(h.hasSessionErrs) == 0 {
			return "", nil
		}
		err := h.hasSessionErrs[0]
		h.hasSessionErrs = h.hasSessionErrs[1:]
		return "", err
	case "display-message":
		format := args[len(args)-1]
		switch {
		case strings.Contains(format, "#{pane_pid}"):
			return "", errors.New("pane pid unavailable")
		case strings.Contains(format, "#{pane_current_command}"):
			return h.paneCommand, nil
		case strings.Contains(format, "#{pane_dead}"):
			return "0", nil
		}
	case "show-environment":
		return "GC_INSTANCE_TOKEN=" + h.liveToken, nil
	case "list-panes":
		return h.listRows, nil
	}
	return "", nil
}

func (h *heldNameTmux) executeCtx(_ context.Context, args []string) (string, error) {
	return h.execute(args)
}

// tmuxVerbCount counts the calls whose tmux subcommand is verb.
func tmuxVerbCount(calls [][]string, verb string) int {
	n := 0
	for _, call := range calls {
		if tmuxSubcommand(call) == verb {
			n++
		}
	}
	return n
}

// confineStagedFiles keeps a Start under test off the host. The instance token
// reaches tmux through a private command file under os.TempDir, and the sweep
// that precedes staging reads that directory, so both go to a per-test one; the
// agent-slice wrapper is switched off so the launch command is not rewritten
// from the host environment.
func confineStagedFiles(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(AgentSliceEnv, "")
}

// newHeldNameProvider is a Provider over fx. Its state cache lists through fx
// with the process-table scan gated off, so no test reads the host's.
func newHeldNameProvider(fx *heldNameTmux) *Provider {
	tm := &Tmux{exec: fx}
	fetcher := &tmuxFetcher{tm: tm}
	fetcher.snapshotGate.nextAttempt = time.Now().Add(time.Hour)
	return &Provider{
		tm:       tm,
		cfg:      DefaultConfig(),
		cache:    NewStateCache(fetcher, time.Hour),
		workDirs: map[string]string{},
	}
}

func heldNameConfig(freshOnly bool, processNames []string) runtime.Config {
	return runtime.Config{
		Command:      "claude",
		ProcessNames: processNames,
		FreshOnly:    freshOnly,
		Env:          map[string]string{"GC_INSTANCE_TOKEN": heldNameToken},
	}
}

// A start that finds its name already held created nothing, so it owes the
// holder a refusal and nothing else. The teardown after a failed start tells
// mine from theirs by GC_INSTANCE_TOKEN, and every attempt at a bead carries
// that bead's one token, so it kills the live agent another attempt started
// under the same name (#7170). Kills a Start that tears down a session it did
// not create, on each way of finding the name held: the legacy create that
// meets a live agent, the legacy create that cannot tell zombie from healthy
// because no process names are configured, and the FreshOnly create that loses
// the race after its held-name probe came back clean.
func TestStartRefusedHeldNameKillsNothing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		freshOnly    bool
		processNames []string
		hasSession   []error
	}{
		{"live agent", false, []string{"claude"}, nil},
		{"live without process names", false, nil, nil},
		{"FreshOnly create race", true, []string{"claude"}, []error{ErrSessionNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confineStagedFiles(t)
			fx := &heldNameTmux{
				createErr:      ErrSessionExists,
				hasSessionErrs: tc.hasSession,
				paneCommand:    "claude",
				liveToken:      heldNameToken,
			}
			p := newHeldNameProvider(fx)

			err := p.Start(context.Background(), "gc-test", heldNameConfig(tc.freshOnly, tc.processNames))

			t.Logf("tmux calls: %q", fx.calls)
			if !errors.Is(err, runtime.ErrSessionExists) {
				t.Errorf("Start = %v, want runtime.ErrSessionExists", err)
			}
			if kills := tmuxVerbCount(fx.calls, "kill-session"); kills != 0 {
				t.Errorf("Start issued %d kill-session for a name it did not create, want 0", kills)
			}
		})
	}
}

// The refusal guard keys on the refusal, never on the token alone: a start that
// did create the session and then failed still owns it, and its teardown is
// what keeps a failed attempt from leaving a half-started agent behind.
// Canceling the context as the create lands fails the attempt after it
// created the session, with the holder's token equal to its own. Kills a guard
// that skips the teardown for every failed start.
func TestStartFailureAfterOwnCreateStillKillsSession(t *testing.T) {
	confineStagedFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx := &heldNameTmux{
		onCreate:    cancel,
		paneCommand: "claude",
		liveToken:   heldNameToken,
	}
	p := newHeldNameProvider(fx)

	err := p.Start(ctx, "gc-test", heldNameConfig(false, []string{"claude"}))

	t.Logf("tmux calls: %q", fx.calls)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want context.Canceled", err)
	}
	if kills := tmuxVerbCount(fx.calls, "kill-session"); kills != 1 {
		t.Errorf("Start issued %d kill-session after failing past its own create, want 1", kills)
	}
}

// A refused start still ends the cache's view of the name: another attempt is
// mid-start on it, and the cache has no way to see that. The read after the
// refusal must therefore be a fresh list, not a hit on the snapshot taken
// before it, whether or not the holder's token matched. Kills a refusal that
// returns without invalidating the cache.
func TestStartRefusedHeldNameInvalidatesStateCache(t *testing.T) {
	for _, tc := range []struct {
		name      string
		liveToken string
	}{
		{"holder's token matches", heldNameToken},
		{"holder's token differs", "token-of-another-bead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confineStagedFiles(t)
			fx := &heldNameTmux{
				createErr:   ErrSessionExists,
				paneCommand: "claude",
				liveToken:   tc.liveToken,
				listRows:    "gc-test\t0\tclaude\t\t0\t1000\t$1\t900",
			}
			p := newHeldNameProvider(fx)
			if !p.IsRunning("gc-test") {
				t.Fatalf("priming read: IsRunning = false, want the snapshot to list gc-test; tmux calls = %q", fx.calls)
			}

			err := p.Start(context.Background(), "gc-test", heldNameConfig(false, []string{"claude"}))
			if !errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("Start = %v, want runtime.ErrSessionExists", err)
			}
			listsBefore := tmuxVerbCount(fx.calls, "list-panes")
			p.IsRunning("gc-test")

			t.Logf("tmux calls: %q", fx.calls)
			if got := tmuxVerbCount(fx.calls, "list-panes"); got != listsBefore+1 {
				t.Errorf("list-panes calls = %d after the read that followed the refusal, want %d: the read hit a stale cache", got, listsBefore+1)
			}
		})
	}
}

// On the legacy path the held name only surfaces when the create runs, after
// pre_start. That order is what makes the refusal unable to undo a hook's side
// effects, and this fix leaves it as it is. The create hook records whether the
// pre_start marker already existed when the create arrived. Kills a change that
// probes for the held name ahead of pre_start without the rest of that move.
func TestStartLegacyHeldNameRunsPreStartBeforeRefusal(t *testing.T) {
	confineStagedFiles(t)
	marker := filepath.Join(t.TempDir(), "pre-start-ran")
	markerAtCreate := false
	fx := &heldNameTmux{
		createErr:   ErrSessionExists,
		paneCommand: "claude",
		liveToken:   heldNameToken,
	}
	fx.onCreate = func() {
		_, err := os.Stat(marker)
		markerAtCreate = err == nil
	}
	p := newHeldNameProvider(fx)
	cfg := heldNameConfig(false, []string{"claude"})
	cfg.PreStart = []string{"touch " + marker}

	err := p.Start(context.Background(), "gc-test", cfg)

	t.Logf("tmux calls: %q", fx.calls)
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start = %v, want runtime.ErrSessionExists", err)
	}
	if !markerAtCreate {
		t.Error("pre_start had not run when the create reached tmux, want it to run first")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("pre_start marker: %v, want it left behind by the refused start", statErr)
	}
}
