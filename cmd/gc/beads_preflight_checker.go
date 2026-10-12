package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout"
)

// newBeadsPreflightChecker builds the native-store preflight for a scope of
// the city at cityPath. cfg is that city's config when the caller has it in
// hand, or nil to have the opt-in read city.toml when it is needed.
func newBeadsPreflightChecker(cityPath, provider string, cfg *config.City) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                         fsys.OSFS{},
		Provider:                   provider,
		BDContext:                  preflightBDContextReader(cityPath),
		DatabaseProjectID:          preflightDatabaseProjectIDReader(cityPath),
		DeferIdentityToNativeOpen:  preflightIdentityDeferredReader(cityPath),
		DatabaseSchemaCursors:      preflightDatabaseSchemaCursorsReader(cityPath),
		AllowSchemaBehindMigrate:   preflightAllowSchemaBehindMigrateReader(cityPath, cfg),
		SchemaLatestIgnoredVersion: beads.SchemaCursorIgnored,
	}
}

// preflightBDContextReader parses `bd context --json` for the fields gc can
// actually trust from it: backend, dolt_mode and bd's own semver. It
// deliberately does NOT read the envelope's "schema_version" field into
// anything schema-related — that field is bd's JSON envelope format version
// (cmd/bd/output.go's JSONSchemaVersion, a constant stamped on every
// response, currently 1), not the database's migration cursor. The real
// schema signal comes from preflightDatabaseSchemaCursorsReader, which reads
// the database directly over SQL. See contract.PreflightBDContext's doc
// comment.
func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	return func(scope string) (contract.PreflightBDContext, error) {
		out, err := bdCommandRunnerForCity(cityPath)(scope, "bd", "context", "--json")
		if err != nil {
			return contract.PreflightBDContext{}, err
		}
		var raw struct {
			Backend   string `json:"backend"`
			DoltMode  string `json:"dolt_mode"`
			BDVersion string `json:"bd_version"`
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return contract.PreflightBDContext{}, fmt.Errorf("parse bd context --json: %w", err)
		}
		return contract.PreflightBDContext{
			Backend:   raw.Backend,
			DoltMode:  raw.DoltMode,
			BDVersion: raw.BDVersion,
		}, nil
	}
}

// preflightDatabaseSchemaCursorsReader reads the beads database's own
// migration cursors directly over SQL — the authoritative schema signal — by
// reusing the same connection-acquisition path as
// preflightDatabaseProjectIDReader and the proxied lane's mature cursor
// reader (internal/beads/proxyendpoint.ReadCursorReportOverConn).
func preflightDatabaseSchemaCursorsReader(cityPath string) func(scope string) (contract.PreflightSchemaCursors, bool, error) {
	return func(scope string) (contract.PreflightSchemaCursors, bool, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return contract.PreflightSchemaCursors{}, false, err
		}
		if target.DoltMode == "proxied-server" {
			// The proxied lane already gates its own schema compatibility
			// (internal/beads.CursorsMatchPinned) before gc ever opens the
			// linked library against it; there is no direct SQL endpoint for
			// this control-plane probe to read here.
			return contract.PreflightSchemaCursors{}, false, nil
		}
		// Pooled handle owned by internal/doltpool; do not Close.
		var db *sql.DB
		if target.Socket != "" {
			db, err = managedDoltOpenDatabaseSocket(target.Socket, target.User, target.Database)
		} else {
			db, err = managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		}
		if err != nil {
			return contract.PreflightSchemaCursors{}, false, err
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := db.Conn(ctx)
		if err != nil {
			return contract.PreflightSchemaCursors{}, false, err
		}
		defer conn.Close() //nolint:errcheck // best-effort release of a pooled handle
		if err := conn.PingContext(ctx); err != nil {
			return contract.PreflightSchemaCursors{}, false, err
		}
		report, err := proxyendpoint.ReadCursorReportOverConn(ctx, conn)
		if err != nil {
			return contract.PreflightSchemaCursors{}, false, err
		}
		return preflightSchemaCursorsFromReport(report), true, nil
	}
}

// preflightSchemaCursorsFromReport maps a raw proxyendpoint.CursorReport onto
// the schema gate's own contract.PreflightSchemaCursors. It is factored out of
// preflightDatabaseSchemaCursorsReader so the CLAMPED ignored-lane value is
// unit-testable without a database: Ignored must be
// report.Reality.EffectiveIgnored(report.Cursors.Ignored), the number the
// linked library's migrationSource.atLatest actually asks about, never the
// raw on-disk report.Cursors.Ignored the clamp exists to stop comparing (see
// CursorReality.EffectiveIgnored's doc comment).
func preflightSchemaCursorsFromReport(report proxyendpoint.CursorReport) contract.PreflightSchemaCursors {
	return contract.PreflightSchemaCursors{
		Main:           report.Cursors.Main,
		Ignored:        report.Reality.EffectiveIgnored(report.Cursors.Ignored),
		IgnoredChecked: report.Reality.Checked(),
	}
}

// preflightAllowSchemaBehindMigrateReader reports the city's
// beads.allow_schema_behind_migrate decision for every scope the preflight
// asks about; see cityAllowSchemaBehindMigrate.
func preflightAllowSchemaBehindMigrateReader(cityPath string, cfg *config.City) func(scope string) bool {
	return func(string) bool {
		return cityAllowSchemaBehindMigrate(cityPath, cfg)
	}
}

// cityAllowSchemaBehindMigrate resolves the beads.allow_schema_behind_migrate
// rollout gate (internal/rollout) for the city at cityPath: the city's config
// plus the registered break-glass override GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE,
// looked up in the ambient process environment first and then in the city's
// expanded workspace.env. The ambient value wins because the variable is a
// break-glass override: the operator's live environment outranks what the
// city has checked in.
//
// The opt-in belongs to the city, not to a store scope: a rig's scope root may
// sit outside the city's directory tree, so the decision is keyed by cityPath
// alone. The native-store preflight (preflightAllowSchemaBehindMigrateReader)
// and the native open env (nativeDoltOpenEnvForScopeContext, which hands the
// decision to the linked library as beads.BDAllowRemoteMigrateEnvKey) both
// decide through this function, so a behind schema the preflight passes on the
// opt-in is one the open lets the library migrate.
//
// cfg is the city's config when the caller has it in hand. When it is nil the
// gate is read from city.toml without the side effects of a full config load;
// only a city with no city.toml has no config opt-in. Any other config that
// cannot be read or resolved — including a city.toml whose include, fragment
// or imported pack is missing — leaves the opt-in off and is reported once per
// city and error.
func cityAllowSchemaBehindMigrate(cityPath string, cfg *config.City) bool {
	if cfg == nil {
		cityTOML := filepath.Join(cityPath, "city.toml")
		if _, err := os.Stat(cityTOML); errors.Is(err, os.ErrNotExist) {
			cfg = &config.City{}
		} else {
			loaded, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, cityTOML, skipRevisionSnapshot)
			if err != nil {
				warnAllowSchemaBehindMigrateUnresolved(cityPath, err)
				return false
			}
			cfg = loaded
		}
	}
	workspaceEnv := expandEnvMap(cfg.Workspace.Env)
	lookup := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := workspaceEnv[key]
		return v, ok
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{LookupEnv: lookup})
	if err != nil {
		warnAllowSchemaBehindMigrateUnresolved(cityPath, err)
		return false
	}
	return flags.AllowSchemaBehindMigrate()
}

// allowSchemaBehindMigrateWarnOut receives the unresolved-opt-in warning.
// Tests swap it to capture the line.
var allowSchemaBehindMigrateWarnOut io.Writer = os.Stderr

// allowSchemaBehindMigrateWarned dedupes the warning per city and error: the
// opt-in is resolved on every preflight and native open, and the operator
// needs the line once, not once per open.
var allowSchemaBehindMigrateWarned sync.Map

// warnAllowSchemaBehindMigrateUnresolved reports, once per process, city and
// error, that the city's beads.allow_schema_behind_migrate opt-in could not be
// resolved and is treated as off.
func warnAllowSchemaBehindMigrateUnresolved(cityPath string, err error) {
	key := cityPath + "\x00" + err.Error()
	if _, loaded := allowSchemaBehindMigrateWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	fmt.Fprintf(allowSchemaBehindMigrateWarnOut, "gc: warning: resolving %s for %s: %v; treating the opt-in as off\n", rollout.KeyBeadsAllowSchemaBehindMigrate, cityPath, err) //nolint:errcheck // best-effort stderr
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
		return target.External || target.DoltMode == "proxied-server"
	}
}

func preflightDatabaseProjectIDReader(cityPath string) func(scope string) (string, bool, error) {
	return func(scope string) (string, bool, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return "", false, err
		}
		if target.DoltMode == "proxied-server" {
			// Identity is verified by the provider-owned proxied connection;
			// there is no direct SQL endpoint to probe here.
			return "", false, nil
		}
		// Pooled handle owned by internal/doltpool; do not Close.
		var db *sql.DB
		if target.Socket != "" {
			db, err = managedDoltOpenDatabaseSocket(target.Socket, target.User, target.Database)
		} else {
			db, err = managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		}
		if err != nil {
			return "", false, err
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return "", false, err
		}
		return readDatabaseProjectID(ctx, db)
	}
}
