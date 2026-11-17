package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installProviderExternalDial swaps the probe's dialer for one test and
// returns a pointer to the last (network, address) it was asked to dial.
func installProviderExternalDial(t *testing.T, dial func(ctx context.Context, network, address string) (net.Conn, error)) *[2]string {
	t.Helper()
	var last [2]string
	prev := providerExternalDial
	providerExternalDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		last = [2]string{network, address}
		return dial(ctx, network, address)
	}
	t.Cleanup(func() { providerExternalDial = prev })
	return &last
}

func refusedProviderExternalDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("connect: connection refused")
}

// The probe reads the same keys the provider script's direct:external arm
// reads, so the probe and the init cannot disagree about the endpoint.
func TestProbeProviderExternalEndpointDialsTheScriptsEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name        string
		env         []string
		wantNetwork string
		wantAddress string
	}{
		{name: "tcp", env: []string{"GC_DOLT_HOST=db.example.test", "GC_DOLT_PORT=4406"}, wantNetwork: "tcp", wantAddress: "db.example.test:4406"},
		{name: "ipv6 host is bracketed", env: []string{"GC_DOLT_HOST=::1", "GC_DOLT_PORT=4406"}, wantNetwork: "tcp", wantAddress: "[::1]:4406"},
		{name: "socket", env: []string{"BEADS_DOLT_SERVER_SOCKET=/var/run/dolt/external.sock"}, wantNetwork: "unix", wantAddress: "/var/run/dolt/external.sock"},
		{
			name:        "socket wins over host and port, as in the script",
			env:         []string{"GC_DOLT_HOST=db.example.test", "GC_DOLT_PORT=4406", "BEADS_DOLT_SERVER_SOCKET=/var/run/dolt/external.sock"},
			wantNetwork: "unix", wantAddress: "/var/run/dolt/external.sock",
		},
		{
			name:        "BEADS_DOLT_SERVER_HOST is not what the script reads",
			env:         []string{"GC_DOLT_HOST=real.example.test", "GC_DOLT_PORT=4406", "BEADS_DOLT_SERVER_HOST=other.example.test", "BEADS_DOLT_SERVER_PORT=1"},
			wantNetwork: "tcp", wantAddress: "real.example.test:4406",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closed := false
			last := installProviderExternalDial(t, func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				_ = server.Close()
				return &closeRecordingConn{Conn: client, closed: &closed}, nil
			})
			if err := probeProviderExternalEndpoint(context.Background(), tt.env); err != nil {
				t.Fatalf("probe of an answering endpoint = %v, want nil", err)
			}
			if last[0] != tt.wantNetwork || last[1] != tt.wantAddress {
				t.Fatalf("dialed (%q, %q), want (%q, %q)", last[0], last[1], tt.wantNetwork, tt.wantAddress)
			}
			if !closed {
				t.Fatal("probe left its connection open")
			}
		})
	}
}

type closeRecordingConn struct {
	net.Conn
	closed *bool
}

func (c *closeRecordingConn) Close() error {
	*c.closed = true
	return c.Conn.Close()
}

func TestProbeProviderExternalEndpointRefusalNamesTheEndpointAndSaysNothingRan(t *testing.T) {
	for _, tt := range []struct {
		name     string
		env      []string
		wantName string
	}{
		{name: "tcp", env: []string{"GC_DOLT_HOST=db.example.test", "GC_DOLT_PORT=4406"}, wantName: "db.example.test:4406"},
		{name: "socket", env: []string{"BEADS_DOLT_SERVER_SOCKET=/var/run/dolt/external.sock"}, wantName: "/var/run/dolt/external.sock"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installProviderExternalDial(t, refusedProviderExternalDial)
			err := probeProviderExternalEndpoint(context.Background(), tt.env)
			if err == nil {
				t.Fatal("probe of a refusing endpoint = nil, want an error")
			}
			msg := err.Error()
			for _, want := range []string{tt.wantName, "not accepting connections", "bd init was not run", "nothing was written", "gc start", "connection refused"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, "bd dolt start") {
				t.Errorf("error %q suggests bd dolt start, which starts the shadow server by hand", msg)
			}
		})
	}
}

// A dead unix socket needs no listener to set up: a path nothing is bound to
// refuses a dial, so this one goes through the real dialer.
func TestProbeProviderExternalEndpointRealDialRefusesAnUnboundSocket(t *testing.T) {
	installProviderExternalDial(t, dialProviderExternalEndpoint)
	socket := filepath.Join(t.TempDir(), "absent.sock")
	err := probeProviderExternalEndpoint(context.Background(), []string{"BEADS_DOLT_SERVER_SOCKET=" + socket})
	if err == nil {
		t.Fatal("probe of an unbound socket = nil, want an error")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Fatalf("error %q does not name the socket %q", err, socket)
	}
}

// The wait is bounded by a named constant and runs on the context handed to
// the dialer, so a black-holed endpoint cannot hang the init.
func TestProbeProviderExternalEndpointBoundsTheAttempt(t *testing.T) {
	var remaining time.Duration
	installProviderExternalDial(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("dial context has no deadline")
			return nil, ctx.Err()
		}
		remaining = time.Until(deadline)
		return nil, context.DeadlineExceeded
	})
	err := probeProviderExternalEndpoint(context.Background(), []string{"GC_DOLT_HOST=db.example.test", "GC_DOLT_PORT=4406"})
	if err == nil {
		t.Fatal("probe that timed out = nil, want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe error %v does not wrap the dial error", err)
	}
	if providerExternalDialTimeout != 5*time.Second {
		t.Fatalf("providerExternalDialTimeout = %v, want 5s (verifyExternalDoltEndpoint's bound)", providerExternalDialTimeout)
	}
	if remaining <= 0 || remaining > providerExternalDialTimeout {
		t.Fatalf("dial deadline was %v away, want within (0, %v]", remaining, providerExternalDialTimeout)
	}
}

func TestProbeProviderExternalEndpointDialsOnce(t *testing.T) {
	calls := 0
	installProviderExternalDial(t, func(context.Context, string, string) (net.Conn, error) {
		calls++
		return nil, errors.New("refused")
	})
	_ = probeProviderExternalEndpoint(context.Background(), []string{"GC_DOLT_HOST=db.example.test", "GC_DOLT_PORT=4406"})
	if calls != 1 {
		t.Fatalf("dial attempts = %d, want exactly 1", calls)
	}
}

// With no endpoint at all there is nothing to dial; that is an error, not a
// silent pass that lets init run against a default.
func TestProbeProviderExternalEndpointRefusesAnEnvironmentWithNoEndpoint(t *testing.T) {
	installProviderExternalDial(t, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed with no endpoint configured")
		return nil, errors.New("unreachable")
	})
	if err := probeProviderExternalEndpoint(context.Background(), []string{"GC_DOLT_HOST=db.example.test"}); err == nil {
		t.Fatal("probe with no port = nil, want an error")
	}
	if err := probeProviderExternalEndpoint(context.Background(), nil); err == nil {
		t.Fatal("probe with an empty environment = nil, want an error")
	}
}
