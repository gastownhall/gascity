package proxyendpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// Window is the commit window between the HEAD the admitting probe pinned
	// and Head, read only when the two differ. It is the evidence a moved HEAD
	// is attributed BY: a window whose commits carry no "schema:" prefix and
	// whose changed tables are all outside the library's open-write set is a
	// move another bd client made, and beads.ProxiedOpenUnmoved lets that open
	// stand. Nil when no window applies — the pin carried no hash (a memoized
	// admission) or HEAD did not move — and never a substitute for the hashes
	// above.
	Window *PostOpenWindow
}

// PostOpenWindow is the commit window between the hash admission pinned and the
// hash the open observed, as Dolt's own table functions report it.
//
// Two statements over the same pinned connection as the observation: dolt_log
// lists the commit messages (newest first), dolt_diff_summary the tables each
// with whether its schema changed. Neither statement takes a bind parameter, so
// the two hashes are INLINED into the SQL — after isDoltCommitHash has accepted
// them, which is the injection boundary, not a formality.
//
// A window is evidence, not a pardon. It is read only for a MOVED HEAD, and it
// is the caller's job (beads.ProxiedOpenUnmoved) to decide what it proves: the
// shapes only a linked-library open mints, and the failure shapes that prove
// nothing.
type PostOpenWindow struct {
	// Commits are the window's commit messages, newest first, capped at
	// PostOpenWindowCommitLimit.
	Commits []string
	// Tables are the tables dolt_diff_summary reports as changed in the
	// window.
	Tables []WindowTable
	// Truncated reports that the window held more commits than the cap, so
	// Commits is a prefix and attribution cannot see all of them.
	Truncated bool
	// Unreadable, when non-empty, names why the window could not be read (a
	// statement the server refused, or a hash it could not be parameterized
	// with). Whatever partial data the other statements produced is kept, for
	// the log line, and the caller must still decline on it.
	Unreadable string
}

// WindowTable is one table dolt_diff_summary reports as changed in the window.
type WindowTable struct {
	// Name is the table's name.
	Name string
	// SchemaChange reports whether the window changed the table's schema, not
	// only its rows.
	SchemaChange bool
}

// PostOpenWindowCommitLimit caps the commits one window read lists. A linked
// library open's own writes are a handful of commits; a longer window than
// this is a busy store's, and the flag that says so (Truncated) is cheaper
// than listing all of it on a connection the library is waiting to use.
const PostOpenWindowCommitLimit = 64

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
// So the read pins ONE connection and issues the statement twice on it: the
// first advances whatever root the connection was holding, and the SECOND is
// the answer. Pinning is what makes that true — two statements through the
// pool could land on two connections, and the second could be the stale one.
// The cost is one extra round trip on a connection the library already holds;
// no dial, no handshake, no session of gc's own on bd's proxy. It also leaves
// the connection advanced when it goes back to the pool, which the library's
// own first read on this path was going to need anyway.
//
// since is the HEAD the admitting probe pinned: when it is empty (a memoized
// admission, Pin.withoutHead) or equal to the observed HEAD, no window is read
// and report.Window is nil; when the two hashes differ, the same pinned
// connection answers the window's two statements, so attribution never costs a
// connection of its own. A window the server will not answer is carried on the
// report (Window.Unreadable) and is NOT an error: the hashes were observed, and
// it is the caller's verdict that decides what the unreadable window proves.
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
	report.Window = readPostOpenWindow(ctx, conn, since, report.Head)
	return report, nil
}

// postOpenLogQuery and postOpenDiffSummaryQuery are the window's two
// statements, with the hashes INLINED. Dolt's table functions take no bind
// parameters, so there is no prepared form to hand them; isDoltCommitHash
// accepting both hashes first is what makes inlining them safe.
func postOpenLogQuery(since, after string) string {
	return fmt.Sprintf("SELECT message FROM dolt_log('%s..%s') ORDER BY date DESC LIMIT %d",
		since, after, PostOpenWindowCommitLimit+1)
}

func postOpenDiffSummaryQuery(since, after string) string {
	return fmt.Sprintf("SELECT table_name, schema_change FROM dolt_diff_summary('%s', '%s')", since, after)
}

// isDoltCommitHash reports whether s is a Dolt commit hash exactly as
// DOLT_HASHOF answers one: 32 characters of Dolt's base32 alphabet. It is the
// injection boundary for the window's inlined statements, and the reason an
// observation that cannot prove its hashes says Unreadable instead of asking.
func isDoltCommitHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		digit, lower := '0' <= c && c <= '9', 'a' <= c && c <= 'v'
		if !digit && !lower {
			return false
		}
	}
	return true
}

// readPostOpenWindow reads the commit window between since and after over the
// connection the observation is already pinned to, and returns nil when no
// window applies: since empty (a memoized admission) or equal to after (HEAD
// did not move). A hash pair that cannot be parameterized, or a statement the
// server refuses, is an Unreadable window — never an error, and never nil,
// because "could not attribute" and "nothing to attribute" are different
// answers the caller must be able to tell apart.
func readPostOpenWindow(ctx context.Context, conn *sql.Conn, since, after string) *PostOpenWindow {
	since, after = strings.TrimSpace(since), strings.TrimSpace(after)
	if since == "" || since == after {
		return nil
	}
	window := &PostOpenWindow{}
	if !isDoltCommitHash(since) || !isDoltCommitHash(after) {
		window.Unreadable = fmt.Sprintf("the moved HEAD pair %s..%s is not a pair of Dolt commit hashes, so the window's table functions cannot be parameterized for it", since, after)
		return window
	}
	if commits, truncated, err := readWindowCommits(ctx, conn, since, after); err != nil {
		window.Unreadable = err.Error()
	} else {
		window.Commits, window.Truncated = commits, truncated
	}
	// The tables are read even when the commits failed, so an operator's log
	// line can still name the evidence the half that answered produced.
	if tables, err := readWindowTables(ctx, conn, since, after); err != nil {
		if window.Unreadable == "" {
			window.Unreadable = err.Error()
		}
	} else {
		window.Tables = tables
	}
	return window
}

func readWindowCommits(ctx context.Context, conn *sql.Conn, since, after string) (commits []string, truncated bool, err error) {
	rows, err := conn.QueryContext(ctx, postOpenLogQuery(since, after))
	if err != nil {
		return nil, false, fmt.Errorf("reading dolt_log(%s..%s): %w", since, after, err)
	}
	defer rows.Close() //nolint:errcheck // the connection returns to the library's pool
	for rows.Next() {
		var message sql.NullString
		if err := rows.Scan(&message); err != nil {
			return nil, false, fmt.Errorf("reading dolt_log(%s..%s): %w", since, after, err)
		}
		commits = append(commits, message.String)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading dolt_log(%s..%s): %w", since, after, err)
	}
	if len(commits) > PostOpenWindowCommitLimit {
		// The query asked for the cap PLUS ONE: the extra row is the flag, and
		// it is dropped here rather than handed to attribution as evidence it
		// cannot weigh.
		return commits[:PostOpenWindowCommitLimit], true, nil
	}
	return commits, false, nil
}

func readWindowTables(ctx context.Context, conn *sql.Conn, since, after string) ([]WindowTable, error) {
	rows, err := conn.QueryContext(ctx, postOpenDiffSummaryQuery(since, after))
	if err != nil {
		return nil, fmt.Errorf("reading dolt_diff_summary(%s, %s): %w", since, after, err)
	}
	defer rows.Close() //nolint:errcheck // the connection returns to the library's pool
	var tables []WindowTable
	for rows.Next() {
		var name sql.NullString
		var change any
		if err := rows.Scan(&name, &change); err != nil {
			return nil, fmt.Errorf("reading dolt_diff_summary(%s, %s): %w", since, after, err)
		}
		tables = append(tables, WindowTable{Name: name.String, SchemaChange: windowSchemaChange(change)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading dolt_diff_summary(%s, %s): %w", since, after, err)
	}
	return tables, nil
}

// windowSchemaChange reads dolt_diff_summary's schema_change column, which
// drivers hand back as any of a boolean, a number or a string depending on the
// server, as one bool.
func windowSchemaChange(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []byte:
		return windowSchemaChange(string(t))
	case string:
		return t == "1" || t == "true" || t == "yes"
	default:
		return false
	}
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
