package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// initStorageChoice is gc init's --storage: whether the new city keeps every
// storage class on its work ledger (single) or is born split, with the five
// infrastructure classes in one local SQLite store
// (docs/runbooks/split-storage-classes.md).
type initStorageChoice string

const (
	initStorageSingle initStorageChoice = "single"
	initStorageSplit  initStorageChoice = "split"
)

// initStorageDefault is the choice when --storage is not given. New cities
// stay single-store until the split-by-default flip.
const initStorageDefault = initStorageSingle

// initSplitBindingName names the binding a born-split city's five
// infrastructure classes share.
const initSplitBindingName = "infra"

// initSplitBDMinVersion is the oldest bd a split city admits (maintainer
// ruling 2026-11-04): a split request on an older bd starts the city
// single-store with an upgrade hint.
const initSplitBDMinVersion = "1.3.2"

// parseInitStorageChoice parses --storage. Empty means the default.
func parseInitStorageChoice(raw string) (initStorageChoice, error) {
	switch choice := initStorageChoice(strings.TrimSpace(raw)); choice {
	case "":
		return initStorageDefault, nil
	case initStorageSingle, initStorageSplit:
		return choice, nil
	default:
		return "", fmt.Errorf("--storage %q: want %q or %q", raw, initStorageSingle, initStorageSplit)
	}
}

// initPrefixPattern is the shape --prefix accepts: an ASCII letter or digit,
// then letters, digits, '_' or '-'.
var initPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// parseInitPrefix parses --prefix. Empty means "derive it from the city name".
func parseInitPrefix(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	prefix := strings.TrimSpace(raw)
	if !initPrefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("--prefix %q: want letters, digits, '_' or '-', starting with a letter or digit", raw)
	}
	return prefix, nil
}

// bornSplitStorageConfig is the [storage] a born-split city carries: work
// stays on the work ledger, and graph, sessions, messaging, orders and nudges
// share one sqlite-beads binding under .gc/store, created on first start.
func bornSplitStorageConfig() *config.StorageConfig {
	return &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      "work",
			Graph:     initSplitBindingName,
			Sessions:  initSplitBindingName,
			Messaging: initSplitBindingName,
			Orders:    initSplitBindingName,
			Nudges:    initSplitBindingName,
		},
		Bindings: map[string]config.StorageBindingConfig{
			initSplitBindingName: {Provider: "sqlite-beads", Path: ".gc/store"},
		},
	}
}

// resolveInitStorage decides the [storage] a plain gc init writes for cfg, the
// city shape it built, and choice. It returns nil for a single-store city, and
// a notice for stderr when a split request starts the city single-store. It
// refuses a split on a remote session runtime: a split city's remote sessions
// refuse mail, nudges, sessions, orders and workflows until worker-site
// transport (CN-2) exists.
func resolveInitStorage(cfg *config.City, choice initStorageChoice) (*config.StorageConfig, string, error) {
	if choice != initStorageSplit {
		return nil, "", nil
	}
	if runtime, remote := cityRunsRemoteSessions(cfg); remote {
		return nil, "", fmt.Errorf("--storage=split: this city's session runtime %q runs sessions on remote hosts, and a split city's remote sessions refuse mail, nudges, sessions, orders and workflows until worker-site transport (CN-2) exists; use --storage=single", runtime)
	}
	if providerUsesBdStoreContract(initWorkStoreProvider(cfg)) {
		if version, ok := depMeetsMinVersion("bd", initSplitBDMinVersion); version != "" && !ok {
			return nil, fmt.Sprintf("gc init: bd v%s is older than v%s, the oldest bd a split city admits; this city starts single-store. To split it, upgrade bd (https://github.com/gastownhall/beads/releases), then run gc storage migrate (docs/runbooks/split-storage-classes.md).", version, initSplitBDMinVersion), nil
		}
	}
	return bornSplitStorageConfig(), "", nil
}

// initWorkStoreProvider is the beads provider the new city's work store will
// run: GC_BEADS, else the city shape's [beads] provider, else bd. It reads cfg
// rather than city.toml because init decides before it writes the file.
func initWorkStoreProvider(cfg *config.City) string {
	if provider := strings.TrimSpace(os.Getenv("GC_BEADS")); provider != "" {
		return provider
	}
	if cfg != nil {
		if provider := strings.TrimSpace(cfg.Beads.Provider); provider != "" {
			return provider
		}
	}
	return "bd"
}
