package proxyendpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/doltpool"
)

// TestOpenSessionRequiresADatabaseAndPort: the session opener's argument
// contract fails closed before any dial — no database name, or no port, is the
// caller's bug, not a proxy state to probe.
func TestOpenSessionRequiresADatabaseAndPort(t *testing.T) {
	ctx := context.Background()
	if _, _, err := OpenSession(ctx, 13306, "  "); err == nil || !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("OpenSession with a blank database = %v, want ErrNoDatabase", err)
	}
	if _, _, err := OpenSession(ctx, 0, "beads"); err == nil {
		t.Fatal("OpenSession without a port returned no error, want one naming the port")
	}
	if _, _, err := OpenSession(ctx, -1, "beads"); err == nil {
		t.Fatal("OpenSession with a negative port returned no error, want one naming the port")
	}
}

// TestOpenSessionHandshakeFailureDialsOnce: a failed greeting is one dial, not
// database/sql's ErrBadConn retry ladder — the proxy sees exactly one accept,
// and nothing registers in the process-lifetime doltpool.
func TestOpenSessionHandshakeFailureDialsOnce(t *testing.T) {
	proxy := startProbeListener(t, listenerClosesAtOnce)
	ctx, cancel := context.WithTimeout(context.Background(), ProbeSessionTimeout)
	defer cancel()
	_, _, err := OpenSession(ctx, proxy.port, "beads")
	if err == nil {
		t.Fatal("OpenSession against a proxy that closes without a greeting returned no error")
	}
	if got := proxy.accepted.Load(); got != 1 {
		t.Fatalf("the failed session dialed %d time(s), want exactly 1: a redialing opener multiplies the proxy's accepts behind one caller-visible failure", got)
	}
	if got := doltpool.Len(); got != 0 {
		t.Fatalf("the session registered %d pool(s) in doltpool, want 0", got)
	}
	if got := doltpool.TotalOpenConns(); got != 0 {
		t.Fatalf("doltpool holds %d open connection(s), want 0", got)
	}
}

// TestOpenSessionCloseClosesTheSocketOnce: the close function is the whole
// point of the session — it closes the session's socket (observed server-side
// as the peer going away), it is idempotent, and it leaves no handle behind.
func TestOpenSessionCloseClosesTheSocketOnce(t *testing.T) {
	proxy := startProbeListener(t, listenerGreetsAndStays)
	ctx, cancel := context.WithTimeout(context.Background(), ProbeSessionTimeout)
	defer cancel()
	conn, closeSession, err := OpenSession(ctx, proxy.port, "beads")
	if err != nil {
		t.Fatalf("OpenSession against a greeting proxy: %v", err)
	}
	if conn == nil {
		t.Fatal("OpenSession returned no connection with no error")
	}
	if err := closeSession(); err != nil {
		t.Fatalf("closeSession: %v", err)
	}
	select {
	case <-proxy.gone:
	case <-time.After(ProbeSessionTimeout):
		t.Fatal("the session's socket was still open after closeSession: the proxy never saw the peer go away")
	}
	if got := proxy.accepted.Load(); got != 1 {
		t.Fatalf("the session dialed %d time(s), want exactly 1", got)
	}
	if err := closeSession(); err != nil {
		t.Fatalf("second closeSession = %v, want nil: the close function is idempotent", err)
	}
	if got := doltpool.Len(); got != 0 {
		t.Fatalf("the session registered %d pool(s) in doltpool, want 0", got)
	}
	if got := doltpool.TotalOpenConns(); got != 0 {
		t.Fatalf("doltpool holds %d open connection(s) after close, want 0", got)
	}
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
