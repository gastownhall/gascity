package tailfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// lastLinesCases' wants are what GNU tail -n writes for each case, checked
// against coreutils 8.32 and 9.4.
var lastLinesCases = []struct {
	name    string
	content string
	lines   int
	want    string
}{
	{"trailing newline", "a\nb\nc\n", 2, "b\nc\n"},
	{"unterminated last line counts as a line", "a\nb\nc", 2, "b\nc"},
	{"more lines than the file has", "a\nb\n", 5, "a\nb\n"},
	{"zero lines", "a\nb\n", 0, ""},
	{"negative count means its magnitude", "a\nb\nc\n", -1, "c\n"},
	{"empty file", "", 3, ""},
	{"blank lines count", "\n\n\n", 2, "\n\n"},
	{"one unterminated line", "only", 1, "only"},
	{"crlf is bytes, not a line ending", "a\r\nb\r\n", 1, "b\r\n"},
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTailWritesLastLines(t *testing.T) {
	defer func(previous int) { scanChunk = previous }(scanChunk)
	// The small chunk sizes put line boundaries on every side of a chunk edge.
	for _, chunk := range []int{scanChunk, 1, 2, 3} {
		scanChunk = chunk
		for _, tc := range lastLinesCases {
			path := writeFile(t, tc.content)
			var out bytes.Buffer
			if err := Tail(context.Background(), path, tc.lines, false, &out, nil); err != nil {
				t.Fatalf("chunk %d, %s: %v", chunk, tc.name, err)
			}
			if out.String() != tc.want {
				t.Errorf("chunk %d, %s: wrote %q, want %q", chunk, tc.name, out.String(), tc.want)
			}
		}
	}
}

func TestTailReportsMissingFile(t *testing.T) {
	err := Tail(context.Background(), filepath.Join(t.TempDir(), "absent"), 10, false, &bytes.Buffer{}, nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Tail = %v, want a not-exist error", err)
	}
}

// syncBuffer is a bytes.Buffer safe for a follower writing while the test
// reads, which signals changed after each write.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{changed: make(chan struct{}, 1)}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput waits, waking on each write, until the follower has written
// exactly want.
func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for out.String() != want {
		select {
		case <-out.changed:
		case <-deadline.C:
			t.Fatalf("follower wrote %q, want %q", out.String(), want)
		}
	}
}

// startFollow runs a following Tail and returns a stop function that ends it
// and reports what it returned.
func startFollow(t *testing.T, path string, lines int, out *syncBuffer, onTruncate func()) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Tail(ctx, path, lines, true, out, onTruncate) }()
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("follow did not return after its context ended")
			return nil
		}
	}
	// The follower holds the file open; it must be gone before TempDir
	// cleanup removes the file, which Windows refuses while it is open.
	t.Cleanup(func() { _ = stop() })
	return stop
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestTailFollowWritesAppends(t *testing.T) {
	path := writeFile(t, "one\ntwo\n")
	out := newSyncBuffer()
	stop := startFollow(t, path, 1, out, nil)
	waitForOutput(t, out, "two\n")
	appendTo(t, path, "three\n")
	waitForOutput(t, out, "two\nthree\n")
	appendTo(t, path, "four, unterminated")
	waitForOutput(t, out, "two\nthree\nfour, unterminated")
	if err := stop(); err != nil {
		t.Fatalf("follow returned %v, want nil when its context ends", err)
	}
}

func TestTailFollowRereadsTruncatedFile(t *testing.T) {
	path := writeFile(t, "a line from before the truncation\n")
	out := newSyncBuffer()
	var mu sync.Mutex
	truncations := 0
	stop := startFollow(t, path, 10, out, func() {
		mu.Lock()
		truncations++
		mu.Unlock()
	})
	waitForOutput(t, out, "a line from before the truncation\n")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, out, "a line from before the truncation\nafter\n")
	if err := stop(); err != nil {
		t.Fatalf("follow returned %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if truncations != 1 {
		t.Fatalf("onTruncate called %d times, want 1", truncations)
	}
}
