// Package doltpool provides a shared, process-lifetime *sql.DB registry
// for Go-native Dolt (MySQL-protocol) connections. Each distinct
// (host, port, user, password, database) combination is opened once and
// reused across callers, eliminating the per-operation Open+Close
// pattern that produces TIME_WAIT churn (2,618 sockets observed from
// one call site) and unbounded backend connections.
//
// database/sql's *sql.DB is itself a connection pool: Open here is lazy
// (no dial), connections are created on demand and bounded by the
// per-endpoint caps below. Callers must NEVER Close a returned handle;
// call Shutdown once on process exit if orderly cleanup is wanted.
//
// Ported from the vp-kxbh worktree skeleton (city-scale plan item 1.2).
package doltpool

import (
	"database/sql"
	"fmt"
	"net"
	"sync"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// Per-endpoint connection caps. With S scopes a city consumes at most
// S×maxOpenConns backend connections from gc's Go-native paths, which the
// supervisor doctor budget (≤0.8×@@max_connections, plan item 2.7) can
// reason about.
const (
	maxOpenConns    = 5
	maxIdleConns    = 2
	connMaxLifetime = time.Hour
	// connMaxIdleTime must stay STRICTLY BELOW the server's wait_timeout,
	// which gc's managed dolt config sets to 30s. Without it an idle pooled
	// connection lives until connMaxLifetime (an hour), so the SERVER reaps
	// it first and logs an error per reap:
	//
	//   Error reading packet from client N: read tcp ... i/o timeout
	//   io.ReadFull(header size) failed
	//
	// 6565 such lines in one day were observed on a single managed city,
	// arriving on a clean 30s cycle — the reaper working, logged at ERROR.
	// Closing idle connections client-side first removes the events at the
	// source rather than muting them. 20s leaves a 10s margin; equality
	// with wait_timeout would be a race, not a bound.
	connMaxIdleTime = 20 * time.Second
	connTimeout     = 5 * time.Second
	readTimeout     = 30 * time.Second
	writeTimeout    = 30 * time.Second
)

var registry = &poolRegistry{
	dbs: make(map[string]*sql.DB),
}

type poolRegistry struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
}

// key includes the password so a credential rotation (e.g. a managed
// server restart republishing auth) yields a fresh pool instead of
// serving stale-credential connections. Keys live only in process
// memory and are never logged.
func key(host, port, user, password, database string) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", user, host, port, database, password)
}

// formatDSN builds the go-sql-driver DSN for one endpoint.
func formatDSN(host, port, user, password, database string) string {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	// JoinHostPort, not concatenation: a literal IPv6 host must be
	// bracketed ("::1" -> "[::1]:3306"), or the driver is handed the
	// unparseable "::1:3306".
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.DBName = database
	cfg.Timeout = connTimeout
	cfg.ReadTimeout = readTimeout
	cfg.WriteTimeout = writeTimeout
	cfg.AllowNativePasswords = true
	// DATETIME columns scan into time.Time (the convoy workflow snapshot
	// reads created_at/updated_at this way); string scans still work.
	cfg.ParseTime = true
	return cfg.FormatDSN()
}

// Open returns the shared *sql.DB for the given Dolt endpoint, creating
// it on first use. database may be empty for server-level connections
// (SHOW DATABASES, health probes). The returned handle must never be
// closed by the caller; call Shutdown on process exit.
func Open(host, port, user, password, database string) (*sql.DB, error) {
	k := key(host, port, user, password, database)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if db, ok := registry.dbs[k]; ok {
		return db, nil
	}
	db, err := sql.Open("mysql", formatDSN(host, port, user, password, database))
	if err != nil {
		return nil, fmt.Errorf("opening pooled dolt connection to %s:%s/%s: %w", host, port, database, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	registry.dbs[k] = db
	return db, nil
}

// Shutdown closes all pooled connections and empties the registry. Call
// once on process exit; subsequent Open calls recreate pools on demand.
func Shutdown() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for k, db := range registry.dbs {
		db.Close() //nolint:errcheck // best-effort close on shutdown
		delete(registry.dbs, k)
	}
}

// Len returns the number of distinct endpoint entries in the registry.
func Len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.dbs)
}

// TotalOpenConns returns the sum of open connections across all pooled
// *sql.DB instances. Use this for observability gauges; it does not
// imply pool health.
func TotalOpenConns() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	total := 0
	for _, db := range registry.dbs {
		total += db.Stats().OpenConnections
	}
	return total
}
