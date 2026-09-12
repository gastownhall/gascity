package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// handshakeDeadline delivers a deadline event at the test's barrier rather than
// racing process startup against a wall-clock timer.
type handshakeDeadline struct{ done chan struct{} }

func (c *handshakeDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *handshakeDeadline) Done() <-chan struct{}       { return c.done }
func (c *handshakeDeadline) Value(any) any               { return nil }
func (c *handshakeDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type handshakeBackpressure struct {
	io.WriteCloser
	entered chan struct{}
}

func (w *handshakeBackpressure) Write([]byte) (int, error) {
	close(w.entered)
	// The fake server never reads stdin. This exceeds the kernel pipe capacity,
	// so only closing the owned pipe or killing the child can release the write.
	return w.WriteCloser.Write(bytes.Repeat([]byte("x"), 4*1024*1024))
}

func TestStartupBlockedHandshakeWrite(t *testing.T) {
	for _, mode := range []string{"cancel", "same-provider-stop", "cross-provider-deadline", "configured-deadline"} {
		t.Run(mode, func(t *testing.T) {
			p := newTestProvider(t)
			p.cfg.HandshakeTimeout = time.Hour
			if mode == "configured-deadline" {
				// The timer itself is under test. Other cases inject the deadline
				// event after the write barrier, independent of wall-clock time.
				p.cfg.HandshakeTimeout = 100 * time.Millisecond
			}
			name := testName()
			observer := NewProviderWithDir(p.dir, Config{})
			bound := testingContext(t)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadline := &handshakeDeadline{done: make(chan struct{})}
			ctx := parent
			if mode == "cross-provider-deadline" {
				ctx = deadline
			}
			w := &handshakeBackpressure{entered: make(chan struct{})}
			p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
				w.WriteCloser = sc.stdin
				sc.stdin = w
				return p.handshake(ctx, sc, wd, servers)
			}
			command := strings.Replace(fakeACPShellCommand(), "for line in sys.stdin:", "import signal\nsignal.pause()\nfor line in sys.stdin:", 1)
			started := make(chan error, 1)
			go func() {
				started <- p.Start(ctx, name, runtime.Config{Command: command, Env: map[string]string{"GC_SESSION_ID": "blocked"}})
			}()
			select {
			case <-w.entered:
			case <-bound.Done():
				t.Fatal("handshake write not reached")
			}
			// Break the actual pipe on RED too, so the regression cannot leak a child.
			t.Cleanup(func() { _ = w.Close(); _ = p.Stop(name) })
			stopped := make(chan error, 1)
			switch mode {
			case "cancel":
				cancel()
				stopped <- nil
			case "same-provider-stop":
				go func() { stopped <- p.Stop(name) }()
			case "cross-provider-deadline":
				go func() { stopped <- observer.Stop(name) }()
				close(deadline.done)
			case "configured-deadline":
				go func() { stopped <- observer.Stop(name) }()
			}
			select {
			case err := <-started:
				if err == nil {
					t.Fatal("blocked handshake succeeded")
				}
				want := context.Canceled
				if mode == "cross-provider-deadline" || mode == "configured-deadline" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Errorf("Start error = %v, want %v", err, want)
				}
			case <-bound.Done():
				t.Fatal("canceled handshake write kept Start and lifecycle lock blocked")
			}
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-bound.Done():
				t.Fatal("Stop did not finish")
			}
			if observer.IsRunning(name) {
				t.Error("failed handshake process survived")
			}
			if got, err := observer.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "" {
				t.Errorf("failed handshake metadata survived: %q, %v", got, err)
			}
			p.handshakeFunc = nil
			p.cfg.HandshakeTimeout = testutil.ExecRaceTimeout
			if err := p.Start(bound, name, runtime.Config{Command: fakeACPShellCommand(), Nudge: "retry", Env: map[string]string{"GC_SESSION_ID": "retry"}}); err != nil {
				t.Fatal(err)
			}
			if got, err := observer.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "retry" {
				t.Errorf("retry identity: %q, %v", got, err)
			}
		})
	}
}

func TestStartupHandshakeCancellationDisarmedOnSuccess(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	ctx, cancel := context.WithCancel(testingContext(t))
	defer cancel()
	var pipe io.WriteCloser
	var handshakeCtx context.Context
	p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
		pipe, handshakeCtx = sc.stdin, ctx
		return p.handshake(ctx, sc, wd, servers)
	}
	if err := p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	cancel()
	if handshakeCtx.Err() == nil {
		t.Fatal("handshake context was not canceled after completion")
	}
	if _, err := pipe.Write([]byte("\n")); err != nil {
		t.Fatalf("completed handshake cancellation closed live stdin: %v", err)
	}
	if err := p.Nudge(name, runtime.TextContent("still usable")); err != nil {
		t.Fatalf("Nudge after successful transfer: %v", err)
	}
}

// The process is a fake ACP server; the handshake boundary is channel-gated so
// metadata is inspected while both the sentinel and control socket are live.
func TestStartupOwnership(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel", "stop"} {
		t.Run(outcome, func(t *testing.T) {
			p := newTestProvider(t)
			name := testName()
			ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
				close(entered)
				select {
				case <-release:
					if outcome == "failure" {
						return errors.New("injected handshake failure")
					}
					return p.handshake(ctx, sc, wd, servers)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			cfg := runtime.Config{Command: fakeACPShellCommand(), WorkDir: t.TempDir(), Env: map[string]string{
				"GC_SESSION_ID": "session-a", "GC_INSTANCE_TOKEN": "incarnation-a", "GC_RUNTIME_EPOCH": "2",
				"ANTHROPIC_API_KEY": "must-not-persist", "GC_CONTROLLER_TOKEN": "also-secret",
			}}
			result := make(chan error, 1)
			go func() { result <- p.Start(ctx, name, cfg) }()
			t.Cleanup(func() { cancel(); _ = p.Stop(name) })
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("startup did not reach handshake")
			}
			reader := NewProviderWithDir(p.dir, Config{})
			for _, provider := range []*Provider{p, reader} {
				if !provider.IsRunning(name) {
					t.Error("runtime not visible during handshake")
				}
				for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
					if got, err := provider.GetMeta(name, key); err != nil || got != cfg.Env[key] {
						t.Errorf("GetMeta(%s) = %q, %v during handshake", key, got, err)
					}
				}
			}
			for _, key := range []string{"ANTHROPIC_API_KEY", "GC_CONTROLLER_TOKEN"} {
				if _, err := os.Stat(p.metaPath(name, key)); !os.IsNotExist(err) {
					t.Errorf("secret sidecar %s exists: %v", key, err)
				}
			}
			// A foreign attempt must not overwrite identity or stop this process.
			foreign := cfg
			foreign.Env = map[string]string{"GC_SESSION_ID": "foreign", "GC_INSTANCE_TOKEN": "foreign"}
			for _, provider := range []*Provider{p, reader} {
				if err := provider.Start(ctx, name, foreign); !errors.Is(err, runtime.ErrSessionExists) {
					t.Errorf("foreign Start = %v, want ErrSessionExists", err)
				}
				if got, err := provider.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "session-a" {
					t.Errorf("foreign attempt changed ownership: %q, %v", got, err)
				}
			}
			switch outcome {
			case "cancel":
				cancel()
			case "stop":
				if err := p.Stop(name); err != nil {
					t.Fatal(err)
				}
			default:
				close(release)
			}
			var err error
			select {
			case err = <-result:
			case <-testingContext(t).Done():
				t.Fatal("Start did not finish")
			}
			if (err == nil) != (outcome == "success") {
				t.Fatalf("Start = %v for %s", err, outcome)
			}
			if outcome == "success" {
				if err := p.Stop(name); err != nil {
					t.Fatal(err)
				}
			}
			if p.IsRunning(name) || reader.IsRunning(name) {
				t.Error("runtime survived cleanup")
			}
			for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
				if got, err := reader.GetMeta(name, key); err != nil || got != "" {
					t.Errorf("metadata survived cleanup: %s = %q, %v", key, got, err)
				}
			}
			p.handshakeFunc = nil
			if err := p.Start(testingContext(t), name, foreign); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got, err := reader.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "foreign" {
				t.Errorf("retry identity = %q, %v", got, err)
			}
			if got, err := reader.GetMeta(name, "GC_RUNTIME_EPOCH"); err != nil || got != "" {
				t.Errorf("retry inherited omitted epoch = %q, %v", got, err)
			}
		})
	}
}

// blockedInitialWrite models pipe backpressure after the real handshake. Close
// interrupts the write; returnWrite lets the test delay Start's deferred cleanup
// until after a replacement has acquired the same runtime name.
type blockedInitialWrite struct {
	io.WriteCloser
	entered     chan struct{}
	closed      chan struct{}
	returnWrite chan struct{}
	once        sync.Once
}

func (w *blockedInitialWrite) Write([]byte) (int, error) {
	close(w.entered)
	<-w.closed
	<-w.returnWrite
	return 0, io.ErrClosedPipe
}

func (w *blockedInitialWrite) Close() error {
	w.once.Do(func() {
		_ = w.WriteCloser.Close()
		close(w.closed)
	})
	return nil
}

func TestStartupBlockedInitialNudgeCanBeStopped(t *testing.T) {
	p := newTestProvider(t)
	p.cfg.HandshakeTimeout = testutil.ExecRaceTimeout
	name := testName()
	ctx := testingContext(t)
	w := &blockedInitialWrite{entered: make(chan struct{}), closed: make(chan struct{}), returnWrite: make(chan struct{})}
	p.handshakeFunc = func(ctx context.Context, sc *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
		if err := p.handshake(ctx, sc, wd, servers); err != nil {
			return err
		}
		w.WriteCloser = sc.stdin
		sc.stdin = w
		return nil
	}
	started := make(chan error, 1)
	go func() {
		started <- p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand(), Nudge: "initial prompt"})
	}()
	select {
	case <-w.entered:
	case <-ctx.Done():
		t.Fatal("initial write not reached")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(name) }()
	// Always break the injected blockage, including on the pre-fix deadlock.
	release := sync.OnceFunc(func() { close(w.returnWrite) })
	t.Cleanup(func() {
		_ = w.Close()
		release()
		_ = p.Stop(name)
	})
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Stop blocked behind initial nudge's lifecycle lock")
	}
	select {
	case <-w.closed:
	default:
		t.Fatal("Stop did not close the blocked pipe")
	}
	reader := NewProviderWithDir(p.dir, Config{})
	p.handshakeFunc = nil
	if err := p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "replacement"}}); err != nil {
		t.Fatalf("replacement Start: %v", err)
	}
	t.Cleanup(func() { _ = reader.Stop(name) })
	release()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("original Start did not finish")
	}
	if !reader.IsRunning(name) {
		t.Error("old Start cleanup removed replacement socket")
	}
	if got, err := reader.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "replacement" {
		t.Errorf("old Start cleanup changed replacement metadata: %q, %v", got, err)
	}
}

func testingContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testutil.ExecRaceTimeout)
	t.Cleanup(cancel)
	return ctx
}

// Stop must retain ownership until the canceled startup has drained its writes
// and process cleanup, rather than exposing the name to a newer incarnation.
func TestStartupStopRetainsReservation(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.handshakeFunc = func(ctx context.Context, _ *sessionConn, _ string, _ []runtime.MCPServerConfig) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}
	ctx := testingContext(t)
	result := make(chan error, 1)
	go func() { result <- p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand()}) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handshake not reached")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(name) }()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("handshake not canceled")
	}
	p.mu.Lock()
	_, reserved := p.conns[name]
	p.mu.Unlock()
	if !reserved {
		t.Error("Stop released reservation before startup cleanup")
	}
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Error("canceled Start succeeded")
		}
	case <-ctx.Done():
		t.Fatal("Start hung")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Stop hung")
	}
}

// Once the failed process has exited, socket liveness alone no longer fences a
// different provider from the still-pending startup's metadata cleanup.
func TestStartupFailureFencesCrossProviderRetry(t *testing.T) {
	p := newTestProvider(t)
	reader := NewProviderWithDir(p.dir, Config{})
	name := testName()
	ctx := testingContext(t)
	var sc *sessionConn
	p.handshakeFunc = func(ctx context.Context, conn *sessionConn, wd string, servers []runtime.MCPServerConfig) error {
		sc = conn
		return p.handshake(ctx, conn, wd, servers)
	}
	exited, release := make(chan struct{}), make(chan struct{})
	p.activityWrite = func(string, []byte) error {
		_ = sc.stdin.Close()
		<-sc.done
		close(exited)
		<-release
		return errors.New("injected publication failure")
	}
	result := make(chan error, 1)
	cfg := runtime.Config{Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "old"}}
	go func() { result <- p.Start(ctx, name, cfg) }()
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("process did not exit")
	}
	if reader.IsRunning(name) {
		t.Error("process socket survived exit")
	}
	err := reader.Start(ctx, name, cfg)
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("concurrent retry = %v, want ErrSessionExists", err)
	}
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Error("failed startup succeeded")
		}
	case <-ctx.Done():
		t.Fatal("startup hung")
	}
	if err == nil {
		_ = reader.Stop(name)
	}
	cfg.Env = map[string]string{"GC_SESSION_ID": "new"}
	if err := reader.Start(ctx, name, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Stop(name) })
	if got, err := reader.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "new" {
		t.Fatalf("retry metadata = %q, %v", got, err)
	}
	if !reader.IsRunning(name) {
		t.Error("retry socket lost")
	}
}

func TestStartupMetadataFailureDoesNotPublishLiveness(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	path := p.metaPath(name, "GC_SESSION_ID")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "obstruction"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := p.Start(testingContext(t), name, runtime.Config{
		Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "session", "GC_INSTANCE_TOKEN": "token"},
	})
	if err == nil {
		_ = p.Stop(name)
		t.Fatal("Start accepted unwritable identity")
	}
	if p.IsRunning(name) {
		t.Fatal("failed seeding published liveness")
	}
	if got, err := p.GetMeta(name, "GC_INSTANCE_TOKEN"); err != nil || got != "" {
		t.Errorf("partial seed survived: %q, %v", got, err)
	}
}

func TestStartupCancellationDuringActivityWrite(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	entered, release := make(chan struct{}), make(chan struct{})
	p.activityWrite = func(path string, data []byte) error {
		close(entered)
		<-release
		return runtime.WritePrivateFile(path, data)
	}
	ctx, cancel := context.WithCancel(testingContext(t))
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- p.Start(ctx, name, runtime.Config{Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "canceled"}})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("activity write not reached")
	}
	cancel()
	close(release)
	select {
	case err := <-result:
		if err == nil {
			_ = p.Stop(name)
			t.Fatal("Start committed after cancellation during activity write")
		}
	case <-testingContext(t).Done():
		t.Fatal("Start hung")
	}
	if p.IsRunning(name) {
		t.Error("canceled runtime survived")
	}
	for _, key := range []string{"GC_SESSION_ID", lastActivityMetaKey} {
		if got, err := p.GetMeta(name, key); err != nil || got != "" {
			t.Errorf("canceled startup metadata survived: %s = %q, %v", key, got, err)
		}
	}
}

func TestStopStaleConnectionPreservesNewIncarnation(t *testing.T) {
	p := newTestProvider(t)
	reader := NewProviderWithDir(p.dir, Config{})
	name := testName()
	ctx := testingContext(t)
	cfg := runtime.Config{Command: fakeACPShellCommand(), Env: map[string]string{"GC_SESSION_ID": "old"}}
	if err := p.Start(ctx, name, cfg); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	_ = sc.stdin.Close()
	select {
	case <-sc.done:
	case <-ctx.Done():
		t.Fatal("process did not exit")
	}
	cfg.Env = map[string]string{"GC_SESSION_ID": "new"}
	if err := reader.Start(ctx, name, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Stop(name) })
	if err := p.Stop(name); err == nil {
		t.Error("stale Stop should report the foreign runtime")
	}
	if !reader.IsRunning(name) {
		t.Error("stale Stop killed replacement")
	}
	if got, err := reader.GetMeta(name, "GC_SESSION_ID"); err != nil || got != "new" {
		t.Errorf("stale Stop removed replacement metadata: %q, %v", got, err)
	}
}
