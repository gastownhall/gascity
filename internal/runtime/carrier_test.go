package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"
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

func TestTmuxCarrier_NudgeTypesThenSubmits(t *testing.T) {
	f := NewFake()
	c := NewTmuxCarrier(f, "main")
	if err := c.Nudge(context.Background(), "s", TextContent("hi there")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	wantExec(t, f,
		"tmux send-keys -t main -l hi there",
		"tmux send-keys -t main Enter",
	)
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
	c := NewTmuxCarrier(rec, "main")
	if err := c.Nudge(context.Background(), "s", TextContent("hi there friend")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("got %d exec calls, want 2", len(rec.calls))
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
