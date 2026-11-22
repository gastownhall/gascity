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

// PostOpenDB is a *sql.DB whose every connection reads Before on its first
// statement and After on every later one.
type PostOpenDB struct {
	// DB is the pool. Close it with the test.
	DB *sql.DB

	before, after State
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

// QueryContext answers the bare HEAD read, the plane statement (HEAD, then
// the GROUP_CONCAT of the present tables among the bound table names, NULL
// when none exists) and SHOW COLUMNS FROM <table> LIKE '<column>' (one row
// naming the column when the table carries it; an error, as the server gives,
// when the table is absent).
func (c *conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.mu.Lock()
	c.db.statements = append(c.db.statements, query)
	failure := c.db.err
	state := c.db.after
	if c.issued == 0 {
		state = c.db.before
	}
	c.db.mu.Unlock()
	c.issued++
	if failure != nil {
		return nil, failure
	}
	if strings.HasPrefix(query, "SHOW COLUMNS FROM ") {
		rest := strings.TrimPrefix(query, "SHOW COLUMNS FROM ")
		table, like, _ := strings.Cut(rest, " ")
		_, pattern, _ := strings.Cut(like, "LIKE ")
		column := strings.Trim(strings.TrimSpace(pattern), "'")
		if state.AbsentTables[table] {
			return nil, fmt.Errorf("table not found (Error 1146 (42S02)): %s", table)
		}
		if state.AbsentColumns[table+"."+column] {
			return &rows{columns: showColumns, done: true}, nil
		}
		return &rows{columns: showColumns, values: []driver.Value{[]byte(column), []byte("varchar(255)"), []byte("YES"), []byte(""), nil, []byte("")}}, nil
	}
	if !strings.Contains(query, "DOLT_HASHOF('HEAD')") {
		return nil, fmt.Errorf("proxyendpointtest: no answer for %q", query)
	}
	values := []driver.Value{state.Head}
	if !strings.Contains(query, "information_schema") {
		return &rows{values: values}, nil
	}
	if strings.Count(query, "information_schema.tables") != 1 || strings.Contains(query, "information_schema.columns") || len(args) == 0 {
		return nil, fmt.Errorf("proxyendpointtest: %q is not the plane statement (one tables scan, the bound names, no columns scan)", query)
	}
	var present []string
	for _, arg := range args {
		if table := fmt.Sprint(arg.Value); !state.AbsentTables[table] {
			present = append(present, table)
		}
	}
	var concat driver.Value
	if len(present) > 0 {
		concat = strings.Join(present, ",")
	}
	return &rows{values: append(values, concat)}, nil
}

// showColumns is SHOW COLUMNS' result shape.
var showColumns = []string{"Field", "Type", "Null", "Key", "Default", "Extra"}

type rows struct {
	values  []driver.Value
	columns []string
	done    bool
}

func (r *rows) Columns() []string {
	if r.columns != nil {
		return r.columns
	}
	columns := make([]string, len(r.values))
	for i := range columns {
		columns[i] = fmt.Sprintf("c%d", i)
	}
	return columns
}

func (r *rows) Close() error { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}
