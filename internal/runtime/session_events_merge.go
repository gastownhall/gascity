package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNoSessionEventSource reports that a provider has no session-event
// stream to offer. Composite providers (auto, hybrid) always have a
// SubscribeSessionEvents method, so they return it when none of their
// backends publishes events; an enclosing composite then treats that
// backend as a non-implementer instead of a failed subscribe.
var ErrNoSessionEventSource = errors.New("no backend implements SubscribeSessionEvents")

// SessionEventSource names one backend of a composite provider for
// [SubscribeSessionEventSources].
type SessionEventSource struct {
	// Name identifies the backend in errors (e.g. "default").
	Name string
	// Provider is the backend; it contributes events only when it implements
	// [SessionEventProvider].
	Provider Provider
}

// SubscribeSessionEventSources subscribes to every source that implements
// [SessionEventProvider] and fans their streams into one, for composite
// providers that route sessions across backends.
//
//   - A source whose subscribe returns [ErrNoSessionEventSource] (a nested
//     composite without an event-capable backend) counts as not
//     implementing it.
//   - Any other subscribe error fails the whole subscribe, so a caller never
//     believes a backend is covered when its events are missing. Streams
//     already opened close when ctx is done.
//   - With no stream, the error wraps [ErrNoSessionEventSource].
//   - One stream is returned unchanged; two are merged with
//     [MergeSessionEvents].
func SubscribeSessionEventSources(ctx context.Context, a, b SessionEventSource) (<-chan SessionEvent, error) {
	var streams []<-chan SessionEvent
	for _, src := range []SessionEventSource{a, b} {
		sep, ok := src.Provider.(SessionEventProvider)
		if !ok {
			continue
		}
		ch, err := sep.SubscribeSessionEvents(ctx)
		if errors.Is(err, ErrNoSessionEventSource) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s backend: %w", src.Name, err)
		}
		streams = append(streams, ch)
	}
	switch len(streams) {
	case 0:
		return nil, fmt.Errorf("%w (%s)", ErrNoSessionEventSource, strings.Join([]string{a.Name, b.Name}, ", "))
	case 1:
		return streams[0], nil
	}
	return MergeSessionEvents(ctx, streams[0], streams[1]), nil
}

// MergeSessionEvents fans two session-event streams into one, closing the
// output only when ctx is done or both inputs close. It never closes the
// output because one side has gone quiet or closed early. Each input
// already honors the [SessionEventProvider] contract (opening resync, loss
// coalesced into resync), so the merged stream does too.
func MergeSessionEvents(ctx context.Context, a, b <-chan SessionEvent) <-chan SessionEvent {
	out := make(chan SessionEvent)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			var ev SessionEvent
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
