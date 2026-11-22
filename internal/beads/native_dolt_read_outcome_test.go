package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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
