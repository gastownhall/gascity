//go:build unix

package subprocess

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Start must honor its context at step boundaries and must not hold the
// provider-wide mutex across slow work (ga-q8m0x6, refs #7170). These tests use
// the providerOps seam only: no step blocks on a real clock except the bounded
// reap in the failure-branch test, which waits out runtime.ManagedProcessReapGrace.

// stateCtx is a context whose Err flips to context.Canceled the first time
// cancelWhen reports true. It models "the caller's deadline expired at exactly
// this point of Start" without a hook inside Start: the state a boundary is
// defined by (the sidecar exists, the socket does not) is the trigger.
type stateCtx struct {
	context.Context
	cancelWhen func() bool
	fired      atomic.Bool
}

func (c *stateCtx) Err() error {
	if c.fired.Load() {
		return context.Canceled
	}
	if c.cancelWhen() {
		c.fired.Store(true)
		return context.Canceled
	}
	return nil
}

// recordingStart wraps the provider's real process start, recording each
// command it launched so a test can prove none was leaked.
type recordingStart struct {
	mu        sync.Mutex
	cmds      []*exec.Cmd
	realStart func(*exec.Cmd) (int, error)
}

func newRecordingStart(p *Provider) *recordingStart {
	r := &recordingStart{realStart: p.ops.start}
	p.ops.start = r.start
	return r
}

func (r *recordingStart) start(cmd *exec.Cmd) (int, error) {
	pgid, err := r.realStart(cmd)
	if err == nil {
		r.mu.Lock()
		r.cmds = append(r.cmds, cmd)
		r.mu.Unlock()
	}
	return pgid, err
}

func (r *recordingStart) started() []*exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*exec.Cmd(nil), r.cmds...)
}

// killAllAtCleanup makes sure a test never leaves a session process behind,
// whatever Start did with it.
func (r *recordingStart) killAllAtCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, cmd := range r.started() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Process.Kill()
			}
		}
	})
}

// requireAllReaped fails when a started process is still running or still an
// unreaped zombie: Start returned an error, so nothing may outlive it.
func (r *recordingStart) requireAllReaped(t *testing.T) {
	t.Helper()
	for _, cmd := range r.started() {
		if cmd.ProcessState == nil {
			t.Errorf("process %d was started by a Start that failed and was not killed and reaped", cmd.Process.Pid)
		}
	}
}

// devNullFDs counts this process's open descriptors that point at /dev/null.
func devNullFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}
	n := 0
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == os.DevNull {
			n++
		}
	}
	return n
}

// requireNothingLeaked asserts a failed Start left no trace of name.
func requireNothingLeaked(t *testing.T, p *Provider, name string, fdsBefore int) {
	t.Helper()
	p.mu.Lock()
	_, tracked := p.procs[name]
	_, hasWorkDir := p.workDirs[name]
	p.mu.Unlock()
	if tracked {
		t.Error("session still tracked after a failed Start")
	}
	if hasWorkDir {
		t.Error("work dir still tracked after a failed Start")
	}
	for key := range identityEnv {
		if got, _ := p.GetMeta(name, key); got != "" {
			t.Errorf("identity sidecar %s after a failed Start = %q, want removed", key, got)
		}
	}
	for _, path := range []string{p.sockPath(name), p.sockNamePath(name)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("socket artifact %s after a failed Start: %v, want not exist", path, err)
		}
	}
	if got := devNullFDs(t); got != fdsBefore {
		t.Errorf("open %s descriptors = %d, want %d: Start leaked one", os.DevNull, got, fdsBefore)
	}
}

func sidecarExists(p *Provider, name string) bool {
	matches, _ := filepath.Glob(filepath.Join(p.dir, metaFilePrefix(name)+".meta.*"))
	return len(matches) > 0
}

// blockingStart is a providerOps.start stub that parks the first call until
// released, then delegates to realStart.
type blockingStart struct {
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	realStart func(*exec.Cmd) (int, error)
	// parkOnce parks only the first call, so a later Start of another name is
	// not held by the same stub.
	parkOnce sync.Once
}

func newBlockingStart(realStart func(*exec.Cmd) (int, error)) *blockingStart {
	return &blockingStart{entered: make(chan struct{}), release: make(chan struct{}), realStart: realStart}
}

func (b *blockingStart) start(cmd *exec.Cmd) (int, error) {
	parked := false
	b.parkOnce.Do(func() { parked = true })
	if parked {
		close(b.entered)
		<-b.release
	}
	return b.realStart(cmd)
}

func (b *blockingStart) unblock() { b.once.Do(func() { close(b.release) }) }

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// withinOrFail runs fn and fails the test if it has not returned within d.
func withinOrFail(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Errorf("%s did not return within %v while another Start was blocked in process creation", what, d)
	}
}

// (a) A Start parked in process creation holds no lock any other call needs.
// Kills: p.mu held across the whole of Start.
func TestStartBlockedInProcessCreationDoesNotBlockOtherCalls(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	blk := newBlockingStart(p.ops.start)
	p.ops.start = blk.start
	p.ops.scan = func(string, []string) ([]runtime.LiveRuntime, error) { return nil, nil }
	t.Cleanup(blk.unblock)

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "slow", runtime.Config{Command: "sleep 300", Env: identityEnv})
	}()
	waitClosed(t, blk.entered, "Start to reach providerOps.start")

	const bound = 3 * time.Second
	withinOrFail(t, bound, "IsRunning", func() { p.IsRunning("other") })
	withinOrFail(t, bound, "ProcessAlive", func() { p.ProcessAlive("other", nil) })
	withinOrFail(t, bound, "ListRunning", func() { _, _ = p.ListRunning("") })
	withinOrFail(t, bound, "Interrupt", func() { _ = p.Interrupt("other") })
	withinOrFail(t, bound, "FindRuntimesBySessionID", func() { _, _ = p.FindRuntimesBySessionID("S") })
	withinOrFail(t, bound, "Stop of another name", func() { _ = p.Stop("other") })
	withinOrFail(t, bound, "CopyTo", func() { _ = p.CopyTo("other", "/nonexistent", "") })

	blk.unblock()
	if err := <-startErr; err != nil {
		t.Fatalf("Start after release: %v", err)
	}
	if err := p.Stop("slow"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// (a) continued: a different name's Start is not queued behind the wedged one.
// Kills: a provider-wide start lock (acceptable only when the PR says so; this
// change takes the per-name reservation the ACP provider uses).
func TestStartOfAnotherNameIsNotQueuedBehindBlockedStart(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	blk := newBlockingStart(p.ops.start)
	p.ops.start = blk.start
	t.Cleanup(blk.unblock)

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "wedged", runtime.Config{Command: "sleep 300"})
	}()
	waitClosed(t, blk.entered, "first Start to reach providerOps.start")

	withinOrFail(t, 5*time.Second, "Start of a different name", func() {
		if err := p.Start(context.Background(), "free", runtime.Config{Command: "sleep 300"}); err != nil {
			t.Errorf("Start(free) = %v, want nil", err)
		}
	})

	// Release first: a Start still queued behind the wedged one must be able
	// to finish before the cleanup Stops below.
	blk.unblock()
	if err := <-startErr; err != nil {
		t.Fatalf("Start(wedged) after release: %v", err)
	}
	_ = p.Stop("wedged")
	_ = p.Stop("free")
}

// (b) A cancel before staging stops Start before it touches anything.
// Kills: a Start that ignores ctx and runs to completion.
func TestStartCanceledBeforeStagingLeaksNothing(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	fds := devNullFDs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Start(ctx, "pre", runtime.Config{Command: "sleep 300", WorkDir: t.TempDir(), Env: identityEnv})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start(canceled ctx) = %v, want an error wrapping context.Canceled", err)
	}
	if n := len(rec.started()); n != 0 {
		t.Errorf("Start created %d process(es) under an already-canceled ctx", n)
	}
	requireNothingLeaked(t, p, "pre", fds)
}

// (b) A cancel after the /dev/null open and before process creation closes the
// descriptor. The trigger is the descriptor itself appearing.
// Kills: no ctx check before process creation, or one that returns without
// closing nullFile.
func TestStartCanceledBeforeProcessCreationClosesDevNull(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	fds := devNullFDs(t)
	ctx := &stateCtx{Context: context.Background(), cancelWhen: func() bool { return devNullFDs(t) > fds }}

	err := p.Start(ctx, "nofd", runtime.Config{Command: "sleep 300", WorkDir: t.TempDir(), Env: identityEnv})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want an error wrapping context.Canceled", err)
	}
	if n := len(rec.started()); n != 0 {
		t.Errorf("Start created %d process(es) after the ctx was canceled before creation", n)
	}
	requireNothingLeaked(t, p, "nofd", fds)
}

// (b) A cancel between process creation and the metadata write kills and reaps
// the process. The cancel is real: the start stub cancels the ctx after the
// process exists.
// Kills: a Start that commits a session whose ctx was already done.
func TestStartCanceledAfterProcessCreationKillsProcess(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	fds := devNullFDs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realStart := p.ops.start
	p.ops.start = func(cmd *exec.Cmd) (int, error) {
		pgid, err := realStart(cmd)
		cancel()
		return pgid, err
	}

	err := p.Start(ctx, "created", runtime.Config{Command: "sleep 300", WorkDir: t.TempDir(), Env: identityEnv})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want an error wrapping context.Canceled", err)
	}
	if n := len(rec.started()); n != 1 {
		t.Fatalf("expected exactly one process to have been created, got %d", n)
	}
	rec.requireAllReaped(t)
	requireNothingLeaked(t, p, "created", fds)
}

// (b) A cancel after the identity sidecar is written and before the control
// socket is set up must clear the sidecar and reap the process. The trigger is
// the sidecar file existing with no socket yet.
// Kills: a cancellation path that forgets clearSessionMeta.
func TestStartCanceledBetweenMetadataAndSocketClearsSidecar(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	fds := devNullFDs(t)
	var listenCalls atomic.Int32
	realListen := p.ops.listen
	p.ops.listen = func(network, addr string) (net.Listener, error) {
		listenCalls.Add(1)
		return realListen(network, addr)
	}
	ctx := &stateCtx{Context: context.Background(), cancelWhen: func() bool {
		_, nameErr := os.Lstat(p.sockNamePath("seeded"))
		return sidecarExists(p, "seeded") && os.IsNotExist(nameErr)
	}}

	err := p.Start(ctx, "seeded", runtime.Config{Command: "sleep 300", WorkDir: t.TempDir(), Env: identityEnv})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want an error wrapping context.Canceled", err)
	}
	if n := listenCalls.Load(); n != 0 {
		t.Errorf("control socket was set up %d time(s) after the ctx was canceled", n)
	}
	rec.requireAllReaped(t)
	requireNothingLeaked(t, p, "seeded", fds)
}

// (b) A cancel after the control socket exists and before the commit tears the
// socket down and reaps the process; a Start that reported failure is never
// committed. The cancel is real: the listen stub cancels after binding.
// Kills: a commit that does not re-check ctx.
func TestStartCanceledBetweenSocketAndCommitLeavesNoSession(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	fds := devNullFDs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis := newFakeListener()
	p.ops.listen = func(_, _ string) (net.Listener, error) {
		cancel()
		return lis, nil
	}

	err := p.Start(ctx, "bound", runtime.Config{Command: "sleep 300", WorkDir: t.TempDir(), Env: identityEnv})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want an error wrapping context.Canceled", err)
	}
	select {
	case <-lis.closed:
	default:
		t.Error("control-socket listener was left open after the canceled Start")
	}
	rec.requireAllReaped(t)
	requireNothingLeaked(t, p, "bound", fds)
	if p.IsRunning("bound") {
		t.Error("IsRunning reports a session whose Start failed")
	}
}

// (c) Concurrent Starts of one name: exactly one wins, the rest are told the
// session exists, and no loser leaves a process behind. Run under -race.
func TestConcurrentStartsOfOneNameHaveOneWinner(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	realStart := p.ops.start
	p.ops.start = func(cmd *exec.Cmd) (int, error) {
		time.Sleep(20 * time.Millisecond) // widen the window in which starts overlap
		return realStart(cmd)
	}

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			errs[i] = p.Start(context.Background(), "contended", runtime.Config{Command: "sleep 300", Env: identityEnv})
		}(i)
	}
	close(gate)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case !errors.Is(err, runtime.ErrSessionExists):
			t.Errorf("Start #%d = %v, want nil or runtime.ErrSessionExists", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d concurrent Starts of one name succeeded, want exactly 1", winners)
	}
	if !p.IsRunning("contended") {
		t.Error("the winning Start's session is not running")
	}
	live := 0
	for _, cmd := range rec.started() {
		if cmd.ProcessState == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d started process(es) are unreaped, want 1 (the winner): a losing Start leaked its process", live)
	}
	if err := p.Stop("contended"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// (d) The failure-branch Wait after Kill is bounded. The start stub launches a
// real process but reports a process group that does not exist, so the Kill
// reaches nothing and the process is never reaped. Start must give up after the
// reap grace and say the process may still be alive.
// Kills: an unbounded cmd.Wait() in the control-socket failure branch.
func TestStartFailureBranchWaitIsBounded(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	realStart := p.ops.start
	p.ops.start = func(cmd *exec.Cmd) (int, error) {
		if _, err := realStart(cmd); err != nil {
			return 0, err
		}
		return 1 << 30, nil // a group nobody is in: the kill is a no-op
	}
	p.ops.listen = func(_, _ string) (net.Listener, error) {
		return nil, errors.New("session socket not found")
	}

	err := runGuarded(t, "Start", func() error {
		return p.Start(context.Background(), "stuck", runtime.Config{Command: "sleep 300", Env: identityEnv})
	})
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("Start = %v, want errors.Is runtime.ErrProcessNotConfirmedDead", err)
	}
	if runtime.IsSessionGone(err) {
		t.Errorf("IsSessionGone(%v) = true: a possibly-live process must not read as gone, even with %q in the cause", err, "not found")
	}
	if !strings.Contains(err.Error(), "session socket not found") {
		t.Errorf("Start error %q dropped the original cause", err)
	}
	p.mu.Lock()
	_, tracked := p.procs["stuck"]
	p.mu.Unlock()
	if tracked {
		t.Error("session tracked after a failed Start")
	}
}

// (d) The same bound on the metadata-write failure branch is reached through a
// canceled ctx whose process cannot be reaped: the cancel path must also
// return ErrProcessNotConfirmedDead joined with the ctx error.
// Kills: a cancellation path that waits forever for a process that outlives
// its kill.
func TestStartCanceledWithUnreapableProcessReportsNotConfirmedDead(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realStart := p.ops.start
	p.ops.start = func(cmd *exec.Cmd) (int, error) {
		if _, err := realStart(cmd); err != nil {
			return 0, err
		}
		cancel()
		return 1 << 30, nil
	}

	err := runGuarded(t, "Start", func() error {
		return p.Start(ctx, "unreapable", runtime.Config{Command: "sleep 300", Env: identityEnv})
	})
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want both runtime.ErrProcessNotConfirmedDead and context.Canceled", err)
	}
}

// (e) Stop of a name whose Start is still in flight waits for that Start and
// then stops what it started; it never returns nil while leaving the new
// process behind. Stop of any other name is not held (checked in (a)).
// Kills: a Stop that finds no tracked session mid-Start, reports success, and
// leaves the committed process running.
func TestStopWaitsForInFlightStartOfSameName(t *testing.T) {
	p := newTestProvider(t)
	rec := newRecordingStart(p)
	rec.killAllAtCleanup(t)
	blk := newBlockingStart(p.ops.start)
	p.ops.start = blk.start
	t.Cleanup(blk.unblock)

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "racing", runtime.Config{Command: "sleep 300", Env: identityEnv})
	}()
	waitClosed(t, blk.entered, "Start to reach providerOps.start")

	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop("racing") }()
	select {
	case err := <-stopErr:
		t.Fatalf("Stop returned %v while the Start of the same name was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}

	blk.unblock()
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not return after the Start it waited for finished")
	}
	if p.IsRunning("racing") {
		t.Error("session still running after Stop returned: Stop left the process of an in-flight Start behind")
	}
	for _, cmd := range rec.started() {
		if err := syscall.Kill(cmd.Process.Pid, 0); err == nil {
			// a zombie also answers 0; ProcessState set means reaped
			if cmd.ProcessState == nil {
				t.Errorf("process %d still running after Stop returned", cmd.Process.Pid)
			}
		}
	}
}

// (e) When the Start a Stop waited for fails, Stop reports success: there is
// nothing left to stop, and the failed Start left nothing behind.
func TestStopAfterInFlightStartFailsIsNoOp(t *testing.T) {
	p := newTestProvider(t)
	fds := devNullFDs(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	p.ops.start = func(*exec.Cmd) (int, error) {
		close(entered)
		<-release
		return 0, errors.New("fork refused")
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), "doomed", runtime.Config{Command: "sleep 300", Env: identityEnv})
	}()
	waitClosed(t, entered, "Start to reach providerOps.start")

	stopErr := make(chan error, 1)
	go func() { stopErr <- p.Stop("doomed") }()
	releaseOnce.Do(func() { close(release) })

	if err := <-startErr; err == nil || !strings.Contains(err.Error(), "fork refused") {
		t.Fatalf("Start = %v, want the fork failure", err)
	}
	select {
	case err := <-stopErr:
		if err != nil {
			t.Errorf("Stop of a name whose Start failed = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the Start it waited for failed")
	}
	requireNothingLeaked(t, p, "doomed", fds)
}
