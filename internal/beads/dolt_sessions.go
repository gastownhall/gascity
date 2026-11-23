package beads

import (
	"context"
	"database/sql"
	"fmt"
)

// DoltConnection is one client session a Dolt sql-server reports.
type DoltConnection struct {
	ID      int64
	User    string
	Host    string
	DB      string
	Command string
}

// ForeignDoltSessions lists the client sessions on the Dolt sql-server db
// reaches, other than the one this call runs on: SHOW PROCESSLIST less
// CONNECTION_ID(), on one pinned connection.
//
// It is the storage migration's liveness check on a Dolt server the migration
// started itself (S5 §2.3): any other session there is a writer nothing else
// proved stopped. Every other session counts, the caller's own idle ones in
// db's pool or elsewhere included, so the caller closes those first.
func ForeignDoltSessions(ctx context.Context, db *sql.DB) ([]DoltConnection, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing Dolt sessions: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returns the pinned connection to the pool
	var own int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&own); err != nil {
		return nil, fmt.Errorf("listing Dolt sessions: reading this session's id: %w", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT ID, USER, HOST, DB, COMMAND FROM information_schema.PROCESSLIST")
	if err != nil {
		return nil, fmt.Errorf("listing Dolt sessions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []DoltConnection
	for rows.Next() {
		var c DoltConnection
		var user, host, db, command sql.NullString
		if err := rows.Scan(&c.ID, &user, &host, &db, &command); err != nil {
			return nil, fmt.Errorf("listing Dolt sessions: %w", err)
		}
		if c.ID == own {
			continue
		}
		c.User, c.Host, c.DB, c.Command = user.String, host.String, db.String, command.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing Dolt sessions: %w", err)
	}
	return out, nil
}
