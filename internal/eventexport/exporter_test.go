package eventexport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// chanSource is a test Source backed by a channel.
type chanSource struct{ ch chan events.TaggedEvent }

func (s *chanSource) Next(ctx context.Context) (events.TaggedEvent, error) {
	select {
	case <-ctx.Done():
		return events.TaggedEvent{}, ctx.Err()
	case te := <-s.ch:
		return te, nil
	}
}

func tagged(city string, e events.Event) events.TaggedEvent { //nolint:unparam // helper kept general
	return events.TaggedEvent{Event: e, City: city}
}

type capture struct {
	mu      sync.Mutex
	batches []Batch
	auth    string
	status  int
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = r.Header.Get("Authorization")
	body, _ := io.ReadAll(r.Body)
	var b Batch
	if json.Unmarshal(body, &b) == nil {
		c.batches = append(c.batches, b)
	}
	st := c.status
	if st == 0 {
		st = http.StatusOK
	}
	w.WriteHeader(st)
}

func (c *capture) all() []Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Batch(nil), c.batches...)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) { //nolint:unparam // helper kept general
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestExporter_BatchesRedactsAdvancesCursor(t *testing.T) {
	cp := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cp.handler))
	defer srv.Close()

	src := &chanSource{ch: make(chan events.TaggedEvent, 8)}
	exp := New(Config{
		Endpoint: srv.URL, Token: "tok-123", Salt: []byte("s"), ExportRef: true,
		BatchMax: 100, BatchInterval: 15 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tagged("c1", ev(1, "bead.closed", "controller", "mc-1", "", `{"bead":{"title":"secret"}}`))
	src.ch <- tagged("c1", ev(2, "bead.updated", "controller", "mc-2", "", `{"bead":{"title":"churn"}}`)) // dropped
	src.ch <- tagged("c1", ev(3, "order.completed", "controller", "nightly-sweep", "", ""))

	waitFor(t, 2*time.Second, func() bool {
		for _, b := range cp.all() {
			if len(b.Events) > 0 {
				return true
			}
		}
		return exp.Cursors()["c1"] >= 3
	})
	// cursor must advance to 3 even though seq 2 was dropped by the allowlist.
	waitFor(t, 2*time.Second, func() bool { return exp.Cursors()["c1"] == 3 })

	cancel()
	<-done

	// collect every event across batches
	var types []string
	var blob strings.Builder
	for _, b := range cp.all() {
		if b.CityID != "c1" || b.SchemaVersion != SchemaVersion {
			t.Fatalf("bad batch envelope: %+v", b)
		}
		for _, e := range b.Events {
			types = append(types, e.Type)
		}
		j, _ := json.Marshal(b)
		blob.Write(j)
	}
	if cp.auth != "Bearer tok-123" {
		t.Fatalf("auth header = %q", cp.auth)
	}
	if strings.Contains(strings.Join(types, ","), "bead.updated") {
		t.Fatalf("bead.updated must not be exported, got %v", types)
	}
	for _, f := range []string{"secret", "churn", "title", "payload"} {
		if strings.Contains(blob.String(), f) {
			t.Fatalf("LEAK: %q in exported batches", f)
		}
	}
}

func TestExporter_HoldsCursorOnSinkFailure(t *testing.T) {
	cp := &capture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(cp.handler))
	defer srv.Close()

	src := &chanSource{ch: make(chan events.TaggedEvent, 8)}
	exp := New(Config{
		Endpoint: srv.URL, Token: "t", Salt: []byte("s"), ExportRef: true,
		BatchMax: 100, BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tagged("c1", ev(5, "bead.closed", "controller", "mc-5", "", ""))

	// sink is failing: at least one attempt happens, cursor must NOT advance.
	waitFor(t, 2*time.Second, func() bool { return len(cp.all()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if c := exp.Cursors()["c1"]; c != 0 {
		t.Fatalf("cursor advanced to %d despite sink failure", c)
	}

	// recover: cursor advances once the sink accepts.
	cp.mu.Lock()
	cp.status = http.StatusOK
	cp.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return exp.Cursors()["c1"] == 5 })

	cancel()
	<-done
}
