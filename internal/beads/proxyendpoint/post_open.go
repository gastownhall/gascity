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
}

// postOpenHeadQuery is the observation's statement.
const postOpenHeadQuery = "SELECT DOLT_HASHOF('HEAD')"

// ErrPostOpenNoPool reports a post-open read handed no pool.
var ErrPostOpenNoPool = errors.New("proxyendpoint: post-open read has no pool")

// ReadPostOpen observes the database over db — the pool the linked library's
// open built — AFTER the library's open-time work, and not before it.
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
func ReadPostOpen(ctx context.Context, db *sql.DB) (PostOpenReport, error) {
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
	return report, nil
}

// readPostOpenOnce issues the observation's statement once on conn.
//
// A NULL or empty hash is an error rather than "": the caller reads "" as "not
// observed", and a statement that ran and returned nothing is a failure to
// observe, which the caller must be able to log as one.
func readPostOpenOnce(ctx context.Context, conn *sql.Conn) (PostOpenReport, error) {
	var head sql.NullString
	if err := conn.QueryRowContext(ctx, postOpenHeadQuery).Scan(&head); err != nil {
		return PostOpenReport{}, err
	}
	trimmed := strings.TrimSpace(head.String)
	if !head.Valid || trimmed == "" {
		return PostOpenReport{}, errors.New("DOLT_HASHOF('HEAD') returned no hash")
	}
	return PostOpenReport{Head: trimmed}, nil
}
