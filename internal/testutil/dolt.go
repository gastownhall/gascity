package testutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// DoltGlobalConfig is the dolt global config a test seeds under an isolated
// DOLT_ROOT_PATH. It carries the author identity gc's init preflight and
// `dolt commit` require, and keeps dolt off the network: metrics.disabled stops
// the usage-metrics call to eventsapi.dolthub.com, and versioncheck.disabled
// stops the latest-release lookup that every `dolt version` otherwise makes as a
// synchronous HTTPS fetch with no timeout of its own. The pinned bd probes
// `dolt version` at proxied init with a fixed 10 s timeout and kills it, so on a
// slow or firewalled network an unseeded version check fails an otherwise
// correct test.
//
// metrics.disabled does not stop dolt forking a detached `dolt send-metrics`
// after each command; only the DOLT_DISABLE_EVENT_FLUSH environment variable
// does, which internal/testenv sets for every go-test binary.
const DoltGlobalConfig = `{"user.name":"gc-test","user.email":"gc-test@test.local","metrics.disabled":"true","versioncheck.disabled":"true"}`

// SeedDoltGlobalConfig writes DoltGlobalConfig to root/.dolt/config_global.json,
// where dolt reads its global config when DOLT_ROOT_PATH (or, without it, HOME)
// is root.
func SeedDoltGlobalConfig(root string) error {
	dir := filepath.Join(root, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating dolt config dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config_global.json")
	if err := os.WriteFile(path, []byte(DoltGlobalConfig), 0o644); err != nil {
		return fmt.Errorf("seeding dolt global config %s: %w", path, err)
	}
	return nil
}

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
