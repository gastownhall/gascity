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
	"strings"
	"sync"
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
	// ProbeDriverTimeoutSlack is how far the driver's own socket deadlines sit
	// ABOVE the session budget, and it is a correctness margin rather than a
	// tuning knob.
	//
	// go-sql-driver arms SetReadDeadline(now+ReadTimeout) before every read
	// (connection.go readWithTimeout) and, on ANY read error, readPacket closes
	// the connection and returns mc.canceled — the context error — only if the
	// context watcher has ALREADY fired; otherwise it logs the real error and
	// returns the sentinel mysql.ErrInvalidConn (packets.go readPacket). The
	// context path is two scheduling hops longer (timer -> AfterFunc closes
	// Done() -> watcher goroutine -> mc.cancel), so with the two deadlines set
	// equal the socket deadline usually wins and a slow proxy's session comes
	// back spelled as a connection-level failure. The confirming dial then
	// succeeds, and the verdict is accepted_no_greeting: the zombie signature
	// §3.4 escalates to `bd ping` -> `recover` (`bd dolt stop`), on a proxy that
	// is merely slow. Measured 5 of 8 probes against a silent listener at
	// production budgets.
	//
	// Keeping the driver's deadlines strictly above the session budget makes the
	// context watcher the thing that ends a slow session, so the error carries
	// the fact the probe owns — its own clock ran out. The driver deadlines stay
	// in place as a backstop for the case the watcher cannot cover: a read that
	// blocks with no context deadline at all.
	ProbeDriverTimeoutSlack = 1 * time.Second
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
	columnExistsQuery  = "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?"
	// cursorExistsWithHeadQuery is cursorExistsQuery with the database's HEAD
	// commit hash riding along as a second column. It is the FIRST statement of
	// every probe session (the main lane's existence check), so the HEAD
	// observation costs no statement, no round trip and no session of its own
	// (council pr2 D-F3).
	//
	// DOLT_HASHOF('HEAD') is a Dolt system function over the session's own
	// DATABASE(), not a table read, so it can neither miss nor poison the
	// session's catalog snapshot the way a SELECT against an absent table would.
	// It is also a function the linked library itself calls unconditionally on
	// the server path (beads v1.3.0 internal/storage/dolt/versioned.go
	// GetCurrentCommit, and schema/lock.go's fresh-bootstrap capture), so an
	// engine that could not answer it is one the library does not support
	// either. An aggregate beside a column-free scalar function is one row even
	// when the table is absent, which is what keeps the existence answer intact.
	cursorExistsWithHeadQuery = "SELECT COUNT(*), DOLT_HASHOF('HEAD') FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
)

// The IGNORED lane's sentinels, mirrored from the linked library.
//
// They are here because the number in ignored_schema_migrations is NOT the
// number the library acts on. beads' migrationSource.currentVersion applies a
// "cursor reality floor": before it believes a cursor it asks whether the live
// schema corroborates it, and when a sentinel is missing it clamps the cursor
// down (beads v1.3.0 internal/storage/schema/schema.go, currentVersion ->
// cursorRealityFloor, and ignoredSource's sentinelTables/sentinelColumns at
// :290-311). beads documents both contradicted shapes as real in the field: a
// historical ignored-v16 ordinal collision leaves `leases` present without the
// column 0016 adds, and a database materialized out of band can be missing the
// wisps tables altogether.
//
// A reader that compares only the raw MAX therefore compares a number the
// library does not use. On a database at raw ignored=26 with `leases.
// granted_node` absent the library computes min(26, 11) = 11, decides the
// ignored lane is not at latest, and MigrateUp replays 0012-0025 — against a
// database bd owns, from a handle an embedder opened to read.
//
// The floor values are the library's, not gc's. They are EXPORTED so the drift
// pin can live in internal/beads beside the cursor pin — this package cannot
// import beadstest without a cycle — and because they are part of the gate's
// stated contract rather than an implementation detail of the session.
var ignoredSentinelTables = []string{"wisps", "wisp_dependencies"}

// IgnoredSentinelTables returns the ignored lane's sentinel TABLES, copied so a
// caller cannot edit the library's facts.
func IgnoredSentinelTables() []string {
	return append([]string(nil), ignoredSentinelTables...)
}

const (
	// IgnoredSentinelColumnTable is the table carrying the ignored lane's
	// single sentinel COLUMN.
	IgnoredSentinelColumnTable = "leases"
	// IgnoredSentinelColumnName is that sentinel column.
	IgnoredSentinelColumnName = "granted_node"
	// IgnoredSentinelColumnFloor is beads' replayFloor for that sentinel: the
	// highest ignored version still believable when the column is absent.
	IgnoredSentinelColumnFloor = 11
	// IgnoredSentinelTableFloor is the floor for a missing sentinel TABLE. The
	// library floors those at zero: the absence of ignored/0001's own tables is
	// evidence against the whole series.
	IgnoredSentinelTableFloor = 0
)

// ErrNoDatabase reports a probe asked to read cursors without naming a database.
//
// The cursor reads are scoped by DATABASE(): with no database selected it is
// NULL, both existence probes count zero rows, both cursors read zero, and the
// probe reports `served` with main=0 ignored=0 — a proxy that answered and a
// database that is catastrophically behind, which is not what happened. The
// design's own sketch of the session (doltpool.Open(host, port, "root", "", ""))
// invites exactly that call, so the probe refuses it rather than answering it.
// The refusal is not connection-level and not a timeout, so it classifies as
// ProbeUnknown: it says something about the caller, not about the proxy.
var ErrNoDatabase = errors.New("proxyendpoint: probe requires a database name")

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
	// closed it without completing the MySQL handshake. That is the signature of
	// a live proxy whose backend dial failed: the proxy parses no wire protocol,
	// so a dead Dolt child shows up as an accept followed by a close.
	//
	// The token reads as "no greeting arrived", and it also covers a greeting
	// that arrived before the close, because the driver spells the two the same
	// way: whether the peer closes before writing HandshakeV10 or after writing
	// it and before answering the handshake response, the read fails with
	// `unexpected EOF` and go-sql-driver returns mysql.ErrInvalidConn. There is
	// nothing in the session error to tell them apart, and the confirming dial
	// succeeds either way.
	//
	// That costs nothing here: a backend that greets and then hangs up is
	// disturbed in the same way and wants the same response
	// (backend_unreachable, three in a row before one bd ping, recover only if
	// the ping fails). A reader that needs the narrower fact — greeting bytes
	// seen or not — has to read the wire itself, which this probe deliberately
	// does not do.
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

// CursorReality is how much of a database's IGNORED-lane cursor the live schema
// corroborates — the embedder-side mirror of beads' cursorRealityFloor.
//
// It is a separate type rather than a field on Cursors on purpose. Cursors is
// the pair a diagnostic REPORTS (it is serialized into the doctor payload and
// the OpenAPI schema), and the raw on-disk numbers are what an operator needs to
// see there. The reality is what the GATE must decide on, and conflating the two
// would either hide the disk truth from the payload or move a private clamp into
// a public schema.
//
// Limited false means nothing was missing: believe the cursor as read, which is
// the shape of every healthy database.
//
// # Its zero value is REFUSED, not believed (council pr2 D-F11)
//
// "Limited false" is also what a CursorReality nobody filled in says, and a
// gate that read the zero value as "nothing was missing" compared exactly the
// raw cursor A-F2 exists to stop comparing. ProbeIO.Session is an injection
// seam and ProbeResult a plain struct, so a second Session — PR3's write arm,
// an acceptance seam — that populated Cursors and forgot Reality reopened A-F2
// with no compile error and no failing test.
//
// So a reality also records whether a session actually EVALUATED it. The mark
// is unexported: in production only readCursorsOver sets it, on every session
// that completed its reads (including the cursor-0 case, where the library too
// has nothing to corroborate). The admission gate refuses an unchecked reality
// with a non-terminal verdict. A test stub gets a checked one only by asking
// for it by name, through ServedProbeForTest.
type CursorReality struct {
	// Limited reports that a sentinel the library probes is absent, so the
	// library will disbelieve the cursor down to Floor.
	Limited bool
	// Floor is the highest ignored-lane version still believable.
	Floor int
	// Missing names the sentinel whose absence set the floor, for a message an
	// operator can act on.
	Missing string

	// checked reports that a probe session evaluated this reality. See the
	// type doc: the zero value is refused.
	checked bool
}

// Checked reports whether a probe session actually evaluated this reality. An
// unchecked reality is not evidence: a gate must refuse it rather than read
// its Limited=false as "nothing was missing".
func (r CursorReality) Checked() bool { return r.checked }

// EffectiveIgnored is the ignored-lane cursor the LINKED LIBRARY will compute
// from this database, which is not always the number on disk.
//
// This is the number a schema gate must compare, because it is the number
// migrationSource.atLatest asks about and therefore the number that decides
// whether MigrateUp replays the ignored series.
func (r CursorReality) EffectiveIgnored(raw int) int {
	if r.Limited && r.Floor < raw {
		return r.Floor
	}
	return raw
}

// String renders the clamp for a message, or "" when nothing was missing.
func (r CursorReality) String() string {
	if !r.Limited {
		return ""
	}
	return "the ignored lane's sentinel " + r.Missing +
		" is absent, so the linked library believes that cursor only up to " + strconv.Itoa(r.Floor)
}

// CursorReport is everything one probe session reads about a database's schema:
// the two cursors as they sit on disk, how much of the ignored one the live
// schema corroborates, and where HEAD was.
type CursorReport struct {
	Cursors Cursors
	Reality CursorReality
	// Head is the database's HEAD commit hash as of this session. See
	// ProbeResult.Head.
	Head string
}

// ProbeResult is one probe's outcome plus whatever it learned.
type ProbeResult struct {
	Outcome ProbeOutcome
	// Cursors are meaningful only for ProbeServed.
	Cursors Cursors
	// Reality is the ignored lane's cursor reality, meaningful only for
	// ProbeServed. A gate must read it: see CursorReality.
	Reality CursorReality
	// Head is the database's HEAD commit hash at probe time, meaningful only
	// for ProbeServed. It is read in the same statement as the main lane's
	// existence check (cursorExistsWithHeadQuery), so a served probe always
	// carries one.
	//
	// It is not schema evidence and no admission decision is made from it.
	// It exists so a caller can re-read the same value AFTER opening the
	// library and see whether the open moved HEAD, which catches every write
	// the schema gate cannot see THAT COMMITS. It sees nothing on the
	// dolt_ignore'd plane, which is never committed (council pr2 E-S4); see
	// ReadPostOpen and beads.ProxiedOpenUnmoved for the half that reads it.
	Head string
	// Err is the failure behind any outcome other than served.
	Err error
}

// ProbeIO is the two IO operations a probe performs. They are injected rather
// than called directly so the outcome table is provable without a proxy, a
// listener or a database anywhere in the test binary: the classification is the
// part with the bugs, and it is pure.
type ProbeIO struct {
	// Session performs the MySQL handshake and reads both cursors — and the
	// ignored lane's cursor reality — over one pinned connection.
	Session func(ctx context.Context) (CursorReport, error)
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
	return probeWithBudget(ctx, probeIO, ProbeSessionTimeout)
}

// probeWithBudget is Probe with the session budget injected, so a test can pin
// the classification on a real socket without paying the production budget once
// per probe. Production has exactly one budget: ProbeSessionTimeout.
func probeWithBudget(ctx context.Context, probeIO ProbeIO, budget time.Duration) ProbeResult {
	if probeIO.Session == nil {
		return ProbeResult{Err: errors.New("proxyendpoint: probe has no session to run")}
	}
	sessionCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started := time.Now()
	report, sessionErr := probeIO.Session(sessionCtx)
	spent := time.Since(started)
	if sessionErr == nil {
		return ProbeResult{Outcome: ProbeServed, Cursors: report.Cursors, Reality: report.Reality, Head: report.Head}
	}
	// The budget expiring is a fact this function owns, and it outranks every
	// spelling the driver may have put on the error. go-sql-driver turns a socket
	// read deadline into mysql.ErrInvalidConn (see ProbeDriverTimeoutSlack), and
	// a classifier that believed that spelling reported the zombie signature for
	// a proxy that had merely not answered yet. Asking the clock instead is
	// unfalsifiable: if this budget is gone, nothing about the session can be
	// evidence about the endpoint, and no confirming dial is worth spending on it.
	//
	// The budget is read two ways because the two readings can disagree by a
	// scheduling hop. sessionCtx.Err() is the context's own verdict, but it is set
	// by a time.AfterFunc goroutine, and on a loaded box the netpoller can hand a
	// socket deadline to the reader before that callback runs — measured 1 in 8
	// with the deadlines equal, which is how this arrives wearing the driver's
	// spelling. A session that consumed the whole budget is the same fact read
	// off the clock, and it cannot lose that race.
	if sessionCtx.Err() != nil || spent >= budget {
		return ProbeResult{Outcome: ProbeUnknown, Err: sessionErr}
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
// evidence. accepted_no_greeting requires a failure that could only have
// happened after something accepted the connection (see IsPostAcceptFailure) AND
// a dial that something accepted; refused requires the kernel's ECONNREFUSED. A
// confirming dial that timed out is neither: it is a busy accept queue, and on a
// live same-generation record calling that "refused" would name a healthy proxy
// as draining.
func ClassifyProbe(sessionErr, dialErr error) ProbeOutcome {
	switch {
	case sessionErr == nil:
		return ProbeServed
	case IsIndeterminate(sessionErr):
		return ProbeUnknown
	case !IsConnectionLevel(sessionErr):
		return ProbeUnknown
	case dialErr == nil:
		// "It accepted us and said nothing" is a claim about a greeting, so the
		// session has to have got far enough to be owed one. A session refused
		// by the kernel, confirmed by a dial a just-respawned proxy accepts,
		// reaches this arm with no greeting ever attempted — a ~millisecond
		// window on a proxy restart, but the design escalates on three of these
		// in a row, and evidence that was never collected must not count as one.
		if !IsPostAcceptFailure(sessionErr) {
			return ProbeUnknown
		}
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

// IsPostAcceptFailure reports whether err could only have happened AFTER
// something accepted the connection.
//
// It is the narrower half of IsConnectionLevel, and it is what
// accepted_no_greeting needs. Every error here is a peer that had the connection
// and then dropped it: an EOF or an unexpected EOF mid-handshake, the driver's
// ErrInvalidConn (which is how go-sql-driver reports a greeting read that
// failed), a reset, a broken pipe. A dial the kernel refused or a host it could
// not reach is connection-level too, but it never reached a greeting, so it is
// not evidence that one was withheld.
//
// The distinction has a real window: on a proxy restart a session refused at
// t=0 and a confirming dial the new proxy accepts at t=1ms would otherwise be
// reported as the zombie signature with nothing having been read at all.
func IsPostAcceptFailure(err error) bool {
	if err == nil || IsIndeterminate(err) {
		return false
	}
	for _, sentinel := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		driver.ErrBadConn,
		mysql.ErrInvalidConn,
		syscall.ECONNRESET,
		syscall.EPIPE,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
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
	return probeIOWithDriverTimeout(port, database, probeDriverTimeout(ProbeSessionTimeout))
}

// probeIOWithDriverTimeout is DefaultProbeIO with the driver's socket deadlines
// injected, so a test can drive the real driver over a real socket with the
// deadlines it wants — including the equal-budget configuration this package
// used to ship, which must classify as ProbeUnknown through the context guard in
// probeWithBudget rather than through this margin.
func probeIOWithDriverTimeout(port int, database string, driverTimeout time.Duration) ProbeIO {
	addr := net.JoinHostPort(Host, strconv.Itoa(port))
	return ProbeIO{
		Session: func(ctx context.Context) (CursorReport, error) {
			return readCursors(ctx, port, database, driverTimeout)
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
//
// The database is required: the cursors are DATABASE()-scoped, so a probe with
// none selected would report served with both cursors at zero. Callers get
// ProbeUnknown and ErrNoDatabase instead. gc's own caller cannot reach it — the
// connection target defaults the name to "beads" — which is precisely why the
// refusal lives here rather than in a comment.
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
func readCursors(ctx context.Context, port int, database string, driverTimeout time.Duration) (CursorReport, error) {
	if strings.TrimSpace(database) == "" {
		// Cursors read against no database are zeros, not evidence.
		return CursorReport{}, ErrNoDatabase
	}
	connector, err := probeConnector(port, database, driverTimeout)
	if err != nil {
		return CursorReport{}, err
	}
	return readCursorsOver(ctx, connector)
}

// probeDriverTimeout is the driver-level socket deadline for a session budget:
// strictly above it, so the context watcher is what ends a slow session and the
// driver's deadlines are only the backstop. See ProbeDriverTimeoutSlack.
func probeDriverTimeout(budget time.Duration) time.Duration {
	return budget + ProbeDriverTimeoutSlack
}

// probeConnector builds the driver connector for one probe session.
//
// It is a connector rather than a DSN because sql.OpenDB over a connector is the
// one way to get a *sql.DB no registry owns, which is the whole point of it. The
// timeouts are minutes below the pool's: a diagnostic that could outlive its own
// deadline through a driver-level read timeout would be a diagnostic with no
// bound at all. They are also strictly ABOVE the session budget the context
// carries, which is the F1 fix — see ProbeDriverTimeoutSlack for why an equal
// deadline made a slow proxy read as a dead one.
func probeConnector(port int, database string, driverTimeout time.Duration) (driver.Connector, error) {
	cfg := mysql.NewConfig()
	cfg.User = probeUser
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(Host, strconv.Itoa(port))
	cfg.DBName = database
	cfg.Timeout = driverTimeout
	cfg.ReadTimeout = driverTimeout
	cfg.WriteTimeout = driverTimeout
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
//
// One probe is exactly one TCP session, and the dial is made HERE, outside
// database/sql, rather than by db.Conn. db.Conn runs its acquisition through
// DB.retry, which re-dials on any error that errors.Is driver.ErrBadConn — up
// to maxBadConnRetries (2) more times — so a connector whose handshake failure
// is spelled ErrBadConn would cost bd's proxy three accepts for one probe. That
// is three sessions against bd's idle watcher, three greetings withheld where
// the probe reports one, and three rungs of the zombie ladder from one check.
// go-sql-driver v1.10.0 spells a failed greeting ErrInvalidConn, so today's
// driver does not reach that path, but earlier releases rewrote exactly that
// error to ErrBadConn "for sql.Driver to retry", and nothing in the probe's
// contract may rest on a driver's spelling of the day. Dialing directly returns
// the first failure to the classifier as it was produced, and the handle is
// then opened over a oneSessionConnector that can hand database/sql that one
// connection and never dial another.
//
// Everything after acquisition runs on the pinned *sql.Conn, where database/sql
// does not retry: Conn.PingContext and Conn.QueryContext go straight to the
// driver connection they hold (pingDC, queryDC), and a bad-connection error
// there is returned and the connection discarded.
func readCursorsOver(ctx context.Context, connector driver.Connector) (CursorReport, error) {
	var report CursorReport
	// A caller who has already given up is owed no dial at all.
	if err := ctx.Err(); err != nil {
		return report, err
	}
	session, err := connector.Connect(ctx)
	if err != nil {
		return report, err
	}
	one := &oneSessionConnector{session: session, driver: connector.Driver()}
	db := sql.OpenDB(one)
	// One connection, never idle: the probe needs exactly one session, and it
	// must leave nothing behind for anything to reuse.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	defer db.Close() //nolint:errcheck // the probe's own handle, closed on every path
	// If database/sql never took the session — a context that expired between
	// the dial and db.Conn — nothing else will close it.
	defer one.closeUntaken()
	conn, err := db.Conn(ctx)
	if err != nil {
		return report, err
	}
	defer conn.Close() //nolint:errcheck // closes the socket: this handle retains no idle connection
	if err := conn.PingContext(ctx); err != nil {
		return report, err
	}
	if report.Cursors.Main, report.Head, err = readMainCursorAndHead(ctx, conn); err != nil {
		return report, err
	}
	if report.Cursors.Ignored, err = readCursor(ctx, conn, cursorTableIgnored, ignoredCursorQuery); err != nil {
		return report, err
	}
	// The reality check is skipped for a cursor of zero, exactly as the library
	// skips it: currentVersion returns before cursorRealityFloor when the cursor
	// is 0, because there is no claim to contradict.
	if report.Cursors.Ignored > 0 {
		if report.Reality, err = readIgnoredReality(ctx, conn); err != nil {
			return report, err
		}
	}
	// The one production place a reality is marked evaluated (council pr2
	// D-F11): every read this session owed has completed.
	report.Reality.checked = true
	return report, nil
}

// errProbeRedial is what a probe's handle gets if database/sql ever asks for a
// second connection. It must never happen — every statement runs on the one
// pinned connection — and if it does it is refused rather than dialed, and it
// is deliberately NOT connection-level: a second session is gc's bug, not
// evidence about the proxy, so it classifies as unknown and never as a verdict.
var errProbeRedial = errors.New("proxyendpoint: probe session asked for a second connection; one probe is one session")

// oneSessionConnector hands database/sql the probe's one already-dialed session,
// once. It is the cap that makes a second dial impossible rather than merely
// unlikely: whatever database/sql's retry policy is, this connector has nothing
// to dial with.
type oneSessionConnector struct {
	driver driver.Driver

	mu      sync.Mutex
	session driver.Conn
	taken   bool
}

func (c *oneSessionConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.taken {
		return nil, errProbeRedial
	}
	c.taken = true
	return c.session, nil
}

func (c *oneSessionConnector) Driver() driver.Driver { return c.driver }

// closeUntaken closes the session if database/sql never took it. Once taken,
// the session is database/sql's to close, and the pinned connection's close
// does it.
func (c *oneSessionConnector) closeUntaken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.taken {
		return
	}
	c.taken = true
	_ = c.session.Close() //nolint:errcheck // an untaken session the probe is abandoning
}

// readIgnoredReality asks the live schema how much of the ignored lane's cursor
// it corroborates, over the SAME pinned connection as the cursor reads.
//
// The order is the library's: sentinel TABLES first, because they floor at zero
// and short-circuit — no column probe could lower that answer — and the sentinel
// COLUMN after. Both are point reads against information_schema, which always
// succeed, so neither can poison the session's catalog snapshot the way a bare
// SELECT against an absent table would.
func readIgnoredReality(ctx context.Context, conn *sql.Conn) (CursorReality, error) {
	for _, table := range ignoredSentinelTables {
		exists, err := readExistsCount(ctx, conn, cursorExistsQuery, table)
		if err != nil {
			return CursorReality{}, fmt.Errorf("probing ignored-lane sentinel table %s: %w", table, err)
		}
		if exists == 0 {
			return CursorReality{Limited: true, Floor: IgnoredSentinelTableFloor, Missing: table}, nil
		}
	}
	exists, err := readExistsCount(ctx, conn, columnExistsQuery, IgnoredSentinelColumnTable, IgnoredSentinelColumnName)
	if err != nil {
		return CursorReality{}, fmt.Errorf("probing ignored-lane sentinel column %s.%s: %w",
			IgnoredSentinelColumnTable, IgnoredSentinelColumnName, err)
	}
	if exists == 0 {
		return CursorReality{
			Limited: true,
			Floor:   IgnoredSentinelColumnFloor,
			Missing: IgnoredSentinelColumnTable + "." + IgnoredSentinelColumnName,
		}, nil
	}
	return CursorReality{}, nil
}

// readExistsCount runs one information_schema COUNT(*) and returns it.
func readExistsCount(ctx context.Context, conn *sql.Conn, query string, args ...any) (int, error) {
	var count int
	if err := conn.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// readMainCursorAndHead is readCursor for the main lane, with the database's
// HEAD commit hash read as a second column of the SAME existence statement.
//
// It is the session's first statement, so HEAD is observed before anything
// else this session reads and at no cost in statements: a probe session issues
// exactly what it issued before the HEAD observation existed (council pr2
// D-F3). A HEAD that cannot be read fails the session exactly as an unreadable
// cursor does — the classifier sees a server error, not a connection-level one,
// and reports ProbeUnknown, which admits nothing.
func readMainCursorAndHead(ctx context.Context, conn *sql.Conn) (int, string, error) {
	var exists int
	var head sql.NullString
	if err := conn.QueryRowContext(ctx, cursorExistsWithHeadQuery, cursorTableMain).Scan(&exists, &head); err != nil {
		return 0, "", fmt.Errorf("probing %s existence and HEAD: %w", cursorTableMain, err)
	}
	version, err := readCursorVersion(ctx, conn, cursorTableMain, mainCursorQuery, exists)
	if err != nil {
		return 0, "", err
	}
	return version, strings.TrimSpace(head.String), nil
}

// readCursor reads one cursor table's highest applied version, treating a table
// that does not exist as version 0 — which is what it means: a database that
// predates that lane has applied none of it.
func readCursor(ctx context.Context, conn *sql.Conn, table, query string) (int, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, cursorExistsQuery, table).Scan(&exists); err != nil {
		return 0, fmt.Errorf("probing %s existence: %w", table, err)
	}
	return readCursorVersion(ctx, conn, table, query, exists)
}

// readCursorVersion is the second half of a cursor read: given the existence
// count, the table's MAX(version), or 0 for a table that is not there.
func readCursorVersion(ctx context.Context, conn *sql.Conn, table, query string, exists int) (int, error) {
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
