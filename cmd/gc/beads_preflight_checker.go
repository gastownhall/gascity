package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rollout"
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                         fsys.OSFS{},
		Provider:                   provider,
		BDContext:                  preflightBDContextReader(cityPath),
		DatabaseProjectID:          preflightDatabaseProjectIDReader(cityPath),
		DeferIdentityToNativeOpen:  preflightIdentityDeferredReader(cityPath),
		DatabaseSchemaCursors:      preflightDatabaseSchemaCursorsReader(cityPath),
		AllowSchemaBehindMigrate:   preflightAllowSchemaBehindMigrateReader(cityPath),
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
// preflightDatabaseSchemaCursorsReader so the CLAMPED ignored-lane value (M9,
// G4 re-review) is unit-testable without a database: Ignored must be
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

// preflightAllowSchemaBehindMigrateReader reports whether the scope has
// opted in to letting the linked beads library migrate its database forward
// when its schema is behind the library's ceiling (the
// beads.allow_schema_behind_migrate rollout gate). It is a thin per-city
// adapter over cityAllowSchemaBehindMigrate, the ONE resolver this decision
// has: see that function's doc comment for why the preflight gate and the
// native-open path must never carry two copies of it.
func preflightAllowSchemaBehindMigrateReader(cityPath string) func(scope string) bool {
	return func(string) bool {
		return cityAllowSchemaBehindMigrate(cityPath)
	}
}

// cityAllowSchemaBehindMigrate is the single resolver for the
// beads.allow_schema_behind_migrate rollout gate (internal/rollout), read
// through the city's own config plus its registered break-glass env override
// GC_BEADS_ALLOW_SCHEMA_BEHIND_MIGRATE. The env lookup checks the ambient
// process environment first, then the city's own workspace.env — the same
// source order and expansion workspacePinnedBdBinaryOptional (cmd/gc/bd_env.go)
// uses for BD_BIN, so a city.toml pin resolves consistently across gc's
// environment-sourced config. This composed lookup is passed to
// rollout.Resolve only as this call site's own ResolveOptions.LookupEnv; it
// does not change how any other rollout gate resolves.
//
// It is called from two places that must never disagree: the native-store
// preflight schema gate (via preflightAllowSchemaBehindMigrateReader, through
// contract.PreflightChecker.AllowSchemaBehindMigrate) and, wired onto
// beads.AllowSchemaBehindMigrateForScope in this file's init(), the direct
// native-open path's own BD_ALLOW_REMOTE_MIGRATE withhold
// (internal/beads/native_dolt_store.go). A preflight that declared a behind
// schema eligible on this opt-in, whose open then failed to honor the exact
// same opt-in, would hand bd's own RemoteMigrateGateError an eligibility
// preflight never warned about — so there is exactly one function that reads
// this decision, not a preflight copy and an open-path copy that can drift.
func cityAllowSchemaBehindMigrate(cityPath string) bool {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return false
	}
	lookup := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		env := expandEnvMap(cfg.Workspace.Env)
		v, ok := env[key]
		return v, ok
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{LookupEnv: lookup})
	if err != nil {
		return false
	}
	return flags.AllowSchemaBehindMigrate()
}

// init wires beads.AllowSchemaBehindMigrateForScope to cityAllowSchemaBehindMigrate
// exactly once, before any goroutine can race it, so the direct native-open
// path shares the preflight gate's resolver rather than its own ambient-only
// default (see cityAllowSchemaBehindMigrate's doc comment).
func init() {
	beads.AllowSchemaBehindMigrateForScope = cityAllowSchemaBehindMigrate
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
