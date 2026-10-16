package worker

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A RuntimeHandle has no session row and so no transcript: its turns come from
// its provider alone. A provider that observes turns is a protocol source;
// anything else, tmux included, is none. Nor has it deliver-now (its Interrupt
// sends the provider's Interrupt alone, without a family's steps), so it
// replaces no turn: Replace is none.

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
		false, false, TurnReplaceNone,
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
		ch, err := tp.SubscribeTurnEvents(ctx)
		events = turnHints(h.sessionName, ch, err)
	}
	return startTurnWatch(ctx, protocolTurnWatch(defaultTurnTiming, h.protocolTurnHistory, events, h.sessionName)), nil
}

func (h *RuntimeHandle) noTurnSource() error {
	return fmt.Errorf("turns of %s: %w", h.sessionName, runtime.ErrTurnIdentityUnsupported)
}

// protocolTurnHistory reads the provider's history of the session, bounded by
// ctx and shared with concurrent reads of it. Its key is scoped to the city
// the handle was built for, as a SessionHandle's is, so both handles of one
// city's session share one read; a handle built without a city is scoped to
// its provider, which a city does not share with another.
func (h *RuntimeHandle) protocolTurnHistory(ctx context.Context) (runtime.TurnHistory, error) {
	tp, ok := h.provider.(runtime.TurnObservationProvider)
	if !ok {
		return runtime.TurnHistory{}, h.noTurnSource()
	}
	city, err := sessionpkg.NewCityDir(h.cityPath)
	key := turnObservationKey("history", turnCityScope(city, err, h.provider), h.sessionName)
	return ObserveShared(ctx, key, func(ctx context.Context) (runtime.TurnHistory, error) {
		hist, err := tp.TurnHistory(ctx, h.sessionName)
		if err != nil {
			return runtime.TurnHistory{}, err
		}
		return hist, nil
	})
}
