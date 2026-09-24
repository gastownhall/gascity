package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ACP JSON-RPC capture transcript, format v1.
//
// When [Config.TranscriptRoot] is set and the session environment carries
// GC_SESSION_ID and GC_CONTINUATION_EPOCH, the provider records the JSON-RPC
// traffic it exchanges with the agent to
//
//	<TranscriptRoot>/<GC_SESSION_ID>/<GC_CONTINUATION_EPOCH>.jsonl
//
// (path built by citylayout.ACPTranscriptPathForDir; readers use the same
// helper). Directories are 0700 and the file is 0600 because prompts and tool
// output are sensitive. The file is opened in append mode: an agent restart
// within one continuation epoch appends to the same file, and a session reset
// (a new epoch) starts a new file. Files are never rotated or purged here.
//
// The file holds one JSON object per line:
//
//   - A header at every agent start:
//     {"gc_acp_capture":1,"ts":…,"session_id":…,"session_name":…,
//     "continuation_epoch":…,"runtime_epoch":…,"pid":…}
//   - One record per JSON-RPC line: {"ts":…,"dir":"out"|"in","msg":<message>}.
//     "out" is a message gc wrote to the agent's stdin (recorded immediately
//     before the write is attempted, so it always precedes the agent's reply);
//     "in" is a line read from the agent's stdout that is a complete JSON
//     object, whether or not gc could decode it as a JSON-RPC message it
//     handles. msg is the exact JSON-RPC bytes spliced in, never re-marshaled.
//     Other stdout lines (banners, non-object JSON) are not captured.
//   - Exception: an out message that carries credentials (session/new, whose
//     MCP server definitions hold env values, headers and URLs) is recorded
//     as {"ts":…,"dir":"out","redacted":true,"msg":<message>}, where msg is
//     the message re-encoded with runtime.RedactMCPServerConfigs applied. It
//     is not the exact wire bytes; the id and method are unchanged.
//   - After records were dropped because the writer fell behind:
//     {"ts":…,"dir":"meta","dropped":N}, placed exactly where the gap is (and
//     once more at close for drops that no later record followed).
//
// ts values are RFC 3339 with nanoseconds, taken when the line was queued.
// Readers must skip lines that do not parse: a crash or a failed write can
// leave a record cut short. The next writer to open the file starts on a new
// line, so a torn record never swallows the header that follows it.
// Capture never blocks the read loop or stdin writers and never fails the
// session: I/O errors are reported once per session on stderr and capture
// stops for that agent process.

// captureFormatVersion is the value of the header's gc_acp_capture field.
const captureFormatVersion = 1

// captureQueueLines bounds the in-memory queue between the JSON-RPC paths and
// the file writer. Beyond it, records are dropped and counted.
const captureQueueLines = 4096

// captureCloseTimeout bounds how long process-exit cleanup waits for the
// writer to flush the queue and close the file.
const captureCloseTimeout = time.Second

// captureDirection is the dir field of a capture record.
type captureDirection string

const (
	captureOut  captureDirection = "out"
	captureIn   captureDirection = "in"
	captureMeta captureDirection = "meta"
	// captureHeaderRecord marks a pre-rendered header line; it never appears
	// as a dir value in the file.
	captureHeaderRecord captureDirection = "header"
)

// captureIdentity is the session identity recorded in a capture header.
type captureIdentity struct {
	SessionID         string
	SessionName       string
	ContinuationEpoch string
	RuntimeEpoch      string
	PID               int
}

type captureHeader struct {
	Version           int    `json:"gc_acp_capture"`
	TS                string `json:"ts"`
	SessionID         string `json:"session_id"`
	SessionName       string `json:"session_name"`
	ContinuationEpoch string `json:"continuation_epoch"`
	RuntimeEpoch      string `json:"runtime_epoch"`
	PID               int    `json:"pid"`
}

type captureRecord struct {
	ts  time.Time
	dir captureDirection
	// raw is the JSON-RPC message (or the rendered header line). The capture
	// owns it once queued; callers must not mutate it.
	raw []byte
	// droppedBefore counts records dropped immediately before this one.
	droppedBefore int64
	// redacted marks raw as a redacted re-encoding rather than wire bytes.
	redacted bool
}

// transcriptCapture writes capture records to a file from a single goroutine.
// A nil *transcriptCapture is a disabled capture; every method is a no-op.
type transcriptCapture struct {
	label   string
	onError func(error)

	mu      sync.Mutex // guards queue sends, closed and dropped
	queue   chan captureRecord
	closed  bool
	dropped int64

	w    io.WriteCloser
	done chan struct{}
}

// openTranscriptCapture creates the private session directory under root,
// opens the epoch file for append, and queues the header. The returned
// capture must be closed.
func openTranscriptCapture(root string, id captureIdentity) (*transcriptCapture, error) {
	path, err := citylayout.ACPTranscriptPathForDir(root, id.SessionID, id.ContinuationEpoch)
	if err != nil {
		return nil, err
	}
	if err := runtime.EnsurePrivateDir(root); err != nil {
		return nil, err
	}
	if err := runtime.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// O_RDWR (not O_WRONLY) so the torn-tail check below can read the last
	// byte; every write still goes through O_APPEND.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening acp transcript %q: %w", path, err)
	}
	// Mode on create is narrowed by umask only; a pre-existing file from an
	// older writer is tightened here.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("setting mode on acp transcript %q: %w", path, err)
	}
	if err := terminateTornTail(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("repairing acp transcript tail %q: %w", path, err)
	}
	c := newTranscriptCapture(f, captureQueueLines, id.SessionName, nil)
	c.recordHeader(id)
	return c, nil
}

// terminateTornTail appends a newline when f is non-empty and does not end in
// one. A previous writer that died partway through a record (a crash between
// the writes of one flush, a short write on a full disk) leaves such a tail;
// without the newline this start's header would be glued onto the torn bytes
// and no longer parse.
func terminateTornTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// newTranscriptCapture starts a capture writer over w with a queue of
// queueLines records. label names the session in stderr diagnostics; onError,
// when nil, reports to stderr.
func newTranscriptCapture(w io.WriteCloser, queueLines int, label string, onError func(error)) *transcriptCapture {
	if queueLines <= 0 {
		queueLines = captureQueueLines
	}
	c := &transcriptCapture{
		label:   label,
		onError: onError,
		queue:   make(chan captureRecord, queueLines),
		w:       w,
		done:    make(chan struct{}),
	}
	if c.onError == nil {
		c.onError = func(err error) {
			fmt.Fprintf(os.Stderr, "acp: transcript capture for %q disabled: %v\n", label, err)
		}
	}
	go c.run()
	return c
}

// recordHeader queues the per-start header line.
func (c *transcriptCapture) recordHeader(id captureIdentity) {
	if c == nil {
		return
	}
	now := time.Now()
	line, err := json.Marshal(captureHeader{
		Version:           captureFormatVersion,
		TS:                now.UTC().Format(time.RFC3339Nano),
		SessionID:         id.SessionID,
		SessionName:       id.SessionName,
		ContinuationEpoch: id.ContinuationEpoch,
		RuntimeEpoch:      id.RuntimeEpoch,
		PID:               id.PID,
	})
	if err != nil {
		c.onError(fmt.Errorf("encoding header: %w", err))
		return
	}
	c.enqueue(captureRecord{ts: now, dir: captureHeaderRecord, raw: line})
}

// record queues one JSON-RPC message. It never blocks: when the queue is full
// the record is dropped and counted. raw must be a complete JSON value and is
// owned by the capture after the call.
func (c *transcriptCapture) record(dir captureDirection, raw []byte) {
	if c == nil {
		return
	}
	c.enqueue(captureRecord{ts: time.Now(), dir: dir, raw: raw})
}

// recordRedacted queues an out message whose raw bytes are a redacted
// re-encoding of what was sent; the record is marked "redacted":true.
func (c *transcriptCapture) recordRedacted(dir captureDirection, raw []byte) {
	if c == nil {
		return
	}
	c.enqueue(captureRecord{ts: time.Now(), dir: dir, raw: raw, redacted: true})
}

func (c *transcriptCapture) enqueue(rec captureRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	rec.droppedBefore = c.dropped
	select {
	case c.queue <- rec:
		c.dropped = 0
	default:
		c.dropped++
	}
}

// close stops accepting records, then waits up to timeout for the writer to
// flush and close the file. A writer still wedged after timeout finishes in
// the background. Safe to call more than once.
func (c *transcriptCapture) close(timeout time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.dropped > 0 {
			// The queue send cannot block the caller; if the final marker
			// itself does not fit, the writer's tail marker covers it.
			rec := captureRecord{ts: time.Now(), dir: captureMeta, droppedBefore: c.dropped}
			select {
			case c.queue <- rec:
				c.dropped = 0
			default:
			}
		}
		close(c.queue)
	}
	c.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		fmt.Fprintf(os.Stderr, "acp: transcript capture for %q: flush did not finish within %v\n", c.label, timeout)
	}
}

// run drains the queue into the file. It is the only goroutine touching w.
func (c *transcriptCapture) run() {
	defer close(c.done)
	bw := bufio.NewWriterSize(c.w, 64*1024)
	failed := false
	fail := func(err error) {
		if !failed {
			failed = true
			c.onError(err)
		}
	}
	var line []byte
	for rec := range c.queue {
		if failed {
			continue
		}
		line = line[:0]
		if rec.droppedBefore > 0 {
			line = appendDropMarker(line, rec.ts, rec.droppedBefore)
		}
		switch rec.dir {
		case captureHeaderRecord:
			line = append(line, rec.raw...)
			line = append(line, '\n')
		case captureMeta:
			// A pure drop marker; already rendered above.
		default:
			line = appendCaptureRecord(line, rec)
		}
		if _, err := bw.Write(line); err != nil {
			fail(fmt.Errorf("writing: %w", err))
			continue
		}
		if len(c.queue) == 0 {
			if err := bw.Flush(); err != nil {
				fail(fmt.Errorf("flushing: %w", err))
			}
		}
	}
	if !failed {
		c.mu.Lock()
		tail := c.dropped
		c.mu.Unlock()
		if tail > 0 {
			if _, err := bw.Write(appendDropMarker(nil, time.Now(), tail)); err != nil {
				fail(fmt.Errorf("writing: %w", err))
			}
		}
		if err := bw.Flush(); err != nil {
			fail(fmt.Errorf("flushing: %w", err))
		}
	}
	if err := c.w.Close(); err != nil && !failed {
		fail(fmt.Errorf("closing: %w", err))
	}
}

func appendCaptureRecord(dst []byte, rec captureRecord) []byte {
	dst = append(dst, `{"ts":`...)
	dst = strconv.AppendQuote(dst, rec.ts.UTC().Format(time.RFC3339Nano))
	dst = append(dst, `,"dir":`...)
	dst = strconv.AppendQuote(dst, string(rec.dir))
	if rec.redacted {
		dst = append(dst, `,"redacted":true`...)
	}
	dst = append(dst, `,"msg":`...)
	dst = append(dst, rec.raw...)
	return append(dst, "}\n"...)
}

func appendDropMarker(dst []byte, ts time.Time, n int64) []byte {
	dst = append(dst, `{"ts":`...)
	dst = strconv.AppendQuote(dst, ts.UTC().Format(time.RFC3339Nano))
	dst = append(dst, `,"dir":"meta","dropped":`...)
	dst = strconv.AppendInt(dst, n, 10)
	return append(dst, "}\n"...)
}
