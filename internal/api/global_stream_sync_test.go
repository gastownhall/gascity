package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// The supervisor stream's membership sync runs off its select loop, keeps the
// stream open when every city detaches, and prunes departed cities from the
// composite id while resuming them if they return. Every test waits for facts
// (watcher attached/closed, frames delivered), never for wall-clock time.

// latestCloseProvider wraps an events.Fake and exposes, for each watcher it
// hands out, a channel closed when that watcher is closed. The supervisor
// stream attaches twice (precheck probe, then the stream), so tests wait on
// the most recent watcher's close.
type latestCloseProvider struct {
	*events.Fake
	mu      sync.Mutex
	watched chan struct{} // closed and replaced on every Watch
	closed  chan struct{} // the latest watcher's close signal
}

func newLatestCloseProvider() *latestCloseProvider {
	return &latestCloseProvider{Fake: events.NewFake(), watched: make(chan struct{})}
}

func (p *latestCloseProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	inner, err := p.Fake.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	p.mu.Lock()
	p.closed = closed
	close(p.watched)
	p.watched = make(chan struct{})
	p.mu.Unlock()
	return &closeSignalWatcher{Watcher: inner, closed: closed}, nil
}

// lastClosed returns the latest watcher's close signal.
func (p *latestCloseProvider) lastClosed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// nextWatch returns a channel closed by the next Watch call.
func (p *latestCloseProvider) nextWatch() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watched
}

// gatedAttachProvider is a latestCloseProvider whose next Watch, once armed,
// signals entered and then blocks until released, standing in for a city whose
// watcher is slow to attach. The stream keeps delivering the other cities
// while it waits.
type gatedAttachProvider struct {
	*latestCloseProvider
	gateMu  sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

// arm gates the next Watch. It returns the entered signal and the release
// function for that Watch; release is idempotent.
func (p *gatedAttachProvider) arm() (<-chan struct{}, func()) {
	gate, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.gateMu.Lock()
	p.gate, p.entered = gate, entered
	p.gateMu.Unlock()
	return entered, func() { once.Do(func() { close(gate) }) }
}

func (p *gatedAttachProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.gateMu.Lock()
	gate, entered := p.gate, p.entered
	p.gate, p.entered = nil, nil
	p.gateMu.Unlock()
	if gate != nil {
		close(entered)
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.latestCloseProvider.Watch(ctx, afterSeq)
}

// gatedListResolver blocks ListCities while its gate is armed, standing in for
// a city provider listing that hangs (a stuck city registry or controller).
type gatedListResolver struct {
	*dynamicCityResolver
	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (r *gatedListResolver) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = make(chan struct{})
	r.entered = make(chan struct{})
	r.once = sync.Once{}
}

func (r *gatedListResolver) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		close(r.gate)
		r.gate = nil
	}
}

func (r *gatedListResolver) ListCities() []CityInfo {
	r.mu.Lock()
	gate, entered := r.gate, r.entered
	r.mu.Unlock()
	if gate != nil {
		r.once.Do(func() { close(entered) })
		<-gate
	}
	return r.dynamicCityResolver.ListCities()
}

// A membership sync whose city listing hangs must not stall delivery from the
// cities already attached: the sync runs off the stream's select loop.
func TestSupervisorGlobalEventStreamDeliversWhileCitySyncHangs(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	resolver := &gatedListResolver{dynamicCityResolver: newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})}
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")
	resolver.arm()
	t.Cleanup(resolver.release) // runs before the stream's own cleanup stops it

	resolver.addCity(newNamedFakeState(t, "beta", events.NewFake()))
	waitSignal(t, resolver.entered, "the membership sync to list cities")

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1")
}

// Every city leaving must not end the stream: it stays open (heartbeats keep
// flowing) and a city that starts afterwards is attached and delivered.
func TestSupervisorGlobalEventStreamSurvivesEveryCityDetaching(t *testing.T) {
	alphaProv := newLatestCloseProvider()
	alpha := newNamedFakeState(t, "alpha", alphaProv)
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	resolver.removeCity(alpha)
	waitSignal(t, alphaProv.lastClosed(), "the stream to close city alpha's watcher")

	betaProv := newWatchSignalProvider()
	resolver.addCity(newNamedFakeState(t, "beta", betaProv))
	waitSignal(t, betaProv.watched, "the stream to attach city beta")
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "beta:1")
}

// A city removed mid-stream and re-added resumes from the last seq the stream
// delivered for it: what it wrote while away arrives, nothing twice, and the
// composite id drops the city while it is gone.
func TestSupervisorGlobalEventStreamRemoveAndReaddResumesWithoutGap(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	betaProv := newLatestCloseProvider()
	beta := newNamedFakeState(t, "beta", betaProv)
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha, "beta": beta})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "alpha:0,beta:1")

	resolver.removeCity(beta)
	waitSignal(t, betaProv.lastClosed(), "the stream to close city beta's watcher")
	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1")

	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-2"})
	reattached := betaProv.nextWatch()
	resolver.addCity(beta)
	waitSignal(t, reattached, "the stream to re-attach city beta")
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-3"})
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-2", "alpha:1,beta:2")
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-3", "alpha:1,beta:3")
}

// A city that returns rejoins the composite id at its resume position before
// its watcher can deliver: a frame sent while the returning city's watcher is
// still attaching already names it, so no frame the stream sends once it
// re-attaches the city carries an id a reconnect would resume without it.
func TestSupervisorGlobalEventStreamReturningCityRejoinsIDBeforeItsWatcherDelivers(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	betaProv := &gatedAttachProvider{latestCloseProvider: newLatestCloseProvider()}
	beta := newNamedFakeState(t, "beta", betaProv)
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha, "beta": beta})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "alpha:0,beta:1")

	resolver.removeCity(beta)
	waitSignal(t, betaProv.lastClosed(), "the stream to close city beta's watcher")
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-2"})

	entered, release := betaProv.arm()
	t.Cleanup(release) // runs before the stream's own cleanup stops it
	resolver.addCity(beta)
	waitSignal(t, entered, "the stream to start re-attaching city beta")
	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1,beta:1")

	release()
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-2", "alpha:1,beta:2")
}
