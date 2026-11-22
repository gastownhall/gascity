package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests pin what the native read loop does after a transient-classified
// failure (#7802): it asks the failing read's own pool for one ping before it
// reconnects, and a pool that answers ends the read with ErrNativeReadDeadline
// instead of a reopen-and-replay against the same endpoint.

// pingMode is how a fakePingConnector's connections answer a ping.
type pingMode int

const (
	// pingAnswers answers every ping.
	pingAnswers pingMode = iota
	// pingRefused fails every ping the way a dial to a dead port does.
	pingRefused
	// pingBlocks signals that the ping started and then waits for its context.
	pingBlocks
)

// fakePingConnector is a socket-free driver.Connector whose connections answer
// only a ping. Its conns implement driver.Pinger: without one database/sql
// treats every ping as answered, which would make the refused and blocking
// modes indistinguishable from the answering one.
type fakePingConnector struct {
	mode    pingMode
	pings   atomic.Int32
	started chan struct{}
}

func (c *fakePingConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakePingConn{connector: c}, nil
}

func (c *fakePingConnector) Driver() driver.Driver { return fakePingDriver{connector: c} }

type fakePingDriver struct{ connector *fakePingConnector }

func (d fakePingDriver) Open(string) (driver.Conn, error) {
	return &fakePingConn{connector: d.connector}, nil
}

type fakePingConn struct{ connector *fakePingConnector }

var errFakePingConnUnsupported = errors.New("fake ping conn: only Ping is supported")

func (*fakePingConn) Prepare(string) (driver.Stmt, error) { return nil, errFakePingConnUnsupported }
func (*fakePingConn) Close() error                        { return nil }
func (*fakePingConn) Begin() (driver.Tx, error)           { return nil, errFakePingConnUnsupported }

func (c *fakePingConn) Ping(ctx context.Context) error {
	c.connector.pings.Add(1)
	switch c.connector.mode {
	case pingRefused:
		return refusedDialError()
	case pingBlocks:
		select {
		case c.connector.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	default:
		return nil
	}
}

// refusedDialError is the error a dial to a port nothing listens on returns.
func refusedDialError() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// pooledStorageSpy is a library handle that exports a pool through
// UnderlyingDB, as every server-mode *dolt.DoltStore does.
type pooledStorageSpy struct {
	*nativeDoltStorageSpy
	db *sql.DB
}

func (s *pooledStorageSpy) UnderlyingDB() *sql.DB { return s.db }

// newPooledStorageSpy returns a handle whose pool pings in mode, and the
// connector that counts those pings.
func newPooledStorageSpy(t *testing.T, mode pingMode) (*pooledStorageSpy, *fakePingConnector) {
	t.Helper()
	connector := &fakePingConnector{mode: mode, started: make(chan struct{}, 1)}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fake pool: %v", err)
		}
	})
	return &pooledStorageSpy{nativeDoltStorageSpy: &nativeDoltStorageSpy{}, db: db}, connector
}

// errPostConnectRead is the error beads' withReadTx surfaces when the driver
// closed an established connection mid-read: go-sql-driver returns the
// ErrInvalidConn sentinel for a read deadline and for a server EOF alike.
var errPostConnectRead = errors.New("begin read tx: invalid connection")

// failOn returns a read that fails with readErr on the handle dead and succeeds
// on any other, counting every call.
func failOn(dead beadslib.Storage, readErr error, calls *int32) func(context.Context, beadslib.Storage) error {
	return func(_ context.Context, storage beadslib.Storage) error {
		atomic.AddInt32(calls, 1)
		if storage == dead {
			return readErr
		}
		return nil
	}
}

func TestNativeReadSlowQueryOnAnsweringEndpointFailsFast(t *testing.T) {
	answering, connector := newPooledStorageSpy(t, pingAnswers)
	var reopens int32
	// The endpoint never moved: every reopen lands on the same server, whose
	// query is just as slow.
	store := storeWithReopen(answering, answering, &reopens)
	store.readRetryBudgetOverride = 3 * time.Second

	var calls int32
	started := time.Now()
	err := store.withReadRetry(failOn(answering, errPostConnectRead, &calls))
	elapsed := time.Since(started)

	if !errors.Is(err, ErrNativeReadDeadline) {
		t.Fatalf("read err = %v after %s (%d calls, %d reopens), want ErrNativeReadDeadline",
			err, elapsed, atomic.LoadInt32(&calls), atomic.LoadInt32(&reopens))
	}
	if !errors.Is(err, errPostConnectRead) {
		t.Errorf("read err = %v, want the read error preserved in the chain", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("read ran %d times, want 1: replaying on an endpoint that answers cannot finish the query", n)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Errorf("reopens = %d, want 0", n)
	}
	if n := connector.pings.Load(); n != 1 {
		t.Errorf("pings = %d, want exactly one endpoint probe", n)
	}
	if elapsed >= nativeReadEndpointProbeBudget {
		t.Errorf("read took %s, want one pass plus at most the %s probe budget", elapsed, nativeReadEndpointProbeBudget)
	}
}

// shortenEndpointProbeBudget sets nativeReadEndpointProbeBudget for one test.
func shortenEndpointProbeBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := nativeReadEndpointProbeBudget
	nativeReadEndpointProbeBudget = budget
	t.Cleanup(func() { nativeReadEndpointProbeBudget = previous })
}

func TestNativeReadUnansweredEndpointStillReconnects(t *testing.T) {
	shortenEndpointProbeBudget(t, 50*time.Millisecond)
	for _, tc := range []struct {
		name string
		mode pingMode
	}{
		{name: "refused ping", mode: pingRefused},
		{name: "ping outlives the probe budget", mode: pingBlocks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead, connector := newPooledStorageSpy(t, tc.mode)
			fresh, _ := newPooledStorageSpy(t, pingAnswers)
			var reopens int32
			store := storeWithReopen(dead, fresh, &reopens)
			store.readRetryBudgetOverride = 5 * time.Second

			var calls int32
			if err := store.withReadRetry(failOn(dead, errPostConnectRead, &calls)); err != nil {
				t.Fatalf("read err = %v (%d calls, %d reopens), want the replay on the fresh handle to succeed",
					err, atomic.LoadInt32(&calls), atomic.LoadInt32(&reopens))
			}
			if n := atomic.LoadInt32(&reopens); n != 1 {
				t.Errorf("reopens = %d, want 1", n)
			}
			if n := atomic.LoadInt32(&calls); n != 2 {
				t.Errorf("read ran %d times, want 2: the failed pass and the replay", n)
			}
			if n := connector.pings.Load(); n != 1 {
				t.Errorf("pings = %d, want 1", n)
			}
		})
	}
}

func TestNativeReadDialStageFailureReconnectsWithoutProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readErr error
	}{
		{name: "dial text", readErr: errors.New("begin read tx: dial tcp 127.0.0.1:3307: connect: connection refused")},
		{name: "typed dial error", readErr: fmt.Errorf("begin read tx: %w", refusedDialError())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The pool answers now, but the pass could not reach the endpoint
			// at all: a restart that finished after the library gave up still
			// needs the reopen.
			dead, connector := newPooledStorageSpy(t, pingAnswers)
			fresh, _ := newPooledStorageSpy(t, pingAnswers)
			var reopens int32
			store := storeWithReopen(dead, fresh, &reopens)
			store.readRetryBudgetOverride = 5 * time.Second

			var calls int32
			if err := store.withReadRetry(failOn(dead, tc.readErr, &calls)); err != nil {
				t.Fatalf("read err = %v, want the replay on the fresh handle to succeed", err)
			}
			if n := connector.pings.Load(); n != 0 {
				t.Errorf("pings = %d, want 0: a dial-stage failure never reached the endpoint", n)
			}
			if n := atomic.LoadInt32(&reopens); n != 1 {
				t.Errorf("reopens = %d, want 1", n)
			}
			if n := atomic.LoadInt32(&calls); n != 2 {
				t.Errorf("read ran %d times, want 2", n)
			}
		})
	}
}

// alwaysFails is a read that fails with readErr on every handle.
func alwaysFails(readErr error) func(context.Context, beadslib.Storage) error {
	return func(context.Context, beadslib.Storage) error { return readErr }
}

func TestNativeReadBudgetExhaustionIsTyped(t *testing.T) {
	shortenEndpointProbeBudget(t, 50*time.Millisecond)
	dead, _ := newPooledStorageSpy(t, pingRefused)
	store := newNativeDoltStoreForTest(dead)
	var reopens int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		next, _ := newPooledStorageSpy(t, pingRefused)
		return next, nil
	}
	store.readRetryBudgetOverride = 300 * time.Millisecond

	err := store.withReadRetry(alwaysFails(errPostConnectRead))

	if !errors.Is(err, ErrNativeReadBudgetExhausted) {
		t.Fatalf("read err = %v (%d reopens), want ErrNativeReadBudgetExhausted", err, atomic.LoadInt32(&reopens))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("read err = %v, want context.DeadlineExceeded in the chain", err)
	}
	if errors.Is(err, ErrNativeReadDeadline) {
		t.Errorf("read err = %v matches ErrNativeReadDeadline, but no endpoint ever answered", err)
	}
	const want = "native Dolt read retry budget exhausted (context deadline exceeded), last error: begin read tx: invalid connection"
	if got := err.Error(); got != want {
		t.Errorf("read err text = %q, want it unchanged: %q", got, want)
	}
	if atomic.LoadInt32(&reopens) == 0 {
		t.Error("reopens = 0, want the unanswered endpoint to reconnect until the budget ran out")
	}
}

func TestNativeReadRecoveryFailureIsTyped(t *testing.T) {
	shortenEndpointProbeBudget(t, 50*time.Millisecond)
	errReopen := errors.New("open managed dolt: permission denied")

	t.Run("reconnect after a transient read error", func(t *testing.T) {
		dead, _ := newPooledStorageSpy(t, pingRefused)
		store := newNativeDoltStoreForTest(dead)
		var reopens int32
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return nil, errReopen
		}
		store.readRetryBudgetOverride = 5 * time.Second

		err := store.withReadRetry(alwaysFails(errPostConnectRead))

		if !errors.Is(err, ErrNativeReadRecoveryFailed) {
			t.Fatalf("read err = %v, want ErrNativeReadRecoveryFailed", err)
		}
		if !errors.Is(err, errReopen) {
			t.Errorf("read err = %v, want the reopen hook's error in the chain", err)
		}
		if errors.Is(err, ErrNativeReadDeadline) || errors.Is(err, ErrNativeReadBudgetExhausted) {
			t.Errorf("read err = %v matches another read outcome", err)
		}
		const want = "native Dolt reconnect after transient read error (begin read tx: invalid connection): open managed dolt: permission denied"
		if got := err.Error(); got != want {
			t.Errorf("read err text = %q, want it unchanged: %q", got, want)
		}
		if n := atomic.LoadInt32(&reopens); n != 1 {
			t.Errorf("reopens = %d, want 1", n)
		}
	})

	t.Run("re-pin of a stale pool before the read", func(t *testing.T) {
		live, _ := newPooledStorageSpy(t, pingAnswers)
		store := newNativeDoltStoreForTest(live)
		store.reopen = func(context.Context) (beadslib.Storage, error) { return nil, errReopen }
		store.readRetryBudgetOverride = 5 * time.Second
		if !store.markPoolStale() {
			t.Fatal("markPoolStale refused a store with a reopen hook")
		}

		var calls int32
		err := store.withReadRetry(failOn(nil, nil, &calls))

		if !errors.Is(err, ErrNativeReadRecoveryFailed) || !errors.Is(err, errReopen) {
			t.Fatalf("read err = %v, want ErrNativeReadRecoveryFailed wrapping the reopen hook's error", err)
		}
		const want = "native Dolt re-pin after a proxy generation change: open managed dolt: permission denied"
		if got := err.Error(); got != want {
			t.Errorf("read err text = %q, want it unchanged: %q", got, want)
		}
		if n := atomic.LoadInt32(&calls); n != 0 {
			t.Errorf("read ran %d times, want 0: the pool was stale", n)
		}
	})
}
