package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
)

var (
	errReloadableEventsProviderClosed          = errors.New("reloadable event provider is closed")
	errReloadableEventsProviderNil             = errors.New("reloadable event provider cannot swap to nil")
	errReloadableEventsProviderSame            = errors.New("reloadable event provider cannot swap to its current provider")
	errReloadableEventsProviderNotDurable      = errors.New("current event provider does not support durable recording")
	errReloadableEventsProviderMissingDelegate = errors.New("reloadable event provider has no delegate")
	errReloadableEventsSequenceRegression      = errors.New("event provider sequence would regress")
	errReloadableEventsHistoryMismatch         = errors.New("replacement event provider is missing equivalent retained history")
	errReloadableEventsHeadAdvanced            = errors.New("current event provider head advanced during validation (cross-process writer)")
)

type effectiveEventsConfig struct {
	provider string
	rotation eventsRotationSettings
}

// effectiveEventsProviderConfig resolves the semantic backend identity used
// for hot-reload comparison. GC_EVENTS has the same precedence as provider
// construction. Rotation only contributes to file-backed identity because it
// has no effect on fake, fail, or exec providers.
func effectiveEventsProviderConfig(cfg config.EventsConfig) effectiveEventsConfig {
	provider := strings.TrimSpace(cfg.Provider)
	if override := strings.TrimSpace(os.Getenv("GC_EVENTS")); override != "" {
		provider = override
	}

	switch {
	case strings.HasPrefix(provider, "exec:"), provider == "fake", provider == "fail":
		return effectiveEventsConfig{provider: provider}
	default:
		return effectiveEventsConfig{
			provider: "file",
			rotation: eventsRotationSettingsFromConfig(cfg, io.Discard),
		}
	}
}

// reloadableEventsProvider gives every long-lived city component one stable
// provider identity while allowing config reload to replace its backend. Each
// synchronous operation holds a read lease through the delegated call; swap
// takes the exclusive lease, so an operation (especially a durable batch) can
// never straddle two backends.
type reloadableEventsProvider struct {
	mu           sync.RWMutex
	current      events.Provider
	providerName string
	generation   uint64
	closed       bool

	watchMu  sync.Mutex
	watchers map[*reloadableEventsWatcher]uint64
}

func newReloadableEventsProvider(provider events.Provider) *reloadableEventsProvider {
	return &reloadableEventsProvider{
		current:      provider,
		providerName: eventProviderBackendName(provider),
		watchers:     make(map[*reloadableEventsWatcher]uint64),
	}
}

// swap replaces the delegate after validate succeeds under the exclusive
// operation lease. It returns the detached provider to the caller, which owns
// closing it after reload has resumed its workers. Watchers created against the
// detached generation are closed so stream clients reconnect with their cursor
// instead of remaining silently attached to the obsolete backend.
func (p *reloadableEventsProvider) swap(
	next events.Provider,
	validate func(current, next events.Provider) error,
) (events.Provider, error) {
	return p.swapNamed(next, eventProviderBackendName(next), validate)
}

func (p *reloadableEventsProvider) swapNamed(
	next events.Provider,
	providerName string,
	validate func(current, next events.Provider) error,
) (events.Provider, error) {
	if next == nil {
		return nil, errReloadableEventsProviderNil
	}
	if next == p {
		return nil, errReloadableEventsProviderSame
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errReloadableEventsProviderClosed
	}
	if sameEventsProvider(p.current, next) {
		p.mu.Unlock()
		return nil, errReloadableEventsProviderSame
	}
	if validate != nil {
		if err := validate(p.current, next); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}

	old := p.current
	oldGeneration := p.generation
	p.current = next
	p.providerName = providerName
	p.generation++
	staleWatchers := p.detachWatchers(oldGeneration)
	p.mu.Unlock()

	for _, watcher := range staleWatchers {
		_ = watcher.Close()
	}
	return old, nil
}

func eventProviderBackendName(provider events.Provider) string {
	switch provider.(type) {
	case nil:
		return "none"
	case *events.FileRecorder:
		return "file"
	case *events.Fake:
		return "fake"
	case *eventsexec.Provider:
		return "exec"
	default:
		return fmt.Sprintf("%T", provider)
	}
}

func (p *reloadableEventsProvider) ActiveEventProviderName() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.providerName == "" {
		return eventProviderBackendName(p.current)
	}
	return p.providerName
}

func (p *reloadableEventsProvider) setActiveProviderName(providerName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.providerName = providerName
}

func validateReloadableEventsSequence(current, next events.Provider) error {
	if current == nil || next == nil {
		return errReloadableEventsProviderMissingDelegate
	}
	currentHead, err := current.LatestSeq()
	if err != nil {
		return fmt.Errorf("read current event provider head: %w", err)
	}
	nextHead, err := next.LatestSeq()
	if err != nil {
		return fmt.Errorf("read replacement event provider head: %w", err)
	}
	if nextHead < currentHead {
		return fmt.Errorf("%w: replacement head %d is below current head %d; copy equivalent event history before retrying",
			errReloadableEventsSequenceRegression, nextHead, currentHead)
	}

	// A non-regressing head is necessary but not sufficient. An independently
	// populated replacement can be ahead while omitting an event that reached
	// the current provider after candidate construction. The wrapper's exclusive
	// swap lease fences every in-process writer while these snapshots are read;
	// prove that each event retained by the current provider exists unchanged at
	// the same sequence in the replacement before detaching it.
	//
	// The lease does NOT fence cross-process writers: `gc event emit` and bd
	// hooks append straight to .gc/events.jsonl through their own FileRecorder
	// (openCityEventEmitProvider), invisible to this in-process mutex. Such a
	// writer is caught below by a final head re-check, not by this history proof.
	filter := events.Filter{}
	if currentHead < ^uint64(0) {
		filter.BeforeSeq = currentHead + 1
	}
	currentHistory, err := listReloadableEventsHistory(current, filter)
	if err != nil {
		return fmt.Errorf("read current retained event history: %w", err)
	}
	nextHistory, err := listReloadableEventsHistory(next, filter)
	if err != nil {
		return fmt.Errorf("read replacement retained event history: %w", err)
	}

	nextBySeq := make(map[uint64]events.Event, len(nextHistory))
	for _, event := range nextHistory {
		if _, duplicate := nextBySeq[event.Seq]; duplicate {
			return fmt.Errorf("%w: replacement contains duplicate sequence %d", errReloadableEventsHistoryMismatch, event.Seq)
		}
		nextBySeq[event.Seq] = event
	}
	currentSeqs := make(map[uint64]struct{}, len(currentHistory))
	for _, retained := range currentHistory {
		if _, duplicate := currentSeqs[retained.Seq]; duplicate {
			return fmt.Errorf("%w: current provider contains duplicate sequence %d", errReloadableEventsHistoryMismatch, retained.Seq)
		}
		currentSeqs[retained.Seq] = struct{}{}
		replacement, ok := nextBySeq[retained.Seq]
		if !ok {
			return fmt.Errorf("%w: replacement head %d omits current sequence %d",
				errReloadableEventsHistoryMismatch, nextHead, retained.Seq)
		}
		if !sameReloadableEvent(retained, replacement) {
			return fmt.Errorf("%w: sequence %d differs (current type %q, replacement type %q)",
				errReloadableEventsHistoryMismatch, retained.Seq, retained.Type, replacement.Type)
		}
	}

	// Final fence against cross-process writers. Re-read the current head one
	// last time: if it advanced past the head this validation proved equivalent,
	// a separate process (gc event emit / bd hook) committed an event that only
	// exists in the backend we are about to abandon. Reject the swap with the
	// same retry semantics as a sequence regression — the installed-config
	// tracker leaves installedEventsConfigResolved unset on any swap error and
	// retries the reload — so the late event is never silently stranded. This
	// narrows the window to the interval between this read and the caller's
	// pointer swap (both under the lease, no I/O), rather than eliminating it.
	finalHead, err := current.LatestSeq()
	if err != nil {
		return fmt.Errorf("re-read current event provider head: %w", err)
	}
	if finalHead != currentHead {
		return fmt.Errorf("%w: head advanced from %d to %d; copy the late event into the replacement before retrying",
			errReloadableEventsHeadAdvanced, currentHead, finalHead)
	}
	return nil
}

func listReloadableEventsHistory(provider events.Provider, filter events.Filter) ([]events.Event, error) {
	if inFlight, ok := provider.(events.InFlightProvider); ok {
		return inFlight.ListInFlight(filter)
	}
	return provider.List(filter)
}

func sameReloadableEvent(left, right events.Event) bool {
	return left.Seq == right.Seq &&
		left.Type == right.Type &&
		left.Ts.Equal(right.Ts) &&
		left.Actor == right.Actor &&
		left.Subject == right.Subject &&
		left.Message == right.Message &&
		bytes.Equal(left.Payload, right.Payload) &&
		left.RunID == right.RunID &&
		left.SessionID == right.SessionID &&
		left.StepID == right.StepID
}

func sameEventsProvider(left, right events.Provider) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() || !leftValue.Type().Comparable() {
		return false
	}
	return leftValue.Interface() == rightValue.Interface()
}

func (p *reloadableEventsProvider) detachWatchers(generation uint64) []*reloadableEventsWatcher {
	p.watchMu.Lock()
	defer p.watchMu.Unlock()
	watchers := make([]*reloadableEventsWatcher, 0)
	for watcher, watcherGeneration := range p.watchers {
		if watcherGeneration != generation {
			continue
		}
		delete(p.watchers, watcher)
		watchers = append(watchers, watcher)
	}
	return watchers
}

func (p *reloadableEventsProvider) unregisterWatcher(watcher *reloadableEventsWatcher) {
	p.watchMu.Lock()
	delete(p.watchers, watcher)
	p.watchMu.Unlock()
}

func (p *reloadableEventsProvider) Record(event events.Event) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed || p.current == nil {
		return
	}
	p.current.Record(event)
}

func (p *reloadableEventsProvider) RecordDurably(batch ...events.Event) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return errReloadableEventsProviderMissingDelegate
	}
	durable, ok := p.current.(events.DurableRecorder)
	if !ok {
		return errReloadableEventsProviderNotDurable
	}
	return durable.RecordDurably(batch...)
}

func (p *reloadableEventsProvider) List(filter events.Filter) ([]events.Event, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return nil, errReloadableEventsProviderMissingDelegate
	}
	return p.current.List(filter)
}

func (p *reloadableEventsProvider) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return nil, errReloadableEventsProviderMissingDelegate
	}
	if tail, ok := p.current.(events.TailProvider); ok {
		return tail.ListTail(filter, limit)
	}
	filter.Limit = 0
	listed, err := p.current.List(filter)
	if err != nil || limit <= 0 || len(listed) <= limit {
		return listed, err
	}
	result := make([]events.Event, limit)
	copy(result, listed[len(listed)-limit:])
	return result, nil
}

func (p *reloadableEventsProvider) ListInFlight(filter events.Filter) ([]events.Event, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return nil, errReloadableEventsProviderMissingDelegate
	}
	if inFlight, ok := p.current.(events.InFlightProvider); ok {
		return inFlight.ListInFlight(filter)
	}
	return p.current.List(filter)
}

func (p *reloadableEventsProvider) LatestSeq() (uint64, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return 0, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return 0, errReloadableEventsProviderMissingDelegate
	}
	return p.current.LatestSeq()
}

func (p *reloadableEventsProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return nil, errReloadableEventsProviderMissingDelegate
	}
	watcher, err := p.current.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	tracked := &reloadableEventsWatcher{owner: p, current: watcher}
	p.watchMu.Lock()
	p.watchers[tracked] = p.generation
	p.watchMu.Unlock()
	return tracked, nil
}

func (p *reloadableEventsProvider) ForceRotate() (events.RotationResult, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return events.RotationResult{}, errReloadableEventsProviderClosed
	}
	if p.current == nil {
		return events.RotationResult{}, errReloadableEventsProviderMissingDelegate
	}
	rotator, ok := p.current.(events.RotatingProvider)
	if !ok {
		return events.RotationResult{}, events.ErrRotationUnsupported
	}
	return rotator.ForceRotate()
}

func (p *reloadableEventsProvider) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	current := p.current
	p.current = nil
	watchers := p.detachWatchers(p.generation)
	p.mu.Unlock()

	for _, watcher := range watchers {
		_ = watcher.Close()
	}
	if current == nil {
		return nil
	}
	return current.Close()
}

type reloadableEventsWatcher struct {
	owner   *reloadableEventsProvider
	current events.Watcher
	once    sync.Once
	err     error
}

func (w *reloadableEventsWatcher) Next() (events.Event, error) {
	return w.current.Next()
}

func (w *reloadableEventsWatcher) Close() error {
	w.once.Do(func() {
		if w.owner != nil {
			w.owner.unregisterWatcher(w)
		}
		if w.current != nil {
			w.err = w.current.Close()
		}
	})
	return w.err
}
