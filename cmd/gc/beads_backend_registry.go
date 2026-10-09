package main

import "github.com/gastownhall/gascity/internal/beads"

// registerBeadsBackends registers the remote beads backends gc links with the
// beads library registry. It is called once from mainExitCode, the composition
// root, before any store opens; there is no init() registration (see
// internal/beads/contract/backend_bundle.go). Repeat calls are no-ops.
func registerBeadsBackends() {
	beads.RegisterRemoteBackends("gc/" + version)
}
