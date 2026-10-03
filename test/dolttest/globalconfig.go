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
// network calls dolt makes from a fresh HOME. dolt takes no environment
// variable for either; the config file is the only switch.
//   - versioncheck.disabled: `dolt version` otherwise fetches the latest
//     release synchronously and waits up to 30 s for it, while the pinned bd
//     probes `dolt version` at init with a fixed 10 s timeout and kills it.
//   - metrics.disabled: `dolt status` and `dolt sql` otherwise send metrics.
const GlobalConfigJSON = `{"user.name":"gc-test","user.email":"gc-test@test.local","versioncheck.disabled":"true","metrics.disabled":"true"}`

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
