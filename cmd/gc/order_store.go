package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

type (
	orderStoreResolver  func(orders.Order) (beads.Store, error)
	orderStoresResolver func(orders.Order) ([]beads.Store, error)
)

func openCityOrderStore(stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

func openOrderStoreForOrder(cityPath string, cfg *config.City, a orders.Order, stderr io.Writer, cmdName string) (beads.Store, int) {
	target, err := resolveOrderStoreTarget(cityPath, cfg, a)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

func resolveOrderStoreTarget(cityPath string, cfg *config.City, a orders.Order) (execStoreTarget, error) {
	if strings.TrimSpace(a.Rig) == "" {
		prefix := ""
		if cfg != nil {
			prefix = config.EffectiveHQPrefix(cfg)
		}
		return execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: prefix}, nil
	}
	if cfg == nil {
		return execStoreTarget{}, fmt.Errorf("rig-scoped order %q requires city config", a.ScopedName())
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	rig, ok := rigByName(cfg, a.Rig)
	if !ok {
		return execStoreTarget{}, fmt.Errorf("rig %q not found in %s", a.Rig, filepath.Join(cityPath, "city.toml"))
	}
	if strings.TrimSpace(rig.Path) == "" {
		return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before dispatching rig-scoped orders", rig.Name, rig.Name)
	}
	return execStoreTarget{
		ScopeRoot: rig.Path,
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}, nil
}

func resolveOrderExecTarget(cityPath string, cfg *config.City, a orders.Order) (execStoreTarget, error) {
	return resolveOrderStoreTarget(cityPath, cfg, a)
}

func orderStoreTargetKey(target execStoreTarget) string {
	return target.ScopeKind + "\x00" + filepath.Clean(target.ScopeRoot)
}

func orderExecEnv(cityPath string, cfg *config.City, target execStoreTarget, a orders.Order) []string {
	var env map[string]string
	if target.ScopeKind == "rig" {
		env = bdRuntimeEnvForRig(cityPath, cfg, target.ScopeRoot)
	} else {
		env = bdRuntimeEnv(cityPath)
		env["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	env["GC_STORE_ROOT"] = target.ScopeRoot
	env["GC_STORE_SCOPE"] = target.ScopeKind
	env["GC_BEADS_PREFIX"] = target.Prefix
	if target.ScopeKind == "rig" {
		env["GC_RIG"] = target.RigName
		env["GC_RIG_ROOT"] = target.ScopeRoot
	} else {
		env["GC_RIG"] = ""
		env["GC_RIG_ROOT"] = ""
	}
	if a.Source != "" {
		env["ORDER_DIR"] = filepath.Dir(a.Source)
	}
	if a.FormulaLayer != "" {
		packDir := filepath.Dir(a.FormulaLayer)
		env["PACK_DIR"] = packDir
		env["GC_PACK_DIR"] = packDir

		packName := filepath.Base(packDir)
		if packName != "." && packName != string(filepath.Separator) {
			env["GC_PACK_NAME"] = packName
			env["GC_PACK_STATE_DIR"] = citylayout.PackStateDir(cityPath, packName)
		}
	}
	if a.Rig != "" && target.RigName == "" {
		env["GC_RIG"] = a.Rig
	}
	applyOrderExecCanonicalDoltEnv(cityPath, target.ScopeRoot, env)
	ensureProjectedDoltEnvExplicit(env)
	return mergeRuntimeEnv(nil, env)
}

func applyOrderExecCanonicalDoltEnv(cityPath, scopeRoot string, env map[string]string) {
	if env == nil {
		return
	}
	if strings.TrimSpace(scopeRoot) == "" {
		scopeRoot = cityPath
	}
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		if applyOrderExecManagedDoltFallback(cityPath, scopeRoot, env, err) {
			return
		}
		return
	}
	if !ok {
		return
	}
	applyCanonicalDoltTargetEnv(env, target)
	applyResolvedDoltAuthEnv(env, doltauth.AuthScopeRoot(cityPath, scopeRoot, target), strings.TrimSpace(target.User))
	if target.External {
		env["GC_DOLT_MANAGED_LOCAL"] = "0"
		clearManagedDoltRuntimeLayoutEnv(env, cityPath)
	} else {
		env["GC_DOLT_MANAGED_LOCAL"] = "1"
		applyManagedDoltRuntimeLayoutEnv(env, cityPath)
	}
	mirrorBeadsDoltEnv(env)
}

func applyOrderExecManagedDoltFallback(cityPath, scopeRoot string, env map[string]string, _ error) bool {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, "")
	if err != nil || resolved.Kind != contract.ScopeConfigAuthoritative {
		return false
	}
	switch resolved.State.EndpointOrigin {
	case contract.EndpointOriginManagedCity:
	case contract.EndpointOriginInheritedCity:
		cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
		if err != nil || cityResolved.Kind != contract.ScopeConfigAuthoritative || cityResolved.State.EndpointOrigin != contract.EndpointOriginManagedCity {
			return false
		}
	default:
		return false
	}

	layout, err := resolveManagedDoltOrderRuntimeLayout(cityPath, env)
	if err != nil {
		return false
	}
	delete(env, "GC_DOLT_HOST")
	if port := managedDoltPortForLayout(layout); port != "" {
		env["GC_DOLT_PORT"] = port
	} else {
		delete(env, "GC_DOLT_PORT")
	}
	env["GC_DOLT_MANAGED_LOCAL"] = "1"
	applyManagedDoltRuntimeLayoutEnv(env, cityPath)
	target := contract.DoltConnectionTarget{EndpointOrigin: resolved.State.EndpointOrigin}
	applyResolvedDoltAuthEnv(env, doltauth.AuthScopeRoot(cityPath, scopeRoot, target), strings.TrimSpace(resolved.State.DoltUser))
	mirrorBeadsDoltEnv(env)
	return true
}

func applyManagedDoltRuntimeLayoutEnv(env map[string]string, cityPath string) {
	layout, err := resolveManagedDoltOrderRuntimeLayout(cityPath, env)
	if err != nil {
		return
	}
	env["GC_DOLT_DATA_DIR"] = layout.DataDir
	env["GC_DOLT_LOG_FILE"] = layout.LogFile
	env["GC_DOLT_STATE_FILE"] = managedDoltOrderStateFile(layout)
	env["GC_DOLT_PID_FILE"] = layout.PIDFile
	env["GC_DOLT_LOCK_FILE"] = layout.LockFile
	env["GC_DOLT_CONFIG_FILE"] = layout.ConfigFile
}

func clearManagedDoltRuntimeLayoutEnv(env map[string]string, cityPath string) {
	root := normalizePathForCompare(filepath.Join(managedDoltOrderPackStateDir(cityPath, env), "external-target"))
	env["GC_DOLT_DATA_DIR"] = root
	env["GC_DOLT_LOG_FILE"] = filepath.Join(root, "dolt.log")
	env["GC_DOLT_STATE_FILE"] = filepath.Join(root, "dolt-state.json")
	env["GC_DOLT_PID_FILE"] = filepath.Join(root, "dolt.pid")
	env["GC_DOLT_LOCK_FILE"] = filepath.Join(root, "dolt.lock")
	env["GC_DOLT_CONFIG_FILE"] = filepath.Join(root, "dolt-config.yaml")
}

func resolveManagedDoltOrderRuntimeLayout(cityPath string, env map[string]string) (managedDoltRuntimeLayout, error) {
	cityPath = filepath.Clean(strings.TrimSpace(cityPath))
	if cityPath == "" || cityPath == "." {
		return managedDoltRuntimeLayout{}, fmt.Errorf("missing --city")
	}
	cityPath = normalizePathForCompare(cityPath)
	packStateDir := managedDoltOrderPackStateDir(cityPath, env)
	layout := managedDoltRuntimeLayout{
		PackStateDir: packStateDir,
		DataDir:      normalizePathForCompare(filepath.Join(cityPath, ".beads", "dolt")),
		LogFile:      normalizePathForCompare(filepath.Join(packStateDir, "dolt.log")),
		StateFile:    normalizePathForCompare(filepath.Join(packStateDir, "dolt-state.json")),
		PIDFile:      normalizePathForCompare(filepath.Join(packStateDir, "dolt.pid")),
		LockFile:     normalizePathForCompare(filepath.Join(packStateDir, "dolt.lock")),
		ConfigFile:   normalizePathForCompare(filepath.Join(packStateDir, "dolt-config.yaml")),
	}
	layout.DataDir = managedDoltOrderDataDir(cityPath, layout.DataDir)
	return layout, nil
}

func managedDoltOrderPackStateDir(cityPath string, env map[string]string) string {
	if runtimeDir := trustedAmbientCityRuntimeDir(cityPath); runtimeDir != "" {
		return normalizePathForCompare(filepath.Join(runtimeDir, "packs", "dolt"))
	}
	if env != nil {
		if runtimeDir := strings.TrimSpace(env["GC_CITY_RUNTIME_DIR"]); runtimeDir != "" {
			return normalizePathForCompare(filepath.Join(runtimeDir, "packs", "dolt"))
		}
	}
	return normalizePathForCompare(citylayout.PackStateDir(cityPath, "dolt"))
}

func managedDoltOrderDataDir(cityPath, fallback string) string {
	if dataDir := publishedManagedDoltDataDir(cityPath); dataDir != "" {
		return dataDir
	}
	if info, err := os.Stat(fallback); err == nil && info.IsDir() {
		return fallback
	}
	legacy := normalizePathForCompare(filepath.Join(cityPath, ".gc", "dolt-data"))
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return fallback
}

func publishedManagedDoltDataDir(cityPath string) string {
	packStateDir := managedDoltOrderPackStateDir(cityPath, nil)
	data, err := os.ReadFile(managedDoltOrderStateFile(managedDoltRuntimeLayout{
		PackStateDir: packStateDir,
	})) //nolint:gosec // path is derived from managed city layout
	if err != nil {
		return ""
	}
	var state doltRuntimeState
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	dataDir := strings.TrimSpace(state.DataDir)
	if dataDir == "" {
		return ""
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		return ""
	}
	if state.Running {
		if !validPublishedManagedDoltDataDirState(cityPath, state, dataDir) {
			return ""
		}
		return normalizePathForCompare(dataDir)
	}
	if !state.Running && managedDoltDefaultDataDirExists(cityPath, dataDir) {
		return ""
	}
	return normalizePathForCompare(dataDir)
}

func validPublishedManagedDoltDataDirState(cityPath string, state doltRuntimeState, dataDir string) bool {
	if !state.Running || state.Port <= 0 || state.PID <= 0 {
		return false
	}
	if !samePath(strings.TrimSpace(state.DataDir), dataDir) {
		return false
	}
	if !pidAlive(state.PID) || !doltPortReachable(strconv.Itoa(state.Port)) {
		return false
	}
	holderPID := findPortHolderPID(strconv.Itoa(state.Port))
	if holderPID > 0 {
		return holderPID == state.PID
	}
	layout := managedDoltOrderRuntimeLayoutForDataDir(cityPath, dataDir)
	owned, deleted := inspectManagedDoltOwnership(state.PID, layout)
	return owned && !deleted
}

func managedDoltOrderRuntimeLayoutForDataDir(cityPath, dataDir string) managedDoltRuntimeLayout {
	packStateDir := managedDoltOrderPackStateDir(cityPath, nil)
	return managedDoltRuntimeLayout{
		PackStateDir: packStateDir,
		DataDir:      normalizePathForCompare(dataDir),
		LogFile:      normalizePathForCompare(filepath.Join(packStateDir, "dolt.log")),
		StateFile:    normalizePathForCompare(filepath.Join(packStateDir, "dolt-state.json")),
		PIDFile:      normalizePathForCompare(filepath.Join(packStateDir, "dolt.pid")),
		LockFile:     normalizePathForCompare(filepath.Join(packStateDir, "dolt.lock")),
		ConfigFile:   normalizePathForCompare(filepath.Join(packStateDir, "dolt-config.yaml")),
	}
}

func managedDoltDefaultDataDirExists(cityPath, dataDir string) bool {
	for _, candidate := range []string{
		filepath.Join(cityPath, ".beads", "dolt"),
		filepath.Join(cityPath, ".gc", "dolt-data"),
	} {
		if samePath(candidate, dataDir) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func managedDoltOrderStateFile(layout managedDoltRuntimeLayout) string {
	return filepath.Join(layout.PackStateDir, "dolt-state.json")
}

func managedDoltPortForLayout(layout managedDoltRuntimeLayout) string {
	data, err := os.ReadFile(managedDoltOrderStateFile(layout)) //nolint:gosec // path is derived from managed city layout
	if err != nil {
		return ""
	}
	var state doltRuntimeState
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	if !validDoltRuntimeStateForLayout(state, layout) {
		return ""
	}
	return strconv.Itoa(state.Port)
}

func validDoltRuntimeStateForLayout(state doltRuntimeState, layout managedDoltRuntimeLayout) bool {
	if !state.Running || state.Port <= 0 || state.PID <= 0 {
		return false
	}
	if !samePath(strings.TrimSpace(state.DataDir), layout.DataDir) {
		return false
	}
	if !pidAlive(state.PID) || !doltPortReachable(strconv.Itoa(state.Port)) {
		return false
	}
	holderPID := findPortHolderPID(strconv.Itoa(state.Port))
	if holderPID > 0 && holderPID != state.PID {
		return false
	}
	owned, deleted := inspectManagedDoltOwnership(state.PID, layout)
	if deleted {
		return false
	}
	if holderPID == state.PID {
		return true
	}
	return owned
}

func cachedOrderStoresResolver(cityPath string, cfg *config.City) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.Store, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.Store{primary}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				return nil, err
			}
			out = append(out, legacy)
		}
		return out, nil
	}
}

func cachedOrderHistoryStoresResolver(cityPath string, cfg *config.City, stderr io.Writer) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.Store, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.Store{primary}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				fmt.Fprintf(stderr, "gc order history: legacy city fallback unavailable for %s: %v\n", a.ScopedName(), err) //nolint:errcheck
				return out, nil
			}
			out = append(out, legacy)
		}
		return out, nil
	}
}

func legacyOrderCityFallbackNeeded(cityPath string, target execStoreTarget) bool {
	return target.ScopeKind == "rig" && filepath.Clean(target.ScopeRoot) != filepath.Clean(cityPath)
}

func legacyOrderCityTarget(cityPath string, cfg *config.City) execStoreTarget {
	prefix := ""
	if cfg != nil {
		prefix = config.EffectiveHQPrefix(cfg)
	}
	return execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: prefix}
}
