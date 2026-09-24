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
		c, err := openTranscriptCapture(root, id)
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
	if c, err := openTranscriptCapture(root, id); err == nil {
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

// startPromptStop runs one agent lifetime of the default fake agent: see
// runPromptStop.
func startPromptStop(t *testing.T, p *Provider, name string, env map[string]string) {
	t.Helper()
	runPromptStop(t, p, name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
		Env:     env,
	})
}

// runPromptStop runs one agent lifetime: Start, one prompt round trip
// (waiting until the prompt response has been dispatched), then Stop, which
// flushes and closes the capture before it returns. It asserts the process
// monitor released the capture (writer finished, file closed) by the time
// Stop returned.
func runPromptStop(t *testing.T, p *Provider, name string, cfg runtime.Config) {
	t.Helper()
	if err := p.Start(context.Background(), name, cfg); err != nil {
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
	if sc.capture != nil {
		select {
		case <-sc.capture.done:
		default:
			t.Fatal("capture writer still running after Stop: the process monitor did not close the capture")
		}
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

// TestStart_CaptureRedactsSessionNewMCPCredentials proves MCP server
// credentials handed to the agent in session/new never reach the durable
// transcript, and that the redacted record is marked so readers know msg is
// not the exact wire bytes.
func TestStart_CaptureRedactsSessionNewMCPCredentials(t *testing.T) {
	root := t.TempDir()
	p := newCaptureTestProvider(t, root)
	runPromptStop(t, p, testName(), runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
		Env:     captureSessionEnv(),
		MCPServers: []runtime.MCPServerConfig{
			{
				Name:      "remote",
				Transport: runtime.MCPTransportHTTP,
				URL:       "https://probeuser:PROBE-URL-PW@example.invalid/mcp?key=PROBE-QUERY",
				Headers:   map[string]string{"Authorization": "Bearer PROBE-HEADER-TOKEN"},
			},
			{
				Name:      "local",
				Transport: runtime.MCPTransportStdio,
				Command:   "probe-mcp",
				Args:      []string{"--api-key", "PROBE-ARG-KEY"},
				Env:       map[string]string{"GITHUB_TOKEN": "PROBE-ENV-TOKEN", "PLAIN": "PROBE-PLAIN-ENV"},
			},
		},
	})

	data, err := os.ReadFile(filepath.Join(root, "s-1", "2.jsonl"))
	if err != nil {
		t.Fatalf("reading capture: %v", err)
	}
	for _, secret := range []string{
		"PROBE-URL-PW", "probeuser", "PROBE-QUERY", "PROBE-HEADER-TOKEN",
		"PROBE-ARG-KEY", "PROBE-ENV-TOKEN", "PROBE-PLAIN-ENV",
	} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("capture contains MCP credential %q", secret)
		}
	}
	var sessionNew map[string]json.RawMessage
	for _, rec := range parseCaptureLines(t, data) {
		if strings.Contains(string(rec["msg"]), `"method":"session/new"`) {
			sessionNew = rec
		}
	}
	if sessionNew == nil {
		t.Fatal("no session/new record captured")
	}
	if string(sessionNew["redacted"]) != "true" {
		t.Fatalf("session/new record redacted = %s, want true", sessionNew["redacted"])
	}
	msg := string(sessionNew["msg"])
	for _, kept := range []string{`"remote"`, `"local"`, `"probe-mcp"`, `"GITHUB_TOKEN"`, runtime.RedactedMCPValue} {
		if !strings.Contains(msg, kept) {
			t.Errorf("redacted session/new msg lost %s: %s", kept, msg)
		}
	}
}

// recordingStdin is an agent stdin stub. When check is set it runs before
// each write is accepted.
type recordingStdin struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	check func()
}

func (w *recordingStdin) Write(p []byte) (int, error) {
	if w.check != nil {
		w.check()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingStdin) Close() error { return nil }

// TestSessionConn_RedactedCaptureKeepsWireBytes proves redaction applies to the
// capture only: the agent still receives the real MCP credentials.
func TestSessionConn_RedactedCaptureKeepsWireBytes(t *testing.T) {
	stdin := &recordingStdin{}
	sc := newSessionConn(nil, stdin, nil, 100, nil)
	sink := newCaptureSink()
	sc.capture = newTranscriptCapture(sink, 16, "worker-1", nil)

	servers := []runtime.MCPServerConfig{{
		Name:      "local",
		Transport: runtime.MCPTransportStdio,
		Command:   "probe-mcp",
		Env:       map[string]string{"GITHUB_TOKEN": "WIRE-SECRET"},
	}}
	req, _ := newSessionNewRequest("/work", servers)
	redacted, err := redactedSessionNewParams("/work", servers)
	if err != nil {
		t.Fatalf("redactedSessionNewParams: %v", err)
	}
	if _, err := sc.sendRequestRedacted(req, redacted); err != nil {
		t.Fatalf("sendRequestRedacted: %v", err)
	}
	sc.capture.close(time.Second)

	if !strings.Contains(stdin.buf.String(), "WIRE-SECRET") {
		t.Fatalf("agent stdin lost the real credential: %s", stdin.buf.String())
	}
	lines := sink.lines(t)
	if len(lines) != 1 {
		t.Fatalf("got %d capture lines, want 1", len(lines))
	}
	if strings.Contains(string(lines[0]["msg"]), "WIRE-SECRET") {
		t.Fatalf("capture holds the credential: %s", lines[0]["msg"])
	}
	var wire, captured JSONRPCMessage
	if err := json.Unmarshal(bytes.TrimSpace(stdin.buf.Bytes()), &wire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[0]["msg"], &captured); err != nil {
		t.Fatal(err)
	}
	if captured.ID == nil || wire.ID == nil || *captured.ID != *wire.ID || captured.Method != wire.Method {
		t.Fatalf("captured envelope %+v does not match wire envelope %+v", captured, wire)
	}
}

// TestSessionConn_OutRecordQueuedBeforeStdinWrite pins the ordering contract:
// the out record is already queued when the stdin write happens, so it can
// never land after the agent's reply.
func TestSessionConn_OutRecordQueuedBeforeStdinWrite(t *testing.T) {
	sink := newCaptureSink()
	gate := make(chan struct{})
	sink.gate = gate
	sink.entered = make(chan struct{}, 1)
	capture := newTranscriptCapture(sink, 16, "worker-1", nil)
	// Park the writer inside Write on a first record so the queue length
	// observed below counts only records queued after it.
	capture.record(captureIn, []byte(`{"park":true}`))
	<-sink.entered

	var writes, queuedAtWrite int
	stdin := &recordingStdin{check: func() {
		writes++
		queuedAtWrite = len(capture.queue)
	}}
	sc := newSessionConn(nil, stdin, nil, 100, nil)
	sc.capture = capture
	if err := sc.sendNotification(newInitializedNotification()); err != nil {
		t.Fatalf("sendNotification: %v", err)
	}
	close(gate)
	capture.close(time.Second)
	if writes == 0 {
		t.Fatal("stdin was never written")
	}
	if queuedAtWrite != 1 {
		t.Fatalf("queued records at stdin write = %d, want 1 (out record queued before the write)", queuedAtWrite)
	}
}

// TestReadLoop_CaptureOwnsLineBytes feeds the read loop far more than the
// scanner's 64KiB buffer while the capture writer is held, so the scanner must
// reuse its buffer before any record is written. Every captured msg must still
// equal the line that was read.
func TestReadLoop_CaptureOwnsLineBytes(t *testing.T) {
	const n = 300
	sink := newCaptureSink()
	gate := make(chan struct{})
	sink.gate = gate
	sink.entered = make(chan struct{}, 1)
	sc := newSessionConn(nil, nil, nil, 100, nil)
	sc.capture = newTranscriptCapture(sink, 2*n, "worker-1", nil)

	var input bytes.Buffer
	want := make([]string, n)
	for i := 0; i < n; i++ {
		want[i] = `{"jsonrpc":"2.0","method":"test/fill","params":{"n":` + itoa(i) +
			`,"pad":"` + strings.Repeat(string(rune('a'+i%26)), 1024) + `"}}`
		input.WriteString(want[i])
		input.WriteByte('\n')
	}
	if input.Len() <= 2*64*1024 {
		t.Fatalf("input is %d bytes; must exceed the scanner buffer several times", input.Len())
	}
	sc.readLoop(&input)
	close(gate)
	sc.capture.close(5 * time.Second)

	lines := sink.lines(t)
	if len(lines) != n {
		t.Fatalf("got %d capture lines, want %d", len(lines), n)
	}
	for i, rec := range lines {
		if got := string(rec["msg"]); got != want[i] {
			t.Fatalf("line %d msg corrupted:\n got  %.120s\n want %.120s", i, got, want[i])
		}
	}
}

// oddLinesFakeCommand answers the handshake, then on session/prompt emits a
// non-JSON banner, an agent-to-client request with a string id (valid
// JSON-RPC: ACP RequestId is null, number or string), a JSON array line, a
// control session/update and the prompt response.
func oddLinesFakeCommand() string {
	return `exec python3 -u -c '
import sys, json
def out(o):
    print(json.dumps(o), flush=True)
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    m = msg.get("method", "")
    i = msg.get("id")
    if m == "initialize":
        out({"jsonrpc":"2.0","id":i,"result":{"protocolVersion":1}})
    elif m == "session/new":
        out({"jsonrpc":"2.0","id":i,"result":{"sessionId":"sess-1"}})
    elif m == "session/prompt":
        print("BANNER this is not json", flush=True)
        out({"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"sess-1","toolCall":{"toolCallId":"t1"},"options":[]}})
        print("[1,2,3]", flush=True)
        out({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"CONTROL"}}}})
        out({"jsonrpc":"2.0","id":i,"result":{"stopReason":"end_turn"}})
'`
}

// TestStart_CapturesEveryJSONObjectLine proves the capture records any JSON
// object the agent prints, including JSON-RPC messages gc's own decoder does
// not accept (a string id), and skips non-object lines.
func TestStart_CapturesEveryJSONObjectLine(t *testing.T) {
	root := t.TempDir()
	p := newCaptureTestProvider(t, root)
	runPromptStop(t, p, testName(), runtime.Config{
		Command: oddLinesFakeCommand(),
		WorkDir: t.TempDir(),
		Env:     captureSessionEnv(),
	})
	data, err := os.ReadFile(filepath.Join(root, "s-1", "2.jsonl"))
	if err != nil {
		t.Fatalf("reading capture: %v", err)
	}
	got := strings.Join(captureShape(t, parseCaptureLines(t, data)), " | ")
	want := strings.Join([]string{
		"header",
		"out initialize",
		"in response",
		"out initialized",
		"out session/new",
		"in response",
		"out session/prompt",
		"in session/request_permission",
		"in session/update",
		"in response",
	}, " | ")
	if got != want {
		t.Fatalf("capture shape:\n got  %s\n want %s", got, want)
	}
	for _, skipped := range []string{"BANNER", "[1,2,3]"} {
		if bytes.Contains(data, []byte(skipped)) {
			t.Errorf("capture recorded non-object stdout line %q", skipped)
		}
	}
	if !bytes.Contains(data, []byte(`"id": "perm-1"`)) && !bytes.Contains(data, []byte(`"id":"perm-1"`)) {
		t.Fatal("string-id request not captured")
	}
}

// TestOpenTranscriptCapture_TornTailStartsFreshLine proves a restart appends
// its header on a new line when an earlier writer died mid-record, so the
// header (the only boundary between agent starts) still parses.
func TestOpenTranscriptCapture_TornTailStartsFreshLine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "transcripts", "acp")
	id := testCaptureIdentity()
	c, err := openTranscriptCapture(root, id)
	if err != nil {
		t.Fatal(err)
	}
	c.record(captureIn, []byte(`{"jsonrpc":"2.0","id":3,"result":{}}`))
	c.close(time.Second)

	path := filepath.Join(root, "s-1", "2.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const torn = `{"ts":"2026-09-24T00:00:00Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/upd`
	if _, err := f.WriteString(torn); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := openTranscriptCapture(root, id)
	if err != nil {
		t.Fatal(err)
	}
	c2.record(captureOut, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	c2.close(time.Second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var shape []string
	for _, l := range lines {
		var obj map[string]json.RawMessage
		switch {
		case l == torn:
			shape = append(shape, "torn")
		case json.Unmarshal([]byte(l), &obj) != nil:
			shape = append(shape, "garbage")
		case obj["gc_acp_capture"] != nil:
			shape = append(shape, "header")
		default:
			shape = append(shape, "record")
		}
	}
	if got, want := strings.Join(shape, " "), "header record torn header record"; got != want {
		t.Fatalf("file shape = %q, want %q", got, want)
	}
}

// TestOpenTranscriptCapture_CleanTailAddsNoBlankLine is the control for the
// torn-tail repair: a file ending in a newline gets no extra line.
func TestOpenTranscriptCapture_CleanTailAddsNoBlankLine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "transcripts", "acp")
	id := testCaptureIdentity()
	for i := 0; i < 2; i++ {
		c, err := openTranscriptCapture(root, id)
		if err != nil {
			t.Fatal(err)
		}
		c.close(time.Second)
	}
	data, err := os.ReadFile(filepath.Join(root, "s-1", "2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\n\n")) || bytes.HasPrefix(data, []byte("\n")) {
		t.Fatalf("capture has a blank line: %q", data)
	}
}
