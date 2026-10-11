package main

import "github.com/gastownhall/gascity/internal/beads"

// registerBeadsBackends registers the remote beads backends gc links with the
// beads library registry. It is called once from mainExitCode, the composition
// root, before any store opens; there is no init() registration (see
// internal/beads/contract/backend_bundle.go). Repeat calls are no-ops.
//
// It also installs the per-scope remote credential lookup (city.toml [beads]
// credential, rigs.beads_credential): a lookup by city and scope, never a
// process-wide credential, so two cities in one supervisor each send their own.
func registerBeadsBackends() {
	beads.RegisterRemoteBackends("gc/" + version)
	beads.SetRemoteCredentialLookup(cityRemoteCredentialLookup)
}
