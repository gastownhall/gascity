package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, CGO_ENABLED=0 safe
)

const (
	sqliteStoreFilename               = "beads.sqlite"
	sqliteDefaultPrefix               = "gc"
	sqliteGraphPrefix                 = "gcg"
	sqliteGraphSequenceFloorFilename  = "graph.seqfloor"
	sqliteClaimFenceKVPrefix          = "gascity.claim-fence.v1/"
	sqliteDefaultRetentionPeriod      = 4 * time.Hour
	sqliteDefaultRetentionSweepPeriod = 30 * time.Second

	// sqliteBusyRetryAttempts is the number of application-level retries after
	// the per-connection busy_timeout is exhausted. Each retry backs off by
	// sqliteBusyRetryDelay before re-attempting, giving competing writers time
	// to release the WAL write lock.
	sqliteBusyRetryAttempts = 3
	sqliteBusyRetryDelay    = 150 * time.Millisecond
)

// SQLiteStoreOptions configures the SQLite bead store.
type SQLiteStoreOptions struct {
	prefix                  string
	reservedPrefixes        []string
	retentionPeriod         time.Duration
	retentionSweepInterval  time.Duration
	disableRetentionSweeper bool
	readOnly                bool
	privateRecovery         bool
}

var (
	_ Store                         = (*SQLiteStore)(nil)
	_ AtomicTxStore                 = (*SQLiteStore)(nil)
	_ ContextReadyReader            = (*SQLiteStore)(nil)
	_ ConditionalAssignmentReleaser = (*SQLiteStore)(nil)
	_ ForeignIDCreator              = (*SQLiteStore)(nil)
)

type sqliteSequenceFloorFile interface {
	Name() string
	Chmod(os.FileMode) error
	WriteString(string) (int, error)
	Sync() error
	Close() error
}

var (
	createSQLiteSequenceFloorTempFile = func(dir, pattern string) (sqliteSequenceFloorFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	openSQLiteSequenceFloorDirectory = func(path string) (sqliteSequenceFloorFile, error) {
		return os.Open(path)
	}
	observeSQLiteSequenceFloorBoundary = func(string) {}
)

// SQLiteStoreOption customizes OpenSQLiteStore.
type SQLiteStoreOption func(*SQLiteStoreOptions)

// WithSQLiteStoreIDPrefix sets the generated bead ID prefix.
func WithSQLiteStoreIDPrefix(prefix string) SQLiteStoreOption {
	return func(o *SQLiteStoreOptions) {
		if strings.TrimSpace(prefix) != "" {
			o.prefix = normalizeIDPrefix(prefix)
		}
	}
}

// WithSQLiteStoreReservedIDPrefixes fences the store to the id namespaces it
// serves: a caller-PINNED id outside all of them is refused by Create.
//
// Minting already keeps generated ids inside the store's own namespace, but an
// explicit id is honored verbatim, so without this a caller can write a bead
// into a class binding under an id the binding does not claim. Such a bead is
// unreachable by every id-shaped lookup and contradicts the binding's own
// namespace declaration, which is what lets the residency resolver eventually
// stop probing.
//
// More than one prefix, because a binding holds more than it mints — the nudge
// queue's records live in the nudges store under their own namespace. An empty
// set leaves the store unfenced, which is the shipped default everywhere the
// store is not a class binding.
//
// CreateWithForeignID deliberately bypasses the fence: carrying a preserved
// foreign id across is the store-migration copy path's entire job.
func WithSQLiteStoreReservedIDPrefixes(prefixes ...string) SQLiteStoreOption {
	return func(o *SQLiteStoreOptions) {
		for _, p := range prefixes {
			if p = normalizeIDPrefix(p); p != "" {
				o.reservedPrefixes = append(o.reservedPrefixes, p)
			}
		}
	}
}

// WithSQLiteStoreRetention configures terminal-record retention. A
// non-positive sweep interval disables the background sweeper.
func WithSQLiteStoreRetention(period, sweepInterval time.Duration) SQLiteStoreOption {
	return func(o *SQLiteStoreOptions) {
		o.retentionPeriod = period
		o.retentionSweepInterval = sweepInterval
		o.disableRetentionSweeper = sweepInterval <= 0
	}
}

// WithSQLiteStoreReadOnly opens the store strictly read-only: connections use
// SQLite's file:...?mode=ro, schema application (which would issue writes —
// journal-mode pragmas, CREATE TABLE) is skipped, and the retention sweeper
// never starts. A mode=ro connection cannot acquire the write lock, so it can
// neither mutate a row NOR checkpoint the WAL on close — the source's main db
// AND -wal stay byte-identical across open/read/close, which the "must stay
// bit-intact for rollback" migration-source contract requires. The full read
// surface (List/Get/DepList) works, reading WAL-resident rows a stopped writer
// left uncheckpointed. The file must already exist; the parent directory is
// never created.
func WithSQLiteStoreReadOnly() SQLiteStoreOption {
	return func(o *SQLiteStoreOptions) {
		o.readOnly = true
	}
}

// WithSQLiteStorePrivateRecovery opens an existing disposable snapshot
// read-write before exact schema validation so SQLite can recover a hot
// journal. It never creates a database or applies or repairs schema.
func WithSQLiteStorePrivateRecovery() SQLiteStoreOption {
	return func(o *SQLiteStoreOptions) {
		o.privateRecovery = true
	}
}

// isSQLiteBusy reports whether err is a SQLite write-contention error.
// The modernc driver returns "database is locked (5) (SQLITE_BUSY)" when the
// per-connection busy_timeout expires without acquiring the WAL write lock.
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// retryOnBusy retries fn up to sqliteBusyRetryAttempts times when it returns
// a SQLITE_BUSY error, backing off by sqliteBusyRetryDelay between attempts.
// The busy_timeout PRAGMA already retries at the C layer for 5 s per call, so
// each application-level retry is an additional 5 s+ window for the lock.
func retryOnBusy(fn func() error) error {
	err := fn()
	for attempt := 0; attempt < sqliteBusyRetryAttempts && isSQLiteBusy(err); attempt++ {
		time.Sleep(sqliteBusyRetryDelay)
		err = fn()
	}
	return err
}

// SQLiteStore is a pure-Go SQLite-backed Store using modernc.org/sqlite.
// No CGO required. Builds unconditionally with CGO_ENABLED=0.
//
// Concurrency model: a single write connection serializes mutations; a pool
// of 8 read connections allows concurrent reads in WAL mode.
type SQLiteStore struct {
	db                         *sql.DB // write connection (MaxOpenConns=1)
	readDB                     *sql.DB // read pool (MaxOpenConns=8)
	path                       string
	prefix                     string
	reservedPrefixes           []string // when non-empty, the namespaces a pinned id must carry
	retentionPeriod            time.Duration
	retentionSweepInterval     time.Duration
	disableRetentionSweeper    bool
	retentionStop              context.CancelFunc
	retentionDone              chan struct{}
	seq                        atomic.Int64 // in-memory sequence; recovered from DB on Open
	sequenceFloorMu            sync.Mutex
	sequenceFloorBeforePersist func() // test-only seam for the serialized floor critical section.
	closeMu                    sync.Mutex
	closeReadDB                func() error // test-only seam; production falls back to readDB.Close.
	closeWriteDB               func() error // test-only seam; production falls back to db.Close.
	readOnly                   bool
	hasRevisionColumn          bool
	legacyDepsPrimaryKey       bool
	sequenceFloorPath          string
	localStrings               *localSidecar // clone-local data; see Store.SetLocalString
}

// OpenSQLiteStore opens or creates a pure-Go SQLite bead store under dir.
func OpenSQLiteStore(dir string, opts ...SQLiteStoreOption) (Store, error) {
	cfg := SQLiteStoreOptions{
		prefix:                 sqliteDefaultPrefix,
		retentionPeriod:        sqliteDefaultRetentionPeriod,
		retentionSweepInterval: sqliteDefaultRetentionSweepPeriod,
		// Graph history is retained by default. A caller that deliberately
		// wants terminal-record deletion must opt in with
		// WithSQLiteStoreRetention; this preserves the rollback window for a
		// combined infra database.
		disableRetentionSweeper: true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.prefix == "" {
		cfg.prefix = sqliteDefaultPrefix
	}
	if cfg.readOnly && cfg.privateRecovery {
		return nil, fmt.Errorf("opening sqlite store: read-only and private recovery modes are mutually exclusive")
	}
	if !cfg.readOnly && !cfg.privateRecovery {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("opening sqlite store: %w", err)
		}
	}
	dbPath := filepath.Join(dir, sqliteStoreFilename)
	_, statErr := os.Stat(dbPath)
	databaseExists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat sqlite store %s: %w", dbPath, statErr)
	}
	if cfg.readOnly && !databaseExists {
		return nil, fmt.Errorf("opening read-only sqlite store %s: %w", dbPath, statErr)
	}
	if cfg.privateRecovery && !databaseExists {
		return nil, fmt.Errorf("opening sqlite private recovery store %s: requires an existing database", dbPath)
	}
	var preflightLayout sqliteStoreSchemaLayout
	preflighted := databaseExists && !cfg.privateRecovery
	if preflighted {
		var err error
		preflightLayout, err = inspectSQLiteStoreSchemaAtPath(context.Background(), dbPath)
		if err != nil {
			return nil, fmt.Errorf("opening sqlite store %s: %w", dbPath, err)
		}
	}

	// Per-connection PRAGMAs ride the modernc `_pragma=` DSN form so they
	// apply to EVERY pooled connection. The mattn-style `?_busy_timeout=`
	// query parameter is silently ignored by modernc, which would leave the
	// read pool without the busy timeout the retry machinery below assumes.
	//
	// The URI is built structurally, not by concatenating path and query. A
	// city directory may legitimately contain ?, #, %, or spaces; none may
	// become a SQLite parameter or fragment while opening a migration source.
	//
	// One DSN serves both handles, so every pragma on it is charged
	// sqliteStorePerStoreConnections times, not once. sqliteStoreDSNWithMode
	// keeps that budget honest.
	dsn := sqliteStoreDSN(dbPath, cfg.readOnly)
	if cfg.privateRecovery {
		dsn = sqliteStorePrivateRecoveryDSN(dbPath)
	}

	// Write connection: single connection serializes all mutations.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite store %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	s := &SQLiteStore{
		db:                      db,
		path:                    dbPath,
		prefix:                  cfg.prefix,
		reservedPrefixes:        cfg.reservedPrefixes,
		retentionPeriod:         cfg.retentionPeriod,
		retentionSweepInterval:  cfg.retentionSweepInterval,
		disableRetentionSweeper: cfg.disableRetentionSweeper,
		readOnly:                cfg.readOnly,
		sequenceFloorPath:       filepath.Join(dir, sqliteGraphSequenceFloorFilename),
		localStrings:            newLocalSidecar(filepath.Join(dir, ".beads", "local-strings.json")),
	}

	// Schema application is creation-only. Existing databases were admitted
	// through a read-only exact-schema preflight above, before this writable
	// connection existed. New databases receive the current schema.
	if !cfg.readOnly && !databaseExists {
		if err := s.applySchema(context.Background()); err != nil {
			db.Close() //nolint:errcheck
			return nil, err
		}
	}
	layout, err := inspectSQLiteStoreSchema(context.Background(), db)
	if err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	if preflighted && layout != preflightLayout {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("opening sqlite store %s: unsupported sqlite schema: layout changed after preflight", dbPath)
	}
	s.hasRevisionColumn = layout.hasRevisionColumn
	s.legacyDepsPrimaryKey = layout.legacyDepsPrimaryKey
	if err := s.recoverSequence(context.Background()); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}

	// Read pool: multiple concurrent read connections.
	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("opening sqlite read pool %s: %w", dbPath, err)
	}
	readDB.SetMaxOpenConns(sqliteReadPoolSize)
	readDB.SetMaxIdleConns(sqliteReadPoolSize)
	readDB.SetConnMaxIdleTime(5 * time.Minute)
	s.readDB = readDB

	if !cfg.readOnly {
		s.startRetentionSweeper()
	}
	return s, nil
}

// sqliteStoreDSNReadOnlyMode is the mode= value that makes a connection
// incapable of writing or checkpointing.
const sqliteStoreDSNReadOnlyMode = "ro"

// sqliteReadPoolSize is how many connections the read pool may hand out at
// once, and sqliteStorePerStoreConnections is that plus the single write
// connection. Every pragma on the DSN is charged once per connection, so this
// is the multiplier on anything the DSN makes a connection allocate.
const (
	sqliteReadPoolSize             = 8
	sqliteStorePerStoreConnections = sqliteReadPoolSize + 1
)

// sqliteStoreDSN returns a file URI whose path and query are encoded
// independently. In read-only mode SQLite's mode=ro is a hard capability: it
// cannot take a write lock or checkpoint a source WAL during close.
func sqliteStoreDSN(path string, readOnly bool) string {
	mode := ""
	if readOnly {
		mode = sqliteStoreDSNReadOnlyMode
	}
	return sqliteStoreDSNWithMode(path, mode)
}

func sqliteStorePrivateRecoveryDSN(path string) string {
	return sqliteStoreDSNWithMode(path, "rw")
}

func sqliteStoreDSNWithMode(path, mode string) string {
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")

	// mmap_size on every mode. It maps the database instead of read()-ing
	// every page into the per-connection page cache, and it is the only tuning
	// pragma here because it is the only one measurement supports. On a
	// 231 MB / 51k-bead graph, 8 concurrent Ready() scans go 11.3 s -> 8.2 s
	// with the mapping alone. It touches only this process's address space,
	// never the database bytes, so it is as correct on the read-only
	// migration-source open as on the writer.
	//
	// It costs address space, not memory: nine connections map the same file
	// nine times, which inflates VmSize by ~1.9 GB and VmRSS by the same file
	// pages counted once per mapping. Pss — the honest figure — is unchanged,
	// and anonymous memory goes DOWN, because pages served through the mapping
	// never enter the page cache.
	query.Add("_pragma", "mmap_size(268435456)")

	// Three pragmas that look like they belong here and were measured out.
	// Reasons, so nobody re-adds them from general SQLite advice:
	//
	// cache_size — the page cache is per-connection, so any value here is a
	// sqliteStorePerStoreConnections-way budget. cache_size(-64000) on all
	// nine measured 1.19 GB of anonymous memory (modernc's allocator rounds
	// each ~4.2 KiB page+header into its 8 KiB size class, ~2.1x nominal) and
	// it is not even faster: with the mapping in place it bought nothing on
	// any workload tried, and past the mmap window it was monotonically
	// slower, because every page-cache allocation and eviction goes through
	// libc's one global allocator mutex that all eight readers share. 8
	// readers x 60k indexed lookups on a 441 MB graph: default 11.5 s / 53 MB
	// anonymous, -8000 12.5 s / 156 MB, -64000 17.9 s / 907 MB. Writes were
	// within noise at every size, in single-row and 4000-row transactions.
	//
	// temp_store(MEMORY) — modernc's sqlite3VdbeSorterInit allocates the
	// sorter's bump arena only when the temp store is NOT in memory, so
	// temp_store(MEMORY) turns every sorter record into an individual malloc
	// behind that same global mutex. On the 8-connection read pool it made
	// concurrent Ready() ~1.8x slower than no pragmas at all (20.6 s vs
	// 11.3 s) — a regression on precisely the sorted scans it looks like it
	// should help.
	//
	// synchronous(NORMAL) — a genuine ~30% write win that is not safe to take
	// until the sequence floor leads the allocator. This store rebuilds its ID
	// allocator at open from MAX(numeric suffix) over durable rows, so a WAL
	// tail lost to a host crash regresses the allocator and reissues bead IDs
	// that already escaped into the event log, into gc.root_bead_id and
	// gc.step_ref in other class stores, and into printed output — durable
	// references that then resolve, silently, to a different bead. FULL is
	// what makes a returned ID durable before the caller sees it. The
	// graph.seqfloor sidecar was built for this hazard but does not cover it
	// today: it is written only at genesis, it trails rather than leads the
	// allocator, and the other four reserved prefixes have no floor at all.

	if mode != "" {
		query.Set("mode", mode)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

func (s *SQLiteStore) applySchema(ctx context.Context) error {
	for _, stmt := range sqliteStoreCreationSchemaStatements() {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("applying sqlite schema: %w", err)
		}
	}
	return nil
}

// sqliteDepsUsesLegacyPrimaryKey detects the earlier schema whose deps
// primary key includes dep_type. It must remain readable and writable without
// rebuilding the table: existing installations need to be able to roll back
// to the old binary after this one has written more edges.
func sqliteDepsUsesLegacyPrimaryKey(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(deps)`)
	if err != nil {
		return false, fmt.Errorf("inspecting sqlite deps schema: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	primaryKeys := make(map[string]int, 3)
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultVal, &pk); err != nil {
			return false, fmt.Errorf("inspecting sqlite deps schema: %w", err)
		}
		primaryKeys[name] = pk
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("inspecting sqlite deps schema: %w", err)
	}
	return primaryKeys["issue_id"] > 0 && primaryKeys["depends_on_id"] > 0 && primaryKeys["dep_type"] > 0, nil
}

func (s *SQLiteStore) recoverSequence(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM beads WHERE id LIKE ?`, s.prefix+"-%")
	if err != nil {
		return fmt.Errorf("recovering sqlite sequence: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var maxSeq int64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if n := int64(numericIDSuffix(id)); n > maxSeq {
			maxSeq = n
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if s.prefix == sqliteGraphPrefix {
		floor, err := s.SequenceFloor()
		if err != nil {
			return fmt.Errorf("recovering sqlite graph sequence floor: %w", err)
		}
		if floor > maxSeq {
			maxSeq = floor
		}
	}
	s.seq.Store(maxSeq)
	return nil
}

// StoreHealthPath returns the SQLite database file path.
func (s *SQLiteStore) StoreHealthPath() string {
	if s == nil {
		return ""
	}
	return s.path
}

// ensureOpen reports ErrStoreClosed once CloseStore has released the store's
// database handles. Every exported method that reaches a handle starts with
// it, so a use-after-close returns an error instead of dereferencing nil;
// methods that only delegate inherit the guard from the method they call.
func (s *SQLiteStore) ensureOpen() error {
	if s == nil || s.db == nil || s.readDB == nil {
		return fmt.Errorf("sqlite store: %w", ErrStoreClosed)
	}
	return nil
}

// Ping verifies that the SQLite store is reachable.
func (s *SQLiteStore) Ping() error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if err := s.db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("pinging sqlite store: %w", err)
	}
	return nil
}

// CloseStore stops the background retention sweeper and closes both the write
// and read database connections. Idempotent — safe to call multiple times.
// Every other method reports ErrStoreClosed once it has run.
func (s *SQLiteStore) CloseStore() error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	if s.retentionStop != nil {
		s.retentionStop()
		s.retentionStop = nil
	}
	if s.retentionDone != nil {
		<-s.retentionDone
		s.retentionDone = nil
	}

	var errs []error
	if s.closeReadDB != nil || s.readDB != nil {
		var err error
		if s.closeReadDB != nil {
			err = s.closeReadDB()
		} else {
			err = s.readDB.Close()
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			s.closeReadDB = nil
			s.readDB = nil
		}
	}
	if s.closeWriteDB != nil || s.db != nil {
		var err error
		if s.closeWriteDB != nil {
			err = s.closeWriteDB()
		} else {
			err = s.db.Close()
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			s.closeWriteDB = nil
			s.db = nil
		}
	}
	return errors.Join(errs...)
}

// CreateWithForeignID persists a new bead KEEPING its explicit ID (any prefix),
// mirroring BdStore's forced foreign-prefix create for the store-migration copy
// path. Create already honors a caller-pinned id verbatim and enforces the hard
// duplicate-id contract, so this is a guarded delegation. It satisfies
// ForeignIDCreator.
func (s *SQLiteStore) CreateWithForeignID(b Bead) (Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, err
	}
	if strings.TrimSpace(b.ID) == "" {
		return Bead{}, fmt.Errorf("creating bead with foreign id: empty id")
	}
	return s.create(b, true)
}

// Create persists a new bead, minting a prefixed sequential id when the
// caller did not pin one; an explicit id is honored verbatim with a hard
// duplicate-id error, provided it carries one of the store's reserved
// namespaces when the store is fenced (WithSQLiteStoreReservedIDPrefixes).
func (s *SQLiteStore) Create(b Bead) (Bead, error) {
	return s.create(b, false)
}

// create is the shared body. allowForeign is the CreateWithForeignID
// exemption: the store-migration copy path carries preserved ids across, and
// refusing them there would leave the beads nowhere at all.
func (s *SQLiteStore) create(b Bead, allowForeign bool) (Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, err
	}
	if !allowForeign {
		if err := s.checkPinnedIDNamespace(b.ID); err != nil {
			return Bead{}, err
		}
	}
	var stored Bead
	autoID := b.ID == ""
	err := retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite create: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		stored = s.normalizeCreate(b)
		if autoID {
			// Store-generated id: self-heal a stale sequence floor so a suffix
			// already minted by another process is never reissued.
			id, err := s.mintUniqueIDTx(ctx, tx, stored.ID, nil)
			if err != nil {
				return err
			}
			stored.ID = id
		} else if err := s.ensureCreateDoesNotExist(ctx, tx, stored.ID); err != nil {
			// Caller-pinned id keeps the hard duplicate-id contract intact.
			return err
		}
		if err := s.clearClaimFenceTx(ctx, tx, stored.ID); err != nil {
			return err
		}
		if err := s.upsertBeadTx(ctx, tx, stored); err != nil {
			return err
		}
		for _, dep := range depsFromBeadFields(stored) {
			if err := s.depAddTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite create: commit: %w", err)
		}
		return nil
	})
	if err != nil {
		return Bead{}, err
	}
	return cloneBead(stored), nil
}

// checkPinnedIDNamespace enforces the fence. An empty id is a mint request and
// an unfenced store claims no namespace, so both pass untouched.
//
// It runs BEFORE any read of the database, which is deliberate and is part of
// the contract: a store must answer "I do not serve that namespace" without
// first checking whether it happens to hold the row. Checking existence first
// would leak the presence of a relic — CreateWithForeignID can carry a foreign
// id in — through a refusal about a namespace the store disclaims.
//
// The id is tested EXACTLY as it will be stored. Trimming it first would let a
// caller pin "  gcn-1" — or an id that is nothing but spaces — past a check the
// stored row then fails, which is the one outcome the fence exists to prevent:
// a resident bead that no id-shaped lookup of this namespace can reach. Only
// create's own mint test (an id of length zero) is exempt.
//
// The refusal wraps ErrPinnedIDOutsideNamespace so a caller can route the id to
// a sibling binding instead of parsing this message.
func (s *SQLiteStore) checkPinnedIDNamespace(id string) error {
	if len(s.reservedPrefixes) == 0 {
		return nil
	}
	if id == "" {
		return nil
	}
	lowered := strings.ToLower(id)
	for _, prefix := range s.reservedPrefixes {
		if strings.HasPrefix(lowered, prefix+"-") {
			return nil
		}
	}
	return fmt.Errorf("sqlite create: id %q is outside this store's namespaces (%s): a pinned id must carry one of them, or use the foreign-id create the store migration uses: %w", id, strings.Join(s.reservedPrefixes, ", "), ErrPinnedIDOutsideNamespace)
}

func (s *SQLiteStore) normalizeCreate(b Bead) Bead {
	b = cloneBead(b)
	if b.ID == "" {
		b.ID = s.nextID()
	} else if n := numericIDSuffix(b.ID); n > 0 {
		s.ensureSequenceAtLeast(int64(n))
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}
	return b
}

func (s *SQLiteStore) nextID() string {
	return fmt.Sprintf("%s-%d", s.prefix, s.seq.Add(1))
}

// AdvanceSequenceFloor lifts the store's in-memory id sequence so the next
// auto-minted id has a numeric suffix strictly greater than n. It never lowers
// the floor. Call SetSequenceFloor when the floor must survive a reopen.
func (s *SQLiteStore) AdvanceSequenceFloor(n int64) {
	s.ensureSequenceAtLeast(n)
}

// SetSequenceFloor persists a nonnegative Graph ID floor and raises the
// in-memory allocator to the same value. The Graph provider sets this after a
// fenced cross-class census, so a gcg ID observed outside beads.sqlite cannot
// be reissued after a restart.
func (s *SQLiteStore) SetSequenceFloor(n int64) error {
	if s == nil {
		return errors.New("setting sqlite sequence floor on nil store")
	}
	if s.readOnly {
		return errors.New("setting sqlite sequence floor on read-only store")
	}
	if n < 0 {
		return fmt.Errorf("setting sqlite sequence floor: negative value %d", n)
	}
	s.sequenceFloorMu.Lock()
	defer s.sequenceFloorMu.Unlock()
	current, err := s.SequenceFloor()
	if err != nil {
		return err
	}
	if current > n {
		n = current
	}
	if allocated := s.seq.Load(); allocated > n {
		n = allocated
	}
	if s.sequenceFloorBeforePersist != nil {
		s.sequenceFloorBeforePersist()
	}
	persisted, err := persistSQLiteSequenceFloorAtLeast(s.sequenceFloorPath, n)
	if err != nil {
		return fmt.Errorf("setting sqlite sequence floor: %w", err)
	}
	s.ensureSequenceAtLeast(persisted)
	return nil
}

// SequenceFloor returns the persisted Graph ID floor. An absent sidecar is the
// genesis floor zero; malformed or negative contents are rejected rather than
// silently allowing a reserved-ID collision.
func (s *SQLiteStore) SequenceFloor() (int64, error) {
	if s == nil {
		return 0, errors.New("reading sqlite sequence floor on nil store")
	}
	return readSQLiteSequenceFloor(s.sequenceFloorPath)
}

func readSQLiteSequenceFloor(path string) (int64, error) {
	bytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(bytes) == 0 {
		return 0, fmt.Errorf("reading %s: empty floor", path)
	}
	if bytes[len(bytes)-1] != '\n' {
		return 0, fmt.Errorf("reading %s: floor lacks trailing newline", path)
	}
	text := string(bytes[:len(bytes)-1])
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("reading %s: invalid nonnegative floor %q", path, text)
	}
	if string(bytes) != strconv.FormatInt(n, 10)+"\n" {
		return 0, fmt.Errorf("reading %s: non-canonical floor %q", path, string(bytes))
	}
	return n, nil
}

func writeSQLiteSequenceFloor(path string, floor int64) (returnErr error) {
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	tempPattern, err := sqliteSequenceFloorTempPattern(dir, "."+filepath.Base(path)+"-")
	if err != nil {
		return fmt.Errorf("preparing %s temporary file: %w", path, err)
	}
	tmp, err := createSQLiteSequenceFloorTempFile(dir, tempPattern)
	if err != nil {
		return fmt.Errorf("creating %s temporary file: %w", path, err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-temp-open")
	tmpPath := tmp.Name()
	temporaryClosePending := true
	defer func() {
		if temporaryClosePending {
			if err := tmp.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("closing %s temporary file: %w", path, err))
			}
		}
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("removing %s temporary file: %w", path, err))
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("setting %s temporary file mode: %w", path, err)
	}
	if _, err := tmp.WriteString(strconv.FormatInt(floor, 10) + "\n"); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	temporaryClosePending = false
	observeSQLiteSequenceFloorBoundary("sequence-floor-temp-close-before")
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-temp-close-after")
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-renamed")
	directory, err := openSQLiteSequenceFloorDirectory(dir)
	if err != nil {
		return fmt.Errorf("opening %s for sync: %w", dir, err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-directory-open")
	directoryClosePending := true
	defer func() {
		if directoryClosePending {
			if err := directory.Close(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("closing %s directory: %w", dir, err))
			}
		}
	}()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	directoryClosePending = false
	observeSQLiteSequenceFloorBoundary("sequence-floor-directory-close-before")
	if err := directory.Close(); err != nil {
		return fmt.Errorf("closing %s directory: %w", dir, err)
	}
	observeSQLiteSequenceFloorBoundary("sequence-floor-directory-close-after")
	return nil
}

func (s *SQLiteStore) ensureSequenceAtLeast(n int64) {
	for {
		cur := s.seq.Load()
		if n <= cur {
			return
		}
		if s.seq.CompareAndSwap(cur, n) {
			return
		}
	}
}

// idExistsTx reports whether a bead with the given id is already persisted
// within the open transaction.
func (s *SQLiteStore) idExistsTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM beads WHERE id=?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking duplicate sqlite bead %q: %w", id, err)
	}
	return true, nil
}

// ensureCreateDoesNotExist is the hard-fail uniqueness check for caller-pinned
// IDs: a pinned id that already exists is a duplicate-id error, preserving
// resume and crash-adoption semantics. Auto-generated ids self-heal via
// mintUniqueIDTx instead.
func (s *SQLiteStore) ensureCreateDoesNotExist(ctx context.Context, tx *sql.Tx, id string) error {
	exists, err := s.idExistsTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("creating bead %q: duplicate id", id)
	}
	return nil
}

// reseedSeqFromTx lifts the in-memory sequence floor to the on-disk max suffix
// observed within the open transaction. It mirrors recoverSequence's MAX-suffix
// scan but reads through tx so it sees IDs minted by other processes since this
// store opened — the stale-seq self-heal in one step.
func (s *SQLiteStore) reseedSeqFromTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM beads WHERE id LIKE ?`, s.prefix+"-%")
	if err != nil {
		return fmt.Errorf("reseeding sqlite sequence: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var maxSeq int64
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if n := int64(numericIDSuffix(id)); n > maxSeq {
			maxSeq = n
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.ensureSequenceAtLeast(maxSeq)
	return nil
}

// mintUniqueIDMaxAttempts bounds the collision-retry loop in mintUniqueIDTx so
// a pathological store can never spin forever.
const mintUniqueIDMaxAttempts = 100000

// mintUniqueIDTx returns a store-generated bead ID that is free within the open
// transaction and unclaimed by seen. It starts from candidate (an
// already-normalized auto id); on the first collision it reseeds the sequence
// floor once (lifting a stale floor past concurrently-minted IDs in one step),
// then mints fresh ids until one is free. seen lets a single batch avoid
// minting the same fresh id twice; callers may pass nil for a standalone mint.
func (s *SQLiteStore) mintUniqueIDTx(ctx context.Context, tx *sql.Tx, candidate string, seen map[string]bool) (string, error) {
	id := candidate
	reseeded := false
	for attempt := 0; attempt < mintUniqueIDMaxAttempts; attempt++ {
		taken := seen[id]
		if !taken {
			exists, err := s.idExistsTx(ctx, tx, id)
			if err != nil {
				return "", err
			}
			taken = exists
		}
		if !taken {
			if seen != nil {
				seen[id] = true
			}
			return id, nil
		}
		if !reseeded {
			if err := s.reseedSeqFromTx(ctx, tx); err != nil {
				return "", err
			}
			reseeded = true
		}
		id = s.nextID()
	}
	return "", fmt.Errorf("minting unique sqlite bead id: exhausted %d attempts from candidate %q", mintUniqueIDMaxAttempts, candidate)
}

func (s *SQLiteStore) upsertBeadTx(ctx context.Context, tx *sql.Tx, b Bead) error {
	payload, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("sqlite marshal bead %q: %w", b.ID, err)
	}
	tier := "main"
	if b.Ephemeral {
		tier = "wisp"
	}
	var priority any
	if b.Priority != nil {
		priority = *b.Priority
	}
	update := `
			tier=excluded.tier,
			 title=excluded.title,
			 status=excluded.status,
			 issue_type=excluded.issue_type,
			 priority=excluded.priority,
			 created_at=excluded.created_at,
			 updated_at=excluded.updated_at,
			 assignee=excluded.assignee,
			 from_agent=excluded.from_agent,
			 parent_id=excluded.parent_id,
			 ref=excluded.ref,
			 description=excluded.description,
			 bead_json=excluded.bead_json`
	if s.hasRevisionColumn {
		update += `, revision=beads.revision+1`
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO beads(id,tier,title,status,issue_type,priority,created_at,updated_at,assignee,from_agent,parent_id,ref,description,bead_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET `+update,
		b.ID, tier, b.Title, b.Status, b.Type, priority, b.CreatedAt.UnixNano(), sqliteUnixNanoOrZero(b.UpdatedAt),
		b.Assignee, b.From, b.ParentID, b.Ref, b.Description, string(payload))
	if err != nil {
		return fmt.Errorf("sqlite upsert bead %q: %w", b.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE bead_id=?`, b.ID); err != nil {
		return fmt.Errorf("sqlite replace labels for %q: %w", b.ID, err)
	}
	for _, label := range b.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO labels(bead_id,label) VALUES(?,?)`, b.ID, label); err != nil {
			return fmt.Errorf("sqlite insert label for %q: %w", b.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata WHERE bead_id=?`, b.ID); err != nil {
		return fmt.Errorf("sqlite replace metadata for %q: %w", b.ID, err)
	}
	for k, v := range b.Metadata {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(bead_id,meta_key,meta_value) VALUES(?,?,?)`, b.ID, k, v); err != nil {
			return fmt.Errorf("sqlite insert metadata for %q: %w", b.ID, err)
		}
	}
	return nil
}

func sqliteUnixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// IDPrefix returns the bead ID prefix owned by this store, without trailing "-".
func (s *SQLiteStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.prefix
}

// Get retrieves a bead by ID.
func (s *SQLiteStore) Get(id string) (Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, err
	}
	row := s.readDB.QueryRowContext(
		context.Background(),
		`SELECT `+s.sqliteBeadProjection()+` FROM beads b WHERE b.id=?`,
		id,
	)
	b, err := scanSQLiteBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
	}
	return b, nil
}

func (s *SQLiteStore) revisionSelectExpr(tableAlias string) string {
	if !s.hasRevisionColumn {
		return "0"
	}
	if tableAlias != "" {
		return tableAlias + ".revision"
	}
	return "revision"
}

// sqliteBeadProjection selects every out-of-band concurrency carrier beside a
// bead's JSON payload. Both Revision and ClaimFence deliberately stay off the
// public Bead JSON wire, so every SQLite read path must project them here.
// Every such path aliases the beads table as "b".
func (s *SQLiteStore) sqliteBeadProjection() string {
	const alias = "b"
	return alias + `.bead_json, ` + s.revisionSelectExpr(alias) + `,
		COALESCE((SELECT value FROM kv k WHERE k.key='` + sqliteClaimFenceKVPrefix + `' || ` + alias + `.id), '0')`
}

type sqliteScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteBead(row sqliteScanner) (Bead, error) {
	var (
		raw             string
		revision        int64
		claimFenceValue string
	)
	if err := row.Scan(&raw, &revision, &claimFenceValue); err != nil {
		return Bead{}, err
	}
	var b Bead
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return Bead{}, err
	}
	// Bead.Revision and ClaimFence are json:"-". Revision lives in the current
	// table column while ClaimFence uses the rollback-compatible kv carrier;
	// every SELECT feeding this scanner projects both values.
	b.Revision = revision
	claimFence, err := strconv.ParseInt(claimFenceValue, 10, 64)
	if err != nil || claimFence < 0 {
		if err == nil {
			err = fmt.Errorf("negative value")
		}
		return Bead{}, fmt.Errorf("decoding claim fence for bead %q: %w", b.ID, err)
	}
	b.ClaimFence = claimFence
	return cloneBead(b), nil
}

// applySQLiteUpdateOpts applies opts to b with SQLiteStore.Update's exact
// field semantics (nil pointers skipped, Labels appended, RemoveLabels
// filtered, Metadata merged). Update and UpdateIfMatch share it so the fenced
// and unfenced paths cannot drift.
func applySQLiteUpdateOpts(b Bead, opts UpdateOpts) Bead {
	if opts.Title != nil {
		b.Title = *opts.Title
	}
	if opts.Status != nil {
		b.Status = *opts.Status
	}
	if opts.Type != nil {
		b.Type = *opts.Type
	}
	if opts.Priority != nil {
		b.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		b.Description = *opts.Description
	}
	if opts.ParentID != nil {
		b.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		b.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			b.Metadata[k] = v
		}
	}
	if len(opts.Labels) > 0 {
		b.Labels = append(b.Labels, opts.Labels...)
	}
	if len(opts.RemoveLabels) > 0 {
		remove := make(map[string]bool, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = true
		}
		filtered := b.Labels[:0]
		for _, label := range b.Labels {
			if !remove[label] {
				filtered = append(filtered, label)
			}
		}
		b.Labels = filtered
	}
	return b
}

// Update modifies fields of an existing bead.
func (s *SQLiteStore) Update(id string, opts UpdateOpts) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite update: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		before := b
		b = applySQLiteUpdateOpts(b, opts)
		b.UpdatedAt = time.Now()
		if err := s.upsertBeadTx(ctx, tx, b); err != nil {
			return err
		}
		if err := s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, before, &b); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still
// has the expected assignee.
func (s *SQLiteStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if err := s.ensureOpen(); err != nil {
		return false, err
	}
	var released bool
	err := retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite release-if-current: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		b, err := s.getTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if b.Status != "in_progress" || b.Assignee != expectedAssignee {
			return nil
		}
		before := b
		b.Status = "open"
		b.Assignee = ""
		b.UpdatedAt = time.Now()
		if err := s.upsertBeadTx(ctx, tx, b); err != nil {
			return err
		}
		if err := s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, before, &b); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

func (s *SQLiteStore) getTx(ctx context.Context, tx *sql.Tx, id string) (Bead, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+s.sqliteBeadProjection()+` FROM beads b WHERE b.id=?`, id)
	b, err := scanSQLiteBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return b, err
}

// Close sets a bead's status to closed.
func (s *SQLiteStore) Close(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	if b.Status == "closed" {
		return nil
	}
	status := "closed"
	return s.Update(id, UpdateOpts{Status: &status})
}

// Reopen sets a bead's status to open.
func (s *SQLiteStore) Reopen(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	if b.Status == "open" {
		return nil
	}
	status := "open"
	return s.Update(id, UpdateOpts{Status: &status})
}

// CloseAll closes multiple beads and applies metadata to each closed bead.
func (s *SQLiteStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if err := s.ensureOpen(); err != nil {
		return 0, err
	}
	closed := 0
	for _, id := range ids {
		b, err := s.Get(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return closed, err
		}
		if b.Status == "closed" {
			continue
		}
		opts := UpdateOpts{Status: ptrTo("closed"), Metadata: maps.Clone(metadata)}
		if err := s.Update(id, opts); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *SQLiteStore) List(query ListQuery) ([]Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	sqlText, args := sqliteListSQL(query, s.sqliteBeadProjection())
	rows, err := s.readDB.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sqlite beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	for rows.Next() {
		b, err := scanSQLiteBead(rows)
		if err != nil {
			return nil, fmt.Errorf("listing sqlite beads: %w", err)
		}
		if !query.Matches(b) {
			continue
		}
		result = append(result, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sqlite beads: %w", err)
	}
	sortBeadsForQuery(result, query.Sort)
	if query.Limit > 0 && len(result) > query.Limit {
		result = result[:query.Limit]
	}
	return result, nil
}

func sqliteListSQL(q ListQuery, projection string) (string, []any) {
	where := []string{}
	args := []any{}
	switch q.TierMode {
	case TierWisps:
		// NoHistory rows live in SQLite's main tier but remain part of the
		// logical wisp tier, so final tier filtering happens after decode.
	case TierBoth:
	default:
		where = append(where, "b.tier='main'")
	}
	if q.Status != "" {
		where = append(where, "b.status=?")
		args = append(args, q.Status)
	} else if !q.IncludeClosed {
		where = append(where, "b.status <> 'closed'")
	}
	if q.Type != "" {
		where = append(where, "b.issue_type=?")
		args = append(args, q.Type)
	}
	if q.Assignee != "" {
		where = append(where, "b.assignee=?")
		args = append(args, q.Assignee)
	}
	if q.ParentID != "" {
		where = append(where, "b.parent_id=?")
		args = append(args, q.ParentID)
	}
	if len(q.ParentIDs) > 0 {
		placeholders := make([]string, len(q.ParentIDs))
		for i, pid := range q.ParentIDs {
			placeholders[i] = "?"
			args = append(args, pid)
		}
		// parent_id IN (...) drives off idx_beads_parent — O(matches) per id.
		where = append(where, "b.parent_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(q.IDs) > 0 {
		placeholders := make([]string, len(q.IDs))
		for i, id := range q.IDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		// id IN (...) drives off the primary key — the batched form of a Get.
		where = append(where, "b.id IN ("+strings.Join(placeholders, ",")+")")
	}
	if !q.CreatedBefore.IsZero() {
		where = append(where, "b.created_at < ?")
		args = append(args, q.CreatedBefore.UnixNano())
	}
	if !q.UpdatedBefore.IsZero() {
		where = append(where, "COALESCE(NULLIF(b.updated_at, 0), b.created_at) < ?")
		args = append(args, q.UpdatedBefore.UnixNano())
	}
	if q.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.bead_id=b.id AND l.label=?)")
		args = append(args, q.Label)
	}
	for k, v := range q.Metadata {
		// `id IN (SELECT bead_id ...)` lets SQLite drive the lookup off
		// idx_metadata_key_value(meta_key, meta_value) -> bead ids, then probe the
		// beads primary key — O(matches). The equivalent EXISTS-correlated form
		// instead SCANs every bead row (O(total beads)), which dominated graph-read
		// cost as the store grew.
		where = append(where, "b.id IN (SELECT m.bead_id FROM metadata m WHERE m.meta_key=? AND m.meta_value=?)")
		args = append(args, k, v)
	}
	sqlText := "SELECT " + projection + " FROM beads b"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	switch q.Sort {
	case SortCreatedAsc:
		sqlText += " ORDER BY b.created_at ASC, b.id ASC"
	case SortCreatedDesc:
		sqlText += " ORDER BY b.created_at DESC, b.id DESC"
	}
	if q.Limit > 0 && sqliteListCanPushLimit(q) {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return sqlText, args
}

// sqliteListCanPushLimit reports whether SQLite can apply q.Limit before the
// Go-side ListQuery match. A source-side limit is only exact when every active
// filter is represented by sqliteListSQL: the wisp tier, plural assignees, and
// seek boundary remain residual filters over decoded Beads. Applying LIMIT
// before any of those filters can discard a later matching row permanently.
func sqliteListCanPushLimit(q ListQuery) bool {
	return q.TierMode != TierWisps && len(q.Assignees) == 0 && q.SeekAfter == nil
}

// ListOpen returns non-closed beads in creation order by default.
func (s *SQLiteStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true, Sort: SortCreatedAsc}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable beads from the requested tier.
func (s *SQLiteStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	return s.readyRows(context.Background(), readyQueryFromArgs(query))
}

// ReadyContext implements ContextReadyReader for the SQLite store. The context
// reaches the driver, which interrupts an in-flight statement on cancellation,
// and the decode loop rechecks it per row, so a slow scan stops instead of
// running to completion behind an abandoned caller. Cancellation always
// surfaces as ctx.Err() — never as an empty result — so a deadline-sensitive
// caller can tell "nothing is ready" from "we never finished looking".
func (s *SQLiteStore) ReadyContext(ctx context.Context, query ...ReadyQuery) ([]Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.readyRows(ctx, readyQueryFromArgs(query))
}

// readyRows is the single ready read shared by Ready and ReadyContext, so both
// answer identically for a live context.
func (s *SQLiteStore) readyRows(ctx context.Context, q ReadyQuery) ([]Bead, error) {
	// An uncancellable context can never fail a check; skip the per-row
	// ctx.Err() calls entirely on the Ready fast path.
	cancellable := ctx.Done() != nil
	contextErr := func() error {
		if !cancellable {
			return nil
		}
		return ctx.Err()
	}

	sqlText, args := sqliteReadySQL(q, s.sqliteBeadProjection())
	rows, err := s.readDB.QueryContext(ctx, sqlText, args...)
	if err != nil {
		// A canceled read surfaces as the driver's "interrupted" error on some
		// paths; report the cause the caller can act on.
		if ctxErr := contextErr(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("listing sqlite ready beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	now := time.Now().UTC()
	for rows.Next() {
		if err := contextErr(); err != nil {
			return nil, err
		}
		b, err := scanSQLiteBead(rows)
		if err != nil {
			if ctxErr := contextErr(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if !IsReadyCandidateForTier(b, now, q.TierMode) {
			continue
		}
		result = append(result, b)
		if q.Limit > 0 && len(result) >= q.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		if ctxErr := contextErr(); ctxErr != nil {
			return nil, ctxErr
		}
		return result, err
	}
	if err := contextErr(); err != nil {
		return nil, err
	}
	return result, nil
}

// sqliteReadySQL builds the ready projection query for q. Tier and limit
// filtering is partly residual: wisp-tier reads decide tier membership after
// decode, so the source-side LIMIT is only safe for the other tier modes.
func sqliteReadySQL(q ReadyQuery, projection string) (string, []any) {
	args := []any{}
	where := []string{
		"b.status='open'",
		`b.issue_type NOT IN ('merge-request','gate','molecule','step','message','session','agent','role','rig')`,
		`NOT EXISTS (
			SELECT 1 FROM deps d
			LEFT JOIN beads blocker ON blocker.id=d.depends_on_id
			WHERE d.issue_id=b.id
			  AND d.dep_type IN ('blocks','waits-for','conditional-blocks')
			  AND COALESCE(blocker.status, '') <> 'closed'
		  )`,
	}
	switch q.TierMode {
	case TierWisps:
		// Filter after decode so NoHistory rows in SQLite's main tier are still
		// visible to logical wisp-tier reads.
	case TierBoth:
	default:
		where = append(where, "b.tier='main'")
	}
	sqlText := `SELECT ` + projection + ` FROM beads b WHERE ` + strings.Join(where, " AND ")
	if q.Assignee != "" {
		sqlText += " AND b.assignee=?"
		args = append(args, q.Assignee)
	}
	sqlText += " ORDER BY b.created_at ASC, b.id ASC"
	if q.Limit > 0 && q.TierMode != TierWisps {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return sqlText, args
}

// Children returns all non-closed beads whose ParentID matches the given ID.
func (s *SQLiteStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByLabel returns non-closed beads matching an exact label string by
// default.
func (s *SQLiteStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status.
func (s *SQLiteStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata returns beads whose metadata contains all key-value pairs.
func (s *SQLiteStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a key-value metadata pair on a bead.
func (s *SQLiteStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch atomically sets multiple metadata keys on a bead.
func (s *SQLiteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if len(kvs) == 0 {
		return nil
	}
	return s.Update(id, UpdateOpts{Metadata: maps.Clone(kvs)})
}

// SetLocalString sets a clone-local string value for a bead. See
// Store.SetLocalString. The value is persisted beside, rather than inside,
// beads.sqlite so topology snapshots and migrations never carry it between
// clones.
func (s *SQLiteStore) SetLocalString(id, key, value string) error {
	if s.readOnly {
		return fmt.Errorf("setting local string on %q: sqlite store is read-only", id)
	}
	if _, err := s.Get(id); err != nil {
		return fmt.Errorf("setting local string on %q: %w", id, err)
	}
	if err := s.localStrings.Set(id, key, value); err != nil {
		return fmt.Errorf("setting local string on %q: %w", id, err)
	}
	return nil
}

// GetLocalString returns the clone-local string value for a bead. See
// Store.GetLocalString.
func (s *SQLiteStore) GetLocalString(id, key string) (string, error) {
	if _, err := s.Get(id); err != nil {
		return "", fmt.Errorf("getting local string on %q: %w", id, err)
	}
	value, err := s.localStrings.Get(id, key)
	if err != nil {
		return "", fmt.Errorf("getting local string on %q: %w", id, err)
	}
	return value, nil
}

// Tx executes fn in one SQLite transaction. Every write is rolled back when
// the callback returns an error, so callers can safely compose Create, Update,
// metadata updates, and Close as one all-or-nothing operation.
func (s *SQLiteStore) Tx(_ string, fn func(tx Tx) error) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite tx: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(&sqliteStoreTx{store: s, ctx: ctx, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite tx: commit: %w", err)
	}
	return nil
}

// AtomicTx reports that Tx uses a real SQLite transaction and rolls all
// callback writes back when the callback fails.
func (s *SQLiteStore) AtomicTx() bool { return true }

type sqliteStoreTx struct {
	store *SQLiteStore
	ctx   context.Context
	tx    *sql.Tx
}

func (t *sqliteStoreTx) Create(b Bead) (Bead, error) {
	stored := t.store.normalizeCreate(b)
	if b.ID == "" {
		id, err := t.store.mintUniqueIDTx(t.ctx, t.tx, stored.ID, nil)
		if err != nil {
			return Bead{}, err
		}
		stored.ID = id
	} else if err := t.store.ensureCreateDoesNotExist(t.ctx, t.tx, stored.ID); err != nil {
		return Bead{}, err
	}
	if err := t.store.clearClaimFenceTx(t.ctx, t.tx, stored.ID); err != nil {
		return Bead{}, err
	}
	if err := t.store.upsertBeadTx(t.ctx, t.tx, stored); err != nil {
		return Bead{}, err
	}
	for _, dep := range depsFromBeadFields(stored) {
		if err := t.store.depAddTx(t.ctx, t.tx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			return Bead{}, err
		}
	}
	return cloneBead(stored), nil
}

func (t *sqliteStoreTx) Update(id string, opts UpdateOpts) error {
	b, err := t.store.getTx(t.ctx, t.tx, id)
	if err != nil {
		return err
	}
	before := b
	b = applySQLiteUpdateOpts(b, opts)
	b.UpdatedAt = time.Now()
	if err := t.store.upsertBeadTx(t.ctx, t.tx, b); err != nil {
		return err
	}
	return t.store.bumpClaimFenceIfOwnershipTransitionTx(t.ctx, t.tx, before, &b)
}

func (t *sqliteStoreTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	return t.Update(id, UpdateOpts{Metadata: maps.Clone(kvs)})
}

func (t *sqliteStoreTx) Close(id string) error {
	b, err := t.store.getTx(t.ctx, t.tx, id)
	if err != nil {
		return err
	}
	if b.Status == "closed" {
		return nil
	}
	before := b
	b.Status = "closed"
	b.UpdatedAt = time.Now()
	if err := t.store.upsertBeadTx(t.ctx, t.tx, b); err != nil {
		return err
	}
	return t.store.bumpClaimFenceIfOwnershipTransitionTx(t.ctx, t.tx, before, &b)
}

// Delete permanently removes a bead and its indexed rows.
func (s *SQLiteStore) Delete(id string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if err := retryOnBusy(func() error {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("sqlite delete: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		res, err := tx.Exec(`DELETE FROM beads WHERE id=?`, id)
		if err != nil {
			return fmt.Errorf("deleting bead %q: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
		}
		if err := s.clearClaimFenceTx(context.Background(), tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM deps WHERE issue_id=? OR depends_on_id=?`, id, id); err != nil {
			return fmt.Errorf("deleting bead %q deps: %w", id, err)
		}
		if err := s.clearGraphEdgeMetadataForBeadsTx(context.Background(), tx, []string{id}); err != nil {
			return err
		}
		return tx.Commit()
	}); err != nil {
		return err
	}
	if err := s.localStrings.DeleteBead(id); err != nil {
		return fmt.Errorf("deleting bead %q: cleaning up local strings: %w", id, err)
	}
	return nil
}

// DepAdd records a dependency edge.
func (s *SQLiteStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return retryOnBusy(func() error {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("sqlite dep add: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		if err := s.depAddTx(context.Background(), tx, issueID, dependsOnID, depType); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) depAddTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType string) error {
	return s.depAddWithMetadataTx(ctx, tx, issueID, dependsOnID, depType, "")
}

// depAddWithMetadataTx adds one dependency and transactionally replaces its
// Graph-only opaque metadata sidecar. The sidecar lives in kv because the
// deployed deps schemas have no metadata column; direct DepAdd calls carry no
// metadata and therefore clear a previously graph-applied value.
func (s *SQLiteStore) depAddWithMetadataTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType, metadata string) error {
	if depType == "" {
		depType = "blocks"
	}
	// One edge per (issue, depends_on) pair; a re-add with a different type
	// updates the edge's type in place — the tree's canonical bd semantics
	// (beadstest DepAddUpdatesType). The earlier schema keyed the PK on the
	// type too, which let contradictory duplicate edges accumulate.
	if s.legacyDepsPrimaryKey {
		if _, err := tx.ExecContext(ctx, `DELETE FROM deps WHERE issue_id=? AND depends_on_id=?`, issueID, dependsOnID); err != nil {
			return fmt.Errorf("replacing legacy dependency %s -> %s: %w", issueID, dependsOnID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)`, issueID, dependsOnID, depType); err != nil {
			return fmt.Errorf("adding legacy dependency %s -> %s: %w", issueID, dependsOnID, err)
		}
		return s.setGraphEdgeMetadataTx(ctx, tx, issueID, dependsOnID, depType, metadata)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)
		ON CONFLICT(issue_id, depends_on_id) DO UPDATE SET dep_type=excluded.dep_type`,
		issueID, dependsOnID, depType)
	if err != nil {
		return fmt.Errorf("adding dependency %s -> %s: %w", issueID, dependsOnID, err)
	}
	return s.setGraphEdgeMetadataTx(ctx, tx, issueID, dependsOnID, depType, metadata)
}

func (s *SQLiteStore) setGraphEdgeMetadataTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType, metadata string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key GLOB ?`, sqliteGraphEdgeMetadataPairPrefix(issueID, dependsOnID)+"*"); err != nil {
		return fmt.Errorf("clearing Graph dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
	}
	if metadata == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kv(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, sqliteGraphEdgeMetadataKey(issueID, dependsOnID, depType), metadata); err != nil {
		return fmt.Errorf("storing Graph dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove removes a dependency edge.
func (s *SQLiteStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite dep remove: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		if _, err := tx.ExecContext(ctx, `DELETE FROM deps WHERE issue_id=? AND depends_on_id=?`, issueID, dependsOnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key GLOB ?`, sqliteGraphEdgeMetadataPairPrefix(issueID, dependsOnID)+"*"); err != nil {
			return fmt.Errorf("clearing Graph dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
		}
		return tx.Commit()
	})
}

// DepList returns dependency edges for a bead.
func (s *SQLiteStore) DepList(id, direction string) ([]Dep, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	col := "issue_id"
	if direction == "up" {
		col = "depends_on_id"
	}
	rows, err := s.readDB.QueryContext(context.Background(),
		`SELECT issue_id, depends_on_id, dep_type FROM deps WHERE `+col+`=?`,
		id)
	if err != nil {
		return nil, fmt.Errorf("listing dependencies for %q: %w", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.IssueID, &d.DependsOnID, &d.Type); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) startRetentionSweeper() {
	if s.disableRetentionSweeper || s.retentionPeriod <= 0 || s.retentionSweepInterval <= 0 {
		s.retentionStop = func() {}
		s.retentionDone = make(chan struct{})
		close(s.retentionDone)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.retentionStop = cancel
	s.retentionDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = s.purgeTerminal(context.Background(), s.retentionPeriod)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *SQLiteStore) purgeTerminal(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan).UnixNano()
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT id FROM beads
		WHERE tier='main'
		  AND status IN ('closed','cancelled','canceled','expired')
		  AND COALESCE(NULLIF(updated_at,0), created_at) < ?
		ORDER BY updated_at ASC
		LIMIT 1000`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sqlite purge terminal query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.Delete(id); err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	return len(ids), nil
}

func ptrTo(v string) *string {
	return &v
}

// numericIDSuffix parses the trailing numeric portion of a bead ID like
// "gc-42" and returns 42. Returns 0 if the ID has no numeric suffix.
func numericIDSuffix(id string) int {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] < '0' || id[i] > '9' {
			if i == len(id)-1 {
				return 0
			}
			n, _ := strconv.Atoi(id[i+1:])
			return n
		}
	}
	n, _ := strconv.Atoi(id)
	return n
}
