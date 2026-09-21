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
	proxy := startProbeListener(t, listenerStaysSilent)

	// A budget well below ProbeSessionTimeout: the probe's own deadline is what
	// ends this session, and the test is about what it leaves behind.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := DefaultProbeIO(proxy.port, "beads").Session(ctx); err == nil {
		t.Fatal("a listener that never greets produced a successful session")
	}

	select {
	case <-proxy.gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy still sees the probe's connection after the probe returned; a session that outlives the check blocks bd's idle watcher for as long as it is held")
	}
	if got := proxy.accepted.Load(); got != 1 {
		t.Fatalf("the session cost the proxy %d accept(s), want exactly 1", got)
	}
	if got := doltpool.Len(); got != 0 {
		t.Fatalf("the probe registered %d pool(s) in the process-lifetime doltpool registry, want 0: a handle nobody may close keeps its connections for the life of the process", got)
	}
	if got := doltpool.TotalOpenConns(); got != 0 {
		t.Fatalf("doltpool holds %d open connection(s) after the probe, want 0", got)
	}
	proxy.stop(t)
}

// TestProbeOnARealSocketNeverCallsASlowProxyAZombie is the fence the fake
// connector could not carry: the real driver, a real socket, and a proxy that
// accepted the connection and has not answered yet.
//
// The fake-connector tests inject context.DeadlineExceeded, which is the error
// go-sql-driver returns only when its context watcher wins the race against its
// own socket deadline. In the shipped configuration it usually LOST that race:
// readPacket turns a read deadline into mysql.ErrInvalidConn, the classifier read
// that as a wire failure, the confirming dial succeeded, and the verdict was
// accepted_no_greeting — the token §3.4 escalates to `bd dolt stop` — for a proxy
// that is alive and slow. Measured 5 of 8 probes at production budgets against
// the listener below.
//
// Both arms run through the real driver. The first is what production sets. The
// second sets the driver's deadlines EQUAL to the session budget, which is the
// configuration that misclassified, and it must still answer unknown: the
// verdict has to come from the expired budget itself rather than from the margin
// that keeps the driver quiet.
func TestProbeOnARealSocketNeverCallsASlowProxyAZombie(t *testing.T) {
	// Short enough to run 8 probes per arm, long enough that a loaded box does
	// not turn the handshake into the thing under test.
	const budget = 200 * time.Millisecond
	const probes = 8

	arms := []struct {
		name          string
		driverTimeout time.Duration
	}{
		{name: "the shipped driver backstop", driverTimeout: probeDriverTimeout(budget)},
		{name: "driver deadlines equal to the session budget", driverTimeout: budget},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			proxy := startProbeListener(t, listenerStaysSilent)
			probeIO := probeIOWithDriverTimeout(proxy.port, "beads", arm.driverTimeout)
			dials := 0
			outcomes := map[string]int{}
			for i := 0; i < probes; i++ {
				result := probeWithBudget(context.Background(), ProbeIO{
					Session: probeIO.Session,
					Dial: func(ctx context.Context) error {
						dials++
						return probeIO.Dial(ctx)
					},
				}, budget)
				outcomes[result.Outcome.String()]++
				if result.Outcome != ProbeUnknown {
					t.Errorf("probe %d of a live-but-silent proxy reported %v (%v), want unknown: %s is the token the design escalates with `bd dolt stop`",
						i+1, result.Outcome, result.Err, result.Outcome)
				}
			}
			if outcomes["accepted_no_greeting"] != 0 {
				t.Errorf("%d of %d probes called a live proxy a zombie: %v", outcomes["accepted_no_greeting"], probes, outcomes)
			}
			// A deadline is not something a second connection can settle, and
			// spending one charges the proxy for the probe's own impatience.
			if dials != 0 {
				t.Errorf("the probes spent %d confirming dial(s) on their own expired budget, want 0", dials)
			}
			proxy.stop(t)
		})
	}
}

// TestProbeOnARealSocketStillNamesAProxyWhoseChildIsGone is the other half of
// the same fence: the fix must not buy its quiet by losing the one verdict this
// probe exists to produce.
//
// A proxy whose Dolt child has exited accepts the connection and closes it
// without a greeting, because bd's proxy parses no wire protocol. That is the
// only legitimate producer of accepted_no_greeting, it happens far inside the
// budget rather than at the end of it, and it must still be recognized over a
// real socket through the real driver.
func TestProbeOnARealSocketStillNamesAProxyWhoseChildIsGone(t *testing.T) {
	const budget = 200 * time.Millisecond
	proxy := startProbeListener(t, listenerClosesAtOnce)
	probeIO := probeIOWithDriverTimeout(proxy.port, "beads", probeDriverTimeout(budget))
	dials := 0
	result := probeWithBudget(context.Background(), ProbeIO{
		Session: probeIO.Session,
		Dial: func(ctx context.Context) error {
			dials++
			return probeIO.Dial(ctx)
		},
	}, budget)
	if result.Outcome != ProbeAcceptedNoGreeting {
		t.Fatalf("a listener that accepts and closes reported %v (%v), want accepted_no_greeting", result.Outcome, result.Err)
	}
	if dials != 1 {
		t.Fatalf("the probe spent %d confirming dial(s), want exactly 1", dials)
	}
	if !IsConnectionLevel(result.Err) || IsIndeterminate(result.Err) {
		t.Fatalf("the session error %v is not classified as a wire failure", result.Err)
	}
	proxy.stop(t)
}

// probeListenerBehavior is what the fixture listener does with a connection it
// has accepted. The two behaviors are the two states of bd's byte-pump proxy a
// probe has to keep apart.
type probeListenerBehavior int

const (
	// listenerStaysSilent accepts and never writes: a proxy in front of a Dolt
	// that is alive and has not answered yet. Every session against it ends on
	// the probe's own budget.
	listenerStaysSilent probeListenerBehavior = iota
	// listenerClosesAtOnce accepts and closes without a greeting: a proxy whose
	// Dolt child has exited, which is accepted_no_greeting's one true source.
	listenerClosesAtOnce
)

// probeListener is one loopback listener standing in for a proxy's data port.
type probeListener struct {
	port     int
	accepted *atomic.Int64
	// gone receives once for every accepted connection the listener has seen go
	// away, which is the only place the client's socket close can be observed.
	gone <-chan struct{}
	stop func(t *testing.T)
}

// startProbeListener opens the one listener this file uses, in the requested
// behavior, and closes it (waiting for its handlers) on test cleanup.
//
// One call site on purpose: every real-socket test here shares it, so the
// package's stream-listener footprint in the repo's shrink-only resource census
// stays at one regardless of how many cases are added.
func startProbeListener(t *testing.T, behavior probeListenerBehavior) probeListener {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(Host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	gone := make(chan struct{}, 64)
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
				if behavior == listenerClosesAtOnce {
					// The deferred close IS the behavior: no greeting, and the
					// peer learns about it immediately.
					return
				}
				// No greeting, ever: the read returns only when the client
				// closes, which is what the socket-lifetime test waits for.
				_, _ = io.Copy(io.Discard, conn)
				select {
				case gone <- struct{}{}:
				default:
				}
			}()
		}
	}()
	var once sync.Once
	stop := func(t *testing.T) {
		t.Helper()
		once.Do(func() {
			if closeErr := listener.Close(); closeErr != nil {
				t.Errorf("close listener: %v", closeErr)
			}
			wg.Wait()
		})
	}
	t.Cleanup(func() { stop(t) })
	return probeListener{
		port:     listener.Addr().(*net.TCPAddr).Port,
		accepted: &accepted,
		gone:     gone,
		stop:     stop,
	}
}

// fakeProbeConnector is a driver.Connector that counts the connections a probe
// opens and closes, so the lifecycle can be asserted without a listener, a
// database or a byte of MySQL wire protocol.
type fakeProbeConnector struct {
	cursors    map[string]int64
	connectErr error
	pingErr    error
	queryErr   error
	opened     atomic.Int64
	closed     atomic.Int64
}

func (c *fakeProbeConnector) Connect(context.Context) (driver.Conn, error) {
	if c.connectErr != nil {
		return nil, c.connectErr
	}
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
func (c *fakeProbeConn) Ping(context.Context) error { return c.connector.pingErr }

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
