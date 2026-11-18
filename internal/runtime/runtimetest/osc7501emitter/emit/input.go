package emit

import (
	"bytes"
	"context"
	"io"
	"time"
)

// The replies a probe-gated handshake reads: the 7501 reply (its introducer,
// a body, then BEL or ESC \) and the DA1 reply (ESC [ ? params c).
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

// awaitReply consumes the handshake's replies and reports whether a 7501
// reply came before the DA1 reply. A terminal answers in order, so the DA1
// reply ends the handshake; input ending, or wait passing, ends it sooner,
// answered only if a 7501 reply came by then. Every other byte stays
// pending, in order, for line reads.
func (i *input) awaitReply(ctx context.Context, wait time.Duration) (bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	answered := false
	for {
		is7501, ok := i.takeReply()
		switch {
		case ok && is7501:
			answered = true
		case ok:
			return answered, nil
		default:
			more, err := i.fill(ctx, timer.C)
			if err != nil {
				return false, err
			}
			if !more {
				return answered, nil
			}
		}
	}
}

// takeReply removes the first complete reply from pending, keeping the bytes
// around it, and reports whether it was a 7501 reply.
func (i *input) takeReply() (is7501, ok bool) {
	for at, c := range i.pending {
		if c != '\x1b' {
			continue
		}
		if n, is7501 := replyLen(i.pending[at:]); n > 0 {
			i.pending = append(i.pending[:at], i.pending[at+n:]...)
			return is7501, true
		}
	}
	return false, false
}

// dropReplies removes every complete reply from pending.
func (i *input) dropReplies() {
	for {
		if _, ok := i.takeReply(); !ok {
			return
		}
	}
}

// replyLen returns the length of the reply b starts with, and whether it is
// a 7501 reply; n is 0 when b starts with no reply, or with one not yet
// complete. A 7501 reply runs through its BEL or ESC \. An escape inside it
// that starts anything else interrupts it, so it is no reply, as the
// reader's Scanner abandons an interrupted sequence.
func replyLen(b []byte) (n int, is7501 bool) {
	if body, ok := bytes.CutPrefix(b, []byte(reply7501)); ok {
		switch end := bytes.IndexAny(body, terminatorBEL+"\x1b"); {
		case end < 0:
		case body[end] == terminatorBEL[0]:
			return len(b) - len(body) + end + len(terminatorBEL), true
		case bytes.HasPrefix(body[end:], []byte(terminatorST)):
			return len(b) - len(body) + end + len(terminatorST), true
		}
		return 0, false
	}
	if params, ok := bytes.CutPrefix(b, []byte(da1ReplyCSI)); ok {
		if rest := bytes.TrimLeft(params, "0123456789;"); len(rest) > 0 && rest[0] == 'c' {
			return len(b) - len(rest) + 1, false
		}
	}
	return 0, false
}

// line returns the next non-empty input line (CR or LF ends one); ok is
// false once input ends with nothing left. A terminal reply is never part
// of a line: one arriving after the handshake ended (a DA1 reply past the
// probe's wait, a 7501 reply after the DA1 reply) is dropped.
func (i *input) line(ctx context.Context) (string, bool, error) {
	for {
		i.dropReplies()
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
