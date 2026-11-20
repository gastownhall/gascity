package proxyendpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// PostOpenReport is what the post-open observation read over the linked
// library's own pool, once a library open had returned.
type PostOpenReport struct {
	// Head is the database's HEAD commit hash as of the observation.
	Head string
	// IgnoredCursorTable reports whether the ignored lane's cursor table
	// exists. The library reads an absent one as version 0.
	IgnoredCursorTable bool
	// Reality is the ignored lane's sentinel reality as of the observation,
	// read exactly as the probe reads it (council pr2 E-S4).
	Reality CursorReality
	// Window is what happened between the HEAD admission pinned and Head: the
	// commits and the tables they changed. It is nil when there was nothing
	// to read — no admitted HEAD was given, or HEAD did not move.
	Window *PostOpenWindow
}

// PostOpenWindowCommitLimit bounds how many commit messages a window read
// returns. A window that held more is reported Truncated, and a truncated
// window is never read as anyone's in particular.
const PostOpenWindowCommitLimit = 64

// PostOpenWindow is the evidence for who moved HEAD between admission and the
// post-open observation: the messages of the commits in between, newest first,
// and the tables those commits changed. It is read on the observation's own
// pinned connection, so it describes the same HEAD the report names.
type PostOpenWindow struct {
	// Commits are the commit messages in since..head, newest first, at most
	// PostOpenWindowCommitLimit of them.
	Commits []string
	// Tables are the tables dolt_diff_summary(since, head) reports changed.
	Tables []WindowTable
	// Truncated reports that the window held more than
	// PostOpenWindowCommitLimit commits.
	Truncated bool
	// Unreadable, when set, is why the window could not be read. Commits and
	// Tables are then empty and say nothing.
	Unreadable string
}

// WindowTable is one table a window's commits changed.
type WindowTable struct {
	// Name is the table's name: to_table_name, or from_table_name for a
	// dropped table.
	Name string
	// SchemaChange reports that the table's schema changed.
	SchemaChange bool
}

// doltCommitHash is the shape of a Dolt commit hash: 32 characters of Dolt's
// base32 alphabet. Dolt's table functions take no bind parameters, so a hash
// is inlined into the window statements only once it matches this.
var doltCommitHash = regexp.MustCompile(`^[0-9a-v]{32}$`)

// EffectiveIgnored is the ignored-lane cursor the linked library would compute
// from the observed plane, given the raw cursor the admitting probe read. The
// raw MAX(version) is not re-read — it cannot share the statement, because a
// SELECT against an absent table fails the whole statement — so it is taken
// from the pin, and a cursor table that has vanished reads as 0, which is what
// the library would read.
func (r PostOpenReport) EffectiveIgnored(pinnedRaw int) int {
	if !r.IgnoredCursorTable {
		return 0
	}
	return r.Reality.EffectiveIgnored(pinnedRaw)
}

// IgnoredPlaneReason names what lowers the observed ignored plane's effective
// cursor, for a message an operator can act on, or "" when nothing does.
func (r PostOpenReport) IgnoredPlaneReason() string {
	if !r.IgnoredCursorTable {
		return "the ignored lane's cursor table " + cursorTableIgnored + " is absent"
	}
	return r.Reality.String()
}

// postOpenHeadQuery is HEAD alone: the observation's first column, and the
// statement a single-column read would issue.
const postOpenHeadQuery = "SELECT DOLT_HASHOF('HEAD')"

// postOpenTables are the tables the observation asks about, in order: the
// ignored lane's cursor table, then the library's sentinel tables and then its
// floored sentinel tables, each in the library's own probing order.
func postOpenTables() []string {
	tables := append([]string{cursorTableIgnored}, ignoredSentinelTables...)
	for _, floored := range ignoredSentinelFlooredTables {
		tables = append(tables, floored.Table)
	}
	return tables
}

// postOpenQuery is the observation's ONE statement: HEAD, then one
// information_schema COUNT(*) per table in postOpenTables, then one for the
// sentinel column. Scalar sub-selects against information_schema cannot fail
// on an absent object — they count zero — so the statement always answers, and
// HEAD and the ignored plane are read at the same instant on the same
// connection.
func postOpenQuery() (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(postOpenHeadQuery)
	for _, table := range postOpenTables() {
		b.WriteString(", (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?)")
		args = append(args, table)
	}
	b.WriteString(", (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?)")
	args = append(args, IgnoredSentinelColumnTable, IgnoredSentinelColumnName)
	return b.String(), args
}

// ErrPostOpenNoPool reports a post-open read handed no pool.
var ErrPostOpenNoPool = errors.New("proxyendpoint: post-open read has no pool")

// ReadPostOpen observes the database over db — the pool the linked library's
// open built — AFTER the library's open-time work, and not before it.
//
// It observes two things, in one statement: HEAD, which every committed write
// moves, and the ignored lane's cursor table plus its sentinel reality, which
// HEAD cannot see at all — the dolt_ignore'd plane is never committed, so a
// write to it moves no hash (council pr2 E-S4). What a caller may conclude from
// the second half, and what it may not, is on beads.ProxiedOpenUnmoved.
//
// "After" is not what a single statement on that pool gives you, and that is
// council pr2 E-S3. beads v1.3.0 runs its open-time checks (Ping,
// CheckForwardDrift, verifyProjectIdentity) on a connection of this pool, then
// runs MigrateUp — the dolt_ignore seed commit, the tracked-cursor heal, the
// content_hash pass — on a SEPARATE migration pool, and rebuilds this one only
// when rebuildPoolAfterMigration sees applied > 0. The seed commit and the
// content_hash pass both report 0. The connection that ran the checks is then
// pinned to the PRE-open session root: beads documents that its first later
// statement reads that old root and that only a succeeding statement advances it
// (be-itm5; store.go:2051-2057, :2977-2985; schema.go:629-637;
// post_migration_pool_heal_integration_test.go). One DOLT_HASHOF('HEAD') on the
// pool is exactly that first statement, so it answered the pre-open hash — the
// value the probe saw — and an open that had just committed passed the check.
//
// When since — the HEAD admission pinned — is given and the observed HEAD is
// not it, the read also asks, on the same pinned connection, what lies between
// the two: the commit messages and the changed tables (PostOpenWindow). A
// window it cannot read is carried in Window.Unreadable rather than returned:
// the HEAD was observed, and an error here would read as "not observed".
//
// So the read pins ONE connection and issues the statement twice on it: the
// first advances whatever root the connection was holding, and the SECOND is
// the answer. Pinning is what makes that true — two statements through the
// pool could land on two connections, and the second could be the stale one.
// The cost is one extra round trip on a connection the library already holds;
// no dial, no handshake, no session of gc's own on bd's proxy. It also leaves
// the connection advanced when it goes back to the pool, which the library's
// own first read on this path was going to need anyway.
func ReadPostOpen(ctx context.Context, db *sql.DB, since string) (PostOpenReport, error) {
	if db == nil {
		return PostOpenReport{}, ErrPostOpenNoPool
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read: pinning a pooled connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the library's pool
	if _, err := readPostOpenOnce(ctx, conn); err != nil {
		// A FAILING statement does not advance the session root (be-itm5), so
		// the second answer would be as stale as the first. Report it.
		return PostOpenReport{}, fmt.Errorf("post-open read (advancing statement): %w", err)
	}
	report, err := readPostOpenOnce(ctx, conn)
	if err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read: %w", err)
	}
	if since != "" && since != report.Head {
		report.Window = readPostOpenWindow(ctx, conn, since, report.Head)
	}
	return report, nil
}

// readPostOpenWindow reads what lies between since and head on conn. It never
// fails: whatever stops the read is recorded in the window's Unreadable.
func readPostOpenWindow(ctx context.Context, conn *sql.Conn, since, head string) *PostOpenWindow {
	for _, hash := range []string{since, head} {
		if !doltCommitHash.MatchString(hash) {
			return &PostOpenWindow{Unreadable: fmt.Sprintf("%q is not a Dolt commit hash", hash)}
		}
	}
	commits, truncated, err := readWindowCommits(ctx, conn, since, head)
	if err != nil {
		return &PostOpenWindow{Unreadable: "reading dolt_log: " + err.Error()}
	}
	tables, err := readWindowTables(ctx, conn, since, head)
	if err != nil {
		return &PostOpenWindow{Unreadable: "reading dolt_diff_summary: " + err.Error()}
	}
	return &PostOpenWindow{Commits: commits, Tables: tables, Truncated: truncated}
}

// readWindowCommits reads the messages of the commits in since..head, newest
// first, one past PostOpenWindowCommitLimit so a longer window is detected.
func readWindowCommits(ctx context.Context, conn *sql.Conn, since, head string) ([]string, bool, error) {
	query := fmt.Sprintf("SELECT message FROM dolt_log('%s..%s') LIMIT %d", since, head, PostOpenWindowCommitLimit+1)
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close() //nolint:errcheck // read-only rows; Err reports a failed read
	var commits []string
	for rows.Next() {
		var message sql.NullString
		if err := rows.Scan(&message); err != nil {
			return nil, false, err
		}
		commits = append(commits, message.String)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(commits) > PostOpenWindowCommitLimit {
		return commits[:PostOpenWindowCommitLimit], true, nil
	}
	return commits, false, nil
}

// readWindowTables reads the tables dolt_diff_summary reports changed between
// since and head.
func readWindowTables(ctx context.Context, conn *sql.Conn, since, head string) ([]WindowTable, error) {
	query := fmt.Sprintf("SELECT from_table_name, to_table_name, schema_change FROM dolt_diff_summary('%s', '%s')", since, head)
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only rows; Err reports a failed read
	var tables []WindowTable
	for rows.Next() {
		var from, to sql.NullString
		var schemaChange bool
		if err := rows.Scan(&from, &to, &schemaChange); err != nil {
			return nil, err
		}
		name := to.String
		if name == "" {
			name = from.String
		}
		tables = append(tables, WindowTable{Name: name, SchemaChange: schemaChange})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// readPostOpenOnce issues the observation's statement once on conn.
//
// A NULL or empty hash is an error rather than "": the caller reads "" as "not
// observed", and a statement that ran and returned nothing is a failure to
// observe, which the caller must be able to log as one.
//
// The sentinel reality is derived in the library's order, as readIgnoredReality
// derives it: the first absent sentinel TABLE floors the lane at
// IgnoredSentinelTableFloor and nothing after it can lower that; only with
// every sentinel table present do the absent floored tables and the sentinel
// column floor it, at the lowest of their floors.
func readPostOpenOnce(ctx context.Context, conn *sql.Conn) (PostOpenReport, error) {
	query, args := postOpenQuery()
	tables := postOpenTables()
	var head sql.NullString
	counts := make([]int, len(tables)+1)
	dest := []any{&head}
	for i := range counts {
		dest = append(dest, &counts[i])
	}
	if err := conn.QueryRowContext(ctx, query, args...).Scan(dest...); err != nil {
		return PostOpenReport{}, err
	}
	trimmed := strings.TrimSpace(head.String)
	if !head.Valid || trimmed == "" {
		return PostOpenReport{}, errors.New("DOLT_HASHOF('HEAD') returned no hash")
	}
	report := PostOpenReport{Head: trimmed, IgnoredCursorTable: counts[0] > 0}
	for i, table := range ignoredSentinelTables {
		if counts[i+1] == 0 {
			report.Reality = CursorReality{Limited: true, Floor: IgnoredSentinelTableFloor, Missing: table}
			return report, nil
		}
	}
	for i, floored := range ignoredSentinelFlooredTables {
		if counts[1+len(ignoredSentinelTables)+i] == 0 {
			report.Reality = report.Reality.lowered(floored.Floor, floored.Table)
		}
	}
	if counts[len(counts)-1] == 0 {
		report.Reality = report.Reality.lowered(IgnoredSentinelColumnFloor, IgnoredSentinelColumnTable+"."+IgnoredSentinelColumnName)
	}
	return report, nil
}
