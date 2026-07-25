package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

const beadsV110SchemaVersion = 53

var (
	preflightManagedDoltOpenDatabase = managedDoltOpenDatabase
	preflightDatabaseStateReaderFn   = preflightDatabaseStateReader
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                        fsys.OSFS{},
		Provider:                  provider,
		RequiredSchemaVersion:     beadsV110SchemaVersion,
		BDContext:                 preflightBDContextReader(cityPath),
		DatabaseState:             preflightDatabaseStateReaderFn(cityPath),
		DeferIdentityToNativeOpen: preflightIdentityDeferredReader(cityPath),
	}
}

func preflightDatabaseStateReader(cityPath string) func(scope string) (contract.PreflightDatabaseState, error) {
	return func(scope string) (contract.PreflightDatabaseState, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return contract.PreflightDatabaseState{}, err
		}
		db, err := preflightManagedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		if err != nil {
			return contract.PreflightDatabaseState{}, err
		}
		defer db.Close() //nolint:errcheck // read-only best-effort close

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var version sql.NullInt64
		if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
			return contract.PreflightDatabaseState{}, fmt.Errorf("read beads schema version: %w", err)
		}
		state := contract.PreflightDatabaseState{}
		if version.Valid && version.Int64 > 0 {
			state.SchemaVersion = int(version.Int64)
		}
		projectID, hasProjectID, err := readDatabaseProjectID(ctx, db)
		state.ProjectID = projectID
		state.HasProjectID = hasProjectID
		return state, err
	}
}

func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	return func(scope string) (contract.PreflightBDContext, error) {
		out, err := bdCommandRunnerForCity(cityPath)(scope, "bd", "context", "--json")
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

// preflightIdentityDeferredReader reports whether a scope resolves to an
// external Dolt endpoint (e.g. a hosted beads-gateway). The direct root/plaintext
// project_id probe cannot authenticate such endpoints, so when it comes back
// unconfirmed the identity check defers to beadslib's native-open verification
// (which authenticates via the credential command and refuses to connect on a
// _project_id mismatch) instead of degrading the scope off the native store.
func preflightIdentityDeferredReader(cityPath string) func(scope string) bool {
	return func(scope string) bool {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return false
		}
		return target.External
	}
}
