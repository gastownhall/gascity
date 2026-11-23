package runtime

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// boxTmux is a scripted ExecProvider standing in for a box's tmux. The fence
// read answers read and each guarded command the next of guarded ("" when
// they run out). The box shell's tmux exits readTmux for the read, the next
// of guardedTmux for a guarded command (0 when they run out) and loadTmux for
// a buffer load, and the shell reports a non-zero one as the carrier's box
// scripts do; the connection exits readCode, the next of guardedCodes and
// loadCode. A publish exits publishCode. It records every argv.
type boxTmux struct {
	read         string
	readCode     int
	readTmux     int
	guarded      []string
	guardedCodes []int
	guardedTmux  []int
	loadCode     int
	loadTmux     int
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
		return boxShellOutput(b.readTmux, b.read), b.readCode, nil
	case len(argv) > 2 && argv[0] == "sh" && argv[2] == carrierLoadBufferScript:
		return boxShellOutput(b.loadTmux, ""), b.loadCode, nil
	case len(argv) > 1 && argv[1] == "set-option":
		return nil, b.publishCode, nil
	case isGuarded(argv):
		code, tmuxExit, out := shiftInt(&b.guardedCodes), shiftInt(&b.guardedTmux), ""
		if len(b.guarded) > 0 {
			out, b.guarded = b.guarded[0], b.guarded[1:]
		}
		return boxShellOutput(tmuxExit, out), code, nil
	}
	return nil, 0, nil
}

// boxShellOutput is what a carrier box script prints when its tmux printed
// out and exited tmuxExit.
func boxShellOutput(tmuxExit int, out string) []byte {
	if tmuxExit != 0 {
		return []byte(carrierTmuxFailed + strconv.Itoa(tmuxExit) + "\n")
	}
	return []byte(out + "\n")
}

// shiftInt removes and returns the first of *s, 0 when it is empty.
func shiftInt(s *[]int) int {
	if len(*s) == 0 {
		return 0
	}
	v := (*s)[0]
	*s = (*s)[1:]
	return v
}

// boxTmuxArgs are argv's tmux arguments when argv runs tmux through the
// carrier's box script (carrierTmuxScript).
func boxTmuxArgs(argv []string) ([]string, bool) {
	if len(argv) < 5 || argv[0] != "sh" || argv[1] != "-c" || argv[2] != carrierTmuxScript || argv[3] != "sh" {
		return nil, false
	}
	return argv[4:], true
}

// isFenceRead reports whether argv is the carrier's fence read.
func isFenceRead(argv []string) bool {
	args, ok := boxTmuxArgs(argv)
	return ok && args[0] == "display-message"
}

// isGuarded reports whether argv is a guarded input command.
func isGuarded(argv []string) bool {
	args, ok := boxTmuxArgs(argv)
	return ok && args[0] == "if-shell"
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
// paste-buffer, bare (direct or through the box script) or inside an
// if-shell, as the guarded tmux command string.
func (b *boxTmux) inputs() []string {
	var out []string
	for _, argv := range b.calls {
		args, boxed := boxTmuxArgs(argv)
		switch {
		case len(argv) > 1 && (argv[1] == "send-keys" || argv[1] == "paste-buffer"):
			out = append(out, "BARE "+strings.Join(argv[1:], " "))
		case boxed && (args[0] == "send-keys" || args[0] == "paste-buffer"):
			out = append(out, "BARE "+strings.Join(args, " "))
		case boxed && len(args) > 5 && args[0] == "if-shell":
			out = append(out, args[5])
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
	if got := box.calls[0]; !slices.Equal(got, []string{"sh", "-c", carrierTmuxScript, "sh", "display-message", "-p", "-t", "main", TmuxFenceReadFormat}) {
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
		args, _ := boxTmuxArgs(argv)
		if !isGuarded(argv) || args[3] != "%3" || !strings.Contains(args[4], carrierFence.Digest()+".#{pane_id}") || !strings.Contains(args[4], "#{==:#{pane_id},%3}") {
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
// carrier), a dead pane, or no session (a live box tmux answering with no
// pane).
func TestTmuxCarrierFenceReadMissesSendNothing(t *testing.T) {
	other := InputFence{Generation: 1, Epoch: 1, Token: "old"}
	for name, tc := range map[string]struct {
		read  string
		found string
	}{
		"other":     {read: ownRead(other, "%3"), found: other.Digest()},
		"inherited": {read: carrierFence.Digest() + ".%9|%3|0", found: FenceFoundUnproven},
		"none":      {read: "|%3|0", found: FenceFoundUnproven},
		"dead":      {read: carrierFence.Digest() + ".%3|%3|1", found: FenceFoundDead},
		"absent":    {read: "||", found: FenceFoundAbsent},
	} {
		for op, call := range carrierOps() {
			box := &boxTmux{read: tc.read}
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

// Kills fenced keys whose body can send a non-key argument: a dialog body
// asking for text gets an error, and nothing past the fence read is sent.
func TestTmuxCarrierFencedKeysRefusesNonKeyNames(t *testing.T) {
	box := &boxTmux{read: ownRead(carrierFence, "%3")}
	err := NewTmuxCarrier(box, "main").FencedKeys(context.Background(), "s", carrierFence, func(sendKeys func(keys ...string) error) error {
		return sendKeys("Enter", "some text")
	})
	if err == nil {
		t.Fatal("FencedKeys sending text = nil, want refused")
	}
	if got := box.inputs(); len(got) != 0 {
		t.Fatalf("inputs = %q, want none", got)
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
// connection's own non-zero exit (ssh 255, kubectl or docker exec 1), tmux in
// the box failing (no server, a wrong socket, no tmux binary), or output that
// is no fence read leaves the incarnation unproven, so it is
// ErrRuntimeUnavailable, never a miss and never gone, and nothing is sent.
// Every unanswered read is a whole *CarrierFenceUnreadError, which the
// provider owning the connection may check against the box's existence.
func TestTmuxCarrierUnreadableFenceIsUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		box    boxTmux
		unread bool
	}{
		"ssh exit":   {box: boxTmux{read: "", readCode: 255}, unread: true},
		"exec exit":  {box: boxTmux{read: ownRead(carrierFence, "%3"), readCode: 1}, unread: true},
		"no server":  {box: boxTmux{readTmux: 1}, unread: true},
		"no tmux":    {box: boxTmux{readTmux: 127}, unread: true},
		"bad output": {box: boxTmux{read: "garbage"}},
	} {
		for op, call := range carrierOps() {
			b := tc.box
			err := call(NewTmuxCarrier(&b, "main"), carrierFence)
			if !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) || IsSessionGone(err) {
				t.Errorf("%s/%s: %v, want ErrRuntimeUnavailable", name, op, err)
			}
			var unread *CarrierFenceUnreadError
			if got := errors.As(err, &unread); got != tc.unread || got != errors.Is(err, ErrCarrierFenceUnread) || (got && (unread.Partial || unread.Session != "s")) {
				t.Errorf("%s/%s: %v, want an unread error %v (whole, naming s)", name, op, err, tc.unread)
			}
			if len(b.calls) != 1 {
				t.Errorf("%s/%s: calls %q, want the read alone", name, op, b.calls)
			}
		}
	}
	// A session whose name holds a phrase IsSessionGone reads as gone is
	// still unavailable.
	b := boxTmux{readTmux: 1}
	if err := NewTmuxCarrier(&b, "main").InterruptFenced(context.Background(), "box not found", carrierFence); IsSessionGone(err) || !errors.Is(err, ErrCarrierFenceUnread) {
		t.Errorf("unanswered read of %q = %v, want unread and not gone", "box not found", err)
	}
}

// Kills a guarded send whose failure reads as delivery or as absence, or
// whose staged buffer outlives it: a guarded command or buffer load the box
// did not answer (the connection's non-zero exit, or tmux in the box failing,
// as when its server died after the read) is a *CarrierFenceUnreadError,
// partial after earlier input, and never a miss; output that is neither empty
// nor a miss is ErrRuntimeUnavailable; a miss read that cannot be parsed is
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
	wantUnread := func(t *testing.T, label string, err error, partial bool) {
		t.Helper()
		var unread *CarrierFenceUnreadError
		if !errors.As(err, &unread) || unread.Partial != partial || !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) || IsSessionGone(err) {
			t.Errorf("%s = %v, want unread (partial %v), never a miss", label, err, partial)
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
	unanswered := map[string]func() *boxTmux{
		"the connection exiting 255": func() *boxTmux { return &boxTmux{read: ownRead(carrierFence, "%3"), guardedCodes: []int{255}} },
		"the box tmux exiting 1":     func() *boxTmux { return &boxTmux{read: ownRead(carrierFence, "%3"), guardedTmux: []int{1}} },
	}

	for label, newBox := range unanswered {
		box := newBox()
		wantUnread(t, "paste with "+label, NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), false)
		if buf := pasteBuffer(box); buf == "" || !slices.Equal(box.deletedBuffers(), []string{buf}) {
			t.Errorf("paste with %s deleted %q, want its buffer %q", label, box.deletedBuffers(), buf)
		}
		for op, call := range carrierOps() {
			wantUnread(t, op+" with "+label, call(NewTmuxCarrier(newBox(), "main"), carrierFence), false)
		}
	}

	box := &boxTmux{read: ownRead(carrierFence, "%3"), guardedCodes: []int{0, 255}}
	wantUnread(t, "Enter with the connection exiting 255", NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), true)
	box = &boxTmux{read: ownRead(carrierFence, "%3"), guardedTmux: []int{0, 1}}
	wantUnread(t, "Enter with the box tmux exiting 1", NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), true)

	for op, call := range carrierOps() {
		box = &boxTmux{read: ownRead(carrierFence, "%3"), guarded: []string{"surprise"}}
		if err := call(NewTmuxCarrier(box, "main"), carrierFence); !errors.Is(err, ErrRuntimeUnavailable) || errors.Is(err, ErrFenceMismatch) {
			t.Errorf("%s with unexpected output = %v, want ErrRuntimeUnavailable", op, err)
		}

		box = &boxTmux{read: ownRead(carrierFence, "%3"), guarded: []string{TmuxFenceMissPrefix + "garbage"}}
		wantMiss(t, op+" with an unparseable miss", call(NewTmuxCarrier(box, "main"), carrierFence), FenceFoundUnproven, false)
	}

	for label, box := range map[string]*boxTmux{
		"the connection exiting 1": {read: ownRead(carrierFence, "%3"), loadCode: 1},
		"the box tmux exiting 1":   {read: ownRead(carrierFence, "%3"), loadTmux: 1},
	} {
		wantUnread(t, "buffer load with "+label, NewTmuxCarrier(box, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence), false)
		if got := box.inputs(); len(got) != 0 {
			t.Errorf("inputs after a buffer load with %s = %q, want none", label, got)
		}
	}

	flaky := &flakyBox{boxTmux: boxTmux{read: ownRead(carrierFence, "%3")}, failIfShell: errBoom}
	err := NewTmuxCarrier(flaky, "main").NudgeFenced(ctx, "s", TextContent("hi"), carrierFence)
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
	if isGuarded(argv) {
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
