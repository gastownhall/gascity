package main

import (
	"path/filepath"

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
// same root, and returns that proxy's PID. Such a record is proof that bd —
// not gc — owns the sql-server child, so the reaper must never kill it. This
// holds under
// test temp directories too: a real-bd lifecycle test running in t.TempDir()
// produces exactly this shape, and a concurrent cleanup used to reap it
// through the test-config-path allowlist.
//
// Ownership is proven by the recorded process still BEING bd's proxy for this
// root, not by where the root sits: bd resolves the root from
// BEADS_PROXIED_SERVER_ROOT_PATH, then the sidecar's root_path, and only then
// the default <scope>/.beads/dolt, so a directory-name check would unprotect
// every scope with an overridden root.
//
// Liveness alone is not that proof. bd leaves proxy.pid on disk when its proxy
// is SIGKILLed, and quarantines the record only on its next adoption in that
// root — which never happens for an abandoned test root. Once the recorded PID
// is reused by any other process, a bare liveness probe would protect the
// orphaned sql-server indefinitely, and a hand-written record naming PID 1
// would protect any server started with that --config (kill(1,0) returns
// EPERM, which reads as alive). So gc also requires the PID's argv to name the
// `db-proxy-child` verb for THIS root, which is what bd actually execs (beads
// internal/storage/dbproxy/proxy/endpoint.go).
//
// The verb and the root are the whole proof; argv[0]'s filename is not part of
// it. bd launches the child with os.Executable(), so argv[0] is whatever the
// operator's bd is called on disk, and a versioned pin
// (BD_BIN=/opt/beads/bd-1.3.0-rc.2) is a supported shape — bd_env.go accepts
// any absolute executable. Requiring the basename "bd" would unprotect every
// live proxy of a versioned install, which is the exact R4 case this helper
// exists for. A process that runs the db-proxy-child verb and names this root
// in --root is bd's proxy whatever its file is called.
//
// A rig that shares its city's proxy root (the migrate-proxied shape) needs no
// special handling here. The reaper only ever sees the sql-server's own argv,
// and that child is launched with --config <shared root>/config.yaml, so the
// root resolved below is the city's — which is exactly where the live
// proxy.pid sits. Mapping each scope to its own provider root
// (proxiedScopeProviderRoot) would answer a question the reaper never asks: it
// classifies processes, and a shared root has one process, not one per scope.
//
// Uncovered: a config_path override that puts config.yaml somewhere other than
// the proxy root — bd allows it, and gc then sees no proxy.pid sibling and
// falls through to the ordinary rules.
func bdOwnedProxyDoltConfig(configPath string) (int, bool) {
	if configPath == "" || filepath.Base(configPath) != bdProxyConfigFileName {
		return 0, false
	}
	root := filepath.Dir(filepath.Clean(configPath))
	record, err := proxyendpoint.Read(root)
	if err != nil {
		return 0, false
	}
	if record.Kind != proxyendpoint.RecordKind || record.PID <= 0 {
		return 0, false
	}
	if !bdProxyPIDAlive(record.PID) {
		return 0, false
	}
	argv, err := bdProxyProcessArgv(record.PID)
	if err != nil || !proxyendpoint.ArgvRunsChild(argv) || !proxyendpoint.ArgvNamesRoot(argv, root) {
		return 0, false
	}
	return record.PID, true
}
