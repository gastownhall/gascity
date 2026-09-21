package proxyendpoint

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	mysql "github.com/go-sql-driver/mysql"
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

// probeUser is the login the probe uses. bd's own proxied CLI speaks to the
// proxy as `root` with no password, so this is the account that exists rather
// than one gc chose.
const probeUser = "root"

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
	// ProbeUnknown is the zero value, and also the honest answer whenever the
	// session failed for a reason that is not the proxy's — a missing database,
	// a denied login, a caller's canceled context, and above all the probe's OWN
	// expired budget. It is never a conclusion about the proxy, and it is the
	// only outcome a reader may reach by running out of time.
	ProbeUnknown ProbeOutcome = iota
	// ProbeRefused means the kernel refused the connection: ECONNREFUSED, and
	// nothing else. On a record whose process is gone this is the ordinary
	// stopped state; on a live same-generation record it is bd's teardown
	// window, where the listener is already closed and the record is removed
	// only after the backend's shutdown GC finishes. A dial that merely ran out
	// of time proves nothing about a listener and is ProbeUnknown.
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
// The FIRST question is whether the probe ran out of its own time, because that
// error is the one the probe manufactures itself and the only one that says
// nothing whatever about the endpoint. context.DeadlineExceeded implements
// net.Error — Timeout() and Temporary() are on it — so a classifier that reached
// for net.Error first read the probe's own two-second budget as a wire failure
// and then, with the confirming dial succeeding against a perfectly healthy
// proxy, reported accepted_no_greeting: the zombie signature, on a proxy that is
// serving bd fine. Under load, an information_schema scan across a city root's
// databases passes two seconds without anything being wrong. The escalation that
// reads it is `bd dolt stop` on a live proxy, which is why the order of these
// arms is a correctness property and not a style.
//
// A session error that is not connection-level — an unknown database, a denied
// login, a canceled caller — is ProbeUnknown for the same reason: those errors
// say something about the database or the caller, and a probe that reported them
// as "the proxy is fine" or "the proxy is gone" would be wrong in both
// directions.
//
// The two proxy verdicts are both positive claims and both need positive
// evidence. accepted_no_greeting requires a real wire failure (an io.EOF, a
// driver ErrInvalidConn, a net.OpError) AND a dial that something accepted;
// refused requires the kernel's ECONNREFUSED. A confirming dial that timed out
// is neither: it is a busy accept queue, and on a live same-generation record
// calling that "refused" would name a healthy proxy as draining.
func ClassifyProbe(sessionErr, dialErr error) ProbeOutcome {
	switch {
	case sessionErr == nil:
		return ProbeServed
	case IsIndeterminate(sessionErr):
		return ProbeUnknown
	case !IsConnectionLevel(sessionErr):
		return ProbeUnknown
	case dialErr == nil:
		return ProbeAcceptedNoGreeting
	case errors.Is(dialErr, syscall.ECONNREFUSED):
		return ProbeRefused
	default:
		return ProbeUnknown
	}
}

// IsIndeterminate reports whether err is the probe's own clock or its caller's
// cancellation rather than anything the endpoint did.
//
// It is the guard in front of every other classification arm. The probe imposes
// its own deadlines (ProbeSessionTimeout, ProbeDialTimeout) and its caller can
// cancel at any moment; both surface as errors that satisfy net.Error, and
// neither is evidence about a proxy. Every one of these maps to ProbeUnknown,
// which is the only outcome that carries no claim.
func IsIndeterminate(err error) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{context.DeadlineExceeded, context.Canceled, os.ErrDeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	// A driver or a listener may report its own timeout type rather than one of
	// the sentinels above; a timeout is a timeout however it is spelled.
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// IsConnectionLevel reports whether err is about the connection rather than
// about the statement or the data.
//
// The set is matched by sentinel and by type, not by text: go-sql-driver wraps a
// server that hangs up before the greeting as ErrInvalidConn or a bare io.EOF
// depending on how far the handshake got, and database/sql retries and rewraps
// on top of that. A text match over that surface is a guess that goes stale on
// a driver bump; errors.Is over exported sentinels does not.
//
// "About the connection" means the peer did something, so a timeout is excluded:
// see IsIndeterminate.
func IsConnectionLevel(err error) bool {
	if err == nil {
		return false
	}
	// The probe's own deadline and its caller's cancellation are not the wire.
	// This arm is FIRST because context.DeadlineExceeded satisfies both the
	// net.Error and the timeout checks below, so any later placement would let
	// the probe's own clock be read as a proxy state.
	if IsIndeterminate(err) {
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

// DefaultProbeIO builds the real IO for one endpoint: a private MySQL session
// against the proxy's loopback port as `root` with no password — the same
// credentials bd's own CLI uses over the same proxy — and a bare TCP dial.
//
// "Private" is the load-bearing word, and it is why this does not reach for
// internal/doltpool. That registry is a process-lifetime cache: its handles must
// never be closed, it retains up to maxIdleConns connections per endpoint, and a
// returned connection stays open for the registry's idle bound (20s) or until
// the process exits. bd's idle watcher counts every accepted TCP connection,
// pooled-idle or not, and cannot arm while one is open, so a probe that parked a
// connection there would defer the very retirement the diagnostic reports —
// through the rest of a ~40-check doctor run, and even after a check doctor has
// already given up on. The probe therefore opens its own unregistered handle
// with no idle slot at all and closes it before it returns: no connection to bd
// survives the check that made it.
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

// readCursors reads both schema cursors over one pinned private connection.
//
// One connection, not two: Dolt pins a session to the catalog snapshot it had
// when a statement failed, so an existence probe and a read that disagreed
// about which connection they ran on could report a table as absent that the
// other connection can see. The existence probe itself is bd's own ordering
// (internal/storage/schema/schema.go) and exists for the harsher version of the
// same hazard: a bare SELECT against a not-yet-created cursor table poisons the
// pooled connection for the rest of its life.
func readCursors(ctx context.Context, port int, database string) (Cursors, error) {
	connector, err := probeConnector(port, database)
	if err != nil {
		return Cursors{}, err
	}
	return readCursorsOver(ctx, connector)
}

// probeConnector builds the driver connector for one probe session.
//
// It is a connector rather than a DSN because sql.OpenDB over a connector is the
// one way to get a *sql.DB no registry owns, which is the whole point of it. The
// timeouts are the probe's own budget rather than the pool's minutes: a
// diagnostic that could outlive its own deadline through a driver-level read
// timeout would be a diagnostic with no bound at all.
func probeConnector(port int, database string) (driver.Connector, error) {
	cfg := mysql.NewConfig()
	cfg.User = probeUser
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(Host, strconv.Itoa(port))
	cfg.DBName = database
	cfg.Timeout = ProbeSessionTimeout
	cfg.ReadTimeout = ProbeSessionTimeout
	cfg.WriteTimeout = ProbeSessionTimeout
	cfg.AllowNativePasswords = true
	return mysql.NewConnector(cfg)
}

// readCursorsOver runs one probe session over connector and closes everything it
// opened before it returns.
//
// The closes are the contract rather than housekeeping, so they are
// deterministic and they are ordered: the pinned connection goes first — with no
// idle slot to go back to, that closes the socket — and then the handle itself,
// which nothing else holds and so cannot outlive this call. A caller's canceled
// or expired context reaches the same returns through db.Conn and the queries,
// so a probe the caller has already abandoned still closes its session here.
func readCursorsOver(ctx context.Context, connector driver.Connector) (Cursors, error) {
	var cursors Cursors
	db := sql.OpenDB(connector)
	// One connection, never idle: the probe needs exactly one session, and it
	// must leave nothing behind for anything to reuse.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close() //nolint:errcheck // the probe's own handle, closed on every path
	conn, err := db.Conn(ctx)
	if err != nil {
		return cursors, err
	}
	defer conn.Close() //nolint:errcheck // closes the socket: this handle retains no idle connection
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
