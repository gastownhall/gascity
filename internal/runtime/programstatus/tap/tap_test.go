package tap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

const testNonce = "r4nd.%3"

// waitLimit bounds every wait on the engine's goroutines.
const waitLimit = 30 * time.Second

// report frames body as a BEL-terminated OSC 7501 report.
func report(body string) string { return "\x1b]7501;" + body + "\a" }

// call is one Publish the fake saw.
type call struct {
	w     Write
	reply chan error // nil when the fake answers by itself
}

// fakePublisher records every Publish. With manual set, each call waits for
// the test to answer on call.reply; otherwise it succeeds at once.
type fakePublisher struct {
	manual bool
	calls  chan call
	mu     sync.Mutex
	writes []Write
}

func newFake(manual bool) *fakePublisher {
	return &fakePublisher{manual: manual, calls: make(chan call, 1024)}
}

func (f *fakePublisher) Publish(_ context.Context, w Write) error {
	f.mu.Lock()
	f.writes = append(f.writes, w)
	f.mu.Unlock()
	if !f.manual {
		f.calls <- call{w: w}
		return nil
	}
	c := call{w: w, reply: make(chan error)}
	f.calls <- c
	return <-c.reply
}

func (f *fakePublisher) all() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Write(nil), f.writes...)
}

func (f *fakePublisher) next(t *testing.T) call {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(waitLimit):
		t.Fatal("no publish arrived")
		return call{}
	}
}

// decode returns the status a write carries.
func decode(t *testing.T, w Write) runtime.ProgramStatus {
	t.Helper()
	st, err := programstatus.Decode(w.Status)
	if err != nil {
		t.Fatalf("Decode(%q): %v", w.Status, err)
	}
	return st
}

// stepReader hands the engine one chunk per Read, and signals each Read
// entry on entered: a Read entry proves the previous chunk was processed.
type stepReader struct {
	chunks  chan string
	entered chan struct{}
}

func newStepReader() *stepReader {
	return &stepReader{chunks: make(chan string), entered: make(chan struct{}, 1024)}
}

func (r *stepReader) Read(p []byte) (int, error) {
	r.entered <- struct{}{}
	s, ok := <-r.chunks
	if !ok {
		return 0, io.EOF
	}
	return copy(p, s), nil
}

// feed hands s to the engine and returns once the engine has processed it.
func (r *stepReader) feed(t *testing.T, s string) {
	t.Helper()
	r.awaitRead(t)
	select {
	case r.chunks <- s:
	case <-time.After(waitLimit):
		t.Fatal("engine stopped reading")
	}
}

// settle returns once the engine has processed every fed chunk.
func (r *stepReader) settle(t *testing.T) {
	t.Helper()
	r.awaitRead(t)
	r.entered <- struct{}{} // the engine is parked in Read; keep the token
}

func (r *stepReader) awaitRead(t *testing.T) {
	t.Helper()
	select {
	case <-r.entered:
	case <-time.After(waitLimit):
		t.Fatal("engine never entered Read")
	}
}

// fastConfig is the default policy with the waits a unit test can afford.
func fastConfig() config {
	c := defaultConfig()
	c.minInterval = time.Millisecond
	c.maxInterval = 8 * time.Millisecond
	c.retryMin = time.Millisecond
	c.retryMax = 4 * time.Millisecond
	return c
}

// start runs the engine in the background and returns its result channel.
func start(r io.Reader, pub Publisher, opts Options, cfg config) <-chan error {
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), r, pub, opts, cfg) }()
	return done
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(waitLimit):
		t.Fatal("tap did not exit")
		return nil
	}
}

func TestTapPublishesRootRecord(t *testing.T) {
	pub := newFake(false)
	in := strings.NewReader(report("state=working:app=gen:msg=aGk="))
	if err := result(t, start(in, pub, Options{Nonce: testNonce}, fastConfig())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	writes := pub.all()
	if len(writes) != 1 {
		t.Fatalf("got %d writes, want 1: %+v", len(writes), writes)
	}
	st := decode(t, writes[0])
	if st.Nonce != testNonce || st.State != runtime.ProgramWorking || st.App != "gen" || st.TurnStarts != 1 {
		t.Fatalf("published %+v", st)
	}
	_, text, err := programstatus.DecodeText(writes[0].Text)
	if err != nil || text.Msg != "hi" {
		t.Fatalf("text %q: %+v, %v", writes[0].Text, text, err)
	}
}

func TestTapOmitsUnchangedText(t *testing.T) {
	pub := newFake(false)
	r := newStepReader()
	done := start(r, pub, Options{Nonce: testNonce}, fastConfig())
	r.feed(t, report("state=working:msg=aGk="))
	if w := pub.next(t).w; w.Text == "" {
		t.Fatal("first write omitted changed text")
	}
	r.feed(t, report("state=blocked:msg=aGk="))
	if w := pub.next(t).w; w.Text != "" || decode(t, w).State != runtime.ProgramBlocked {
		t.Fatalf("second write %+v: want the status alone", w)
	}
	r.feed(t, report("state=blocked:msg=eW8="))
	if w := pub.next(t).w; w.Text == "" {
		t.Fatal("third write omitted changed text")
	}
	close(r.chunks)
	if err := result(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestTapKeepsDrainingWhileWriterBlocked(t *testing.T) {
	const total = 80 << 20
	pub := newFake(true)
	cfg := fastConfig()
	cfg.writeTimeout = time.Hour // the stalled tmux client never times out here
	pr, pw := io.Pipe()
	done := start(pr, pub, Options{Nonce: testNonce}, cfg)

	// One 32 KiB block of pane output: two state changes in screen noise.
	var block bytes.Buffer
	block.WriteString(report("state=working:app=gen"))
	for block.Len() < 32<<10-64 {
		block.WriteString("compiling package ... ok \x1b[32mPASS\x1b[0m\r\n")
	}
	block.WriteString(report("state=idle:app=gen"))
	chunk := block.Bytes()

	fed := make(chan error, 1)
	go func() {
		n := 0
		for n < total {
			m, err := pw.Write(chunk)
			n += m
			if err != nil {
				fed <- err
				return
			}
		}
		fed <- nil
	}()

	first := pub.next(t) // the writer is now stuck inside this publish
	select {
	case err := <-fed:
		if err != nil {
			t.Fatalf("feeding: %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("reader stopped draining while the writer was blocked")
	}

	if _, err := pw.Write([]byte(report("state=done:app=gen"))); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	first.reply <- nil
	// Every later publish carries the latest record.
	var last Write
	for {
		select {
		case c := <-pub.calls:
			last = c.w
			c.reply <- nil
			continue
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(waitLimit):
			t.Fatal("tap did not exit")
		}
		break
	}
	if n := len(pub.all()); n > 3 {
		t.Fatalf("%d publishes while blocked; want the backlog coalesced", n)
	}
	if st := decode(t, last); st.State != runtime.ProgramDone {
		t.Fatalf("final publish %+v, want done", st)
	}
}

func TestTapRetriesFailedWriteLatestWins(t *testing.T) {
	pub := newFake(true)
	r := newStepReader()
	done := start(r, pub, Options{Nonce: testNonce}, fastConfig())

	r.feed(t, report("state=working"))
	c1 := pub.next(t)
	if st := decode(t, c1.w); st.State != runtime.ProgramWorking {
		t.Fatalf("first attempt carried %+v", st)
	}
	r.feed(t, report("state=done"))
	r.settle(t)
	c1.reply <- errors.New("tmux: server busy")

	c2 := pub.next(t)
	if st := decode(t, c2.w); st.State != runtime.ProgramDone {
		t.Fatalf("retry carried %+v, want the latest record (done)", st)
	}
	c2.reply <- nil

	close(r.chunks)
	if err := result(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestTapRetriesSameRecordWhenNothingNewer(t *testing.T) {
	pub := newFake(true)
	r := newStepReader()
	done := start(r, pub, Options{Nonce: testNonce}, fastConfig())

	r.feed(t, report("state=working:msg=aGk="))
	c1 := pub.next(t)
	c1.reply <- errors.New("tmux: server busy")
	c2 := pub.next(t)
	if c2.w != c1.w {
		t.Fatalf("retry %+v differs from failed write %+v", c2.w, c1.w)
	}
	c2.reply <- nil
	close(r.chunks)
	if err := result(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestTapExitsAfterFiveFailures(t *testing.T) {
	pub := newFake(true)
	r := newStepReader()
	done := start(r, pub, Options{Nonce: testNonce}, fastConfig())
	defer close(r.chunks)

	r.feed(t, report("state=working"))
	for i := 0; i < 5; i++ {
		pub.next(t).reply <- errors.New("no server running")
	}
	err := result(t, done)
	if !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("Run = %v, want ErrPublishFailed", err)
	}
	if !strings.Contains(err.Error(), "no server running") {
		t.Fatalf("Run = %v, want the last failure named", err)
	}
	if n := len(pub.all()); n != 5 {
		t.Fatalf("%d attempts, want 5", n)
	}
}

func TestTapFlushesPendingWriteOnEOF(t *testing.T) {
	pub := newFake(false)
	cfg := fastConfig()
	cfg.minInterval = time.Hour // the coalescer would hold the second write
	r := newStepReader()
	done := start(r, pub, Options{Nonce: testNonce}, cfg)

	r.feed(t, report("state=working"))
	pub.next(t)
	r.feed(t, report("state=done"))
	close(r.chunks)
	if err := result(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	writes := pub.all()
	if len(writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(writes))
	}
	if st := decode(t, writes[1]); st.State != runtime.ProgramDone || st.TurnEnds != 1 {
		t.Fatalf("flushed %+v, want done with one turn end", st)
	}
}

// TestTapRunReportsCancel cancels Run while the writer waits and the reader
// is still parked in Read: Run returns ctx's error and publishes nothing
// more, neither a flush nor a retry.
func TestTapRunReportsCancel(t *testing.T) {
	t.Run("coalescing wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pub := newFake(false)
			cfg := fastConfig()
			cfg.minInterval = time.Hour // the second snapshot waits out the interval
			r := newStepReader()
			defer close(r.chunks)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, r, pub, Options{Nonce: testNonce}, cfg) }()

			r.feed(t, report("state=working"))
			pub.next(t)
			r.feed(t, report("state=done"))
			synctest.Wait() // the writer is waiting out the interval
			cancel()
			if err := result(t, done); !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v, want context.Canceled", err)
			}
			if n := len(pub.all()); n != 1 {
				t.Errorf("%d publishes, want none after the cancel", n)
			}
		})
	})
	t.Run("retry wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pub := newFake(true)
			cfg := fastConfig()
			cfg.retryMin, cfg.retryMax = time.Hour, time.Hour // the failed write waits to retry
			r := newStepReader()
			defer close(r.chunks)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, r, pub, Options{Nonce: testNonce}, cfg) }()

			r.feed(t, report("state=working"))
			c := pub.next(t)
			r.feed(t, report("state=done")) // posted while the write is in flight
			c.reply <- errors.New("tmux: server busy")
			synctest.Wait() // the writer is waiting to retry
			cancel()
			if err := result(t, done); !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v, want context.Canceled", err)
			}
			if n := len(pub.all()); n != 1 {
				t.Errorf("%d publishes, want none after the cancel", n)
			}
		})
	})
}

func TestTapFlushFailureStillExitsCleanly(t *testing.T) {
	pub := newFake(true)
	cfg := fastConfig()
	cfg.minInterval = time.Hour
	var logged []string
	opts := Options{Nonce: testNonce, Logf: func(format string, _ ...any) { logged = append(logged, format) }}
	r := newStepReader()
	done := start(r, pub, opts, cfg)

	r.feed(t, report("state=working"))
	pub.next(t).reply <- nil
	r.feed(t, report("state=done"))
	close(r.chunks)
	pub.next(t).reply <- errors.New("can't find pane: %3")
	if err := result(t, done); err != nil {
		t.Fatalf("Run = %v; a failed flush on EOF must not fail the tap", err)
	}
	if n := len(pub.all()); n != 2 {
		t.Fatalf("%d attempts, want one flush and no retry", n)
	}
	if len(logged) == 0 {
		t.Fatal("the failed flush was not reported")
	}
}

func TestTapDropsForeignApp(t *testing.T) {
	t.Run("foreign only", func(t *testing.T) {
		pub := newFake(false)
		in := strings.NewReader(report("state=working:app=codex") + report("state=blocked:id=sub:app=codex"))
		if err := result(t, start(in, pub, Options{Nonce: testNonce, App: "claude"}, fastConfig())); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if writes := pub.all(); len(writes) != 0 {
			t.Fatalf("published %d writes for another program's reports", len(writes))
		}
	})
	t.Run("own and unnamed kept", func(t *testing.T) {
		pub := newFake(false)
		in := strings.NewReader(report("state=working:app=claude") +
			report("state=done:app=codex") + // foreign: dropped
			report("state=working:id=job:app=claude") +
			report("state=working:id=job/step") + // inherits claude from job
			report("state=working:id=job/other:app=codex")) // foreign: dropped
		if err := result(t, start(in, pub, Options{Nonce: testNonce, App: "claude"}, fastConfig())); err != nil {
			t.Fatalf("Run: %v", err)
		}
		writes := pub.all()
		if len(writes) == 0 {
			t.Fatal("nothing published")
		}
		st := decode(t, writes[len(writes)-1])
		if st.State != runtime.ProgramWorking || st.App != "claude" || st.Children != 2 {
			t.Fatalf("published %+v, want claude working with two children", st)
		}
	})
	t.Run("no app anywhere kept", func(t *testing.T) {
		pub := newFake(false)
		in := strings.NewReader(report("state=idle"))
		if err := result(t, start(in, pub, Options{Nonce: testNonce, App: "claude"}, fastConfig())); err != nil {
			t.Fatalf("Run: %v", err)
		}
		writes := pub.all()
		if len(writes) != 1 || decode(t, writes[0]).State != runtime.ProgramIdle {
			t.Fatalf("writes %+v, want the unnamed idle report", writes)
		}
	})
}

func TestTapRecordsProbe(t *testing.T) {
	pub := newFake(false)
	in := strings.NewReader("\x1b]7501;?\x1b\\")
	if err := result(t, start(in, pub, Options{Nonce: testNonce}, fastConfig())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	writes := pub.all()
	if len(writes) != 1 || !decode(t, writes[0]).Probed {
		t.Fatalf("writes %+v, want one with the probed flag", writes)
	}
}

func TestTapReturnsReadError(t *testing.T) {
	pub := newFake(false)
	boom := errors.New("read failed")
	err := result(t, start(io.MultiReader(strings.NewReader(report("state=idle")), errReader{boom}), pub, Options{Nonce: testNonce}, fastConfig()))
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the read error", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestRunRejectsInvalidOptions(t *testing.T) {
	for _, opts := range []Options{
		{Nonce: ""},
		{Nonce: "bad nonce.%3"},
		{Nonce: testNonce, App: "has space"},
	} {
		if err := Run(context.Background(), strings.NewReader(""), newFake(false), opts); err == nil {
			t.Errorf("Run(%+v) succeeded, want an options error", opts)
		}
	}
}
