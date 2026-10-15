//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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

// Fenced text arrives byte for byte (E4), a trailing ";" included, which the
// unfenced send-keys -l path drops (E5, a follow-up bug).
func TestFencedNudgeRealTmuxTextIsByteExact(t *testing.T) {
	p := fenceTestProvider(t, "fx")
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "token-x"}
	for _, text := range []string{fenceTextE4, fenceTextE4 + ";", "x;"} {
		name := "gc-fence-exact-" + strconv.Itoa(len(text))
		file := startRawCat(t, p, name, f, nil)
		if err := p.NudgeFenced(context.Background(), name, runtime.TextContent(text), immediate(f)); err != nil {
			t.Fatalf("NudgeFenced(%q) = %v", text, err)
		}
		waitTyped(t, file, func(got string) bool { return strings.HasSuffix(got, text+"\r") })
	}
	// E5: the unfenced path loses a trailing ";" today.
	name := "gc-fence-exact-unfenced"
	file := startRawCat(t, p, name, f, nil)
	if err := p.NudgeNow(name, runtime.TextContent("x;")); err != nil {
		t.Fatal(err)
	}
	if got := waitTyped(t, file, containsFn("\r")); strings.HasSuffix(got, "x;\r") {
		t.Logf("unfenced send-keys -l kept the trailing ';' (%q): E5 is fixed, update this test", got)
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
