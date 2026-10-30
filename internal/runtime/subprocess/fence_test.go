package subprocess

import (
	"context"
	"errors"
	"net"
	"os/exec"
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
	if err := p.Start(context.Background(), name, runtime.Config{Command: "exec sleep 600", Env: env}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	f, _ := runtime.InputFenceFromEnv(env)
	return f
}

// Kills a subprocess fence that is not checked against the live process:
// input under the started incarnation's fence goes to its process, and
// every op under another fence, to an unstarted session, to one started with
// no incarnation, or to one another provider owns is a miss that sends
// nothing.
func TestSubprocessFencedInputChecksTheLiveProcess(t *testing.T) {
	p := newTestProvider(t)
	if got := runtime.InputFencingFor(NewSeamBackedWithDir(p.dir), "s"); got != runtime.InputFencingChecked {
		t.Fatalf("seam-backed fencing = %q, want checked", got)
	}
	ctx := context.Background()
	fence := startFenced(t, p, "sp-fenced", fenceEnv("2", "tok"))
	if err := p.NudgeFenced(ctx, "sp-fenced", runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}); err != nil {
		t.Fatalf("NudgeFenced under the session's fence: %v", err)
	}
	if err := p.SendKeysFenced(ctx, "sp-fenced", fence, "Enter"); err != nil {
		t.Fatalf("SendKeysFenced under the session's fence: %v", err)
	}
	startFenced(t, p, "sp-bare", nil)
	other := runtime.InputFence{Generation: 2, Epoch: 1, Token: "other"}
	stranger := NewProviderWithDir(p.dir)
	for label, tc := range map[string]struct {
		p     *Provider
		name  string
		fence runtime.InputFence
		found string
	}{
		"other fence":   {p, "sp-fenced", other, fence.Digest()},
		"no identity":   {p, "sp-bare", fence, runtime.FenceFoundUnproven},
		"never started": {p, "sp-gone", fence, runtime.FenceFoundAbsent},
		"another owner": {stranger, "sp-fenced", fence, runtime.FenceFoundUnproven},
	} {
		for op, err := range map[string]error{
			"nudge":     tc.p.NudgeFenced(ctx, tc.name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: tc.fence}),
			"keys":      tc.p.SendKeysFenced(ctx, tc.name, tc.fence, "Enter"),
			"interrupt": tc.p.InterruptFenced(ctx, tc.name, tc.fence),
		} {
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != tc.found || miss.Partial || runtime.IsSessionGone(err) {
				t.Errorf("%s %s: %v, want a miss finding %s", label, op, err, tc.found)
			}
		}
	}
	if !p.IsRunning("sp-fenced") {
		t.Fatal("a refused interrupt reached the process")
	}
	if err := p.InterruptFenced(ctx, "sp-fenced", fence); err != nil {
		t.Fatalf("InterruptFenced under the session's fence: %v", err)
	}
	waitNotRunning(t, p, "sp-fenced")
}

// Kills a fenced interrupt that signals whatever process holds the name: a
// session respawned as another incarnation between the fence check and the
// signal is a miss finding the checked one dead, and the new process keeps
// running.
func TestSubprocessRespawnBetweenCheckAndSendIsRefused(t *testing.T) {
	p := newTestProvider(t)
	first := startFenced(t, p, "sp-respawn", fenceEnv("1", "first"))
	p.fenceChecked = func(string) {
		p.fenceChecked = nil
		if err := p.Stop("sp-respawn"); err != nil {
			t.Errorf("Stop: %v", err)
		}
		startFenced(t, p, "sp-respawn", fenceEnv("2", "second"))
	}
	err := p.InterruptFenced(context.Background(), "sp-respawn", first)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || miss.Partial {
		t.Fatalf("InterruptFenced across a respawn = %v, want a miss finding the checked incarnation dead", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !p.IsRunning("sp-respawn") {
		t.Fatal("the respawned incarnation received the stale interrupt")
	}
}

func waitNotRunning(t *testing.T, p *Provider, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for p.IsRunning(name) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still running after a fenced interrupt", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fencedTestConn installs a hand-built child for name holding fence f: a
// process that has already exited and been waited for, whose exit the
// provider has (doneClosed) or has not yet observed.
func fencedTestConn(t *testing.T, p *Provider, name string, f runtime.InputFence, doneClosed bool) {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a short-lived child: %v", err)
	}
	sc := &sessionConn{cmd: cmd, done: make(chan struct{}), reaped: make(chan struct{}), fence: f.Digest()}
	if doneClosed {
		close(sc.done)
	}
	p.mu.Lock()
	p.procs[name] = sc
	p.mu.Unlock()
}

// Kills a fenced op that acts on a child that has exited: one whose exit the
// provider observed is a whole miss finding it dead for every op, and an
// interrupt whose signal finds the child already gone (the exit not yet
// observed) is the same miss, never an untyped signal error.
func TestSubprocessFencedInputToAnExitedChildIsADeadMiss(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	ctx := context.Background()
	wantDead := func(t *testing.T, label string, err error) {
		t.Helper()
		var miss *runtime.FenceMismatchError
		if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || miss.Partial {
			t.Fatalf("%s = %v, want a whole miss finding the incarnation dead", label, err)
		}
	}

	p := newTestProvider(t)
	fencedTestConn(t, p, "sp-exited", fence, true)
	wantDead(t, "nudge", p.NudgeFenced(ctx, "sp-exited", runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}))
	wantDead(t, "keys", p.SendKeysFenced(ctx, "sp-exited", fence, "Enter"))
	wantDead(t, "interrupt", p.InterruptFenced(ctx, "sp-exited", fence))

	fencedTestConn(t, p, "sp-unreaped", fence, false)
	wantDead(t, "interrupt of an unobserved exit", p.InterruptFenced(ctx, "sp-unreaped", fence))
}

// Kills a fenced op that reads a control socket it cannot probe as absent: a
// session this provider does not own whose socket dial fails with EACCES is
// ErrRuntimeUnavailable, never a miss.
func TestSubprocessFencedInputWithAnUnprobeableSocketIsUnavailable(t *testing.T) {
	p := newTestProvider(t)
	p.ops.dial = func(string, string, time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}
	}
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	ctx := context.Background()
	for op, err := range map[string]error{
		"nudge":     p.NudgeFenced(ctx, "sp-eacces", runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}),
		"keys":      p.SendKeysFenced(ctx, "sp-eacces", fence, "Enter"),
		"interrupt": p.InterruptFenced(ctx, "sp-eacces", fence),
	} {
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrFenceMismatch) {
			t.Errorf("%s = %v, want ErrRuntimeUnavailable", op, err)
		}
	}
}
