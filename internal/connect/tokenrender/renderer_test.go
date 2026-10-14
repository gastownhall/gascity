package tokenrender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
}

func (f *fakeSource) EligibleRows(context.Context) ([]connect.TokenRow, error) {
	f.mu.Lock()
	f.calls++
	n, entered, gate, check := f.calls, f.entered, f.gate, f.check
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
	lo, hi := DefaultInterval*9/10, DefaultInterval*11/10
	var seen []time.Duration
	for range values {
		d := rh.pass(t)
		if d < lo || d > hi {
			t.Fatalf("interval %s outside [%s, %s]", d, lo, hi)
		}
		seen = append(seen, d)
		rh.tick()
	}
	if seen[0] != lo || seen[2] != DefaultInterval {
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
	if len(seen) != 7 {
		t.Fatalf("%d causes, want DES-CN §3.8's 7", len(seen))
	}
}
