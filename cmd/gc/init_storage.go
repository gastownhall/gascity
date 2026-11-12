package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// initStorageChoice is gc init's --storage: whether the new city keeps every
// storage class on its work ledger (single) or is born split, with the five
// infrastructure classes in one local SQLite store
// (docs/runbooks/split-storage-classes.md).
type initStorageChoice string

const (
	// initStorageUnset is no --storage: the city is born split where a split
	// city is admitted, and starts single-store elsewhere with the reason.
	initStorageUnset  initStorageChoice = ""
	initStorageSingle initStorageChoice = "single"
	initStorageSplit  initStorageChoice = "split"
)

// initSplitBindingName names the binding a born-split city's five
// infrastructure classes share.
const initSplitBindingName = "infra"

// initSplitBDMinVersion is the oldest bd release a split city admits
// (maintainer ruling 2026-11-04): a split request on an older release starts
// the city single-store with an upgrade hint (splitBDFloorNotice).
const initSplitBDMinVersion = "1.3.2"

// initSplitBDRemedy is how an operator whose bd keeps a split request
// single-store gets the split.
const initSplitBDRemedy = "To split it, upgrade bd to v" + initSplitBDMinVersion + " or later (https://github.com/gastownhall/beads/releases), then run gc storage migrate (docs/runbooks/split-storage-classes.md)."

// parseInitStorageChoice parses --storage. Empty is initStorageUnset.
func parseInitStorageChoice(raw string) (initStorageChoice, error) {
	switch choice := initStorageChoice(strings.TrimSpace(raw)); choice {
	case initStorageUnset, initStorageSingle, initStorageSplit:
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
// city shape it built at cityPath, and choice. It returns nil for a
// single-store city, and a notice for stderr when the city starts
// single-store although it was not asked to.
//
// A city is born split unless it asked for single-store, or a split city
// would not serve it: a remote session runtime, whose sessions refuse mail,
// nudges, sessions, orders and workflows on a split city until worker-site
// transport (CN-2) exists, or a bd that a split city's gc bd refuses. An
// explicit --storage=split on a remote session runtime is refused rather than
// quietly overridden.
func resolveInitStorage(cfg *config.City, cityPath string, choice initStorageChoice) (*config.StorageConfig, string, error) {
	if choice == initStorageSingle {
		return nil, "", nil
	}
	if runtime, remote := cityRunsRemoteSessions(cfg, cityPath); remote {
		if choice == initStorageSplit {
			return nil, "", fmt.Errorf("--storage=split: this city's session runtime %q runs sessions on remote hosts, and a split city's remote sessions refuse mail, nudges, sessions, orders and workflows until worker-site transport (CN-2) exists; use --storage=single", runtime)
		}
		return nil, fmt.Sprintf("gc init: this city's session runtime %q runs sessions on remote hosts, and a split city's remote sessions refuse mail, nudges, sessions, orders and workflows until worker-site transport (CN-2) exists; this city starts single-store. %s", runtime, initSplitLaterRemedy), nil
	}
	if providerUsesBdStoreContract(initWorkStoreProvider(cfg)) {
		if notice := splitBDFloorNotice(); notice != "" {
			return nil, notice, nil
		}
	}
	return bornSplitStorageConfig(), "", nil
}

// initFromSplitStorageNotice applies a plain gc init's split admission to the
// split a gc init --from source declares in its city.toml. It returns "" when
// the copy keeps the source's [storage], or the notice to print when the copy
// starts single-store instead because a split city would not serve it here: a
// remote session runtime, or a bd a split city's gc bd refuses
// (resolveInitStorage). The shipped examples carry the split, and copying it
// onto such a host would give a city whose every gc bd call is refused, where a
// plain gc init on the same host starts single-store and says why.
//
// A remote stage keeps the source's [storage] as it is: it stages the
// controller's city, so its storage is the controller's (F3).
func initFromSplitStorageNotice(fs fsys.FS, srcToml, cityPath string) (string, error) {
	if initFromStagesARemoteCity() {
		return "", nil
	}
	data, err := fs.ReadFile(srcToml)
	if err != nil {
		return "", nil // the copy and its own parse report this
	}
	src, err := config.Parse(data)
	if err != nil || !config.RelocatesAnyClass(src) {
		return "", nil
	}
	storage, notice, err := resolveInitStorage(src, cityPath, initStorageUnset)
	if err != nil {
		return "", err
	}
	if storage != nil {
		return "", nil
	}
	return notice, nil
}

// initSplitLaterRemedy is how a city that started single-store splits later.
const initSplitLaterRemedy = "To split it later, run gc storage migrate (docs/runbooks/split-storage-classes.md)."

// initStorageLayout names the storage layout of the city.toml at cityPath for
// gc init --json: "split" when it relocates any class off the work ledger,
// else "single". It returns "" when the city has no city.toml.
func initStorageLayout(cityPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the new city's city.toml: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return "", fmt.Errorf("parsing the new city's city.toml: %w", err)
	}
	if config.RelocatesAnyClass(cfg) {
		return string(initStorageSplit), nil
	}
	return string(initStorageSingle), nil
}

// splitBDFloorNotice judges the bd a new split city's gc bd would run, and
// returns "" to admit the split or the notice for a city that starts
// single-store instead.
//
// A split city's gc bd runs only a bd whose command tree matches a pinned
// inventory (bd_inventory_trust.go), so the bd is judged the same way: by its
// tree, never by its version string alone, because every Enterprise build
// reports 1.3.0 and an OSS build from main reports 1.3.2 with another tree. On
// top of that the floor holds OSS releases before 1.3.2 single-store (bd 1.3.1
// has 1.3.2's tree): a binary reporting an older version is admitted only as
// the pinned build its tree matched (the Enterprise 1.3.0 build). With no bd
// at all there is nothing to judge, and the dependency check reports it.
func splitBDFloorNotice() string {
	bdPath, ok := initSplitBDPath()
	if !ok {
		return ""
	}
	inv, err := initJudgeSplitBD(bdPath)
	if err != nil {
		return fmt.Sprintf("gc init: gc could not check the command tree of bd at %s (%v), and a split city's gc bd runs only a bd whose tree gc pins; this city starts single-store. %s", bdPath, err, initSplitBDRemedy)
	}
	version := ""
	if line, err := initRunVersion(bdPath); err == nil {
		version = parseDepVersionLine(line)
	}
	if inv == nil {
		return fmt.Sprintf("gc init: the command tree of bd%s at %s matches no pinned gc bd inventory (%s), so a split city's gc bd would refuse it; this city starts single-store. %s", versionLabel(version), bdPath, pinnedBdInventoryNames(), initSplitBDRemedy)
	}
	if version != "" && compareVersions(version, initSplitBDMinVersion) < 0 && compareVersions(version, inv.Version) != 0 {
		return fmt.Sprintf("gc init: bd v%s at %s is older than v%s, the oldest bd release a split city admits; this city starts single-store. %s", version, bdPath, initSplitBDMinVersion, initSplitBDRemedy)
	}
	return ""
}

// versionLabel renders a parsed bd version for a notice, or nothing when bd
// reported none gc could parse.
func versionLabel(version string) string {
	if version == "" {
		return ""
	}
	return " v" + version
}

// pinnedBdInventoryNames lists the pinned inventories, the bd builds a split
// city's gc bd runs.
func pinnedBdInventoryNames() string {
	invs := bdflags.Inventories()
	names := make([]string, 0, len(invs))
	for _, inv := range invs {
		names = append(names, inv.Name)
	}
	return strings.Join(names, ", ")
}

// initSplitBDPath is the bd a new city's gc bd will run: an ambient BD_BIN
// naming an absolute executable, which gc bd honors for a city whose
// workspace.env pins no bd (and gc init writes none), else bd on PATH. It
// reports false when there is none.
func initSplitBDPath() (string, bool) {
	if raw := strings.TrimSpace(os.Getenv("BD_BIN")); filepath.IsAbs(raw) {
		if path, err := exec.LookPath(raw); err == nil {
			return path, true
		}
	}
	path, err := exec.LookPath("bd")
	return path, err == nil
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

// initJudgeSplitBD returns the pinned inventory whose command tree the bd at
// bdPath matches, or nil when none does: gc bd's own verdict on a split city
// (resolveBdInventory). The new city has no .gc yet, and a refused init leaves
// nothing behind, so the walk runs and caches in a scratch directory that is
// removed afterwards. Tests replace it.
var initJudgeSplitBD = func(bdPath string) (*bdflags.Inventory, error) {
	scratch, err := os.MkdirTemp("", "gc-init-bd-")
	if err != nil {
		return nil, fmt.Errorf("creating a scratch directory to check bd: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of the scratch directory
	verdict, err := resolveBdInventory(scratch, bdPath, bdTrustOpts{})
	if err != nil {
		return nil, err
	}
	if verdict.Inventory == "" {
		return nil, nil
	}
	return bdflags.InventoryByName(verdict.Inventory), nil
}
