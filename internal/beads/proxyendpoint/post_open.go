package proxyendpoint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

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
// So the read pins ONE connection and issues the plane statement AFTER an
// advancing statement on it: the first advances whatever root the connection
// was holding, and the plane statement (schemaPlaneQuery, then SHOW COLUMNS
// for the sentinel column exactly as the probe reads it) is the answer.
// Pinning is what makes that true — two statements through the pool could
// land on two connections, and the second could be the stale one. The cost is
// one extra round trip on a connection the library already holds; no dial, no
// handshake, no session of gc's own on bd's proxy. It also leaves the
// connection advanced when it goes back to the pool, which the library's own
// first read on this path was going to need anyway.
func ReadPostOpen(ctx context.Context, db *sql.DB) (PostOpenReport, error) {
	if db == nil {
		return PostOpenReport{}, ErrPostOpenNoPool
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read: pinning a pooled connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the library's pool
	// The advancing statement is the cheapest one that succeeds: HEAD alone,
	// no catalog scan. Its answer is discarded — it is the statement AFTER it
	// that observes — so it need not carry the plane, and until ga-7lmbiq it
	// did, which doubled every post-open observation's catalog scans.
	//
	// A FAILING statement does not advance the session root (be-itm5), so the
	// observation would be as stale as the first answer. Report it.
	var advanced sql.NullString
	if err := conn.QueryRowContext(ctx, schemaPlaneHeadQuery).Scan(&advanced); err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read (advancing statement): %w", err)
	}
	plane, err := readSchemaPlane(ctx, conn)
	if err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read: %w", err)
	}
	reality, err := readIgnoredReality(ctx, conn, plane)
	if err != nil {
		return PostOpenReport{}, fmt.Errorf("post-open read: %w", err)
	}
	return PostOpenReport{
		Head:               plane.head,
		IgnoredCursorTable: plane.exists(cursorTableIgnored),
		Reality:            reality,
	}, nil
}
