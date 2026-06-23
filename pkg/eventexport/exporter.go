package eventexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// TaggedEvent is the closed set of primitive fields the exporter consumes for
// one event: the source city plus the projectable fields. A Source yields these
// in per-city seq order; the supervisor-coupled adapter builds them from its
// event stream so this package never depends on the event backend. RunID and
// SessionID are opaque correlation ids (empty unless a typed source populates
// them) and are gated through safeRef during projection.
type TaggedEvent struct {
	City      string
	Seq       uint64
	Type      string
	Ts        time.Time
	Actor     string
	Subject   string
	RunID     string
	SessionID string
	_         struct{} // force keyed literals; blocks positional field transposition
}

// Source yields tagged events in per-city seq order. The real Source wraps the
// supervisor event multiplexer (internal/eventfeed); tests use a fake.
type Source interface {
	Next(ctx context.Context) (TaggedEvent, error)
}

// Config configures an Exporter. Endpoint must be non-empty for the exporter to
// do anything — that is the opt-in: absent config means no export.
type Config struct {
	Endpoint string
	// TokenProvider, when non-nil, supplies the bearer sent as
	// Authorization: Bearer on each POST. It is called per POST so a file-backed
	// token can be rotated out of band; an error holds the cursor and retries.
	TokenProvider     func() (string, error)
	Salt              []byte
	ExportRef         bool
	Profile           Profile
	EmitCorrelation   bool          // emit opaque run_id/session_id (default false)
	BatchMax          int           // max events per POST (default 1000)
	BatchInterval     time.Duration // max time between POSTs (default 5s)
	MaxPendingPerCity int           // backpressure threshold (default 50000)
	Client            *http.Client
	Logf              func(format string, args ...any)
}

// Exporter projects events and ships per-city batches to Config.Endpoint. One
// Exporter drives one Run loop.
type Exporter struct {
	cfg Config

	mu      sync.Mutex
	pending map[string][]Envelope // city -> unsent envelopes
	high    map[string]uint64     // city -> highest processed seq (sent or dropped)
	cursor  map[string]uint64     // city -> last durably-acked seq
}

// New builds an Exporter, applying defaults.
func New(cfg Config) *Exporter {
	if cfg.BatchMax <= 0 {
		cfg.BatchMax = 1000
	}
	if cfg.BatchInterval <= 0 {
		cfg.BatchInterval = 5 * time.Second
	}
	if cfg.MaxPendingPerCity <= 0 {
		cfg.MaxPendingPerCity = 50000
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Exporter{
		cfg:     cfg,
		pending: map[string][]Envelope{},
		high:    map[string]uint64{},
		cursor:  map[string]uint64{},
	}
}

// SetCursors seeds resume points (e.g. from persisted state) before Run.
func (e *Exporter) SetCursors(c map[string]uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range c {
		e.cursor[k] = v
		e.high[k] = v
	}
}

// Cursors returns a snapshot of the per-city acked seq, for persistence.
func (e *Exporter) Cursors() map[string]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]uint64, len(e.cursor))
	for k, v := range e.cursor {
		out[k] = v
	}
	return out
}

// Run pulls from src, projects, batches, and ships until ctx is canceled.
//
// Durability: the cursor advances only on a confirmed POST, so a sink outage
// holds the cursor and the in-memory buffer is bounded — once it fills we stop
// pulling from src (backpressure). Because the underlying event watcher polls
// the durable events.jsonl rather than receiving pushes, stalling consumption
// never blocks event recording, and a restart resumes from the persisted cursor.
func (e *Exporter) Run(ctx context.Context, src Source) error {
	// Decouple the blocking Next from the flush timer via a bounded hand-off
	// channel; when it (and pending) fill, the puller blocks => backpressure.
	in := make(chan TaggedEvent, 256)
	go func() {
		for {
			te, err := src.Next(ctx)
			if err != nil {
				close(in)
				return
			}
			select {
			case in <- te:
			case <-ctx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(e.cfg.BatchInterval)
	defer ticker.Stop()
	for {
		// Disable ingest while any city is over its pending cap (backpressure).
		var inCh <-chan TaggedEvent = in
		if e.overCap() {
			inCh = nil
		}
		select {
		case <-ctx.Done():
			// Best-effort final drain, bounded so the detached goroutine cannot
			// linger against a slow/down sink.
			sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			e.flushAll(sctx)
			c()
			return ctx.Err()
		case te, ok := <-inCh:
			if !ok {
				e.flushAll(ctx)
				return nil
			}
			e.ingest(te)
			if e.cityLen(te.City) >= e.cfg.BatchMax {
				e.flushCity(ctx, te.City)
			}
		case <-ticker.C:
			e.flushAll(ctx)
		}
	}
}

func (e *Exporter) ingest(te TaggedEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if te.Seq <= e.high[te.City] {
		return // already processed (resume overlap)
	}
	e.high[te.City] = te.Seq
	opt := Options{Salt: e.cfg.Salt, ExportRef: e.cfg.ExportRef, Profile: e.cfg.Profile, EmitCorrelation: e.cfg.EmitCorrelation}
	env, ok := ProjectEvent(te, opt)
	if !ok {
		return
	}
	// Defense-in-depth at the trust boundary: never ship an envelope that fails
	// the redaction invariants. ProjectEvent builds a valid envelope by
	// construction, so a failure here is a projection bug — drop it loudly rather
	// than egress something unexpected.
	if err := Validate(env, opt); err != nil {
		e.cfg.Logf("eventexport: dropped envelope failing self-validation (seq=%d type=%s): %v", te.Seq, te.Type, err)
		return
	}
	e.pending[te.City] = append(e.pending[te.City], env)
}

func (e *Exporter) cityLen(city string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending[city])
}

func (e *Exporter) overCap() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.pending {
		if len(p) >= e.cfg.MaxPendingPerCity {
			return true
		}
	}
	return false
}

func (e *Exporter) flushAll(ctx context.Context) {
	e.mu.Lock()
	cities := make([]string, 0, len(e.high))
	for c := range e.high {
		cities = append(cities, c)
	}
	e.mu.Unlock()
	sort.Strings(cities)
	for _, c := range cities {
		e.flushCity(ctx, c)
	}
}

// flushCity ships one city's pending batch (if any) and, on success, advances
// the cursor to the high-water of processed seqs — past dropped events too, so
// filtered churn is never re-fetched.
func (e *Exporter) flushCity(ctx context.Context, city string) {
	e.mu.Lock()
	batch := e.pending[city]
	high := e.high[city]
	cur := e.cursor[city]
	e.mu.Unlock()

	if high <= cur {
		return // nothing new processed
	}
	if len(batch) > 0 {
		if err := e.post(ctx, city, batch); err != nil {
			e.cfg.Logf("eventexport: post failed for %s (cursor held at %d): %v", city, cur, err)
			return // hold cursor; retry next tick
		}
	}
	e.mu.Lock()
	// Only clear the envelopes we shipped; anything appended since stays.
	e.pending[city] = e.pending[city][len(batch):]
	if e.cursor[city] < high {
		e.cursor[city] = high
	}
	e.mu.Unlock()
}

func (e *Exporter) post(ctx context.Context, city string, batch []Envelope) error {
	body, err := json.Marshal(Batch{CityID: city, SchemaVersion: SchemaVersion, Events: batch})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.TokenProvider != nil {
		token, terr := e.cfg.TokenProvider()
		if terr != nil {
			return fmt.Errorf("eventexport: resolve token: %w", terr)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := e.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	return nil
}
