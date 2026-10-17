package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var execFence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}

// boxScript is a pack declaring the exec connection and input fencing whose
// exec op logs each in-box command to calls and answers the fence read with
// read and a guarded command with guarded. separable adds proc.provision.
func boxScript(t *testing.T, read, guarded string, separable bool) (script, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	caps := `"proc.exec","input-fence"`
	if separable {
		caps += `,"proc.provision"`
	}
	script = writeScript(t, dir, `
op="$1"
case "$op" in
  protocol) echo '{"version":0,"capabilities":[`+caps+`]}' ;;
  exec)
    cmd=$(cat)
    echo "$cmd" >> `+calls+`
    case "$cmd" in
      *display-message*gc-fence-miss*) echo '`+guarded+`' ;;
      *if-shell*) echo '`+guarded+`' ;;
      *display-message*) echo '`+read+`' ;;
    esac
    ;;
  start|provision|stop) cat > /dev/null ;;
  *) exit 2 ;;
esac
`)
	return script, calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// ownRead is the fence read of agent pane %1 live and holding f as its own.
func ownRead(f runtime.InputFence) string {
	return f.Digest() + ".%1|%1|0"
}

// Kills an exec provider whose fencing does not follow its pack: a pack
// declaring the exec connection and input fencing (input-fence) is atomic
// (the tmux carrier guard); one declaring the exec connection alone, whose
// box tmux need not hold a fence, or no handshake at all cannot fence and
// refuses before running anything.
func TestExecInputFencingFollowsTheInputFenceCapability(t *testing.T) {
	script, _ := boxScript(t, "", "", false)
	for name, p := range map[string]runtime.Provider{"raw": NewProvider(script), "seam-backed": NewSeamBacked(script)} {
		if got := runtime.InputFencingFor(p, "s"); got != runtime.InputFencingAtomic {
			t.Errorf("%s: fencing = %q, want atomic", name, got)
		}
	}
	for label, protocol := range map[string]string{
		"exec connection alone": `echo '{"version":0,"capabilities":["proc.exec"]}'`,
		"input fence alone":     `echo '{"version":0,"capabilities":["input-fence"]}'`,
		"no handshake":          "exit 2",
	} {
		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		script := writeScript(t, dir, `
case "$1" in
  protocol) `+protocol+` ;;
  *) echo "$1" >> `+calls+` ;;
esac
`)
		p := NewSeamBacked(script)
		if got := runtime.InputFencingFor(p, "s"); got != runtime.InputFencingNone {
			t.Errorf("%s: fencing = %q, want none", label, got)
		}
		fp := p.(runtime.FencedInputProvider)
		for op, err := range map[string]error{
			"nudge":     fp.NudgeFenced(context.Background(), "s", runtime.TextContent("hi"), runtime.FencedNudge{Fence: execFence}),
			"keys":      fp.SendKeysFenced(context.Background(), "s", execFence, "Enter"),
			"interrupt": fp.InterruptFenced(context.Background(), "s", execFence),
		} {
			if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
				t.Errorf("%s: %s = %v, want ErrInputFenceUnsupported", label, op, err)
			}
		}
		if got := readCalls(t, calls); len(got) != 0 {
			t.Errorf("%s: the pack ran %q under a fence", label, got)
		}
	}
}

// Kills an exec provider that sends fenced input outside the carrier guard,
// or reports a nudge delivered when the agent was respawned as another
// incarnation between the fence read and the send.
func TestExecFencedNudgeRespawnBetweenCheckAndSendIsRefused(t *testing.T) {
	other := runtime.InputFence{Generation: 3, Epoch: 3, Token: "next"}
	script, calls := boxScript(t, ownRead(execFence), "gc-fence-miss:"+ownRead(other), false)
	fp := NewSeamBacked(script).(runtime.FencedInputProvider)
	err := fp.NudgeFenced(context.Background(), "s", runtime.TextContent("hello"), runtime.FencedNudge{Fence: execFence})
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != other.Digest() || miss.Partial {
		t.Fatalf("NudgeFenced = %v, want a whole miss finding %s", err, other.Digest())
	}
	for _, c := range readCalls(t, calls) {
		if strings.HasPrefix(c, "'tmux' 'send-keys'") || strings.HasPrefix(c, "'tmux' 'paste-buffer'") {
			t.Errorf("unguarded input %q", c)
		}
		if strings.Contains(c, execFence.Token+"'") {
			t.Errorf("command %q carries the token", c)
		}
	}
}

// Kills a ctx-fenced DismissKnownDialogs that sends a dialog key unguarded on
// a pack that can fence.
func TestExecDismissKnownDialogsFencesItsKeys(t *testing.T) {
	script, calls := boxScript(t, ownRead(execFence), "", false)
	p := NewSeamBacked(script).(runtime.DialogProvider)
	ctx := runtime.WithInputFence(context.Background(), execFence)
	// No dialog in a pane that peeks empty: the call reads and returns.
	if err := p.DismissKnownDialogs(ctx, "s", 300*time.Millisecond); err != nil {
		t.Fatalf("fenced DismissKnownDialogs = %v", err)
	}
	other := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 9, Epoch: 9, Token: "other"})
	err := p.DismissKnownDialogs(other, "s", 300*time.Millisecond)
	if !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("DismissKnownDialogs under another fence = %v, want ErrFenceMismatch", err)
	}
	for _, c := range readCalls(t, calls) {
		if strings.HasPrefix(c, "'tmux' 'send-keys'") {
			t.Errorf("unguarded dialog key %q", c)
		}
	}
}

// Kills a ctx-fenced DismissKnownDialogs that guards each dialog key on its
// own: the call is one fenced operation, so the fence is read once, every
// key goes through that read's guard, and a respawn after the first key
// reached the pane is a miss marked partial.
func TestExecDismissKnownDialogsIsOneFencedOperation(t *testing.T) {
	other := runtime.InputFence{Generation: 3, Epoch: 3, Token: "next"}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	guards := filepath.Join(dir, "guards")
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  exec)
    cmd=$(cat)
    echo "$cmd" >> `+calls+`
    case "$cmd" in
      *if-shell*)
        echo x >> `+guards+`
        if [ "$(wc -l < `+guards+`)" -gt 1 ]; then echo 'gc-fence-miss:`+ownRead(other)+`'; fi ;;
      *display-message*) echo '`+ownRead(execFence)+`' ;;
      *capture-pane*) printf '%s\n' 'Resume from summary' 'Resume full session as-is' 'Enter to confirm' ;;
    esac
    ;;
  *) exit 2 ;;
esac
`)
	p := NewSeamBacked(script).(runtime.DialogProvider)
	err := p.DismissKnownDialogs(runtime.WithInputFence(context.Background(), execFence), "s", time.Second)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != other.Digest() || !miss.Partial {
		t.Fatalf("DismissKnownDialogs with a respawn after the first key = %v, want a partial miss finding %s", err, other.Digest())
	}
	var reads, guarded int
	for _, c := range readCalls(t, calls) {
		switch {
		case strings.Contains(c, "'sh' 'display-message'"):
			reads++
		case strings.Contains(c, "'sh' 'if-shell'"):
			guarded++
		case strings.HasPrefix(c, "'tmux' 'send-keys'"), strings.Contains(c, "'sh' 'send-keys'"):
			t.Errorf("unguarded dialog key %q", c)
		}
	}
	if reads != 1 || guarded != 2 {
		t.Fatalf("fence reads = %d, guarded keys = %d; want one read guarding both dialog keys", reads, guarded)
	}
}

// Kills a start or relaunch that leaves the new agent pane without its
// incarnation's fence, or a respawn that keeps its predecessor's: a welded
// start clears a reused pane's fence before the start op and publishes the
// new one after it.
func TestExecStartAndLaunchPublishTheFence(t *testing.T) {
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	publish := "'set-option' '-p' '-F' '-t' 'main' '@gc_fence' '" + execFence.Digest() + ".#{pane_id}'"
	clearFence := "'tmux' 'display-message' '-p' '-t' 'main' '#{@gc_fence}|#{pane_id}|#{pane_pid}' ';' 'set-option' '-p' '-u' '-t' 'main' '@gc_fence'"

	script, calls := boxScript(t, "", "", false)
	if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err != nil {
		t.Fatalf("welded Start: %v", err)
	}
	if got := readCalls(t, calls); len(got) != 2 || got[0] != clearFence || got[1] != "'tmux' "+publish {
		t.Fatalf("welded Start ran %q, want the old fence cleared, then the new one published", got)
	}

	script, calls = boxScript(t, "", "", true)
	p := NewProvider(script)
	if err := p.launchAgent(context.Background(), "s", runtime.Config{Command: "agent", Env: env}); err != nil {
		t.Fatalf("launchAgent: %v", err)
	}
	got := readCalls(t, calls)
	if len(got) != 2 || !strings.HasSuffix(got[1], "'agent' ';' "+publish) {
		t.Fatalf("launch ran %q, want the launch and the publish in one tmux command list", got)
	}
	_ = os.Remove(calls)
	if err := p.launchAgent(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("launchAgent without identity: %v", err)
	}
	got = readCalls(t, calls)
	if len(got) != 2 || !strings.HasSuffix(got[1], "'agent' ';' 'set-option' '-p' '-u' '-t' 'main' '@gc_fence'") {
		t.Fatalf("identity-less launch ran %q, want the fence cleared in the same command list", got)
	}
}

// Kills an exec fenced op that reads a fence read its pack's exec op did not
// answer as absence, or that never lets the pack say the box is gone: the
// op's own non-zero exit, and tmux in the box failing (reported by the
// carrier's box shell), are ErrRuntimeUnavailable unless is-running answers
// a definite "false", which is a miss finding the box absent.
func TestExecUnansweredFenceReadAsksIsRunning(t *testing.T) {
	for shape, exec := range map[string]string{
		"exec op exiting 255": "cat > /dev/null; exit 255",
		"box tmux failing":    "cat > /dev/null; echo 'gc-tmux-failed:1'",
	} {
		for answer, wantAbsent := range map[string]bool{"false": true, "true": false, "": false} {
			dir := t.TempDir()
			script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  exec) `+exec+` ;;
  is-running) echo '`+answer+`' ;;
  *) exit 2 ;;
esac
`)
			p := NewProvider(script)
			ctx := context.Background()
			for op, err := range map[string]error{
				"nudge":     p.NudgeFenced(ctx, "s", runtime.TextContent("hi"), runtime.FencedNudge{Fence: execFence}),
				"keys":      p.SendKeysFenced(ctx, "s", execFence, "Enter"),
				"interrupt": p.InterruptFenced(ctx, "s", execFence),
				"dialogs":   p.DismissKnownDialogs(runtime.WithInputFence(ctx, execFence), "s", time.Second),
			} {
				var miss *runtime.FenceMismatchError
				switch {
				case wantAbsent && (!errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial):
					t.Errorf("%s, %s, is-running %q: %v, want a whole miss finding the box absent", shape, op, answer, err)
				case !wantAbsent && (!errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrFenceMismatch) || runtime.IsSessionGone(err)):
					t.Errorf("%s, %s, is-running %q: %v, want ErrRuntimeUnavailable", shape, op, answer, err)
				}
			}
			fp := NewSeamBacked(script).(runtime.FencedInputProvider)
			if err := fp.SendKeysFenced(ctx, "s", execFence, "Enter"); wantAbsent != errors.Is(err, runtime.ErrFenceMismatch) {
				t.Errorf("%s, seam-backed keys, is-running %q: %v", shape, answer, err)
			}
		}
	}
}

// Kills an exec nudge that loses its partial effect when the box stops
// answering between the text and its Enter: a definite is-running "false"
// is a partial miss finding the box absent, and any other answer leaves the
// partial unread error in place.
func TestExecUnansweredEnterAfterTheTextIsPartial(t *testing.T) {
	for answer, wantAbsent := range map[string]bool{"false": true, "true": false} {
		dir := t.TempDir()
		script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  exec)
    cmd=$(cat)
    case "$cmd" in
      *if-shell*Enter*) exit 255 ;;
      *if-shell*) ;;
      *display-message*) echo '`+ownRead(execFence)+`' ;;
    esac
    ;;
  is-running) echo '`+answer+`' ;;
  *) exit 2 ;;
esac
`)
		err := NewProvider(script).NudgeFenced(context.Background(), "s", runtime.TextContent("hi"), runtime.FencedNudge{Fence: execFence})
		var miss *runtime.FenceMismatchError
		var unread *runtime.CarrierFenceUnreadError
		switch {
		case wantAbsent && (!errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || !miss.Partial):
			t.Errorf("is-running %q: %v, want a partial miss finding the box absent", answer, err)
		case !wantAbsent && (!errors.As(err, &unread) || !unread.Partial || errors.Is(err, runtime.ErrFenceMismatch)):
			t.Errorf("is-running %q: %v, want a partial unread error", answer, err)
		}
	}
}

// Kills a welded start that reports success with its agent pane left
// unproven: when the fence cannot be published, Start fails (and tears the
// box down) rather than leave a pane no fenced input can reach.
func TestExecStartFailsWhenTheFenceCannotBePublished(t *testing.T) {
	dir := t.TempDir()
	stops := filepath.Join(dir, "stops")
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  is-running) echo false ;;
  exec)
    cmd=$(cat)
    case "$cmd" in
      *set-option*-F*) exit 1 ;;
    esac
    ;;
  start) cat > /dev/null ;;
  stop) echo stop >> `+stops+` ;;
  *) exit 2 ;;
esac
`)
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err == nil {
		t.Fatal("Start with the fence publish failing = nil, want an error")
	}
	if got := readCalls(t, stops); len(got) != 1 {
		t.Fatalf("stops = %q, want the box torn down once", got)
	}
}

// Kills a start into a box that was already up which, when the new fence
// cannot be published after the start op returned, leaves the live session
// without the fence it held: the box is kept, so the fence cleared before the
// start op goes back on the same pane and root process.
func TestExecStartRestoresTheLiveFenceWhenThePublishFails(t *testing.T) {
	old := runtime.InputFence{Generation: 1, Epoch: 1, Token: "old"}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	stops := filepath.Join(dir, "stops")
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  is-running) echo true ;;
  exec)
    cmd=$(cat)
    echo "$cmd" >> `+calls+`
    case "$cmd" in
      "'tmux' 'set-option'"*) exit 1 ;;
      *display-message*) echo '`+old.Digest()+`.%1|%1|4242' ;;
    esac
    ;;
  start) cat > /dev/null ;;
  stop) echo stop >> `+stops+` ;;
  *) exit 2 ;;
esac
`)
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err == nil {
		t.Fatal("Start with the fence publish failing = nil, want an error")
	}
	restore := shellQuote([]string{
		"tmux", "if-shell", "-F", "-t", "main",
		"#{&&:#{==:#{pane_id},%1},#{&&:#{==:#{pane_pid},4242},#{==:#{@gc_fence},}}}",
		"'set-option' '-p' '-F' '-t' 'main' '@gc_fence' '" + old.Digest() + ".#{pane_id}'",
	})
	got := readCalls(t, calls)
	if len(got) != 3 || !strings.Contains(got[0], "'set-option' '-p' '-u'") || !strings.HasPrefix(got[1], "'tmux' 'set-option' '-p' '-F'") || got[2] != restore {
		t.Fatalf("Start ran %q, want the fence cleared, the failed publish, then the old fence restored on pane %%1 under pid 4242", got)
	}
	if s := readCalls(t, stops); len(s) != 0 {
		t.Fatalf("stops = %q, want the live box kept", s)
	}
}

// collisionScript is a pack declaring the exec connection and input fencing
// whose box is already up: is-running answers running, the fence read answers read, and
// the start op fails with startErr (and exit 1). Every in-box command is
// logged to calls and every stop to stops.
func collisionScript(t *testing.T, read, startErr string) (script, calls, stops string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	stops = filepath.Join(dir, "stops")
	script = writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","input-fence"]}' ;;
  is-running) echo true ;;
  exec)
    cmd=$(cat)
    echo "$cmd" >> `+calls+`
    case "$cmd" in
      *display-message*) echo '`+read+`' ;;
    esac
    ;;
  start) cat > /dev/null; echo '`+startErr+`' >&2; exit 1 ;;
  stop) echo stop >> `+stops+` ;;
  *) exit 2 ;;
esac
`)
	return script, calls, stops
}

// Kills a colliding start that strips the live session's fence: the start
// op clears the pane's fence first, so when it then refuses because the box
// belongs to a live session (a recognized collision, or a box that was up
// before the attempt), the fence it cleared goes back on the same pane and
// root process, unless the pane has taken another fence since.
func TestExecCollidingStartRestoresTheLiveFence(t *testing.T) {
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	restore := shellQuote([]string{
		"tmux", "if-shell", "-F", "-t", "main",
		"#{&&:#{==:#{pane_id},%1},#{&&:#{==:#{pane_pid},4242},#{==:#{@gc_fence},}}}",
		"'set-option' '-p' '-F' '-t' 'main' '@gc_fence' '" + execFence.Digest() + ".#{pane_id}'",
	})
	live := execFence.Digest() + ".%1|%1|4242"

	for label, startErr := range map[string]string{
		"recognized collision": "session already exists",
		"unrecognized refusal": "box busy",
	} {
		t.Run(label, func(t *testing.T) {
			script, calls, stops := collisionScript(t, live, startErr)
			if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err == nil {
				t.Fatal("colliding Start = nil, want the start op's error")
			}
			got := readCalls(t, calls)
			if len(got) != 2 || !strings.Contains(got[0], "'set-option' '-p' '-u'") || got[1] != restore {
				t.Fatalf("colliding Start ran %q, want the fence cleared and then restored on pane %%1 under pid 4242", got)
			}
			if s := readCalls(t, stops); len(s) != 0 {
				t.Fatalf("stops = %q, want the live box kept", s)
			}
		})
	}

	for label, read := range map[string]string{
		"no own fence":      "|%1|4242",
		"inherited fence":   execFence.Digest() + ".%7|%1|4242",
		"no session":        "||",
		"unexpected output": "garbage",
	} {
		t.Run(label, func(t *testing.T) {
			script, calls, _ := collisionScript(t, read, "session already exists")
			_ = NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env})
			if got := readCalls(t, calls); len(got) != 1 {
				t.Fatalf("colliding Start ran %q, want only the clear: there is no own fence to restore", got)
			}
		})
	}
}

// Kills a start or launch that drives the box tmux of a pack that cannot
// fence (no input-fence, with or without the exec connection), whose box
// tmux need not hold a fence: Start neither clears nor publishes a fence and
// runs no in-box command, and a separable launch carries no fence command.
func TestExecStartOfAPackThatCannotFenceTouchesNoFence(t *testing.T) {
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	for label, caps := range map[string]string{"no capabilities": ``, "exec connection alone": `"proc.exec"`} {
		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":[`+caps+`]}' ;;
  is-running) echo true ;;
  exec) cat >> `+calls+`; echo >> `+calls+` ;;
  start) cat > /dev/null ;;
  *) exit 2 ;;
esac
`)
		if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err != nil {
			t.Fatalf("%s: Start: %v", label, err)
		}
		if got := readCalls(t, calls); len(got) != 0 {
			t.Fatalf("%s: Start ran %q in the box, want nothing", label, got)
		}
	}

	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec","proc.provision"]}' ;;
  exec) cat >> `+calls+`; echo >> `+calls+` ;;
  *) exit 2 ;;
esac
`)
	if err := NewProvider(script).launchAgent(context.Background(), "s", runtime.Config{Command: "agent", Env: env}); err != nil {
		t.Fatalf("launchAgent: %v", err)
	}
	launched := false
	for _, c := range readCalls(t, calls) {
		launched = launched || strings.HasSuffix(c, "'agent'")
		if strings.Contains(c, "@gc_fence") || strings.Contains(c, "';'") {
			t.Errorf("a launch on a pack that cannot fence ran %q, want no fence command", c)
		}
	}
	if !launched {
		t.Errorf("launchAgent ran %q, want the agent launched", readCalls(t, calls))
	}
}
