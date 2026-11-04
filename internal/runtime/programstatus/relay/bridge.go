package relay

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// ProbeReply is what the relay writes into the program's input for each
// support probe it sees: the reply a terminal supporting OSC 7501 gives.
const ProbeReply = "\x1b]7501;?\x1b\\"

// Queue bounds. Typed input past maxQueuedInput waits for the program to
// read (back-pressure into the pane). Replies never wait, because the output
// path that queues them must keep draining; a program that floods probes
// without reading its input has replies past maxQueuedReplies dropped.
const (
	maxQueuedInput   = 64 << 10
	maxQueuedReplies = 64
)

// Read sizes for the two copy directions.
const (
	outputReadSize = 32 << 10
	inputReadSize  = 4 << 10
)

// ErrTerminalGone wraps a failed write to the outer terminal: the pane is
// gone.
var ErrTerminalGone = errors.New("relay: writing the terminal")

// ErrProgramGone is returned by [Bridge.Input] once the program's input can
// no longer be written: the pty's program side is gone, or the bridge is
// closed.
var ErrProgramGone = errors.New("relay: program input closed")

// Bridge is the relay's byte path, independent of ptys and processes: it
// copies the program's output to the terminal, answering support probes into
// the program's input, and the terminal's input to the program. One
// goroutine owns writes to the program's input, in queue order. Run Output
// and Input each on their own goroutine; Close stops the input writer.
type Bridge struct {
	q *inputQueue

	// forwarded, when set, observes how long each output chunk took from
	// its read returning to its forward completing (tests only).
	forwarded func(time.Duration)
}

// NewBridge starts a bridge writing the program's input to programIn.
func NewBridge(programIn io.Writer) *Bridge {
	q := newInputQueue()
	go q.drain(programIn)
	return &Bridge{q: q}
}

// Close stops the bridge's input writer; queued input not yet written is
// dropped. It is safe to call more than once.
func (b *Bridge) Close() { b.q.close() }

// Output copies the program's output to termOut verbatim until programOut
// ends, queueing [ProbeReply] into the program's input ahead of forwarding
// each chunk that completes a probe. It returns nil at programOut's EOF, an
// error wrapping [ErrTerminalGone] when termOut fails, else programOut's
// read error.
func (b *Bridge) Output(programOut io.Reader, termOut io.Writer) error {
	buf := make([]byte, outputReadSize)
	var m programstatus.ProbeMatcher
	for {
		n, err := programOut.Read(buf)
		if n > 0 {
			start := time.Now()
			if probes := m.Feed(buf[:n]); probes > 0 {
				b.q.pushReplies(probes)
			}
			if _, werr := termOut.Write(buf[:n]); werr != nil {
				return fmt.Errorf("%w: %w", ErrTerminalGone, werr)
			}
			if b.forwarded != nil {
				b.forwarded(time.Since(start))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("relay: reading program output: %w", err)
		}
	}
}

// Input copies termIn to the program's input verbatim until termIn ends. It
// returns nil at termIn's EOF, [ErrProgramGone] when the program's input is
// closed, else termIn's read error.
func (b *Bridge) Input(termIn io.Reader) error {
	buf := make([]byte, inputReadSize)
	for {
		n, err := termIn.Read(buf)
		if n > 0 && !b.q.pushInput(append([]byte(nil), buf[:n]...)) {
			if werr := b.q.writeErr(); werr != nil {
				return fmt.Errorf("%w: %w", ErrProgramGone, werr)
			}
			return ErrProgramGone
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("relay: reading terminal input: %w", err)
		}
	}
}

// inputQueue orders everything written to the program's input: typed input
// reads and probe replies, each written whole by one goroutine.
type inputQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []queued
	input   int // bytes of typed input queued
	replies int // replies queued
	closed  bool
	failed  error // the write that closed the queue
}

// queued is one write to the program's input.
type queued struct {
	b     []byte
	reply bool
}

var replyBytes = []byte(ProbeReply)

func newInputQueue() *inputQueue {
	q := &inputQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// pushInput queues one input read, waiting while the queue holds
// maxQueuedInput bytes or more. It reports false once the queue is closed.
func (q *inputQueue) pushInput(b []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.input >= maxQueuedInput && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return false
	}
	q.items = append(q.items, queued{b: b})
	q.input += len(b)
	q.cond.Broadcast()
	return true
}

// pushReplies queues n probe replies without waiting.
func (q *inputQueue) pushReplies(n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for ; n > 0 && q.replies < maxQueuedReplies && !q.closed; n-- {
		q.items = append(q.items, queued{b: replyBytes, reply: true})
		q.replies++
	}
	q.cond.Broadcast()
}

// pop takes the next write, waiting for one; ok is false once the queue is
// closed.
func (q *inputQueue) pop() (b []byte, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, false
	}
	it := q.items[0]
	q.items[0] = queued{}
	q.items = q.items[1:]
	if it.reply {
		q.replies--
	} else {
		q.input -= len(it.b)
	}
	q.cond.Broadcast()
	return it.b, true
}

// writeErr is the write failure that closed the queue, if one did.
func (q *inputQueue) writeErr() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failed
}

// close drops whatever is queued and wakes every waiter.
func (q *inputQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.items = nil
	q.cond.Broadcast()
}

// drain writes queued items to w in order until the queue closes or a write
// fails; a failed write closes the queue, so Input reports ErrProgramGone.
func (q *inputQueue) drain(w io.Writer) {
	for {
		b, ok := q.pop()
		if !ok {
			return
		}
		if _, err := w.Write(b); err != nil {
			q.mu.Lock()
			q.failed = err
			q.mu.Unlock()
			q.close()
			return
		}
	}
}
