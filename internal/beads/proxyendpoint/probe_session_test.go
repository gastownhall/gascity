package proxyendpoint

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doltpool"
)

// TestProbeSessionClosesEveryConnectionItOpens is the fence under the hard
// constraint: a probe may not leave a connection to bd's proxy open once it has
// answered.
//
// bd's idle watcher counts every accepted TCP connection and cannot arm while
// one is open, so a session that outlived the check would defer the retirement
// the check exists to report. It is asserted at the driver level rather than
// through sql.DB's counters because the driver is where the socket is: a closed
// driver connection is the property, and a handle that merely reports zero open
// connections while a pool elsewhere still holds one would satisfy the weaker
// claim.
func TestProbeSessionClosesEveryConnectionItOpens(t *testing.T) {
	t.Run("a served session", func(t *testing.T) {
		fake := &fakeProbeConnector{cursors: map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26}}
		cursors, err := readCursorsOver(context.Background(), fake)
		if err != nil {
			t.Fatalf("readCursorsOver: %v", err)
		}
		if cursors != (Cursors{Main: 66, Ignored: 26}) {
			t.Fatalf("cursors = %v, want main=66 ignored=26", cursors)
		}
		if got := fake.opened.Load(); got != 1 {
			t.Fatalf("the probe opened %d connection(s), want exactly 1", got)
		}
		if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != closed {
			t.Fatalf("the probe opened %d connection(s) and closed %d; a session that outlives the check keeps bd's idle watcher from arming", opened, closed)
		}
	})

	t.Run("a failed cursor read", func(t *testing.T) {
		fake := &fakeProbeConnector{queryErr: errors.New("Error 1146 (42S02): Table doesn't exist")}
		if _, err := readCursorsOver(context.Background(), fake); err == nil {
			t.Fatal("readCursorsOver reported success for a failing query")
		}
		if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != closed || opened != 1 {
			t.Fatalf("a failed read opened %d connection(s) and closed %d, want 1 and 1", opened, closed)
		}
	})

	t.Run("a caller who has already given up", func(t *testing.T) {
		// doctor abandons a timed-out check's goroutine, so the probe must
		// close whatever it opened on the canceled path too.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fake := &fakeProbeConnector{cursors: map[string]int64{mainCursorQuery: 66, ignoredCursorQuery: 26}}
		if _, err := readCursorsOver(ctx, fake); !errors.Is(err, context.Canceled) {
			t.Fatalf("readCursorsOver with a canceled context = %v, want context.Canceled", err)
		}
		if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != closed {
			t.Fatalf("a canceled probe opened %d connection(s) and closed %d", opened, closed)
		}
	})
}

// TestProbeSessionLeavesNoSocketOpenOnARealListener proves the same property on
// a real socket, through the real driver, against a listener that behaves like a
// proxy whose backend never answers: it accepts and then says nothing.
//
// The assertion is made from the SERVER side on purpose. Only the peer can tell
// whether the client's socket is really gone, and "the proxy sees the connection
// close before the check returns" is the constraint stated in the design, where
// a client-side counter is only gc's opinion of it.
func TestProbeSessionLeavesNoSocketOpenOnARealListener(t *testing.T) {
	listener, err := net.Listen("tcp", net.JoinHostPort(Host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck // the fixture listener

	var accepted atomic.Int64
	var wg sync.WaitGroup
	gone := make(chan struct{}, 4)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close() //nolint:errcheck // the fixture's accepted connection
				// No greeting, ever: the read returns only when the client
				// closes, which is exactly what this test is waiting for.
				_, _ = io.Copy(io.Discard, conn)
				gone <- struct{}{}
			}()
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	// A budget well below ProbeSessionTimeout: the probe's own deadline is what
	// ends this session, and the test is about what it leaves behind.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := DefaultProbeIO(port, "beads").Session(ctx); err == nil {
		t.Fatal("a listener that never greets produced a successful session")
	}

	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy still sees the probe's connection after the probe returned; a session that outlives the check blocks bd's idle watcher for as long as it is held")
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("the session cost the proxy %d accept(s), want exactly 1", got)
	}
	if got := doltpool.Len(); got != 0 {
		t.Fatalf("the probe registered %d pool(s) in the process-lifetime doltpool registry, want 0: a handle nobody may close keeps its connections for the life of the process", got)
	}
	if got := doltpool.TotalOpenConns(); got != 0 {
		t.Fatalf("doltpool holds %d open connection(s) after the probe, want 0", got)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	wg.Wait()
}

// fakeProbeConnector is a driver.Connector that counts the connections a probe
// opens and closes, so the lifecycle can be asserted without a listener, a
// database or a byte of MySQL wire protocol.
type fakeProbeConnector struct {
	cursors  map[string]int64
	queryErr error
	opened   atomic.Int64
	closed   atomic.Int64
}

func (c *fakeProbeConnector) Connect(context.Context) (driver.Conn, error) {
	c.opened.Add(1)
	return &fakeProbeConn{connector: c}, nil
}

func (c *fakeProbeConnector) Driver() driver.Driver { return fakeProbeDriver{} }

// fakeProbeDriver exists only because driver.Connector requires one; nothing
// opens a connection through a DSN here.
type fakeProbeDriver struct{}

func (fakeProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("proxyendpoint: the probe fixture has no DSN driver")
}

type fakeProbeConn struct {
	connector *fakeProbeConnector
}

func (c *fakeProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("proxyendpoint: the probe fixture answers queries directly")
}

func (c *fakeProbeConn) Close() error {
	c.connector.closed.Add(1)
	return nil
}

func (c *fakeProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("proxyendpoint: the probe fixture is read-only")
}

// Ping answers the probe's handshake check.
func (c *fakeProbeConn) Ping(context.Context) error { return nil }

// QueryContext answers the existence probe and both cursor reads, which is the
// whole statement surface readCursors uses.
func (c *fakeProbeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.connector.queryErr != nil && query != cursorExistsQuery {
		return nil, c.connector.queryErr
	}
	if query == cursorExistsQuery {
		return &fakeProbeRows{value: 1}, nil
	}
	value, ok := c.connector.cursors[query]
	if !ok {
		return nil, errors.New("proxyendpoint: the probe fixture has no answer for " + query)
	}
	return &fakeProbeRows{value: value}, nil
}

// fakeProbeRows is one row of one integer column, which is the shape of every
// answer the probe reads.
type fakeProbeRows struct {
	value int64
	done  bool
}

func (r *fakeProbeRows) Columns() []string { return []string{"value"} }

func (r *fakeProbeRows) Close() error { return nil }

func (r *fakeProbeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}
