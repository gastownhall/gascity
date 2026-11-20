package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// gc runs `bd migrate schema` only on a database it is bootstrapping (the bd
// pack's finish_bd_schema_migrations). The verb is bd's consent to a one-way
// schema change for every client of the database at once, and on any other
// database that consent is an operator's to give. gc leaves such a database
// at the schema it has and says so instead. gc start, the bead lifecycle a
// start or reload runs, and gc reload's reply each carry a notice for it, and
// gc doctor's bd-schema check lists it (bdSchemaDoctorCheck). All of them
// compare the database's migration cursors, read over SQL, with the schema of
// the beads library linked into this binary. The pinned bd can differ from
// that library (an OSS city can pin any bd), so the notice also names what the
// pinned bd reports. A rig whose database the pinned bd cannot open at all is
// left unserved rather than failing the city (bdSchemaUnservedError).

// bdSchemaScope is one bd scope whose database the bd pack's init adopts on a
// Dolt server: the only kind of database whose migration init used to run on
// every start and now leaves to an operator.
type bdSchemaScope struct {
	// label names the scope to an operator: "city" or `rig "name"`.
	label string
	// root is the scope directory.
	root string
	// rig is the rig's name, empty for the city scope.
	rig string
}

// bdSchemaGap is a bd scope whose database is not at the schema of the beads
// library linked into this binary.
type bdSchemaGap struct {
	scope bdSchemaScope
	// database and endpoint say which database it is and where it is served;
	// host, port and socket are the endpoint's parts, for a configured
	// migrate command.
	database string
	endpoint string
	host     string
	port     string
	socket   string
	// observed are the database's cursors as the linked library reads them,
	// and linked the library's own; realityNote, when not empty, says why the
	// ignored lane reads lower than the number on disk.
	observed    proxyendpoint.Cursors
	linked      proxyendpoint.Cursors
	realityNote string
	// direction is beads.ProxiedSkewDirBehind or beads.ProxiedSkewDirAhead,
	// for the first lane that differs, main before ignored.
	direction string
	// hasRemote reports that the database has a Dolt remote, which changes
	// what bd asks of the operator who migrates it (bd's #4259 gate).
	hasRemote bool
}

// behind reports whether the database is behind the linked library.
func (g bdSchemaGap) behind() bool {
	return g.direction == beads.ProxiedSkewDirBehind
}

// bdSchemaCursors renders a cursor pair the way operators write a schema:
// v<main>/<ignored>.
func bdSchemaCursors(c proxyendpoint.Cursors) string {
	return fmt.Sprintf("v%d/%d", c.Main, c.Ignored)
}

// bdSchemaCity is what a notice needs to know about the city a gap is in.
type bdSchemaCity struct {
	path string
	// split reports a city whose gc bd is an allowlist that refuses
	// `migrate` (see bd_split_allowlist.go).
	split bool
	// allowBehind reports the city's beads.allow_schema_behind_migrate
	// opt-in, under which gc's own store open migrates a behind database.
	allowBehind bool
	// migrateCommand is the city's beads.migrate_command template, empty for
	// gc's default command.
	migrateCommand string
}

// newBdSchemaCity resolves what a notice needs to know about the city at
// cityPath from its config.
func newBdSchemaCity(cityPath string, cfg *config.City) bdSchemaCity {
	city := bdSchemaCity{
		path:        cityPath,
		split:       workStoreClassHolds(cfg) != nil,
		allowBehind: cityAllowSchemaBehindMigrate(cityPath, cfg),
	}
	if cfg != nil {
		city.migrateCommand = cfg.Beads.MigrateCommand
	}
	return city
}

// defaultMigrateCommand is gc's own command for migrating the gap's database
// with the pinned bd. Where gc bd forwards `migrate`, it is gc bd, which projects the
// scope's own store, server and credentials over whatever the shell holds.
// A split city's gc bd refuses `migrate`, so there it is the pinned bd itself,
// run from the scope directory with BEADS_DIR pinned to the scope; bd also
// takes a server from BEADS_DOLT_* variables, which the notice says to leave
// unset.
func (g bdSchemaGap) defaultMigrateCommand(city bdSchemaCity, bdPath string) string {
	if !city.split {
		if g.scope.rig == "" {
			return fmt.Sprintf("gc bd --city %s migrate schema", shellQuotePOSIXPath(city.path))
		}
		return fmt.Sprintf("gc bd --city %s --rig %s migrate schema", shellQuotePOSIXPath(city.path), shellQuotePOSIXPath(g.scope.rig))
	}
	bd := "bd"
	if strings.TrimSpace(bdPath) != "" {
		bd = shellQuotePOSIXPath(bdPath)
	}
	root := shellQuotePOSIXPath(g.scope.root)
	return fmt.Sprintf("cd %s && BEADS_DIR=%s/.beads %s migrate schema", root, root, bd)
}

// migrateCommand is the exact command the notice names for migrating the
// gap's database, followed by what an operator needs to know to run it: the
// city's beads.migrate_command rendered for the database when it is set, else
// gc's default command. A configured command almost always runs bd itself,
// which takes a server and database from BEADS_DOLT_* variables over the
// scope's own, so it carries the same shell note as a split city's default.
// A configured template that does not render for this database (config load
// renders it for sample databases, so this is rare) falls back to the default
// command and says why.
func (g bdSchemaGap) migrateCommand(city bdSchemaCity, bdPath string) string {
	if city.migrateCommand == "" {
		command := g.defaultMigrateCommand(city, bdPath)
		if city.split {
			command += " (from a shell with no BEADS_DOLT_* variables set)"
		}
		return command
	}
	bd := strings.TrimSpace(bdPath)
	if bd == "" {
		bd = "bd"
	}
	command, err := config.RenderBeadsMigrateCommand(city.migrateCommand, config.BeadsMigrateCommandData{
		City:     city.path,
		Rig:      g.scope.rig,
		ScopeDir: g.scope.root,
		BeadsDir: filepath.Join(g.scope.root, ".beads"),
		Database: g.database,
		Host:     g.host,
		Port:     g.port,
		Socket:   g.socket,
		Bd:       bd,
	})
	if err != nil {
		fallback := city
		fallback.migrateCommand = ""
		return fmt.Sprintf("%s (beads.migrate_command did not render: %v)", g.migrateCommand(fallback, bdPath), err)
	}
	return command + " (from a shell with no other BEADS_DOLT_* variables set)"
}

// twoLaneCaution is the sentence every behind notice puts ahead of its
// command: bd records a schema in two migration lanes, and a migrator that
// moves only the main one leaves the database behind, where a bd may refuse
// to open it.
func (g bdSchemaGap) twoLaneCaution() string {
	return fmt.Sprintf(" The migrator must move both lanes, main and ignored, to %s: one that moves only the main lane leaves it behind, where a bd may refuse to open it. Afterwards gc doctor --check bd-schema reads both lanes.",
		bdSchemaCursors(g.linked))
}

// notice is the operator's line for the gap: what the database is at, what gc
// expects, why gc leaves it there, and what to do. bdVersion is what the
// pinned bd reports about itself, empty when it could not be read.
func (g bdSchemaGap) notice(city bdSchemaCity, bdPath, bdVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "bd schema: %s database %q on %s is at schema %s (main/ignored migration lanes)",
		g.scope.label, g.database, g.endpoint, bdSchemaCursors(g.observed))
	if g.realityNote != "" {
		fmt.Fprintf(&b, " (%s)", g.realityNote)
	}
	relation := "behind"
	if !g.behind() {
		relation = "ahead of"
	}
	fmt.Fprintf(&b, ", %s %s, the schema of the beads library linked into gc", relation, bdSchemaCursors(g.linked))
	if v := strings.TrimSpace(bdVersion); v != "" {
		fmt.Fprintf(&b, " (the pinned bd reports %q)", v)
	}
	b.WriteString(".")
	if !g.behind() {
		fmt.Fprintf(&b, " gc's native store cannot open it, so gc reaches it through the pinned bd, which refuses it unless it knows %s: run a gc built on that schema, and a bd that knows it.",
			bdSchemaCursors(g.observed))
		return b.String()
	}
	if city.allowBehind {
		b.WriteString(" beads.allow_schema_behind_migrate is on for this city, so gc's own store migrates it the next time it opens it; that setting is the operator's consent.")
		b.WriteString(g.twoLaneCaution())
		fmt.Fprintf(&b, " To migrate it first, once every client of this database runs a bd that knows %s: %s", bdSchemaCursors(g.linked), g.migrateCommand(city, bdPath))
	} else {
		b.WriteString(" gc migrates a database only while bootstrapping it: migrating changes the schema for every client of the database at once, and a bd built for the older schema then refuses it. Until it is migrated, the pinned bd may refuse it too.")
		b.WriteString(g.twoLaneCaution())
		fmt.Fprintf(&b, " Once every client of this database runs a bd that knows %s, an operator migrates it with a bd at that schema: %s", bdSchemaCursors(g.linked), g.migrateCommand(city, bdPath))
	}
	if g.hasRemote {
		b.WriteString(" (it has a Dolt remote: bd asks the one clone that migrates for all of them to add --force and then run `bd dolt push`; follow bd's message)")
	}
	return b.String()
}

// bdSchemaScopesFor lists the city and each rig in rigs whose database the bd
// pack's init adopts on a Dolt server (bdSchemaScopeEligible).
func bdSchemaScopesFor(cityPath string, rigs []config.Rig) []bdSchemaScope {
	var scopes []bdSchemaScope
	if bdSchemaScopeEligible(cityPath, cityPath) {
		scopes = append(scopes, bdSchemaScope{label: "city", root: cityPath})
	}
	for _, rig := range rigs {
		if bdSchemaScopeEligible(cityPath, rig.Path) {
			scopes = append(scopes, bdSchemaScope{label: fmt.Sprintf("rig %q", rig.Name), root: rig.Path, rig: rig.Name})
		}
	}
	return scopes
}

// bdSchemaScopeEligible reports whether root is a scope whose init runs the
// bd pack's adopt path against a Dolt server: a bd store-contract scope that
// is neither provider-owned, proxied, embedded, DoltLite nor bound to a store
// gc does not serve, in a city that does not reach its store through a hosted
// credential provider. No other kind of scope ever had the migration step;
// an embedded store, in particular, is bd's own to migrate or refuse. A
// predicate that cannot answer leaves the scope out: the notice is advisory,
// and the scope's own init and health checks report what broke.
func bdSchemaScopeEligible(cityPath, root string) bool {
	if strings.TrimSpace(root) == "" || !scopeUsesManagedBdStoreContract(cityPath, root) || cityUsesDoltliteBeadsBackend(cityPath) {
		return false
	}
	if owned, err := scopeProviderOwned(cityPath, root); err != nil || owned {
		return false
	}
	if skips, err := scopeSkipsManagedDoltForInit(cityPath, root); err != nil || skips {
		return false
	}
	if embedded, err := scopeMetadataRecordsEmbeddedDolt(fsys.OSFS{}, root); err != nil || embedded {
		return false
	}
	if scopeUsesProxiedDoltMode(cityPath, root) {
		return false
	}
	if hosted, err := citySelectsHostedBeadsCredentialProvider(cityPath); err != nil || hosted {
		return false
	}
	return true
}

// errBdSchemaNoEndpoint is the read error for a scope whose database has no
// direct SQL endpoint to read: its scope config is not authoritative, or it
// turned out to be proxied.
var errBdSchemaNoEndpoint = errors.New("no direct Dolt endpoint resolves for this scope's database")

// readBdScopeSchemaGap reads scope's database cursors over SQL and compares
// them with the linked library's, the way the proxied lane's admission gate
// does (beads.CursorsMatchPinned, which reads the ignored lane as the library
// will). It returns found=false for a database at the library's schema, and
// an error when the database could not be read.
func readBdScopeSchemaGap(cityPath string, scope bdSchemaScope) (bdSchemaGap, bool, error) {
	var (
		gap   bdSchemaGap
		found bool
	)
	read, err := withScopeDoltConn(cityPath, scope.root, func(ctx context.Context, conn *sql.Conn, target contract.DoltConnectionTarget) error {
		report, err := proxyendpoint.ReadCursorReportOverConn(ctx, conn)
		if err != nil {
			return err
		}
		gap, found = bdSchemaGapFromReport(scope, target, report)
		if found {
			gap.hasRemote = doltDatabaseHasRemote(ctx, conn)
		}
		return nil
	})
	if err != nil {
		return bdSchemaGap{}, false, err
	}
	if !read {
		return bdSchemaGap{}, false, errBdSchemaNoEndpoint
	}
	return gap, found, nil
}

// bdSchemaGapFromReport compares a database's cursor report with the linked
// library's cursors and returns the gap, found=false when there is none.
func bdSchemaGapFromReport(scope bdSchemaScope, target contract.DoltConnectionTarget, report proxyendpoint.CursorReport) (bdSchemaGap, bool) {
	match, _, direction := beads.CursorsMatchPinned(report.Cursors, report.Reality)
	if match {
		return bdSchemaGap{}, false
	}
	main, ignored := beads.PinnedSchemaCursors()
	gap := bdSchemaGap{
		scope:    scope,
		database: target.Database,
		endpoint: bdSchemaEndpoint(target),
		host:     strings.TrimSpace(target.Host),
		port:     strings.TrimSpace(target.Port),
		socket:   strings.TrimSpace(target.Socket),
		observed: proxyendpoint.Cursors{
			Main:    report.Cursors.Main,
			Ignored: report.Reality.EffectiveIgnored(report.Cursors.Ignored),
		},
		linked:    proxyendpoint.Cursors{Main: main, Ignored: ignored},
		direction: direction,
	}
	// Explain the ignored lane only where the library reads it lower than
	// the number on disk.
	if gap.observed.Ignored < report.Cursors.Ignored {
		gap.realityNote = report.Reality.String()
	}
	return gap, true
}

// bdSchemaEndpoint names where a target's database is served.
func bdSchemaEndpoint(target contract.DoltConnectionTarget) string {
	if socket := strings.TrimSpace(target.Socket); socket != "" {
		return socket
	}
	return strings.TrimSpace(target.Host) + ":" + strings.TrimSpace(target.Port)
}

// doltDatabaseHasRemote reports whether the connection's database has a Dolt
// remote. A query that fails reads as no remote: the notice then omits the
// remote note, and bd asks for --force itself when the database needs it.
func doltDatabaseHasRemote(ctx context.Context, conn *sql.Conn) bool {
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_remotes").Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// bdVersionLine returns the first line `<bdPath> version` prints, empty when
// bdPath is empty or the command fails: the notice names what the pinned bd
// reports about itself only when it says. It is one bounded attempt with the
// init probes' first budget, not initRunVersion's retries: a notice must not
// hold up a start or a reload.
func bdVersionLine(bdPath string) string {
	if strings.TrimSpace(bdPath) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), initProbeAttemptTimeouts[0])
	defer cancel()
	cmd := initRunVersionCommandContext(ctx, bdPath, "version")
	cmd.WaitDelay = initProbeWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// bdSchemaReport is what a pass over a city's bd scopes found: the gaps, each
// one's notice in the same order, and the scopes whose database could not be
// read, for the callers that report those (gc doctor) rather than skip them
// (gc start and gc reload).
type bdSchemaReport struct {
	gaps       []bdSchemaGap
	notices    []string
	unreadable []bdSchemaUnreadable
}

// bdSchemaUnreadable is a scope whose database could not be read.
type bdSchemaUnreadable struct {
	label string
	err   error
}

// bdSchemaDeps are the reads behind a bd schema report, swapped by tests.
type bdSchemaDeps struct {
	read    func(cityPath string, scope bdSchemaScope) (bdSchemaGap, bool, error)
	binary  func(cityPath, scopeRoot string) string
	version func(bdPath string) string
}

// liveBdSchemaDeps reads real databases and runs the pinned bd.
var liveBdSchemaDeps = bdSchemaDeps{
	read:    readBdScopeSchemaGap,
	binary:  doctorScopeBdBinary,
	version: bdVersionLine,
}

// collectBdSchemaReport reads every scope and renders each gap's notice,
// asking each distinct pinned bd for its version once.
func collectBdSchemaReport(city bdSchemaCity, scopes []bdSchemaScope, deps bdSchemaDeps) bdSchemaReport {
	var report bdSchemaReport
	versions := map[string]string{}
	for _, scope := range scopes {
		gap, found, err := deps.read(city.path, scope)
		if err != nil {
			report.unreadable = append(report.unreadable, bdSchemaUnreadable{label: scope.label, err: err})
			continue
		}
		if !found {
			continue
		}
		bdPath := deps.binary(city.path, scope.root)
		version, asked := versions[bdPath]
		if !asked {
			version = deps.version(bdPath)
			versions[bdPath] = version
		}
		report.gaps = append(report.gaps, gap)
		report.notices = append(report.notices, gap.notice(city, bdPath, version))
	}
	return report
}

// bdSchemaNotices returns the notice for every bd scope of the city at
// cityPath (the city and cfg's rigs) whose database is not at the linked
// library's schema. Scopes whose database cannot be read are left out: gc
// doctor's bd-schema check names them. It returns nothing where gc runs no
// Dolt (GC_DOLT=skip).
func bdSchemaNotices(cityPath string, cfg *config.City) []string {
	if cfg == nil || gcDoltSkip() {
		return nil
	}
	scopes := bdSchemaScopesFor(cityPath, cfg.Rigs)
	if len(scopes) == 0 {
		return nil
	}
	return collectBdSchemaReport(newBdSchemaCity(cityPath, cfg), scopes, liveBdSchemaDeps).notices
}

// bdSchemaNoticesFor is where gc start, the boot lifecycle and gc reload get
// their notices, replaced by the tests that pin those call sites.
var bdSchemaNoticesFor = bdSchemaNotices

// writeBdSchemaNotices writes each notice on a line of its own after prefix.
func writeBdSchemaNotices(w io.Writer, prefix string, notices []string) {
	if w == nil {
		return
	}
	for _, notice := range notices {
		fmt.Fprintf(w, "%s%s\n", prefix, notice) //nolint:errcheck // best-effort operator notice
	}
}

// bdSchemaUnservedError is a scope whose init succeeded but whose database
// the pinned bd could not open afterwards, while the database's schema is not
// the linked library's. gc migrates only a database it is bootstrapping, so
// such a database stays as it is until an operator migrates it. The bead
// lifecycle leaves a rig in that state unserved, as it leaves any rig whose
// store will not open, rather than failing the whole city for it.
type bdSchemaUnservedError struct {
	err error
}

func (e *bdSchemaUnservedError) Error() string {
	return fmt.Sprintf("waiting for initialized bead scope readiness: %v (the database is not at the schema of the beads library linked into gc; gc doctor's bd-schema check names the command that migrates it)", e.err)
}

func (e *bdSchemaUnservedError) Unwrap() error { return e.err }

// scopeReadinessError wraps the failure of a scope's post-init readiness
// wait. It is a bdSchemaUnservedError when the scope's database reads as not
// at the linked library's schema, the likely reason the pinned bd refused it,
// and a plain error otherwise, including when the database cannot be read.
func scopeReadinessError(cityPath, dir string, err error) error {
	if _, found, readErr := liveBdSchemaDeps.read(cityPath, bdSchemaScope{root: dir}); readErr == nil && found {
		return &bdSchemaUnservedError{err: err}
	}
	return fmt.Errorf("waiting for initialized bead scope readiness: %w", err)
}
