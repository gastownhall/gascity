package proxyendpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/doltpool"
)

// Probe budgets. A probe is a diagnostic, not a retry loop: it either answers
// inside this window or the caller treats the endpoint as unproven and escalates
// through a bd verb.
const (
	// ProbeDialTimeout bounds the bare TCP dial that separates "nothing is
	// listening" from "something accepted us".
	ProbeDialTimeout = 500 * time.Millisecond
	// ProbeSessionTimeout bounds the MySQL handshake and both cursor reads.
	ProbeSessionTimeout = 2 * time.Second
)

// Cursor table names. bd has kept these stable across the whole 1.x line
// (beads internal/storage/schema/schema.go), which is what makes two read-only
// point queries a safe thing for a co-resident reader to issue.
const (
	mainCursorQuery    = "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"
	ignoredCursorQuery = "SELECT COALESCE(MAX(version), 0) FROM ignored_schema_migrations"
	cursorTableMain    = "schema_migrations"
	cursorTableIgnored = "ignored_schema_migrations"
	cursorExistsQuery  = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
)

// ProbeOutcome is what one probe of a proxy's data port concluded. The three
// real outcomes are the three observable states of bd's byte-pump proxy, and
// they are not orderable: `refused` on a live record is a proxy draining its
// backend, `accepted_no_greeting` is a proxy whose Dolt child has exited, and
// `served` is the only one a reader may act on.
type ProbeOutcome int

// Probe outcomes.
const (
	// ProbeUnknown is the zero value, and also the honest answer when the
	// session failed for a reason that is not about the connection at all —
	// a missing database, a denied login, a caller's canceled context. It is
	// never a conclusion about the proxy.
	ProbeUnknown ProbeOutcome = iota
	// ProbeRefused means nothing accepted the connection. On a record whose
	// process is gone this is the ordinary stopped state; on a live
	// same-generation record it is bd's teardown window, where the listener is
	// already closed and the record is removed only after the backend's
	// shutdown GC finishes.
	ProbeRefused
	// ProbeAcceptedNoGreeting means the proxy accepted the connection and then
	// closed it without a MySQL greeting. That is the signature of a live proxy
	// whose backend dial failed: the proxy parses no wire protocol, so a dead
	// Dolt child shows up as an accept followed by a close.
	ProbeAcceptedNoGreeting
	// ProbeServed means the handshake completed and both schema cursors were
	// read.
	ProbeServed
)

// String renders the outcome as the token the diagnostics report.
func (o ProbeOutcome) String() string {
	switch o {
	case ProbeRefused:
		return "refused"
	case ProbeAcceptedNoGreeting:
		return "accepted_no_greeting"
	case ProbeServed:
		return "served"
	default:
		return "unknown"
	}
}

// Cursors are a database's two schema-migration cursors: the main lane and the
// dolt-ignored lane. Both are read because bd's own shared-store migration gate
// consults only the main one, so a reader that compared only that lane could
// meet a database its linked library would migrate without consent.
type Cursors struct {
	Main    int `json:"main"`
	Ignored int `json:"ignored"`
}

// String renders the pair compactly for a message.
func (c Cursors) String() string {
	return "main=" + strconv.Itoa(c.Main) + " ignored=" + strconv.Itoa(c.Ignored)
}

// ProbeResult is one probe's outcome plus whatever it learned.
type ProbeResult struct {
	Outcome ProbeOutcome
	// Cursors are meaningful only for ProbeServed.
	Cursors Cursors
	// Err is the failure behind any outcome other than served.
	Err error
}

// ProbeIO is the two IO operations a probe performs. They are injected rather
// than called directly so the outcome table is provable without a proxy, a
// listener or a database anywhere in the test binary: the classification is the
// part with the bugs, and it is pure.
type ProbeIO struct {
	// Session performs the MySQL handshake and reads both cursors over one
	// pinned connection.
	Session func(ctx context.Context) (Cursors, error)
	// Dial is a bare TCP connect-and-close, used only to disambiguate a failed
	// session: something that accepts is a live proxy, and something that
	// refuses is not listening at all.
	Dial func(ctx context.Context) error
}

// Probe asks a proxy's data port which of the three states it is in.
//
// The session runs FIRST and the bare dial only if it failed, so a healthy
// endpoint costs the backend exactly one session rather than two. The dial is
// what keys the two failure arms apart: the design deliberately does not branch
// on driver error text, because "connection refused" and "EOF" are the driver's
// spelling of the day, while "did anything accept me" is a property of the
// proxy.
func Probe(ctx context.Context, probeIO ProbeIO) ProbeResult {
	if probeIO.Session == nil {
		return ProbeResult{Err: errors.New("proxyendpoint: probe has no session to run")}
	}
	sessionCtx, cancel := context.WithTimeout(ctx, ProbeSessionTimeout)
	defer cancel()
	cursors, sessionErr := probeIO.Session(sessionCtx)
	if sessionErr == nil {
		return ProbeResult{Outcome: ProbeServed, Cursors: cursors}
	}
	var dialErr error
	if IsConnectionLevel(sessionErr) && probeIO.Dial != nil {
		dialCtx, dialCancel := context.WithTimeout(ctx, ProbeDialTimeout)
		defer dialCancel()
		dialErr = probeIO.Dial(dialCtx)
	}
	return ProbeResult{Outcome: ClassifyProbe(sessionErr, dialErr), Err: sessionErr}
}

// ClassifyProbe maps a session error and the confirming dial's result onto an
// outcome. It is pure, and it is where the three-way split lives.
//
// A session error that is not connection-level — an unknown database, a denied
// login, a canceled caller — is deliberately ProbeUnknown rather than being
// forced into one of the proxy states. Those errors say something about the
// database or the caller, and a probe that reported them as "the proxy is fine"
// or "the proxy is gone" would be wrong in both directions.
func ClassifyProbe(sessionErr, dialErr error) ProbeOutcome {
	switch {
	case sessionErr == nil:
		return ProbeServed
	case !IsConnectionLevel(sessionErr):
		return ProbeUnknown
	case dialErr != nil:
		return ProbeRefused
	default:
		return ProbeAcceptedNoGreeting
	}
}

// IsConnectionLevel reports whether err is about the connection rather than
// about the statement or the data.
//
// The set is matched by sentinel and by type, not by text: go-sql-driver wraps a
// server that hangs up before the greeting as ErrInvalidConn or a bare io.EOF
// depending on how far the handshake got, and database/sql retries and rewraps
// on top of that. A text match over that surface is a guess that goes stale on
// a driver bump; errors.Is over exported sentinels does not.
func IsConnectionLevel(err error) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		driver.ErrBadConn,
		mysql.ErrInvalidConn,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EPIPE,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// DefaultProbeIO builds the real IO for one endpoint: a pooled MySQL session
// against the proxy's loopback port as `root` with no password — the same
// credentials bd's own CLI uses over the same proxy — and a bare TCP dial.
//
// It borrows one connection from the shared pool and returns it before it
// returns. That matters for more than tidiness: any open TCP connection, pooled
// or not, keeps bd's idle watcher from arming, so a reader that held one could
// defeat a finite idle timeout the operator asked for. The borrowed connection
// goes back to a pool whose own idle bound (doltpool's connMaxIdleTime) is
// strictly below bd's default 30s window, so a probe can delay a retirement by
// that bound at worst and can never prevent one.
func DefaultProbeIO(port int, database string) ProbeIO {
	addr := net.JoinHostPort(Host, strconv.Itoa(port))
	return ProbeIO{
		Session: func(ctx context.Context) (Cursors, error) {
			return readCursors(ctx, port, database)
		},
		Dial: func(ctx context.Context) error {
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			return conn.Close()
		},
	}
}

// ProbeEndpoint probes a validated endpoint's data port for one database.
func ProbeEndpoint(ctx context.Context, ep Endpoint, database string) ProbeResult {
	return Probe(ctx, DefaultProbeIO(ep.Record.Port, database))
}

// readCursors reads both schema cursors over one pinned connection.
//
// One connection, not two: Dolt pins a session to the catalog snapshot it had
// when a statement failed, so an existence probe and a read that disagreed
// about which connection they ran on could report a table as absent that the
// other connection can see. The existence probe itself is bd's own ordering
// (internal/storage/schema/schema.go) and exists for the harsher version of the
// same hazard: a bare SELECT against a not-yet-created cursor table poisons the
// pooled connection for the rest of its life.
func readCursors(ctx context.Context, port int, database string) (Cursors, error) {
	var cursors Cursors
	db, err := doltpool.Open(Host, strconv.Itoa(port), "root", "", database)
	if err != nil {
		return cursors, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return cursors, err
	}
	defer conn.Close() //nolint:errcheck // returning a borrowed connection to the pool
	if err := conn.PingContext(ctx); err != nil {
		return cursors, err
	}
	if cursors.Main, err = readCursor(ctx, conn, cursorTableMain, mainCursorQuery); err != nil {
		return cursors, err
	}
	if cursors.Ignored, err = readCursor(ctx, conn, cursorTableIgnored, ignoredCursorQuery); err != nil {
		return cursors, err
	}
	return cursors, nil
}

// readCursor reads one cursor table's highest applied version, treating a table
// that does not exist as version 0 — which is what it means: a database that
// predates that lane has applied none of it.
func readCursor(ctx context.Context, conn *sql.Conn, table, query string) (int, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, cursorExistsQuery, table).Scan(&exists); err != nil {
		return 0, fmt.Errorf("probing %s existence: %w", table, err)
	}
	if exists == 0 {
		return 0, nil
	}
	var version int
	if err := conn.QueryRowContext(ctx, query).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s version: %w", table, err)
	}
	return version, nil
}
