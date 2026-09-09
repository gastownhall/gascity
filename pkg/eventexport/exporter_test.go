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
// run/session/step correlation only when Config.EmitCorrelation is true.
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
		src.ch <- TaggedEvent{City: "c1", Seq: 1, Type: "bead.closed", Ts: fixedTS, Actor: "ctl", Subject: "mc-1", RunID: "wf-root-abc", SessionID: "sess-9f2a", StepID: "mc-step-7"}
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
	if on.Events[0].RunID != "wf-root-abc" || on.Events[0].SessionID != "sess-9f2a" || on.Events[0].StepID != "mc-step-7" {
		t.Fatalf("EmitCorrelation=true must carry run/session/step, got %+v", on.Events[0])
	}
	// A receiver pinned to this build's schema accepts populated optional
	// correlation fields without a schema mismatch.
	if err := ValidateBatch(on); err != nil {
		t.Fatalf("receiver must accept a populated batch: %v", err)
	}

	off := run(false)
	if off.Events[0].RunID != "" || off.Events[0].SessionID != "" || off.Events[0].StepID != "" {
		t.Fatalf("EmitCorrelation=false must drop run/session/step, got %+v", off.Events[0])
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

// TestParseRetryAfter covers both RFC 9110 Retry-After forms plus the inputs a
// sink can realistically get wrong, which must fall back to our own backoff.
func TestParseRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want time.Duration
	}{
		{"absent", "", 0},
		{"delay seconds", "30", 30 * time.Second},
		{"padded delay seconds", "  7  ", 7 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"unparsable", "soon", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}

	future := time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 90*time.Second {
		t.Fatalf("parseRetryAfter(future HTTP-date) = %s, want within (0, 90s]", got)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Fatalf("parseRetryAfter(past HTTP-date) = %s, want 0", got)
	}
}

// TestExporterHoldOffBacksOffAndHonorsRetryAfter proves consecutive failures
// back off geometrically from BatchInterval, that a sink's Retry-After wins
// when it sends one, and that neither can park a city past maxHoldOff.
func TestExporterHoldOffBacksOffAndHonorsRetryAfter(t *testing.T) {
	exp := New(Config{Endpoint: "https://example.invalid/ingest", BatchInterval: time.Second})

	generic := errors.New("dial failed")
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		exp.holdOff("c1", generic)
		if got := exp.retryHold["c1"]; got != want {
			t.Fatalf("failure %d: hold = %s, want %s", i+1, got, want)
		}
		if !exp.retryAt["c1"].After(time.Now()) {
			t.Fatalf("failure %d: retryAt must be in the future", i+1)
		}
	}

	// A usable Retry-After replaces the doubling schedule outright.
	exp.holdOff("c1", &statusError{code: http.StatusTooManyRequests, retryAfter: 45 * time.Second})
	if got := exp.retryHold["c1"]; got != 45*time.Second {
		t.Fatalf("Retry-After hold = %s, want 45s", got)
	}

	// An absurd one is capped, so a bad hint cannot disable the export.
	exp.holdOff("c1", &statusError{code: http.StatusTooManyRequests, retryAfter: 72 * time.Hour})
	if got := exp.retryHold["c1"]; got != maxHoldOff {
		t.Fatalf("oversized Retry-After hold = %s, want the %s cap", got, maxHoldOff)
	}
}

// TestExporterFlushHoldsCursorWhileBackedOff proves a city under an active hold
// issues no request at all. Retrying through a rate limit is self-reinforcing —
// it keeps the caller over the limit — so suppressing the attempt is the point.
func TestExporterFlushHoldsCursorWhileBackedOff(t *testing.T) {
	var dialed int32
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&dialed, 1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	exp := New(Config{
		Endpoint: "https://example.invalid/ingest", Salt: testSalt, ExportRef: true,
		BatchInterval: time.Second, Client: &http.Client{Transport: rt},
	})
	exp.ingest(tev("c1", 1, "bead.closed", "controller", "mc-1"))
	exp.retryAt["c1"] = time.Now().Add(time.Hour)

	exp.flushCity(context.Background(), "c1")
	if n := atomic.LoadInt32(&dialed); n != 0 {
		t.Fatalf("made %d requests while backed off, want 0", n)
	}
	if c := exp.Cursors()["c1"]; c != 0 {
		t.Fatalf("cursor advanced to %d while backed off", c)
	}

	// Once the hold expires the same flush ships, and success clears the hold.
	exp.retryAt["c1"] = time.Now().Add(-time.Second)
	exp.flushCity(context.Background(), "c1")
	if n := atomic.LoadInt32(&dialed); n != 1 {
		t.Fatalf("made %d requests after the hold expired, want 1", n)
	}
	if c := exp.Cursors()["c1"]; c != 1 {
		t.Fatalf("cursor = %d after a confirmed POST, want 1", c)
	}
	if _, held := exp.retryAt["c1"]; held {
		t.Fatalf("a confirmed POST must clear the hold")
	}
}

// TestExporterFlushCapsBatchAtBatchMax proves a backlog ships in BatchMax-sized
// POSTs and that a capped flush advances the cursor only to the prefix it
// shipped: advancing to the processed high-water would skip the remainder.
func TestExporterFlushCapsBatchAtBatchMax(t *testing.T) {
	const batchMax = 3
	const lastSeq = uint64(7)

	var sizes []int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var b Batch
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, err
		}
		sizes = append(sizes, len(b.Events))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	exp := New(Config{
		Endpoint: "https://example.invalid/ingest", Salt: testSalt, ExportRef: true,
		BatchMax: batchMax, BatchInterval: time.Second, Client: &http.Client{Transport: rt},
	})
	for seq := uint64(1); seq <= lastSeq; seq++ {
		exp.ingest(tev("c1", seq, "bead.closed", "controller", "mc-x"))
	}

	exp.flushCity(context.Background(), "c1")
	if c := exp.Cursors()["c1"]; c != batchMax {
		t.Fatalf("cursor = %d after a capped flush, want the last shipped seq (%d)", c, batchMax)
	}
	exp.flushCity(context.Background(), "c1")
	exp.flushCity(context.Background(), "c1")
	if c := exp.Cursors()["c1"]; c != lastSeq {
		t.Fatalf("cursor = %d after draining, want %d", c, lastSeq)
	}

	want := []int{batchMax, batchMax, 1}
	if len(sizes) != len(want) {
		t.Fatalf("posted batches %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("posted batches %v, want %v", sizes, want)
		}
	}
}
