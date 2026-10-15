package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
		"#{&&:#{==:#{pane_id},%1},#{&&:#{==:#{@gc_fence},1172ccc4fbf9f6a6.#{pane_id}},#{!=:#{pane_dead},1}}}",
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
	g := newPaneGuard(fenceModelSession, testFence.Digest(), false)
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
				return errors.Is(err, runtime.ErrRuntimeUnavailable) && errors.Is(err, ErrNoServer) &&
					!errors.Is(err, runtime.ErrFenceMismatch) && !runtime.IsSessionGone(err)
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

// A session that vanishes while the server lives makes tmux 3.4 answer
// if-shell -F -t and display-message -t with exit 0 and every pane field
// empty. Wherever that lands in a fenced operation (its fence read, its
// adoption, a guard, a hidden write's check) it is an absent, non-partial
// miss: nothing typed, no buffer left. Kills a parse of the empty report as
// unproven or as an unexpected-output error.
func TestFencedOperationMissesAbsentWhenSessionVanishes(t *testing.T) {
	gone := func(m *fenceModel) { m.gone = true }
	cases := []struct {
		name   string
		hidden bool
		set    func(m *fenceModel)
	}{
		{"before the fence read", false, func(m *fenceModel) {
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 1 {
					gone(m)
				}
			}
		}},
		{"before the adoption", false, func(m *fenceModel) {
			m.own = ""
			m.beforeAdopt = gone
		}},
		{"before the first guard", false, func(m *fenceModel) {
			m.beforeGuard = func(m *fenceModel, _ int) { gone(m) }
		}},
		{"before a hidden write's check", true, func(m *fenceModel) {
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 2 { // verify read the pane first; this is the write's check
					gone(m)
				}
			}
		}},
	}
	for _, c := range cases {
		ops := map[string]func(*Provider) error{
			"text": func(p *Provider) error {
				return p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: testFence, Immediate: true})
			},
			"keys": func(p *Provider) error {
				return p.SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
			},
		}
		if c.hidden {
			ops = hiddenFenceOps
		}
		for opName, op := range ops {
			t.Run(c.name+"/"+opName, func(t *testing.T) {
				m := claudeModel("")()
				c.set(m)
				p := fenceModelProvider(m)
				var hidden *syncBuffer
				if c.hidden {
					hidden = installHiddenClient(t, p)
				}
				err := op(p)
				var miss *runtime.FenceMismatchError
				if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
					t.Fatalf("err = %v, want a non-partial absent miss", err)
				}
				if _, events, buffers := m.snapshot(); len(events) != 0 || buffers != 0 {
					t.Fatalf("typed %q, left %d buffers", events, buffers)
				}
				if hidden != nil && hidden.String() != "" {
					t.Fatalf("hidden client received %q", hidden.String())
				}
			})
		}
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
		{"env with no token is unproven", func(m *fenceModel) { delete(m.env, runtime.InstanceTokenEnv) }, runtime.FenceFoundUnproven, ""},
		{"incomplete env defaults counters", func(m *fenceModel) {
			delete(m.env, runtime.RuntimeEpochEnv)
			delete(m.env, runtime.ContinuationEpochEnv)
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

// FI9: Start's and Relaunch's keystrokes (the startup dialogs, the startup
// prompt) are never guarded by a caller's fence, even when their context
// carries one, or a fenced operation's guard: they type into the
// incarnation they are creating.
func TestStartRelaunchNeverGuardedByCallerFence(t *testing.T) {
	accept := true
	caller := runtime.InputFence{Generation: 9, Epoch: 9, Token: "caller"}
	for name, launch := range map[string]func(ctx context.Context, p *Provider, cfg runtime.Config) error{
		"start": func(ctx context.Context, p *Provider, cfg runtime.Config) error {
			return p.Start(ctx, fenceModelSession, cfg)
		},
		"relaunch": func(ctx context.Context, p *Provider, cfg runtime.Config) error {
			return p.Relaunch(ctx, fenceModelSession, cfg)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newFenceModel(nil, fenceEnv(testFence))
			m.overlay, m.overlayUntil = "Resume from summary\nResume full session as-is\nEnter to confirm", "Enter"
			m.screen = staticShellScreen
			p := fenceModelProvider(m)
			ctx := runtime.WithInputFence(context.Background(), caller)
			ctx = withPaneGuard(ctx, newPaneGuard(fenceModelSession, caller.Digest(), false))
			cfg := runtime.Config{Command: "sh", WorkDir: t.TempDir(), Env: fenceEnv(testFence), Nudge: "startup prompt", AcceptStartupDialogs: &accept}
			_ = launch(ctx, p, cfg)
			bare, events, _ := m.snapshot()
			if len(bare) == 0 || !slices.Contains(events, "key:Enter") {
				t.Fatalf("bare %q, events %q: want the startup keys sent directly", bare, events)
			}
			if m.guards != 0 {
				t.Fatalf("%d startup commands ran under a guard", m.guards)
			}
		})
	}
}

// The tmux provider's fencing is a static trait: the zero provider answers.
func TestTmuxInputFencingIsStatic(t *testing.T) {
	if got := (&Provider{}).InputFencing("anything"); got != runtime.InputFencingAtomic {
		t.Fatalf("InputFencing = %q, want atomic", got)
	}
}

// hiddenFenceOps are the fenced operations that type through a hidden
// attach client when the session has one.
var hiddenFenceOps = map[string]func(p *Provider) error{
	"text": func(p *Provider) error {
		return p.NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: testFence, Immediate: true})
	},
	"keys": func(p *Provider) error {
		return p.SendKeysFenced(context.Background(), fenceModelSession, testFence, "Up", "Enter")
	},
	"interrupt": func(p *Provider) error { return p.InterruptFenced(context.Background(), fenceModelSession, testFence) },
}

// The hidden attach client's PTY writes cannot be conditional inside tmux,
// so each is checked right before it: a new incarnation that takes the pane
// after the precheck and before the first write gets nothing, and the miss
// is not partial. Kills a hidden path that skips its check.
func TestHiddenAttachInputChecksFenceBeforeEachWrite(t *testing.T) {
	newer := runtime.InputFence{Generation: 3, Epoch: 1, Token: "newer"}
	for name, op := range hiddenFenceOps {
		t.Run(name, func(t *testing.T) {
			m := claudeModel("")()
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 2 { // verify read the pane first; this is the write's check
					m.own = newer.Digest() + "." + m.paneID
				}
			}
			p := fenceModelProvider(m)
			hidden := installHiddenClient(t, p)
			err := op(p)
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != newer.Digest() || miss.Partial {
				t.Fatalf("err = %v, want a non-partial miss finding the new incarnation", err)
			}
			if got := hidden.String(); got != "" {
				t.Fatalf("hidden client received %q", got)
			}
		})
	}
}

// tmux delivers a client's input to its active pane: a human who split a
// session and left focus on their own pane gets no fenced input through the
// hidden client, and the miss says the target is unproven.
func TestHiddenAttachInputRefusesActivePaneOtherThanAgent(t *testing.T) {
	for name, op := range hiddenFenceOps {
		t.Run(name, func(t *testing.T) {
			m := claudeModel("")()
			m.active = fenceModelSurvivor
			m.survivorOwn = testFence.Digest() + "." + fenceModelSurvivor // even a copied value
			p := fenceModelProvider(m)
			hidden := installHiddenClient(t, p)
			err := op(p)
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundUnproven || miss.Partial {
				t.Fatalf("err = %v, want a non-partial unproven miss", err)
			}
			if got := hidden.String(); got != "" {
				t.Fatalf("hidden client received %q", got)
			}
		})
	}
}

// tmux evaluates if-shell -t and display-message -t in the window's active
// pane when the target pane is gone. A guard must still miss (absent), from
// its condition and not only from the inner command's strict target, even
// when the surviving pane holds the same digest.
func TestGuardMissesWhenAgentPaneGoneInLiveWindow(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = staticShellScreen
	m.survivorOwn = testFence.Digest() + "." + fenceModelSurvivor
	m.beforeGuard = func(m *fenceModel, _ int) { m.paneGone = true }
	err := fenceModelProvider(m).SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
		t.Fatalf("err = %v, want a non-partial absent miss", err)
	}
	if _, events, _ := m.snapshot(); len(events) != 0 {
		t.Fatalf("events %q", events)
	}
	if m.strictGone != 0 {
		t.Fatalf("the guard's condition passed in the survivor: %d inner commands tried the gone pane", m.strictGone)
	}
}

// verify's read falls back the same way: a pane killed between the agent
// pane lookup and the fence read reads as absent, not as the survivor.
func TestVerifyReadOfGonePaneIsAbsent(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = staticShellScreen
	m.survivorOwn = testFence.Digest() + "." + fenceModelSurvivor
	m.beforeRead = func(m *fenceModel, _ int) { m.paneGone = true }
	err := fenceModelProvider(m).SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent {
		t.Fatalf("err = %v, want an absent miss", err)
	}
	if m.guards != 0 {
		t.Fatalf("%d guarded commands ran after a failed verify", m.guards)
	}
}

// Adoption whose pane dies before its if-shell: the fallback either reports
// the survivor's own value or fails the strict set-option; both are absent.
func TestAdoptionOfGonePaneIsAbsent(t *testing.T) {
	for _, survivorOwn := range []string{"", "1234567890abcdef." + fenceModelSurvivor} {
		m := newFenceModel(nil, fenceEnv(testFence))
		m.screen = staticShellScreen
		m.survivorOwn = survivorOwn
		m.beforeAdopt = func(m *fenceModel) { m.paneGone = true }
		err := fenceModelProvider(m).SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent {
			t.Fatalf("survivor own %q: err = %v, want an absent miss", survivorOwn, err)
		}
		if m.guards != 0 {
			t.Fatalf("survivor own %q: %d guarded commands ran", survivorOwn, m.guards)
		}
	}
}

// A marked session whose agent pane is gone is absent.
func TestFencedInputToMarkedSessionWithoutAgentPaneIsAbsent(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.orphan = true
	err := fenceModelProvider(m).SendKeysFenced(context.Background(), fenceModelSession, testFence, "Enter")
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent {
		t.Fatalf("err = %v, want an absent miss", err)
	}
}

// A server lost on a send the body swallows (the copy-mode exit) is still
// the operation's result: a fenced call never reports success it could not
// deliver.
func TestFencedSwallowedNoServerIsUnavailable(t *testing.T) {
	m := claudeModel("")()
	m.inMode = true
	m.failGuard = map[int]error{1: ErrNoServer}
	err := fenceModelProvider(m).NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: testFence, Immediate: true})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
	if _, events, _ := m.snapshot(); !slices.Contains(events, "text:hello") {
		t.Fatalf("events %q: want the body to have run past the swallowed failure", events)
	}
}

// Partial counts only what carries the operation's effect: a nudge's text
// (and what follows it), and any key of a keys, interrupt or ctx-fenced
// operation. A copy-mode exit, a composer clear or a dialog key before a
// nudge's text never makes a miss partial.
func TestFencePartialCountsOnlyEffectInput(t *testing.T) {
	t.Run("nudge miss after the composer clear", func(t *testing.T) {
		m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
		m.env["GC_PROVIDER"] = "opencode"
		m.screen = staticShellScreen
		m.inMode, m.detached = true, true
		m.beforeGuard = func(m *fenceModel, _ int) {
			if slices.Contains(m.events, "key:C-u") {
				m.dead = true
			}
		}
		err := fenceModelProvider(m).NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: testFence, Immediate: true})
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Partial {
			_, events, _ := m.snapshot()
			t.Fatalf("err = %v (events %q), want a non-partial miss", err, events)
		}
		if _, events, _ := m.snapshot(); !slices.Equal(events, []string{"copy:cancel", "key:C-u"}) {
			t.Fatalf("events %q, want the copy-mode exit and the clear before the miss", events)
		}
	})
	t.Run("nudge miss after the text", func(t *testing.T) {
		m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
		m.env["GC_PROVIDER"] = "opencode"
		m.screen = staticShellScreen
		m.beforeGuard = func(m *fenceModel, _ int) {
			if slices.Contains(m.events, "text:hello") {
				m.dead = true
			}
		}
		err := fenceModelProvider(m).NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: testFence, Immediate: true})
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || !miss.Partial {
			t.Fatalf("err = %v, want a partial miss", err)
		}
	})
	t.Run("copy-mode exit never counts", func(t *testing.T) {
		m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
		p := fenceModelProvider(m)
		g := newPaneGuard(fenceModelSession, testFence.Digest(), false)
		ctx := withPaneGuard(context.Background(), g)
		if err := g.verify(ctx, p.tm); err != nil {
			t.Fatal(err)
		}
		target := fakeAgentPane(fenceModelSession)
		if _, err := p.tm.runCtx(ctx, "send-keys", "-t", target, "-X", "cancel"); err != nil {
			t.Fatal(err)
		}
		m.dead = true
		_, err := p.tm.runCtx(ctx, "send-keys", "-t", target, "Enter")
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Partial {
			t.Fatalf("err = %v, want a non-partial miss", err)
		}
	})
}

// Fenced literal text that looks like a flag is text.
func TestFencedLiteralTextThatLooksLikeAFlag(t *testing.T) {
	for _, text := range []string{"-t", "-l", "-X", "-H"} {
		m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
		m.env["GC_PROVIDER"] = "opencode"
		m.screen = staticShellScreen
		err := fenceModelProvider(m).NudgeFenced(context.Background(), fenceModelSession, runtime.TextContent(text), runtime.FencedNudge{Fence: testFence, Immediate: true})
		if err != nil {
			t.Fatalf("NudgeFenced(%q) = %v", text, err)
		}
		if _, events, _ := m.snapshot(); !slices.Contains(events, "text:"+text) {
			t.Fatalf("NudgeFenced(%q) typed %q", text, events)
		}
	}
}

// FI7: unfenced input commands keep main's behavior, never cut short by the
// caller's context; only a fenced operation's input runs under it.
func TestUnfencedInputCommandIgnoresCallerContext(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence))
	tm := fenceModelProvider(m).tm
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tm.runCtx(ctx, "send-keys", "-t", fakeAgentPane(fenceModelSession), "Enter"); err != nil {
		t.Fatalf("unfenced send-keys under a canceled ctx = %v", err)
	}
	if _, events, _ := m.snapshot(); !slices.Equal(events, []string{"key:Enter"}) {
		t.Fatalf("events %q", events)
	}
}

// wrapError classifies set-option's missing-pane answer as a missing
// session, as it does tmux's other "can't find" answers.
func TestWrapErrorNoSuchPane(t *testing.T) {
	err := wrapError(errors.New("exit status 1"), "no such pane: =lab:@0.%1", []string{"set-option"})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("wrapError(no such pane) = %v, want ErrSessionNotFound", err)
	}
}
