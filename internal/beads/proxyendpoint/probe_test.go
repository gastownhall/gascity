package proxyendpoint

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestClassifyProbeOutcomeTable pins the three-way split the whole error
// classification in the later slices hangs off.
//
// It is driven by the two facts a probe collects rather than by error text on
// purpose: go-sql-driver reports a server that hangs up before the greeting as
// ErrInvalidConn on one path and a bare io.EOF on another, and a classifier that
// keyed on either spelling would silently stop recognizing the zombie signature
// on a driver bump.
func TestClassifyProbeOutcomeTable(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	timedOut := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}

	cases := []struct {
		name       string
		sessionErr error
		dialErr    error
		want       ProbeOutcome
	}{
		{
			name: "a session that completed is served",
			want: ProbeServed,
		},
		{
			// A live proxy whose Dolt child is gone: the accept succeeds, so the
			// confirming dial succeeds too, and the session died on the wire.
			name:       "accepted then closed before the greeting",
			sessionErr: driver.ErrBadConn,
			want:       ProbeAcceptedNoGreeting,
		},
		{
			name:       "a bare EOF with a live listener is the same signature",
			sessionErr: io.EOF,
			want:       ProbeAcceptedNoGreeting,
		},
		{
			name:       "an unexpected EOF mid-handshake is the same signature",
			sessionErr: io.ErrUnexpectedEOF,
			want:       ProbeAcceptedNoGreeting,
		},
		{
			// Nothing is listening: both the session and the confirming dial are
			// refused.
			name:       "refused when the confirming dial is refused too",
			sessionErr: refused,
			dialErr:    refused,
			want:       ProbeRefused,
		},
		{
			name:       "a reset connection with nothing listening is refused",
			sessionErr: syscall.ECONNRESET,
			dialErr:    refused,
			want:       ProbeRefused,
		},
		{
			// An unknown database says nothing about the proxy in either
			// direction, and forcing it into a proxy state would be wrong twice.
			name:       "an unknown database is not a proxy verdict",
			sessionErr: errors.New("Error 1049 (42000): Unknown database 'beads'"),
			want:       ProbeUnknown,
		},
		{
			name:       "access denied is not a proxy verdict",
			sessionErr: errors.New("Error 1045 (28000): Access denied for user 'root'"),
			want:       ProbeUnknown,
		},
		{
			name:       "a canceled caller is not a proxy verdict",
			sessionErr: context.Canceled,
			want:       ProbeUnknown,
		},
		{
			// The regression this table exists for. The probe imposes its own
			// two-second budget, context.DeadlineExceeded satisfies net.Error,
			// and the confirming dial against a healthy-but-loaded proxy
			// SUCCEEDS — so classifying the deadline as connection-level
			// produced the zombie signature for a proxy that is serving bd,
			// and the escalation behind that signature is `bd dolt stop`.
			name:       "the probe's own expired budget is not the zombie signature",
			sessionErr: context.DeadlineExceeded,
			want:       ProbeUnknown,
		},
		{
			name:       "a deadline wrapped by a cursor read is still not a proxy verdict",
			sessionErr: fmt.Errorf("reading %s version: %w", cursorTableMain, context.DeadlineExceeded),
			want:       ProbeUnknown,
		},
		{
			// And when the loopback accept queue is momentarily full, the
			// confirming dial times out too. Nothing here says a listener is
			// absent, and "refused" on a live same-generation record names a
			// healthy proxy as draining.
			name:       "a deadline on both the session and the confirming dial is undetermined",
			sessionErr: context.DeadlineExceeded,
			dialErr:    timedOut,
			want:       ProbeUnknown,
		},
		{
			name:       "a real wire failure with a dial that only timed out is undetermined",
			sessionErr: io.EOF,
			dialErr:    timedOut,
			want:       ProbeUnknown,
		},
		{
			name:       "an i/o timeout mid-session is not the zombie signature either",
			sessionErr: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded},
			want:       ProbeUnknown,
		},
		{
			// A dial refused for a reason that is not the kernel refusing a
			// connection proves nothing about a listener.
			name:       "an unreachable host on the confirming dial is undetermined",
			sessionErr: io.EOF,
			dialErr:    &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH},
			want:       ProbeUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyProbe(tc.sessionErr, tc.dialErr); got != tc.want {
				t.Fatalf("ClassifyProbe(%v, %v) = %v, want %v", tc.sessionErr, tc.dialErr, got, tc.want)
			}
		})
	}
}

// TestProbeRunsTheSessionFirst pins the session cost: a healthy endpoint is one
// session and no confirming dial, because a probe against a busy proxy that
// opened two sessions would be a probe an operator cannot afford per admission.
func TestProbeRunsTheSessionFirst(t *testing.T) {
	dials := 0
	got := Probe(context.Background(), ProbeIO{
		Session: func(context.Context) (Cursors, error) { return Cursors{Main: 66, Ignored: 26}, nil },
		Dial:    func(context.Context) error { dials++; return nil },
	})
	if got.Outcome != ProbeServed {
		t.Fatalf("Probe outcome = %v, want served", got.Outcome)
	}
	if got.Cursors != (Cursors{Main: 66, Ignored: 26}) {
		t.Fatalf("Probe cursors = %v, want main=66 ignored=26", got.Cursors)
	}
	if dials != 0 {
		t.Fatalf("a served probe made %d confirming dial(s), want 0", dials)
	}
}

// TestProbeConfirmsWithOneDial pins that the disambiguating dial runs exactly
// once, and only for a connection-level failure.
func TestProbeConfirmsWithOneDial(t *testing.T) {
	t.Run("connection-level failure confirms", func(t *testing.T) {
		dials := 0
		got := Probe(context.Background(), ProbeIO{
			Session: func(context.Context) (Cursors, error) { return Cursors{}, io.EOF },
			Dial:    func(context.Context) error { dials++; return nil },
		})
		if got.Outcome != ProbeAcceptedNoGreeting {
			t.Fatalf("Probe outcome = %v, want accepted_no_greeting", got.Outcome)
		}
		if dials != 1 {
			t.Fatalf("confirming dials = %d, want 1", dials)
		}
		if !errors.Is(got.Err, io.EOF) {
			t.Fatalf("Probe dropped the session error: %v", got.Err)
		}
	})

	t.Run("a statement-level failure does not confirm", func(t *testing.T) {
		dials := 0
		got := Probe(context.Background(), ProbeIO{
			Session: func(context.Context) (Cursors, error) { return Cursors{}, errors.New("Error 1049: Unknown database") },
			Dial:    func(context.Context) error { dials++; return nil },
		})
		if got.Outcome != ProbeUnknown {
			t.Fatalf("Probe outcome = %v, want unknown", got.Outcome)
		}
		if dials != 0 {
			t.Fatalf("a statement-level failure made %d dial(s), want 0", dials)
		}
	})

	t.Run("no session is a refusal to guess", func(t *testing.T) {
		got := Probe(context.Background(), ProbeIO{})
		if got.Outcome != ProbeUnknown || got.Err == nil {
			t.Fatalf("Probe with no session = %v (%v), want unknown with an error", got.Outcome, got.Err)
		}
	})
}

func TestIsConnectionLevel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EOF", io.EOF, true},
		{"wrapped EOF", fmt.Errorf("handshake: %w", io.EOF), true},
		{"bad conn", driver.ErrBadConn, true},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"reset", syscall.ECONNRESET, true},
		{"broken pipe", syscall.EPIPE, true},
		{"a MySQL error is not about the connection", errors.New("Error 1146: Table doesn't exist"), false},
		{"a canceled context is the caller's, not the wire's", context.Canceled, false},
		// context.DeadlineExceeded implements net.Error, so it used to fall
		// into the net.Error arm and become a verdict about the proxy.
		{"the probe's own deadline is the probe's, not the wire's", context.DeadlineExceeded, false},
		{"a wrapped deadline is the same", fmt.Errorf("handshake: %w", context.DeadlineExceeded), false},
		{"an i/o timeout is a clock, not a peer", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"a socket deadline is the same", os.ErrDeadlineExceeded, false},
	}
	for _, tc := range cases {
		if got := IsConnectionLevel(tc.err); got != tc.want {
			t.Errorf("%s: IsConnectionLevel(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestIsIndeterminate pins the guard every other classification arm sits behind.
func TestIsIndeterminate(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"the session budget", context.DeadlineExceeded, true},
		{"a wrapped session budget", fmt.Errorf("probing %s existence: %w", cursorTableMain, context.DeadlineExceeded), true},
		{"a canceled caller", context.Canceled, true},
		{"a socket deadline", os.ErrDeadlineExceeded, true},
		{"a dial that timed out", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"a refused dial is a fact about the endpoint", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, false},
		{"an EOF is a fact about the endpoint", io.EOF, false},
		{"a MySQL error is a fact about the database", errors.New("Error 1049: Unknown database"), false},
	}
	for _, tc := range cases {
		if got := IsIndeterminate(tc.err); got != tc.want {
			t.Errorf("%s: IsIndeterminate(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestProbeNeverConcludesFromItsOwnDeadline drives the three places the probe's
// budget can expire through the real session code.
//
// The table above states the rule; this states that the rule is reached from
// where the deadline actually lands. Each case also asserts the probe spent no
// confirming dial: a deadline is not something a second connection can settle,
// and spending one would charge a loaded proxy for the probe's own impatience.
func TestProbeNeverConcludesFromItsOwnDeadline(t *testing.T) {
	cases := []struct {
		name  string
		build func() *fakeProbeConnector
	}{
		{
			name: "the dial",
			build: func() *fakeProbeConnector {
				return &fakeProbeConnector{connectErr: context.DeadlineExceeded}
			},
		},
		{
			name: "the greeting",
			build: func() *fakeProbeConnector {
				return &fakeProbeConnector{pingErr: context.DeadlineExceeded}
			},
		},
		{
			name: "a cursor query",
			build: func() *fakeProbeConnector {
				return &fakeProbeConnector{queryErr: context.DeadlineExceeded}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.build()
			dials := 0
			got := Probe(context.Background(), ProbeIO{
				Session: func(ctx context.Context) (Cursors, error) { return readCursorsOver(ctx, fake) },
				Dial:    func(context.Context) error { dials++; return nil },
			})
			if got.Outcome != ProbeUnknown {
				t.Fatalf("a deadline on %s produced %v, want unknown: a slow proxy is not a stopped one", tc.name, got.Outcome)
			}
			if dials != 0 {
				t.Fatalf("a deadline on %s spent %d confirming dial(s), want 0", tc.name, dials)
			}
			if opened, closed := fake.opened.Load(), fake.closed.Load(); opened != closed {
				t.Fatalf("a deadline on %s opened %d connection(s) and closed %d", tc.name, opened, closed)
			}
		})
	}
}

// TestProbeRefusesAnUnnamedDatabase pins the one input that would have produced
// a confident wrong answer.
//
// Both cursor reads are scoped by DATABASE(). With no database selected it is
// NULL, both existence probes count zero, both cursors read zero, and the probe
// would report `served main=0 ignored=0` — which the cursor gate reads as a
// database far behind the library on both lanes, with a schema_skew reason, for a
// database it never looked at. It costs the proxy no session either: the refusal
// happens before anything is dialed.
func TestProbeRefusesAnUnnamedDatabase(t *testing.T) {
	for _, database := range []string{"", "   "} {
		dials := 0
		probeIO := DefaultProbeIO(1, database)
		got := Probe(context.Background(), ProbeIO{
			Session: probeIO.Session,
			Dial:    func(context.Context) error { dials++; return nil },
		})
		if got.Outcome != ProbeUnknown {
			t.Fatalf("a probe with database %q reported %v, want unknown", database, got.Outcome)
		}
		if !errors.Is(got.Err, ErrNoDatabase) {
			t.Fatalf("a probe with database %q reported %v, want ErrNoDatabase", database, got.Err)
		}
		if dials != 0 {
			t.Fatalf("a probe with database %q spent %d dial(s), want 0", database, dials)
		}
	}
}

// TestProbeOutcomeTokensAreStable pins the strings automation reads.
func TestProbeOutcomeTokensAreStable(t *testing.T) {
	want := map[ProbeOutcome]string{
		ProbeUnknown:            "unknown",
		ProbeRefused:            "refused",
		ProbeAcceptedNoGreeting: "accepted_no_greeting",
		ProbeServed:             "served",
	}
	for outcome, token := range want {
		if got := outcome.String(); got != token {
			t.Errorf("ProbeOutcome(%d).String() = %q, want %q", outcome, got, token)
		}
	}
	if len(want) != int(ProbeServed)+1 {
		t.Fatalf("the probe outcome enum has %d values but %d are pinned", int(ProbeServed)+1, len(want))
	}
}

// TestDefaultProbeIODialRefusesAClosedPort exercises the real dial against a
// port nothing is listening on, so the production Dial is not untested code.
// It opens no listener of its own: the point is the absence of one.
func TestDefaultProbeIODialRefusesAClosedPort(t *testing.T) {
	// Port 1 on loopback needs no listener to be a valid negative: an
	// unprivileged process cannot have bound it, so the dial is refused.
	probeIO := DefaultProbeIO(1, "beads")
	ctx, cancel := context.WithTimeout(context.Background(), ProbeDialTimeout)
	defer cancel()
	if err := probeIO.Dial(ctx); err == nil {
		t.Fatal("dialing 127.0.0.1:1 succeeded; the refused arm of the probe is untested")
	} else if !IsConnectionLevel(err) {
		t.Fatalf("a refused dial produced %v, which the classifier does not recognize as connection-level", err)
	}
}
