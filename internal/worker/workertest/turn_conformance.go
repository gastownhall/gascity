package workertest

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
	worker "github.com/gastownhall/gascity/internal/worker"
)

// CheckTurnHandle checks h against the worker.TurnHandle contract
// (engdocs/design/x11-turn-identity-coverage.md §5.2), whatever its source:
//   - the capabilities are normalized, and name a known source, interrupt
//     mode and replacement;
//   - an unsupported interrupt mode has Fence none;
//   - a session with no source has nothing on every axis, and CurrentTurn and
//     WatchTurns refuse it with runtime.ErrTurnIdentityUnsupported;
//   - a source records turn starts; CurrentTurn either reads a well-formed
//     history or fails with an error that says why (unavailable, gone,
//     initializing or unidentifiable), never an empty error;
//   - WatchTurns sends an update first, history or error, and closes once
//     ctx ends.
//
// It returns the first violation.
func CheckTurnHandle(ctx context.Context, h worker.TurnHandle) error {
	caps, err := h.TurnCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("TurnCapabilities: %w", err)
	}
	if err := checkTurnCapabilities(caps); err != nil {
		return err
	}
	if caps.Source == worker.TurnSourceNone {
		if _, err := h.CurrentTurn(ctx); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			return fmt.Errorf("CurrentTurn with no source = %w, want ErrTurnIdentityUnsupported", err)
		}
		if _, err := h.WatchTurns(ctx); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			return fmt.Errorf("WatchTurns with no source = %w, want ErrTurnIdentityUnsupported", err)
		}
		return nil
	}
	hist, err := h.CurrentTurn(ctx)
	if err := checkTurnRead(hist, err); err != nil {
		return fmt.Errorf("CurrentTurn: %w", err)
	}
	watchCtx, cancel := context.WithCancel(ctx)
	ch, err := h.WatchTurns(watchCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("WatchTurns: %w", err)
	}
	select {
	case u, ok := <-ch:
		if !ok {
			cancel()
			return errors.New("WatchTurns closed before its first update")
		}
		if err := checkTurnRead(u.History, u.Err); err != nil {
			cancel()
			return fmt.Errorf("WatchTurns first update: %w", err)
		}
	case <-ctx.Done():
		cancel()
		return fmt.Errorf("WatchTurns sent no first update: %w", ctx.Err())
	}
	cancel()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("WatchTurns did not close after its ctx ended: %w", ctx.Err())
		}
	}
}

func checkTurnCapabilities(caps worker.TurnCapabilities) error {
	if caps.TurnCapabilities != caps.Normalized() {
		return fmt.Errorf("capabilities %+v are not normalized", caps.TurnCapabilities)
	}
	switch caps.Source {
	case worker.TurnSourceProtocol, worker.TurnSourceTranscript, worker.TurnSourceNone:
	default:
		return fmt.Errorf("unknown turn source %q", caps.Source)
	}
	switch caps.Interrupt {
	case runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol, runtime.TurnInterruptUnsupported:
	default:
		return fmt.Errorf("unknown interrupt mode %q", caps.Interrupt)
	}
	switch caps.Replace {
	case worker.TurnReplaceNone, worker.TurnReplaceInterrupt, worker.TurnReplaceInterruptReset, worker.TurnReplaceRestart:
	default:
		return fmt.Errorf("unknown replacement %q", caps.Replace)
	}
	if caps.Interrupt == runtime.TurnInterruptUnsupported && caps.Fence != runtime.TurnFenceNone {
		return fmt.Errorf("fence %q with an unsupported interrupt, want none", caps.Fence)
	}
	none := runtime.TurnCapabilities{}.Normalized()
	if caps.Source == worker.TurnSourceNone && caps.TurnCapabilities != none {
		return fmt.Errorf("no source but capabilities %+v, want none", caps.TurnCapabilities)
	}
	if caps.Source != worker.TurnSourceNone && caps.TurnCapabilities == none {
		return fmt.Errorf("source %q with no capabilities", caps.Source)
	}
	return nil
}

// checkTurnRead checks one read: a well-formed history, or an error that
// says why there is none, with no history beside it.
func checkTurnRead(hist runtime.TurnHistory, err error) error {
	if err != nil {
		if hist.Current.Active() || len(hist.Recent) > 0 {
			return fmt.Errorf("history %+v beside error %w", hist, err)
		}
		for _, known := range []error{runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound, runtime.ErrSessionInitializing, runtime.ErrTurnIdentityUnsupported} {
			if errors.Is(err, known) {
				return nil
			}
		}
		return fmt.Errorf("read error %w names no turn-read failure", err)
	}
	if hist.ObservedAt.IsZero() {
		return errors.New("history has no ObservedAt")
	}
	if hist.Current.Active() && !runtime.IsTurnIdentity(hist.Current.TurnID) {
		return fmt.Errorf("current turn %q is not a turn identity", hist.Current.TurnID)
	}
	for i, t := range hist.Recent {
		if !runtime.IsTurnIdentity(t.TurnID) {
			return fmt.Errorf("recent turn %d %q is not a turn identity", i, t.TurnID)
		}
		if t.TurnID == hist.Current.TurnID {
			return fmt.Errorf("recent turn %d %q is also the current turn", i, t.TurnID)
		}
	}
	return nil
}
