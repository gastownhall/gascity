package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
)

// dynamicCityResolver is a CityResolver whose set of running cities changes
// while a supervisor event stream is open. It implements CityChangeNotifier
// the way the supervisor's city registry does: the channel returned by
// CityChanges is closed at the next change.
type dynamicCityResolver struct {
	mu      sync.Mutex
	cities  map[string]*fakeState
	changed chan struct{}
	notify  bool
}

func newDynamicCityResolver(cities map[string]*fakeState) *dynamicCityResolver {
	return &dynamicCityResolver{cities: cities, changed: make(chan struct{}), notify: true}
}

func (r *dynamicCityResolver) ListCities() []CityInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CityInfo, 0, len(r.cities))
	for name, s := range r.cities {
		out = append(out, CityInfo{Name: name, Path: s.CityPath(), Running: true})
	}
	return out
}

func (r *dynamicCityResolver) CityState(name string) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.cities[name]; ok {
		return s
	}
	return nil
}

func (r *dynamicCityResolver) CityChanges() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// addCity registers s under its city name and signals the change.
func (r *dynamicCityResolver) addCity(s *fakeState) {
	r.update(func(cities map[string]*fakeState) { cities[s.cityName] = s })
}

// removeCity drops s's city and signals the change.
func (r *dynamicCityResolver) removeCity(s *fakeState) {
	r.update(func(cities map[string]*fakeState) { delete(cities, s.cityName) })
}

func (r *dynamicCityResolver) update(mutate func(map[string]*fakeState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mutate(r.cities)
	if r.notify {
		close(r.changed)
		r.changed = make(chan struct{})
	}
}

// pollingOnlyResolver hides CityChanges so the stream must fall back to its
// periodic resync.
type pollingOnlyResolver struct{ r *dynamicCityResolver }

func (p pollingOnlyResolver) ListCities() []CityInfo      { return p.r.ListCities() }
func (p pollingOnlyResolver) CityState(name string) State { return p.r.CityState(name) }

// watchSignalProvider wraps an events.Fake and reports when a watcher is
// attached and when it is closed, so stream tests wait for facts rather than
// sleeping.
type watchSignalProvider struct {
	*events.Fake
	watched chan struct{} // closed on the first Watch
	closed  chan struct{} // closed when the first watcher is closed
	once    sync.Once
}

func newWatchSignalProvider() *watchSignalProvider {
	return &watchSignalProvider{Fake: events.NewFake(), watched: make(chan struct{}), closed: make(chan struct{})}
}

func (p *watchSignalProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	inner, err := p.Fake.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	first := false
	p.once.Do(func() { first = true })
	if !first {
		return inner, nil
	}
	defer close(p.watched)
	return &closeSignalWatcher{Watcher: inner, closed: p.closed}, nil
}

type closeSignalWatcher struct {
	events.Watcher
	once   sync.Once
	closed chan struct{}
}

func (w *closeSignalWatcher) Close() error {
	err := w.Watcher.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

func newNamedFakeState(t *testing.T, name string, prov events.Provider) *fakeState {
	t.Helper()
	s := newFakeState(t)
	s.cityName = name
	s.eventProv = prov
	return s
}

// pipeResponseWriter is an http.ResponseWriter whose body is the write end of
// an io.Pipe, so a streaming handler runs in-process and a test reads each SSE
// frame as the handler writes it, with no loopback listener. As with net/http,
// the response is committed by the first Write or Flush; committed is closed
// at that moment.
type pipeResponseWriter struct {
	header    http.Header
	body      *io.PipeWriter
	committed chan struct{}
	once      sync.Once

	mu     sync.Mutex
	status int
}

func newPipeResponseWriter(body *io.PipeWriter) *pipeResponseWriter {
	return &pipeResponseWriter{header: make(http.Header), body: body, committed: make(chan struct{})}
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.commit()
	return w.body.Write(p)
}

func (w *pipeResponseWriter) Flush() { w.commit() }

func (w *pipeResponseWriter) commit() {
	w.WriteHeader(http.StatusOK)
	w.once.Do(func() { close(w.committed) })
}

func (w *pipeResponseWriter) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// sseStream is an SSE response a handler is serving in-process. Frames are
// parsed as the handler writes them.
type sseStream struct {
	frames <-chan sseTestFrame
	stop   func()
}

// openSSEStream serves GET path on h in-process and returns once the handler
// has committed a 200 response. stop cancels the request and returns only after
// the handler has returned, so everything the handler defers (watcher closes,
// pending-monitor leases) has run by then. It is also registered as cleanup.
func openSSEStream(t *testing.T, h http.Handler, path, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	pr, pw := io.Pipe()
	w := newPipeResponseWriter(pw)
	served := make(chan struct{})
	go func() {
		defer close(served)
		h.ServeHTTP(w, req)
		pw.Close() //nolint:errcheck // reports EOF to the reader
	}()

	frames := make(chan sseTestFrame, 64)
	go func() {
		defer close(frames)
		// Closing the read end unblocks a handler write the test no longer
		// consumes, so the handler can always return.
		defer pr.Close() //nolint:errcheck
		var current sseTestFrame
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if current.Event != "" || current.ID != "" || current.Data != "" {
					select {
					case frames <- current:
					case <-ctx.Done():
						return
					}
				}
				current = sseTestFrame{}
			case strings.HasPrefix(line, "event: "):
				current.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				current.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				current.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-served:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Errorf("GET %s: handler did not return after the request was canceled", path)
			}
		})
	}
	t.Cleanup(stop)

	select {
	case <-w.committed:
	case <-served:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("GET %s: handler committed no response", path)
	}
	if code := w.statusCode(); code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, code)
	}
	return &sseStream{frames: frames, stop: stop}
}

// liveGlobalStream is an open GET /v0/events/stream served in-process.
type liveGlobalStream struct {
	frames <-chan sseTestFrame
}

func openLiveGlobalStream(t *testing.T, h http.Handler, query, lastEventID string) *liveGlobalStream {
	t.Helper()
	stream := openSSEStream(t, h, "/v0/events/stream"+query, lastEventID)
	return &liveGlobalStream{frames: stream.frames}
}

// nextTaggedEvent returns the next tagged_event frame, skipping heartbeats.
func (s *liveGlobalStream) nextTaggedEvent(t *testing.T) (sseTestFrame, map[string]any) {
	t.Helper()
	deadline := time.After(testutil.GoroutineRaceTimeout)
	for {
		select {
		case frame, ok := <-s.frames:
			if !ok {
				t.Fatal("event stream ended before the expected tagged_event")
			}
			if frame.Event != "tagged_event" {
				continue
			}
			return frame, decodeSSETestData(t, frame)
		case <-deadline:
			t.Fatal("no tagged_event arrived on the supervisor event stream")
			return sseTestFrame{}, nil
		}
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertTaggedEvent(t *testing.T, frame sseTestFrame, data map[string]any, city, subject, id string) {
	t.Helper()
	if data["city"] != city || data["subject"] != subject {
		t.Fatalf("tagged_event city=%v subject=%v, want city=%s subject=%s; data=%s", data["city"], data["subject"], city, subject, frame.Data)
	}
	if frame.ID != id {
		t.Fatalf("SSE id = %q, want %q", frame.ID, id)
	}
}

// Regression for #6861: a client connected to the supervisor-scope stream
// before a city started must receive that city's events once it starts,
// tagged with the city, starting from "now" — and the composite SSE id must
// carry the new city's attach cursor so a reconnect resumes it without a gap.
func TestSupervisorGlobalEventStreamAttachesCityStartedMidStream(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	betaProv := newWatchSignalProvider()
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-old-1"})
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-old-2"})
	resolver.addCity(newNamedFakeState(t, "beta", betaProv))
	waitSignal(t, betaProv.watched, "the stream to attach city beta")

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1,beta:2")

	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-new"})
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-new", "alpha:1,beta:3")
}

// A resume cursor that names a city which was not running at connect time
// resumes that city from the cursor when it starts mid-stream, so events it
// produced while the client was away are replayed rather than skipped.
func TestSupervisorGlobalEventStreamResumesMidStreamCityFromLastEventID(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "alpha:0,beta:1")

	betaProv := newWatchSignalProvider()
	for _, subject := range []string{"beta-1", "beta-2", "beta-3"} {
		betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: subject})
	}
	resolver.addCity(newNamedFakeState(t, "beta", betaProv))

	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-2", "alpha:0,beta:2")
	frame, data = stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-3", "alpha:0,beta:3")
}

// after_cursor=0 asks for replay from zero for every provider; a city that
// starts mid-stream is replayed from zero too, which is what lets an async
// city.create caller that captured cursor "0" see its city's early events.
func TestSupervisorGlobalEventStreamZeroCursorReplaysMidStreamCity(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "?after_cursor=0", "")

	betaProv := newWatchSignalProvider()
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	resolver.addCity(newNamedFakeState(t, "beta", betaProv))

	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "alpha:0,beta:1")
}

// A city that stops (leaves the provider set) mid-stream is detached: its
// watcher is closed, and the stream keeps delivering the remaining cities.
func TestSupervisorGlobalEventStreamDetachesCityStoppedMidStream(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	betaProv := newWatchSignalProvider()
	beta := newNamedFakeState(t, "beta", betaProv)
	resolver := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	resolver.addCity(beta)
	waitSignal(t, betaProv.watched, "the stream to attach city beta")
	resolver.removeCity(beta)
	waitSignal(t, betaProv.closed, "the stream to close city beta's watcher")

	alpha.eventProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "alpha-1"})
	frame, data := stream.nextTaggedEvent(t)
	// The departed city is pruned from the composite id.
	assertTaggedEvent(t, frame, data, "alpha", "alpha-1", "alpha:1")
}

// A resolver without a change notifier still has new cities picked up by the
// stream's periodic resync.
func TestSupervisorGlobalEventStreamResyncsWithoutChangeNotifier(t *testing.T) {
	alpha := newNamedFakeState(t, "alpha", events.NewFake())
	dynamic := newDynamicCityResolver(map[string]*fakeState{"alpha": alpha})
	dynamic.notify = false
	sm := NewSupervisorMux(pollingOnlyResolver{r: dynamic}, nil, false, "test", "", time.Now())
	sm.eventStreamResync = 10 * time.Millisecond // the resync period is the subject under test
	stream := openLiveGlobalStream(t, sm, "", "")

	betaProv := newWatchSignalProvider()
	dynamic.addCity(newNamedFakeState(t, "beta", betaProv))
	waitSignal(t, betaProv.watched, "the periodic resync to attach city beta")

	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "alpha:0,beta:1")
}

// startOnStreamAttachProvider runs start when the stream itself attaches to it.
// The supervisor stream attaches twice: its precheck probes every provider and
// closes the probe, then the stream proper attaches. The second Watch is
// therefore the moment the stream has read the city set but has not yet begun
// serving events.
type startOnStreamAttachProvider struct {
	*events.Fake
	mu      sync.Mutex
	watches int
	start   func()
}

func (p *startOnStreamAttachProvider) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) {
	p.mu.Lock()
	p.watches++
	streamAttach := p.watches == 2
	p.mu.Unlock()
	if streamAttach {
		p.start()
	}
	return p.Fake.Watch(ctx, afterSeq)
}

// A city that starts while the stream is still connecting — after it read the
// city set, before it began waiting for changes — must be attached by the
// change signal, not left for the slow periodic resync.
func TestSupervisorGlobalEventStreamAttachesCityStartedWhileConnecting(t *testing.T) {
	resolver := newDynamicCityResolver(map[string]*fakeState{})
	betaProv := newWatchSignalProvider()
	beta := newNamedFakeState(t, "beta", betaProv)
	alphaProv := &startOnStreamAttachProvider{Fake: events.NewFake(), start: func() { resolver.addCity(beta) }}
	resolver.addCity(newNamedFakeState(t, "alpha", alphaProv))
	sm := NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
	stream := openLiveGlobalStream(t, sm, "", "")

	waitSignal(t, betaProv.watched, "the change signal to attach city beta")
	betaProv.Record(events.Event{Type: events.SessionWoke, Actor: "tester", Subject: "beta-1"})
	frame, data := stream.nextTaggedEvent(t)
	assertTaggedEvent(t, frame, data, "beta", "beta-1", "alpha:0,beta:1")
}
