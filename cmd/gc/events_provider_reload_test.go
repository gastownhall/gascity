package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestReloadableEventsProviderSwapRoutesOperationsAndClosesOldWatchers(t *testing.T) {
	initial := &closeTrackingEventsProvider{Provider: events.NewFake()}
	next := &closeTrackingEventsProvider{Provider: events.NewFake()}
	provider := newReloadableEventsProvider(initial)

	provider.Record(events.Event{Type: "before", Actor: "test"})
	initialHead, err := provider.LatestSeq()
	if err != nil {
		t.Fatalf("initial LatestSeq: %v", err)
	}
	watcher, err := provider.Watch(context.Background(), initialHead)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	watchResult := make(chan error, 1)
	go func() {
		_, watchErr := watcher.Next()
		watchResult <- watchErr
	}()

	old, err := provider.swap(next, nil)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if old != initial {
		t.Fatalf("old provider = %T %p, want initial %p", old, old, initial)
	}
	select {
	case watchErr := <-watchResult:
		if watchErr == nil {
			t.Fatal("old-generation watcher returned nil error after swap")
		}
	case <-time.After(time.Second):
		t.Fatal("old-generation watcher was not closed by swap")
	}

	durable, ok := any(provider).(events.DurableRecorder)
	if !ok {
		t.Fatalf("provider type %T does not preserve DurableRecorder", provider)
	}
	if err := durable.RecordDurably(
		events.Event{Type: "after.one", Actor: "test"},
		events.Event{Type: "after.two", Actor: "test"},
	); err != nil {
		t.Fatalf("RecordDurably after swap: %v", err)
	}

	initialEvents, err := initial.List(events.Filter{})
	if err != nil {
		t.Fatalf("initial List: %v", err)
	}
	if len(initialEvents) != 1 || initialEvents[0].Type != "before" {
		t.Fatalf("initial events = %+v, want only before", initialEvents)
	}
	nextEvents, err := next.List(events.Filter{})
	if err != nil {
		t.Fatalf("next List: %v", err)
	}
	if len(nextEvents) != 2 || nextEvents[0].Type != "after.one" || nextEvents[1].Type != "after.two" {
		t.Fatalf("next events = %+v, want ordered durable batch", nextEvents)
	}

	if err := old.Close(); err != nil {
		t.Fatalf("close old provider: %v", err)
	}
	if got := initial.closeCalls.Load(); got != 1 {
		t.Fatalf("old provider close calls = %d, want 1", got)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("close reloadable provider: %v", err)
	}
	if got := next.closeCalls.Load(); got != 1 {
		t.Fatalf("current provider close calls = %d, want 1", got)
	}
}

func TestReloadableEventsProviderRejectedSwapKeepsCurrentProvider(t *testing.T) {
	initial := events.NewFake()
	next := events.NewFake()
	provider := newReloadableEventsProvider(initial)
	wantErr := errors.New("sequence regression")

	old, err := provider.swap(next, func(current, candidate events.Provider) error {
		if current != initial || candidate != next {
			t.Fatalf("validator providers = (%T %p, %T %p), want initial and next", current, current, candidate, candidate)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("swap error = %v, want %v", err, wantErr)
	}
	if old != nil {
		t.Fatalf("old provider = %T, want nil on rejected swap", old)
	}

	provider.Record(events.Event{Type: "still-initial", Actor: "test"})
	initialEvents, _ := initial.List(events.Filter{})
	nextEvents, _ := next.List(events.Filter{})
	if len(initialEvents) != 1 || initialEvents[0].Type != "still-initial" {
		t.Fatalf("initial events = %+v, want post-rejection record", initialEvents)
	}
	if len(nextEvents) != 0 {
		t.Fatalf("next events = %+v, want empty after rejected swap", nextEvents)
	}
}

func TestReloadableEventsProviderRejectsAheadReplacementMissingCurrentHistory(t *testing.T) {
	current := events.NewFake()
	current.Record(events.Event{Type: "current.one", Actor: "test", Ts: time.Unix(1, 0).UTC()})
	next := events.NewFake()
	for i := 0; i < 3; i++ {
		next.Record(events.Event{Type: "replacement.only", Actor: "test", Ts: time.Unix(int64(i+1), 0).UTC()})
	}
	provider := newReloadableEventsProvider(current)

	// This record models an emission that lands after the replacement's
	// construction-time probe but before the swap takes its exclusive lease.
	// A head-only check sees current=2 and replacement=3 and would accept a
	// replacement that does not contain either retained current event.
	provider.Record(events.Event{Type: "current.late", Actor: "test", Ts: time.Unix(2, 0).UTC()})
	if _, err := provider.swap(next, validateReloadableEventsSequence); !errors.Is(err, errReloadableEventsHistoryMismatch) {
		t.Fatalf("swap error = %v, want %v", err, errReloadableEventsHistoryMismatch)
	}

	provider.Record(events.Event{Type: "still.current", Actor: "test"})
	currentEvents, err := current.List(events.Filter{})
	if err != nil {
		t.Fatalf("list current events: %v", err)
	}
	if got := currentEvents[len(currentEvents)-1].Type; got != "still.current" {
		t.Fatalf("active provider after rejected swap recorded %q, want still.current", got)
	}
}

func TestReloadableEventsProviderRejectsSwapWhenCurrentHeadAdvancesDuringValidation(t *testing.T) {
	ev1 := events.Event{Type: "seed.one", Actor: "test", Ts: time.Unix(1, 0).UTC()}
	lateEvent := events.Event{Type: "cross.process.late", Actor: "emit", Ts: time.Unix(2, 0).UTC()}

	currentFake := events.NewFake()
	currentFake.Record(ev1)
	nextFake := events.NewFake()
	nextFake.Record(ev1)

	current := &headAdvancingEventsProvider{Provider: currentFake, inject: func() {
		// Models a separate `gc event emit` / bd hook process appending straight
		// to the file backend after validation snapshots the head but before the
		// swap installs the replacement.
		currentFake.Record(lateEvent)
	}}
	provider := newReloadableEventsProvider(current)

	if _, err := provider.swap(nextFake, validateReloadableEventsSequence); !errors.Is(err, errReloadableEventsHeadAdvanced) {
		t.Fatalf("swap error = %v, want %v", err, errReloadableEventsHeadAdvanced)
	}
	// The rejected swap keeps the current (file) backend, so the late event is
	// not stranded in an abandoned provider.
	if got := provider.ActiveEventProviderName(); got != eventProviderBackendName(current) {
		t.Fatalf("active provider after rejected swap = %q, want current backend", got)
	}

	// A retried swap against a fully caught-up replacement (now carrying the
	// late event too) is accepted.
	nextFake.Record(lateEvent)
	old, err := provider.swap(nextFake, validateReloadableEventsSequence)
	if err != nil {
		t.Fatalf("retried swap with caught-up replacement: %v", err)
	}
	if old != current {
		t.Fatalf("detached provider = %T, want the retained current backend", old)
	}
}

func TestReloadableEventsProviderRejectsNilAndCurrentProviderSwaps(t *testing.T) {
	initial := events.NewFake()
	provider := newReloadableEventsProvider(initial)

	if _, err := provider.swap(nil, nil); !errors.Is(err, errReloadableEventsProviderNil) {
		t.Fatalf("nil swap error = %v, want %v", err, errReloadableEventsProviderNil)
	}
	if _, err := provider.swap(initial, nil); !errors.Is(err, errReloadableEventsProviderSame) {
		t.Fatalf("same-provider swap error = %v, want %v", err, errReloadableEventsProviderSame)
	}
	if _, err := provider.swap(provider, nil); !errors.Is(err, errReloadableEventsProviderSame) {
		t.Fatalf("self swap error = %v, want %v", err, errReloadableEventsProviderSame)
	}
}

func TestReloadableEventsProviderPreservesOptionalReadAndRotationCapabilities(t *testing.T) {
	backing := events.NewFake()
	for i := 0; i < 3; i++ {
		backing.Record(events.Event{Type: "match", Actor: "test"})
	}
	provider := newReloadableEventsProvider(&providerWithoutOptionalCapabilities{Provider: backing})

	tailProvider, ok := any(provider).(events.TailProvider)
	if !ok {
		t.Fatalf("provider type %T does not preserve TailProvider", provider)
	}
	tail, err := tailProvider.ListTail(events.Filter{Type: "match", Limit: 1}, 2)
	if err != nil {
		t.Fatalf("ListTail fallback: %v", err)
	}
	if len(tail) != 2 || tail[0].Seq != 2 || tail[1].Seq != 3 {
		t.Fatalf("ListTail fallback = %+v, want seq 2,3", tail)
	}

	inFlightProvider, ok := any(provider).(events.InFlightProvider)
	if !ok {
		t.Fatalf("provider type %T does not preserve InFlightProvider", provider)
	}
	inFlight, err := inFlightProvider.ListInFlight(events.Filter{Type: "match"})
	if err != nil {
		t.Fatalf("ListInFlight fallback: %v", err)
	}
	if len(inFlight) != 3 {
		t.Fatalf("ListInFlight fallback returned %d events, want 3", len(inFlight))
	}

	rotating := &rotatingEventsProvider{Provider: events.NewFake()}
	rotationProvider := newReloadableEventsProvider(rotating)
	rotator, ok := any(rotationProvider).(events.RotatingProvider)
	if !ok {
		t.Fatalf("provider type %T does not preserve RotatingProvider", rotationProvider)
	}
	result, err := rotator.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !result.Rotated || rotating.rotateCalls.Load() != 1 {
		t.Fatalf("rotation result = %+v, calls = %d; want one delegated rotation", result, rotating.rotateCalls.Load())
	}
	rotateErr := errors.New("rotate failed")
	rotating.rotateErr = rotateErr
	if _, err := rotator.ForceRotate(); !errors.Is(err, rotateErr) {
		t.Fatalf("ForceRotate error = %v, want %v", err, rotateErr)
	}

	unsupported := newReloadableEventsProvider(&providerWithoutOptionalCapabilities{Provider: events.NewFake()})
	if _, err := unsupported.ForceRotate(); !errors.Is(err, events.ErrRotationUnsupported) {
		t.Fatalf("unsupported ForceRotate error = %v, want %v", err, events.ErrRotationUnsupported)
	}
}

// headAdvancingEventsProvider delegates to an underlying provider but, on its
// first successful LatestSeq call, invokes inject once to simulate a
// cross-process writer appending to the same backend mid-validation.
type headAdvancingEventsProvider struct {
	events.Provider
	inject   func()
	injected atomic.Bool
}

func (p *headAdvancingEventsProvider) LatestSeq() (uint64, error) {
	seq, err := p.Provider.LatestSeq()
	if err == nil && p.injected.CompareAndSwap(false, true) && p.inject != nil {
		p.inject()
	}
	return seq, err
}

type closeTrackingEventsProvider struct {
	events.Provider
	closeCalls atomic.Int32
}

func (p *closeTrackingEventsProvider) Close() error {
	p.closeCalls.Add(1)
	return p.Provider.Close()
}

func (p *closeTrackingEventsProvider) RecordDurably(batch ...events.Event) error {
	durable, ok := p.Provider.(events.DurableRecorder)
	if !ok {
		return errors.New("test provider is not durable")
	}
	return durable.RecordDurably(batch...)
}

type providerWithoutOptionalCapabilities struct {
	events.Provider
}

type rotatingEventsProvider struct {
	events.Provider
	rotateCalls atomic.Int32
	rotateErr   error
}

func (p *rotatingEventsProvider) ForceRotate() (events.RotationResult, error) {
	p.rotateCalls.Add(1)
	return events.RotationResult{Rotated: true}, p.rotateErr
}
