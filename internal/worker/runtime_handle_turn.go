package worker

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A RuntimeHandle has no session row and so no transcript: its turns come from
// its provider alone. A provider that observes turns is a protocol source;
// anything else, tmux included, is none.

// TurnCapabilities reports what the provider can tell about the session's
// turns.
func (h *RuntimeHandle) TurnCapabilities(context.Context) (TurnCapabilities, error) {
	return h.turnCapabilities(), nil
}

func (h *RuntimeHandle) turnCapabilities() TurnCapabilities {
	return composeTurnCapabilities(
		runtime.TurnCapabilitiesFor(h.provider, h.sessionName),
		runtime.TurnInterruptModeFor(h.provider, h.sessionName),
		SessionLogAdapter{}.TurnSupportFor(h.providerName),
		false, false,
	)
}

// CurrentTurn reads the session's turns from the provider.
func (h *RuntimeHandle) CurrentTurn(ctx context.Context) (runtime.TurnHistory, error) {
	if h.turnCapabilities().Source != TurnSourceProtocol {
		return runtime.TurnHistory{}, h.noTurnSource()
	}
	return h.protocolTurnHistory(ctx)
}

// WatchTurns watches the session's turns from the provider, its turn events
// for the session waking the watcher between backstop polls.
func (h *RuntimeHandle) WatchTurns(ctx context.Context) (<-chan TurnUpdate, error) {
	if h.turnCapabilities().Source != TurnSourceProtocol {
		return nil, h.noTurnSource()
	}
	var events <-chan runtime.TurnEvent
	if tp, ok := h.provider.(runtime.TurnEventProvider); ok {
		if ch, err := tp.SubscribeTurnEvents(ctx); err == nil {
			events = ch
		}
	}
	return startTurnWatch(ctx, protocolTurnWatch(defaultTurnTiming, h.protocolTurnHistory, events, h.sessionName)), nil
}

func (h *RuntimeHandle) noTurnSource() error {
	return fmt.Errorf("turns of %s: %w", h.sessionName, runtime.ErrTurnIdentityUnsupported)
}

// protocolTurnHistory reads the provider's history of the session, bounded by
// ctx. A runtime handle knows no city, so its key is scoped to runtime
// handles: two cities' handles of one session name share the one-in-flight
// limit, which costs a retry, never a wrong answer.
func (h *RuntimeHandle) protocolTurnHistory(ctx context.Context) (runtime.TurnHistory, error) {
	tp, ok := h.provider.(runtime.TurnObservationProvider)
	if !ok {
		return runtime.TurnHistory{}, h.noTurnSource()
	}
	key := turnObservationKey("history", "runtime-handle", h.sessionName)
	return ObserveBounded(ctx, key, func(ctx context.Context) (runtime.TurnHistory, error) {
		return tp.TurnHistory(ctx, h.sessionName)
	})
}
