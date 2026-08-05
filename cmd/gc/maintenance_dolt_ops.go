package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storehealth"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// maintenanceStoreSizeFunc returns a probe that measures the on-disk size
// of the city's managed Dolt store. It feeds the store-maintenance size
// gate (skip CALL DOLT_GC below MinStoreMB) and the before/after byte
// deltas recorded on each run. A missing store reads as 0 bytes.
func maintenanceStoreSizeFunc(cityPath string) func() int64 {
	return func() int64 {
		return storehealth.WalkSize(storehealth.StorePath(cityPath))
	}
}

// maintenanceDoltOpsFactory builds the DoltOpsFactory the store-maintenance
// loop uses to sweep CALL DOLT_GC across the city's managed Dolt databases.
// It returns nil when the city scope is Postgres-backed — there is no Dolt
// server to GC — which leaves the loop in observe-only mode.
//
// The connection is resolved per cycle (not cached) so endpoint changes are
// picked up and no idle connection is held across the maintenance interval.
// A resolution or dial failure surfaces as a stage="gc" MaintenanceError for
// that cycle; the loop alerts and retries on the next interval rather than
// failing supervisor startup.
func maintenanceDoltOpsFactory(cityPath string) supervisor.DoltOpsFactory {
	if scopeBackendIsPostgres(cityPath, cityPath) {
		return nil
	}
	return supervisor.NewSQLDoltOps(func(_ context.Context) (*sql.DB, error) {
		dsn, err := resolveMaintenanceDoltDSN(cityPath)
		if err != nil {
			return nil, err
		}
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, err
		}
		// The sweep is sequential (one database at a time); a small pool is
		// enough and bounds idle connections held against the Dolt server.
		db.SetMaxOpenConns(2)
		db.SetConnMaxLifetime(time.Minute)
		return db, nil
	})
}

// resolveMaintenanceDoltDSN resolves the city-scope managed Dolt endpoint
// and credentials into a go-sql-driver/mysql DSN. Mirrors the resolution in
// internal/api (resolveDoltConnection + buildDoltDSN), kept local because
// those helpers are unexported there.
func resolveMaintenanceDoltDSN(cityPath string) (string, error) {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, cityPath)
	if err != nil {
		return "", fmt.Errorf("resolve dolt target: %w", err)
	}
	host := strings.TrimSpace(target.Host)
	if host == "" {
		return "", fmt.Errorf("missing dolt host for city %s", cityPath)
	}
	port, err := strconv.Atoi(strings.TrimSpace(target.Port))
	if err != nil {
		return "", fmt.Errorf("parse dolt port %q: %w", target.Port, err)
	}
	auth := doltauth.Resolve(doltauth.AuthScopeRoot(cityPath, cityPath, target), strings.TrimSpace(target.User), host, port)
	return buildMaintenanceDoltDSN(auth.User, auth.Password, host, port, target.Database), nil
}

// buildMaintenanceDoltDSN formats a mysql DSN for the maintenance sweep.
// The database is the city default; the sweep selects each target database
// with USE, so the DSN only needs an initial database the connection can
// open. AllowNativePasswords mirrors every other managed-Dolt connection in
// this binary.
func buildMaintenanceDoltDSN(user, password, host string, port int, database string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	cfg := mysql.Config{
		User:                 user,
		Passwd:               password,
		Net:                  "tcp",
		Addr:                 fmt.Sprintf("%s:%d", host, port),
		DBName:               database,
		AllowNativePasswords: true,
		// No ReadTimeout: CALL DOLT_GC can run for minutes and is bounded by
		// the per-database context deadline (DoltMaintenance.GCTimeout), not
		// the driver's socket read timeout. Timeout is the dial timeout only.
		Timeout: 10 * time.Second,
	}
	return cfg.FormatDSN()
}
