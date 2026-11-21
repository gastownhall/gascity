package main

import (
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// bd's proxy root layout, as written by `bd init --proxied-server`:
// <root>/{config.yaml,proxy.pid,...}, where root defaults to
// <scope>/.beads/dolt but follows BEADS_PROXIED_SERVER_ROOT_PATH or the
// sidecar's root_path when either is set. The sql-server child is launched
// with --config pointing at that config.yaml, and proxy.pid is the proxy's own
// liveness record.
//
// The layout, the record schema and the supervisor's argv shape are read by
// internal/beads/proxyendpoint, gc's one reader of bd's proxy files. The reaper
// held the only copy of that parser and now shares it.
//
// What the reaper deliberately does NOT share is the question.
// proxyendpoint.Validate answers "may gc dial this endpoint", which also
// demands a schema-2 record, a birth token and a matching root identity. The
// reaper asks the narrower "is this process bd's, so that reaping it would
// reap bd's proxy" — and that answer has to stay yes for a record too old or
// too sparse to dial, because a process gc cannot identify well enough to talk
// to is still a process gc must not kill.
//
// That is also why it reads the record through proxyendpoint.ReadOwnership
// rather than Read: encoding/json fails a whole decode on a type mismatch in
// ANY field, so a proxy.pid from a newer bd — birth promoted to an object,
// schema written as a string — would read as "no record" through the strict
// decoder and unprotect a live proxy over a field the reaper never consults.
const bdProxyConfigFileName = proxyendpoint.ConfigFileName

// bdProxyPIDAlive and bdProxyProcessArgv are the process-table reads that
// decide whether a proxy.pid record still describes bd's proxy. Both are
// injectable so reaper tests can drive every branch from a fabricated process
// table.
var (
	bdProxyPIDAlive    = pidutil.Alive
	bdProxyProcessArgv = pidutil.Cmdline
)

// bdOwnedProxyDoltConfig reports whether configPath is the sql-server config
// of a bd proxy root whose proxy.pid names a live `bd db-proxy-child` for that
// same root, and returns that proxy's PID (proxyendpoint.ConfigOwner, read
// through the reaper's injectable process-table reads). Such a record is proof
// that bd — not gc — owns the sql-server child, so the reaper must never kill
// it. This holds under test temp directories too: a real-bd lifecycle test
// running in t.TempDir() produces exactly this shape, and a concurrent cleanup
// used to reap it through the test-config-path allowlist.
//
// A rig that shares its city's proxy root (the migrate-proxied shape) needs no
// special handling here. The reaper only ever sees the sql-server's own argv,
// and that child is launched with --config <shared root>/config.yaml, so the
// root resolved is the city's — which is exactly where the live proxy.pid
// sits. Mapping each scope to its own provider root (proxiedScopeProviderRoot)
// would answer a question the reaper never asks: it classifies processes, and
// a shared root has one process, not one per scope.
func bdOwnedProxyDoltConfig(configPath string) (int, bool) {
	return proxyendpoint.ConfigOwner(configPath, proxyendpoint.ProcessTable{
		Alive: bdProxyPIDAlive,
		Argv:  bdProxyProcessArgv,
	})
}
