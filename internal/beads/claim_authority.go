package beads

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The claim-authority boundary moves the decision "may this identity hold
// routed work" into the Dolt server. Versioned BEFORE INSERT / BEFORE UPDATE
// triggers on the bead tables refuse a routed claim or assignment whose
// assignee has no row in bd's dolt-ignored local_metadata table, and the
// controller publishes one row per authorized identity there. Triggers
// replicate through dolt_schemas; the rows are node-local and never create a
// commit, so a wipe of the rows fails closed for routed claims only.

// ClaimAuthorityMarker prefixes every refusal the claim-authority trigger
// raises. The DDL renderer and the classifier share it so the two cannot drift.
const ClaimAuthorityMarker = "gc-claim-authority:"

// ClaimAuthorityVersion is the trigger generation gc renders. It is part of
// every trigger name, so an upgrade installs the next generation and drops the
// previous one in one commit.
const ClaimAuthorityVersion = 1

// ClaimAuthorityHeartbeatRefresh is how often a publisher rewrites its
// heartbeat row when nothing else about it changed.
const ClaimAuthorityHeartbeatRefresh = time.Minute

// ClaimAuthorityPublisherTTL is how long a publisher heartbeat stays fresh.
// Any publisher prunes the rows of another whose heartbeat is older.
const ClaimAuthorityPublisherTTL = 15 * time.Minute

const (
	claimAuthorityTriggerPrefix = "gc_claim_authority_v"
	claimAuthorityIDKeyPrefix   = "gc.claim_authority.id."
	claimAuthorityPubKeyPrefix  = "gc.claim_authority.publisher."
	claimAuthorityLocalTable    = "local_metadata"
	claimAuthorityCommitAuthor  = "gascity <claim-authority@gascity.local>"
	// mysqlErrSignal is the errno a SIGNAL SQLSTATE '45000' carries.
	mysqlErrSignal = 1644
)

// claimAuthorityBeadTables are the bd tables that carry claimable rows.
var claimAuthorityBeadTables = []string{"issues", "wisps"}

// ErrClaimNotAuthorized reports that the store's claim-authority boundary
// refused a routed claim or assignment: the assignee is not a published
// authorized claimant. It is not "already claimed"; the bead is untouched and
// a retry after the next publish can succeed.
var ErrClaimNotAuthorized = errors.New("claim not authorized by the store's claim authority")

// ClaimNotAuthorizedError carries the server's refusal text. It matches
// ErrClaimNotAuthorized under errors.Is and unwraps to the original error.
type ClaimNotAuthorizedError struct {
	// Message is the refusal text, starting at ClaimAuthorityMarker.
	Message string
	// Err is the error the refusal was classified from.
	Err error
}

// Error returns the refusal text.
func (e *ClaimNotAuthorizedError) Error() string {
	return ErrClaimNotAuthorized.Error() + ": " + e.Message
}

// Is reports whether target is ErrClaimNotAuthorized.
func (e *ClaimNotAuthorizedError) Is(target error) bool { return target == ErrClaimNotAuthorized }

// Unwrap returns the classified error.
func (e *ClaimNotAuthorizedError) Unwrap() error { return e.Err }

// ClassifyClaimAuthorityError returns err as a *ClaimNotAuthorizedError when
// it is a claim-authority refusal: a MySQL SIGNAL (errno 1644) whose message
// carries ClaimAuthorityMarker, or any error whose text carries the marker (bd
// reports the server's refusal on stderr or in its JSON). Every other error,
// and nil, is returned unchanged.
func ClassifyClaimAuthorityError(err error) error {
	if err == nil || errors.Is(err, ErrClaimNotAuthorized) {
		return err
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == mysqlErrSignal && strings.Contains(myErr.Message, ClaimAuthorityMarker) {
		return &ClaimNotAuthorizedError{Message: refusalText(myErr.Message), Err: err}
	}
	if text := err.Error(); strings.Contains(text, ClaimAuthorityMarker) {
		return &ClaimNotAuthorizedError{Message: refusalText(text), Err: err}
	}
	return err
}

// refusalText trims text to the refusal itself: from the marker to the end of
// its line.
func refusalText(text string) string {
	if at := strings.Index(text, ClaimAuthorityMarker); at > 0 {
		text = text[at:]
	}
	if nl := strings.IndexAny(text, "\r\n"); nl >= 0 {
		text = text[:nl]
	}
	return strings.TrimSpace(text)
}

// ClaimAuthoritySource says why an identity is in a city's authority set.
type ClaimAuthoritySource string

const (
	// ClaimAuthoritySession is an identity of an open session bead.
	ClaimAuthoritySession ClaimAuthoritySource = "session"
	// ClaimAuthorityNamed is an assignee form of a configured named session.
	ClaimAuthorityNamed ClaimAuthoritySource = "named"
	// ClaimAuthorityAllow is an identity listed in [beads] claim_authority_allow.
	ClaimAuthorityAllow ClaimAuthoritySource = "allow"
)

// ClaimAuthorityEntry is one authorized claimant a city publishes.
type ClaimAuthorityEntry struct {
	Identity string
	Source   ClaimAuthoritySource
}

// ClaimAuthorityPublisher identifies the city that owns a set of rows.
type ClaimAuthorityPublisher struct {
	// Tag is the city tag (ClaimAuthorityCityTag); every row the city writes
	// ends in it, and the city prunes only rows that do.
	Tag string
	// City is the effective city name, recorded for operators.
	City string
	// Mode is the city's beads.claim_authority mode, advertised in its
	// heartbeat.
	Mode string
}

// ClaimAuthorityCityTag returns the 16-hex-character tag that scopes a city's
// rows: SHA-256 over the canonical city path and the effective city name.
func ClaimAuthorityCityTag(cityPath, cityName string) string {
	sum := sha256.Sum256([]byte(cityPath + "\x00" + cityName))
	return hex.EncodeToString(sum[:])[:16]
}

// claimAuthorityIdentityHash is the key component the trigger recomputes with
// SHA2(NEW.assignee, 256).
func claimAuthorityIdentityHash(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// ClaimAuthorityIdentityKey returns the local_metadata key that authorizes
// identity on behalf of the city tagged tag.
func ClaimAuthorityIdentityKey(identity, tag string) string {
	return claimAuthorityIDKeyPrefix + claimAuthorityIdentityHash(identity) + "." + tag
}

func claimAuthorityPublisherKey(tag string) string {
	return claimAuthorityPubKeyPrefix + tag
}

// claimAuthorityTriggerName returns the trigger name for table and event
// ("bu" or "bi") at version.
func claimAuthorityTriggerName(version int, table, event string) string {
	return fmt.Sprintf("%s%d_%s_%s", claimAuthorityTriggerPrefix, version, table, event)
}

// claimAuthorityMetaText renders the trimmed text value of metadata key on row
// alias row ("NEW", "OLD" or a table name), or the empty string when absent.
func claimAuthorityMetaText(row, key string) string {
	return fmt.Sprintf(`COALESCE(TRIM(JSON_UNQUOTE(JSON_EXTRACT(%s.metadata, '$."%s"'))), '')`, row, key)
}

// claimAuthorityRoutedSQL renders the routed predicate over row: a non-empty
// gc.routed_to, or a workflow root with a non-empty gc.run_target. It is the
// SQL form of the controller's routedToOrLegacyWorkflowTarget, rendered from
// the same beadmeta keys.
func claimAuthorityRoutedSQL(row string) string {
	return fmt.Sprintf("(%s <> '' OR (%s = '%s' AND %s <> ''))",
		claimAuthorityMetaText(row, beadmeta.RoutedToMetadataKey),
		claimAuthorityMetaText(row, beadmeta.KindMetadataKey), beadmeta.KindWorkflow,
		claimAuthorityMetaText(row, beadmeta.RunTargetMetadataKey))
}

// claimAuthorityUnauthorizedSQL renders "no authority row exists for
// NEW.assignee", as an exact key-range match on the identity hash.
func claimAuthorityUnauthorizedSQL() string {
	prefix := fmt.Sprintf("CONCAT('%s', SHA2(NEW.assignee, 256), '.')", claimAuthorityIDKeyPrefix)
	// '/' is the byte after '.', so [prefix, prefix-with-'/') is every key
	// that starts with the identity hash followed by a dot.
	upper := fmt.Sprintf("CONCAT('%s', SHA2(NEW.assignee, 256), '/')", claimAuthorityIDKeyPrefix)
	return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM `%s` WHERE `key` >= %s AND `key` < %s)", claimAuthorityLocalTable, prefix, upper)
}

// ClaimAuthorityTriggerDDL renders the CREATE TRIGGER statement for one bead
// table: the BEFORE INSERT variant when insert is true, else BEFORE UPDATE.
// The trigger signals SQLSTATE 45000 when a non-empty assignee is newly set,
// or the row enters in_progress, on routed work, and no authority row names
// the assignee. Unassigning, closing and writes to unrouted rows pass.
func ClaimAuthorityTriggerDDL(table string, insert bool) string {
	event, when, cond := "bu", "BEFORE UPDATE", fmt.Sprintf(
		"(NOT (COALESCE(OLD.assignee, '') <=> COALESCE(NEW.assignee, '')) OR (NEW.status = 'in_progress' AND NOT (COALESCE(OLD.status, '') <=> 'in_progress')))\n"+
			"     AND (%s OR %s)", claimAuthorityRoutedSQL("OLD"), claimAuthorityRoutedSQL("NEW"))
	if insert {
		event, when, cond = "bi", "BEFORE INSERT", claimAuthorityRoutedSQL("NEW")
	}
	name := claimAuthorityTriggerName(ClaimAuthorityVersion, table, event)
	return fmt.Sprintf("CREATE TRIGGER `%s` %s ON `%s` FOR EACH ROW\n"+
		"BEGIN\n"+
		"  DECLARE gc_msg VARCHAR(1024);\n"+
		"  IF COALESCE(NEW.assignee, '') <> ''\n"+
		"     AND %s\n"+
		"     AND %s\n"+
		"  THEN\n"+
		"    SET gc_msg = CONCAT('%s ', NEW.assignee, ' is not an authorized claimant for routed bead ', NEW.id);\n"+
		"    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = gc_msg;\n"+
		"  END IF;\n"+
		"END", name, when, table, cond, claimAuthorityUnauthorizedSQL(), ClaimAuthorityMarker)
}

// claimAuthorityDesiredTriggers returns trigger name → DDL for the current
// version over tables.
func claimAuthorityDesiredTriggers(tables []string) map[string]string {
	out := make(map[string]string, 2*len(tables))
	for _, table := range tables {
		out[claimAuthorityTriggerName(ClaimAuthorityVersion, table, "bu")] = ClaimAuthorityTriggerDDL(table, false)
		out[claimAuthorityTriggerName(ClaimAuthorityVersion, table, "bi")] = ClaimAuthorityTriggerDDL(table, true)
	}
	return out
}

// ClaimAuthorityDB is the SQL surface the claim-authority operations need;
// *sql.DB, *sql.Conn and *sql.Tx all satisfy it.
type ClaimAuthorityDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ClaimAuthorityCapability describes whether a database can host the
// boundary.
type ClaimAuthorityCapability struct {
	// Tables are the bead tables present, in render order.
	Tables []string
	// LocalMetadata reports that bd's local_metadata table exists.
	LocalMetadata bool
	// TriggerPrivilege reports that the connected principal holds TRIGGER.
	TriggerPrivilege bool
}

// CanHost reports whether the database can host the boundary.
func (c ClaimAuthorityCapability) CanHost() bool {
	return c.LocalMetadata && c.TriggerPrivilege && slices.Contains(c.Tables, "issues")
}

// Reason names what is missing when the database cannot host the boundary.
func (c ClaimAuthorityCapability) Reason() string {
	var missing []string
	if !slices.Contains(c.Tables, "issues") {
		missing = append(missing, "no issues table")
	}
	if !c.LocalMetadata {
		missing = append(missing, "no local_metadata table")
	}
	if !c.TriggerPrivilege {
		missing = append(missing, "principal lacks TRIGGER")
	}
	return strings.Join(missing, ", ")
}

// ProbeClaimAuthorityCapability reads which bead tables and local_metadata
// exist in the connection's current database and whether the principal holds
// TRIGGER.
func ProbeClaimAuthorityCapability(ctx context.Context, db ClaimAuthorityDB) (ClaimAuthorityCapability, error) {
	var capability ClaimAuthorityCapability
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return capability, fmt.Errorf("listing tables for claim authority: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return capability, fmt.Errorf("scanning table name: %w", err)
		}
		present[strings.ToLower(name)] = true
	}
	if err := closeRows(rows); err != nil {
		return capability, fmt.Errorf("listing tables for claim authority: %w", err)
	}
	for _, table := range claimAuthorityBeadTables {
		if present[table] {
			capability.Tables = append(capability.Tables, table)
		}
	}
	capability.LocalMetadata = present[claimAuthorityLocalTable]

	grants, err := db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		// Dolt denies SHOW GRANTS to a principal without access to the
		// mysql database, and such a principal holds no TRIGGER either.
		if strings.Contains(err.Error(), "Access denied") {
			return capability, nil
		}
		return capability, fmt.Errorf("reading grants for claim authority: %w", err)
	}
	for grants.Next() {
		var line string
		if err := grants.Scan(&line); err != nil {
			_ = grants.Close()
			return capability, fmt.Errorf("scanning grant: %w", err)
		}
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "ALL PRIVILEGES") || strings.Contains(upper, "TRIGGER") {
			capability.TriggerPrivilege = true
		}
	}
	if err := closeRows(grants); err != nil {
		return capability, fmt.Errorf("reading grants for claim authority: %w", err)
	}
	return capability, nil
}

func closeRows(rows *sql.Rows) error {
	iterErr := rows.Err()
	closeErr := rows.Close()
	if iterErr != nil {
		return iterErr
	}
	return closeErr
}

// ClaimAuthorityTriggerState is what the database holds of the boundary.
type ClaimAuthorityTriggerState struct {
	// Installed names every claim-authority trigger present, any version.
	Installed []string
	// Current reports that exactly the current version's triggers for the
	// database's bead tables are installed, each matching its rendered DDL.
	Current bool
}

// Armed reports whether any claim-authority trigger is installed. The
// presence of a trigger is what arms enforcement.
func (s ClaimAuthorityTriggerState) Armed() bool { return len(s.Installed) > 0 }

// readClaimAuthorityTriggers returns name → stored DDL for every installed
// claim-authority trigger.
func readClaimAuthorityTriggers(ctx context.Context, db ClaimAuthorityDB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, fragment FROM dolt_schemas WHERE type = 'trigger'")
	if err != nil {
		return nil, fmt.Errorf("reading dolt_schemas triggers: %w", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var name, fragment string
		if err := rows.Scan(&name, &fragment); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning trigger: %w", err)
		}
		if strings.HasPrefix(name, claimAuthorityTriggerPrefix) {
			out[name] = fragment
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("reading dolt_schemas triggers: %w", err)
	}
	return out, nil
}

// ReadClaimAuthorityTriggerState reports which claim-authority triggers the
// database holds and whether they are the current rendering for tables.
func ReadClaimAuthorityTriggerState(ctx context.Context, db ClaimAuthorityDB, tables []string) (ClaimAuthorityTriggerState, error) {
	installed, err := readClaimAuthorityTriggers(ctx, db)
	if err != nil {
		return ClaimAuthorityTriggerState{}, err
	}
	state := ClaimAuthorityTriggerState{}
	for name := range installed {
		state.Installed = append(state.Installed, name)
	}
	sort.Strings(state.Installed)
	desired := claimAuthorityDesiredTriggers(tables)
	state.Current = len(installed) == len(desired)
	for name, ddl := range desired {
		if strings.TrimSpace(installed[name]) != strings.TrimSpace(ddl) {
			state.Current = false
		}
	}
	return state, nil
}

// ArmClaimAuthority installs the current triggers on tables and drops any
// other claim-authority trigger, then stages dolt_schemas and commits it, so
// the DDL never sits uncommitted for another committer to sweep up. It is a
// no-op when the database already holds exactly the current rendering, and
// reports whether it changed anything. conn must be one session: the stage and
// the commit read its working set.
func ArmClaimAuthority(ctx context.Context, conn *sql.Conn, tables []string) (bool, error) {
	installed, err := readClaimAuthorityTriggers(ctx, conn)
	if err != nil {
		return false, err
	}
	desired := claimAuthorityDesiredTriggers(tables)
	var stmts []string
	for name, stored := range installed {
		if ddl, keep := desired[name]; !keep || strings.TrimSpace(stored) != strings.TrimSpace(ddl) {
			stmts = append(stmts, "DROP TRIGGER IF EXISTS `"+name+"`")
		}
	}
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if stored, ok := installed[name]; ok && strings.TrimSpace(stored) == strings.TrimSpace(desired[name]) {
			continue
		}
		stmts = append(stmts, desired[name])
	}
	if len(stmts) == 0 {
		return false, nil
	}
	sort.SliceStable(stmts, func(i, j int) bool {
		return strings.HasPrefix(stmts[i], "DROP") && !strings.HasPrefix(stmts[j], "DROP")
	})
	if err := commitClaimAuthorityDDL(ctx, conn, stmts, fmt.Sprintf("gc: arm claim authority v%d", ClaimAuthorityVersion)); err != nil {
		return false, fmt.Errorf("arming claim authority: %w", err)
	}
	return true, nil
}

// DisarmClaimAuthority drops every claim-authority trigger through the same
// committed-DDL path ArmClaimAuthority uses, and reports whether it dropped
// any.
func DisarmClaimAuthority(ctx context.Context, conn *sql.Conn) (bool, error) {
	installed, err := readClaimAuthorityTriggers(ctx, conn)
	if err != nil {
		return false, err
	}
	if len(installed) == 0 {
		return false, nil
	}
	names := make([]string, 0, len(installed))
	for name := range installed {
		names = append(names, name)
	}
	sort.Strings(names)
	stmts := make([]string, 0, len(names))
	for _, name := range names {
		stmts = append(stmts, "DROP TRIGGER IF EXISTS `"+name+"`")
	}
	if err := commitClaimAuthorityDDL(ctx, conn, stmts, "gc: disarm claim authority"); err != nil {
		return false, fmt.Errorf("disarming claim authority: %w", err)
	}
	return true, nil
}

func commitClaimAuthorityDDL(ctx context.Context, conn *sql.Conn, stmts []string, message string) error {
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("executing %q: %w", firstLine(stmt), err)
		}
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('dolt_schemas')"); err != nil {
		return fmt.Errorf("staging dolt_schemas: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, claimAuthorityCommitAuthor); err != nil {
		return fmt.Errorf("committing dolt_schemas: %w", err)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// claimAuthorityRow is the JSON value of an identity row.
type claimAuthorityRow struct {
	Identity    string               `json:"identity"`
	City        string               `json:"city"`
	Source      ClaimAuthoritySource `json:"source"`
	PublishedAt time.Time            `json:"published_at"`
}

// ClaimAuthorityHeartbeat is a publisher's heartbeat row.
type ClaimAuthorityHeartbeat struct {
	Tag         string    `json:"-"`
	City        string    `json:"city"`
	Mode        string    `json:"mode"`
	PublishedAt time.Time `json:"published_at"`
}

// ClaimAuthorityPublishResult counts what one publish wrote.
type ClaimAuthorityPublishResult struct {
	Upserted  int
	Deleted   int
	Heartbeat bool
}

// Changed reports whether the publish wrote anything.
func (r ClaimAuthorityPublishResult) Changed() bool {
	return r.Upserted > 0 || r.Deleted > 0 || r.Heartbeat
}

// PublishClaimAuthority makes the database's rows for pub match entries:
// missing identities are inserted and, when prune is true, this city's rows
// for identities no longer in entries are deleted. Rows of other cities are
// never touched. The heartbeat is rewritten when it is missing, its mode
// changed, or it is older than ClaimAuthorityHeartbeatRefresh. Writes go to
// the dolt-ignored local_metadata table and create no commit.
func PublishClaimAuthority(ctx context.Context, db ClaimAuthorityDB, pub ClaimAuthorityPublisher, entries []ClaimAuthorityEntry, now time.Time, prune bool) (ClaimAuthorityPublishResult, error) {
	var result ClaimAuthorityPublishResult
	if strings.TrimSpace(pub.Tag) == "" {
		return result, errors.New("publishing claim authority: empty city tag")
	}
	existing, err := claimAuthorityCityKeys(ctx, db, pub.Tag)
	if err != nil {
		return result, err
	}
	desired := map[string]ClaimAuthorityEntry{}
	for _, entry := range entries {
		identity := strings.TrimSpace(entry.Identity)
		if identity == "" {
			continue
		}
		key := ClaimAuthorityIdentityKey(identity, pub.Tag)
		if _, seen := desired[key]; !seen {
			desired[key] = ClaimAuthorityEntry{Identity: identity, Source: entry.Source}
		}
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if existing[key] {
			continue
		}
		entry := desired[key]
		value, err := json.Marshal(claimAuthorityRow{Identity: entry.Identity, City: pub.City, Source: entry.Source, PublishedAt: now.UTC()})
		if err != nil {
			return result, fmt.Errorf("encoding claim authority row for %q: %w", entry.Identity, err)
		}
		if _, err := db.ExecContext(ctx, "REPLACE INTO `local_metadata` (`key`, `value`) VALUES (?, ?)", key, string(value)); err != nil {
			return result, fmt.Errorf("publishing claim authority for %q: %w", entry.Identity, err)
		}
		result.Upserted++
	}
	if prune {
		stale := make([]string, 0)
		for key := range existing {
			if _, keep := desired[key]; !keep {
				stale = append(stale, key)
			}
		}
		sort.Strings(stale)
		for _, key := range stale {
			if _, err := db.ExecContext(ctx, "DELETE FROM `local_metadata` WHERE `key` = ?", key); err != nil {
				return result, fmt.Errorf("pruning claim authority row %q: %w", key, err)
			}
			result.Deleted++
		}
	}
	wrote, err := refreshClaimAuthorityHeartbeat(ctx, db, pub, now)
	if err != nil {
		return result, err
	}
	result.Heartbeat = wrote
	return result, nil
}

// claimAuthorityCityKeys returns the identity keys the city tagged tag owns.
func claimAuthorityCityKeys(ctx context.Context, db ClaimAuthorityDB, tag string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT `key` FROM `local_metadata` WHERE `key` LIKE ?", claimAuthorityIDKeyPrefix+"%."+tag)
	if err != nil {
		return nil, fmt.Errorf("reading claim authority rows: %w", err)
	}
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning claim authority row: %w", err)
		}
		out[key] = true
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("reading claim authority rows: %w", err)
	}
	return out, nil
}

func refreshClaimAuthorityHeartbeat(ctx context.Context, db ClaimAuthorityDB, pub ClaimAuthorityPublisher, now time.Time) (bool, error) {
	heartbeats, err := ReadClaimAuthorityHeartbeats(ctx, db)
	if err != nil {
		return false, err
	}
	for _, hb := range heartbeats {
		if hb.Tag == pub.Tag && hb.Mode == pub.Mode && hb.City == pub.City && now.Sub(hb.PublishedAt) < ClaimAuthorityHeartbeatRefresh {
			return false, nil
		}
	}
	value, err := json.Marshal(ClaimAuthorityHeartbeat{City: pub.City, Mode: pub.Mode, PublishedAt: now.UTC()})
	if err != nil {
		return false, fmt.Errorf("encoding claim authority heartbeat: %w", err)
	}
	if _, err := db.ExecContext(ctx, "REPLACE INTO `local_metadata` (`key`, `value`) VALUES (?, ?)", claimAuthorityPublisherKey(pub.Tag), string(value)); err != nil {
		return false, fmt.Errorf("writing claim authority heartbeat: %w", err)
	}
	return true, nil
}

// ReadClaimAuthorityHeartbeats returns every publisher heartbeat in the
// database, ordered by tag.
func ReadClaimAuthorityHeartbeats(ctx context.Context, db ClaimAuthorityDB) ([]ClaimAuthorityHeartbeat, error) {
	rows, err := db.QueryContext(ctx, "SELECT `key`, `value` FROM `local_metadata` WHERE `key` LIKE ? ORDER BY `key`", claimAuthorityPubKeyPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("reading claim authority heartbeats: %w", err)
	}
	var out []ClaimAuthorityHeartbeat
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning claim authority heartbeat: %w", err)
		}
		var hb ClaimAuthorityHeartbeat
		if err := json.Unmarshal([]byte(value), &hb); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("decoding claim authority heartbeat %q: %w", key, err)
		}
		hb.Tag = strings.TrimPrefix(key, claimAuthorityPubKeyPrefix)
		out = append(out, hb)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("reading claim authority heartbeats: %w", err)
	}
	return out, nil
}

// RemoveClaimAuthorityCity deletes every row the city tagged tag owns,
// heartbeat included.
func RemoveClaimAuthorityCity(ctx context.Context, db ClaimAuthorityDB, tag string) error {
	if strings.TrimSpace(tag) == "" {
		return errors.New("removing claim authority rows: empty city tag")
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM `local_metadata` WHERE `key` LIKE ? OR `key` = ?", claimAuthorityIDKeyPrefix+"%."+tag, claimAuthorityPublisherKey(tag)); err != nil {
		return fmt.Errorf("removing claim authority rows for city tag %s: %w", tag, err)
	}
	return nil
}

// PruneStaleClaimAuthorityPublishers removes the rows of every publisher
// other than selfTag whose heartbeat is older than ttl, and returns the tags
// it pruned.
func PruneStaleClaimAuthorityPublishers(ctx context.Context, db ClaimAuthorityDB, selfTag string, now time.Time, ttl time.Duration) ([]string, error) {
	heartbeats, err := ReadClaimAuthorityHeartbeats(ctx, db)
	if err != nil {
		return nil, err
	}
	var pruned []string
	for _, hb := range heartbeats {
		if hb.Tag == selfTag || now.Sub(hb.PublishedAt) <= ttl {
			continue
		}
		if err := RemoveClaimAuthorityCity(ctx, db, hb.Tag); err != nil {
			return pruned, err
		}
		pruned = append(pruned, hb.Tag)
	}
	return pruned, nil
}

// ClaimAuthorityIdentityAuthorized reports whether any city published
// identity, which is the trigger's own test.
func ClaimAuthorityIdentityAuthorized(ctx context.Context, db ClaimAuthorityDB, identity string) (bool, error) {
	prefix := claimAuthorityIDKeyPrefix + claimAuthorityIdentityHash(identity)
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `local_metadata` WHERE `key` >= ? AND `key` < ?", prefix+".", prefix+"/").Scan(&n); err != nil {
		return false, fmt.Errorf("reading claim authority for %q: %w", identity, err)
	}
	return n > 0, nil
}

// ClaimAuthorityHolder is an unclosed routed bead with an assignee.
type ClaimAuthorityHolder struct {
	Table    string
	BeadID   string
	Assignee string
	Status   string
}

// ListRoutedClaimHolders returns every unclosed routed bead with an assignee
// in tables, using the trigger's routed predicate.
func ListRoutedClaimHolders(ctx context.Context, db ClaimAuthorityDB, tables []string) ([]ClaimAuthorityHolder, error) {
	var out []ClaimAuthorityHolder
	for _, table := range tables {
		if !slices.Contains(claimAuthorityBeadTables, table) {
			return nil, fmt.Errorf("listing routed claim holders: unknown bead table %q", table)
		}
		query := fmt.Sprintf("SELECT id, assignee, status FROM `%s` WHERE COALESCE(assignee, '') <> '' AND status <> 'closed' AND %s ORDER BY id",
			table, claimAuthorityRoutedSQL("`"+table+"`"))
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("listing routed claim holders in %s: %w", table, err)
		}
		for rows.Next() {
			holder := ClaimAuthorityHolder{Table: table}
			if err := rows.Scan(&holder.BeadID, &holder.Assignee, &holder.Status); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scanning routed claim holder: %w", err)
			}
			out = append(out, holder)
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("listing routed claim holders in %s: %w", table, err)
		}
	}
	return out, nil
}
