//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fenceTestProvider is a tmux provider on its own private server, torn
// down (that socket only) when the test ends.
func fenceTestProvider(t *testing.T, tag string) *Provider {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName(tag)
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	p := NewProviderWithConfig(cfg)
	t.Cleanup(func() { _ = p.TeardownServer() })
	return p
}

// rawCatCommand is a pane that writes every byte it is typed, unechoed and
// unbuffered, to file.
func rawCatCommand(file string) string {
	return fmt.Sprintf(`sh -c 'stty raw -echo; exec cat > %s'`, file)
}

// startRawCat starts session name as f's incarnation running rawCatCommand,
// and returns its capture file.
func startRawCat(t *testing.T, p *Provider, name string, f runtime.InputFence, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "typed")
	env := fenceEnv(f)
	env["GC_PROVIDER"] = "opencode" // the single-attempt fallback submit; no Escape
	maps.Copy(env, extra)
	if err := p.Start(context.Background(), name, runtime.Config{Command: rawCatCommand(file), WorkDir: dir, Env: env}); err != nil {
		t.Fatalf("Start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	waitCaptureFile(t, file)
	return file
}

// eventually polls cond until it holds, failing after 10s: each wait ends on
// the signal the test is about, not on elapsed time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// waitCaptureFile waits for rawCatCommand's shell to open file: stty has
// run, and cat reads the raw tty from here on.
func waitCaptureFile(t *testing.T, file string) {
	t.Helper()
	eventually(t, "the pane to open "+file, func() bool {
		_, err := os.Stat(file)
		return err == nil
	})
}

func typedSoFar(file string) string {
	data, _ := os.ReadFile(file)
	return string(data)
}

// waitTyped waits until file's typed bytes satisfy cond and returns them.
func waitTyped(t *testing.T, file string, cond func(string) bool) string {
	t.Helper()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for {
		got := typedSoFar(file)
		if cond(got) {
			return got
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("typed into %s: %q, never what the test waits for", file, got)
		}
	}
}

func containsFn(want string) func(string) bool {
	return func(got string) bool { return strings.Contains(got, want) }
}

func listBuffers(t *testing.T, p *Provider) string {
	t.Helper()
	out, err := p.tm.run("list-buffers", "-F", "#{buffer_name}")
	if err != nil && !errors.Is(err, ErrNoServer) {
		t.Fatalf("list-buffers: %v", err)
	}
	return strings.TrimSpace(out)
}

func immediate(f runtime.InputFence) runtime.FencedNudge {
	return runtime.FencedNudge{Fence: f, Immediate: true}
}

// The plan's acceptance test (A7): a nudge taken under incarnation A's fence
// reaches nothing after A is replaced by B under the same name — no keystroke
// in B, no buffer left — and B's own fence delivers.
func TestFencedNudgeRealTmuxRespawnBetweenTakeAndDelivery(t *testing.T) {
	p := fenceTestProvider(t, "fr")
	const name = "gc-fence-respawn"
	fa := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-a"}
	fb := runtime.InputFence{Generation: 2, Epoch: 1, Token: "token-b"}
	_ = startRawCat(t, p, name, fa, nil)
	if err := p.Stop(name); err != nil {
		t.Fatal(err)
	}
	fileB := startRawCat(t, p, name, fb, nil)

	err := p.NudgeFenced(context.Background(), name, runtime.TextContent("for-a"), immediate(fa))
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != fb.Digest() || miss.Partial {
		t.Fatalf("NudgeFenced(A's fence) = %v, want a non-partial miss finding B", err)
	}
	if b := listBuffers(t, p); b != "" {
		t.Fatalf("buffers left: %q", b)
	}
	if err := p.NudgeFenced(context.Background(), name, runtime.TextContent("for-b"), immediate(fb)); err != nil {
		t.Fatalf("NudgeFenced(B's fence) = %v", err)
	}
	// B's own delivery is the signal that anything sent before it has landed.
	if got := waitTyped(t, fileB, containsFn("for-b\r")); strings.Contains(got, "for-a") {
		t.Fatalf("B received %q: A's fenced nudge reached it", got)
	}
}

// F2's other half, the V2-2 residual: a same-identity relaunch keeps the
// incarnation, so its fence still delivers into the respawned process.
func TestFencedNudgeRealTmuxSameIdentityRelaunchDelivers(t *testing.T) {
	p := fenceTestProvider(t, "fl")
	const name = "gc-fence-relaunch"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-r"}
	_ = startRawCat(t, p, name, f, nil)
	dir := t.TempDir()
	file := filepath.Join(dir, "typed")
	if err := p.Relaunch(context.Background(), name, runtime.Config{Command: rawCatCommand(file), WorkDir: dir}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitCaptureFile(t, file)
	if err := p.NudgeFenced(context.Background(), name, runtime.TextContent("after-relaunch"), immediate(f)); err != nil {
		t.Fatalf("NudgeFenced after a same-identity relaunch = %v", err)
	}
	waitTyped(t, file, containsFn("after-relaunch\r"))
}

// A pane without its own fence (an older gc, a failed publish) is adopted
// once from the session environment and then holds the fence.
func TestFencedNudgeRealTmuxAdoptsUnfencedPane(t *testing.T) {
	p := fenceTestProvider(t, "fa")
	const name = "gc-fence-adopt"
	f := runtime.InputFence{Generation: 3, Epoch: 2, Token: "token-adopt"}
	file := startRawCat(t, p, name, f, nil)
	target, err := p.tm.agentPaneTarget(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.tm.run("set-option", "-p", "-u", "-t", target, inputFenceOption); err != nil {
		t.Fatal(err)
	}
	if err := p.NudgeFenced(context.Background(), name, runtime.TextContent("adopted"), immediate(f)); err != nil {
		t.Fatalf("NudgeFenced into an unfenced pane = %v", err)
	}
	waitTyped(t, file, containsFn("adopted\r"))
	out, err := p.tm.run("display-message", "-p", "-t", target, fenceReadFormat)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := parseFenceRead(out); !ok || r.own() != f.Digest() {
		t.Fatalf("pane fence after adoption = %q, want own %s", out, f.Digest())
	}
}

// fenceTextE4 is the byte-exactness corpus: quotes, a mid-string ";" and
// "\;", a format, shell syntax, a tab, a newline and UTF-8.
const fenceTextE4 = "it's \"q\" a;b c\\;d #{pane_id} $HOME ~ {} %if \tTAB\nLF ünï 日本"

// Text arrives byte for byte (E4), fenced and unfenced alike: a trailing ";",
// ";;" or "\;" and a leading "-" included, which tmux's argument parser would
// otherwise read as a separator or a flag (E5).
func TestFencedNudgeRealTmuxTextIsByteExact(t *testing.T) {
	p := fenceTestProvider(t, "fx")
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-x"}
	texts := []string{fenceTextE4, fenceTextE4 + ";", ";", "x;", "x;;", `x\;`, "-x", "-l", "- item"}
	deliveries := []struct {
		name string
		send func(name, text string) error
	}{
		{"fenced", func(name, text string) error {
			return p.NudgeFenced(context.Background(), name, runtime.TextContent(text), immediate(f))
		}},
		{"nudge", func(name, text string) error { return p.NudgeNow(name, runtime.TextContent(text)) }},
		{"debounced", func(name, text string) error { return p.tm.SendKeysDebounced(name, text, 0) }},
	}
	for _, d := range deliveries {
		// One session per delivery: each text ends the segment its Enter
		// ends (a nudge clears the input line with C-u before it types).
		name := "gc-fence-exact-" + d.name
		file := startRawCat(t, p, name, f, nil)
		var sent []string
		for _, text := range texts {
			if err := d.send(name, text); err != nil {
				t.Errorf("%s(%q) = %v", d.name, text, err)
				continue
			}
			sent = append(sent, text)
			n := len(sent)
			waitTyped(t, file, func(got string) bool { return strings.Count(got, "\r") >= n })
		}
		got := strings.Split(strings.TrimSuffix(typedSoFar(file), "\r"), "\r")
		if len(got) != len(sent) {
			t.Errorf("%s typed %q, want %d lines %q", d.name, got, len(sent), sent)
			continue
		}
		for i, text := range sent {
			// Exact: only a nudge's C-u (\x15) may precede the text, so a
			// stray "\" or "--" typed in front of it fails.
			if strings.TrimLeft(got[i], "\x15") != text {
				t.Errorf("%s(%q) typed %q", d.name, text, got[i])
			}
		}
	}
}

// A remain-on-exit corpse is a miss (found dead), never a silent drop: tmux
// accepts send-keys to a dead pane and discards them (E9).
func TestFencedInputRealTmuxDeadPaneMisses(t *testing.T) {
	p := fenceTestProvider(t, "fd")
	const name = "gc-fence-dead"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-d"}
	if err := p.tm.NewSessionWithCommandAndEnv(name, t.TempDir(), "sleep 0.5", fenceEnv(f)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.tm.KillSession(name) })
	if err := p.tm.SetRemainOnExit(name, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pane to die", func() bool {
		dead, _ := p.tm.IsPaneDead(name)
		return dead
	})
	for op, err := range map[string]error{
		"nudge":     p.NudgeFenced(context.Background(), name, runtime.TextContent("x"), immediate(f)),
		"keys":      p.SendKeysFenced(context.Background(), name, f, "Enter"),
		"interrupt": p.InterruptFenced(context.Background(), name, f),
	} {
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead {
			t.Errorf("%s into a dead pane = %v, want a miss finding it dead", op, err)
		}
	}
}

// Keys and interrupts under another incarnation's fence send nothing.
func TestFencedSendKeysAndInterruptRealTmuxMismatch(t *testing.T) {
	p := fenceTestProvider(t, "fk")
	const name = "gc-fence-keys"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-k"}
	other := runtime.InputFence{Generation: 1, Epoch: 2, Token: "token-k"}
	file := startRawCat(t, p, name, f, nil)
	if err := p.SendKeysFenced(context.Background(), name, other, "Enter"); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("SendKeysFenced(other) = %v", err)
	}
	if err := p.InterruptFenced(context.Background(), name, other); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("InterruptFenced(other) = %v", err)
	}
	if err := p.SendKeysFenced(context.Background(), name, f, "Enter"); err != nil {
		t.Fatalf("SendKeysFenced(own) = %v", err)
	}
	// The own-fence Enter is the signal; anything before it was misfenced.
	if got := waitTyped(t, file, containsFn("\r")); got != "\r" {
		t.Fatalf("pane received %q, want only the own-fence Enter", got)
	}
}

// Caller-proven adoption (DES-BX D9) on real tmux: refused for another pid,
// written for the pane's own.
func TestAdoptInputFenceRealTmux(t *testing.T) {
	p := fenceTestProvider(t, "fp")
	const name = "gc-fence-proven"
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-p"}
	if err := p.tm.NewSessionWithCommandAndEnv(name, t.TempDir(), "sleep 300", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.tm.KillSession(name) })
	pidText, err := p.tm.GetPanePID(name)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AdoptInputFence(name, f, pid+100000); !errors.Is(err, ErrPaneRootChanged) {
		t.Fatalf("AdoptInputFence(other pid) = %v, want ErrPaneRootChanged", err)
	}
	if err := p.SendKeysFenced(context.Background(), name, f, "Enter"); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("an unproven pane with no session identity accepted fenced input: %v", err)
	}
	if err := p.AdoptInputFence(name, f, pid); err != nil {
		t.Fatalf("AdoptInputFence(own pid) = %v", err)
	}
	if err := p.SendKeysFenced(context.Background(), name, f, "Enter"); err != nil {
		t.Fatalf("SendKeysFenced after adoption = %v", err)
	}
}

// fencedTmuxProvider serves tm's sessions through the fenced entry points.
func fencedTmuxProvider(tm *Tmux) *Provider {
	return &Provider{tm: tm, cfg: tm.cfg, cache: NewStateCache(&tmuxFetcher{tm: tm}, time.Second), workDirs: map[string]string{}}
}

// The nudge integration scenarios, through NudgeFenced with the pane's own
// fence: each ends as its unfenced test does.
func TestFencedNudgeParityRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-parity"}
	start := func(t *testing.T, tm *Tmux, name, command, process string, env map[string]string) {
		t.Helper()
		maps.Copy(env, fenceEnv(f))
		_ = tm.KillSession(name)
		if err := tm.NewSessionWithCommandAndEnv(name, t.TempDir(), command, env); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tm.KillSession(name) })
		eventually(t, name+" to run "+process, func() bool {
			cmd, err := tm.GetPaneCommand(name)
			return err == nil && cmd == process
		})
	}
	captureHas := func(tm *Tmux, name string, cond func(string) bool) func() bool {
		return func() bool {
			out, err := tm.CapturePaneAll(name)
			return err == nil && cond(out)
		}
	}
	ctx := context.Background()

	t.Run("clear before paste", func(t *testing.T) {
		tm := testTmux()
		name := fmt.Sprintf("gt-fence-clear-%d", time.Now().UnixNano()%100000)
		start(t, tm, name, "cat -v", "cat", map[string]string{"GC_PROVIDER": "opencode"})
		if _, err := tm.run("send-keys", "-t", name, "-l", "leftover-draft"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the leftover draft", captureHas(tm, name, containsFn("leftover-draft")))
		if err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent("fresh-message"), immediate(f)); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the submitted message", captureHas(tm, name, func(out string) bool {
			return slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool { return strings.TrimSpace(l) == "fresh-message" })
		}))
		if out, _ := tm.CapturePaneAll(name); strings.Contains(out, "leftover-draftfresh-message") {
			t.Fatalf("pending input was not cleared before the paste:\n%s", out)
		}
	})

	t.Run("submit confirmed once", func(t *testing.T) {
		tm := testTmux()
		dir := t.TempDir()
		fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
		name := fmt.Sprintf("gt-fence-confirm-%d", time.Now().UnixNano()%100000)
		start(t, tm, name, fake, "fakeclaude", map[string]string{"GC_PROVIDER": "claude", "GC_TEST_BUSY_AFTER": "1"})
		if err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent("hello-confirm"), immediate(f)); err != nil {
			t.Fatal(err)
		}
		out, _ := tm.CapturePaneAll(name)
		if !strings.Contains(out, "esc to interrupt") || strings.Contains(out, "ENTER#2") {
			t.Fatalf("pane:\n%s", out)
		}
	})

	t.Run("submit re-entered", func(t *testing.T) {
		tm := testTmux()
		dir := t.TempDir()
		fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
		name := fmt.Sprintf("gt-fence-reenter-%d", time.Now().UnixNano()%100000)
		start(t, tm, name, fake, "fakeclaude", map[string]string{"GC_PROVIDER": "claude", "GC_TEST_BUSY_AFTER": "2"})
		if err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent("hello-reenter"), immediate(f)); err != nil {
			t.Fatal(err)
		}
		out, _ := tm.CapturePaneAll(name)
		if !strings.Contains(out, "ENTER#2") || !strings.Contains(out, "esc to interrupt") {
			t.Fatalf("pane:\n%s", out)
		}
	})

	t.Run("submit unconfirmed", func(t *testing.T) {
		tm := testTmux()
		dir := t.TempDir()
		fake := buildBusyOnEnterBinary(t, dir, "fakeclaude-neverbusy")
		name := fmt.Sprintf("gt-fence-unconfirmed-%d", time.Now().UnixNano()%100000)
		start(t, tm, name, fake, "fakeclaude-neverbusy", map[string]string{"GC_PROVIDER": "claude", "GC_TEST_BUSY_AFTER": "100"})
		err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent("hello-unconfirmed"), immediate(f))
		if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("err = %v, want ErrNudgeSubmitUnconfirmed", err)
		}
	})

	// TestNudgeRecoversStagedDraftSwallowedByPasteIngest's recovery case on
	// the fenced path: a codex-shaped TUI eats the first submits of a large
	// paste, and the fenced nudge still lands it exactly once.
	t.Run("staged draft recovered", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 required for the fake TUI")
		}
		script, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui_codex.py"))
		if err != nil {
			t.Fatal(err)
		}
		tm := testTmux()
		tm.cfg.NudgeReadyTimeout = 10 * time.Second
		tm.cfg.NudgeLockTimeout = 30 * time.Second
		logPath := filepath.Join(t.TempDir(), "tui.log")
		const swallow = submitEnterMaxSends + 2
		name := fmt.Sprintf("gt-fence-staged-%d", time.Now().UnixNano()%100000)
		cmd := fmt.Sprintf("python3 %s %s %d %.3f 30", shellQuote(script), shellQuote(logPath), swallow, 0.1)
		start(t, tm, name, cmd, "python3", map[string]string{"GC_PROVIDER": "codex"})
		eventually(t, "the fake TUI to paint", captureHas(tm, name, containsFn("fake-codex-tui")))
		large := "gc-staged-draft-probe " + strings.Repeat("work the claimed bead ", 60)
		if err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent(large), immediate(f)); err != nil {
			t.Fatalf("NudgeFenced = %v, want the staged draft recovered", err)
		}
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(raw), "\tSUBMIT\t"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
		if got := strings.Count(string(raw), "\tSWALLOWED\t"); got != swallow {
			t.Fatalf("TUI swallowed submits = %d, want %d", got, swallow)
		}
	})

	t.Run("declared submit sequence", func(t *testing.T) {
		origSeq := nudgeSubmitKeySequences
		nudgeSubmitKeySequences = map[string][]string{"testfam": {"Escape", "Enter"}}
		defer func() { nudgeSubmitKeySequences = origSeq }()
		origSkip := providersSkippingEscapeBeforeEnter
		providersSkippingEscapeBeforeEnter = append(append([]string(nil), origSkip...), "testfam")
		defer func() { providersSkippingEscapeBeforeEnter = origSkip }()
		tm := testTmux()
		name := fmt.Sprintf("gt-fence-testfam-%d", time.Now().UnixNano()%10000)
		start(t, tm, name, "cat -v", "cat", map[string]string{"GC_PROVIDER": "testfam"})
		if err := fencedTmuxProvider(tm).NudgeFenced(ctx, name, runtime.TextContent("hello"), immediate(f)); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the declared Escape", captureHas(tm, name, containsFn("^[")))
	})
}

// The guard's false branch on real tmux: between an operation's precheck and
// its first guarded command the pane's fence changes, or the agent pane is
// killed while a human's split survives in its window holding the same
// digest (tmux evaluates if-shell in the survivor then), or the whole session
// is killed while the server lives (tmux answers with empty fields then).
// Each misses, typed nothing anywhere, and leaves no buffer.
func TestFencedGuardFalseBranchRealTmux(t *testing.T) {
	p := fenceTestProvider(t, "fg")
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-g"}
	newer := runtime.InputFence{Generation: 2, Epoch: 1, Token: "token-g"}
	verified := func(t *testing.T, name string) (context.Context, string) {
		t.Helper()
		g := newPaneGuard(name, f.Digest(), false)
		ctx := withPaneGuard(context.Background(), g)
		if err := g.verify(ctx, p.tm); err != nil {
			t.Fatalf("verify: %v", err)
		}
		target, err := p.tm.agentPaneTarget(name)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, target
	}

	t.Run("fence changed after verify", func(t *testing.T) {
		const name = "gc-fence-flip"
		file := startRawCat(t, p, name, f, nil)
		ctx, target := verified(t, name)
		if _, err := p.tm.run("set-option", "-p", "-F", "-t", target, inputFenceOption, newer.Digest()+".#{pane_id}"); err != nil {
			t.Fatal(err)
		}
		_, err := p.tm.runCtx(ctx, "send-keys", "-t", target, "-l", "for-old")
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != newer.Digest() || miss.Partial {
			t.Fatalf("guarded paste = %v, want a non-partial miss finding the new fence", err)
		}
		if b := listBuffers(t, p); b != "" {
			t.Fatalf("buffers left: %q", b)
		}
		// An own-fence delivery is the signal that anything earlier landed.
		if err := p.SendKeysFenced(context.Background(), name, newer, "Enter"); err != nil {
			t.Fatal(err)
		}
		if got := waitTyped(t, file, containsFn("\r")); got != "\r" {
			t.Fatalf("pane received %q", got)
		}
	})

	t.Run("session killed, server survives", func(t *testing.T) {
		const name = "gc-fence-vanish"
		// A sibling session keeps the server up: tmux then answers the
		// guard's if-shell and miss report for the gone session with exit 0
		// and empty fields, not an error.
		sibling := startRawCat(t, p, "gc-fence-sibling", f, nil)
		_ = startRawCat(t, p, name, f, nil)
		ctx, target := verified(t, name)
		hiddenCtx, _ := verified(t, name)
		if _, err := p.tm.run("kill-session", "-t", "="+name); err != nil {
			t.Fatal(err)
		}
		var miss *runtime.FenceMismatchError
		if err := paneGuardFrom(hiddenCtx).checkLive(hiddenCtx, p.tm); !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
			t.Fatalf("hidden write check on a killed session = %v, want a non-partial absent miss", err)
		}
		_, err := p.tm.runCtx(ctx, "send-keys", "-t", target, "-l", "for-agent")
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent || miss.Partial {
			t.Fatalf("guarded paste into a killed session = %v, want a non-partial absent miss", err)
		}
		if b := listBuffers(t, p); b != "" {
			t.Fatalf("buffers left: %q", b)
		}
		// The sibling's own-fence delivery is the signal anything earlier landed.
		if err := p.SendKeysFenced(context.Background(), "gc-fence-sibling", f, "Enter"); err != nil {
			t.Fatal(err)
		}
		if got := waitTyped(t, sibling, containsFn("\r")); got != "\r" {
			t.Fatalf("the sibling received %q", got)
		}
	})

	t.Run("agent pane killed, split survives", func(t *testing.T) {
		const name = "gc-fence-split"
		_ = startRawCat(t, p, name, f, nil)
		dir := t.TempDir()
		human := filepath.Join(dir, "human")
		if _, err := p.tm.run("split-window", "-d", "-t", paneTarget(name), rawCatCommand(human)); err != nil {
			t.Fatal(err)
		}
		waitCaptureFile(t, human)
		ctx, target := verified(t, name)
		out, err := p.tm.run("list-panes", "-t", paneTarget(name), "-F", "#{pane_id}")
		if err != nil {
			t.Fatal(err)
		}
		var survivor string
		for _, id := range strings.Fields(out) {
			if !strings.HasSuffix(target, "."+id) {
				survivor = id
			}
		}
		// The survivor even holds the agent's digest as its own fence.
		if _, err := p.tm.run("set-option", "-p", "-F", "-t", survivor, inputFenceOption, f.Digest()+".#{pane_id}"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.tm.run("kill-pane", "-t", target); err != nil {
			t.Fatal(err)
		}
		_, err = p.tm.runCtx(ctx, "send-keys", "-t", target, "-l", "for-agent")
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundAbsent {
			t.Fatalf("guarded paste into a killed pane = %v, want an absent miss", err)
		}
		if b := listBuffers(t, p); b != "" {
			t.Fatalf("buffers left: %q", b)
		}
		// Unfenced keys to the survivor are the signal anything earlier landed.
		if _, err := p.tm.run("send-keys", "-t", survivor, "Enter"); err != nil {
			t.Fatal(err)
		}
		if got := waitTyped(t, human, containsFn("\r")); got != "\r" {
			t.Fatalf("the human's pane received %q", got)
		}
	})
}

// The hidden attach client types into its session's active pane. On real
// tmux, with a human's split focused, fenced input through the client is
// refused as unproven and reaches neither pane, even when the split holds a
// copy of the agent's fence; with focus back on the agent
// pane it reaches only the agent.
func TestFencedHiddenAttachRealTmuxRefusesFocusedSplit(t *testing.T) {
	p := fenceTestProvider(t, "fh")
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-h"}
	const name = "gc-fence-hidden"
	agent := startRawCat(t, p, name, f, nil)
	target, err := p.tm.agentPaneTarget(name)
	if err != nil {
		t.Fatal(err)
	}
	human := filepath.Join(t.TempDir(), "human")
	// No -d: the human's new pane takes focus.
	out, err := p.tm.run("split-window", "-P", "-F", "#{pane_id}", "-t", target, rawCatCommand(human))
	if err != nil {
		t.Fatal(err)
	}
	humanPane := strings.TrimSpace(out)
	waitCaptureFile(t, human)
	// The human's pane even holds the agent's digest as its own fence: only
	// the active-pane check can refuse it.
	if _, err := p.tm.run("set-option", "-p", "-F", "-t", humanPane, inputFenceOption, f.Digest()+".#{pane_id}"); err != nil {
		t.Fatal(err)
	}
	if err := p.tm.ensureHiddenAttachedClient(name); err != nil {
		t.Fatalf("ensureHiddenAttachedClient: %v", err)
	}
	t.Cleanup(func() { p.tm.CloseHiddenAttachClient(name) })

	err = p.SendKeysFenced(context.Background(), name, f, "Enter")
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundUnproven || miss.Partial {
		t.Fatalf("fenced keys with the human's split focused = %v, want a non-partial unproven miss", err)
	}

	if _, err := p.tm.run("select-pane", "-t", target); err != nil {
		t.Fatal(err)
	}
	if err := p.SendKeysFenced(context.Background(), name, f, "Enter"); err != nil {
		t.Fatalf("fenced keys with the agent focused: %v", err)
	}
	// Each pane's first byte is the signal that nothing earlier landed there.
	if got := waitTyped(t, agent, containsFn("\r")); got != "\r" {
		t.Fatalf("the agent's pane received %q", got)
	}
	if _, err := p.tm.run("send-keys", "-t", humanPane, "Enter"); err != nil {
		t.Fatal(err)
	}
	if got := waitTyped(t, human, containsFn("\r")); got != "\r" {
		t.Fatalf("the human's pane received %q", got)
	}
}
