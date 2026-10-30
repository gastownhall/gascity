package events

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestMultiplexerListAll(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1", Ts: time.Unix(1, 0)})
	f1.Record(Event{Type: SessionStopped, Actor: "a1", Ts: time.Unix(3, 0)})

	f2 := NewFake()
	f2.Record(Event{Type: SessionWoke, Actor: "b1", Ts: time.Unix(2, 0)})

	m.Add("city-a", f1)
	m.Add("city-b", f2)

	evts, err := m.ListAll(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3", len(evts))
	}
	// Should be sorted by timestamp.
	if evts[0].City != "city-a" || evts[1].City != "city-b" || evts[2].City != "city-a" {
		t.Errorf("unexpected city ordering: %v, %v, %v", evts[0].City, evts[1].City, evts[2].City)
	}
}

func TestMultiplexerListAllWithFilter(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1"})
	f1.Record(Event{Type: SessionStopped, Actor: "a1"})

	m.Add("city-a", f1)

	evts, err := m.ListAll(Filter{Type: SessionWoke})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != SessionWoke {
		t.Errorf("got type %q, want %q", evts[0].Type, SessionWoke)
	}
}

func TestMultiplexerListAllAppliesGlobalLimitAfterMerge(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "first", Ts: time.Unix(1, 0)})
	f1.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "fourth", Ts: time.Unix(4, 0)})

	f2 := NewFake()
	f2.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "second", Ts: time.Unix(2, 0)})
	f2.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "third", Ts: time.Unix(3, 0)})

	m.Add("city-a", f1)
	m.Add("city-b", f2)

	evts, err := m.ListAll(Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].Subject != "first" {
		t.Errorf("evts[0].Subject = %q, want first", evts[0].Subject)
	}
	if evts[1].Subject != "second" {
		t.Errorf("evts[1].Subject = %q, want second", evts[1].Subject)
	}
}

func TestMultiplexerListAllOrdersEqualTimestampsDeterministically(t *testing.T) {
	m := NewMultiplexer()
	ts := time.Unix(1, 0)

	alpha := NewFake()
	alpha.Events = []Event{
		{Seq: 5, Type: SessionWoke, Subject: "alpha", Ts: ts},
	}

	beta := NewFake()
	beta.Events = []Event{
		{Seq: 2, Type: SessionWoke, Subject: "beta-two", Ts: ts},
		{Seq: 1, Type: SessionWoke, Subject: "beta-one", Ts: ts},
	}

	m.Add("beta", beta)
	m.Add("alpha", alpha)

	evts, err := m.ListAll(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3", len(evts))
	}
	got := []string{
		evts[0].City + ":" + evts[0].Subject,
		evts[1].City + ":" + evts[1].Subject,
		evts[2].City + ":" + evts[2].Subject,
	}
	want := []string{"alpha:alpha", "beta:beta-one", "beta:beta-two"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func TestMultiplexerListAllDoesNotWaitForSlowProvider(t *testing.T) {
	m := NewMultiplexer()
	m.providerTimeout = 20 * time.Millisecond

	healthy := NewFake()
	healthy.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "healthy", Ts: time.Unix(1, 0)})
	slow := newBlockingProvider()
	defer slow.release()

	m.Add("healthy", healthy)
	m.Add("slow", slow)

	result := make(chan struct {
		evts []TaggedEvent
		err  error
	}, 1)
	go func() {
		evts, err := m.ListAll(Filter{})
		result <- struct {
			evts []TaggedEvent
			err  error
		}{evts: evts, err: err}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("ListAll() error = %v", got.err)
		}
		if len(got.evts) != 1 || got.evts[0].City != "healthy" || got.evts[0].Subject != "healthy" {
			t.Fatalf("ListAll() = %+v, want only healthy provider event", got.evts)
		}
	case <-time.After(200 * time.Millisecond):
		slow.release()
		t.Fatal("ListAll() waited for the slow provider")
	}
}

func TestMultiplexerListTailLimitsAcrossCities(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "old-a", Ts: time.Unix(1, 0)})
	f1.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "new-a", Ts: time.Unix(3, 0)})

	f2 := NewFake()
	f2.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "old-b", Ts: time.Unix(2, 0)})
	f2.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "new-b", Ts: time.Unix(4, 0)})

	m.Add("city-a", f1)
	m.Add("city-b", f2)

	evts, err := m.ListTail(Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].Subject != "new-a" || evts[1].Subject != "new-b" {
		t.Fatalf("subjects = [%s %s], want [new-a new-b]", evts[0].Subject, evts[1].Subject)
	}
}

func TestMultiplexerListTailOrdersEqualTimestampsDeterministically(t *testing.T) {
	m := NewMultiplexer()
	ts := time.Unix(1, 0)

	alpha := NewFake()
	alpha.Events = []Event{
		{Seq: 5, Type: SessionWoke, Subject: "alpha", Ts: ts},
	}

	beta := NewFake()
	beta.Events = []Event{
		{Seq: 2, Type: SessionWoke, Subject: "beta-two", Ts: ts},
		{Seq: 1, Type: SessionWoke, Subject: "beta-one", Ts: ts},
	}

	m.Add("beta", beta)
	m.Add("alpha", alpha)

	evts, err := m.ListTail(Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	got := []string{
		evts[0].City + ":" + evts[0].Subject,
		evts[1].City + ":" + evts[1].Subject,
	}
	want := []string{"beta:beta-one", "beta:beta-two"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func TestMultiplexerListTailDoesNotWaitForSlowProvider(t *testing.T) {
	m := NewMultiplexer()
	m.providerTimeout = 20 * time.Millisecond

	healthy := NewFake()
	healthy.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "old", Ts: time.Unix(1, 0)})
	healthy.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "new", Ts: time.Unix(2, 0)})
	slow := newBlockingProvider()
	defer slow.release()

	m.Add("healthy", healthy)
	m.Add("slow", slow)

	result := make(chan struct {
		evts []TaggedEvent
		err  error
	}, 1)
	go func() {
		evts, err := m.ListTail(Filter{}, 1)
		result <- struct {
			evts []TaggedEvent
			err  error
		}{evts: evts, err: err}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("ListTail() error = %v", got.err)
		}
		if len(got.evts) != 1 || got.evts[0].City != "healthy" || got.evts[0].Subject != "new" {
			t.Fatalf("ListTail() = %+v, want only healthy provider tail event", got.evts)
		}
	case <-time.After(200 * time.Millisecond):
		slow.release()
		t.Fatal("ListTail() waited for the slow provider")
	}
}

func TestMultiplexerListTailUsesFallbackAndSkipsErrors(t *testing.T) {
	m := NewMultiplexer()

	listOnly := NewFake()
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "list-old", Ts: time.Unix(1, 0)})
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "list-middle", Ts: time.Unix(4, 0)})
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "list-new", Ts: time.Unix(6, 0)})

	tailCapable := NewFake()
	tailCapable.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "tail-old", Ts: time.Unix(2, 0)})
	tailCapable.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "tail-middle", Ts: time.Unix(3, 0)})
	tailCapable.Record(Event{Type: SessionWoke, Actor: "b1", Subject: "tail-new", Ts: time.Unix(5, 0)})

	m.Add("list-only", &providerWithoutTail{fake: listOnly})
	m.Add("tail-capable", tailCapable)
	m.Add("broken", NewFailFake())

	evts, err := m.ListTail(Filter{Type: SessionWoke}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3", len(evts))
	}
	got := []string{evts[0].Subject, evts[1].Subject, evts[2].Subject}
	want := []string{"list-middle", "tail-new", "list-new"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %v, want %v", got, want)
		}
	}
}

func TestMultiplexerListTailIgnoresFilterLimitForListOnlyProviders(t *testing.T) {
	m := NewMultiplexer()

	listOnly := NewFake()
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "old", Ts: time.Unix(1, 0)})
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "middle", Ts: time.Unix(2, 0)})
	listOnly.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "new", Ts: time.Unix(3, 0)})
	m.Add("list-only", &providerWithoutTail{fake: listOnly})

	evts, err := m.ListTail(Filter{Type: SessionWoke, Limit: 1}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	got := []string{evts[0].Subject, evts[1].Subject}
	want := []string{"middle", "new"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %v, want %v", got, want)
		}
	}
}

func TestMultiplexerListTailLimitZeroDelegatesToListAll(t *testing.T) {
	m := NewMultiplexer()
	f := NewFake()
	f.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "old", Ts: time.Unix(1, 0)})
	f.Record(Event{Type: SessionStopped, Actor: "a1", Subject: "ignored", Ts: time.Unix(2, 0)})
	f.Record(Event{Type: SessionWoke, Actor: "a1", Subject: "new", Ts: time.Unix(3, 0)})
	m.Add("city-a", f)

	evts, err := m.ListTail(Filter{Type: SessionWoke}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].Subject != "old" || evts[1].Subject != "new" {
		t.Fatalf("subjects = [%s %s], want [old new]", evts[0].Subject, evts[1].Subject)
	}
}

func TestMultiplexerLatestCursorSkipsBrokenProviders(t *testing.T) {
	m := NewMultiplexer()
	alpha := NewFake()
	alpha.Record(Event{Type: SessionWoke, Actor: "a1"})
	alpha.Record(Event{Type: SessionWoke, Actor: "a1"})
	beta := NewFake()
	beta.Record(Event{Type: SessionWoke, Actor: "b1"})

	m.Add("alpha", alpha)
	m.Add("beta", beta)
	m.Add("broken", NewFailFake())

	cursors, err := m.LatestCursor()
	if err == nil {
		t.Fatal("LatestCursor() error = nil, want broken provider error")
	}
	if len(cursors) != 2 {
		t.Fatalf("cursor count = %d, want 2: %v", len(cursors), cursors)
	}
	if cursors["alpha"] != 2 || cursors["beta"] != 1 {
		t.Fatalf("cursors = %v, want alpha:2 beta:1", cursors)
	}
	if _, ok := cursors["broken"]; ok {
		t.Fatalf("broken provider included in cursor map: %v", cursors)
	}
}

func TestMultiplexerLatestCursorDoesNotWaitForSlowProvider(t *testing.T) {
	m := NewMultiplexer()
	m.providerTimeout = 20 * time.Millisecond

	healthy := NewFake()
	healthy.Record(Event{Type: SessionWoke, Actor: "a1"})
	slow := newBlockingProvider()
	defer slow.release()

	m.Add("healthy", healthy)
	m.Add("slow", slow)

	result := make(chan struct {
		cursors map[string]uint64
		err     error
	}, 1)
	go func() {
		cursors, err := m.LatestCursor()
		result <- struct {
			cursors map[string]uint64
			err     error
		}{cursors: cursors, err: err}
	}()

	select {
	case got := <-result:
		if got.err == nil {
			t.Fatal("LatestCursor() error = nil, want slow provider timeout error")
		}
		if len(got.cursors) != 1 || got.cursors["healthy"] != 1 {
			t.Fatalf("LatestCursor() cursors = %v, want healthy:1", got.cursors)
		}
	case <-time.After(200 * time.Millisecond):
		slow.release()
		t.Fatal("LatestCursor() waited for the slow provider")
	}
}

func TestMultiplexerWatch(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f2 := NewFake()
	m.Add("city-a", f1)
	m.Add("city-b", f2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	w, err := m.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	// Record events after watch is started.
	f1.Record(Event{Type: SessionWoke, Actor: "a1"})
	f2.Record(Event{Type: SessionWoke, Actor: "b1"})

	// Should receive both events.
	got := make(map[string]bool)
	for i := 0; i < 2; i++ {
		te, err := w.Next()
		if err != nil {
			t.Fatal(err)
		}
		got[te.City] = true
	}
	if !got["city-a"] || !got["city-b"] {
		t.Errorf("missing cities: %v", got)
	}
}

func TestMultiplexerWatchDoesNotWaitForSlowProvider(t *testing.T) {
	m := NewMultiplexer()
	m.providerTimeout = 20 * time.Millisecond

	healthy := NewFake()
	slow := newBlockingProvider()
	defer slow.release()

	m.Add("healthy", healthy)
	m.Add("slow", slow)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := make(chan struct {
		w   *MuxWatcher
		err error
	}, 1)
	go func() {
		w, err := m.Watch(ctx, nil)
		result <- struct {
			w   *MuxWatcher
			err error
		}{w: w, err: err}
	}()

	var w *MuxWatcher
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Watch() error = %v", got.err)
		}
		w = got.w
	case <-time.After(200 * time.Millisecond):
		slow.release()
		cancel()
		t.Fatal("Watch() waited for the slow provider")
	}
	defer w.Close() //nolint:errcheck

	healthy.Record(Event{Type: SessionWoke, Actor: "a1"})
	te, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if te.City != "healthy" || te.Actor != "a1" {
		t.Fatalf("Next() = %+v, want healthy provider event", te)
	}
}

func TestMultiplexerWatchWithCursors(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "old"})    // seq=1
	f1.Record(Event{Type: SessionStopped, Actor: "old"}) // seq=2
	m.Add("city-a", f1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start watching from seq=1, should skip seq=1 but get seq=2.
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	te, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if te.Actor != "old" || te.Seq != 2 {
		t.Errorf("got seq=%d actor=%q, want seq=2 actor=old", te.Seq, te.Actor)
	}
}

func TestMultiplexerRemove(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1"})
	m.Add("city-a", f1)
	m.Remove("city-a")

	evts, err := m.ListAll(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 0 {
		t.Errorf("got %d events after remove, want 0", len(evts))
	}
}

func TestParseCursorFormatCursor(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]uint64
	}{
		{"", nil},
		{"city-a:5", map[string]uint64{"city-a": 5}},
		{"city-a:5,city-b:12", map[string]uint64{"city-a": 5, "city-b": 12}},
	}
	for _, tt := range tests {
		got := ParseCursor(tt.input)
		if tt.want == nil && got != nil {
			t.Errorf("ParseCursor(%q) = %v, want nil", tt.input, got)
			continue
		}
		for k, v := range tt.want {
			if got[k] != v {
				t.Errorf("ParseCursor(%q)[%q] = %d, want %d", tt.input, k, got[k], v)
			}
		}
	}

	// Round-trip test.
	m := map[string]uint64{"alpha": 10, "beta": 20}
	s := FormatCursor(m)
	m2 := ParseCursor(s)
	for k, v := range m {
		if m2[k] != v {
			t.Errorf("round-trip: %q = %d, want %d", k, m2[k], v)
		}
	}

	malformed := ParseCursor("alpha:nope,beta:7")
	if _, ok := malformed["alpha"]; ok {
		t.Fatalf("ParseCursor retained malformed alpha sequence: %v", malformed)
	}
	if malformed["beta"] != 7 {
		t.Fatalf("ParseCursor beta = %d, want 7", malformed["beta"])
	}
}

func TestWrapForSSE(t *testing.T) {
	m := NewMultiplexer()
	f1 := NewFake()
	m.Add("city-a", f1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mw, err := m.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := WrapForSSE(mw)
	defer w.Close() //nolint:errcheck

	f1.Record(Event{Type: SessionWoke, Actor: "actor-a"})

	e, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if e.Actor != "city-a/actor-a" {
		t.Errorf("Actor = %q, want %q", e.Actor, "city-a/actor-a")
	}
}

func TestMultiplexerSkipsBrokenProvider(t *testing.T) {
	m := NewMultiplexer()

	f1 := NewFake()
	f1.Record(Event{Type: SessionWoke, Actor: "a1"})
	m.Add("city-a", f1)

	broken := NewFailFake()
	m.Add("city-b", broken)

	// ListAll should still work, skipping the broken provider.
	evts, err := m.ListAll(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
}

type providerWithoutTail struct {
	fake *Fake
}

func (p *providerWithoutTail) Record(e Event) {
	p.fake.Record(e)
}

func (p *providerWithoutTail) List(filter Filter) ([]Event, error) {
	return p.fake.List(filter)
}

func (p *providerWithoutTail) LatestSeq() (uint64, error) {
	return p.fake.LatestSeq()
}

func (p *providerWithoutTail) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	return p.fake.Watch(ctx, afterSeq)
}

func (p *providerWithoutTail) Close() error {
	return p.fake.Close()
}

type blockingProvider struct {
	unblock chan struct{}
	once    sync.Once
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{unblock: make(chan struct{})}
}

func (p *blockingProvider) release() {
	p.once.Do(func() { close(p.unblock) })
}

func (p *blockingProvider) Record(Event) {}

func (p *blockingProvider) List(Filter) ([]Event, error) {
	<-p.unblock
	return nil, context.Canceled
}

func (p *blockingProvider) ListTail(Filter, int) ([]Event, error) {
	<-p.unblock
	return nil, context.Canceled
}

func (p *blockingProvider) LatestSeq() (uint64, error) {
	<-p.unblock
	return 0, context.Canceled
}

func (p *blockingProvider) Watch(ctx context.Context, _ uint64) (Watcher, error) {
	select {
	case <-p.unblock:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *blockingProvider) Close() error {
	p.release()
	return nil
}

// headStart attaches a city at its provider's current head ("now").
func headStart(_ string, p Provider) (uint64, error) {
	return p.LatestSeq()
}

// signalingProvider wraps a Fake and reports each Watch attach and each
// watcher Close, so tests can wait for the fact instead of sleeping.
type signalingProvider struct {
	*Fake
	mu      sync.Mutex
	watches int
	closed  chan struct{} // closed when the most recent watcher is closed
}

func newSignalingProvider() *signalingProvider {
	return &signalingProvider{Fake: NewFake()}
}

func (p *signalingProvider) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	inner, err := p.Fake.Watch(ctx, afterSeq)
	if err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	p.mu.Lock()
	p.watches++
	p.closed = closed
	p.mu.Unlock()
	return &signalingWatcher{Watcher: inner, closed: closed}, nil
}

func (p *signalingProvider) watchCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watches
}

func (p *signalingProvider) lastWatcherClosed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

type signalingWatcher struct {
	Watcher
	once   sync.Once
	closed chan struct{}
}

func (w *signalingWatcher) Close() error {
	err := w.Watcher.Close()
	w.once.Do(func() { close(w.closed) })
	return err
}

// nextResultWithin reads the next Next result, failing the test if Next does
// not return before the goroutine race deadline.
func nextResultWithin(t *testing.T, w *MuxWatcher) (TaggedEvent, error) {
	t.Helper()
	type result struct {
		te  TaggedEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		te, err := w.Next()
		ch <- result{te: te, err: err}
	}()
	select {
	case r := <-ch:
		return r.te, r.err
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Next() did not return")
		return TaggedEvent{}, nil
	}
}

// nextWithin reads the next tagged event, failing the test on an error.
func nextWithin(t *testing.T, w *MuxWatcher) TaggedEvent {
	t.Helper()
	te, err := nextResultWithin(t, w)
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	return te
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s was not closed", what)
	}
}

// A city whose provider appears after Watch started must be attached by Sync
// and its events delivered tagged with the city, starting from the cursor the
// start function chose (here: its head, so older events are not replayed).
func TestMuxWatcherSyncAttachesCityAddedAfterWatch(t *testing.T) {
	m := NewMultiplexer()
	a := NewFake()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	b := NewFake()
	b.Record(Event{Type: SessionWoke, Actor: "before-attach"}) // seq=1
	started, err := w.Sync(map[string]Provider{"city-a": a, "city-b": b}, headStart)
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(started) != 1 || started["city-b"] != 1 {
		t.Fatalf("Sync() started = %v, want map[city-b:1]", started)
	}
	if got := w.Cities(); len(got) != 2 || got[0] != "city-a" || got[1] != "city-b" {
		t.Fatalf("Cities() = %v, want [city-a city-b]", got)
	}

	b.Record(Event{Type: SessionWoke, Actor: "after-attach"}) // seq=2
	te := nextWithin(t, w)
	if te.City != "city-b" || te.Seq != 2 || te.Actor != "after-attach" {
		t.Fatalf("Next() = city %q seq %d actor %q, want city-b seq 2 after-attach", te.City, te.Seq, te.Actor)
	}

	a.Record(Event{Type: SessionWoke, Actor: "a1"})
	if te := nextWithin(t, w); te.City != "city-a" || te.Actor != "a1" {
		t.Fatalf("Next() = %+v, want city-a event after Sync", te)
	}
}

// Syncing an unchanged provider set must not re-attach cities that are
// already watched (no duplicate watchers, no duplicate events).
func TestMuxWatcherSyncKeepsAttachedCities(t *testing.T) {
	m := NewMultiplexer()
	a := newSignalingProvider()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	for i := 0; i < 2; i++ {
		started, err := w.Sync(map[string]Provider{"city-a": a}, headStart)
		if err != nil {
			t.Fatalf("Sync() #%d error = %v", i, err)
		}
		if len(started) != 0 {
			t.Fatalf("Sync() #%d started = %v, want none", i, started)
		}
	}
	if got := a.watchCount(); got != 1 {
		t.Fatalf("provider Watch calls = %d, want 1", got)
	}
}

// A city that disappears from the provider set is detached: its watcher is
// closed (no leaked goroutine or poller) and its later events are not
// delivered. When it comes back, Sync resumes it from the start cursor so a
// city that stops and restarts mid-stream loses nothing.
func TestMuxWatcherSyncDetachesRemovedCityAndResumesOnReturn(t *testing.T) {
	m := NewMultiplexer()
	a := NewFake()
	b := newSignalingProvider()
	m.Add("city-a", a)
	m.Add("city-b", b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0, "city-b": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	if _, err := w.Sync(map[string]Provider{"city-a": a}, headStart); err != nil {
		t.Fatalf("Sync() detach error = %v", err)
	}
	waitClosed(t, b.lastWatcherClosed(), "detached city-b watcher")
	if got := w.Cities(); len(got) != 1 || got[0] != "city-a" {
		t.Fatalf("Cities() = %v, want [city-a]", got)
	}

	b.Record(Event{Type: SessionWoke, Actor: "while-detached"}) // city-b seq=1
	a.Record(Event{Type: SessionWoke, Actor: "a1"})
	if te := nextWithin(t, w); te.City != "city-a" || te.Actor != "a1" {
		t.Fatalf("Next() = %+v, want city-a a1 (detached city-b must not deliver)", te)
	}

	resume := func(city string, _ Provider) (uint64, error) {
		if city != "city-b" {
			t.Errorf("start called for %q, want only city-b", city)
		}
		return 0, nil // the last seq the consumer saw for city-b
	}
	started, err := w.Sync(map[string]Provider{"city-a": a, "city-b": b}, resume)
	if err != nil {
		t.Fatalf("Sync() re-attach error = %v", err)
	}
	if started["city-b"] != 0 || len(started) != 1 {
		t.Fatalf("Sync() started = %v, want map[city-b:0]", started)
	}
	if te := nextWithin(t, w); te.City != "city-b" || te.Seq != 1 || te.Actor != "while-detached" {
		t.Fatalf("Next() = %+v, want city-b seq 1 replayed from the resume cursor", te)
	}
}

// Swapping the only watched city for another in one Sync must not end the
// stream: "all watchers finished" is reported only when nothing is attached.
func TestMuxWatcherSyncSwapsLastCityWithoutFinishing(t *testing.T) {
	m := NewMultiplexer()
	a := newSignalingProvider()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	b := NewFake()
	if _, err := w.Sync(map[string]Provider{"city-b": b}, headStart); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	waitClosed(t, a.lastWatcherClosed(), "detached city-a watcher")
	b.Record(Event{Type: SessionWoke, Actor: "b1"})
	if te := nextWithin(t, w); te.City != "city-b" || te.Actor != "b1" {
		t.Fatalf("Next() = %+v, want city-b b1", te)
	}
}

// Detaching every city keeps the existing contract: Next reports that all
// watchers finished, and a later Sync cannot revive the watcher.
func TestMuxWatcherSyncDetachingEveryCityFinishes(t *testing.T) {
	m := NewMultiplexer()
	a := NewFake()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	if _, err := w.Sync(map[string]Provider{}, headStart); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if _, err := nextResultWithin(t, w); err == nil {
		t.Fatal("Next() error = nil, want all watchers finished")
	}
	b := newSignalingProvider()
	if _, err := w.Sync(map[string]Provider{"city-b": b}, headStart); !errors.Is(err, ErrMuxWatcherFinished) {
		t.Fatalf("Sync() after finish error = %v, want ErrMuxWatcherFinished", err)
	}
	if got := b.watchCount(); got != 0 {
		t.Fatalf("provider Watch calls after finish = %d, want 0", got)
	}
}

// A provider that cannot be attached is reported (never silently dropped),
// does not disturb the cities already watched, and is retried by the next
// Sync.
func TestMuxWatcherSyncReportsAttachFailureAndRetries(t *testing.T) {
	m := NewMultiplexer()
	a := NewFake()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck

	_, err = w.Sync(map[string]Provider{"city-a": a, "city-b": NewFailFake()}, headStart)
	if err == nil || !strings.Contains(err.Error(), "city-b") {
		t.Fatalf("Sync() error = %v, want an error naming city-b", err)
	}
	if got := w.Cities(); len(got) != 1 || got[0] != "city-a" {
		t.Fatalf("Cities() = %v, want [city-a]", got)
	}

	healthy := NewFake()
	started, err := w.Sync(map[string]Provider{"city-a": a, "city-b": healthy}, headStart)
	if err != nil {
		t.Fatalf("Sync() retry error = %v", err)
	}
	if _, ok := started["city-b"]; !ok {
		t.Fatalf("Sync() retry started = %v, want city-b attached", started)
	}
	healthy.Record(Event{Type: SessionWoke, Actor: "b1"})
	if te := nextWithin(t, w); te.City != "city-b" || te.Actor != "b1" {
		t.Fatalf("Next() = %+v, want city-b b1", te)
	}
}

// Closing the MuxWatcher closes every city watcher, including ones attached
// by Sync, and a closed watcher refuses to attach more.
func TestMuxWatcherCloseReleasesSyncedCities(t *testing.T) {
	m := NewMultiplexer()
	a := NewFake()
	m.Add("city-a", a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := m.Watch(ctx, map[string]uint64{"city-a": 0})
	if err != nil {
		t.Fatal(err)
	}
	b := newSignalingProvider()
	if _, err := w.Sync(map[string]Provider{"city-a": a, "city-b": b}, headStart); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitClosed(t, b.lastWatcherClosed(), "synced city-b watcher after Close")
	c := newSignalingProvider()
	if _, err := w.Sync(map[string]Provider{"city-c": c}, headStart); !errors.Is(err, ErrMuxWatcherFinished) {
		t.Fatalf("Sync() after Close error = %v, want ErrMuxWatcherFinished", err)
	}
	if got := c.watchCount(); got != 0 {
		t.Fatalf("provider Watch calls after Close = %d, want 0", got)
	}
}
