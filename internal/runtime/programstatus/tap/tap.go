// Package tap is the capture engine of `gc-osc7501 tap` (design §4.2): it
// reads a pane's output from tmux pipe-pane -O, keeps the program's OSC 7501
// records, and publishes the root record to the pane's options.
//
// Two goroutines split the work. The reader never blocks on the writer: it
// scans every chunk, applies reports to the [programstatus.Store], and
// leaves the latest snapshot in a one-slot mailbox, because tmux buffers a
// stalled pipe reader's output without bound. The writer coalesces
// snapshots (at least 25 ms apart, backing off to 1 s under sustained
// churn), publishes the latest through a [Publisher], retries a failed
// write latest-wins, gives up after repeated failures, and on EOF flushes
// the pending snapshot once.
//
// The package imports only the standard library, internal/runtime and
// programstatus.
package tap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// ErrPublishFailed is returned by [Run] when consecutive writes kept
// failing; it wraps the last failure. The tap exits so readers see the
// capture as lost and heal re-arms the pane.
var ErrPublishFailed = errors.New("tap: publishing kept failing")

// Write is one publish: the encoded @gc_ps value and, when the record's text
// changed since the last successful publish, the encoded @gc_ps_text value.
type Write struct {
	Status string // the @gc_ps value; never empty
	Text   string // the @gc_ps_text value; empty when the text is unchanged
}

// Publisher writes to the pane. Publish must return once ctx is done.
type Publisher interface {
	Publish(ctx context.Context, w Write) error
}

// Options configures [Run].
type Options struct {
	// Nonce is the capture incarnation, "<rand>.<pane id>"; it is stamped on
	// every published value.
	Nonce string
	// App, when set, drops every report whose effective app (its own, else
	// its nearest stored ancestor's) names another program. Reports with no
	// app anywhere are kept.
	App string
	// Logf receives failures the tap survives (a failed write it retries, a
	// failed final flush). Nil discards them.
	Logf func(format string, args ...any)
}

// Run reads pane output from r until EOF, publishing through pub, and
// returns nil after the final flush. It returns an error wrapping
// [ErrPublishFailed] after consecutive failed writes, a read error, or an
// options error. When ctx ends first, Run returns an error wrapping ctx's
// error and skips the final flush. On a publish failure or cancellation Run
// returns without waiting for a read that is blocked; the caller is expected
// to exit.
func Run(ctx context.Context, r io.Reader, pub Publisher, opts Options) error {
	return run(ctx, r, pub, opts, defaultConfig())
}

// snapshot is one state of the store: what a publish encodes.
type snapshot struct {
	status runtime.ProgramStatus
	text   programstatus.Text
}

// mailbox is the one-slot handoff from reader to writer: a put replaces any
// snapshot the writer has not taken yet, and never blocks.
type mailbox struct {
	mu      sync.Mutex
	snap    snapshot
	pending bool
	notify  chan struct{}
}

func (m *mailbox) put(s snapshot) {
	m.mu.Lock()
	m.snap, m.pending = s, true
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *mailbox) take() (snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snap, m.pending
	m.pending = false
	return s, ok
}

type engine struct {
	cfg  config
	opts Options
	pub  Publisher
	box  mailbox
	// changes counts record changes the reader has posted, for the
	// coalescer's churn measure.
	changes atomic.Uint64

	// Writer state.
	coalesce   *coalescer
	pending    snapshot // the latest snapshot not yet published
	hasPending bool
	published  programstatus.Text // text as of the last successful publish
}

func run(ctx context.Context, r io.Reader, pub Publisher, opts Options, cfg config) error {
	if !programstatus.ValidNonce(opts.Nonce) {
		return fmt.Errorf("tap: invalid nonce %q", opts.Nonce)
	}
	if opts.App != "" && !programstatus.ValidApp(opts.App) {
		return fmt.Errorf("tap: invalid app %q", opts.App)
	}
	e := &engine{
		cfg:      cfg,
		opts:     opts,
		pub:      pub,
		box:      mailbox{notify: make(chan struct{}, 1)},
		coalesce: newCoalescer(cfg),
	}
	readDone := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		readErr = e.read(r)
	}()
	if err := e.write(ctx, readDone); err != nil {
		return err
	}
	<-readDone // write returns nil only once the reader finished
	if readErr != nil {
		return fmt.Errorf("tap: reading pane output: %w", readErr)
	}
	return nil
}

// read scans r until EOF, posting a snapshot after every chunk that changed
// the record. It never waits on the writer.
func (e *engine) read(r io.Reader) error {
	store := programstatus.NewStore(e.opts.Nonce)
	var sc programstatus.Scanner
	last := snapshot{status: store.Status(), text: store.Text()}
	handle := func(ev programstatus.Event) {
		switch ev.Kind {
		case programstatus.EventProbe:
			store.Probe()
		case programstatus.EventPromptStart:
			store.PromptStart()
		case programstatus.EventReport:
			rep, err := programstatus.ParseBody(ev.Body)
			if err != nil {
				return // dropped whole, as the spec requires
			}
			if e.opts.App != "" {
				if app := store.EffectiveApp(rep); app != "" && app != e.opts.App {
					return
				}
			}
			store.Apply(rep, time.Now())
		}
	}
	buf := make([]byte, e.cfg.readSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sc.Feed(buf[:n], handle)
			next := snapshot{status: store.Status(), text: store.Text()}
			if delta := next.status.Seq - last.status.Seq; delta > 0 || next.text != last.text {
				e.changes.Add(max(delta, 1))
				last = next
				e.box.put(next)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// write publishes posted snapshots until the reader finishes, then flushes.
func (e *engine) write(ctx context.Context, readDone <-chan struct{}) error {
	var last time.Time // start of the last publish
	for {
		select {
		case <-e.box.notify:
		case <-readDone:
			e.flush(ctx)
			return nil
		case <-ctx.Done():
			return fmt.Errorf("tap: %w", ctx.Err())
		}
		if !last.IsZero() {
			if wait := e.coalesce.interval - time.Since(last); wait > 0 {
				eof, err := e.sleep(ctx, wait, readDone)
				if err != nil {
					return err
				}
				if eof {
					e.flush(ctx)
					return nil
				}
			}
		}
		e.takeLatest()
		if !e.hasPending {
			continue
		}
		last = time.Now()
		e.coalesce.observe(last, e.changes.Load())
		if err := e.publishWithRetry(ctx, readDone); err != nil {
			return err
		}
	}
}

// publishWithRetry publishes the pending snapshot, retrying failures with
// backoff and always with the latest snapshot, until one write succeeds,
// the reader finishes (then it flushes once), ctx ends (then it returns
// ctx's error), or maxFailures writes in a row failed.
func (e *engine) publishWithRetry(ctx context.Context, readDone <-chan struct{}) error {
	for failures := 0; ; {
		err := e.publish(ctx)
		if err == nil {
			return nil
		}
		failures++
		if failures >= e.cfg.maxFailures {
			return fmt.Errorf("%w: %d consecutive writes: %w", ErrPublishFailed, failures, err)
		}
		e.logf("tap: write %d of %d failed, retrying: %v", failures, e.cfg.maxFailures, err)
		eof, err := e.sleep(ctx, e.cfg.retryDelay(failures), readDone)
		if err != nil {
			return err
		}
		if eof {
			return nil // the caller sees readDone and flushes
		}
		e.takeLatest()
	}
}

// flush publishes the pending snapshot once, if there is one. Its failure is
// logged, not returned: the nonce guard makes a late write harmless, and a
// tap at EOF has nothing left to retry for.
func (e *engine) flush(ctx context.Context) {
	e.takeLatest()
	if !e.hasPending {
		return
	}
	if err := e.publish(ctx); err != nil {
		e.logf("tap: final flush failed: %v", err)
	}
}

// takeLatest replaces the pending snapshot with a newer one, if posted.
func (e *engine) takeLatest() {
	if s, ok := e.box.take(); ok {
		e.pending, e.hasPending = s, true
	}
}

// publish writes the pending snapshot once, bounded by writeTimeout.
func (e *engine) publish(ctx context.Context) error {
	status, err := programstatus.Encode(e.pending.status)
	if err != nil {
		return err
	}
	w := Write{Status: status}
	if e.pending.text != e.published {
		if w.Text, err = programstatus.EncodeText(e.opts.Nonce, e.pending.text); err != nil {
			return err
		}
	}
	wctx, cancel := context.WithTimeout(ctx, e.cfg.writeTimeout)
	defer cancel()
	if err := e.pub.Publish(wctx, w); err != nil {
		return err
	}
	e.published, e.hasPending = e.pending.text, false
	return nil
}

// sleep waits d. It returns eof when the reader finished first, and ctx's
// error when ctx ended first.
func (e *engine) sleep(ctx context.Context, d time.Duration, readDone <-chan struct{}) (eof bool, err error) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false, nil
	case <-readDone:
		return true, nil
	case <-ctx.Done():
		return false, fmt.Errorf("tap: %w", ctx.Err())
	}
}

func (e *engine) logf(format string, args ...any) {
	if e.opts.Logf != nil {
		e.opts.Logf(format, args...)
	}
}
