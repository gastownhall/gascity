package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// defaultOutputBufferLines is the default circular buffer size for Peek output.
const defaultOutputBufferLines = 1000

// sessionConn tracks a running ACP agent process and its JSON-RPC connection.
type sessionConn struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	done     chan struct{}      // closed when process exits
	cancel   context.CancelFunc // cancels in-progress handshake (sentinel only, set by Start)
	listener net.Listener       // control socket for cross-process ops

	mu             sync.Mutex
	sessionID      string
	activePromptID int64 // non-zero when a prompt response is pending
	outputBuf      []string
	outputBufMax   int
	lastActivity   time.Time

	// stdinMu serializes writes to the agent's stdin pipe. Separate from
	// mu so that a slow/blocked stdin write cannot prevent dispatch (which
	// needs mu) from routing responses, avoiding a circular pipe deadlock.
	stdinMu sync.Mutex

	// nudgeMu serializes Nudge calls so that waitIdle → setActivePrompt →
	// sendRequest is atomic with respect to other Nudge calls.
	nudgeMu sync.Mutex

	// pending tracks response waiters by request ID.
	pending map[int64]chan JSONRPCMessage
}

// newSessionConn creates a sessionConn with the given buffer size.
func newSessionConn(cmd *exec.Cmd, stdin io.WriteCloser, lis net.Listener, bufSize int) *sessionConn {
	if bufSize <= 0 {
		bufSize = defaultOutputBufferLines
	}
	return &sessionConn{
		cmd:          cmd,
		stdin:        stdin,
		done:         make(chan struct{}),
		listener:     lis,
		outputBufMax: bufSize,
		pending:      make(map[int64]chan JSONRPCMessage),
	}
}

// readLoop reads JSON-RPC messages from the agent's stdout and dispatches them.
// It runs until the reader returns EOF or an error.
func (sc *sessionConn) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// ACP messages can be large (e.g., file contents in updates).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg JSONRPCMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // skip non-JSON lines (e.g., startup banners)
		}

		sc.dispatch(msg)
	}

	// readLoop exited (EOF, scanner error, or oversized frame). Log the
	// scanner error if present, then clear busy state and drain pending
	// channels so callers don't hang.
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "acp: readLoop exit: %v\n", err)
	}
	sc.drainPending()
}

// dispatch routes a decoded JSON-RPC message.
func (sc *sessionConn) dispatch(msg JSONRPCMessage) {
	// Notification (no ID): handle session/update.
	if msg.ID == nil && msg.Method == "session/update" {
		sc.handleUpdate(msg)
		return
	}

	// Response (has ID, no method): route to waiter.
	if msg.ID != nil && msg.Method == "" {
		sc.mu.Lock()
		ch, ok := sc.pending[*msg.ID]
		if ok {
			delete(sc.pending, *msg.ID)
		}
		// Clear busy state if this is the active prompt response.
		if sc.activePromptID != 0 && *msg.ID == sc.activePromptID {
			sc.activePromptID = 0
		}
		sc.mu.Unlock()
		if ok {
			ch <- msg
		}
		return
	}
}

// handleUpdate processes a session/update notification.
func (sc *sessionConn) handleUpdate(msg JSONRPCMessage) {
	var params SessionUpdateParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.lastActivity = time.Now()
	for _, block := range params.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		// Split multi-line text into individual lines for the buffer.
		lines := strings.Split(block.Text, "\n")
		for _, line := range lines {
			sc.appendLine(line)
		}
	}
}

// appendLine adds a line to the circular output buffer. Caller must hold mu.
func (sc *sessionConn) appendLine(line string) {
	if len(sc.outputBuf) >= sc.outputBufMax {
		// Shift buffer: drop oldest line.
		copy(sc.outputBuf, sc.outputBuf[1:])
		sc.outputBuf[len(sc.outputBuf)-1] = line
	} else {
		sc.outputBuf = append(sc.outputBuf, line)
	}
}

// sendRequest encodes a JSON-RPC message to the agent's stdin and registers
// a response waiter. Returns the response channel.
func (sc *sessionConn) sendRequest(msg JSONRPCMessage) (chan JSONRPCMessage, error) {
	if msg.ID == nil {
		return nil, sc.sendNotification(msg)
	}

	ch := make(chan JSONRPCMessage, 1)
	sc.mu.Lock()
	sc.pending[*msg.ID] = ch
	sc.mu.Unlock()

	data, err := json.Marshal(msg)
	if err != nil {
		sc.mu.Lock()
		delete(sc.pending, *msg.ID)
		sc.mu.Unlock()
		return nil, fmt.Errorf("marshal: %w", err)
	}

	sc.stdinMu.Lock()
	_, err = fmt.Fprintf(sc.stdin, "%s\n", data)
	sc.stdinMu.Unlock()
	if err != nil {
		sc.mu.Lock()
		delete(sc.pending, *msg.ID)
		sc.mu.Unlock()
		return nil, fmt.Errorf("write: %w", err)
	}

	return ch, nil
}

// sendNotification encodes a JSON-RPC notification (no response expected).
func (sc *sessionConn) sendNotification(msg JSONRPCMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	sc.stdinMu.Lock()
	_, err = fmt.Fprintf(sc.stdin, "%s\n", data)
	sc.stdinMu.Unlock()
	return err
}

// setActivePrompt marks the given request ID as the active prompt.
func (sc *sessionConn) setActivePrompt(id int64) {
	sc.mu.Lock()
	sc.activePromptID = id
	sc.mu.Unlock()
}

// drainPending clears busy state and closes all pending response channels.
// Safe to call multiple times — closed channels are deleted from the map.
func (sc *sessionConn) drainPending() {
	sc.mu.Lock()
	sc.activePromptID = 0
	for id, ch := range sc.pending {
		close(ch)
		delete(sc.pending, id)
	}
	sc.mu.Unlock()
}

// isBusy reports whether a prompt response is pending.
func (sc *sessionConn) isBusy() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.activePromptID != 0
}

// waitIdle polls until the agent is not busy or the timeout expires.
// Returns true if the agent became idle, false on timeout.
func (sc *sessionConn) waitIdle(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		if !sc.isBusy() {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// peekLines returns the last n lines from the output buffer.
// If n <= 0, returns all lines.
func (sc *sessionConn) peekLines(n int) string {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	lines := sc.outputBuf
	if n > 0 && n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// clearOutput resets the output buffer.
func (sc *sessionConn) clearOutput() {
	sc.mu.Lock()
	sc.outputBuf = sc.outputBuf[:0]
	sc.mu.Unlock()
}

// getLastActivity returns the time of the last session/update notification.
func (sc *sessionConn) getLastActivity() time.Time {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.lastActivity
}

// alive reports whether the process is still running.
func (sc *sessionConn) alive() bool {
	select {
	case <-sc.done:
		return false
	default:
		return true
	}
}

// limitedWriter is a thread-safe io.Writer that keeps only the last max bytes.
type limitedWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	w.mu.Unlock()
	return len(p), nil
}

// String returns the captured bytes as a string.
func (w *limitedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
