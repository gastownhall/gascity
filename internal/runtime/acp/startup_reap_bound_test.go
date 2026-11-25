//go:build unix

// A process that outlives its own SIGKILL (D-state under I/O) must not wedge
// Start, Stop or a retry forever, and giving up must not release the name: a
// canceled startup owns its reservation until its process and writes drain.
// A real subprocess cannot be made unkillable, so these tests hold the wait
// open through the Provider's unexported reap seams (reapGate and the bounds),
// the way handshakeFunc holds the handshake open.
package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// reapWait is a hang detector for something that must happen within the injected
// (tens of milliseconds) bounds; it only ever expires on a failure, and the
// deadline rule in TESTING.md puts its floor at testutil.ExecRaceTimeout. The
// injected bounds themselves are the subject under test.
const reapWait = testutil.ExecRaceTimeout

var errReapCause = errors.New("injected startup failure")

// reapHold stands in for a process that has been SIGKILLed but is not yet
// confirmed dead: a gated wait channel closes only after the real process-exit
// channel closed AND the test released the hold.
type reapHold struct {
	release chan struct{}
	once    sync.Once
}

func newReapHold(t *testing.T) *reapHold {
	t.Helper()
	h := &reapHold{release: make(chan struct{})}
	t.Cleanup(h.let)
	return h
}

func (h *reapHold) let() { h.once.Do(func() { close(h.release) }) }

func (h *reapHold) gate(done <-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		<-done
		<-h.release
		close(out)
	}()
	return out
}

// newReapBoundProvider returns a provider whose post-SIGKILL waits never
// complete until hold is released, and whose bounds are short.
func newReapBoundProvider(t *testing.T, hold *reapHold) *Provider {
	t.Helper()
	p := newTestProvider(t)
	p.reapGate = hold.gate
	p.reapBound = 50 * time.Millisecond
	p.startupStopBound = 200 * time.Millisecond
	p.stopLockBound = 200 * time.Millisecond
	p.stopLockPoll = 5 * time.Millisecond
	return p
}

func awaitReap(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(reapWait)
	defer deadline.Stop()
	for !cond() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func lifecycleLockFree(p *Provider, name string) bool {
	f, err := p.lockLifecycle(name, true)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// cancelStartup does what Stop's startup-cancel path does to the reservation:
// it cancels the handshake context the sentinel carries.
func cancelStartup(p *Provider, name string) {
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	sc.cancel()
}

func reapSentinel(p *Provider, name string) *sessionConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[name]
}

// startReapCase is one of the six post-SIGKILL waits in Start.
type startReapCase struct {
	name  string
	cause string
	// arm makes Start fail at this wait; it returns a cleanup that removes any
	// on-disk obstruction before a retry.
	arm func(t *testing.T, p *Provider, name string) func()
}

func startReapCases() []startReapCase {
	return []startReapCase{
		{"control socket creation failed", "creating control socket", func(t *testing.T, p *Provider, name string) func() {
			dir := p.sockNamePath(name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "obstruction"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return func() { _ = os.RemoveAll(dir) }
		}},
		{"handshake failed", "command not found", func(_ *testing.T, p *Provider, _ string) func() {
			p.handshakeFunc = func(context.Context, *sessionConn, string, []runtime.MCPServerConfig) error {
				return fmt.Errorf("sh: agent: command not found: %w", errReapCause)
			}
			return func() {}
		}},
		{"stopped during startup after handshake", "stopped during startup", func(_ *testing.T, p *Provider, name string) func() {
			p.handshakeFunc = func(context.Context, *sessionConn, string, []runtime.MCPServerConfig) error { return nil }
			p.afterHandshake = func(string) { cancelStartup(p, name) }
			return func() {}
		}},
		{"initial activity publish failed", "publishing initial activity", func(_ *testing.T, p *Provider, _ string) func() {
			p.handshakeFunc = func(context.Context, *sessionConn, string, []runtime.MCPServerConfig) error { return nil }
			p.activityWrite = func(string, []byte) error { return fmt.Errorf("agent: not found: %w", errReapCause) }
			return func() {}
		}},
		{"activity publisher install failed", "starting activity publication", func(_ *testing.T, p *Provider, _ string) func() {
			p.handshakeFunc = func(_ context.Context, sc *sessionConn, _ string, _ []runtime.MCPServerConfig) error {
				sc.mu.Lock()
				sc.activityPublisherClosed = true
				sc.mu.Unlock()
				return nil
			}
			return func() {}
		}},
		{"stopped during startup after activity publish", "stopped during startup", func(_ *testing.T, p *Provider, name string) func() {
			p.handshakeFunc = func(context.Context, *sessionConn, string, []runtime.MCPServerConfig) error { return nil }
			var once sync.Once
			p.activityWrite = func(path string, data []byte) error {
				once.Do(func() { cancelStartup(p, name) })
				return runtime.WritePrivateFile(path, data)
			}
			return func() {}
		}},
	}
}

func reapStartConfig() runtime.Config {
	return runtime.Config{Command: "exec sleep 300", Env: map[string]string{"GC_SESSION_ID": "old"}}
}

// expireStart runs Start into the case's wait with the process held
// "not yet dead", and returns once Start gave up waiting.
func expireStart(t *testing.T, tc startReapCase) (p *Provider, name string, hold *reapHold, sentinel *sessionConn, undo func(), err error) {
	t.Helper()
	hold = newReapHold(t)
	p = newReapBoundProvider(t, hold)
	name = testName()
	undo = tc.arm(t, p, name)
	result := make(chan error, 1)
	begin := time.Now()
	go func() { result <- p.Start(testingContext(t), name, reapStartConfig()) }()
	select {
	case err = <-result:
	case <-time.After(reapWait):
		t.Fatalf("Start still waiting for a process that never confirms dead (%s)", tc.name)
	}
	if elapsed := time.Since(begin); elapsed > reapWait/2 {
		t.Fatalf("Start took %v to give up; the injected bound is %v", elapsed, p.reapBound)
	}
	sentinel = reapSentinel(p, name)
	return p, name, hold, sentinel, undo, err
}

// (a) Each of the six waits is bounded; on expiry the name stays reserved.
func TestStartReapWaitExpiryKeepsReservation(t *testing.T) {
	for _, tc := range startReapCases() {
		t.Run(tc.name, func(t *testing.T) {
			p, name, _, sentinel, _, err := expireStart(t, tc)
			if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
				t.Fatalf("Start = %v, want ErrProcessNotConfirmedDead", err)
			}
			if runtime.IsSessionGone(err) {
				t.Errorf("an unconfirmed kill reads as gone: %v", err)
			}
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("error %q lost its original cause %q", err, tc.cause)
			}
			if strings.Contains(tc.name, "handshake failed") || strings.Contains(tc.name, "publish failed") {
				if !errors.Is(err, errReapCause) {
					t.Errorf("error %v does not wrap the original cause", err)
				}
			}
			if sentinel == nil {
				t.Fatal("reservation released while the process may still be alive")
			}
			if !p.IsRunning(name) {
				t.Error("IsRunning false while the reservation drains")
			}
			if err2 := p.Start(testingContext(t), name, reapStartConfig()); !errors.Is(err2, runtime.ErrSessionExists) {
				t.Errorf("second Start = %v, want ErrSessionExists", err2)
			}
			if lifecycleLockFree(p, name) {
				t.Error("lifecycle flock released while the process may still be alive")
			}
			select {
			case <-sentinel.done:
				t.Error("startup completion signaled before the process was confirmed dead")
			default:
			}
			if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "old" {
				t.Errorf("metadata cleaned (%q) before the process was confirmed dead", got)
			}
		})
	}
}

// (b) Releasing the hold lets the single drain finish and free the name for a
// retry, whose state the first attempt's cleanup must not touch.
func TestStartReapDrainReleasesAfterProcessConfirmedDead(t *testing.T) {
	for _, tc := range startReapCases() {
		t.Run(tc.name, func(t *testing.T) {
			p, name, hold, sentinel, undo, err := expireStart(t, tc)
			if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
				t.Fatalf("Start = %v, want ErrProcessNotConfirmedDead", err)
			}
			hold.let()
			select {
			case <-sentinel.done:
			case <-time.After(reapWait):
				t.Fatal("drain never released the reservation")
			}
			if !lifecycleLockFree(p, name) {
				t.Error("startup completion signaled while the flock was still held")
			}
			if reapSentinel(p, name) != nil {
				t.Error("sentinel survived the drain")
			}
			if p.IsRunning(name) {
				t.Error("IsRunning true after the drain")
			}
			if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "" {
				t.Errorf("drain left metadata %q behind", got)
			}
			undo()

			// A retry through another provider shares only the directory, so it
			// is exactly what a late cleanup from the first attempt would hit.
			retry := NewProviderWithDir(p.dir, Config{HandshakeTimeout: 5 * time.Second})
			cfg := runtime.Config{Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "new"}}
			if err := retry.Start(testingContext(t), name, cfg); err != nil {
				t.Fatalf("retry after the drain: %v", err)
			}
			t.Cleanup(func() { _ = retry.Stop(name) })
			if got, err := retry.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "new" {
				t.Errorf("retry metadata = %q, %v; the first attempt's cleanup touched it", got, err)
			}
			if !retry.IsRunning(name) {
				t.Error("retry socket lost")
			}
		})
	}
}

// (c) Stop on a name that is still starting is bounded too, keeps the
// reservation and the metadata, and is retryable.
func TestStopDuringStartupIsBounded(t *testing.T) {
	hold := newReapHold(t)
	p := newReapBoundProvider(t, hold)
	p.reapBound = time.Minute // Start's own reap wait outlives Stop's bound
	p.startupStopBound = 50 * time.Millisecond
	name := testName()
	entered := make(chan struct{})
	p.handshakeFunc = func(ctx context.Context, _ *sessionConn, _ string, _ []runtime.MCPServerConfig) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	result := make(chan error, 1)
	go func() { result <- p.Start(testingContext(t), name, reapStartConfig()) }()
	select {
	case <-entered:
	case <-time.After(reapWait):
		t.Fatal("handshake not reached")
	}
	for i := 0; i < 2; i++ {
		begin := time.Now()
		err := p.Stop(name)
		if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
			t.Fatalf("Stop #%d = %v, want ErrProcessNotConfirmedDead", i+1, err)
		}
		if runtime.IsSessionGone(err) {
			t.Errorf("Stop #%d reads as gone: %v", i+1, err)
		}
		if elapsed := time.Since(begin); elapsed > reapWait/2 {
			t.Fatalf("Stop #%d took %v; the injected bound is %v", i+1, elapsed, p.startupStopBound)
		}
		if reapSentinel(p, name) == nil {
			t.Fatalf("Stop #%d released the reservation", i+1)
		}
		if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "old" {
			t.Errorf("Stop #%d cleaned metadata (%q) on the startup-cancel path", i+1, got)
		}
	}
	hold.let()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled Start succeeded")
		}
	case <-time.After(reapWait):
		t.Fatal("Start never finished after the process was confirmed dead")
	}
	if err := p.Stop(name); err != nil {
		t.Errorf("Stop after the drain = %v, want nil", err)
	}
}

// (c, handover) A Stop that finds a handed-over drain reports the same typed
// error, and succeeds once the drain has finished.
func TestStopWhileStartDrainsThenSucceeds(t *testing.T) {
	tc := startReapCases()[1]
	p, name, hold, sentinel, _, err := expireStart(t, tc)
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("Start = %v", err)
	}
	if err := p.Stop(name); !errors.Is(err, runtime.ErrProcessNotConfirmedDead) || runtime.IsSessionGone(err) {
		t.Fatalf("Stop during drain = %v, want a non-gone ErrProcessNotConfirmedDead", err)
	}
	if got, _ := p.GetMeta(name, "GC_SESSION_ID"); got != "old" {
		t.Errorf("Stop cleaned metadata (%q) while the drain still owned the reservation", got)
	}
	hold.let()
	select {
	case <-sentinel.done:
	case <-time.After(reapWait):
		t.Fatal("drain never finished")
	}
	if err := p.Stop(name); err != nil {
		t.Errorf("Stop after the drain = %v, want nil", err)
	}
}

// (d) Stop's lifecycle-lock wait is bounded.
func TestStopLifecycleLockWaitIsBounded(t *testing.T) {
	hold := newReapHold(t)
	p := newReapBoundProvider(t, hold)
	holder := NewProviderWithDir(p.dir, Config{})
	name := testName()
	lock, err := holder.lockLifecycle(name, true)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = lock.Close()
		}
	})
	result := make(chan error, 1)
	go func() { result <- p.Stop(name) }()
	select {
	case err := <-result:
		if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
			t.Fatalf("Stop = %v, want ErrProcessNotConfirmedDead", err)
		}
		if runtime.IsSessionGone(err) {
			t.Errorf("Stop reads as gone: %v", err)
		}
		if errors.Is(err, runtime.ErrSessionExists) {
			t.Errorf("Stop callers branch only on IsSessionGone; ErrSessionExists would read as a collision: %v", err)
		}
	case <-time.After(reapWait):
		t.Fatal("Stop blocked on a lifecycle lock another holder never releases")
	}
	_ = lock.Close()
	released = true
	if err := p.Stop(name); err != nil {
		t.Errorf("Stop after the holder released = %v, want nil", err)
	}
}

// (d) A lock failure other than contention returns at once instead of polling
// to the deadline.
func TestStopLifecycleLockErrorReturnsAtOnce(t *testing.T) {
	hold := newReapHold(t)
	file := filepath.Join(shortTempDir(t), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewProviderWithDir(file, Config{})
	p.reapGate = hold.gate
	p.stopLockBound = time.Minute
	p.stopLockPoll = 5 * time.Millisecond
	begin := time.Now()
	err := p.Stop(testName())
	if err == nil {
		t.Fatal("Stop succeeded with an unusable lifecycle directory")
	}
	if errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Errorf("a non-contention lock error was reported as an unconfirmed kill: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > reapWait/2 {
		t.Errorf("Stop polled for %v before returning a non-contention error", elapsed)
	}
}

// (e) The handover has exactly one owner. Releasing the hold at the instant the
// bound expires races Start's deferred release against the drain; under -race a
// double clearSentinel/releaseStartup, a close of a closed channel or a read of
// cmd.ProcessState before done fails here.
func TestStartReapHandoverIsSingleOwner(t *testing.T) {
	cases := startReapCases()
	for i := 0; i < 20; i++ {
		tc := cases[i%len(cases)]
		t.Run(fmt.Sprintf("%02d/%s", i, tc.name), func(t *testing.T) {
			hold := newReapHold(t)
			p := newTestProvider(t)
			p.reapGate = hold.gate
			p.reapBound = time.Millisecond
			p.startupStopBound = time.Second
			name := testName()
			tc.arm(t, p, name)
			time.AfterFunc(p.reapBound, hold.let)
			result := make(chan error, 1)
			go func() { result <- p.Start(testingContext(t), name, reapStartConfig()) }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("Start succeeded")
				}
			case <-time.After(reapWait):
				t.Fatal("Start hung")
			}
			awaitReap(t, "the reservation to be released", func() bool {
				return reapSentinel(p, name) == nil && lifecycleLockFree(p, name)
			})
			if p.IsRunning(name) {
				t.Error("IsRunning true after the reservation drained")
			}
		})
	}
}
