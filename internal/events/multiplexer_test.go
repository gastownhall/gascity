package events

import (
	"context"
	"testing"
	"time"
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

	f1.Record(Event{Type: SessionWoke, Actor: "mayor"})

	e, err := w.Next()
	if err != nil {
		t.Fatal(err)
	}
	if e.Actor != "city-a/mayor" {
		t.Errorf("Actor = %q, want %q", e.Actor, "city-a/mayor")
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
