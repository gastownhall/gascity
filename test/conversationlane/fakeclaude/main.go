// Command fakeclaude is a scripted Claude Code-like terminal UI for the
// conversation-lane evidence tests (test/conversationlane): it draws the
// screen gc's tmux runtime reads as Claude Code's, takes input the way
// Claude Code does, and records what it was typed and when.
//
//	fakeclaude --state <dir> [claude flags...]
//
// The flags gc passes the claude family are ignored. The program runs as
// the session's agent in a tmux pane under the name claude, so gc's process
// probes find it.
//
// The screen is Claude Code's input box (the ready prompt "❯ " between two
// full-width rules, the cursor a reverse-video cell) under its transcript.
// During a turn it adds the spinner row with its elapsed timer and "esc to
// interrupt", redrawn every second as Claude Code's is, and the footer says
// "esc to interrupt" too; when idle it draws nothing until something
// changes.
//
// Input: text typed or pasted (bracketed paste is enabled) goes into the
// input box, a line feed as a new line of it; Ctrl-U empties it and
// Backspace deletes a character. Enter (a carriage return) on an idle input
// box submits its text, which starts a short reply turn, the way Claude Code
// goes busy on a submit. Enter during a turn queues the text as Claude Code
// does: the message shows above the input box and the input box shows
// Claude's hint "Press up to edit queued messages" with the cursor drawn on
// its first character. A turn ending with messages queued starts a reply
// turn for them. Escape during a turn interrupts it.
//
// The test starts a long turn by writing its length in seconds to
// <dir>/turn. Every event is appended to <dir>/events.jsonl as one JSON line
// (Event): ready, turn_start, turn_end, submit, queued, escape, and
// input_during_turn for any input but a bare Enter that arrives while a turn
// runs.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Event is one line of events.jsonl.
type Event struct {
	At    string `json:"at"`
	Event string `json:"event"`
	// Text is what was submitted or queued, or the input that arrived
	// during a turn.
	Text string `json:"text,omitempty"`
	// Turn reports whether a turn was running when the event happened.
	Turn bool `json:"turn"`
	// Seconds is a turn_start's length.
	Seconds float64 `json:"seconds,omitempty"`
}

const (
	// replyTurn is how long the reply to a submitted message runs.
	replyTurn = 2 * time.Second
	// tick is how often the program looks at its control file and clock.
	tick = 100 * time.Millisecond
	// spinnerRedraw is how often a running turn redraws its spinner row.
	spinnerRedraw = time.Second
	// defaultWidth and defaultHeight are the screen when the terminal size
	// cannot be read.
	defaultWidth, defaultHeight = 80, 24
	// cursorCell draws the cursor on an empty input box.
	cursorCell = "\x1b[7m \x1b[27m"
	// queuedHint is Claude Code's input box while messages wait behind a
	// running turn, the cursor on the hint's first character.
	queuedHint = "\x1b[7mP\x1b[27m\x1b[2mress up to edit queued messages\x1b[22m"
	// promptPrefix is Claude Code's ready prompt; it draws the space after
	// the glyph as a no-break space.
	promptPrefix = "\u276f\u00a0"
	ctrlU        = 0x15
	backspace    = 0x7f
	ctrlH        = 0x08
	esc          = 0x1b
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	dir := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--state" {
			dir = args[i+1]
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "usage: fakeclaude --state <dir> [claude flags...]") //nolint:errcheck // usage
		return 1
	}
	u, err := newTUI(dir, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		saved, err := term.MakeRaw(fd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: raw mode: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer term.Restore(fd, saved) //nolint:errcheck // best effort on exit
		if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
			u.width, u.height = w, h
		}
	}
	if err := u.loop(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "fakeclaude: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// tui is the program's state. Only loop's goroutine touches it.
type tui struct {
	dir           string
	out           io.Writer
	width, height int
	transcript    []string
	input         []byte
	queued        []string
	pasting       bool
	// turnStart and turnEnd bound the running turn; turnEnd is zero when
	// idle.
	turnStart, turnEnd time.Time
	lastDraw           time.Time
}

func newTUI(dir string, out io.Writer) (*tui, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the state dir: %w", err)
	}
	return &tui{dir: dir, out: out, width: defaultWidth, height: defaultHeight}, nil
}

// loop draws the screen and serves input and the clock until stdin closes
// or the pane hangs up.
func (u *tui) loop(stdin io.Reader) error {
	chunks := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP, syscall.SIGTERM)
	// Bracketed paste on, the terminal's own cursor hidden: the input box
	// draws its cursor.
	if _, err := io.WriteString(u.out, "\x1b[?2004h\x1b[?25l"); err != nil {
		return err
	}
	if err := u.draw(); err != nil {
		return err
	}
	if err := u.record(Event{Event: "ready"}); err != nil {
		return err
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case b := <-chunks:
			if err := u.feed(b); err != nil {
				return err
			}
			if err := u.draw(); err != nil {
				return err
			}
		case <-ticker.C:
			changed, err := u.step(time.Now())
			if err != nil {
				return err
			}
			if changed || (u.busy() && time.Since(u.lastDraw) >= spinnerRedraw) {
				if err := u.draw(); err != nil {
					return err
				}
			}
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading input: %w", err)
		case <-hangup:
			return nil
		}
	}
}

func (u *tui) busy() bool { return !u.turnEnd.IsZero() }

// step starts the turn the control file asks for and ends a turn whose time
// is up. It reports whether the screen changed.
func (u *tui) step(now time.Time) (bool, error) {
	control := filepath.Join(u.dir, "turn")
	data, err := os.ReadFile(control)
	switch {
	case err == nil:
		if err := os.Remove(control); err != nil {
			return false, fmt.Errorf("claiming the turn request: %w", err)
		}
		seconds, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil || seconds <= 0 {
			return false, fmt.Errorf("turn request %q: want a positive number of seconds", data)
		}
		length := time.Duration(seconds * float64(time.Second))
		u.startTurn(now, length)
		return true, u.record(Event{Event: "turn_start", Turn: true, Seconds: length.Seconds()})
	case !os.IsNotExist(err):
		return false, fmt.Errorf("reading the turn request: %w", err)
	}
	if !u.busy() || now.Before(u.turnEnd) {
		return false, nil
	}
	return true, u.endTurn(now)
}

// startTurn runs a turn of length from now, or extends the running one.
func (u *tui) startTurn(now time.Time, length time.Duration) {
	if !u.busy() {
		u.turnStart = now
	}
	if end := now.Add(length); end.After(u.turnEnd) {
		u.turnEnd = end
	}
}

// endTurn ends the running turn; messages queued behind it start the next.
func (u *tui) endTurn(now time.Time) error {
	u.turnEnd = time.Time{}
	u.transcript = append(u.transcript, "⏺ Done.")
	if err := u.record(Event{Event: "turn_end"}); err != nil {
		return err
	}
	if len(u.queued) == 0 {
		return nil
	}
	for _, q := range u.queued {
		u.transcript = append(u.transcript, promptPrefix+q)
	}
	u.queued = nil
	u.startTurn(now, replyTurn)
	return nil
}

// feed takes one read of input.
func (u *tui) feed(b []byte) error {
	if u.busy() && !bytes.Equal(b, []byte{'\r'}) {
		if err := u.record(Event{Event: "input_during_turn", Text: string(b), Turn: true}); err != nil {
			return err
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == esc {
			if i+1 < len(b) && b[i+1] == '[' {
				j := i + 2
				for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
					j++
				}
				switch string(b[i+2 : min(j+1, len(b))]) {
				case "200~":
					u.pasting = true
				case "201~":
					u.pasting = false
				}
				i = j
				continue
			}
			if err := u.escape(); err != nil {
				return err
			}
			continue
		}
		if u.pasting {
			if c == '\r' {
				c = '\n'
			}
			u.input = append(u.input, c)
			continue
		}
		switch c {
		case '\r':
			if err := u.enter(); err != nil {
				return err
			}
		case '\n':
			u.input = append(u.input, c)
		case ctrlU:
			u.input = nil
		case backspace, ctrlH:
			if _, size := utf8.DecodeLastRune(u.input); size > 0 {
				u.input = u.input[:len(u.input)-size]
			}
		default:
			if c >= 0x20 {
				u.input = append(u.input, c)
			}
		}
	}
	return nil
}

// enter submits the input box, or queues it behind a running turn.
func (u *tui) enter() error {
	text := strings.TrimSpace(string(u.input))
	if text == "" {
		return nil
	}
	u.input = nil
	if u.busy() {
		u.queued = append(u.queued, text)
		return u.record(Event{Event: "queued", Text: text, Turn: true})
	}
	for i, line := range strings.Split(text, "\n") {
		if i == 0 {
			line = promptPrefix + line
		} else {
			line = "  " + line
		}
		u.transcript = append(u.transcript, line)
	}
	u.startTurn(time.Now(), replyTurn)
	return u.record(Event{Event: "submit", Text: text})
}

// escape interrupts a running turn, as Escape does in Claude Code.
func (u *tui) escape() error {
	busy := u.busy()
	if err := u.record(Event{Event: "escape", Turn: busy}); err != nil {
		return err
	}
	if busy {
		u.transcript = append(u.transcript, "  ⎿  Interrupted by user")
		return u.endTurn(time.Now())
	}
	return nil
}

// rows is the screen, top to bottom.
func (u *tui) rows() []string {
	rule := strings.Repeat("─", u.width)
	var box []string
	switch {
	case len(u.input) > 0:
		lines := strings.Split(string(u.input), "\n")
		for i, line := range lines {
			if i == 0 {
				line = promptPrefix + line
			} else {
				line = "  " + line
			}
			if i == len(lines)-1 {
				line += cursorCell
			}
			box = append(box, line)
		}
	case len(u.queued) > 0:
		box = []string{promptPrefix + queuedHint}
	default:
		box = []string{promptPrefix + cursorCell}
	}
	var status []string
	footer := "  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	if u.busy() {
		elapsed := int(time.Since(u.turnStart).Seconds())
		status = append(status, "", fmt.Sprintf("✻ Working… (%ds · esc to interrupt)", elapsed))
		for _, q := range u.queued {
			status = append(status, promptPrefix+q)
		}
		footer += " · esc to interrupt"
	}
	tail := append(append(append(status, ""), rule), box...)
	tail = append(tail, rule, footer)
	room := max(u.height-1-len(tail), 0)
	transcript := u.transcript
	if len(transcript) > room {
		transcript = transcript[len(transcript)-room:]
	}
	return append(append([]string(nil), transcript...), tail...)
}

// draw redraws the whole screen in place from the top: no scrolling, so
// nothing a turn drew is left in the pane's history.
func (u *tui) draw() error {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, row := range u.rows() {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("\x1b[2K")
		b.WriteString(row)
	}
	b.WriteString("\x1b[J")
	u.lastDraw = time.Now()
	_, err := io.WriteString(u.out, b.String())
	return err
}

// record appends e to events.jsonl.
func (u *tui) record(e Event) error {
	e.At = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(u.dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening the event log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("recording %s: %w", e.Event, err)
	}
	return f.Close()
}
