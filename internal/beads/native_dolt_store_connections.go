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

// DoltConnectionLister is the optional surface of a store, other than a
// NativeDoltStore, served by a Dolt sql-server that can list the server's
// other client sessions.
type DoltConnectionLister interface {
	// ForeignConnections lists every client session on the server except the
	// one it asks on.
	ForeignConnections(ctx context.Context) ([]DoltConnection, error)
}

// ForeignDoltConnections lists the client sessions on the Dolt server s is
// served by, other than the one the call runs on. ok is false for a store no
// server answers for (a file, SQLite, exec or proxied store).
//
// It is the storage migration's liveness check on a Dolt server the migration
// started itself (S5 §2.3): any other session there is a writer nothing else
// proved stopped. It is a function, not a NativeDoltStore method, so the
// layers that hold an engine's whole method set (the CachingStore, the class
// emitter) need no forward for a check only the migration makes.
func ForeignDoltConnections(ctx context.Context, s Store) (sessions []DoltConnection, ok bool, err error) {
	if native, isNative := As[*NativeDoltStore](s); isNative {
		sessions, err = native.foreignConnections(ctx)
		return sessions, true, err
	}
	if lister, isLister := As[DoltConnectionLister](s); isLister {
		sessions, err = lister.ForeignConnections(ctx)
		return sessions, true, err
	}
	return nil, false, nil
}

// foreignConnections is SHOW PROCESSLIST less CONNECTION_ID(), on one pinned
// connection. Every other session counts, including idle connections of this
// store's own pool beyond the pinned one. gc's one-shot opens cap that pool at
// one connection (cmd/gc nativeDoltCliPoolCap), so on the migration's open
// the pinned connection is the only one it has.
func (s *NativeDoltStore) foreignConnections(ctx context.Context) ([]DoltConnection, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
	if !ok {
		return nil, fmt.Errorf("listing Dolt connections: the linked library's %T exports no UnderlyingDB", storage)
	}
	conn, err := accessor.UnderlyingDB().Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing Dolt connections: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returns the pinned connection to the pool
	var own int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&own); err != nil {
		return nil, fmt.Errorf("listing Dolt connections: reading this session's id: %w", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT ID, USER, HOST, DB, COMMAND FROM information_schema.PROCESSLIST")
	if err != nil {
		return nil, fmt.Errorf("listing Dolt connections: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []DoltConnection
	for rows.Next() {
		var c DoltConnection
		var user, host, db, command sql.NullString
		if err := rows.Scan(&c.ID, &user, &host, &db, &command); err != nil {
			return nil, fmt.Errorf("listing Dolt connections: %w", err)
		}
		if c.ID == own {
			continue
		}
		c.User, c.Host, c.DB, c.Command = user.String, host.String, db.String, command.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing Dolt connections: %w", err)
	}
	return out, nil
}
