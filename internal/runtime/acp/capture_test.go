package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// captureSink is an in-memory io.WriteCloser for capture writer tests. When
// gate is non-nil every Write first announces itself on entered and then
// blocks until gate is closed, which lets a test hold the writer goroutine
// inside an I/O call and fill the queue deterministically.
type captureSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	closed  bool
	gate    chan struct{}
	entered chan struct{}
	writes  chan struct{}
	err     error
}

func newCaptureSink() *captureSink {
	return &captureSink{writes: make(chan struct{}, 64)}
}

func (s *captureSink) Write(p []byte) (int, error) {
	if s.gate != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	n, _ := s.buf.Write(p)
	select {
	case s.writes <- struct{}{}:
	default:
	}
	return n, nil
}

func (s *captureSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *captureSink) lines(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return parseCaptureLines(t, s.buf.Bytes())
}

func parseCaptureLines(t *testing.T, data []byte) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			t.Fatalf("capture line is not a JSON object: %q: %v", sc.Text(), err)
		}
		out = append(out, obj)
	}
	return out
}

func rawString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode string %s: %v", raw, err)
	}
	return s
}

func testCaptureIdentity() captureIdentity {
	return captureIdentity{
		SessionID:         "s-1",
		SessionName:       "worker-1",
		ContinuationEpoch: "2",
		RuntimeEpoch:      "5",
		PID:               4242,
	}
}

func TestCapture_HeaderAndEnvelope(t *testing.T) {
	sink := newCaptureSink()
	c := newTranscriptCapture(sink, 16, "worker-1", nil)
	c.recordHeader(testCaptureIdentity())
	c.record(captureOut, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	c.record(captureIn, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	c.close(time.Second)

	lines := sink.lines(t)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	h := lines[0]
	if string(h["gc_acp_capture"]) != "1" {
		t.Fatalf("header gc_acp_capture = %s, want 1", h["gc_acp_capture"])
	}
	for key, want := range map[string]string{
		"session_id":         "s-1",
		"session_name":       "worker-1",
		"continuation_epoch": "2",
		"runtime_epoch":      "5",
	} {
		if got := rawString(t, h[key]); got != want {
			t.Fatalf("header %s = %q, want %q", key, got, want)
		}
	}
	if string(h["pid"]) != "4242" {
		t.Fatalf("header pid = %s, want 4242", h["pid"])
	}
	if _, err := time.Parse(time.RFC3339Nano, rawString(t, h["ts"])); err != nil {
		t.Fatalf("header ts not RFC3339Nano: %v", err)
	}
	for i, wantDir := range []string{"out", "in"} {
		rec := lines[i+1]
		if got := rawString(t, rec["dir"]); got != wantDir {
			t.Fatalf("line %d dir = %q, want %q", i+1, got, wantDir)
		}
		if _, err := time.Parse(time.RFC3339Nano, rawString(t, rec["ts"])); err != nil {
			t.Fatalf("line %d ts not RFC3339Nano: %v", i+1, err)
		}
		if len(rec["msg"]) == 0 {
			t.Fatalf("line %d has no msg", i+1)
		}
	}
	if !sink.closed {
		t.Fatal("close did not close the underlying writer")
	}
}

// TestCapture_SplicesRawBytes proves msg is the exact JSON-RPC bytes, not a
// re-marshaled copy: whitespace, key order and number spelling survive.
func TestCapture_SplicesRawBytes(t *testing.T) {
	raw := `{"method":"session/update",  "jsonrpc":"2.0","params":{"z":1.50,"a":"é"}}`
	sink := newCaptureSink()
	c := newTranscriptCapture(sink, 16, "worker-1", nil)
	c.record(captureIn, []byte(raw))
	c.close(time.Second)

	lines := sink.lines(t)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if got := string(lines[0]["msg"]); got != raw {
		t.Fatalf("msg = %s\nwant  %s", got, raw)
	}
}

func TestCapture_PreservesOrder(t *testing.T) {
	sink := newCaptureSink()
	c := newTranscriptCapture(sink, 4096, "worker-1", nil)
	const n = 500
	for i := 0; i < n; i++ {
		c.record(captureOut, []byte(`{"id":`+itoa(i)+`}`))
	}
	c.close(time.Second)
	lines := sink.lines(t)
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	for i, rec := range lines {
		if got, want := string(rec["msg"]), `{"id":`+itoa(i)+`}`; got != want {
			t.Fatalf("line %d msg = %s, want %s", i, got, want)
		}
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestCapture_OverflowDropsAndMarksGap holds the writer inside a Write so the
// queue fills, then checks the dropped records are counted, never block the
// caller, and are reported by a meta line exactly where the gap is and at close.
func TestCapture_OverflowDropsAndMarksGap(t *testing.T) {
	sink := newCaptureSink()
	gate := make(chan struct{})
	sink.gate = gate
	sink.entered = make(chan struct{}, 1)
	c := newTranscriptCapture(sink, 2, "worker-1", nil)

	c.record(captureOut, []byte(`{"id":0}`))
	<-sink.entered // writer dequeued id 0 and is blocked flushing it

	c.record(captureOut, []byte(`{"id":1}`)) // queued
	c.record(captureOut, []byte(`{"id":2}`)) // queued (queue now full)
	c.record(captureOut, []byte(`{"id":3}`)) // dropped
	c.record(captureOut, []byte(`{"id":4}`)) // dropped
	c.record(captureOut, []byte(`{"id":5}`)) // dropped

	close(gate)
	// Wait until ids 1 and 2 are flushed, so the queue has room again.
	waitCaptureContains(t, sink, `{"id":2}`)

	c.record(captureOut, []byte(`{"id":6}`))
	c.record(captureOut, []byte(`{"id":7}`))
	c.close(time.Second)

	lines := sink.lines(t)
	var got []string
	for _, rec := range lines {
		switch rawString(t, rec["dir"]) {
		case "meta":
			got = append(got, "dropped="+string(rec["dropped"]))
		default:
			got = append(got, string(rec["msg"]))
		}
	}
	want := []string{`{"id":0}`, `{"id":1}`, `{"id":2}`, "dropped=3", `{"id":6}`, `{"id":7}`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("records = %v\nwant      %v", got, want)
	}
}

func waitCaptureContains(t *testing.T, sink *captureSink, needle string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		sink.mu.Lock()
		found := bytes.Contains(sink.buf.Bytes(), []byte(needle))
		sink.mu.Unlock()
		if found {
			return
		}
		select {
		case <-sink.writes:
		case <-deadline:
			t.Fatalf("capture never wrote %s", needle)
		}
	}
}

func TestCapture_TrailingDropsReportedAtClose(t *testing.T) {
	sink := newCaptureSink()
	gate := make(chan struct{})
	sink.gate = gate
	sink.entered = make(chan struct{}, 1)
	c := newTranscriptCapture(sink, 1, "worker-1", nil)
	c.record(captureOut, []byte(`{"id":0}`))
	<-sink.entered
	c.record(captureOut, []byte(`{"id":1}`)) // queued
	c.record(captureOut, []byte(`{"id":2}`)) // dropped
	c.record(captureOut, []byte(`{"id":3}`)) // dropped
	closeDone := make(chan struct{})
	go func() {
		c.close(5 * time.Second)
		close(closeDone)
	}()
	close(gate)
	<-closeDone

	lines := sink.lines(t)
	last := lines[len(lines)-1]
	if rawString(t, last["dir"]) != "meta" || string(last["dropped"]) != "2" {
		t.Fatalf("last line = %v, want meta dropped=2", last)
	}
}

func TestCapture_CloseIsBounded(t *testing.T) {
	sink := newCaptureSink()
	gate := make(chan struct{})
	sink.gate = gate
	sink.entered = make(chan struct{}, 1)
	defer close(gate)
	c := newTranscriptCapture(sink, 4, "worker-1", nil)
	c.record(captureOut, []byte(`{"id":0}`))
	<-sink.entered

	start := time.Now()
	c.close(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("close blocked %v on a wedged writer", elapsed)
	}
	// Records after close are ignored, not a panic on a closed queue.
	c.record(captureIn, []byte(`{"id":1}`))
	c.close(50 * time.Millisecond)
}

func TestCapture_WriteErrorReportedOnce(t *testing.T) {
	sink := newCaptureSink()
	sink.err = errors.New("disk full")
	var mu sync.Mutex
	var reports []error
	c := newTranscriptCapture(sink, 16, "worker-1", func(err error) {
		mu.Lock()
		reports = append(reports, err)
		mu.Unlock()
	})
	for i := 0; i < 10; i++ {
		c.record(captureOut, []byte(`{"id":1}`))
	}
	c.close(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("got %d error reports, want exactly 1: %v", len(reports), reports)
	}
}

func TestCapture_NilIsDisabled(_ *testing.T) {
	var c *transcriptCapture
	c.recordHeader(testCaptureIdentity())
	c.record(captureOut, []byte(`{}`))
	c.close(time.Second)
}

func TestOpenTranscriptCapture_PermsAndAppend(t *testing.T) {
	root := filepath.Join(t.TempDir(), "transcripts", "acp")
	id := testCaptureIdentity()

	for i := 0; i < 2; i++ {
		c, err := openTranscriptCapture(root, id, nil)
		if err != nil {
			t.Fatalf("openTranscriptCapture #%d: %v", i, err)
		}
		c.record(captureOut, []byte(`{"n":`+itoa(i)+`}`))
		c.close(time.Second)
	}

	path := filepath.Join(root, "s-1", "2.jsonl")
	for _, dir := range []string{root, filepath.Dir(path)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, perm)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("transcript mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := parseCaptureLines(t, data)
	var shape []string
	for _, rec := range lines {
		if _, ok := rec["gc_acp_capture"]; ok {
			shape = append(shape, "header")
			continue
		}
		shape = append(shape, string(rec["msg"]))
	}
	want := []string{"header", `{"n":0}`, "header", `{"n":1}`}
	if strings.Join(shape, " ") != strings.Join(want, " ") {
		t.Fatalf("file shape = %v, want %v (restart in one epoch appends)", shape, want)
	}
}

func TestOpenTranscriptCapture_RejectsUnsafeIdentity(t *testing.T) {
	root := t.TempDir()
	id := testCaptureIdentity()
	id.SessionID = "../escape"
	if c, err := openTranscriptCapture(root, id, nil); err == nil {
		c.close(time.Second)
		t.Fatal("openTranscriptCapture accepted a session id with a path separator")
	}
}

func newCaptureTestProvider(t *testing.T, transcriptRoot string) *Provider {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "acp")
	return NewProviderWithDir(dir, Config{
		HandshakeTimeout:  5 * time.Second,
		NudgeBusyTimeout:  2 * time.Second,
		OutputBufferLines: 100,
		TranscriptRoot:    transcriptRoot,
	})
}

func captureSessionEnv() map[string]string {
	return map[string]string{
		"GC_SESSION_ID":         "s-1",
		"GC_CONTINUATION_EPOCH": "2",
		"GC_RUNTIME_EPOCH":      "3",
	}
}

// startPromptStop runs one agent lifetime: Start, one prompt round trip
// (waiting until the prompt response has been dispatched), then Stop, which
// flushes and closes the capture before it returns.
func startPromptStop(t *testing.T, p *Provider, name string, env map[string]string) {
	t.Helper()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
		Env:     env,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		_ = p.Stop(name)
		t.Fatalf("Nudge: %v", err)
	}
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if !sc.waitIdle(5 * time.Second) {
		_ = p.Stop(name)
		t.Fatal("prompt response never arrived")
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// captureShape renders each line as "header", "<dir> <method>" for requests
// and notifications, or "<dir> response" for responses.
func captureShape(t *testing.T, lines []map[string]json.RawMessage) []string {
	t.Helper()
	var shape []string
	for _, rec := range lines {
		if _, ok := rec["gc_acp_capture"]; ok {
			shape = append(shape, "header")
			continue
		}
		dir := rawString(t, rec["dir"])
		var msg struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(rec["msg"], &msg); err != nil {
			t.Fatalf("msg is not a JSON-RPC object: %s", rec["msg"])
		}
		if msg.Method != "" {
			shape = append(shape, dir+" "+msg.Method)
		} else {
			shape = append(shape, dir+" response")
		}
	}
	return shape
}

func TestStart_CapturesJSONRPCTranscript(t *testing.T) {
	root := t.TempDir()
	p := newCaptureTestProvider(t, root)
	name := testName()
	startPromptStop(t, p, name, captureSessionEnv())

	path := filepath.Join(root, "s-1", "2.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading capture: %v", err)
	}
	lines := parseCaptureLines(t, data)
	got := captureShape(t, lines)
	want := []string{
		"header",
		"out initialize",
		"in response",
		"out initialized",
		"out session/new",
		"in response",
		"out session/prompt",
		"in session/update",
		"in response",
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("capture shape:\n got  %v\n want %v", got, want)
	}
	h := lines[0]
	if rawString(t, h["session_id"]) != "s-1" || rawString(t, h["continuation_epoch"]) != "2" ||
		rawString(t, h["runtime_epoch"]) != "3" || rawString(t, h["session_name"]) != name {
		t.Fatalf("header identity = %v", h)
	}
	if string(h["pid"]) == "0" {
		t.Fatal("header pid = 0, want the agent process id")
	}
	if !strings.Contains(string(lines[7]["msg"]), `"echo: hello"`) {
		t.Fatalf("session/update msg = %s, want the agent's echoed text", lines[7]["msg"])
	}
}

func TestStart_CaptureRestartInSameEpochAppends(t *testing.T) {
	root := t.TempDir()
	p := newCaptureTestProvider(t, root)
	name := testName()
	startPromptStop(t, p, name, captureSessionEnv())
	startPromptStop(t, p, name, captureSessionEnv())

	data, err := os.ReadFile(filepath.Join(root, "s-1", "2.jsonl"))
	if err != nil {
		t.Fatalf("reading capture: %v", err)
	}
	headers := 0
	for _, s := range captureShape(t, parseCaptureLines(t, data)) {
		if s == "header" {
			headers++
		}
	}
	if headers != 2 {
		t.Fatalf("got %d headers, want 2 (one per agent start)", headers)
	}
}

func TestStart_CaptureDisabledWithoutRootOrSessionEnv(t *testing.T) {
	t.Run("no transcript root", func(t *testing.T) {
		p := newCaptureTestProvider(t, "")
		startPromptStop(t, p, testName(), captureSessionEnv())
		// Nothing to inspect: an empty root must not write relative to the
		// working directory.
		if _, err := os.Stat(filepath.Join("s-1", "2.jsonl")); err == nil {
			t.Fatal("capture wrote a relative path with an empty TranscriptRoot")
		}
	})
	for _, missing := range []string{"GC_SESSION_ID", "GC_CONTINUATION_EPOCH"} {
		t.Run("missing "+missing, func(t *testing.T) {
			root := t.TempDir()
			p := newCaptureTestProvider(t, root)
			env := captureSessionEnv()
			delete(env, missing)
			startPromptStop(t, p, testName(), env)
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("transcript root has %d entries, want none", len(entries))
			}
		})
	}
}
