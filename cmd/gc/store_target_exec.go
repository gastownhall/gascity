package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

type execStoreTarget struct {
	ScopeRoot string
	ScopeKind string
	Prefix    string
	RigName   string
}

var execProjectedDoltEnvKeys = []string{
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_PASSWORD",
}

func setExecProjectedDoltEnvEmpty(env map[string]string) {
	for _, key := range execProjectedDoltEnvKeys {
		env[key] = ""
	}
}

func copyExecProjectedDoltEnv(dst, src map[string]string) {
	for _, key := range execProjectedDoltEnvKeys {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

func gcExecStoreEnv(cityPath string, target execStoreTarget, provider string) map[string]string {
	env := cityRuntimeEnvMapForCity(cityPath)
	env["GC_PROVIDER"] = provider
	env["GC_STORE_ROOT"] = target.ScopeRoot
	env["GC_STORE_SCOPE"] = target.ScopeKind
	env["GC_BEADS_PREFIX"] = target.Prefix
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	setExecProjectedDoltEnvEmpty(env)
	env["BEADS_DIR"] = ""
	env["BEADS_DOLT_AUTO_START"] = ""
	env["GC_BIN"] = ""
	if execProviderUsesCanonicalBdScopeFiles(provider) {
		if gcBin := resolveProviderLifecycleGCBinary(); gcBin != "" {
			env["GC_BIN"] = gcBin
		}
	}
	if target.ScopeKind == "rig" {
		env["GC_RIG"] = target.RigName
		env["GC_RIG_ROOT"] = target.ScopeRoot
	}
	return env
}

func gcExecLifecycleInitProcessEnv(cityPath string, target execStoreTarget, provider string) ([]string, error) {
	env := gcExecStoreEnv(cityPath, target, provider)
	if !execProviderNeedsScopedDoltInit(provider) {
		return mergeRuntimeEnv(os.Environ(), env), nil
	}
	if target.ScopeKind == "rig" {
		cfg, err := loadCityConfig(cityPath, io.Discard)
		if err != nil {
			return nil, err
		}
		copyExecProjectedDoltEnv(env, bdRuntimeEnvForRig(cityPath, cfg, target.ScopeRoot))
	} else {
		copyExecProjectedDoltEnv(env, bdRuntimeEnv(cityPath))
	}
	return mergeRuntimeEnv(os.Environ(), env), nil
}

// execProviderBase returns the normalized base name of an exec: provider's
// script, with the .sh extension stripped so callers can match by logical
// name regardless of whether the script file on disk uses .sh.
func execProviderBase(provider string) string {
	script := strings.TrimSpace(strings.TrimPrefix(provider, "exec:"))
	return strings.TrimSuffix(filepath.Base(script), ".sh")
}

func execProviderNeedsScopedDoltInit(provider string) bool {
	return execProviderBase(provider) == "gc-beads-k8s"
}

func execProviderUsesCanonicalBdScopeFiles(provider string) bool {
	return execProviderBase(provider) == "gc-beads-bd"
}

func execProviderNeedsScopedDoltStoreEnv(provider string) bool {
	return execProviderUsesCanonicalBdScopeFiles(provider)
}

func resolveConfiguredExecStoreTarget(cityPath, storePath string) (execStoreTarget, error) {
	scopeRoot := resolveStoreScopeRoot(cityPath, storePath)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return execStoreTarget{}, err
	}
	if samePath(scopeRoot, cityPath) {
		return execStoreTarget{
			ScopeRoot: scopeRoot,
			ScopeKind: "city",
			Prefix:    config.EffectiveHQPrefix(cfg),
		}, nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for i := range cfg.Rigs {
		if samePath(cfg.Rigs[i].Path, scopeRoot) {
			return execStoreTarget{
				ScopeRoot: scopeRoot,
				ScopeKind: "rig",
				Prefix:    cfg.Rigs[i].EffectivePrefix(),
				RigName:   cfg.Rigs[i].Name,
			}, nil
		}
	}
	return execStoreTarget{}, fmt.Errorf("scope %q is not declared in %s", scopeRoot, filepath.Join(cityPath, "city.toml"))
}
