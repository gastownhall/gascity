package eventexport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chanSource is a test Source backed by a channel.
type chanSource struct{ ch chan TaggedEvent }

func (s *chanSource) Next(ctx context.Context) (TaggedEvent, error) {
	select {
	case <-ctx.Done():
		return TaggedEvent{}, ctx.Err()
	case te := <-s.ch:
		return te, nil
	}
}

func tev(city string, seq uint64, typ, actor, subject string) TaggedEvent { //nolint:unparam // helper kept general
	return TaggedEvent{City: city, Seq: seq, Type: typ, Ts: fixedTS, Actor: actor, Subject: subject}
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

func (c *capture) authHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
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

	src := &chanSource{ch: make(chan TaggedEvent, 8)}
	exp := New(Config{
		Endpoint: srv.URL, TokenProvider: func() (string, error) { return "tok-123", nil },
		Salt: testSalt, ExportRef: true,
		BatchMax: 100, BatchInterval: 15 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tev("c1", 1, "bead.closed", "controller", "mc-1")
	src.ch <- tev("c1", 2, "bead.updated", "controller", "mc-2") // dropped
	src.ch <- tev("c1", 3, "order.completed", "controller", "nightly-sweep")

	// cursor must advance to 3 even though seq 2 was dropped by the allowlist.
	waitFor(t, 2*time.Second, func() bool { return exp.Cursors()["c1"] == 3 })

	cancel()
	<-done

	var types []string
	var blob strings.Builder
	wantCity := CityHash(testSalt, "c1")
	for _, b := range cp.all() {
		if b.CityHash != wantCity || b.SchemaVersion != SchemaVersion {
			t.Fatalf("bad batch envelope: %+v", b)
		}
		for _, e := range b.Events {
			types = append(types, e.Type)
		}
		j, _ := json.Marshal(b)
		blob.Write(j)
	}
	if cp.authHeader() != "Bearer tok-123" {
		t.Fatalf("auth header = %q", cp.authHeader())
	}
	if strings.Contains(strings.Join(types, ","), "bead.updated") {
		t.Fatalf("bead.updated must not be exported, got %v", types)
	}
	for _, f := range []string{"nightly-sweep", "payload"} {
		if strings.Contains(blob.String(), f) {
			t.Fatalf("LEAK: %q in exported batches", f)
		}
	}
}

// TestExporter_HashesCityName proves the cleartext city name never reaches the
// wire: the batch carries only a salted, opaque city_hash partition key.
func TestExporter_HashesCityName(t *testing.T) {
	cp := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cp.handler))
	defer srv.Close()

	const city = "acme-prod" // operator-chosen, customer-identifying
	src := &chanSource{ch: make(chan TaggedEvent, 4)}
	exp := New(Config{
		Endpoint: srv.URL, Salt: testSalt, ExportRef: true,
		BatchMax: 100, BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tev(city, 1, "bead.closed", "controller", "mc-1")
	waitFor(t, 2*time.Second, func() bool { return exp.Cursors()[city] == 1 })
	cancel()
	<-done

	batches := cp.all()
	if len(batches) == 0 {
		t.Fatal("no batch captured")
	}
	want := CityHash(testSalt, city)
	for _, b := range batches {
		if b.CityHash != want {
			t.Fatalf("city_hash = %q, want %q (salted hash of %q)", b.CityHash, want, city)
		}
		j, _ := json.Marshal(b)
		if strings.Contains(string(j), city) {
			t.Fatalf("LEAK: cleartext city %q on the wire: %s", city, j)
		}
	}
}

func TestExporter_HoldsCursorOnSinkFailure(t *testing.T) {
	cp := &capture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(cp.handler))
	defer srv.Close()

	src := &chanSource{ch: make(chan TaggedEvent, 8)}
	exp := New(Config{
		Endpoint: srv.URL, TokenProvider: func() (string, error) { return "t", nil },
		Salt: testSalt, ExportRef: true,
		BatchMax: 100, BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tev("c1", 5, "bead.closed", "controller", "mc-5")

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

// TestExporter_TokenProviderErrorHoldsCursor proves a TokenProvider error holds
// the cursor (fail-closed: no unauthenticated POST, retry next tick).
func TestExporter_TokenProviderErrorHoldsCursor(t *testing.T) {
	var dialed int32
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&dialed, 1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	exp := New(Config{
		Endpoint: "https://example.invalid/ingest", Salt: testSalt, ExportRef: true,
		TokenProvider: func() (string, error) { return "", errors.New("boom") },
		BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: &http.Client{Transport: rt},
	})
	src := &chanSource{ch: make(chan TaggedEvent, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tev("c1", 1, "bead.closed", "controller", "mc-1")
	time.Sleep(80 * time.Millisecond) // let several flush ticks fire
	cancel()
	<-done

	if c := exp.Cursors()["c1"]; c != 0 {
		t.Fatalf("cursor advanced to %d despite token error", c)
	}
	if n := atomic.LoadInt32(&dialed); n != 0 {
		t.Fatalf("made %d HTTP dials despite token error (must fail closed before dialing)", n)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestExporter_EmitCorrelation proves the end-to-end exported batch carries
// run_id/session_id only when Config.EmitCorrelation is true (the v1-compatible
// opt-in; SchemaVersion stays 1 since the envelope already defines the fields).
func TestExporter_EmitCorrelation(t *testing.T) {
	run := func(emit bool) Batch {
		cp := &capture{}
		srv := httptest.NewServer(http.HandlerFunc(cp.handler))
		defer srv.Close()
		src := &chanSource{ch: make(chan TaggedEvent, 4)}
		exp := New(Config{
			Endpoint: srv.URL, Salt: testSalt, ExportRef: true, EmitCorrelation: emit,
			BatchMax: 100, BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
			Client: srv.Client(),
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = exp.Run(ctx, src); close(done) }()
		src.ch <- TaggedEvent{City: "c1", Seq: 1, Type: "bead.closed", Ts: fixedTS, Actor: "ctl", Subject: "mc-1", RunID: "wf-root-abc", SessionID: "sess-9f2a"}
		waitFor(t, 2*time.Second, func() bool { return exp.Cursors()["c1"] == 1 })
		cancel()
		<-done
		all := cp.all()
		if len(all) == 0 || len(all[0].Events) == 0 {
			t.Fatalf("expected at least one exported event")
		}
		// Emitting run/session correlation is version-neutral: it must not change
		// the batch schema_version away from the package's current SchemaVersion.
		// Assert the const, not a literal, so a legitimate wire bump (e.g. the v2
		// city_id->city_hash change in #3678) doesn't falsely fail this guard.
		if all[0].SchemaVersion != SchemaVersion {
			t.Fatalf("schema_version = %d, want %d (emitting run/session is version-neutral)", all[0].SchemaVersion, SchemaVersion)
		}
		return all[0]
	}

	on := run(true)
	if on.Events[0].RunID != "wf-root-abc" || on.Events[0].SessionID != "sess-9f2a" {
		t.Fatalf("EmitCorrelation=true must carry run/session, got %+v", on.Events[0])
	}
	// Headline invariant: a v1-pinned receiver accepts the populated batch with no
	// schema mismatch — emitting run/session is v1-compatible (no flag day).
	if err := ValidateBatch(on); err != nil {
		t.Fatalf("v1 receiver must accept a populated batch: %v", err)
	}

	off := run(false)
	if off.Events[0].RunID != "" || off.Events[0].SessionID != "" {
		t.Fatalf("EmitCorrelation=false must drop run/session, got %+v", off.Events[0])
	}
}

// TestExporter_NoTokenProviderNoAuthHeader proves a nil TokenProvider sends no
// Authorization header (the unauthenticated opt-out), mirroring the old empty-
// token behavior.
func TestExporter_NoTokenProviderNoAuthHeader(t *testing.T) {
	cp := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(cp.handler))
	defer srv.Close()

	src := &chanSource{ch: make(chan TaggedEvent, 4)}
	exp := New(Config{
		Endpoint: srv.URL, Salt: testSalt, ExportRef: true,
		BatchMax: 100, BatchInterval: 10 * time.Millisecond, MaxPendingPerCity: 1000,
		Client: srv.Client(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = exp.Run(ctx, src); close(done) }()

	src.ch <- tev("c1", 1, "bead.closed", "controller", "mc-1")
	waitFor(t, 2*time.Second, func() bool { return exp.Cursors()["c1"] == 1 })
	cancel()
	<-done

	if h := cp.authHeader(); h != "" {
		t.Fatalf("nil TokenProvider must send no Authorization header, got %q", h)
	}
}
