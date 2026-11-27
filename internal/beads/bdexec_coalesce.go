package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
)

// bdChildSpec is everything that determines what one bd child does: the
// resolved executable, its working directory, its argv and its complete
// environment.
type bdChildSpec struct {
	name string
	path string
	dir  string
	args []string
	env  []string
}

// bdChildResult is the outcome of runBDChild. refused is set, and nothing
// else, when the command was refused before it could start.
type bdChildResult struct {
	out     []byte
	stderr  string
	err     error
	refused error
}

// runBDChild runs one child for spec, bound to ctx: a ctx that ends kills the
// child's whole process tree. onStart runs immediately before the child
// starts; when it returns false the child is not started and the result
// carries ctx's error. The result matches cmd.Output with a separate stderr
// buffer.
func runBDChild(ctx context.Context, spec bdChildSpec, onStart func() bool) bdChildResult {
	cmd := exec.CommandContext(ctx, spec.path, spec.args...)
	if err := execenv.CheckBatchFileArgs(cmd); err != nil {
		return bdChildResult{refused: err}
	}
	cmd.WaitDelay = 2 * time.Second
	prepareCommandForTimeout(cmd)
	cmd.Dir = spec.dir
	cmd.Cancel = func() error {
		return killCommandTree(cmd)
	}
	cmd.Env = spec.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if onStart != nil && !onStart() {
		return bdChildResult{err: fmt.Errorf("not started: %w", context.Cause(ctx))}
	}
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	return bdChildResult{out: stdout.Bytes(), stderr: stderr.String(), err: err}
}

// bdCommandCoalesces reports whether identical concurrent calls of a bd argv
// may share one child: read-lane verbs except sql, which runs arbitrary
// statements.
func bdCommandCoalesces(args []string) bool {
	return bdCommandIsRead(args) && args[0] != "sql"
}

// bdReadKey identifies calls whose children would be indistinguishable: the
// resolved executable, the dir, every arg and the final child env, each
// prefixed with its length so no two specs share a key by concatenation. It
// holds the child env as is, credentials included, as the spec of the flight
// it keys does, and never leaves the process.
type bdReadKey string

// bdReadKeyFor returns the key of spec.
func bdReadKeyFor(spec bdChildSpec) bdReadKey {
	var b strings.Builder
	writeKeyField(&b, spec.path)
	writeKeyField(&b, spec.dir)
	writeKeyCount(&b, len(spec.args))
	for _, arg := range spec.args {
		writeKeyField(&b, arg)
	}
	writeKeyCount(&b, len(spec.env))
	for _, kv := range spec.env {
		writeKeyField(&b, kv)
	}
	return bdReadKey(b.String())
}

// writeKeyCount writes n in decimal, closed by a colon.
func writeKeyCount(b *strings.Builder, n int) {
	b.WriteString(strconv.Itoa(n))
	b.WriteByte(':')
}

// writeKeyField writes s prefixed with its length.
func writeKeyField(b *strings.Builder, s string) {
	writeKeyCount(b, len(s))
	b.WriteString(s)
}

// bdReadCoalescer shares bd read children among identical concurrent calls
// (#9092). Per key there is at most one running child and at most one pending
// flight. A caller joins the key's pending flight or creates one; a flight
// stops accepting joiners in the same critical section that marks it running,
// immediately before its child starts. So every caller gets the output of a
// child that started after its call began, N overlapping identical callers
// cost at most two children, and completed results are never cached. A flight
// has no budget of its own: each caller waits under its own budget, and the
// flight ends when its last waiter leaves.
type bdReadCoalescer struct {
	gate *bdExecGate

	mu   sync.Mutex
	keys map[bdReadKey]*bdReadKeyState
}

// bdReadKeyState is one key's entry. It stays in the map until every flight
// created for the key has finished, abandoned flights included, so each
// flight finds the entry it was created under until its own finish.
type bdReadKeyState struct {
	pending *bdReadFlight
	// running is non-nil while a child for the key is alive and is closed
	// once that child has been reaped.
	running chan struct{}
	// flights counts the key's flights that have not finished.
	flights int
}

// bdReadFlight is one shared child. Fields above mu-guarded ones are set at
// creation; result fields are written by the flight goroutine before done is
// closed and read only after it.
type bdReadFlight struct {
	key  bdReadKey
	spec bdChildSpec
	// ctx bounds the flight's waits and its child. It has no deadline and is
	// canceled when the last waiter leaves.
	ctx     context.Context
	cancel  context.CancelFunc
	created time.Time
	callers []string
	done    chan struct{}

	// Guarded by bdReadCoalescer.mu.
	waiters   int
	joined    int
	started   bool
	abandoned bool

	res      bdChildResult
	admitErr error
	waited   time.Duration
}

func newBDReadCoalescer(gate *bdExecGate) *bdReadCoalescer {
	return &bdReadCoalescer{gate: gate, keys: make(map[bdReadKey]*bdReadKeyState)}
}

// do runs spec through a shared flight and returns this caller's view of the
// result (see wait).
func (c *bdReadCoalescer) do(parent, ctx context.Context, spec bdChildSpec, timeout time.Duration, start time.Time) (out []byte, status string, traceErr, resultErr error) {
	f, created := c.join(parent, spec)
	if created {
		go c.fly(f)
	}
	return c.wait(parent, ctx, f, spec, timeout, start)
}

// wait blocks until flight f, which the caller has joined, finishes or the
// caller's ctx ends, then drops the caller from f and returns the caller's
// view of the result, classified against the caller's own parent, ctx, budget
// and start. ctx must end by the caller's per-command budget: the waiters'
// contexts are all that bounds a flight.
func (c *bdReadCoalescer) wait(parent, ctx context.Context, f *bdReadFlight, spec bdChildSpec, timeout time.Duration, start time.Time) (out []byte, status string, traceErr, resultErr error) {
	select {
	case <-f.done:
		c.leave(f)
	case <-ctx.Done():
		started := c.leave(f)
		err := c.waiterGaveUpError(parent, ctx, spec, timeout, start, started)
		return nil, waiterGaveUpStatus(ctx, started), err, err
	}
	if f.admitErr != nil {
		err := c.gate.slotWaitError(parent, f.admitErr, timeout, start)
		return nil, "slot-wait", err, err
	}
	if f.res.refused != nil {
		err := fmt.Errorf("running %s: %w", spec.name, f.res.refused)
		return nil, "refused", err, err
	}
	out = bytes.Clone(f.res.out)
	status, traceErr, resultErr = classifyBDExecResult(parent, ctx, spec.name, timeout, start, out, f.res.stderr, f.res.err)
	return out, status, traceErr, resultErr
}

// waiterGaveUpError is the error for a caller whose own budget ended before
// its flight finished: the slot-wait error while the flight had not started
// its child, and otherwise the timeout or cancellation a direct call would
// report for its own child.
func (c *bdReadCoalescer) waiterGaveUpError(parent, ctx context.Context, spec bdChildSpec, timeout time.Duration, start time.Time, started bool) error {
	switch {
	case !started:
		return c.gate.slotWaitError(parent, ctx.Err(), timeout, start)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return bdExecTimeoutError(parent, timeout, start)
	default:
		return fmt.Errorf("running %s: %w", spec.name, ctx.Err())
	}
}

func waiterGaveUpStatus(ctx context.Context, started bool) string {
	switch {
	case !started:
		return "slot-wait"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	default:
		return "canceled"
	}
}

// join adds the caller to the key's pending flight, or creates one and
// reports created, and the caller then runs the new flight with fly. A new
// flight keeps the creator's context values but neither its cancellation nor
// a deadline, so no waiter is bounded by another's budget. Its context ends
// when its last waiter leaves. Every waiter joined before the child started
// and waits at most its own per-command budget, so the child never runs past
// that budget counted from the child's start.
func (c *bdReadCoalescer) join(parent context.Context, spec bdChildSpec) (f *bdReadFlight, created bool) {
	key := bdReadKeyFor(spec)
	callers := bdTraceCallersIfEnabled()
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.keys[key]
	if st == nil {
		st = &bdReadKeyState{}
		c.keys[key] = st
	}
	if pending := st.pending; pending != nil {
		pending.waiters++
		pending.joined++
		return pending, false
	}
	spec.args = slices.Clone(spec.args)
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	f = &bdReadFlight{
		key:     key,
		spec:    spec,
		ctx:     ctx,
		cancel:  cancel,
		created: time.Now(),
		callers: callers,
		done:    make(chan struct{}),
		waiters: 1,
		joined:  1,
	}
	st.pending = f
	st.flights++
	return f, true
}

// fly runs a flight: it waits until the key has no running child, takes a
// read slot from the gate, then spawns the child.
func (c *bdReadCoalescer) fly(f *bdReadFlight) {
	defer close(f.done)
	defer f.cancel()
	if err := c.awaitIdleKey(f); err != nil {
		f.admitErr = err
		c.finish(f)
		return
	}
	release, err := c.gate.acquire(f.ctx, true)
	f.waited = time.Since(f.created)
	if err != nil {
		f.admitErr = err
		c.finish(f)
		return
	}
	f.res = runBDChild(f.ctx, f.spec, func() bool { return c.markRunning(f) })
	release()
	c.finish(f)
	if f.started {
		c.mu.Lock()
		waiters := f.joined
		c.mu.Unlock()
		recordBDExecTelemetry(f.spec.name, f.spec.dir, f.spec.args, f.created, f.res.out, f.res.stderr, f.res.err,
			bdTraceExtra{waitMs: f.waited.Milliseconds(), waiters: waiters, callers: f.callers})
	}
}

// awaitIdleKey blocks until the flight's key has no running child. Only the
// key's pending flight can start a child, so the key stays idle once seen.
func (c *bdReadCoalescer) awaitIdleKey(f *bdReadFlight) error {
	for {
		c.mu.Lock()
		running := c.keys[f.key].running
		c.mu.Unlock()
		if running == nil {
			return nil
		}
		select {
		case <-running:
		case <-f.ctx.Done():
			return f.ctx.Err()
		}
	}
}

// leave drops one waiter from the flight and reports whether the flight had
// started its child. When the last waiter leaves, the flight is abandoned:
// it leaves the pending slot, so no later caller joins it, and its context is
// canceled, which aborts its waits or kills its child.
func (c *bdReadCoalescer) leave(f *bdReadFlight) (started bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.waiters--
	if f.waiters == 0 {
		f.abandoned = true
		if st := c.keys[f.key]; st != nil && st.pending == f {
			st.pending = nil
		}
		f.cancel()
	}
	return f.started
}

// markRunning closes the flight to joiners and marks its key running, in one
// critical section, immediately before the child starts. An abandoned flight
// is not started: a newer flight for the key may already be waiting for the
// key to go idle.
func (c *bdReadCoalescer) markRunning(f *bdReadFlight) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.abandoned {
		return false
	}
	st := c.keys[f.key]
	st.running = make(chan struct{})
	if st.pending == f {
		st.pending = nil
	}
	f.started = true
	return true
}

// finish clears the flight's marks once its child has been reaped (or never
// started) and drops the key once none of its flights is left.
func (c *bdReadCoalescer) finish(f *bdReadFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.keys[f.key]
	if f.started {
		close(st.running)
		st.running = nil
	}
	if st.pending == f {
		st.pending = nil
	}
	st.flights--
	if st.flights == 0 {
		delete(c.keys, f.key)
	}
}
