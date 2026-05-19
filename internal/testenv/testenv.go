// Package testenv scrubs the leak-vector GC_* env vars at test-binary init
// time so a leak from an agent session (e.g. GC_CITY pointing at a live city)
// cannot reach test code and corrupt that city. See PR #746 for the incident.
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
// (GC_FAST_UNIT, GC_DOLT_REAL_BINARY, GC_*_HELPER, ...) flow through
// untouched so opt-in test paths and helper-subprocess trampolines keep
// working.
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
package testenv

import (
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

// LeakVectorVars is the list of GC_* env vars that point at live-city paths
// or session identities. If any of these survive into a test process, the
// test can write to the live city or pose as a real session. Stripped
// unconditionally at package init except for names listed in PassthroughVar.
//
// Adding a new GC_* var that names a city-path or session identity? Add it
// here too. Test-gate vars (GC_FAST_UNIT, GC_DOLT_REAL_BINARY, ...) do NOT
// belong here — they're how tests opt into expensive paths.
var LeakVectorVars = []string{
	"GC_AGENT",
	"GC_ALIAS",
	"GC_CITY",
	"GC_CITY_PATH",
	"GC_CITY_ROOT",
	"GC_CITY_RUNTIME_DIR",
	"GC_CONTROL_DISPATCHER_TRACE_DEFAULT",
	"GC_DIR",
	"GC_HOME",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_TMUX_SESSION",
}

func init() {
	if !isGoTestBinary() {
		// Testscript subcommand mode (e.g. this binary was copied to
		// $PATH/bin/gc by testscript.Main). Testscript owns the child env
		// exactly — skip the scrub so env vars it sets reach the subcommand.
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
	for _, name := range LeakVectorVars {
		if !keep[name] {
			_ = os.Unsetenv(name)
		}
	}
}
