package runtime

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoTurnEventSource reports that a provider has no turn-event stream to
// offer. Composite providers (auto, hybrid) always have a
// SubscribeTurnEvents method, so they return it when none of their backends
// publishes turn events; an enclosing composite then treats that backend as
// a non-implementer instead of a failed subscribe.
var ErrNoTurnEventSource = errors.New("no backend implements SubscribeTurnEvents")

// TurnEventSource names one backend of a composite provider for
// [SubscribeTurnEventSources].
type TurnEventSource struct {
	// Name identifies the backend in errors (e.g. "default").
	Name string
	// Provider is the backend; it contributes events only when it implements
	// [TurnEventProvider].
	Provider Provider
}

// SubscribeTurnEventSources subscribes to every source that implements
// [TurnEventProvider] and fans their streams into one, for composite
// providers that route sessions across backends. It follows the rules of
// [SubscribeSessionEventSources]:
//
//   - A source whose subscribe returns [ErrNoTurnEventSource] (a nested
//     composite without a turn-event backend) counts as not implementing it.
//   - Any other subscribe error fails the whole subscribe. Streams already
//     opened close when ctx is done.
//   - With no stream, the error wraps [ErrNoTurnEventSource].
//   - One stream is returned unchanged; two are merged with
//     [MergeTurnEvents].
func SubscribeTurnEventSources(ctx context.Context, a, b TurnEventSource) (<-chan TurnEvent, error) {
	var streams []<-chan TurnEvent
	for _, src := range []TurnEventSource{a, b} {
		tep, ok := src.Provider.(TurnEventProvider)
		if !ok {
			continue
		}
		ch, err := tep.SubscribeTurnEvents(ctx)
		if errors.Is(err, ErrNoTurnEventSource) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s backend: %w", src.Name, err)
		}
		streams = append(streams, ch)
	}
	switch len(streams) {
	case 0:
		return nil, fmt.Errorf("%w (%s, %s)", ErrNoTurnEventSource, a.Name, b.Name)
	case 1:
		return streams[0], nil
	}
	return MergeTurnEvents(ctx, streams[0], streams[1]), nil
}

// MergeTurnEvents fans two turn-event streams into one, closing the output
// only when ctx is done or both inputs close. A turn runs in one session on
// one backend, so its started and completed events come from the same input
// and keep their order.
func MergeTurnEvents(ctx context.Context, a, b <-chan TurnEvent) <-chan TurnEvent {
	return mergeStreams(ctx, a, b)
}

// mergeStreams fans two streams into one, closing the output only when ctx
// is done or both inputs close. It never closes the output because one side
// has gone quiet or closed early.
func mergeStreams[E any](ctx context.Context, a, b <-chan E) <-chan E {
	out := make(chan E)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			var ev E
			var ok bool
			select {
			case <-ctx.Done():
				return
			case ev, ok = <-a:
				if !ok {
					a = nil
					continue
				}
			case ev, ok = <-b:
				if !ok {
					b = nil
					continue
				}
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
