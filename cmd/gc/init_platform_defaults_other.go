//go:build !windows

package main

// sessionProviderRuntimeDefault is the session provider an unnamed selection
// resolves to at runtime. Unix keeps the empty selection so the registry
// fallback resolves to tmux.
const sessionProviderRuntimeDefault = ""

// freshScopeTransportDefault is the beads transport a fresh provider-owned
// scope takes when nothing selects one.
const freshScopeTransportDefault = "proxied"

// beadsTransportDefaultHelp is the --beads-transport help's account of the
// default.
const beadsTransportDefaultHelp = "Default proxied: bd owns the Dolt process, any bd read restarts it, and gc stop stops it. direct is the escape hatch and is also bd-owned (bd init --server), not the legacy gc-managed server."
