package main

import (
	"context"
	"net"
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

// probeProviderExternalEndpoint is a stub that accepts every endpoint.
func probeProviderExternalEndpoint(_ context.Context, _ []string) error {
	return nil
}
