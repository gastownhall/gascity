//go:build windows

package main

// sessionProviderRuntimeDefault is the session provider an unnamed selection
// resolves to at runtime. Windows cannot run tmux natively, so the portable
// empty city.toml selection resolves to herdr here.
const sessionProviderRuntimeDefault = "herdr"

// freshScopeTransportDefault is the beads transport a fresh provider-owned
// scope takes when nothing selects one. bd 1.3.0 cannot initialize a proxied
// store on Windows: it syncs the .beads directory after writing
// proxied_server_client_info.json, and Windows refuses to flush a directory
// ("Access is denied"). The direct transport initializes and runs natively.
const freshScopeTransportDefault = "direct"

// beadsTransportDefaultHelp is the --beads-transport help's account of the
// default.
const beadsTransportDefaultHelp = "Default direct on Windows, where bd cannot yet initialize a proxied store: bd owns the Dolt server (bd init --server), and gc stop stops it. proxied, the default elsewhere, has bd own the Dolt process and restart it on any bd read."
