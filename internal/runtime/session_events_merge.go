package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// SessionEventSource names one backend of a composite provider for
// [SubscribeSessionEventSources].
type SessionEventSource struct {
	// Name identifies the backend in log lines and errors (e.g. "default").
	Name string
	// Provider is the backend; it contributes events only when it implements
	// [SessionEventProvider].
	Provider Provider
}

// SubscribeSessionEventSources subscribes to every source that implements
// [SessionEventProvider] and fans their streams into one, for composite
// providers that route sessions across backends.
//
//   - No implementer is an error.
//   - One implementer's stream (or subscribe error) is returned unchanged.
//   - With several, each is subscribed. A backend whose subscribe fails is
//     logged once to stderr and left out; if every subscribe fails, the
//     joined errors are returned. A single surviving stream is returned
//     unchanged; otherwise the merged stream closes when every input has
//     closed or ctx is done.
//
// Each input already honors the [SessionEventProvider] contract (opening
// resync, loss coalesced into resync), so the merged stream does too: every
// input's resync reaches the consumer, which treats it as "poll now".
func SubscribeSessionEventSources(ctx context.Context, stderr io.Writer, sources ...SessionEventSource) (<-chan SessionEvent, error) {
	type implementer struct {
		name string
		sep  SessionEventProvider
	}
	var impls []implementer
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		names = append(names, src.Name)
		if sep, ok := src.Provider.(SessionEventProvider); ok {
			impls = append(impls, implementer{name: src.Name, sep: sep})
		}
	}
	switch len(impls) {
	case 0:
		return nil, fmt.Errorf("no backend (%s) implements SubscribeSessionEvents", strings.Join(names, ", "))
	case 1:
		return impls[0].sep.SubscribeSessionEvents(ctx)
	}

	var streams []<-chan SessionEvent
	var errs []error
	for _, impl := range impls {
		ch, err := impl.sep.SubscribeSessionEvents(ctx)
		if err != nil {
			err = fmt.Errorf("%s backend session-event subscribe: %w", impl.name, err)
			errs = append(errs, err)
			fmt.Fprintf(stderr, "gc: %v (continuing with the other backends' events)\n", err) //nolint:errcheck // best-effort stderr
			continue
		}
		streams = append(streams, ch)
	}
	switch len(streams) {
	case 0:
		return nil, errors.Join(errs...)
	case 1:
		return streams[0], nil
	}
	return mergeSessionEvents(ctx, streams), nil
}

// mergeSessionEvents forwards every input onto one channel, which closes
// once all inputs have closed or ctx is done.
func mergeSessionEvents(ctx context.Context, inputs []<-chan SessionEvent) <-chan SessionEvent {
	out := make(chan SessionEvent)
	var wg sync.WaitGroup
	for _, in := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-in:
					if !ok {
						return
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
