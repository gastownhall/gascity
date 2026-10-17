package acp

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func fenceEnv(gen, token string) map[string]string {
	return map[string]string{runtime.RuntimeEpochEnv: gen, runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: token}
}

func startFenced(t *testing.T, p *Provider, name string, env map[string]string) runtime.InputFence {
	t.Helper()
	if err := p.Start(context.Background(), name, runtime.Config{Command: fakeACPShellCommand(), WorkDir: t.TempDir(), Env: env}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	f, _ := runtime.InputFenceFromEnv(env)
	return f
}

func peekHas(p *Provider, name, want string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := p.Peek(name, 0); strings.Contains(out, want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Kills an ACP fence that is not checked against the live process: input
// under the started incarnation's fence is delivered on its connection, and
// every op under another fence, to an unstarted session, or to a session
// whose start named no incarnation is a miss that sends nothing.
func TestACPFencedInputChecksTheLiveProcess(t *testing.T) {
	p := newTestProvider(t)
	if got := runtime.InputFencingFor(NewSeamBackedWithDir(p.dir, Config{}), "s"); got != runtime.InputFencingChecked {
		t.Fatalf("seam-backed fencing = %q, want checked", got)
	}
	ctx := context.Background()
	name := testName()
	fence := startFenced(t, p, name, fenceEnv("2", "tok"))
	if err := p.NudgeFenced(ctx, name, runtime.TextContent("fenced hello"), runtime.FencedNudge{Fence: fence}); err != nil {
		t.Fatalf("NudgeFenced under the session's fence: %v", err)
	}
	if !peekHas(p, name, "echo: fenced hello") {
		t.Fatal("the fenced nudge never reached the agent")
	}
	if err := p.SendKeysFenced(ctx, name, fence, "Enter"); err != nil {
		t.Fatalf("SendKeysFenced: %v", err)
	}
	if err := p.InterruptFenced(ctx, name, fence); err != nil {
		t.Fatalf("InterruptFenced: %v", err)
	}

	bare := testName()
	startFenced(t, p, bare, nil)
	other := runtime.InputFence{Generation: 2, Epoch: 1, Token: "other"}
	for target, want := range map[string]struct {
		fence runtime.InputFence
		found string
	}{
		name:           {other, fence.Digest()},
		bare:           {fence, runtime.FenceFoundUnproven},
		name + "-gone": {fence, runtime.FenceFoundAbsent},
	} {
		for op, err := range map[string]error{
			"nudge":     p.NudgeFenced(ctx, target, runtime.TextContent("stale"), runtime.FencedNudge{Fence: want.fence}),
			"keys":      p.SendKeysFenced(ctx, target, want.fence, "Enter"),
			"interrupt": p.InterruptFenced(ctx, target, want.fence),
		} {
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != want.found || miss.Partial || runtime.IsSessionGone(err) {
				t.Errorf("%s %s: %v, want a miss finding %s", target, op, err, want.found)
			}
		}
	}
	if err := p.NudgeFenced(ctx, name, runtime.TextContent("x"), runtime.FencedNudge{Fence: runtime.InputFence{Generation: 1, Epoch: 1}}); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("NudgeFenced(invalid) = %v, want ErrInvalidInputFence", err)
	}
	time.Sleep(200 * time.Millisecond)
	if out, _ := p.Peek(name, 0); strings.Contains(out, "stale") {
		t.Fatalf("a refused nudge reached the agent: %q", out)
	}
}

// Kills a fenced nudge or interrupt that reports success when the session
// was respawned as another incarnation between the fence check and the
// send: the send stays on the checked process's connection, which is gone,
// so it is a miss finding that incarnation dead, and the new incarnation
// receives nothing.
func TestACPRespawnBetweenCheckAndSendIsRefused(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()
	for op, call := range map[string]func(string, runtime.InputFence) error{
		"nudge": func(name string, f runtime.InputFence) error {
			return p.NudgeFenced(ctx, name, runtime.TextContent("stale"), runtime.FencedNudge{Fence: f})
		},
		"interrupt": func(name string, f runtime.InputFence) error { return p.InterruptFenced(ctx, name, f) },
	} {
		name := testName()
		first := startFenced(t, p, name, fenceEnv("1", "first"))
		p.fenceChecked = func(string) {
			p.fenceChecked = nil
			if err := p.Stop(name); err != nil {
				t.Errorf("Stop: %v", err)
			}
			startFenced(t, p, name, fenceEnv("2", "second"))
		}
		err := call(name, first)
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || miss.Partial {
			t.Fatalf("%s across a respawn = %v, want a whole miss finding the checked incarnation dead (nothing was sent)", op, err)
		}
		time.Sleep(200 * time.Millisecond)
		if out, _ := p.Peek(name, 0); strings.Contains(out, "stale") {
			t.Fatalf("%s: the respawned incarnation received the stale nudge: %q", op, out)
		}
		second, _ := runtime.InputFenceFromEnv(fenceEnv("2", "second"))
		if err := p.NudgeFenced(ctx, name, runtime.TextContent("fresh"), runtime.FencedNudge{Fence: second}); err != nil || !peekHas(p, name, "echo: fresh") {
			t.Fatalf("%s: NudgeFenced under the new fence = %v, want delivered", op, err)
		}
	}
}

// Kills a fenced op that sends to a session another process owns, which
// this provider cannot prove: its socket is live, so the miss is unproven,
// never absent and never a delivery.
func TestACPFencedInputToAnotherOwnerIsUnproven(t *testing.T) {
	owner := newTestProvider(t)
	name := testName()
	fence := startFenced(t, owner, name, fenceEnv("1", "tok"))
	other := NewProviderWithDir(owner.dir, Config{})
	err := other.InterruptFenced(context.Background(), name, fence)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundUnproven {
		t.Fatalf("InterruptFenced from another provider = %v, want an unproven miss", err)
	}
}

// Kills a fenced interrupt that reports a miss for an interrupt that reached
// the checked agent: session/cancel was written to it, and the agent then
// exited before the settle wait ended. The interrupt landed, so the call
// succeeds as the unfenced one does.
func TestACPFencedInterruptThatReachedTheAgentSucceeds(t *testing.T) {
	env := fenceEnv("1", "tok")
	env["GC_ON_CANCEL"] = "ignore"
	f := startInterruptFake(t, 50*time.Millisecond, env)
	fence, _ := runtime.InputFenceFromEnv(env)
	f.hold(t)
	sc := f.conn(t)
	sc.beforeTurnSignal = func() {
		waitUntil(t, "the fake to log session/cancel", func() bool { return f.countMethod(t, "session/cancel") == 1 })
		_ = sc.cmd.Process.Kill()
		waitExited(t, sc)
	}
	if err := f.p.InterruptFenced(context.Background(), f.name, fence); err != nil {
		t.Fatalf("InterruptFenced = %v, want nil: session/cancel reached the checked agent", err)
	}
}

// fencedTestConn installs a hand-built connection for name holding fence f,
// with stdin as its agent's input.
func fencedTestConn(p *Provider, name string, f runtime.InputFence, stdin *closedPipeStdin) *sessionConn {
	sc := newSessionConn(nil, stdin, nil, 100, nil)
	sc.sessionID = "session-1"
	sc.fence = f.Digest()
	p.mu.Lock()
	p.conns[name] = sc
	p.mu.Unlock()
	return sc
}

// Kills a fenced op that sends into an agent that has exited, or a fenced
// nudge that reports success when the agent exits before the prompt is
// written (Nudge's best-effort nil): each is a whole miss finding the
// incarnation dead.
func TestACPFencedInputToAnExitedAgentIsADeadMiss(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	wantDead := func(t *testing.T, label string, err error) {
		t.Helper()
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || miss.Partial {
			t.Fatalf("%s = %v, want a whole miss finding the incarnation dead", label, err)
		}
	}

	t.Run("exited before the check", func(t *testing.T) {
		p := NewProviderWithDir(shortTempDir(t), Config{})
		name := testName()
		sc := fencedTestConn(p, name, fence, &closedPipeStdin{writeCalled: make(chan struct{})})
		close(sc.done)
		ctx := context.Background()
		wantDead(t, "nudge", p.NudgeFenced(ctx, name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}))
		wantDead(t, "keys", p.SendKeysFenced(ctx, name, fence, "Enter"))
		wantDead(t, "interrupt", p.InterruptFenced(ctx, name, fence))
	})

	t.Run("exited while queued for the prompt", func(t *testing.T) {
		p := NewProviderWithDir(shortTempDir(t), Config{})
		name := testName()
		sc := fencedTestConn(p, name, fence, &closedPipeStdin{writeCalled: make(chan struct{})})
		p.fenceChecked = func(string) {
			sc.nudgeMu.Lock()
			go func() {
				time.Sleep(50 * time.Millisecond)
				close(sc.done)
				sc.nudgeMu.Unlock()
			}()
		}
		wantDead(t, "nudge", p.NudgeFenced(context.Background(), name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}))
	})

	t.Run("output drained", func(t *testing.T) {
		p := NewProviderWithDir(shortTempDir(t), Config{StopGrace: 2 * time.Second})
		name := testName()
		sc := fencedTestConn(p, name, fence, &closedPipeStdin{writeCalled: make(chan struct{})})
		p.fenceChecked = func(string) {
			sc.mu.Lock()
			sc.drained = true
			sc.mu.Unlock()
			go func() {
				time.Sleep(50 * time.Millisecond)
				close(sc.done)
			}()
		}
		wantDead(t, "nudge", p.NudgeFenced(context.Background(), name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}))
	})

	t.Run("exited during the write", func(t *testing.T) {
		p := NewProviderWithDir(shortTempDir(t), Config{NudgeBusyTimeout: 2 * time.Second})
		name := testName()
		stdin := &closedPipeStdin{writeCalled: make(chan struct{})}
		sc := fencedTestConn(p, name, fence, stdin)
		go func() {
			<-stdin.writeCalled
			close(sc.done)
		}()
		wantDead(t, "nudge", p.NudgeFenced(context.Background(), name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}))
	})
}

// Kills a fenced op that reads a control socket it cannot probe as absent:
// a session this provider does not own whose socket dial fails with EACCES
// cannot be told apart from a live one, so the op is ErrRuntimeUnavailable,
// never a miss.
func TestACPFencedInputWithAnUnprobeableSocketIsUnavailable(t *testing.T) {
	p := NewProviderWithDir(shortTempDir(t), Config{})
	p.dial = func(string, string, time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}
	}
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	ctx := context.Background()
	name := testName()
	for op, err := range map[string]error{
		"nudge":     p.NudgeFenced(ctx, name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}),
		"keys":      p.SendKeysFenced(ctx, name, fence, "Enter"),
		"interrupt": p.InterruptFenced(ctx, name, fence),
	} {
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrFenceMismatch) {
			t.Errorf("%s = %v, want ErrRuntimeUnavailable", op, err)
		}
	}
}
