package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Pins the guard's exact if-shell: the condition requires the pane's own
// fence and a live pane, the else branch deletes the staged buffer and
// reports what it found, and every argument is single-quoted.
func TestGuardedInputArgsGolden(t *testing.T) {
	got, err := guardedInputArgs("=lab:@1.%1", "1172ccc4fbf9f6a6", []string{"paste-buffer", "-d", "-r", "-b", "gc-nudge-1-2", "-t", "=lab:@1.%1"}, "gc-nudge-1-2")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"if-shell", "-F", "-t", "=lab:@1.%1",
		"#{&&:#{==:#{@gc_fence},1172ccc4fbf9f6a6.#{pane_id}},#{!=:#{pane_dead},1}}",
		"'paste-buffer' '-d' '-r' '-b' 'gc-nudge-1-2' '-t' '=lab:@1.%1'",
		"'delete-buffer' '-b' 'gc-nudge-1-2' ; 'display-message' '-p' '-t' '=lab:@1.%1' 'gc-fence-miss:#{@gc_fence}|#{pane_id}|#{pane_dead}'",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("guardedInputArgs =\n%q\nwant\n%q", got, want)
	}
	keys, err := guardedInputArgs("=lab:@1.%1", "1172ccc4fbf9f6a6", []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if keys[6] != "'display-message' '-p' '-t' '=lab:@1.%1' 'gc-fence-miss:#{@gc_fence}|#{pane_id}|#{pane_dead}'" {
		t.Fatalf("keys else branch = %q, want only the miss report", keys[6])
	}
}

// Kills a guard that lets text or a format into a command string tmux
// parses: a quote, a control byte, a format and an empty argument are
// refused, as is a digest that is not one.
func TestGuardedInputArgsRefusesUnsafeArgs(t *testing.T) {
	for _, arg := range []string{"it's", "a\nb", "a\x1bb", "a\x7f", "#{pane_pid}", "x#{@gc_fence}", ""} {
		if _, err := guardedInputArgs("=lab:@1.%1", "1172ccc4fbf9f6a6", []string{"send-keys", "-t", "=lab:@1.%1", arg}, ""); err == nil {
			t.Errorf("argument %q was accepted", arg)
		}
	}
	if _, err := guardedInputArgs("=lab:@1.%1", "abc}", []string{"send-keys", "-t", "=lab:@1.%1", "Enter"}, ""); err == nil {
		t.Error("an invalid digest was accepted")
	}
}

// Kills a guarded send-keys that accepts text or an unknown flag as a key,
// or that addresses anything but the session's agent pane.
func TestFencedSendKeysRefusesNonKeysAndForeignTargets(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	p := fenceModelProvider(m)
	g := newPaneGuard(fenceModelSession, testFence.Digest())
	ctx := withPaneGuard(context.Background(), g)
	for _, args := range [][]string{
		{"send-keys", "-t", "=lab:@1.%1", "hello world"},
		{"send-keys", "-t", "=lab:@1.%1", "-H", "41"},
		{"send-keys", "-t", "=other:@1.%1", "Enter"},
		{"send-keys", "-t", "%1", "Enter"},
		{"send-keys", "-t", "=lab:", "Enter"},
		{"paste-buffer", "-t", "=lab:@1.%1"},
	} {
		if _, err := p.tm.runCtx(ctx, args...); err == nil || errors.Is(err, runtime.ErrFenceMismatch) {
			t.Errorf("%q = %v, want a refusal", args, err)
		}
	}
	if _, events, _ := m.snapshot(); len(events) != 0 {
		t.Fatalf("refused commands typed %q", events)
	}
}

// A new incarnation takes the pane after the precheck, before the first
// keystroke: the paste misses, its buffer is deleted, nothing reaches the
// pane, every later input short-circuits, and the miss is not partial.
func TestFenceMissStopsOperationAndDeletesBuffer(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = staticShellScreen
	newer := runtime.InputFence{Generation: 3, Epoch: 1, Token: "newer"}
	m.beforeGuard = func(m *fenceModel, _ int) { m.own = newer.Digest() + "." + m.paneID }
	p := fenceModelProvider(m)
	err := p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent(strings.Repeat("x", maxSendKeysLiteralLen+10)), runtime.FencedNudge{Fence: testFence, Immediate: true})
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != newer.Digest() || miss.Partial {
		t.Fatalf("err = %v, want a non-partial miss finding the new incarnation", err)
	}
	_, events, buffers := m.snapshot()
	if len(events) != 0 || buffers != 0 {
		t.Fatalf("events %q, %d buffers left; want none", events, buffers)
	}
	if m.guards != 1 {
		t.Fatalf("%d guarded commands ran, want the operation stopped at its first miss", m.guards)
	}
}

// The incarnation changes between the paste and the submit: the text was
// typed, the submit misses, and the result says so (Partial).
func TestFenceMissAfterAppliedInputIsPartial(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = staticShellScreen
	m.beforeGuard = func(m *fenceModel, n int) {
		if n == 2 {
			m.dead = true
		}
	}
	p := fenceModelProvider(m)
	err := p.SendKeysFenced(context.Background(), fenceModelSession, testFence, "Escape", "Enter", "Enter")
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || !miss.Partial {
		t.Fatalf("err = %v, want a partial miss finding a dead pane", err)
	}
	if _, events, _ := m.snapshot(); !slices.Equal(events, []string{"key:Escape"}) {
		t.Fatalf("events = %q, want only the key before the miss", events)
	}
}

// Kills a fenced path that turns a gone session, a dead pane or a missing
// server into success, as the unfenced best-effort paths do.
func TestFencedGoneAndNoServerNeverSucceed(t *testing.T) {
	ops := map[string]func(*Provider) error{
		"nudge": func(p *Provider) error {
			return p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hi"), runtime.FencedNudge{Fence: testFence, Immediate: true})
		},
		"keys": func(p *Provider) error {
			return p.SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
		},
		"interrupt": func(p *Provider) error { return p.InterruptFenced(context.Background(), fenceModelSession, testFence) },
		"clear": func(p *Provider) error {
			return p.ClearInput(runtime.WithInputFence(context.Background(), testFence), fenceModelSession, 0)
		},
	}
	for name, op := range ops {
		for _, c := range []struct {
			fault string
			set   func(*fenceModel)
			check func(error) bool
		}{
			{"gone", func(m *fenceModel) { m.gone = true }, func(err error) bool {
				var miss *runtime.FenceMismatchError
				return errors.As(err, &miss) && miss.Found == runtime.FenceFoundAbsent
			}},
			{"dead", func(m *fenceModel) { m.dead = true }, func(err error) bool {
				var miss *runtime.FenceMismatchError
				return errors.As(err, &miss) && miss.Found == runtime.FenceFoundDead
			}},
			{"no server", func(m *fenceModel) { m.noServer = true }, func(err error) bool {
				return errors.Is(err, runtime.ErrRuntimeUnavailable) && !errors.Is(err, runtime.ErrFenceMismatch)
			}},
			{"gone mid-operation", func(m *fenceModel) { m.beforeGuard = func(m *fenceModel, _ int) { m.gone = true } }, func(err error) bool {
				var miss *runtime.FenceMismatchError
				return errors.As(err, &miss) && miss.Found == runtime.FenceFoundAbsent
			}},
		} {
			m := claudeModel("draft")()
			c.set(m)
			err := op(fenceModelProvider(m))
			if !c.check(err) {
				t.Errorf("%s with %s: err = %v", name, c.fault, err)
			}
			if _, events, _ := m.snapshot(); len(events) != 0 {
				t.Errorf("%s with %s typed %q", name, c.fault, events)
			}
		}
	}
	// Unfenced, the same faults stay the best-effort no-op they were.
	m := claudeModel("")()
	m.gone = true
	if err := fenceModelProvider(m).SendKeys(fenceModelSession, "Enter"); err != nil {
		t.Fatalf("unfenced SendKeys to a gone session = %v, want nil", err)
	}
}

// Kills adoption that trusts anything but the pane's own value or the
// session's environment, writes over an own value, or adopts more than once.
func TestFenceAdoptionFromSessionEnv(t *testing.T) {
	other := runtime.InputFence{Generation: 1, Epoch: 1, Token: "other"}
	cases := []struct {
		name      string
		setup     func(m *fenceModel)
		wantFound string // "" = delivered
		wantOwn   string
	}{
		{"own value matches", func(m *fenceModel) { m.ownFenced(testFence) }, "", testFence.Digest() + ".%1"},
		{"own value differs, env ignored", func(m *fenceModel) { m.ownFenced(other) }, other.Digest(), other.Digest() + ".%1"},
		{"no value adopts the env", func(*fenceModel) {}, "", testFence.Digest() + ".%1"},
		{"inherited value is not own", func(m *fenceModel) { m.inherited = other.Digest() + ".%9" }, "", testFence.Digest() + ".%1"},
		{"env with no token is unproven", func(m *fenceModel) { delete(m.env, "GC_INSTANCE_TOKEN") }, runtime.FenceFoundUnproven, ""},
		{"incomplete env defaults counters", func(m *fenceModel) {
			delete(m.env, "GC_RUNTIME_EPOCH")
			delete(m.env, "GC_CONTINUATION_EPOCH")
		}, runtime.InputFence{Generation: 1, Epoch: 1, Token: testFence.Token}.Digest(), runtime.InputFence{Generation: 1, Epoch: 1, Token: testFence.Token}.Digest() + ".%1"},
		{"env names another incarnation", func(m *fenceModel) { m.env = fenceEnv(other) }, other.Digest(), other.Digest() + ".%1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newFenceModel(nil, fenceEnv(testFence))
			m.screen = staticShellScreen
			c.setup(m)
			err := fenceModelProvider(m).SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
			var miss *runtime.FenceMismatchError
			switch {
			case c.wantFound == "" && err != nil:
				t.Fatalf("err = %v, want delivery", err)
			case c.wantFound != "" && (!errors.As(err, &miss) || miss.Found != c.wantFound):
				t.Fatalf("err = %v, want a miss finding %s", err, c.wantFound)
			}
			if m.own != c.wantOwn {
				t.Fatalf("pane fence = %q, want %q", m.own, c.wantOwn)
			}
		})
	}
	// Adoption happens once: a second operation reads the own value.
	m := newFenceModel(nil, fenceEnv(testFence))
	m.screen = staticShellScreen
	p := fenceModelProvider(m)
	for range 2 {
		if err := p.SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter"); err != nil {
			t.Fatal(err)
		}
	}
	envReads := 0
	for _, c := range m.calls {
		if c[0] == "show-environment" && len(c) == 3 {
			envReads++
		}
	}
	if envReads != 1 {
		t.Fatalf("session env read %d times, want once", envReads)
	}
}

// A created session's pane publishes the fence its start environment names;
// a start environment with no token publishes nothing.
func TestStartPublishesInputFence(t *testing.T) {
	for _, withToken := range []bool{true, false} {
		exec := &fakeExecutor{out: "%3"}
		tm := NewTmux()
		tm.exec = exec
		env := map[string]string{"GC_SESSION_NAME": "lab"}
		if withToken {
			env = fenceEnv(testFence)
		}
		if err := tm.NewSessionWithCommandAndEnv("lab", "", "sleep 1", env); err != nil {
			t.Fatal(err)
		}
		var sets [][]string
		for _, c := range exec.calls {
			if slices.Contains(c, inputFenceOption) {
				sets = append(sets, c[1:])
			}
		}
		want := [][]string{{"set-option", "-p", "-F", "-t", "%3", inputFenceOption, testFence.Digest() + ".#{pane_id}"}}
		if !withToken {
			want = nil
		}
		if !slices.EqualFunc(sets, want, slices.Equal) {
			t.Fatalf("token %v: fence writes %q, want %q", withToken, sets, want)
		}
		for _, c := range exec.calls {
			if strings.Contains(strings.Join(c, " "), testFence.Token) && slices.Contains(c, inputFenceOption) {
				t.Fatalf("the token reached argv: %q", c)
			}
		}
	}
}

// A relaunch republishes the fence the session environment names, the
// identity the respawned process inherits.
func TestRelaunchRepublishesFromSessionEnv(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence))
	tm := fenceModelProvider(m).tm
	if err := (&tmuxStartOps{tm: tm}).respawnAgent(fenceModelSession, "", "claude", nil); err != nil {
		t.Fatal(err)
	}
	if want := testFence.Digest() + ".%1"; m.own != want {
		t.Fatalf("pane fence after respawn = %q, want %q", m.own, want)
	}
}

// Caller-proven adoption (DES-BX D9) writes only while the pane still runs
// the proven process, and never over a different root.
func TestAdoptInputFenceRequiresProvenPanePID(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence))
	p := fenceModelProvider(m)
	if err := p.AdoptInputFence(fenceModelSession, testFence, 1111); !errors.Is(err, ErrPaneRootChanged) {
		t.Fatalf("AdoptInputFence(other pid) = %v, want ErrPaneRootChanged", err)
	}
	if m.own != "" {
		t.Fatalf("a refused adoption wrote %q", m.own)
	}
	if err := p.AdoptInputFence(fenceModelSession, testFence, 4242); err != nil {
		t.Fatalf("AdoptInputFence(proven pid) = %v", err)
	}
	if want := testFence.Digest() + ".%1"; m.own != want {
		t.Fatalf("pane fence = %q, want %q", m.own, want)
	}
	if err := p.AdoptInputFence(fenceModelSession, runtime.InputFence{}, 4242); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("AdoptInputFence(zero fence) = %v, want ErrInvalidInputFence", err)
	}
}

// FI9: Start's keystrokes (the startup dialogs, the startup prompt) are
// never guarded by a caller's fence, even when Start's context carries one:
// they type into the incarnation Start is creating.
func TestStartRelaunchNeverGuardedByCallerFence(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence))
	m.overlay, m.overlayUntil = "Resume from summary\nResume full session as-is\nEnter to confirm", "Enter"
	m.screen = staticShellScreen
	tm := fenceModelProvider(m).tm
	ops := &tmuxStartOps{tm: tm}
	ctx := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 9, Epoch: 9, Token: "caller"})
	_ = tm.DismissKnownDialogs(ctx, fenceModelSession, 10*time.Millisecond)
	_ = ops.sendKeys(fenceModelSession, "startup prompt")
	bare, events, _ := m.snapshot()
	if len(bare) == 0 || !slices.Contains(events, "text:startup prompt") || !slices.Contains(events, "key:Enter") {
		t.Fatalf("bare %q, events %q: want the startup keys sent directly", bare, events)
	}
	if m.guards != 0 {
		t.Fatalf("%d startup commands ran under a guard", m.guards)
	}
}

// The tmux provider's fencing is a static trait: the zero provider answers.
func TestTmuxInputFencingIsStatic(t *testing.T) {
	if got := (&Provider{}).InputFencing("anything"); got != runtime.InputFencingAtomic {
		t.Fatalf("InputFencing = %q, want atomic", got)
	}
}
