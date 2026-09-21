package proxyendpoint

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
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
	}
	for _, tc := range cases {
		if got := IsConnectionLevel(tc.err); got != tc.want {
			t.Errorf("%s: IsConnectionLevel(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
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
