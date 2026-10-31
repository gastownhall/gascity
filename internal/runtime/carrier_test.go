package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var errBoom = errors.New("boom: transport failure")

// recordingExec is a minimal ExecProvider that captures raw argv (so a test
// can assert exact arguments, not the space-joined form) and returns a
// configurable result.
type recordingExec struct {
	calls [][]string
	out   []byte
	code  int
	err   error
}

func (r *recordingExec) Exec(_ context.Context, _ string, argv []string) ([]byte, int, error) {
	r.calls = append(r.calls, argv)
	return r.out, r.code, r.err
}

// execMessages returns the argv (space-joined) of every Exec call recorded by
// the fake, in order.
func execMessages(f *Fake) []string {
	var out []string
	for _, c := range f.SnapshotCalls() {
		if c.Method == "Exec" {
			out = append(out, c.Message)
		}
	}
	return out
}

func wantExec(t *testing.T, f *Fake, want ...string) {
	t.Helper()
	got := execMessages(f)
	if len(got) != len(want) {
		t.Fatalf("exec calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("exec[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// testCarrier is a tmux carrier whose sleeps take no real time. Each sleep is
// written into log (when non-nil) as "sleep <d>", so a test can see where the
// pauses fall between the tmux commands.
func testCarrier(conn ExecProvider, log *[]string) *tmuxCarrier {
	c := NewTmuxCarrier(conn, "main").(*tmuxCarrier)
	c.sleep = func(ctx context.Context, d time.Duration) error {
		if log != nil {
			*log = append(*log, fmt.Sprintf("sleep %s", d))
		}
		return ctx.Err()
	}
	return c
}

// fakePane is a tmux pane running a TUI with an input box. It drops the first
// dropEnters Enters it gets, as Gemini CLI 0.64 drops an Enter sent right
// behind the text, and submits on the next one. Every command
// and every carrier sleep goes into log, in order.
type fakePane struct {
	log        []string
	box        string // what the input box holds
	dropEnters int
	submitted  []string
	peekErr    error
	enterErr   error
	// label, when set, is what the box shows for typed text instead of the
	// text itself (the way a TUI shows a large paste).
	label string
}

func (p *fakePane) Exec(_ context.Context, _ string, argv []string) ([]byte, int, error) {
	p.log = append(p.log, strings.Join(argv, " "))
	switch {
	case slices.Equal(argv[:2], []string{"tmux", "capture-pane"}):
		if p.peekErr != nil {
			return nil, 0, p.peekErr
		}
		return []byte("Welcome\n\n> " + p.box + "\n  ~/work  model\n\n\n"), 0, nil
	case len(argv) == 6 && argv[4] == "-l":
		p.box = argv[5]
		if p.label != "" {
			p.box = p.label
		}
	case len(argv) == 5 && argv[4] == "Enter":
		if p.enterErr != nil {
			return nil, 0, p.enterErr
		}
		if p.dropEnters > 0 {
			p.dropEnters--
			break
		}
		if p.box != "" {
			p.submitted = append(p.submitted, p.box)
		}
		p.box = ""
	}
	return nil, 0, nil
}

func (p *fakePane) enters() int {
	n := 0
	for _, l := range p.log {
		if strings.HasSuffix(l, " Enter") {
			n++
		}
	}
	return n
}

func wantLog(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("log =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestTmuxCarrier_NudgeTypesThenSubmits(t *testing.T) {
	f := NewFake()
	c := testCarrier(f, nil)
	if err := c.Nudge(context.Background(), "s", TextContent("hi there")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	// An empty pane cannot show the text, so there is nothing to watch: one
	// look, one Enter.
	wantExec(t, f,
		"tmux send-keys -t main -l hi there",
		"tmux capture-pane -t main -p -S -10",
		"tmux send-keys -t main Enter",
	)
}

// The Enter waits nudgeSubmitDebounce behind the text, like the local tmux
// runtime. Back to back, Gemini CLI 0.64 drops it.
func TestTmuxCarrier_NudgePausesBeforeEnter(t *testing.T) {
	p := &fakePane{}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	wantLog(t, p.log,
		"tmux send-keys -t main -l Hi",
		"sleep 500ms",
		"tmux capture-pane -t main -p -S -10",
		"tmux send-keys -t main Enter",
		"sleep 500ms",
		"tmux capture-pane -t main -p -S -10",
	)
	if !slices.Equal(p.submitted, []string{"Hi"}) {
		t.Errorf("submitted = %q, want [Hi]", p.submitted)
	}
}

// A dropped Enter leaves the text in the box. The carrier sees the box
// unchanged and presses again, so the text is sent once.
func TestTmuxCarrier_NudgeRepressesDroppedEnter(t *testing.T) {
	p := &fakePane{dropEnters: 1}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !slices.Equal(p.submitted, []string{"Hi"}) {
		t.Fatalf("submitted = %q, want [Hi] (the dropped Enter was not pressed again)", p.submitted)
	}
	if got := p.enters(); got != 2 {
		t.Errorf("Enters = %d, want 2", got)
	}
}

// The presses are bounded: a box that never takes the text gets
// nudgeSubmitMaxPresses Enters, then Nudge returns. The Enters went out, so
// it reports success; an error would make a caller type the text twice.
func TestTmuxCarrier_NudgeStopsAfterMaxPresses(t *testing.T) {
	p := &fakePane{dropEnters: 100}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := p.enters(); got != nudgeSubmitMaxPresses {
		t.Errorf("Enters = %d, want %d", got, nudgeSubmitMaxPresses)
	}
	p2 := &fakePane{dropEnters: nudgeSubmitMaxPresses - 1}
	c2 := testCarrier(p2, &p2.log)
	if err := c2.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !slices.Equal(p2.submitted, []string{"Hi"}) {
		t.Errorf("submitted = %q, want [Hi] on the last allowed press", p2.submitted)
	}
}

// An Enter that lands changes the box, and is never pressed again: a second
// Enter could land in the turn it just started.
func TestTmuxCarrier_NudgeDoesNotRepressALandedEnter(t *testing.T) {
	p := &fakePane{}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := p.enters(); got != 1 {
		t.Errorf("Enters = %d, want 1", got)
	}
}

// A box that does not show the text (a paste shown as a label) cannot tell a
// dropped Enter from a landed one, so it gets one Enter and no second look.
func TestTmuxCarrier_NudgeUnreadableBoxGetsOneEnter(t *testing.T) {
	p := &fakePane{dropEnters: 1, label: "[Pasted text #1 +3 lines]"}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("line one\nline two")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	wantLog(t, p.log,
		"tmux send-keys -t main -l line one\nline two",
		"sleep 500ms",
		"tmux capture-pane -t main -p -S -10",
		"tmux send-keys -t main Enter",
	)
}

// A failed look is the same: one Enter, and success.
func TestTmuxCarrier_NudgePeekErrorGetsOneEnter(t *testing.T) {
	p := &fakePane{dropEnters: 1, peekErr: errBoom}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := p.enters(); got != 1 {
		t.Errorf("Enters = %d, want 1", got)
	}
}

// A failed Enter is a failed delivery, and the caller hears it.
func TestTmuxCarrier_NudgeEnterErrorSurfaces(t *testing.T) {
	p := &fakePane{enterErr: errBoom}
	c := testCarrier(p, &p.log)
	if err := c.Nudge(context.Background(), "s", TextContent("Hi")); !errors.Is(err, errBoom) {
		t.Fatalf("Nudge err = %v, want errBoom", err)
	}
}

// A message longer than the box is wide wraps; its tail is still found.
func TestTmuxCarrier_NudgeFindsAWrappedMessage(t *testing.T) {
	msg := strings.Repeat("please read the bead and start the work ", 5)
	box := msg[:60] + "\n│ " + msg[60:120] + "\n│ " + msg[120:]
	if !boxShowsText(paneBox("history\n> "+box+"\nfooter\n\n"), msg) {
		t.Errorf("boxShowsText missed the wrapped message's tail")
	}
	if boxShowsText(paneBox("history\n> \nfooter\n"), msg) {
		t.Errorf("boxShowsText found text in an empty box")
	}
}

func TestTmuxCarrier_NudgeEmptyIsNoOp(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if err := c.Nudge(context.Background(), "s", TextContent("")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if got := execMessages(f); len(got) != 0 {
		t.Errorf("empty Nudge issued %v, want no commands", got)
	}
}

func TestTmuxCarrier_SendKeys(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if err := c.SendKeys(context.Background(), "s", "Down", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	wantExec(t, f, "tmux send-keys -t main Down Enter")
}

func TestTmuxCarrier_Interrupt(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if err := c.Interrupt(context.Background(), "s"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	wantExec(t, f, "tmux send-keys -t main C-c")
}

func TestTmuxCarrier_ClearScrollback(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if err := c.ClearScrollback(context.Background(), "s"); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}
	wantExec(t, f, "tmux clear-history -t main")
}

func TestTmuxCarrier_PeekCapturesPaneAndReturnsOutput(t *testing.T) {
	f := NewFake()
	f.ExecResults = map[string]FakeExecResult{"s": {Output: "captured pane text"}}
	c := NewTmuxCarrier(f, "main")
	got, err := c.Peek(context.Background(), "s", 40)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if got != "captured pane text" {
		t.Errorf("Peek output = %q, want %q", got, "captured pane text")
	}
	wantExec(t, f, "tmux capture-pane -t main -p -S -40")
}

func TestTmuxCarrier_PeekAllScrollback(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if _, err := c.Peek(context.Background(), "s", 0); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	wantExec(t, f, "tmux capture-pane -t main -p -S -")
}

func TestTmuxCarrier_NudgeMessageIsASingleArg(t *testing.T) {
	// Guards against a regression that splits the message into multiple argv
	// elements — which the space-joined Fake assertions above cannot catch.
	rec := &recordingExec{}
	c := testCarrier(rec, nil)
	if err := c.Nudge(context.Background(), "s", TextContent("hi there friend")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if len(rec.calls) != 3 {
		t.Fatalf("got %d exec calls, want 3 (type, look, Enter)", len(rec.calls))
	}
	want := []string{"tmux", "send-keys", "-t", "main", "-l", "hi there friend"}
	if !slices.Equal(rec.calls[0], want) {
		t.Errorf("first argv = %v, want %v (message must be one element)", rec.calls[0], want)
	}
}

func TestTmuxCarrier_NudgeFirstStepErrorSkipsEnter(t *testing.T) {
	// A type failure surfaces the error and skips the Enter submit.
	rec := &recordingExec{err: errBoom}
	c := NewTmuxCarrier(rec, "main")
	err := c.Nudge(context.Background(), "s", TextContent("hi"))
	if !errors.Is(err, errBoom) {
		t.Fatalf("Nudge err = %v, want errBoom", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("issued %d exec calls, want 1 (Enter must be skipped after the type fails)", len(rec.calls))
	}
}

func TestTmuxCarrier_PeekPropagatesTransportError(t *testing.T) {
	// The carrier is honest: it returns the transport error, unlike k8s which
	// swallows a failed capture to ("", nil). The provider owns that policy.
	rec := &recordingExec{err: errBoom}
	c := NewTmuxCarrier(rec, "main")
	out, err := c.Peek(context.Background(), "s", 10)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Peek err = %v, want errBoom", err)
	}
	if out != "" {
		t.Errorf("Peek output = %q, want empty on error", out)
	}
}

// Nudge text reaches the in-box pane byte for byte (E5): the carrier escapes a
// final ";" and puts "--" before a leading "-", as every send-keys -l does.
func TestTmuxCarrier_NudgeTextIsTmuxLiteral(t *testing.T) {
	for text, want := range map[string][]string{
		"x;":     {"tmux", "send-keys", "-t", "main", "-l", `x\;`},
		"-x":     {"tmux", "send-keys", "-t", "main", "-l", "--", "-x"},
		"- item": {"tmux", "send-keys", "-t", "main", "-l", "--", "- item"},
		"a;b":    {"tmux", "send-keys", "-t", "main", "-l", "a;b"},
	} {
		r := &recordingExec{}
		if err := testCarrier(r, nil).Nudge(context.Background(), "s", TextContent(text)); err != nil {
			t.Fatalf("Nudge(%q): %v", text, err)
		}
		enter := []string{"tmux", "send-keys", "-t", "main", "Enter"}
		if len(r.calls) < 2 || !slices.Equal(r.calls[0], want) || !slices.Equal(r.calls[len(r.calls)-1], enter) {
			t.Errorf("Nudge(%q) argv = %q, want %q first and Enter last", text, r.calls, want)
		}
	}
}
