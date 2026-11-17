package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// providerExternalDialTimeout bounds the one reachability attempt made before
// a direct-external provider-owned init.
const providerExternalDialTimeout = 5 * time.Second

// providerExternalDial is the dialer probeProviderExternalEndpoint uses.
// Tests replace it so they never wait on a real network.
var providerExternalDial = dialProviderExternalEndpoint

// dialProviderExternalEndpoint opens, and the caller closes, one connection to
// address on network.
func dialProviderExternalEndpoint(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

// probeProviderExternalEndpoint dials, once, the endpoint the provider script's
// direct:external arm will hand to bd init: BEADS_DOLT_SERVER_SOCKET when set,
// otherwise GC_DOLT_HOST:GC_DOLT_PORT. It only opens and closes a connection; it
// sends no query and checks no identity or credentials. A refusal names the
// endpoint and says bd init was not run, so a dead external server stops the
// init before bd can leave a half-initialized .beads/dolt behind.
func probeProviderExternalEndpoint(ctx context.Context, environ []string) error {
	env := runtimeEnvEntriesToMap(environ)
	network, address := "tcp", ""
	if socket := strings.TrimSpace(env["BEADS_DOLT_SERVER_SOCKET"]); socket != "" {
		network, address = "unix", socket
	} else {
		host := strings.TrimSpace(env["GC_DOLT_HOST"])
		port := strings.TrimSpace(env["GC_DOLT_PORT"])
		if host == "" || port == "" {
			return fmt.Errorf("external dolt endpoint is not configured (GC_DOLT_HOST=%q, GC_DOLT_PORT=%q, no BEADS_DOLT_SERVER_SOCKET): bd init was not run and nothing was written", host, port)
		}
		address = net.JoinHostPort(host, port)
	}
	ctx, cancel := context.WithTimeout(ctx, providerExternalDialTimeout)
	defer cancel()
	conn, err := providerExternalDial(ctx, network, address)
	if err != nil {
		return fmt.Errorf("external dolt endpoint %s is not accepting connections: %w; bd init was not run and nothing was written; start the server, then rerun gc start", address, err)
	}
	_ = conn.Close()
	return nil
}
