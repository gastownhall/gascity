package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// boxTmux is a scripted ExecProvider standing in for a box's tmux. It
// answers the fence read with read, or the read script's absent sentinel when
// noTmux is set (the box tmux could not answer), exiting readCode (the
// connection's own exit). Each guarded command answers the next of guarded
// ("" when they run out), exiting the next of guardedCodes (0 when they run
// out). A buffer load exits loadCode and a publish publishCode. It records
// every argv.
type boxTmux struct {
	read         string
	readCode     int
	noTmux       bool
	guarded      []string
	guardedCodes []int
	loadCode     int
	publishCode  int
	err          error
	calls        [][]string
}

func (b *boxTmux) Exec(_ context.Context, _ string, argv []string) ([]byte, int, error) {
	b.calls = append(b.calls, argv)
	if b.err != nil {
		return nil, -1, b.err
	}
	switch {
	case isFenceRead(argv):
		if b.noTmux {
			return []byte(carrierFenceAbsent + "\n"), b.readCode, nil
		}
		return []byte(b.read + "\n"), b.readCode, nil
	case len(argv) > 2 && argv[0] == "sh" && argv[2] == carrierLoadBufferScript:
		return nil, b.loadCode, nil
	case len(argv) > 1 && argv[1] == "set-option":
		return nil, b.publishCode, nil
	case len(argv) > 1 && argv[1] == "if-shell":
		code := 0
		if len(b.guardedCodes) > 0 {
			code = b.guardedCodes[0]
			b.guardedCodes = b.guardedCodes[1:]
		}
		if len(b.guarded) == 0 {
			return nil, code, nil
		}
		out := b.guarded[0]
		b.guarded = b.guarded[1:]
		return []byte(out), code, nil
	}
	return nil, 0, nil
}

// isFenceRead reports whether argv is the carrier's fence read.
func isFenceRead(argv []string) bool {
	return len(argv) > 2 && argv[0] == "sh" && argv[2] == carrierFenceReadScript
}

// deletedBuffers are the buffers the box was told to delete.
func (b *boxTmux) deletedBuffers() []string {
	var out []string
	for _, argv := range b.calls {
		if len(argv) == 4 && argv[1] == "delete-buffer" {
			out = append(out, argv[3])
		}
	}
	return out
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
	if got := box.calls[0]; !slices.Equal(got, []string{"sh", "-c", carrierFenceReadScript, "sh", "main", TmuxFenceReadFormat}) {
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
// carrier), a dead pane, no session, or a box tmux that cannot answer (no
// server, no session: the read script's absent sentinel).
func TestTmuxCarrierFenceReadMissesSendNothing(t *testing.T) {
	other := InputFence{Generation: 1, Epoch: 1, Token: "old"}
	for name, tc := range map[string]struct {
		read   string
		noTmux bool
		found  string
	}{
		"other":     {read: ownRead(other, "%3"), found: other.Digest()},
		"inherited": {read: carrierFence.Digest() + ".%9|%3|0", found: FenceFoundUnproven},
		"none":      {read: "|%3|0", found: FenceFoundUnproven},
		"dead":      {read: carrierFence.Digest() + ".%3|%3|1", found: FenceFoundDead},
		"absent":    {read: "||", found: FenceFoundAbsent},
		"no tmux":   {noTmux: true, found: FenceFoundAbsent},
	} {
		for op, call := range carrierOps() {
			box := &boxTmux{read: tc.read, noTmux: tc.noTmux}
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

// Kills a fence read that takes a box it cannot read for an absent one: the
// connection's own non-zero exit (ssh 255, kubectl or docker exec 1) or
// output that is no fence read leaves the incarnation unproven, so it is
// ErrRuntimeUnavailable, never a miss, and nothing is sent.
func TestTmuxCarrierUnreadableFenceIsUnavailable(t *testing.T) {
	for name, box := range map[string]*boxTmux{
		"ssh exit":   {read: "", readCode: 255},
		"exec exit":  {read: ownRead(carrierFence, "%3"), readCode: 1},
		"bad output": {read: "garbage"},
	} {
		for op, call := range carrierOps() {
			b := *box
			err := call(NewTmuxCarrier(&b, "main"), carrierFence)
			if !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) || IsSessionGone(err) {
				t.Errorf("%s/%s: %v, want ErrRuntimeUnavailable", name, op, err)
			}
			if len(b.calls) != 1 {
				t.Errorf("%s/%s: calls %q, want the read alone", name, op, b.calls)
			}
		}
	}
}

// Kills a guarded send whose failure reads as delivery, or whose staged
// buffer outlives it: a non-zero exit of the guarded command (tmux cannot
// resolve the pane, or the inner command failed) is a miss finding the pane
// absent, partial after earlier input; output that is neither empty nor a
// miss is ErrRuntimeUnavailable; a miss read that cannot be parsed is
// unproven; and every failed paste deletes its buffer.
func TestTmuxCarrierGuardedSendFailures(t *testing.T) {
	ctx := context.Background()
	wantMiss := func(t *testing.T, label string, err error, found string, partial bool) {
		t.Helper()
		var miss *FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != found || miss.Partial != partial {
			t.Errorf("%s = %v, want a miss finding %s (partial %v)", label, err, found, partial)
		}
	}
	pasteBuffer := func(b *boxTmux) string {
		for _, argv := range b.calls {
			if len(argv) > 2 && argv[0] == "sh" && argv[2] == carrierLoadBufferScript {
				return argv[len(argv)-1]
			}
		}
		return ""
	}

	box := &boxTmux{read: ownRead(carrierFence, "%3"), guardedCodes: []int{1}}
	wantMiss(t, "paste exiting 1", NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), FenceFoundAbsent, false)
	if buf := pasteBuffer(box); buf == "" || !slices.Equal(box.deletedBuffers(), []string{buf}) {
		t.Errorf("paste exiting 1 deleted %q, want its buffer %q", box.deletedBuffers(), buf)
	}

	box = &boxTmux{read: ownRead(carrierFence, "%3"), guardedCodes: []int{0, 1}}
	wantMiss(t, "Enter exiting 1", NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), FenceFoundAbsent, true)

	for op, call := range carrierOps() {
		box = &boxTmux{read: ownRead(carrierFence, "%3"), guardedCodes: []int{1}}
		wantMiss(t, op+" exiting 1", call(NewTmuxCarrier(box, "main"), carrierFence), FenceFoundAbsent, false)

		box = &boxTmux{read: ownRead(carrierFence, "%3"), guarded: []string{"surprise"}}
		if err := call(NewTmuxCarrier(box, "main"), carrierFence); !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) {
			t.Errorf("%s with unexpected output = %v, want ErrRuntimeUnavailable", op, err)
		}

		box = &boxTmux{read: ownRead(carrierFence, "%3"), guarded: []string{TmuxFenceMissPrefix + "garbage"}}
		wantMiss(t, op+" with an unparseable miss", call(NewTmuxCarrier(box, "main"), carrierFence), FenceFoundUnproven, false)
	}

	box = &boxTmux{read: ownRead(carrierFence, "%3"), loadCode: 1}
	err := NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence)
	if !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) {
		t.Errorf("NudgeFenced with the buffer load failing = %v, want ErrRuntimeUnavailable", err)
	}
	if got := box.inputs(); len(got) != 0 {
		t.Errorf("inputs after a failed buffer load = %q, want none", got)
	}

	flaky := &flakyBox{boxTmux: boxTmux{read: ownRead(carrierFence, "%3")}, failIfShell: errBoom}
	err = NewTmuxCarrier(flaky, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence)
	if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, errBoom) {
		t.Errorf("NudgeFenced with the guarded paste's connection failing = %v, want ErrRuntimeUnavailable wrapping it", err)
	}
	if buf := pasteBuffer(&flaky.boxTmux); buf == "" || !slices.Equal(flaky.deletedBuffers(), []string{buf}) {
		t.Errorf("a failed guarded paste deleted %q, want its buffer %q", flaky.deletedBuffers(), buf)
	}
}

// flakyBox is boxTmux whose guarded commands fail on the connection.
type flakyBox struct {
	boxTmux
	failIfShell error
}

func (b *flakyBox) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	if len(argv) > 1 && argv[1] == "if-shell" {
		b.calls = append(b.calls, argv)
		return nil, -1, b.failIfShell
	}
	return b.boxTmux.Exec(ctx, name, argv)
}

// Kills a publish that reads a box tmux's refusal as success.
func TestTmuxCarrierPublishInputFenceExitCode(t *testing.T) {
	box := &boxTmux{publishCode: 1}
	if err := NewTmuxCarrier(box, "main").PublishInputFence(context.Background(), "s", carrierFence); err == nil {
		t.Fatal("PublishInputFence with tmux exiting 1 = nil, want an error")
	}
}
