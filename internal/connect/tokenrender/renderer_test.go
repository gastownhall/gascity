package tokenrender

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/connect"
)

const testCity = "maintainer-city"

func renderKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

// fakeSource is a RowSource over session beads, filtered with
// EligibleTokenRow as a real source is.
type fakeSource struct {
	mu      sync.Mutex
	beads   []beads.Bead
	err     error
	calls   int
	entered chan int      // receives the call number as each read begins
	gate    chan struct{} // when set, each read waits for a value
	check   func() error  // runs inside each read
	// hook runs inside each read with the read's context and call number.
	hook func(ctx context.Context, n int) error
}

func (f *fakeSource) EligibleRows(ctx context.Context) ([]connect.TokenRow, error) {
	f.mu.Lock()
	f.calls++
	n, entered, gate, check, hook := f.calls, f.entered, f.gate, f.check, f.hook
	f.mu.Unlock()
	if entered != nil {
		entered <- n
	}
	if gate != nil {
		<-gate
	}
	if check != nil {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if hook != nil {
		if err := hook(ctx, n); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var rows []connect.TokenRow
	for _, b := range f.beads {
		if row, ok := EligibleTokenRow(b); ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *fakeSource) set(bs ...beads.Bead) {
	f.mu.Lock()
	f.beads = bs
	f.mu.Unlock()
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func live(id string, generation int) beads.Bead {
	return beads.Bead{ID: id, Status: "open", Metadata: map[string]string{"state": "active", "generation": fmt.Sprint(generation)}}
}

func withState(b beads.Bead, status string, meta map[string]string) beads.Bead {
	m := map[string]string{}
	for k, v := range b.Metadata {
		m[k] = v
	}
	for k, v := range meta {
		m[k] = v
	}
	return beads.Bead{ID: b.ID, Status: status, Metadata: m}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

type harness struct {
	r    *Renderer
	src  *fakeSource
	logs *logSink
	clk  *clock.Fake
}

func newHarness(t *testing.T, outputs []Output, src *fakeSource) *harness {
	t.Helper()
	if src == nil {
		src = &fakeSource{}
	}
	logs := &logSink{}
	clk := &clock.Fake{Time: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	r, err := New(Config{City: testCity, Key: renderKey(), Outputs: outputs, Rows: src, Clock: clk, Logf: logs.logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{r: r, src: src, logs: logs, clk: clk}
}

func output(t *testing.T, ledger string, operator ...string) Output {
	t.Helper()
	dir := t.TempDir()
	o := Output{Ledger: ledger, TokenFile: filepath.Join(dir, "serve-tokens")}
	if len(operator) > 0 {
		o.OperatorFile = filepath.Join(t.TempDir(), "operator-tokens")
		if err := os.WriteFile(o.OperatorFile, []byte(strings.Join(operator, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func expectRender(t *testing.T, operator []string, rows ...connect.TokenRow) []byte {
	t.Helper()
	out, err := connect.RenderTokenFile(operator, renderKey(), testCity, rows)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func bearerOf(t *testing.T, id string, generation int) string {
	t.Helper()
	b, err := connect.MintBearer(renderKey(), testCity, id, generation)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gcLines(content []byte) []string {
	var out []string
	for _, line := range strings.Split(string(content), "\n") {
		if connect.IsGCLine(line) {
			out = append(out, line)
		}
	}
	return out
}

func TestNewRefusesBadConfig(t *testing.T) {
	src := &fakeSource{}
	abs := filepath.Join(t.TempDir(), "f")
	good := Config{City: testCity, Key: renderKey(), Rows: src, Outputs: []Output{{Ledger: "work", TokenFile: abs}}}
	cases := map[string]func(*Config){
		"bad city":             func(c *Config) { c.City = "../x" },
		"empty city":           func(c *Config) { c.City = "" },
		"short key":            func(c *Config) { c.Key = c.Key[:31] },
		"no row source":        func(c *Config) { c.Rows = nil },
		"negative interval":    func(c *Config) { c.Interval = -time.Second },
		"empty ledger":         func(c *Config) { c.Outputs[0].Ledger = "" },
		"relative token file":  func(c *Config) { c.Outputs[0].TokenFile = "tokens" },
		"unclean token file":   func(c *Config) { c.Outputs[0].TokenFile = abs + "/../f" },
		"relative operator":    func(c *Config) { c.Outputs[0].OperatorFile = "op" },
		"duplicate ledger":     func(c *Config) { c.Outputs = append(c.Outputs, Output{Ledger: "work", TokenFile: abs + "2"}) },
		"duplicate token file": func(c *Config) { c.Outputs = append(c.Outputs, Output{Ledger: "infra", TokenFile: abs}) },
		"operator is rendered": func(c *Config) {
			c.Outputs = append(c.Outputs, Output{Ledger: "infra", TokenFile: abs + "2", OperatorFile: abs})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			cfg.Key = slices.Clone(good.Key)
			cfg.Outputs = slices.Clone(good.Outputs)
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted")
			}
		})
	}
	if _, err := New(good); err != nil {
		t.Fatalf("good config refused: %v", err)
	}
	none := good
	none.Outputs = nil
	if _, err := New(none); err != nil {
		t.Fatalf("a renderer with no outputs (every file sidecar-rendered) refused: %v", err)
	}
}

func TestRenderOnceRendersAndIsIdempotent(t *testing.T) {
	work := output(t, "work", "op-a", "op-b")
	infra := output(t, "infra")
	src := &fakeSource{}
	src.set(live("mc-2", 3), live("mc-1", 1), withState(live("mc-3", 1), "open", map[string]string{"state": "asleep"}))
	h := newHarness(t, []Output{work, infra}, src)

	rep, err := h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatalf("RenderOnce: %v", err)
	}
	rows := []connect.TokenRow{{SessionID: "mc-1", Generation: 1}, {SessionID: "mc-2", Generation: 3}}
	if got, want := readFile(t, work.TokenFile), expectRender(t, []string{"op-a", "op-b"}, rows...); !bytes.Equal(got, want) {
		t.Fatalf("work file =\n%s\nwant\n%s", got, want)
	}
	if got, want := readFile(t, infra.TokenFile), expectRender(t, nil, rows...); !bytes.Equal(got, want) {
		t.Fatalf("infra file =\n%s\nwant\n%s", got, want)
	}
	if rep.Rows != 2 || !rep.Outputs[0].Seeded || !rep.Outputs[0].Changed {
		t.Fatalf("first report = %+v", rep)
	}
	mtime := statFile(t, work.TokenFile).ModTime()
	h.clk.Advance(time.Minute)
	rep, err = h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range rep.Outputs {
		if o.Changed || o.Seeded {
			t.Fatalf("an unchanged render rewrote %s: %+v", o.Ledger, o)
		}
	}
	if !statFile(t, work.TokenFile).ModTime().Equal(mtime) {
		t.Fatal("an unchanged render moved the mtime")
	}
}

// TestRenderOnceSeedsBeforeTheFirstRowRead models a sidecar beside the
// sessions ledger's serve: its row reads are refused until its own file holds
// the sentinel, so the seed must land first.
func TestRenderOnceSeedsBeforeTheFirstRowRead(t *testing.T) {
	work := output(t, "work", "op")
	sentinel, err := connect.Sentinel(renderKey(), testCity)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{check: func() error {
		data, err := os.ReadFile(work.TokenFile)
		if err != nil || !connect.TokenFileHoldsLine(data, sentinel) {
			return errors.New("401: the serve does not accept the sentinel yet")
		}
		return nil
	}}
	src.set(live("mc-1", 2))
	h := newHarness(t, []Output{work}, src)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got, want := readFile(t, work.TokenFile), expectRender(t, []string{"op"}, connect.TokenRow{SessionID: "mc-1", Generation: 2}); !bytes.Equal(got, want) {
		t.Fatalf("file =\n%s", got)
	}
}

func TestRenderOnceRowFailureWritesNothingBeyondSeeds(t *testing.T) {
	fresh := output(t, "work")
	rendered := output(t, "infra")
	prior := expectRender(t, nil, connect.TokenRow{SessionID: "mc-1", Generation: 1})
	if err := os.WriteFile(rendered.TokenFile, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	src.setErr(errors.New("sessions ledger unreachable"))
	h := newHarness(t, []Output{fresh, rendered}, src)
	rep, err := h.r.RenderOnce(context.Background())
	if !errors.Is(err, ErrRowsUnavailable) {
		t.Fatalf("error = %v, want render_rows_unavailable", err)
	}
	if !rep.Outputs[0].Seeded {
		t.Fatal("the fresh output was not seeded")
	}
	if got := readFile(t, fresh.TokenFile); !bytes.Equal(got, expectRender(t, nil)) {
		t.Fatalf("fresh file = %q, want the seed", got)
	}
	if got := readFile(t, rendered.TokenFile); !bytes.Equal(got, prior) {
		t.Fatalf("the rendered file changed on a row failure: %q", got)
	}
}

func TestRenderOnceUnreadableOperatorSkipsOnlyThatOutput(t *testing.T) {
	work := output(t, "work", "op")
	sentinel, _ := connect.Sentinel(renderKey(), testCity)
	prior := []byte("op\n" + sentinel + "\n" + bearerOf(t, "mc-old", 1) + "\n")
	if err := os.WriteFile(work.TokenFile, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(work.OperatorFile); err != nil {
		t.Fatal(err)
	}
	infra := output(t, "infra")
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{work, infra}, src)
	_, err := h.r.RenderOnce(context.Background())
	if !errors.Is(err, ErrOperatorUnreadable) {
		t.Fatalf("error = %v, want render_operator_unreadable", err)
	}
	if got := readFile(t, work.TokenFile); !bytes.Equal(got, prior) {
		t.Fatalf("an output with an unreadable operator source was written: %q", got)
	}
	if got, want := readFile(t, infra.TokenFile), expectRender(t, nil, connect.TokenRow{SessionID: "mc-1", Generation: 1}); !bytes.Equal(got, want) {
		t.Fatalf("the other output did not render: %q", got)
	}
	if strings.Contains(err.Error(), "op\n") {
		t.Fatal("error echoes operator content")
	}
}

func TestRenderOnceNeverOverwritesForeignOrNewerFiles(t *testing.T) {
	otherKey := bytes.Repeat([]byte{9}, 32)
	foreignSentinel, _ := connect.Sentinel(otherKey, testCity)
	otherCitySentinel, _ := connect.Sentinel(renderKey(), "other-city")
	ourSentinel, _ := connect.Sentinel(renderKey(), testCity)
	cases := []struct {
		name    string
		content string
		want    error
	}{
		{"another key", "op\n" + foreignSentinel + "\n", ErrForeignSentinel},
		{"another city", otherCitySentinel + "\n", ErrForeignSentinel},
		{"bearers without a sentinel", bearerOf(t, "mc-1", 1) + "\n", ErrForeignSentinel},
		{"a newer bearer format", ourSentinel + "\ngcb2.mc-1.1.mac\n", ErrNewerFormat},
		{"a newer sentinel format", "gcs2.whatever\n", ErrNewerFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := output(t, "work")
			if err := os.WriteFile(o.TokenFile, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			src := &fakeSource{}
			src.set(live("mc-9", 1))
			h := newHarness(t, []Output{o}, src)
			if _, err := h.r.RenderOnce(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := readFile(t, o.TokenFile); string(got) != tc.content {
				t.Fatalf("file overwritten: %q", got)
			}
			if err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-9", Generation: 1}); !errors.Is(err, tc.want) {
				t.Fatalf("Ensure = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEnsureSucceedsOnlyWhenEveryOutputHoldsTheLine(t *testing.T) {
	work, infra := output(t, "work"), output(t, "infra")
	src := &fakeSource{}
	src.set(live("mc-1", 4))
	h := newHarness(t, []Output{work, infra}, src)
	ctx := context.Background()
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 4}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	line := bearerOf(t, "mc-1", 4)
	for _, o := range []Output{work, infra} {
		if !connect.TokenFileHoldsLine(readFile(t, o.TokenFile), line) {
			t.Fatalf("%s lacks the line", o.Ledger)
		}
	}
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 3}); !errors.Is(err, ErrRowIneligible) {
		t.Fatalf("wrong generation = %v, want render_row_ineligible", err)
	}
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-absent", Generation: 1}); !errors.Is(err, ErrRowIneligible) {
		t.Fatalf("absent row = %v, want render_row_ineligible", err)
	}
	src.set(withState(live("mc-1", 4), "open", map[string]string{"state": "suspended"}))
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 4}); !errors.Is(err, ErrRowIneligible) {
		t.Fatalf("suspended row = %v, want render_row_ineligible", err)
	}
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "bad id", Generation: 1}); !errors.Is(err, ErrRowIneligible) {
		t.Fatalf("invalid row = %v, want render_row_ineligible", err)
	}

	// One output that cannot render fails the Ensure even though the other
	// holds the line.
	src.set(live("mc-1", 5))
	if err := os.WriteFile(infra.TokenFile, []byte("gcs1.foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 5})
	if !errors.Is(err, ErrForeignSentinel) {
		t.Fatalf("Ensure with one foreign output = %v, want render_foreign_sentinel", err)
	}
	if !connect.TokenFileHoldsLine(readFile(t, work.TokenFile), bearerOf(t, "mc-1", 5)) {
		t.Fatal("the healthy output did not render")
	}
}

func TestEnsureWithNoOutputsStillChecksTheRow(t *testing.T) {
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, nil, src)
	if err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-1", Generation: 1}); err != nil {
		t.Fatalf("eligible row: %v", err)
	}
	if err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-1", Generation: 2}); !errors.Is(err, ErrRowIneligible) {
		t.Fatalf("ineligible row = %v", err)
	}
	src.setErr(errors.New("down"))
	if err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-1", Generation: 1}); !errors.Is(err, ErrRowsUnavailable) {
		t.Fatalf("row failure = %v", err)
	}
}

func recvCall(t *testing.T, ch chan int) int {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a row read")
		return 0
	}
}

// TestEnsureCoalescesAndWaitsForAFreshRowRead pins Ensure's timing rule: a
// render whose row read began before the call cannot satisfy it, and every
// Ensure queued behind that render shares the next one.
func TestEnsureCoalescesAndWaitsForAFreshRowRead(t *testing.T) {
	work := output(t, "work")
	src := &fakeSource{entered: make(chan int, 16), gate: make(chan struct{})}
	h := newHarness(t, []Output{work}, src)
	ctx := context.Background()

	// Render 1 begins its row read and blocks there, before mc-new exists.
	firstDone := make(chan error, 1)
	go func() {
		_, err := h.r.RenderOnce(ctx)
		firstDone <- err
	}()
	if n := recvCall(t, src.entered); n != 1 {
		t.Fatalf("first read is call %d", n)
	}

	const callers = 8
	queued := make(chan struct{}, callers)
	h.r.onQueued = func() { queued <- struct{}{} }
	results := make(chan error, callers)
	for range callers {
		go func() { results <- h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-new", Generation: 1}) }()
	}
	for range callers {
		select {
		case <-queued:
		case <-time.After(10 * time.Second):
			t.Fatal("an Ensure call never queued")
		}
	}

	// Render 1 completes without the row; had it satisfied the queued
	// Ensure calls they would fail render_row_ineligible.
	src.gate <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("render 1: %v", err)
	}
	src.set(live("mc-new", 1))
	// Exactly one more render serves every queued Ensure.
	if n := recvCall(t, src.entered); n != 2 {
		t.Fatalf("second read is call %d", n)
	}
	src.gate <- struct{}{}
	for range callers {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an Ensure call never returned")
		}
	}
	if got := src.callCount(); got != 2 {
		t.Fatalf("%d row reads; want 2 (one render for all %d Ensure calls)", got, callers)
	}
}

func TestEnsureHonorsContext(t *testing.T) {
	src := &fakeSource{entered: make(chan int, 1), gate: make(chan struct{})}
	h := newHarness(t, []Output{output(t, "work")}, src)
	bg := make(chan struct{})
	go func() {
		defer close(bg)
		h.r.RenderOnce(context.Background()) //nolint:errcheck
	}()
	recvCall(t, src.entered)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure = %v, want the context's error", err)
	}
	src.gate <- struct{}{}
	<-bg
}

// runHarness drives Run with a controlled timer.
type runHarness struct {
	ticks    chan time.Time
	waits    chan time.Duration
	cancel   context.CancelFunc
	finished chan error
}

func startRun(t *testing.T, h *harness) *runHarness {
	t.Helper()
	rh := &runHarness{ticks: make(chan time.Time), waits: make(chan time.Duration, 64), finished: make(chan error, 1)}
	h.r.after = func(d time.Duration) <-chan time.Time {
		rh.waits <- d
		return rh.ticks
	}
	ctx, cancel := context.WithCancel(context.Background())
	rh.cancel = cancel
	go func() { rh.finished <- h.r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-rh.finished
	})
	return rh
}

// pass waits for Run to finish a render and arm its timer.
func (rh *runHarness) pass(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-rh.waits:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not complete a render")
		return 0
	}
}

func (rh *runHarness) tick() { rh.ticks <- time.Time{} }

func TestRunRevokesWithinOneInterval(t *testing.T) {
	work := output(t, "work", "op")
	src := &fakeSource{}
	rows := []beads.Bead{live("mc-closed", 1), live("mc-asleep", 1), live("mc-quarantined", 1), live("mc-regen", 1), live("mc-stays", 2)}
	src.set(rows...)
	h := newHarness(t, []Output{work}, src)
	rh := startRun(t, h)
	rh.pass(t)
	for _, b := range rows {
		gen := 1
		if b.ID == "mc-stays" {
			gen = 2
		}
		if !connect.TokenFileHoldsLine(readFile(t, work.TokenFile), bearerOf(t, b.ID, gen)) {
			t.Fatalf("%s missing after the first render", b.ID)
		}
	}

	src.set(
		withState(rows[0], "closed", nil),
		withState(rows[1], "open", map[string]string{"state": "asleep"}),
		withState(rows[2], "open", map[string]string{"state": "quarantined"}),
		withState(rows[3], "open", map[string]string{"generation": "2"}),
		rows[4],
	)
	rh.tick()
	rh.pass(t)
	got := readFile(t, work.TokenFile)
	want := expectRender(t, []string{"op"}, connect.TokenRow{SessionID: "mc-regen", Generation: 2}, connect.TokenRow{SessionID: "mc-stays", Generation: 2})
	if !bytes.Equal(got, want) {
		t.Fatalf("after one interval =\n%s\nwant\n%s", got, want)
	}
}

func TestRunPicksUpOperatorChanges(t *testing.T) {
	work := output(t, "work", "op-1")
	h := newHarness(t, []Output{work}, nil)
	rh := startRun(t, h)
	rh.pass(t)
	if err := os.WriteFile(work.OperatorFile, []byte("op-2\nop-3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rh.tick()
	rh.pass(t)
	if got, want := readFile(t, work.TokenFile), expectRender(t, []string{"op-2", "op-3"}); !bytes.Equal(got, want) {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestRunJittersTheInterval(t *testing.T) {
	h := newHarness(t, []Output{output(t, "work")}, nil)
	values := []float64{0, 0.999999, 0.5}
	i := 0
	h.r.jitter = func() float64 { v := values[i%len(values)]; i++; return v }
	rh := startRun(t, h)
	// Jitter only shortens the interval, so revocation never waits longer
	// than one Interval (DES-CN §6).
	lo, hi := DefaultInterval*9/10, DefaultInterval
	var seen []time.Duration
	for range values {
		d := rh.pass(t)
		if d < lo || d > hi {
			t.Fatalf("interval %s outside [%s, %s]", d, lo, hi)
		}
		seen = append(seen, d)
		rh.tick()
	}
	if seen[0] != DefaultInterval || seen[2] != DefaultInterval*19/20 || seen[1] <= lo {
		t.Fatalf("intervals = %v", seen)
	}
}

func TestRunLogsEachCauseOnceUntilItClears(t *testing.T) {
	work := output(t, "work", "op", "gcb1.smuggled.1.mac")
	src := &fakeSource{}
	src.setErr(errors.New("sessions ledger down"))
	h := newHarness(t, []Output{work}, src)
	rh := startRun(t, h)
	for range 3 {
		rh.pass(t)
		rh.tick()
	}
	rh.pass(t)
	if n := h.logs.count(string(CauseRowsUnavailable)); n != 1 {
		t.Fatalf("rows cause logged %d times over 4 renders, want 1:\n%s", n, strings.Join(h.logs.lines, "\n"))
	}
	if n := h.logs.count("gc-like"); n != 1 {
		t.Fatalf("gc-like operator line logged %d times, want 1", n)
	}
	src.setErr(nil)
	rh.tick()
	rh.pass(t)
	if n := h.logs.count("session rows, city maintainer-city): cleared"); n != 1 {
		t.Fatalf("recovery logged %d times:\n%s", n, strings.Join(h.logs.lines, "\n"))
	}
	for _, line := range h.logs.lines {
		if strings.Contains(line, "smuggled") {
			t.Fatalf("a log line echoes an operator token: %s", line)
		}
	}
}

// TestAPersistentWriteFailureIsLoggedOnce pins the log-once rule across
// repeated write failures: the cause names no temp file, whose random name
// would make each render's failure a new cause.
func TestAPersistentWriteFailureIsLoggedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	work := output(t, "work")
	src := &fakeSource{}
	h := newHarness(t, []Output{work}, src)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatalf("seeding render: %v", err)
	}
	dir := filepath.Dir(work.TokenFile)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	src.set(live("mc-1", 1))
	for range 3 {
		rep, err := h.r.RenderOnce(context.Background())
		if err == nil || !errors.Is(rep.Outputs[0].Err, os.ErrPermission) {
			t.Fatalf("render into a read-only directory = %v (output: %v), want a permission failure", err, rep.Outputs[0].Err)
		}
	}
	if n := h.logs.count("(ledger work, city maintainer-city)"); n != 1 {
		t.Fatalf("a persistent write failure logged %d times over 3 renders, want 1:\n%s", n, strings.Join(h.logs.lines, "\n"))
	}
	for _, line := range h.logs.lines {
		if strings.Contains(line, ".tmp-") {
			t.Fatalf("a log line names a temp file: %s", line)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatalf("render into the writable directory: %v", err)
	}
	if n := h.logs.count("(ledger work, city maintainer-city): cleared"); n != 1 {
		t.Fatalf("recovery logged %d times:\n%s", n, strings.Join(h.logs.lines, "\n"))
	}
}

func TestRunReturnsWhenTheContextEnds(t *testing.T) {
	h := newHarness(t, []Output{output(t, "work")}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestControllerAndSidecarRenderIdenticalGCLines pins D15's convergence: two
// renderers over the same rows, beside different serves with different
// operator sources, write the same gc lines.
func TestControllerAndSidecarRenderIdenticalGCLines(t *testing.T) {
	src := &fakeSource{}
	src.set(live("mc-1", 1), live("mc-2", 7), withState(live("mc-3", 1), "closed", nil))
	controllerOut := output(t, "work", "ctl-op-1", "ctl-op-2")
	sidecarOut := output(t, "infra", "infra-op")
	controller := newHarness(t, []Output{controllerOut}, src)
	sidecar := newHarness(t, []Output{sidecarOut}, src)
	for _, h := range []*harness{controller, sidecar} {
		if _, err := h.r.RenderOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a, b := gcLines(readFile(t, controllerOut.TokenFile)), gcLines(readFile(t, sidecarOut.TokenFile))
	if !slices.Equal(a, b) || len(a) != 3 {
		t.Fatalf("gc lines differ:\n%q\n%q", a, b)
	}
}

// TestRenderersSharingADirectorySerialize runs two renderers over one
// directory concurrently; both converge on the same bytes.
func TestRenderersSharingADirectorySerialize(t *testing.T) {
	dir := t.TempDir()
	o := Output{Ledger: "work", TokenFile: filepath.Join(dir, "serve-tokens")}
	src := &fakeSource{}
	src.set(live("mc-1", 1), live("mc-2", 1))
	a, b := newHarness(t, []Output{o}, src), newHarness(t, []Output{o}, src)
	var wg sync.WaitGroup
	for _, h := range []*harness{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := h.r.RenderOnce(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	want := expectRender(t, nil, connect.TokenRow{SessionID: "mc-1", Generation: 1}, connect.TokenRow{SessionID: "mc-2", Generation: 1})
	if got := readFile(t, o.TokenFile); !bytes.Equal(got, want) {
		t.Fatalf("file = %q", got)
	}
}

func TestRenderTooLargeIsTyped(t *testing.T) {
	big := strings.Repeat("x", connect.MaxTokenFileBytes-100)
	o := output(t, "work", big)
	src := &fakeSource{}
	src.set(live("mc-1", 1), live("mc-2", 1))
	h := newHarness(t, []Output{o}, src)
	before, _ := os.ReadFile(o.TokenFile)
	_, err := h.r.RenderOnce(context.Background())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want render_too_large", err)
	}
	// The seed fit, the full render did not: the seed stays.
	after := readFile(t, o.TokenFile)
	if bytes.Equal(before, after) || len(gcLines(after)) != 1 {
		t.Fatalf("file holds %d gc lines; want the seed's sentinel only", len(gcLines(after)))
	}
}

func TestDroppedRowsAreCountedAndDoNotBlockOthers(t *testing.T) {
	o := output(t, "work")
	h := newHarness(t, []Output{o}, nil)
	h.r.cfg.Rows = rowsFunc(func() []connect.TokenRow {
		return []connect.TokenRow{{SessionID: "mc-1", Generation: 1}, {SessionID: "bad id", Generation: 1}, {SessionID: "mc-2", Generation: 0}, {SessionID: "mc-1", Generation: 1}}
	})
	rep, err := h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 1 || rep.DroppedRows != 2 {
		t.Fatalf("report rows = %d dropped = %d", rep.Rows, rep.DroppedRows)
	}
	if h.logs.count("dropped 2 rows") != 1 {
		t.Fatalf("dropped rows not logged: %q", h.logs.lines)
	}
}

type rowsFunc func() []connect.TokenRow

func (f rowsFunc) EligibleRows(context.Context) ([]connect.TokenRow, error) { return f(), nil }

func TestOutputsHoldGCLines(t *testing.T) {
	a, b := output(t, "work"), output(t, "infra")
	if held, err := OutputsHoldGCLines([]Output{a, b}); err != nil || held {
		t.Fatalf("absent files = %v, %v", held, err)
	}
	if err := os.WriteFile(a.TokenFile, []byte("operator-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if held, err := OutputsHoldGCLines([]Output{a, b}); err != nil || held {
		t.Fatalf("operator-only = %v, %v", held, err)
	}
	if err := os.WriteFile(b.TokenFile, []byte("gcs1.anything\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if held, err := OutputsHoldGCLines([]Output{a, b}); err != nil || !held {
		t.Fatalf("gc line = %v, %v", held, err)
	}
	if err := os.Remove(b.TokenFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.TokenFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OutputsHoldGCLines([]Output{b}); err == nil {
		t.Fatal("an unreadable token file reported no gc line")
	}
}

func TestErrorIsMatchesItsCauseOnly(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &Error{Cause: CauseForeignSentinel, Ledger: "work", Detail: "d"})
	if !errors.Is(err, ErrForeignSentinel) || errors.Is(err, ErrNewerFormat) {
		t.Fatal("errors.Is mismatch")
	}
	if c, ok := CauseOf(err); !ok || c != CauseForeignSentinel {
		t.Fatalf("CauseOf = %q, %v", c, ok)
	}
	seen := map[Cause]bool{}
	for _, c := range Causes() {
		if seen[c] || !strings.HasPrefix(string(c), "render_") {
			t.Fatalf("cause %q duplicated or unprefixed", c)
		}
		seen[c] = true
	}
	if len(seen) != 8 {
		t.Fatalf("%d causes, want DES-CN §3.8's 8", len(seen))
	}
}

// renderedWith writes a file this renderer would have rendered from operator
// and rows, and returns its bytes.
func renderedWith(t *testing.T, path string, operator []string, rows ...connect.TokenRow) []byte {
	t.Helper()
	content := expectRender(t, operator, rows...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return content
}

// TestRenderOnceRefusesAnIncompleteOperatorSource pins bd-enterprise's
// completeness rule across the copy: a source caught mid-write by a
// non-atomic writer leaves the served file as it is, so a fragment never
// becomes a token and the operator's real token is never dropped.
func TestRenderOnceRefusesAnIncompleteOperatorSource(t *testing.T) {
	for name, partial := range map[string]string{
		"cut mid-token":      "rot",
		"truncated to empty": "",
		"whitespace only":    "\n",
	} {
		t.Run(name, func(t *testing.T) {
			work := output(t, "work", "rotated-token-old")
			src := &fakeSource{}
			src.set(live("mc-1", 1))
			prior := renderedWith(t, work.TokenFile, []string{"rotated-token-old"}, connect.TokenRow{SessionID: "mc-1", Generation: 1})
			if err := os.WriteFile(work.OperatorFile, []byte(partial), 0o600); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, []Output{work}, src)
			_, err := h.r.RenderOnce(context.Background())
			if !errors.Is(err, ErrOperatorUnreadable) {
				t.Fatalf("error = %v, want render_operator_unreadable", err)
			}
			if got := readFile(t, work.TokenFile); !bytes.Equal(got, prior) {
				t.Fatalf("an incomplete operator source changed the served file: %q", got)
			}
		})
	}
}

// TestOperatorLinesWithAnInteriorCarriageReturnRender pins that every line
// ParseOperatorLines returns renders: bd splits only on "\n", so an interior
// "\r" is part of the token and is copied as it is.
func TestOperatorLinesWithAnInteriorCarriageReturnRender(t *testing.T) {
	work := output(t, "work", "tokA\rtokB")
	h := newHarness(t, []Output{work}, nil)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatalf("RenderOnce: %v", err)
	}
	if !connect.TokenFileHoldsLine(readFile(t, work.TokenFile), "tokA\rtokB") {
		t.Fatalf("file = %q, want the operator line copied", readFile(t, work.TokenFile))
	}
}

func TestRenderOnceSkipsANonRegularOperatorSource(t *testing.T) {
	work := output(t, "work", "op")
	if err := os.Remove(work.OperatorFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(work.OperatorFile, 0o600); err != nil {
		t.Fatal(err)
	}
	infra := output(t, "infra")
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{work, infra}, src)
	done := make(chan error, 1)
	go func() {
		_, err := h.r.RenderOnce(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrOperatorUnreadable) {
			t.Fatalf("error = %v, want render_operator_unreadable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a FIFO operator source blocked the render")
	}
	if !connect.TokenFileHoldsLine(readFile(t, infra.TokenFile), bearerOf(t, "mc-1", 1)) {
		t.Fatal("the other output did not render")
	}
}

// TestRenderHonorsContextWhileADirectoryIsLocked models a CLI renderer
// stopped while it holds the directory lock: Ensure must return at its
// deadline and Run at its cancellation, not block on the lock.
func TestRenderHonorsContextWhileADirectoryIsLocked(t *testing.T) {
	work := output(t, "work")
	release := holdDirLock(t, filepath.Dir(work.TokenFile))
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{work}, src)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.r.Ensure(ctx, connect.TokenRow{SessionID: "mc-1", Generation: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Ensure returned %s after its deadline", elapsed)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	busy := make(chan struct{}, 1)
	h.r.onLockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.r.Run(runCtx) }()
	select {
	case <-busy:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never waited on the lock")
	}
	runCancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return while the lock was held")
	}

	// Once the lock is free the same renderer renders.
	release()
	h.r.onLockBusy = nil
	if err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-1", Generation: 1}); err != nil {
		t.Fatalf("Ensure after release: %v", err)
	}
}

// TestOneUnlockableDirectoryDoesNotStopTheOthers pins per-output isolation
// at the lock: an output whose directory cannot be locked fails alone, and
// every other output still revokes and ensures.
func TestOneUnlockableDirectoryDoesNotStopTheOthers(t *testing.T) {
	missing := Output{Ledger: "infra", TokenFile: filepath.Join(t.TempDir(), "not-provisioned", "serve-tokens")}
	work := output(t, "work")
	renderedWith(t, work.TokenFile, nil, connect.TokenRow{SessionID: "mc-revoked", Generation: 1})
	src := &fakeSource{}
	src.set(live("mc-new", 1))
	h := newHarness(t, []Output{missing, work}, src)
	err := h.r.Ensure(context.Background(), connect.TokenRow{SessionID: "mc-new", Generation: 1})
	if !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("Ensure = %v, want the missing directory's render_write_failed", err)
	}
	if !strings.Contains(err.Error(), "ledger infra") || strings.Contains(err.Error(), "ledger work") {
		t.Fatalf("the failure is not attributed to the missing output alone: %v", err)
	}
	got := readFile(t, work.TokenFile)
	if connect.TokenFileHoldsLine(got, bearerOf(t, "mc-revoked", 1)) {
		t.Fatal("the healthy output kept a revoked bearer")
	}
	if !connect.TokenFileHoldsLine(got, bearerOf(t, "mc-new", 1)) {
		t.Fatal("the healthy output lacks the ensured bearer")
	}
}

// TestRenderHoldsEveryDirectoryLockAcrossTheRowRead pins the render's own
// use of the lock: another open file description cannot take any output's
// directory lock while the rows are read.
func TestRenderHoldsEveryDirectoryLockAcrossTheRowRead(t *testing.T) {
	work, infra := output(t, "work"), output(t, "infra")
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	src.check = func() error {
		for _, o := range []Output{work, infra} {
			if !dirLocked(t, filepath.Dir(o.TokenFile)) {
				return fmt.Errorf("%s's directory is not locked during the row read", o.Ledger)
			}
		}
		return nil
	}
	h := newHarness(t, []Output{work, infra}, src)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatalf("RenderOnce: %v", err)
	}
	for _, o := range []Output{work, infra} {
		if dirLocked(t, filepath.Dir(o.TokenFile)) {
			t.Fatalf("%s's directory is still locked after the render", o.Ledger)
		}
	}
}

// TestAStaleRenderNeverLandsAfterANewerOne models a CLI renderer whose row
// read predates a revocation racing another renderer of the same file: the
// directory lock makes the later row read write last, so the revoked bearer
// does not come back.
func TestAStaleRenderNeverLandsAfterANewerOne(t *testing.T) {
	dir := t.TempDir()
	o := Output{Ledger: "work", TokenFile: filepath.Join(dir, "serve-tokens")}
	stale := &fakeSource{entered: make(chan int, 1), gate: make(chan struct{})}
	stale.set(live("mc-x", 1))
	a := newHarness(t, []Output{o}, stale)
	fresh := &fakeSource{}
	fresh.set() // mc-x revoked
	b := newHarness(t, []Output{o}, fresh)

	aDone := make(chan error, 1)
	go func() {
		_, err := a.r.RenderOnce(context.Background())
		aDone <- err
	}()
	recvCall(t, stale.entered) // A has read rows holding mc-x and waits

	busy := make(chan struct{}, 1)
	b.r.onLockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	bDone := make(chan error, 1)
	go func() {
		_, err := b.r.RenderOnce(context.Background())
		bDone <- err
	}()
	select {
	case <-busy: // B waits for A's lock, as it must
	case err := <-bDone:
		t.Fatalf("B rendered while A held its directory lock (err %v)", err)
	case <-time.After(10 * time.Second):
		t.Fatal("B neither waited nor rendered")
	}
	stale.gate <- struct{}{}
	for _, ch := range []chan error{aDone, bDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a render never finished")
		}
	}
	if connect.TokenFileHoldsLine(readFile(t, o.TokenFile), bearerOf(t, "mc-x", 1)) {
		t.Fatal("the stale render landed after the newer one: the revoked bearer is back")
	}
}

func TestRenderOnceWithADirectoryReachedTwoWays(t *testing.T) {
	realDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	a := Output{Ledger: "work", TokenFile: filepath.Join(realDir, "work-tokens")}
	b := Output{Ledger: "infra", TokenFile: filepath.Join(alias, "infra-tokens")}
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{a, b}, src)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.r.RenderOnce(ctx); err != nil {
		t.Fatalf("RenderOnce over one directory reached two ways: %v", err)
	}
	for _, o := range []Output{a, b} {
		if !connect.TokenFileHoldsLine(readFile(t, o.TokenFile), bearerOf(t, "mc-1", 1)) {
			t.Fatalf("%s did not render", o.Ledger)
		}
	}
}

// TestSeedNeverDropsContentItDoesNotOwn pins the tightened seed rule: a
// file with no gc line is seeded over only when that drops nothing, so a
// mis-pointed path or an operator file not yet split into a source is left
// as it is.
func TestSeedNeverDropsContentItDoesNotOwn(t *testing.T) {
	t.Run("larger than the cap", func(t *testing.T) {
		o := output(t, "work")
		big := bytes.Repeat([]byte("not a token file\n"), 3<<20/17)
		if err := os.WriteFile(o.TokenFile, big, 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, []Output{o}, nil)
		if _, err := h.r.RenderOnce(context.Background()); !errors.Is(err, ErrSeedWouldDrop) {
			t.Fatalf("error = %v, want render_seed_would_drop", err)
		}
		if got := readFile(t, o.TokenFile); !bytes.Equal(got, big) {
			t.Fatal("a mis-pointed file was overwritten")
		}
	})
	t.Run("larger than the cap, with no token line", func(t *testing.T) {
		o := output(t, "work")
		blank := bytes.Repeat([]byte("\n"), connect.MaxTokenFileBytes+1)
		if err := os.WriteFile(o.TokenFile, blank, 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, []Output{o}, nil)
		if _, err := h.r.RenderOnce(context.Background()); !errors.Is(err, ErrSeedWouldDrop) {
			t.Fatalf("error = %v, want render_seed_would_drop", err)
		}
		if got := readFile(t, o.TokenFile); !bytes.Equal(got, blank) {
			t.Fatal("a mis-pointed file was overwritten")
		}
	})
	t.Run("operator tokens without a source", func(t *testing.T) {
		o := output(t, "work")
		content := []byte("ops-token-1\nops-token-2\n")
		if err := os.WriteFile(o.TokenFile, content, 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, []Output{o}, nil)
		_, err := h.r.RenderOnce(context.Background())
		if !errors.Is(err, ErrSeedWouldDrop) {
			t.Fatalf("error = %v, want render_seed_would_drop", err)
		}
		if !strings.Contains(err.Error(), "2 ") || strings.Contains(err.Error(), "ops-token") {
			t.Fatalf("the error must name a count and never a line: %v", err)
		}
		if got := readFile(t, o.TokenFile); !bytes.Equal(got, content) {
			t.Fatal("operator tokens were dropped")
		}
	})
	t.Run("operator tokens partly in the source", func(t *testing.T) {
		o := output(t, "work", "ops-token-1")
		content := []byte("ops-token-1\nops-token-2\n")
		if err := os.WriteFile(o.TokenFile, content, 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, []Output{o}, nil)
		if _, err := h.r.RenderOnce(context.Background()); !errors.Is(err, ErrSeedWouldDrop) {
			t.Fatalf("error = %v, want render_seed_would_drop", err)
		}
	})
	t.Run("operator tokens all in the source", func(t *testing.T) {
		o := output(t, "work", "ops-token-2", "ops-token-1", "ops-token-3")
		if err := os.WriteFile(o.TokenFile, []byte("ops-token-1\nops-token-2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, []Output{o}, nil)
		rep, err := h.r.RenderOnce(context.Background())
		if err != nil || !rep.Outputs[0].Seeded {
			t.Fatalf("RenderOnce = %+v, %v; want the seed", rep.Outputs, err)
		}
	})
}

// TestReportsAndRenderersPrintNoSecret pins that ordinary fmt verbs on the
// values a host logs never print the key, the sentinel or a bearer.
func TestReportsAndRenderersPrintNoSecret(t *testing.T) {
	work := output(t, "work", "op-secret")
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{work}, src)
	rep, err := h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sentinel, _ := connect.Sentinel(renderKey(), testCity)
	bearer := bearerOf(t, "mc-1", 1)
	secrets := []string{
		sentinel[len(connect.SentinelPrefix):],
		bearer[strings.LastIndex(bearer, ".")+1:],
		"op-secret",
		fmt.Sprintf("%v", renderKey()),
		fmt.Sprintf("%#v", renderKey()),
	}
	// A digest of an operator token is a secret too: a weak one can be
	// guessed offline from a logged report.
	opDigest := sha256.Sum256([]byte("op-secret"))
	for _, verb := range []string{"%v", "%#v", "%x", "%s"} {
		secrets = append(secrets, strings.Trim(fmt.Sprintf(verb, opDigest), "[]{}"))
	}
	cfg := Config{City: testCity, Key: renderKey(), Outputs: []Output{work}, Rows: src}
	values := map[string]any{"report": rep, "output report": rep.Outputs[0], "renderer": h.r, "config": cfg}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(verb, v)
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Fatalf("%s of the %s prints a secret: %s", verb, name, out)
				}
			}
		}
	}
}

// TestACoalescedEnsureSurvivesAnotherCallersDeadline pins that a queued
// Ensure with a healthy context is not failed by a shared render that lost
// only to another caller's deadline.
func TestACoalescedEnsureSurvivesAnotherCallersDeadline(t *testing.T) {
	work := output(t, "work")
	src := &fakeSource{entered: make(chan int, 8)}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{work}, src)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	recvCall(t, src.entered)

	// A's render waits on the directory lock, holding the render slot.
	release := holdDirLock(t, filepath.Dir(work.TokenFile))
	busy := make(chan struct{}, 1)
	h.r.onLockBusy = func() {
		select {
		case busy <- struct{}{}:
		default:
		}
	}
	queued := make(chan struct{}, 2)
	h.r.onQueued = func() { queued <- struct{}{} }
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	src.hook = func(ctx context.Context, n int) error {
		if n == 2 { // A's row read: it outlives A's context
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	row := connect.TokenRow{SessionID: "mc-1", Generation: 1}
	aDone := make(chan error, 1)
	go func() { aDone <- h.r.Ensure(ctxA, row) }()
	<-queued
	select {
	case <-busy:
	case <-time.After(10 * time.Second):
		t.Fatal("A never waited on the lock")
	}
	// B queues behind A before A's row read begins, so A's render could
	// satisfy it.
	bDone := make(chan error, 1)
	go func() { bDone <- h.r.Ensure(context.Background(), row) }()
	<-queued
	release()
	if n := recvCall(t, src.entered); n != 2 {
		t.Fatalf("A's read is call %d", n)
	}
	cancelA()
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("A = %v, want its own cancellation", err)
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B failed with A's cancellation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never returned")
	}
}

// TestRunSchedulesFromTheRenderStart pins that a slow render does not
// stretch the period: the wait is the interval less the render's duration.
func TestRunSchedulesFromTheRenderStart(t *testing.T) {
	src := &fakeSource{}
	h := newHarness(t, []Output{output(t, "work")}, src)
	src.hook = func(context.Context, int) error {
		h.clk.Advance(4 * time.Second)
		return nil
	}
	h.r.jitter = func() float64 { return 0 }
	rh := startRun(t, h)
	if d := rh.pass(t); d != DefaultInterval-4*time.Second {
		t.Fatalf("wait = %s, want the interval less the 4 s render", d)
	}
	src.hook = func(context.Context, int) error {
		h.clk.Advance(time.Minute)
		return nil
	}
	rh.tick()
	if d := rh.pass(t); d != 0 {
		t.Fatalf("wait after a render longer than the interval = %s, want 0", d)
	}
}

func TestRenderRemovesStaleTempFiles(t *testing.T) {
	o := output(t, "work")
	dir := filepath.Dir(o.TokenFile)
	stale := filepath.Join(dir, ".serve-tokens.tmp-123456")
	unrelated := []string{filepath.Join(dir, ".serve-tokens.tmp-notours"), filepath.Join(dir, ".other.tmp-1")}
	for _, p := range append([]string{stale}, unrelated...) {
		if err := os.WriteFile(p, []byte("gcs1.leaked\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, []Output{o}, nil)
	if _, err := h.r.RenderOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stale temp file survived the render: %v", err)
	}
	for _, p := range unrelated {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a file that is not the renderer's temp file was removed: %v", err)
		}
	}
}

func TestInvalidSessionIDsFromASourceAreCounted(t *testing.T) {
	o := output(t, "work")
	src := &fakeSource{}
	src.set(live("mc-1", 1), live("bad id", 1))
	h := newHarness(t, []Output{o}, src)
	rep, err := h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 1 || rep.DroppedRows != 1 {
		t.Fatalf("rows = %d dropped = %d; want the invalid id counted", rep.Rows, rep.DroppedRows)
	}
}

// TestALockHeldElsewhereFailsOnlyItsDirectory pins the lock budget: a
// directory whose lock another renderer holds (a stuck or stopped CLI mint)
// fails only its own outputs, with a logged cause, and every other output
// still revokes within the render.
func TestALockHeldElsewhereFailsOnlyItsDirectory(t *testing.T) {
	infra, work := output(t, "infra"), output(t, "work")
	renderedWith(t, work.TokenFile, nil, connect.TokenRow{SessionID: "mc-revoked", Generation: 1})
	holdDirLock(t, filepath.Dir(infra.TokenFile))
	src := &fakeSource{}
	src.set(live("mc-new", 1))
	h := newHarness(t, []Output{infra, work}, src)
	h.r.lockBudget = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := h.r.RenderOnce(ctx)
	if ctx.Err() != nil {
		t.Fatalf("the render waited out its context on a held lock: %v", err)
	}
	if !errors.Is(err, ErrWriteFailed) || !errors.Is(rep.Outputs[0].Err, errDirLockHeld) {
		t.Fatalf("RenderOnce = %v (infra: %v), want infra's render_write_failed for a held lock", err, rep.Outputs[0].Err)
	}
	if rep.Outputs[1].Err != nil {
		t.Fatalf("the free output failed: %v", rep.Outputs[1].Err)
	}
	got := readFile(t, work.TokenFile)
	if connect.TokenFileHoldsLine(got, bearerOf(t, "mc-revoked", 1)) || !connect.TokenFileHoldsLine(got, bearerOf(t, "mc-new", 1)) {
		t.Fatal("the free output did not render")
	}
	if h.logs.count("ledger infra") != 1 || h.logs.count("held by another renderer") != 1 {
		t.Fatalf("the held lock was not logged once: %q", h.logs.lines)
	}
}

// TestLockBudgetDefaultsWellUnderTheInterval pins that the default budget
// leaves most of the interval for the render itself.
func TestLockBudgetDefaultsWellUnderTheInterval(t *testing.T) {
	h := newHarness(t, nil, nil)
	if b := h.r.lockBudget; b <= 0 || b > DefaultInterval/2 {
		t.Fatalf("lock budget = %s, want positive and well under %s", b, DefaultInterval)
	}
}

// TestRowFailureMarksEveryWritableOutput pins the per-output cause of a
// failed row read: an output that was not otherwise failed reports
// render_rows_unavailable rather than looking current.
func TestRowFailureMarksEveryWritableOutput(t *testing.T) {
	work, infra := output(t, "work"), output(t, "infra")
	src := &fakeSource{}
	src.setErr(errors.New("sessions ledger unreachable"))
	h := newHarness(t, []Output{work, infra}, src)
	rep, _ := h.r.RenderOnce(context.Background())
	for _, o := range rep.Outputs {
		if !errors.Is(o.Err, ErrRowsUnavailable) {
			t.Fatalf("output %s Err = %v, want render_rows_unavailable", o.Ledger, o.Err)
		}
	}
}

// TestRenderOnceRefusesAFIFOParentWithoutBlocking pins that a FIFO where a
// token file's directory should be fails that output at once: opening it as
// a directory must never wait for a writer.
func TestRenderOnceRefusesAFIFOParentWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "not-a-dir")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Unblock a reader stuck in open(2), should the test have failed.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close() //nolint:errcheck // test cleanup
		}
	})
	bad := Output{Ledger: "infra", TokenFile: filepath.Join(fifo, "serve-tokens")}
	work := output(t, "work")
	h := newHarness(t, []Output{bad, work}, nil)
	done := make(chan Report, 1)
	go func() {
		rep, _ := h.r.RenderOnce(context.Background())
		done <- rep
	}()
	select {
	case rep := <-done:
		if !errors.Is(rep.Outputs[0].Err, ErrWriteFailed) || rep.Outputs[1].Err != nil {
			t.Fatalf("outputs = %+v, want only the FIFO's render_write_failed", rep.Outputs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RenderOnce blocked opening a FIFO as a directory")
	}
	unlock, err := LockOutputs(context.Background(), []Output{work})
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

// TestRenderOnceReturnsAReportItDoesNotShare pins that a host editing the
// report RenderOnce returns cannot change the report a later coalesced
// Ensure checks.
func TestRenderOnceReturnsAReportItDoesNotShare(t *testing.T) {
	src := &fakeSource{}
	src.set(live("mc-1", 1))
	h := newHarness(t, []Output{output(t, "work")}, src)
	rep, err := h.r.RenderOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep.Outputs[0].Err = errors.New("edited by the host")
	h.r.mu.Lock()
	last := h.r.last
	h.r.mu.Unlock()
	if last.Outputs[0].Err != nil {
		t.Fatalf("editing the returned report changed the renderer's: %v", last.Outputs[0].Err)
	}
}
