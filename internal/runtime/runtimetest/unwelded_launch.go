package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// unweldedLaunchSession is the session every RUNTIME-CONTRACT-020 case starts.
const unweldedLaunchSession = "unwelded-launch-contract"

// unweldedFirstTurn is the first turn every RUNTIME-CONTRACT-020 case starts
// its session with.
const unweldedFirstTurn = "unwelded launch contract first turn"

// UnweldedLaunchFixture supplies a runtime whose Start provisions a box and
// then launches the agent into it separately (an exec pack declaring
// proc.provision), the RUNTIME-CONTRACT-020 runtime.
type UnweldedLaunchFixture struct {
	// New returns a provider holding no session, a reader of the input it
	// typed into a session (one entry per nudge, its text, or keys call, in
	// order), and accept, which makes the runtime take input from then on.
	// With refuse set, the runtime answers every nudge with that outcome
	// instead of delivering it until accept is called; New may skip t for
	// a word it cannot produce.
	New func(t *testing.T, refuse runtime.InputOutcome) (sp runtime.Provider, input func(name string) []string, accept func())
	// Config is the start config of a session; dialog asks for an agent
	// that shows a startup dialog at launch, which the runtime's dialog
	// pass answers with keys. The cases add the first turn.
	Config func(t *testing.T, dialog bool) runtime.Config
	// ReadyFloor says the runtime cannot tell when its agent takes input
	// (no idle boundary), so the turn it hands back must not be sent
	// before the config's ReadyDelayMs has passed since the start.
	ReadyFloor bool
}

// unweldedRefusals are the outcomes of a nudge the runtime did not take,
// which the queue holding the handed-back first turn retries.
var unweldedRefusals = []runtime.InputOutcome{
	runtime.InputOutcomePendingInteraction, runtime.InputOutcomeBusy,
	runtime.InputOutcomeRejected, runtime.InputOutcomeNotSent,
}

// RunUnweldedLaunchContract runs the RUNTIME-CONTRACT-020 cases: on the
// un-welded launch path Start types nothing and hands the start config's
// first turn back to its caller ([runtime.TakeStartupNudgeHandback]) once
// (FirstTurnQueued); the startup dialog is answered at the start, before the
// queued turn is sent (DialogAtStart); the caller's queue, sending the
// handed-back turn through Nudge no earlier than its NotBefore, delivers it
// once (QueueDeliversOnce); and a turn the runtime refuses is answered with
// a typed refusal that types nothing, so the queue retries it and a later
// send delivers it once (RefusedRetriedByQueue); and the launch is
// identified from the moment Start returns: the instance token and both
// epochs its config carried are what [runtime.ReadLaunchID] and
// [runtime.ReadInputFence] read, the reads the queue binds, judges and
// fences the handed-back turn by, and a relaunch's replace them
// (LaunchIdentified).
func RunUnweldedLaunchContract(t *testing.T, f UnweldedLaunchFixture) {
	t.Helper()
	t.Run("UnweldedLaunch_LaunchIdentified", func(t *testing.T) {
		sp, _, _ := f.New(t, "")
		if err := checkLaunchIdentified(t, sp, f.Config(t, false)); err != nil {
			t.Error(err)
		}
	})
	t.Run("UnweldedLaunch_FirstTurnQueued", func(t *testing.T) {
		sp, input, _ := f.New(t, "")
		if err := checkFirstTurnQueued(t, sp, input, f.Config(t, false)); err != nil {
			t.Error(err)
		}
	})
	t.Run("UnweldedLaunch_DialogAtStart", func(t *testing.T) {
		sp, input, _ := f.New(t, "")
		if err := checkDialogAtStart(t, sp, input, f.Config(t, true)); err != nil {
			t.Error(err)
		}
	})
	t.Run("UnweldedLaunch_QueueDeliversOnce", func(t *testing.T) {
		sp, input, _ := f.New(t, "")
		if err := checkQueueDeliversOnce(t, sp, input, f.Config(t, false), f.ReadyFloor); err != nil {
			t.Error(err)
		}
	})
	for _, word := range unweldedRefusals {
		t.Run("UnweldedLaunch_RefusedRetriedByQueue_"+string(word), func(t *testing.T) {
			sp, input, accept := f.New(t, word)
			if err := checkRefusedRetriedByQueue(t, sp, input, accept, f.Config(t, false), word); err != nil {
				t.Error(err)
			}
		})
	}
}

// startUnwelded starts the contract session on sp with cfg and its first
// turn, and stops it when the test ends.
func startUnwelded(t *testing.T, sp runtime.Provider, cfg runtime.Config) error {
	t.Helper()
	cfg.Nudge = unweldedFirstTurn
	t.Cleanup(func() { _ = sp.Stop(unweldedLaunchSession) })
	return sp.Start(context.Background(), unweldedLaunchSession, cfg)
}

// firstTurns counts the first turn in input.
func firstTurns(input []string) int {
	n := 0
	for _, line := range input {
		if line == unweldedFirstTurn {
			n++
		}
	}
	return n
}

// takeQueuedTurn takes the turn the contract session's Start handed back,
// failing unless it is the first turn, handed back once.
func takeQueuedTurn(sp runtime.Provider) (runtime.StartupNudge, error) {
	turn, ok := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession)
	if !ok || turn.Text != unweldedFirstTurn {
		return turn, fmt.Errorf("runtime contract 020: handback after Start = %q, %v; want the first turn: the Start's caller had nothing to queue, so the turn was lost", turn.Text, ok)
	}
	if again, ok := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession); ok {
		return turn, fmt.Errorf("runtime contract 020: the handback was taken twice (%q), which queues the turn twice", again.Text)
	}
	return turn, nil
}

// sendQueued sends turn as the queue does: no earlier than its NotBefore,
// through Nudge.
func sendQueued(sp runtime.Provider, turn runtime.StartupNudge) error {
	if wait := time.Until(turn.NotBefore); wait > 0 {
		time.Sleep(wait)
	}
	return sp.Nudge(unweldedLaunchSession, runtime.TextContent(turn.Text))
}

// checkFirstTurnQueued reports how sp breaks FirstTurnQueued.
func checkFirstTurnQueued(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start = %w, want nil", err)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 0 {
		return fmt.Errorf("runtime contract 020: input %q after Start holds the first turn: Start types nothing, its caller's queue sends the turn", got)
	}
	_, err := takeQueuedTurn(sp)
	return err
}

// checkDialogAtStart reports how sp breaks DialogAtStart: Start answers the
// dialog, and the queued turn, sent after, is typed once behind its keys.
func checkDialogAtStart(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start with a startup dialog = %w, want nil", err)
	}
	if got := input(unweldedLaunchSession); len(got) == 0 {
		return errors.New("runtime contract 020: Start typed nothing into the startup dialog: the dialog pass answers it at the start")
	}
	turn, err := takeQueuedTurn(sp)
	if err != nil {
		return err
	}
	if err := sendQueued(sp, turn); err != nil && !runtime.NudgeDelivered(err) {
		return fmt.Errorf("runtime contract 020: sending the queued first turn after the dialog = %w, want delivered", err)
	}
	got := input(unweldedLaunchSession)
	switch at := slices.Index(got, unweldedFirstTurn); {
	case firstTurns(got) != 1:
		return fmt.Errorf("runtime contract 020: input %q holds the first turn %d times, want once", got, firstTurns(got))
	case at == 0:
		return fmt.Errorf("runtime contract 020: input %q: the first turn was typed before the startup dialog was answered", got)
	}
	return nil
}

// checkQueueDeliversOnce reports how sp breaks QueueDeliversOnce.
func checkQueueDeliversOnce(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config, readyFloor bool) error {
	began := time.Now()
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start = %w, want nil", err)
	}
	turn, err := takeQueuedTurn(sp)
	if err != nil {
		return err
	}
	if floor := began.Add(time.Duration(cfg.ReadyDelayMs) * time.Millisecond); readyFloor && turn.NotBefore.Before(floor) {
		return fmt.Errorf("runtime contract 020: the handed-back turn may be sent at %s, before ready_delay_ms (%d) has passed since the start (%s): a runtime without an idle boundary holds it that long", turn.NotBefore.Format(time.RFC3339Nano), cfg.ReadyDelayMs, floor.Format(time.RFC3339Nano))
	}
	if err := sendQueued(sp, turn); err != nil && !runtime.NudgeDelivered(err) {
		return fmt.Errorf("runtime contract 020: sending the queued first turn = %w, want delivered", err)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 1 {
		return fmt.Errorf("runtime contract 020: input %q holds the first turn %d times after the queue sent it, want once", got, firstTurns(got))
	}
	return nil
}

// checkRefusedRetriedByQueue reports how sp breaks RefusedRetriedByQueue for
// a turn refused with word.
func checkRefusedRetriedByQueue(t *testing.T, sp runtime.Provider, input func(string) []string, accept func(), cfg runtime.Config, word runtime.InputOutcome) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start of a runtime refusing input %s = %w, want nil: Start types nothing, so a refusal never fails it", word, err)
	}
	turn, err := takeQueuedTurn(sp)
	if err != nil {
		return err
	}
	err = sendQueued(sp, turn)
	switch {
	case err == nil:
		return fmt.Errorf("runtime contract 020: a queued first turn refused %s read as delivered, so the queue acks a turn never typed", word)
	case runtime.NudgeDelivered(err):
		return fmt.Errorf("runtime contract 020: a queued first turn refused %s read as delivered (%w), so the queue acks a turn never typed", word, err)
	case !errors.Is(err, word.Sentinel()):
		return fmt.Errorf("runtime contract 020: a queued first turn refused %s = %w, want the %s refusal sentinel, which the queue retries", word, err, word)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 0 {
		return fmt.Errorf("runtime contract 020: input %q holds a first turn the runtime refused %s", got, word)
	}
	accept()
	if err := sendQueued(sp, turn); err != nil && !runtime.NudgeDelivered(err) {
		return fmt.Errorf("runtime contract 020: the queue's retry of a turn refused %s = %w, want delivered once the runtime takes input", word, err)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 1 {
		return fmt.Errorf("runtime contract 020: input %q holds the first turn %d times after the retry, want once", got, firstTurns(got))
	}
	return nil
}

// withLaunchIdentity is cfg carrying the identity of one launch: its
// instance token and both epochs.
func withLaunchIdentity(cfg runtime.Config, token string, generation, epoch int) runtime.Config {
	env := make(map[string]string, len(cfg.Env)+3)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env[runtime.InstanceTokenEnv] = token
	env[runtime.RuntimeEpochEnv] = fmt.Sprint(generation)
	env[runtime.ContinuationEpochEnv] = fmt.Sprint(epoch)
	cfg.Env = env
	return cfg
}

// wantLaunchIdentified reports how sp's reads of the contract session miss
// the launch cfg started (after says when).
func wantLaunchIdentified(sp runtime.Provider, cfg runtime.Config, after string) error {
	token := cfg.Env[runtime.InstanceTokenEnv]
	if id, ok := runtime.ReadLaunchID(sp, unweldedLaunchSession); !ok || id != runtime.LaunchID(token) {
		return fmt.Errorf("runtime contract 020: ReadLaunchID %s = %q, %v; want this launch's %q: the queue holds a turn whose launch it cannot read, and drops one bound to the launch it reads", after, id, ok, runtime.LaunchID(token))
	}
	want, _ := runtime.InputFenceFromEnv(cfg.Env)
	if got, ok := runtime.ReadInputFence(sp, unweldedLaunchSession); !ok || got.Digest() != want.Digest() {
		return fmt.Errorf("runtime contract 020: ReadInputFence %s = %s, %v; want this launch's %s: the queue's pre-send read releases the turn on every pass, or fences it to another incarnation", after, got, ok, want)
	}
	return nil
}

// checkLaunchIdentified reports how sp breaks LaunchIdentified.
func checkLaunchIdentified(t *testing.T, sp runtime.Provider, cfg runtime.Config) error {
	first := withLaunchIdentity(cfg, "contract-020-first-launch", 3, 2)
	if err := startUnwelded(t, sp, first); err != nil {
		return fmt.Errorf("runtime contract 020: Start = %w, want nil", err)
	}
	if err := wantLaunchIdentified(sp, first, "after Start"); err != nil {
		return err
	}
	if _, err := takeQueuedTurn(sp); err != nil {
		return err
	}
	r, ok := sp.(runtime.RelaunchProvider)
	if !ok {
		return nil
	}
	second := withLaunchIdentity(cfg, "contract-020-second-launch", 4, 5)
	second.Nudge = unweldedFirstTurn
	if err := r.Relaunch(context.Background(), unweldedLaunchSession, second); err != nil {
		return fmt.Errorf("runtime contract 020: Relaunch = %w, want nil", err)
	}
	runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession)
	return wantLaunchIdentified(sp, second, "after a Relaunch")
}
