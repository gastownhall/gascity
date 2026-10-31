package emit

import (
	"bytes"
	"context"
	"io"
	"time"
)

// The replies probe-gated tells apart.
const (
	reply7501   = "\x1b]7501;"
	da1ReplyCSI = "\x1b[?"
)

// input pumps the terminal's input in one goroutine, so a probe wait can
// time out without losing bytes a later line read needs. The pump starts at
// the first read, so a terminal put in raw mode for a probe is read raw from
// the start.
type input struct {
	in      io.Reader
	chunks  chan []byte
	pending []byte
	eof     bool
}

// newInput returns in's pump, not yet started; a nil in reads as ended.
func newInput(in io.Reader) *input {
	return &input{in: in, eof: in == nil}
}

// start starts the pump once.
func (i *input) start() {
	if i.chunks != nil {
		return
	}
	i.chunks = make(chan []byte, 16)
	go func() {
		defer close(i.chunks)
		for {
			buf := make([]byte, 4096)
			n, err := i.in.Read(buf)
			if n > 0 {
				i.chunks <- buf[:n]
			}
			if err != nil {
				return
			}
		}
	}()
}

// fill waits up to timeout (forever when timeout is nil) for more input. It
// reports false when none came: the input ended, or the wait timed out.
func (i *input) fill(ctx context.Context, timeout <-chan time.Time) (bool, error) {
	if i.eof {
		return false, nil
	}
	i.start()
	select {
	case b, ok := <-i.chunks:
		if !ok {
			i.eof = true
			return false, nil
		}
		i.pending = append(i.pending, b...)
		return true, nil
	case <-timeout:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// awaitReply consumes input until a 7501 reply or a DA1 reply completes, and
// reports whether the 7501 reply came first. Input ending, or wait passing,
// with neither reads as unanswered. Bytes after the deciding reply stay
// pending for line reads.
func (i *input) awaitReply(ctx context.Context, wait time.Duration) (bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		if answered, used, ok := firstReply(i.pending); ok {
			i.pending = i.pending[used:]
			return answered, nil
		}
		more, err := i.fill(ctx, timer.C)
		if err != nil || !more {
			return false, err
		}
	}
}

// firstReply finds the first complete reply in b: a 7501 reply (its
// introducer suffices) or a DA1 reply (CSI ? params c). used is how many
// bytes it and what preceded it took.
func firstReply(b []byte) (answered bool, used int, ok bool) {
	for at := 0; at < len(b); at++ {
		rest := b[at:]
		if bytes.HasPrefix(rest, []byte(reply7501)) {
			return true, at + len(reply7501), true
		}
		if !bytes.HasPrefix(rest, []byte(da1ReplyCSI)) {
			continue
		}
		for j := len(da1ReplyCSI); j < len(rest); j++ {
			c := rest[j]
			if c == 'c' {
				return false, at + j + 1, true
			}
			if (c < '0' || c > '9') && c != ';' {
				break
			}
		}
	}
	return false, 0, false
}

// line returns the next non-empty input line (CR or LF ends one); ok is
// false once input ends with nothing left.
func (i *input) line(ctx context.Context) (string, bool, error) {
	for {
		if at := bytes.IndexAny(i.pending, "\r\n"); at >= 0 {
			text := string(i.pending[:at])
			i.pending = i.pending[at+1:]
			if text == "" {
				continue
			}
			return text, true, nil
		}
		more, err := i.fill(ctx, nil)
		if err != nil {
			return "", false, err
		}
		if !more {
			text := string(i.pending)
			i.pending = nil
			return text, text != "", nil
		}
	}
}
