// Package testenv scrubs leak-vector env vars at test-binary init time so a
// leak from an agent session (e.g. GC_CITY pointing at a live city, or
// GC_BEADS=bd pointing at a managed Dolt runtime) cannot reach test code and
// corrupt that city or spawn orphaned infrastructure. See PR #746 for the
// original city-env incident.
//
// Every real test directory in this repo must contain an untagged
// `testenv_import_test.go` that blank-imports this package:
//
//	import _ "github.com/gastownhall/gascity/internal/testenv"
//
// TestRequiresDedicatedTestenvImportFile in lint_test.go enforces that exact
// layout, rejects stale stubs, and rejects ad hoc imports elsewhere so
// build-tagged files cannot silently satisfy the lint while being excluded
// from the default test binary.
//
// The Makefile test targets and integration shard script also wrap `go test`
// in `env -i` so the same guarantee holds there. This package covers the
// direct-`go test` and IDE-runner paths at test-binary init time.
//
// Scope boundary: this scrub runs before test code, not before arbitrary
// production-package init order. TestNoLeakVectorReadsAtPackageInit enforces
// that non-test code does not read the leak-vector vars during package init or
// top-level var initialization, which keeps the direct-`go test` path safe.
//
// Scope: only the named LeakVectorVars below are scrubbed. Test-gate vars
// (GC_FAST_UNIT, GC_REAL_PROCESS_SIGNAL_TESTS, GC_DOLT_REAL_BINARY,
// GC_*_HELPER, ...) flow through untouched so opt-in test paths and
// helper-subprocess trampolines keep working.
//
// Passthrough: a parent that intentionally launches a helper subprocess
// with seeded leak-vector vars (e.g. workspacesvc's proxy_process tests,
// where proxy_process.go seeds GC_CITY/GC_CITY_PATH/GC_CITY_RUNTIME_DIR/
// GC_CONTROL_DISPATCHER_TRACE_DEFAULT into the child env) can set
// GC_TESTENV_PASSTHROUGH in the child env to a comma-separated list of
// leak-vector var names. init() preserves only those named vars and scrubs
// the rest. The passthrough var itself is always unset so the child cannot
// propagate the list further. Unlike a blanket bypass, every surviving GC_*
// must be explicitly declared.
//
// Testscript subcommand bypass: when the test binary is re-invoked via
// rogpeppe/go-internal/testscript's Main as a registered subcommand (e.g.
// `gc` or `bd`), os.Args[0] is the command name rather than `<pkg>.test`.
// In that mode init() skips the scrub so env vars the testscript has
// deliberately set (via its own `env FOO=bar` line) reach the subcommand.
// Testscript owns the child env fully, so there is no leak risk.
//
// Production Dolt port guard: a Dolt port var (BEADS_DOLT_SERVER_PORT or
// GC_DOLT_PORT) carrying ProdDoltPort that would survive into the process —
// passthrough-preserved in go-test mode, or any value in testscript
// subcommand mode — makes init() panic instead, unless the paired Dolt host
// var survives with a non-local value (3307 is Dolt's default port, so
// external-server fixtures like db.example.com:3307 are legitimate). Test
// debris (testrig, tt, my_db databases) inside the production Dolt store
// traced back to test clients reaching the local server on port 3307
// (ga-4c2ss6). For the rare legitimate case, set ProdDoltPortOptOutVar
// (GC_ALLOW_PROD_DOLT_PORT_IN_TESTS) to "1".
package testenv

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// isGoTestBinary reports whether the current process looks like a Go-built
// test binary. Go `go test` builds binaries named `<pkg>.test` (or
// `<pkg>.test.exe` on Windows) and invokes them directly or via `exec`. A
// testscript subcommand re-invocation renames the binary (e.g. to `gc`) so
// its os.Args[0] will not have the `.test` suffix.
func isGoTestBinary() bool {
	name := filepath.Base(os.Args[0])
	name = strings.TrimSuffix(name, ".exe")
	return strings.HasSuffix(name, ".test")
}

// PassthroughVar names an env var whose value is a comma-separated list of
// leak-vector GC_* var names to preserve through init()'s scrub. Vars not on
// the list are scrubbed as usual; the passthrough var itself is unset so the
// list does not flow onward to further subprocesses.
const PassthroughVar = "GC_TESTENV_PASSTHROUGH"

// LeakVectorVars is the list of env vars that point at live-city paths,
// session identities, bead stores, or Dolt runtimes. If any of these survive
// into a test process, the test can write to the live city, pose as a real
// session, or spawn orphaned test infrastructure. Stripped unconditionally at
// package init except for names listed in PassthroughVar.
//
// Adding a new env var that names a city path, session identity, bead store,
// or managed Dolt target? Add it here too. Test-gate vars (GC_FAST_UNIT,
// GC_REAL_PROCESS_SIGNAL_TESTS, GC_DOLT_REAL_BINARY, ...) do NOT belong here;
// they're how tests opt into expensive paths.
var LeakVectorVars = []string{
	"BEADS_DIR",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"DOLT_ROOT_PATH",
	"GC_AGENT",
	"GC_ALIAS",
	"GC_BEADS",
	"GC_BEADS_SCOPE_ROOT",
	"GC_BIN",
	"GC_CITY",
	"GC_CITY_PATH",
	"GC_CITY_ROOT",
	"GC_CITY_RUNTIME_DIR",
	"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
	"GC_DIR",
	"GC_DOLT",
	"GC_DOLT_HOST",
	"GC_DOLT_PASSWORD",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_HOME",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_TMUX_SESSION",
}

// ProdDoltPort is the well-known port of the production Dolt server on
// maintainer hosts. Nothing listens locally on 3307 in any gc-managed test
// city, so a test binary about to talk to a local Dolt on it can only
// corrupt the production store.
const ProdDoltPort = "3307"

// ProdDoltPortOptOutVar names the env var that disables the production
// Dolt-port guard. Set it to "1" for the rare legitimate case where a test
// process must deliberately target a local Dolt server on ProdDoltPort.
const ProdDoltPortOptOutVar = "GC_ALLOW_PROD_DOLT_PORT_IN_TESTS"

// doltPortVars maps each env var that selects a Dolt server port to the env
// var that selects the matching Dolt server host.
var doltPortVars = map[string]string{
	"BEADS_DOLT_SERVER_PORT": "BEADS_DOLT_SERVER_HOST",
	"GC_DOLT_PORT":           "GC_DOLT_HOST",
}

// isLocalDoltHost reports whether a Dolt host value targets the local
// machine: empty (clients default to localhost), "localhost", a loopback
// address, or an unspecified address.
func isLocalDoltHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// refuseProdDoltPort panics when a Dolt port var that will outlive init()
// points at the local production Dolt server. survives reports whether the
// named var survives the scrub: always in testscript subcommand mode,
// passthrough-listed only in go-test mode. A paired host var that survives
// with a non-local value disarms the guard for that pair — 3307 is Dolt's
// default port, so external-server fixtures use it legitimately.
// ProdDoltPortOptOutVar set to "1" disables the guard entirely.
func refuseProdDoltPort(survives func(name string) bool) {
	if os.Getenv(ProdDoltPortOptOutVar) == "1" {
		return
	}
	for portVar, hostVar := range doltPortVars {
		if !survives(portVar) {
			continue
		}
		if strings.TrimSpace(os.Getenv(portVar)) != ProdDoltPort {
			continue
		}
		host := ""
		if survives(hostVar) {
			host = os.Getenv(hostVar)
		}
		if !isLocalDoltHost(host) {
			continue
		}
		panic("testenv: " + portVar + "=" + ProdDoltPort + " with a local Dolt host points at the production Dolt server; refusing to run tests against it (set " + ProdDoltPortOptOutVar + "=1 to deliberately allow it)")
	}
}

func init() {
	if !isGoTestBinary() {
		// Testscript subcommand mode (e.g. this binary was copied to
		// $PATH/bin/gc by testscript.Main). Testscript owns the child env
		// exactly — skip the scrub so env vars it sets reach the subcommand.
		// Every var survives here, so the prod-Dolt-port guard checks all of
		// them: a testscript-driven gc/bd must never write to the production
		// store either.
		refuseProdDoltPort(func(string) bool { return true })
		return
	}
	keep := map[string]bool{}
	if list := os.Getenv(PassthroughVar); list != "" {
		for _, name := range strings.Split(list, ",") {
			if name = strings.TrimSpace(name); name != "" {
				keep[name] = true
			}
		}
	}
	_ = os.Unsetenv(PassthroughVar)
	refuseProdDoltPort(func(name string) bool { return keep[name] })
	for _, name := range LeakVectorVars {
		if !keep[name] {
			_ = os.Unsetenv(name)
		}
	}
}
