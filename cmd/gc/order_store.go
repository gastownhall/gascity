package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
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
	return mergeRuntimeEnv(nil, env)
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
