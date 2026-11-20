package proxyendpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/doltpool"
)

// TestOpenSessionIsOneClosedSession pins OpenSession's contract at the driver:
// one dial, one pinned connection, and nothing left open once the caller's
// close returns, on the success path and on every failure path.
func TestOpenSessionIsOneClosedSession(t *testing.T) {
	t.Run("a served session closes on the caller's close", func(t *testing.T) {
		fake := &fakeProbeConnector{}
		conn, closeSession, err := openSessionOver(context.Background(), fake)
		if err != nil {
			t.Fatalf("openSessionOver: %v", err)
		}
		if conn == nil {
			t.Fatal("openSessionOver returned no connection")
		}
		if got := fake.dials.Load(); got != 1 {
			t.Fatalf("the session dialed %d time(s), want exactly 1", got)
		}
		if got := fake.closed.Load(); got != 0 {
			t.Fatalf("the session closed %d connection(s) before the caller's close, want 0", got)
		}
		if err := closeSession(); err != nil {
			t.Fatalf("close session: %v", err)
		}
		if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != 1 || closed != 1 {
			t.Fatalf("the session opened %d and closed %d connection(s), want 1 and 1", opened, closed)
		}
	})

	t.Run("a failed ping closes the session", func(t *testing.T) {
		pingErr := errors.New("ping refused")
		fake := &fakeProbeConnector{pingErr: pingErr}
		if _, _, err := openSessionOver(context.Background(), fake); !errors.Is(err, pingErr) {
			t.Fatalf("openSessionOver with a failing ping = %v, want %v", err, pingErr)
		}
		if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != 1 || closed != 1 {
			t.Fatalf("a failed ping opened %d and closed %d connection(s), want 1 and 1", opened, closed)
		}
	})

	t.Run("a caller who has already given up costs no dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fake := &fakeProbeConnector{}
		if _, _, err := openSessionOver(ctx, fake); !errors.Is(err, context.Canceled) {
			t.Fatalf("openSessionOver with a canceled context = %v, want context.Canceled", err)
		}
		if got := fake.dials.Load(); got != 0 {
			t.Fatalf("a canceled session dialed %d time(s), want 0", got)
		}
	})

	t.Run("no database is refused before any dial", func(t *testing.T) {
		if _, _, err := OpenSession(context.Background(), 1, "  "); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("OpenSession with no database = %v, want ErrNoDatabase", err)
		}
	})
}

// TestOpenSessionDialsTheProxyPortOnceOnARealListener drives the real driver
// over a real loopback socket: the session reaches the proxy's port with one
// accept and registers nothing in the process-lifetime doltpool registry.
func TestOpenSessionDialsTheProxyPortOnceOnARealListener(t *testing.T) {
	proxy := startProbeListener(t, listenerAnswersPingThenResets)
	conn, closeSession, err := OpenSession(context.Background(), proxy.port, "hq")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if conn == nil {
		t.Fatal("OpenSession returned no connection")
	}
	if err := closeSession(); err != nil {
		t.Logf("close after the fixture's reset: %v", err)
	}
	if got := proxy.acceptsBefore(t); got != 1 {
		t.Fatalf("the session cost the proxy %d accept(s), want exactly 1", got)
	}
	if got := doltpool.Len(); got != 0 {
		t.Fatalf("the session registered %d pool(s) in doltpool, want 0", got)
	}
	proxy.stop(t)
}
