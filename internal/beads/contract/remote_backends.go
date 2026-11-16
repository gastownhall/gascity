package contract

import "strings"

// remoteBackends are the metadata backends whose bd answers every read by
// calling the workspace's server instead of querying a database itself: bd's
// HTTP store (internal/httpclient) sends each read as a request to a server
// that other clients share. Every SQL backend opens its database directly.
var remoteBackends = map[BackendName]struct{}{
	"http": {},
}

// BackendServesRemotely reports whether bd answers reads against a workspace
// whose .beads/metadata.json names backend by calling a shared server, one
// request per read, so a caller that repeats reads spends that server's
// capacity. Unknown and unset backends report false.
func BackendServesRemotely(backend string) bool {
	_, ok := remoteBackends[BackendName(strings.TrimSpace(backend))]
	return ok
}
