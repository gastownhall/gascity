package proxyendpoint

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// liveEndpointOn is the Endpoint a caller holds after Inspect proved the proxy
// live, pointed at a fixture listener's port.
func liveEndpointOn(port int) Endpoint {
	return Endpoint{Record: Record{Port: port}, Verdict: VerdictLive}
}

// TestWithSessionDialsNothingItIsNotEntitledTo pins the helper's refusals: every
// one of them is answered before a socket is opened, because a dial is the one
// act that can start a stopped proxied store (bd autostarts on connect).
func TestWithSessionDialsNothingItIsNotEntitledTo(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(context.Context, *sql.Conn) error {
		t.Fatal("fn ran for a session that was refused")
		return nil
	}
	cases := []struct {
		name     string
		ctx      context.Context
		verdict  Verdict
		database string
		budget   time.Duration
		fn       func(context.Context, *sql.Conn) error
		want     error
	}{
		{"zero verdict", context.Background(), VerdictUnknown, "beads", time.Second, never, ErrEndpointNotLive},
		{"no record", context.Background(), VerdictNoRecord, "beads", time.Second, never, ErrEndpointNotLive},
		{"dead", context.Background(), VerdictDead, "beads", time.Second, never, ErrEndpointNotLive},
		{"foreign process", context.Background(), VerdictForeignProcess, "beads", time.Second, never, ErrEndpointNotLive},
		{"birth mismatch", context.Background(), VerdictBirthMismatch, "beads", time.Second, never, ErrEndpointNotLive},
		{"undetermined", context.Background(), VerdictUndetermined, "beads", time.Second, never, ErrEndpointNotLive},
		{"empty database", context.Background(), VerdictLive, "", time.Second, never, ErrNoDatabase},
		{"blank database", context.Background(), VerdictLive, "  \t", time.Second, never, ErrNoDatabase},
		{"zero budget", context.Background(), VerdictLive, "beads", 0, never, ErrNoSessionBudget},
		{"negative budget", context.Background(), VerdictLive, "beads", -time.Second, never, ErrNoSessionBudget},
		{"canceled context", canceled, VerdictLive, "beads", time.Second, never, context.Canceled},
		{"nil fn", context.Background(), VerdictLive, "beads", time.Second, nil, ErrNoSessionFunc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxy := startProbeListener(t, listenerStaysSilent)
			ep := Endpoint{Record: Record{Port: proxy.port}, Verdict: tc.verdict}
			err := WithSession(tc.ctx, ep, tc.database, tc.budget, tc.fn)
			if !errors.Is(err, tc.want) {
				t.Fatalf("WithSession = %v, want %v", err, tc.want)
			}
			if got := proxy.acceptsBefore(t); got != 0 {
				t.Fatalf("a refused session cost the proxy %d accept(s), want 0", got)
			}
		})
	}
}

// TestWithSessionIsOneSessionOnARealSocket runs the helper over the real driver
// against a listener that completes the handshake and answers the ping: fn gets
// the pinned connection exactly once and the proxy is accepted exactly once.
func TestWithSessionIsOneSessionOnARealSocket(t *testing.T) {
	proxy := startProbeListener(t, listenerAnswersPingThenResets)
	errFromFn := errors.New("fn's own verdict")
	calls := 0
	err := WithSession(context.Background(), liveEndpointOn(proxy.port), "beads", ProbeSessionTimeout,
		func(_ context.Context, conn *sql.Conn) error {
			calls++
			if conn == nil {
				t.Error("fn received a nil connection")
			}
			return errFromFn
		})
	if !errors.Is(err, errFromFn) {
		t.Fatalf("WithSession = %v, want fn's own error returned as it was produced", err)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d time(s), want exactly 1", calls)
	}
	if got := proxy.acceptsBefore(t); got != 1 {
		t.Fatalf("the session cost the proxy %d accept(s), want exactly 1", got)
	}
}

// TestWithSessionRelaysAMissingDatabaseAsTheServerSpelledIt is the contract the
// doctor check's "database missing" verdict rests on: the login naming a
// database the server does not have fails with ERR 1049, and the caller can
// read that number off the returned error.
func TestWithSessionRelaysAMissingDatabaseAsTheServerSpelledIt(t *testing.T) {
	proxy := startProbeListener(t, listenerRejectsUnknownDatabase)
	err := WithSession(context.Background(), liveEndpointOn(proxy.port), "gone", ProbeSessionTimeout,
		func(context.Context, *sql.Conn) error {
			t.Error("fn ran although the login was rejected")
			return nil
		})
	var serverErr *mysql.MySQLError
	if !errors.As(err, &serverErr) {
		t.Fatalf("WithSession = %v, want a *mysql.MySQLError the caller can classify", err)
	}
	if serverErr.Number != 1049 {
		t.Fatalf("errno = %d, want 1049 (unknown database)", serverErr.Number)
	}
	if got := proxy.acceptsBefore(t); got != 1 {
		t.Fatalf("a rejected login cost the proxy %d accept(s), want exactly 1: it is never retried", got)
	}
}

// TestWithSessionBudgetEndsAProxyThatNeverGreets pins the budget: a proxy in
// front of a Dolt that has not answered ends the session on the caller's budget
// (not a driver timeout), costs one accept, and leaves no socket behind.
func TestWithSessionBudgetEndsAProxyThatNeverGreets(t *testing.T) {
	proxy := startProbeListener(t, listenerStaysSilent)
	start := time.Now()
	err := WithSession(context.Background(), liveEndpointOn(proxy.port), "beads", 250*time.Millisecond,
		func(context.Context, *sql.Conn) error {
			t.Error("fn ran against a proxy that never greeted")
			return nil
		})
	if err == nil {
		t.Fatal("a listener that never greets produced a successful session")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the session took %s against a 250ms budget", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithSession = %v, want the caller's budget (context deadline) to be what ended it", err)
	}
	select {
	case <-proxy.gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy still sees the session's connection after the helper returned")
	}
	if got := proxy.accepted.Load(); got != 1 {
		t.Fatalf("the session cost the proxy %d accept(s), want exactly 1", got)
	}
}

// TestWithSessionOverClosesTheSessionOnEveryPath fences the shared body both the
// cursor probe and WithSession run: whatever fn or the ping returns, the one
// session it opened is closed before it returns, and it is never re-dialed.
func TestWithSessionOverClosesTheSessionOnEveryPath(t *testing.T) {
	boom := errors.New("fn failed")
	cases := []struct {
		name      string
		fake      *fakeProbeConnector
		fn        func(context.Context, *sql.Conn) error
		want      error
		wantCalls int
	}{
		{"fn succeeds", &fakeProbeConnector{}, func(context.Context, *sql.Conn) error { return nil }, nil, 1},
		{"fn fails", &fakeProbeConnector{}, func(context.Context, *sql.Conn) error { return boom }, boom, 1},
		{"ping fails", &fakeProbeConnector{pingErr: boom}, func(context.Context, *sql.Conn) error { return nil }, boom, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := withSessionOver(context.Background(), tc.fake, func(ctx context.Context, conn *sql.Conn) error {
				calls++
				return tc.fn(ctx, conn)
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("withSessionOver = %v, want %v", err, tc.want)
			}
			if calls != tc.wantCalls {
				t.Fatalf("fn ran %d time(s), want %d", calls, tc.wantCalls)
			}
			if got := tc.fake.dials.Load(); got != 1 {
				t.Fatalf("dialed %d time(s), want exactly 1", got)
			}
			if opened, closed := tc.fake.opened.Load(), tc.fake.closed.Load(); opened != 1 || closed != 1 {
				t.Fatalf("opened %d and closed %d session(s), want 1 and 1", opened, closed)
			}
		})
	}
}
