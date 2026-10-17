package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// boxTmux is a scripted ExecProvider standing in for a box's tmux: it
// answers the fence read with read (or readCode), and each guarded command
// with the next of guarded ("" when they run out). It records every argv.
type boxTmux struct {
	read     string
	readCode int
	guarded  []string
	err      error
	calls    [][]string
}

func (b *boxTmux) Exec(_ context.Context, _ string, argv []string) ([]byte, int, error) {
	b.calls = append(b.calls, argv)
	if b.err != nil {
		return nil, -1, b.err
	}
	switch {
	case len(argv) > 1 && argv[1] == "display-message":
		return []byte(b.read + "\n"), b.readCode, nil
	case len(argv) > 1 && argv[1] == "if-shell":
		if len(b.guarded) == 0 {
			return nil, 0, nil
		}
		out := b.guarded[0]
		b.guarded = b.guarded[1:]
		return []byte(out), 0, nil
	}
	return nil, 0, nil
}

// inputs are the box commands that type into a pane: send-keys and
// paste-buffer, bare or inside an if-shell, as the guarded tmux command
// string.
func (b *boxTmux) inputs() []string {
	var out []string
	for _, argv := range b.calls {
		switch {
		case len(argv) > 1 && (argv[1] == "send-keys" || argv[1] == "paste-buffer"):
			out = append(out, "BARE "+strings.Join(argv[1:], " "))
		case len(argv) > 6 && argv[1] == "if-shell":
			out = append(out, argv[6])
		}
	}
	return out
}

var carrierFence = InputFence{Generation: 2, Epoch: 3, Token: "tok"}

func ownRead(f InputFence, pane string) string {
	return f.Digest() + "." + pane + "|" + pane + "|0"
}

// Kills a fenced carrier op that types anything outside the guard, guards on
// another pane or digest than the one its read proved, or puts text in a
// tmux command string.
func TestTmuxCarrierFencedNudgeGuardsEveryInput(t *testing.T) {
	box := &boxTmux{read: ownRead(carrierFence, "%3")}
	c := NewTmuxCarrier(box, "main")
	if err := c.NudgeFenced(context.Background(), "s", TextContent("hi; it's -x"), carrierFence); err != nil {
		t.Fatalf("NudgeFenced: %v", err)
	}
	if got := box.calls[0]; !slices.Equal(got, []string{"tmux", "display-message", "-p", "-t", "main", TmuxFenceReadFormat}) {
		t.Fatalf("first call = %q, want the fence read", got)
	}
	load := box.calls[1]
	if load[0] != "sh" || load[len(load)-2] != "hi; it's -x" {
		t.Fatalf("text load = %q, want the text as one sh argument", load)
	}
	buffer := load[len(load)-1]
	want := []string{
		"'paste-buffer' '-d' '-r' '-b' '" + buffer + "' '-t' '%3'",
		"'send-keys' '-t' '%3' 'Enter'",
	}
	if got := box.inputs(); !slices.Equal(got, want) {
		t.Fatalf("inputs = %q, want %q", got, want)
	}
	for _, argv := range box.calls[2:] {
		if argv[4] != "%3" || !strings.Contains(argv[5], carrierFence.Digest()+".#{pane_id}") || !strings.Contains(argv[5], "#{==:#{pane_id},%3}") {
			t.Errorf("guard %q does not condition on pane %%3 holding the fence", argv)
		}
		if strings.Contains(strings.Join(argv, " "), carrierFence.Token) {
			t.Errorf("argv %q carries the token", argv)
		}
	}
}

// Kills a carrier that reports a nudge delivered when the box's agent pane
// was respawned as another incarnation between the fence read and the send:
// the guard's false branch is a miss naming what it found, and nothing after
// it is sent.
func TestTmuxCarrierRespawnBetweenCheckAndSendIsRefused(t *testing.T) {
	other := InputFence{Generation: 3, Epoch: 3, Token: "next"}
	box := &boxTmux{
		read:    ownRead(carrierFence, "%3"),
		guarded: []string{TmuxFenceMissPrefix + ownRead(other, "%3")},
	}
	c := NewTmuxCarrier(box, "main")
	err := c.NudgeFenced(context.Background(), "s", TextContent("hi"), carrierFence)
	var miss *FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != other.Digest() || miss.Partial || IsSessionGone(err) {
		t.Fatalf("NudgeFenced = %v, want a whole miss finding %s", err, other.Digest())
	}
	if got := box.inputs(); len(got) != 1 {
		t.Fatalf("inputs = %q, want only the refused paste", got)
	}
}

// Kills a miss after the text that does not report the partial effect, and a
// miss whose read shows another pane that is not absent.
func TestTmuxCarrierMissAfterTextIsPartial(t *testing.T) {
	for name, tc := range map[string]struct {
		missRead string
		found    string
	}{
		"dead":       {carrierFence.Digest() + ".%3|%3|1", FenceFoundDead},
		"other pane": {ownRead(carrierFence, "%4"), FenceFoundAbsent},
		"no session": {"||", FenceFoundAbsent},
	} {
		box := &boxTmux{read: ownRead(carrierFence, "%3"), guarded: []string{"", TmuxFenceMissPrefix + tc.missRead}}
		err := NewTmuxCarrier(box, "main").NudgeFenced(context.Background(), "s", TextContent("hi"), carrierFence)
		var miss *FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != tc.found || !miss.Partial {
			t.Errorf("%s: NudgeFenced = %v, want a partial miss finding %s", name, err, tc.found)
		}
	}
}

// Kills a fenced op that sends input after a read that does not prove the
// fence: another incarnation, a pane with no own fence (no adoption over the
// carrier), a dead pane, no session, or a box tmux that cannot answer.
func TestTmuxCarrierFenceReadMissesSendNothing(t *testing.T) {
	other := InputFence{Generation: 1, Epoch: 1, Token: "old"}
	for name, tc := range map[string]struct {
		read  string
		code  int
		found string
	}{
		"other":     {read: ownRead(other, "%3"), found: other.Digest()},
		"inherited": {read: carrierFence.Digest() + ".%9|%3|0", found: FenceFoundUnproven},
		"none":      {read: "|%3|0", found: FenceFoundUnproven},
		"dead":      {read: carrierFence.Digest() + ".%3|%3|1", found: FenceFoundDead},
		"absent":    {read: "||", found: FenceFoundAbsent},
		"no tmux":   {read: "", code: 1, found: FenceFoundAbsent},
	} {
		for op, call := range carrierOps() {
			box := &boxTmux{read: tc.read, readCode: tc.code}
			err := call(NewTmuxCarrier(box, "main"), carrierFence)
			var miss *FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != tc.found || miss.Partial {
				t.Errorf("%s/%s: %v, want a miss finding %s", name, op, err, tc.found)
			}
			if len(box.calls) != 1 {
				t.Errorf("%s/%s: calls %q, want the read alone", name, op, box.calls)
			}
		}
	}
}

func carrierOps() map[string]func(Carrier, InputFence) error {
	ctx := context.Background()
	return map[string]func(Carrier, InputFence) error{
		"nudge":     func(c Carrier, f InputFence) error { return c.NudgeFenced(ctx, "s", TextContent("hi"), f) },
		"keys":      func(c Carrier, f InputFence) error { return c.SendKeysFenced(ctx, "s", f, "Enter") },
		"interrupt": func(c Carrier, f InputFence) error { return c.InterruptFenced(ctx, "s", f) },
	}
}

// Kills a fenced op that runs without a valid fence, reads a pack without the
// exec connection as anything but unfenceable, or a transport failure as a
// miss or as gone.
func TestTmuxCarrierFencedOpRefusals(t *testing.T) {
	for op, call := range carrierOps() {
		box := &boxTmux{}
		if err := call(NewTmuxCarrier(box, "main"), InputFence{Generation: 1, Epoch: 1}); !errors.Is(err, ErrInvalidInputFence) || len(box.calls) != 0 {
			t.Errorf("%s: invalid fence = %v after %d calls, want ErrInvalidInputFence and none", op, err, len(box.calls))
		}
		box = &boxTmux{err: ErrExecUnsupported}
		if err := call(NewTmuxCarrier(box, "main"), carrierFence); !errors.Is(err, ErrInputFenceUnsupported) {
			t.Errorf("%s: no exec op = %v, want ErrInputFenceUnsupported", op, err)
		}
		box = &boxTmux{err: errBoom}
		err := call(NewTmuxCarrier(box, "main"), carrierFence)
		if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, errBoom) || errors.Is(err, ErrFenceMismatch) || IsSessionGone(err) {
			t.Errorf("%s: transport failure = %v, want ErrRuntimeUnavailable wrapping it", op, err)
		}
	}
}

// Kills fenced keys that put a non-key argument into a tmux command string,
// and an interrupt that is not one guarded C-c.
func TestTmuxCarrierFencedKeysAndInterrupt(t *testing.T) {
	box := &boxTmux{read: ownRead(carrierFence, "%3")}
	c := NewTmuxCarrier(box, "main")
	if err := c.SendKeysFenced(context.Background(), "s", carrierFence, "Down", "Enter"); err != nil {
		t.Fatalf("SendKeysFenced: %v", err)
	}
	if err := c.InterruptFenced(context.Background(), "s", carrierFence); err != nil {
		t.Fatalf("InterruptFenced: %v", err)
	}
	want := []string{"'send-keys' '-t' '%3' 'Down' 'Enter'", "'send-keys' '-t' '%3' 'C-c'"}
	if got := box.inputs(); !slices.Equal(got, want) {
		t.Fatalf("inputs = %q, want %q", got, want)
	}
	box = &boxTmux{read: ownRead(carrierFence, "%3")}
	if err := NewTmuxCarrier(box, "main").SendKeysFenced(context.Background(), "s", carrierFence, "it's text"); err == nil || len(box.calls) != 0 {
		t.Fatalf("SendKeysFenced(text) = %v after %q, want refused before any call", err, box.calls)
	}
}

// Kills a publish that carries the token, or targets another pane option.
func TestTmuxCarrierPublishInputFence(t *testing.T) {
	box := &boxTmux{}
	if err := NewTmuxCarrier(box, "main").PublishInputFence(context.Background(), "s", carrierFence); err != nil {
		t.Fatalf("PublishInputFence: %v", err)
	}
	want := []string{"tmux", "set-option", "-p", "-F", "-t", "main", TmuxInputFenceOption, carrierFence.Digest() + ".#{pane_id}"}
	if len(box.calls) != 1 || !slices.Equal(box.calls[0], want) {
		t.Fatalf("calls = %q, want %q", box.calls, want)
	}
	box = &boxTmux{}
	failing := &boxTmux{err: errBoom}
	if err := NewTmuxCarrier(failing, "main").PublishInputFence(context.Background(), "s", carrierFence); !errors.Is(err, errBoom) {
		t.Fatalf("PublishInputFence over a failing connection = %v, want it", err)
	}
	if err := NewTmuxCarrier(box, "main").PublishInputFence(context.Background(), "s", InputFence{}); !errors.Is(err, ErrInvalidInputFence) || len(box.calls) != 0 {
		t.Fatalf("PublishInputFence(zero) = %v, want ErrInvalidInputFence and no call", err)
	}
}
