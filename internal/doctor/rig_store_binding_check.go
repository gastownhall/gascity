package doctor

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
)

// rigStoreBindingMinRetentionPct is the minimum percentage of the most
// recent accepted JSONL export's issue count that the live store must
// retain before RigStoreBindingCheck fails it as collapsed. It mirrors the
// export pack's own GC_BEADS_JSONL_MAX_SHRINK_PCT default (20% max shrink).
const rigStoreBindingMinRetentionPct = 80

// rigStoreBindingQueryTimeout bounds each SQL round-trip the check makes.
const rigStoreBindingQueryTimeout = 5 * time.Second

// mysqlUnknownDatabaseErrno is the standard MySQL wire-protocol error
// number for ER_BAD_DB_ERROR ("Unknown database").
const mysqlUnknownDatabaseErrno = 1049

// RigStoreBindingCheck verifies that a rig's configured Dolt database
// actually exists on its resolved endpoint and holds data consistent with
// the rig's own most recent accepted JSONL export.
//
// This closes a gap RigDoltServerCheck cannot: RigDoltServerCheck only
// TCP-dials the endpoint, and it deliberately skips rigs that inherit the
// city's endpoint, since reachability of a shared server says nothing
// about whether one particular rig's database exists on it. During the
// 2026-08-10 incident, the shared managed Dolt server stayed reachable for
// 16h44m while several inherited-endpoint rigs' databases were missing or
// empty, and nothing in `gc doctor` caught it. RigStoreBindingCheck always
// queries the resolved database directly, regardless of endpoint origin.
type RigStoreBindingCheck struct {
	cityPath string
	rig      config.Rig
	skip     bool
}

// NewRigStoreBindingCheck creates a per-rig store-binding check. skip
// mirrors the same managed-bd-store-contract gate used by
// NewRigDoltServerCheck: callers pass true for rigs that don't use a
// managed/contract-governed bd store, or when dolt checks are globally
// disabled.
func NewRigStoreBindingCheck(cityPath string, rig config.Rig, skip bool) *RigStoreBindingCheck {
	return &RigStoreBindingCheck{cityPath: cityPath, rig: rig, skip: skip}
}

// Name returns the check identifier ("rig:<name>:store-binding").
func (c *RigStoreBindingCheck) Name() string { return "rig:" + c.rig.Name + ":store-binding" }

// Run executes the check.
func (c *RigStoreBindingCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.skip {
		r.Status = StatusOK
		r.Message = "skipped (file backend or GC_DOLT=skip)"
		return r
	}

	rigPath := normalizedRigPath(c.cityPath, c.rig)
	if scopeUsesBDDoltliteStore(c.cityPath, rigPath) {
		r.Status = StatusOK
		r.Message = "not required (bd backend=doltlite)"
		return r
	}

	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, c.cityPath, rigPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: resolve dolt target: %v", c.rig.Name, err)
		r.FixHint = "reconcile the canonical external Dolt endpoint"
		return r
	}

	portNum, err := strconv.Atoi(strings.TrimSpace(target.Port))
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: invalid dolt port %q: %v", c.rig.Name, target.Port, err)
		return r
	}
	authScope := doltauth.AuthScopeRoot(c.cityPath, rigPath, target)
	auth := doltauth.Resolve(authScope, target.User, target.Host, portNum)
	addr := net.JoinHostPort(target.Host, target.Port)

	ctx, cancel := context.WithTimeout(context.Background(), rigStoreBindingQueryTimeout)
	defer cancel()

	db, err := openRigStoreBindingConn(target.Host, target.Port, auth.User, auth.Password, target.Database)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: open dolt connection to %s: %v", c.rig.Name, addr, err)
		return r
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlUnknownDatabaseErrno {
			r.Status = StatusError
			r.Message = fmt.Sprintf("rig %q: database %q missing from dolt endpoint %s", c.rig.Name, target.Database, addr)
			r.FixHint = fmt.Sprintf("create the %q database on %s or restore it from backup", target.Database, addr)
			return r
		}
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: cannot verify store binding at %s: %v", c.rig.Name, addr, err)
		return r
	}

	var liveCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues").Scan(&liveCount); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: query issues count on %s/%s: %v", c.rig.Name, addr, target.Database, err)
		return r
	}
	var liveNewest sql.NullTime
	if err := db.QueryRowContext(ctx, "SELECT MAX(created_at) FROM issues").Scan(&liveNewest); err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: query issues freshness on %s/%s: %v", c.rig.Name, addr, target.Database, err)
		return r
	}

	exportPath, err := latestAcceptedRigExport(c.cityPath, c.rig.Name)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: read export baseline: %v", c.rig.Name, err)
		return r
	}
	if exportPath == "" {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("rig %q: database %q on %s has %d issues (no export baseline to compare)", c.rig.Name, target.Database, addr, liveCount)
		return r
	}

	stats, err := readRigExportStats(exportPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("rig %q: read export baseline %s: %v", c.rig.Name, exportPath, err)
		return r
	}
	r.Details = append(r.Details, fmt.Sprintf("export baseline: %s (%d issues)", filepath.Base(exportPath), stats.lines))

	if stats.lines > 0 && liveCount*100 < stats.lines*rigStoreBindingMinRetentionPct {
		r.Status = StatusError
		if liveCount == 0 {
			r.Message = fmt.Sprintf("rig %q: database %q on %s has an empty issues table but the last accepted export (%s) has %d issues", c.rig.Name, target.Database, addr, filepath.Base(exportPath), stats.lines)
		} else {
			r.Message = fmt.Sprintf("rig %q: database %q on %s has %d issues, below %d%% of the last accepted export's %d (%s)", c.rig.Name, target.Database, addr, liveCount, rigStoreBindingMinRetentionPct, stats.lines, filepath.Base(exportPath))
		}
		r.FixHint = "restore the database from backup or investigate the store collapse before trusting doctor OK"
		return r
	}

	if !stats.newest.IsZero() && !liveNewest.Time.After(stats.newest) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("rig %q: database %q on %s has not advanced past the last accepted export (%s, newest %s); live store looks frozen", c.rig.Name, target.Database, addr, filepath.Base(exportPath), stats.newest.Format(time.RFC3339))
		r.FixHint = "confirm writes are actually reaching this rig's live store, not silently landing elsewhere"
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("rig %q: database %q on %s has %d issues (export baseline %d)", c.rig.Name, target.Database, addr, liveCount, stats.lines)
	return r
}

// CanFix returns false. Restoring a collapsed or missing database is an
// operator decision (from backup, from the JSONL export, or investigating
// why writes stopped reaching it) — gc must not guess.
func (c *RigStoreBindingCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *RigStoreBindingCheck) Fix(_ *CheckContext) error { return nil }

// openRigStoreBindingConn opens a lazy (unconnected) *sql.DB against a
// Dolt sql-server endpoint. The caller must Ping to force connection.
func openRigStoreBindingConn(host, port, user, password, database string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = strings.TrimSpace(user)
	if cfg.User == "" {
		cfg.User = "root"
	}
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.DBName = database
	cfg.Timeout = rigStoreBindingQueryTimeout
	cfg.ReadTimeout = rigStoreBindingQueryTimeout
	cfg.WriteTimeout = rigStoreBindingQueryTimeout
	cfg.AllowNativePasswords = true
	cfg.ParseTime = true
	return sql.Open("mysql", cfg.FormatDSN())
}

// rigExportStats summarizes a rig's accepted JSONL export for comparison
// against the live store.
type rigExportStats struct {
	lines  int
	newest time.Time
}

// latestAcceptedRigExport returns the path to the most recently named
// accepted JSONL export for rigName under cityPath's
// .beads-jsonl-export/<rigName>/ directory, or "" if none exists. Export
// filenames are <rigName>-<date>.jsonl, which sort lexically by recency.
func latestAcceptedRigExport(cityPath, rigName string) (string, error) {
	dir := filepath.Join(cityPath, ".beads-jsonl-export", rigName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	prefix := rigName + "-"
	best := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if name > best {
			best = name
		}
	}
	if best == "" {
		return "", nil
	}
	return filepath.Join(dir, best), nil
}

// readRigExportStats counts issue lines and finds the newest created_at in
// a JSONL export.
func readRigExportStats(path string) (rigExportStats, error) {
	f, err := os.Open(path) //nolint:gosec // path is derived from the city's own export directory
	if err != nil {
		return rigExportStats{}, err
	}
	defer func() { _ = f.Close() }()

	var stats rigExportStats
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		stats.lines++
		var rec struct {
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(line, &rec); err == nil && rec.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, rec.CreatedAt); err == nil && t.After(stats.newest) {
				stats.newest = t
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}
