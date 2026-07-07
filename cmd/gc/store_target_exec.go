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

func execProjectedBackendEnvKeys() []string {
	keys := make([]string, 0, len(projectedBeadsBackendEnvKeys)+len(projectedDoltEnvKeys)+len(projectedPostgresEnvKeys)+len(bdCLIRemoteSyncOptOutEnvKeys)+len(bdAutoBackupOptOutEnvKeys))
	keys = append(keys, projectedBeadsBackendEnvKeys...)
	keys = append(keys, projectedDoltEnvKeys...)
	keys = append(keys, projectedPostgresEnvKeys...)
	keys = appendBdCLIRemoteSyncOptOutEnvKeys(keys)
	keys = appendBdAutoBackupOptOutEnvKeys(keys)
	keys = appendBdContributorRoutingOptOutEnvKeys(keys)
	return keys
}

func setExecProjectedBackendEnvEmpty(env map[string]string) {
	for _, key := range execProjectedBackendEnvKeys() {
		env[key] = ""
	}
	applyBdCLIRemoteSyncOptOut(env)
	applyBdAutoBackupOptOut(env)
	applyBdContributorRoutingOptOut(env)
}

// execProjectedBackendCopyKeys returns the keys carried when projecting a
// resolved backend env map onto an exec-provider / city process env. It extends
// execProjectedBackendEnvKeys with BEADS_DOLT_CREDENTIAL_COMMAND, the hosted
// beads-gateway credential helper mirrorBeadsDoltEnv derives from
// GC_DOLT_CRED_CMD.
//
// That key is intentionally NOT in projectedDoltEnvKeys: it is a
// preserve-from-ambient passthrough key (hostedBeadsCredentialPassthroughKeys),
// not a strip-and-reproject key. Forcing it into projectedDoltEnvKeys would
// break the mergeRuntimeEnv strip symmetry TestProjectedKeysCoverage pins and
// collide with preserveHostedBeadsCredentialEnv. But the whitelist COPY paths
// must still carry the derived value, or a controller that exports only the
// non-sensitive GC_DOLT_CRED_CMD loses the helper on projected exec/process
// envs and bd falls back to the root user (gateway Error 1045). The empty-set
// path (setExecProjectedBackendEnvEmpty) deliberately keeps the original key
// set so it never blanks an ambient credential command for providers that skip
// the copy.
func execProjectedBackendCopyKeys() []string {
	return append(execProjectedBackendEnvKeys(), "BEADS_DOLT_CREDENTIAL_COMMAND")
}

func copyExecProjectedBackendEnv(dst, src map[string]string) {
	for _, key := range execProjectedBackendCopyKeys() {
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
	setExecProjectedBackendEnvEmpty(env)
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
		projected, err := bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
		if err != nil {
			return nil, err
		}
		copyExecProjectedBackendEnv(env, projected)
	} else {
		projected, err := bdRuntimeEnvWithError(cityPath)
		if err != nil {
			return nil, err
		}
		copyExecProjectedBackendEnv(env, projected)
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
