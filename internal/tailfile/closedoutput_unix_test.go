//go:build unix

package tailfile

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestTailFollowEndsWhenItsOutputReaderCloses pins that a follow of a quiet
// file ends, with EPIPE, once the reader of its output pipe closes, as tail -f
// does: `gc supervisor logs -f | head -n 1` would otherwise run until killed.
func TestTailFollowEndsWhenItsOutputReaderCloses(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"quiet empty file", ""},
		{"after the reader took the tail", "last line\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, tc.content)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close() //nolint:errcheck
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Tail(ctx, path, 10, true, w, nil) }()
			if _, err := io.ReadFull(r, make([]byte, len(tc.content))); err != nil {
				t.Fatalf("reading the tail: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, syscall.EPIPE) {
				t.Fatalf("follow returned %v, want an error wrapping EPIPE", err)
			}
			if ctx.Err() != nil {
				t.Fatal("follow ended only when its safety deadline did")
			}
		})
	}
}
