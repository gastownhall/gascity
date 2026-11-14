package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

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
	// New returns a provider holding no session, and a reader of the input
	// it typed into a session: one entry per nudge (its text) or keys
	// call, in order. With refuse set, the runtime answers every nudge
	// with that outcome instead of delivering it; New may skip t for a
	// word it cannot produce.
	New func(t *testing.T, refuse runtime.InputOutcome) (runtime.Provider, func(name string) []string)
	// Config is the start config of a session; dialog asks for an agent
	// that shows a startup dialog at launch, which the runtime's dialog
	// pass answers with keys. The cases add the first turn.
	Config func(t *testing.T, dialog bool) runtime.Config
}

// unweldedRefusals are the outcomes of a first turn the runtime did not
// take, which the un-welded path hands back.
var unweldedRefusals = []runtime.InputOutcome{
	runtime.InputOutcomePendingInteraction, runtime.InputOutcomeBusy,
	runtime.InputOutcomeRejected, runtime.InputOutcomeNotSent,
}

// RunUnweldedLaunchContract runs the RUNTIME-CONTRACT-020 cases: on the
// un-welded launch path the runtime delivers the start config's first turn
// itself, once, after the startup dialog pass (FirstTurnDelivered,
// DialogAtStart); a first turn the runtime refused is handed back to the
// Start's caller ([runtime.TakeStartupNudgeHandback]) and never dropped or
// typed, the start succeeding (RefusedHandedBack); and a gone answer is the
// agent dying during startup ([runtime.ErrSessionDiedDuringStartup]).
func RunUnweldedLaunchContract(t *testing.T, f UnweldedLaunchFixture) {
	t.Helper()
	t.Run("UnweldedLaunch_FirstTurnDelivered", func(t *testing.T) {
		sp, input := f.New(t, "")
		if err := checkFirstTurnDelivered(t, sp, input, f.Config(t, false)); err != nil {
			t.Error(err)
		}
	})
	t.Run("UnweldedLaunch_DialogAtStart", func(t *testing.T) {
		sp, input := f.New(t, "")
		if err := checkDialogAtStart(t, sp, input, f.Config(t, true)); err != nil {
			t.Error(err)
		}
	})
	for _, word := range unweldedRefusals {
		t.Run("UnweldedLaunch_RefusedHandedBack_"+string(word), func(t *testing.T) {
			sp, input := f.New(t, word)
			if err := checkRefusedHandedBack(t, sp, input, f.Config(t, false), word); err != nil {
				t.Error(err)
			}
		})
	}
	t.Run("UnweldedLaunch_GoneDiesDuringStartup", func(t *testing.T) {
		sp, _ := f.New(t, runtime.InputOutcomeGone)
		if err := checkGoneDiesDuringStartup(t, sp, f.Config(t, false)); err != nil {
			t.Error(err)
		}
	})
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

// checkFirstTurnDelivered reports how sp breaks FirstTurnDelivered.
func checkFirstTurnDelivered(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start = %w, want nil", err)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 1 {
		return fmt.Errorf("runtime contract 020: input %q holds the first turn %d times, want once", got, firstTurns(got))
	}
	if turn, ok := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession); ok {
		return fmt.Errorf("runtime contract 020: a delivered first turn was also handed back (%q), which delivers it twice", turn)
	}
	return nil
}

// checkDialogAtStart reports how sp breaks DialogAtStart: the dialog's keys
// precede the first turn, which is typed once.
func checkDialogAtStart(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start with a startup dialog = %w, want nil", err)
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

// checkRefusedHandedBack reports how sp breaks RefusedHandedBack for a first
// turn refused with word.
func checkRefusedHandedBack(t *testing.T, sp runtime.Provider, input func(string) []string, cfg runtime.Config, word runtime.InputOutcome) error {
	if err := startUnwelded(t, sp, cfg); err != nil {
		return fmt.Errorf("runtime contract 020: Start whose first turn was refused %s = %w, want nil: a refused turn never fails a start", word, err)
	}
	if got := input(unweldedLaunchSession); firstTurns(got) != 0 {
		return fmt.Errorf("runtime contract 020: input %q holds a first turn the runtime refused %s", got, word)
	}
	turn, ok := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession)
	if !ok || turn != unweldedFirstTurn {
		return fmt.Errorf("runtime contract 020: handback after a first turn refused %s = %q, %v; want the first turn: it was dropped", word, turn, ok)
	}
	if _, again := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession); again {
		return errors.New("runtime contract 020: the handback was taken twice")
	}
	return nil
}

// checkGoneDiesDuringStartup reports how sp breaks GoneDiesDuringStartup.
func checkGoneDiesDuringStartup(t *testing.T, sp runtime.Provider, cfg runtime.Config) error {
	err := startUnwelded(t, sp, cfg)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		return fmt.Errorf("runtime contract 020: Start whose first turn found the session gone = %w, want runtime.ErrSessionDiedDuringStartup", err)
	}
	if turn, ok := runtime.TakeStartupNudgeHandback(sp, unweldedLaunchSession); ok {
		return fmt.Errorf("runtime contract 020: a failed start handed back %q", turn)
	}
	return nil
}
