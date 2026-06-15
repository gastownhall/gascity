package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                fsys.OSFS{},
		Provider:          provider,
		BDContext:         preflightBDContextReader(cityPath),
		DatabaseProjectID: preflightDatabaseProjectIDReader(cityPath),
	}
}

func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	return func(scope string) (contract.PreflightBDContext, error) {
		scopeRoot := resolveStoreScopeRoot(cityPath, scope)
		cfg, _ := loadCityConfig(cityPath, io.Discard)
		envFn := func(_ string) (map[string]string, error) {
			if samePath(scopeRoot, cityPath) {
				env, err := bdRuntimeEnvWithError(cityPath)
				if env != nil {
					env["BEADS_DIR"] = filepath.Join(scopeRoot, ".beads")
				}
				return env, err
			}
			return bdRuntimeEnvForRigWithError(cityPath, cfg, scopeRoot)
		}
		workDir := preflightBDContextWorkDir(cityPath, scopeRoot, cfg)
		out, err := bdCommandRunnerWithManagedRetryErr(cityPath, envFn)(workDir, "bd", "context", "--json")
		if err != nil {
			return contract.PreflightBDContext{}, err
		}
		var raw struct {
			Backend       string `json:"backend"`
			DoltMode      string `json:"dolt_mode"`
			BDVersion     string `json:"bd_version"`
			SchemaVersion int    `json:"schema_version"`
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return contract.PreflightBDContext{}, fmt.Errorf("parse bd context --json: %w", err)
		}
		return contract.PreflightBDContext{
			Backend:       raw.Backend,
			DoltMode:      raw.DoltMode,
			BDVersion:     raw.BDVersion,
			SchemaVersion: raw.SchemaVersion,
		}, nil
	}
}

func preflightBDContextWorkDir(cityPath, scopeRoot string, cfg *config.City) string {
	if looksLikeGitWorktree(scopeRoot) {
		return scopeRoot
	}
	if samePath(scopeRoot, cityPath) && cfg != nil {
		for i := range cfg.Rigs {
			rigPath := resolveStoreScopeRoot(cityPath, cfg.Rigs[i].Path)
			if looksLikeGitWorktree(rigPath) {
				return rigPath
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil && looksLikeGitWorktree(cwd) {
		return cwd
	}
	return scopeRoot
}

func looksLikeGitWorktree(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

func preflightDatabaseProjectIDReader(cityPath string) func(scope string) (string, bool, error) {
	return func(scope string) (string, bool, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return "", false, err
		}
		db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		if err != nil {
			return "", false, err
		}
		defer db.Close() //nolint:errcheck // read-only best-effort close

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return "", false, err
		}
		return readDatabaseProjectID(ctx, db)
	}
}
