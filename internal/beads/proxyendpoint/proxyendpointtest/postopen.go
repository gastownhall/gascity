// Package proxyendpointtest provides a scripted database/sql pool for tests of
// the proxied-native lane's POST-open observation: the statement gc runs over
// the linked library's own pool once a library open has returned.
//
// It exists because that observation is only as good as the connection it runs
// on, and the connection's behavior is the whole defect it has to survive. beads
// v1.3.0 runs its open-time checks (Ping, CheckForwardDrift,
// verifyProjectIdentity) on a pooled connection, runs its migrations and
// dolt_ignore/cursor-heal commits on a SEPARATE pool, and rebuilds the first
// pool only when a numbered migration applied (rebuildPoolAfterMigration). A
// connection that survives that is pinned to the pre-open session root: its
// FIRST later statement reads the old root, and only a succeeding statement
// advances it (be-itm5; beads' post_migration_pool_heal_integration_test.go).
//
// PostOpenDB models exactly that and nothing more: every connection it hands
// out answers its first statement from Before and every later statement from
// After. A reader that trusts the first answer sees the database as it was
// before the open, which is the false negative the lane's check must not have.
package proxyendpointtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// State is what the database looks like to one statement.
type State struct {
	// Head is what DOLT_HASHOF('HEAD') answers.
	Head string
	// AbsentTables and AbsentColumns are the schema objects that do NOT
	// exist, keyed by table name and by "table.column". Everything not named
	// exists, so a healthy state is a zero value plus a Head.
	AbsentTables  map[string]bool
	AbsentColumns map[string]bool
}

// Window is what the pool answers for the commits between two hashes: the
// dolt_log('<since>..<head>') messages and the dolt_diff_summary rows.
type Window struct {
	// Commits are the commit messages dolt_log answers, newest first.
	Commits []string
	// Tables are the dolt_diff_summary rows.
	Tables []WindowTable
	// Err, when set, is what every window statement fails with. The HEAD
	// statement still answers.
	Err error
}

// WindowTable is one dolt_diff_summary row.
type WindowTable struct {
	// Name is the table, answered as both from_table_name and to_table_name.
	Name string
	// SchemaChange is the row's schema_change column.
	SchemaChange bool
}

// PostOpenDB is a *sql.DB whose every connection reads Before on its first
// statement and After on every later one.
type PostOpenDB struct {
	// DB is the pool. Close it with the test.
	DB *sql.DB

	before, after State
	window        Window
	// Err, when set, is what every statement fails with.
	err error

	mu         sync.Mutex
	conns      int
	statements []string
}

// NewPostOpenDB builds the pool.
func NewPostOpenDB(before, after State) *PostOpenDB {
	p := &PostOpenDB{before: before, after: after}
	p.DB = sql.OpenDB(connector{db: p})
	return p
}

// Failing makes every statement fail with err.
func (p *PostOpenDB) Failing(err error) *PostOpenDB {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	return p
}

// WithWindow scripts the answers to the window statements.
func (p *PostOpenDB) WithWindow(w Window) *PostOpenDB {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.window = w
	return p
}

// Conns reports how many connections the pool opened.
func (p *PostOpenDB) Conns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

// Statements returns every statement issued, in order.
func (p *PostOpenDB) Statements() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.statements...)
}

type connector struct{ db *PostOpenDB }

func (c connector) Connect(context.Context) (driver.Conn, error) {
	c.db.mu.Lock()
	c.db.conns++
	c.db.mu.Unlock()
	return &conn{db: c.db}, nil
}

func (c connector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("proxyendpointtest: the scripted pool has no DSN driver")
}

type conn struct {
	db     *PostOpenDB
	issued int
}

func (c *conn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("proxyendpointtest: the scripted pool answers queries directly")
}

func (c *conn) Close() error { return nil }

func (c *conn) Begin() (driver.Tx, error) {
	return nil, errors.New("proxyendpointtest: the scripted pool is read-only")
}

// QueryContext answers any statement that asks for DOLT_HASHOF('HEAD'),
// followed by one COUNT(*) per information_schema.tables sub-select (one
// argument each, the table) and one per information_schema.columns sub-select
// (two arguments each, table then column), in that order. A dolt_log statement
// answers the scripted window's commit messages, one row each, and a
// dolt_diff_summary statement its tables as (from_table_name, to_table_name,
// schema_change) rows.
func (c *conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	c.db.statements = append(c.db.statements, query)
	failure := c.db.err
	window := c.db.window
	state := c.db.after
	if c.issued == 0 {
		state = c.db.before
	}
	c.db.mu.Unlock()
	c.issued++
	if failure != nil {
		return nil, failure
	}
	switch {
	case strings.Contains(query, "dolt_log("):
		if window.Err != nil {
			return nil, window.Err
		}
		out := &rows{columns: 1}
		for _, message := range window.Commits {
			out.values = append(out.values, []driver.Value{message})
		}
		return out, nil
	case strings.Contains(query, "dolt_diff_summary("):
		if window.Err != nil {
			return nil, window.Err
		}
		out := &rows{columns: 3}
		for _, table := range window.Tables {
			out.values = append(out.values, []driver.Value{table.Name, table.Name, table.SchemaChange})
		}
		return out, nil
	}
	if !strings.Contains(query, "DOLT_HASHOF('HEAD')") {
		return nil, fmt.Errorf("proxyendpointtest: no answer for %q", query)
	}
	values := []driver.Value{state.Head}
	tables := strings.Count(query, "information_schema.tables")
	columns := strings.Count(query, "information_schema.columns")
	if len(args) != tables+2*columns {
		return nil, fmt.Errorf("proxyendpointtest: %d argument(s) for %d table and %d column sub-select(s)",
			len(args), tables, columns)
	}
	for i := 0; i < tables; i++ {
		values = append(values, exists(!state.AbsentTables[fmt.Sprint(args[i].Value)]))
	}
	for i := 0; i < columns; i++ {
		key := fmt.Sprint(args[tables+2*i].Value) + "." + fmt.Sprint(args[tables+2*i+1].Value)
		values = append(values, exists(!state.AbsentColumns[key]))
	}
	return &rows{columns: len(values), values: [][]driver.Value{values}}, nil
}

func exists(present bool) int64 {
	if present {
		return 1
	}
	return 0
}

type rows struct {
	columns int
	values  [][]driver.Value
	next    int
}

func (r *rows) Columns() []string {
	columns := make([]string, r.columns)
	for i := range columns {
		columns[i] = fmt.Sprintf("c%d", i)
	}
	return columns
}

func (r *rows) Close() error { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}
