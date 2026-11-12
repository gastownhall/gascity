package relay

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// da1Request and da1Reply are the DA1 exchange the harness answers, as tmux
// does.
const (
	da1Request = "\x1b[c"
	da1Reply   = "\x1b[?1;2c"
)

// recorder records each write the bridge makes to the program's input.
type recorder struct {
	mu     sync.Mutex
	writes []string
	grew   chan struct{}
	block  chan struct{} // when set, every write waits for it to close
	err    error         // when set, every write fails with it
	failed chan struct{} // closed at the first failed write
}

func newRecorder() *recorder {
	return &recorder{grew: make(chan struct{}), failed: make(chan struct{})}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		select {
		case <-r.failed:
		default:
			close(r.failed)
		}
		return 0, r.err
	}
	r.writes = append(r.writes, string(b))
	close(r.grew)
	r.grew = make(chan struct{})
	return len(b), nil
}

// await waits until the program's input holds want.
func (r *recorder) await(t *testing.T, want string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		got, grew := strings.Join(r.writes, ""), r.grew
		r.mu.Unlock()
		if len(got) >= len(want) {
			if got != want {
				t.Fatalf("program input %q, want %q", got, want)
			}
			return
		}
		select {
		case <-grew:
		case <-deadline.C:
			t.Fatalf("program input %q, want %q", got, want)
		}
	}
}

// reads is a reader returning one chunk per Read, then EOF.
type reads []string

func (r *reads) Read(b []byte) (int, error) {
	if len(*r) == 0 {
		return 0, io.EOF
	}
	n := copy(b, (*r)[0])
	if n < len((*r)[0]) {
		(*r)[0] = (*r)[0][n:]
	} else {
		*r = (*r)[1:]
	}
	return n, nil
}

// paneAnsweringDA1 is the outer terminal: it collects the forwarded output
// and, the moment a forwarded chunk carries a DA1 request, types the DA1
// reply, as tmux does.
type paneAnsweringDA1 struct {
	out   bytes.Buffer
	typed *io.PipeWriter
}

func (p *paneAnsweringDA1) Write(b []byte) (int, error) {
	p.out.Write(b)
	if bytes.Contains(b, []byte(da1Request)) {
		if _, err := p.typed.Write([]byte(da1Reply)); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// TestBridgeQueuesReplyAheadOfDA1Answer drives the bridge with no ptys and
// a pane that answers DA1 synchronously inside the forward of the chunk
// carrying the request: the earliest an answer can exist. The program must
// still read the 7501 reply first.
func TestBridgeQueuesReplyAheadOfDA1Answer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output reads
	}{
		{"probe and DA1 in one read", reads{"\x1b]7501;?\x07\x1b[c"}},
		{"probe and DA1 in separate reads", reads{"\x1b]7501;?\x07", "\x1b[c"}},
		{"ST probe split inside its terminator", reads{"\x1b]7501;?\x1b", "\\\x1b[c"}},
		{"probe split byte by byte", reads{"\x1b", "]", "7", "5", "0", "1", ";", "?", "\x07", "\x1b[c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newRecorder()
			b := NewBridge(in)
			defer b.Close()
			typedR, typedW := io.Pipe()
			defer typedW.Close() //nolint:errcheck // test cleanup
			inputDone := make(chan error, 1)
			go func() { inputDone <- b.Input(typedR) }()
			pane := &paneAnsweringDA1{typed: typedW}
			output := tc.output
			if err := b.Output(&output, pane); err != nil {
				t.Fatalf("Output: %v", err)
			}
			in.await(t, ProbeReply+da1Reply)
			if got := pane.out.String(); got != strings.Join(tc.output, "") {
				t.Fatalf("pane got %q, want the output verbatim", got)
			}
		})
	}
}

func TestBridgeAnswersEveryProbeAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		output  string
		replies int
	}{
		{"BEL probe", "\x1b]7501;?\x07", 1},
		{"ST probe", "\x1b]7501;?\x1b\\", 1},
		{"two probes in one read", "\x1b]7501;?\x07text\x1b]7501;?\x1b\\", 2},
		{"a report", "\x1b]7501;state=working\x07", 0},
		{"a probe with a body", "\x1b]7501;?x\x07", 0},
		{"another OSC", "\x1b]7502;?\x07", 0},
		{"a probe inside DCS passthrough", "\x1bPtmux;\x1b\x1b]7501;?\x07\x1b\\", 0},
		{"a probe interrupted by ESC", "\x1b]750\x1b]7501;?\x07", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newRecorder()
			b := NewBridge(in)
			var pane bytes.Buffer
			output := reads{tc.output}
			if err := b.Output(&output, &pane); err != nil {
				t.Fatal(err)
			}
			const marker = "typed"
			if err := b.Input(strings.NewReader(marker)); err != nil {
				t.Fatal(err)
			}
			in.await(t, strings.Repeat(ProbeReply, tc.replies)+marker)
			b.Close()
			if pane.String() != tc.output {
				t.Fatalf("pane got %q, want %q", pane.String(), tc.output)
			}
		})
	}
}

// A reply is written whole between input reads, never inside one.
func TestBridgeNeverSplitsInputForAReply(t *testing.T) {
	in := newRecorder()
	b := NewBridge(in)
	defer b.Close()
	typedR, typedW := io.Pipe()
	inputDone := make(chan error, 1)
	go func() { inputDone <- b.Input(typedR) }()
	const chunks, size, probes = 200, 3000, 100
	go func() {
		chunk := bytes.Repeat([]byte("a"), size)
		for range chunks {
			if _, err := typedW.Write(chunk); err != nil {
				return
			}
		}
		_ = typedW.Close()
	}()
	var output reads
	for range probes {
		output = append(output, "\x1b]7501;?\x07")
	}
	if err := b.Output(&output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := <-inputDone; err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		in.mu.Lock()
		writes, grew := append([]string(nil), in.writes...), in.grew
		in.mu.Unlock()
		typed, replies := 0, 0
		for _, w := range writes {
			switch {
			case w == ProbeReply:
				replies++
			case strings.Trim(w, "a") == "":
				typed += len(w)
			default:
				t.Fatalf("write %q mixes a reply into typed input", w)
			}
		}
		if typed == chunks*size && replies > 0 {
			if replies > probes {
				t.Fatalf("%d replies for %d probes", replies, probes)
			}
			return
		}
		select {
		case <-grew:
		case <-deadline.C:
			t.Fatalf("program got %d typed bytes and %d replies", typed, replies)
		}
	}
}

// The output path never waits on the program's input: a program that stops
// reading still has all its output forwarded.
func TestBridgeOutputNeverBlocksOnProgramInput(t *testing.T) {
	in := newRecorder()
	in.block = make(chan struct{})
	b := NewBridge(in)
	defer close(in.block)
	defer b.Close()
	var output reads
	for range 10 * maxQueuedReplies {
		output = append(output, "\x1b]7501;?\x07")
	}
	want := strings.Join(output, "")
	var pane bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- b.Output(&output, &pane) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("output blocked behind a program that does not read its input")
	}
	if pane.String() != want {
		t.Fatal("output not forwarded verbatim")
	}
}

func TestBridgeOutputReportsTerminalGone(t *testing.T) {
	b := NewBridge(newRecorder())
	defer b.Close()
	output := reads{"x"}
	err := b.Output(&output, failingWriter{})
	if !errors.Is(err, ErrTerminalGone) || !errors.Is(err, errBroken) {
		t.Fatalf("Output = %v, want ErrTerminalGone wrapping the write error", err)
	}
	if err := b.Output(failingReader{}, io.Discard); err == nil || errors.Is(err, ErrTerminalGone) {
		t.Fatalf("Output on a failing read = %v, want the read error", err)
	}
}

func TestBridgeInputReportsProgramGone(t *testing.T) {
	in := newRecorder()
	in.err = errBroken
	b := NewBridge(in)
	defer b.Close()
	typedR, typedW := io.Pipe()
	defer typedW.Close() //nolint:errcheck // test cleanup
	done := make(chan error, 1)
	go func() { done <- b.Input(typedR) }()
	if _, err := typedW.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	<-in.failed
	// The next read finds the program's input closed.
	if _, err := typedW.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrProgramGone) || !errors.Is(err, errBroken) {
		t.Fatalf("Input = %v, want ErrProgramGone wrapping the write error", err)
	}
}

var errBroken = errors.New("broken")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBroken }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errBroken }
