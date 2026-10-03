package dolttest

import (
	"fmt"
	"os"
	"path/filepath"
)

// GlobalConfigJSON is the dolt global configuration tests seed into the HOME or
// DOLT_ROOT_PATH under which a real dolt or bd child runs. It is the single
// source for that content: no test writes config_global.json itself, and
// internal/testenv's TestOnlyTheSharedHelperWritesDoltGlobalConfig fails one
// that does.
//
// Besides the author identity dolt needs to commit, it switches off the two
// network calls dolt makes from a fresh HOME:
//   - versioncheck.disabled: `dolt version` otherwise fetches the latest
//     release synchronously, and that call has no timeout of its own (30 s
//     behind a black-holed proxy, 60 s behind one that accepts and never
//     answers, unbounded after a handshake), while the pinned bd probes `dolt
//     version` at init with a fixed 10 s timeout and kills it.
//   - metrics.disabled: `dolt status` and `dolt sql` otherwise send metrics.
//
// metrics.disabled stops dolt sending metrics, not forking the process that
// would: see DisableEventFlushVar.
const GlobalConfigJSON = `{"user.name":"gc-test","user.email":"gc-test@test.local","versioncheck.disabled":"true","metrics.disabled":"true"}`

// DisableEventFlushVar names the environment variable that stops dolt forking a
// detached `dolt send-metrics` after every command, a fork GlobalConfigJSON's
// metrics.disabled does not prevent. dolt tests only for its presence, so
// DisableEventFlushValue is cosmetic.
//
// internal/testenv sets it once for every test process, so a child that
// inherits os.Environ() needs nothing more. A child whose environment is built
// from scratch must carry it itself.
const (
	DisableEventFlushVar   = "DOLT_DISABLE_EVENT_FLUSH"
	DisableEventFlushValue = "1"
)

// globalConfigFile is the file dolt reads its global configuration from,
// beneath <root>/.dolt.
const globalConfigFile = "config_global.json"

// UnroutableHTTPSProxy is a proxy address no route reaches: a connection to it
// hangs instead of failing fast, which is how a slow or firewalled network
// looks to a child process.
const UnroutableHTTPSProxy = "http://10.255.255.1:9"

// UnroutableHTTPSProxyEnv returns the environment entries that send every HTTPS
// request a child makes to UnroutableHTTPSProxy. Both spellings are set because
// tools differ on which they read, and NO_PROXY is cleared so an ambient bypass
// list cannot route a request around the black hole.
func UnroutableHTTPSProxyEnv() []string {
	return []string{
		"HTTPS_PROXY=" + UnroutableHTTPSProxy,
		"https_proxy=" + UnroutableHTTPSProxy,
		"NO_PROXY=",
		"no_proxy=",
	}
}

// WriteGlobalConfig seeds GlobalConfigJSON at <root>/.dolt/config_global.json,
// creating the .dolt directory. root is the directory dolt resolves as
// DOLT_ROOT_PATH, or HOME when that is unset.
func WriteGlobalConfig(root string) error {
	dir := filepath.Join(root, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating dolt config dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, globalConfigFile)
	if err := os.WriteFile(path, []byte(GlobalConfigJSON), 0o644); err != nil {
		return fmt.Errorf("writing dolt global config %q: %w", path, err)
	}
	return nil
}
