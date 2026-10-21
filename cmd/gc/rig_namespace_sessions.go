package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// poolSessionBeadIDPattern is the shape of a pool session bead id the city
// pins itself: "<store prefix>-session-<instance token>", the token being 16
// random bytes in lower-case hex (session.NewInstanceToken,
// poolSessionExplicitBeadID). Ids of that shape cannot collide across cities.
// Every other session id is minted by the store ("<prefix>-<n>", or a short
// hash) and repeats across cities, so it is never admitted by provenance.
//
// A prefix segment may hold '_' and '.': bd init turns dots in a prefix into
// underscores, and a derived prefix can keep a dot.
var poolSessionBeadIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.]+(?:-[A-Za-z0-9_.]+)*-session-[0-9a-f]{32}$`)

// embeddedPoolSessionIDPattern matches any value that ends in a pool session
// bead id of any city's prefix: the id itself, or a pool session's runtime
// name "<template base>-<bead id>" (PoolSessionName). Such a value names a
// session of whichever city minted the id, so it is admitted only through
// this city's own session records, never by its spelling.
//
// It requires nothing before "-session-": a store prefix may end in any
// character a store accepts, and matching more values only sends more of
// them to the record check.
var embeddedPoolSessionIDPattern = regexp.MustCompile(`-session-[0-9a-f]{32}$`)

// maxOwnSessionIdentityLen bounds the identities looked up in the session
// store. A pool session name is "<template base>-<bead id>"; anything longer
// is not one of ours. It bounds lookups only: a longer value that embeds a
// pool id is still refused by the lexical arm.
const maxOwnSessionIdentityLen = 512

// isPoolSessionBeadID reports whether id has the unguessable pool shape.
func isPoolSessionBeadID(id string) bool {
	return len(id) <= maxOwnSessionIdentityLen && poolSessionBeadIDPattern.MatchString(id)
}

// embedsPoolSessionID reports whether v, trimmed, is a pool session bead id
// or ends in one after a separator: whatever its spelling, it names a pool
// session, and only the session records of the city that minted it can say
// whose.
func embedsPoolSessionID(v string) bool {
	return embeddedPoolSessionIDPattern.MatchString(strings.TrimSpace(v))
}

// ownPoolSessionIDCandidates returns the bead ids v could name in this
// city's session store: for each of the city's session id prefixes p, v
// itself when it is a pool id "<p>-session-<token>", or the last such id v
// ends with after a "-" (a pool session name). The candidates are lookup keys
// only; the record found must still answer to v.
func ownPoolSessionIDCandidates(v string, prefixes []string) []string {
	if v == "" || len(v) > maxOwnSessionIdentityLen {
		return nil
	}
	var out []string
	add := func(id string) {
		if !isPoolSessionBeadID(id) {
			return
		}
		for _, have := range out {
			if have == id {
				return
			}
		}
		out = append(out, id)
	}
	for _, p := range prefixes {
		marker := p + "-session-"
		if strings.HasPrefix(v, marker) {
			add(v)
			continue
		}
		if i := strings.LastIndex(v, "-"+marker); i >= 0 {
			add(v[i+1:])
		}
	}
	return out
}

// normalizeSessionIDPrefixes trims each prefix the way pool ids are minted
// (poolSessionExplicitBeadID) and drops blanks and duplicates.
func normalizeSessionIDPrefixes(prefixes []string) []string {
	var out []string
	for _, p := range prefixes {
		p = strings.Trim(strings.TrimSpace(p), "-")
		if p == "" {
			continue
		}
		dup := false
		for _, have := range out {
			if have == p {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// ownSessionRecord is what provenance needs from one of the city's session
// records.
type ownSessionRecord struct {
	ID          string
	SessionName string
	Template    string
}

// ownSessionResolver finds this city's own session records. Implementations
// must read only the city's own session records (its sessions store), never
// a rig's store that other cities share and never a census that unions
// stores, and must never be shared between cities: the supervisor runs
// several cities in one process. A failure to read returns an error, never a
// guess.
type ownSessionResolver interface {
	// lookupOwnSession finds the pool session record v names: a pool
	// session bead id, or a pool session name ending in one. It looks up
	// only ids carrying the city's own session id prefixes.
	lookupOwnSession(v string) (ownSessionRecord, bool, error)
	// lookupOwnSessionsByName finds the session records whose recorded
	// session_name is name, whatever the shape of their ids.
	lookupOwnSessionsByName(name string) ([]ownSessionRecord, error)
}

// sessionRecordIndex is the in-memory view of the city's open session
// records the controller keeps (*sessionBeadSnapshot).
type sessionRecordIndex interface {
	FindInfoByID(id string) (sessionpkg.Info, bool)
	FindInfoBySessionName(name string) (sessionpkg.Info, bool)
}

var _ sessionRecordIndex = (*sessionBeadSnapshot)(nil)

const (
	// ownSessionNegativeTTL is how long a "no such record" answer is reused.
	// This city's own sessions reach its snapshot, which is consulted first,
	// so a long-lived miss cannot hide one of its open sessions.
	ownSessionNegativeTTL = time.Hour
	// ownSessionNegativeLimit bounds the remembered misses. Hits are this
	// city's own records and are never evicted.
	ownSessionNegativeLimit = 4096
	// ownSessionErrorBackoff is how long lookups fail fast after the
	// session store fails, unless beginPass ends it earlier.
	ownSessionErrorBackoff = 15 * time.Second
	// ownSessionHitLimit bounds the remembered pool session records, keyed
	// by their bead ids. It is far above any city's session count; reaching
	// it clears them, and they are read again on demand.
	ownSessionHitLimit = 65536
	// ownSessionNameHitLimit bounds the remembered name answers; reaching it
	// clears them.
	ownSessionNameHitLimit = 4096
	// ownSessionNameLookupLimit bounds the records one session_name query
	// returns. Records past it are not seen, so a name whose rig record lies
	// beyond this many records of the same name stays unadmitted (fail
	// closed). A session name is reused by few records.
	ownSessionNameLookupLimit = 64
)

// citySessionResolver is the ownSessionResolver of one city. It answers from,
// in order:
//
//  1. the caller's own session (one-shot commands only): GC_SESSION_ID,
//     GC_SESSION_NAME and GC_TEMPLATE, used only when GC_CITY_PATH (or
//     GC_CITY) names this city;
//  2. the controller's snapshot of the city's open session records;
//  3. a memo of earlier store answers: pool session records are kept by
//     their bead ids (bounded by ownSessionHitLimit) and answer every
//     spelling of their id and name; name answers and misses are kept for
//     ownSessionNegativeTTL; errors are never kept;
//  4. the city's session store: a read by id for each candidate id that
//     carries the city's own session id prefix, or a session_name query.
//
// Concurrent lookups of one key share one store read; a read that fails or
// panics is an error to every caller waiting on it. After a store error,
// lookups that need the store fail fast until ownSessionErrorBackoff passes
// or beginPass is called, and the store handle is reopened on the next
// attempt. It is safe for concurrent use.
type citySessionResolver struct {
	cityPath    string
	envFastPath bool
	self        *ownSessionRecord
	snapshot    func() sessionRecordIndex
	openStore   func() (beads.SessionStore, error)
	idPrefixes  []string
	now         func() time.Time

	// openMu serializes opening the session store; it is never held with mu.
	openMu sync.Mutex
	store  beads.SessionStore

	mu        sync.Mutex
	learned   []string
	hits      map[string]ownSessionRecord
	nameHits  map[string]ownSessionNameHit
	misses    map[string]time.Time
	inflight  map[string]*ownSessionCall
	failUntil time.Time
	failErr   error
}

type ownSessionNameHit struct {
	recs []ownSessionRecord
	at   time.Time
}

type ownSessionCall struct {
	done chan struct{}
	recs []ownSessionRecord
	err  error
}

// citySessionResolverOptions configures newCitySessionResolver. Every source
// is optional; with none, the resolver finds nothing.
type citySessionResolverOptions struct {
	// CityPath is the city whose sessions these are.
	CityPath string
	// Getenv, when set, enables the caller's-own-session fast path. Only
	// one-shot commands set it; the controller serves many sessions.
	Getenv func(string) string
	// Snapshot returns the controller's current snapshot of open session
	// records. It must be loaded from this city's sessions store alone,
	// never from a census that also reads rig stores: another city's session
	// rows in a shared rig store would otherwise vouch for its sessions.
	Snapshot func() sessionRecordIndex
	// OpenStore returns this city's sessions store (the same store the
	// snapshot is loaded from), never a rig's store. It is called lazily,
	// again after a store error and after invalidateStore.
	OpenStore func() (beads.SessionStore, error)
	// IDPrefixes are the bead id prefixes of this city's session records:
	// the sessions store's IDPrefix and, for records migrated from the city
	// store, the city's HQ prefix. Never a rig's prefix. The opened store's
	// own IDPrefix, when it has one, is added: it is found through the handle
	// and any bead-policy layer (beads.IDPrefixFor). Pool ids under any
	// other prefix are never looked up.
	IDPrefixes []string
	// Now is the clock for the memo and the error backoff (time.Now when
	// nil).
	Now func() time.Time
}

// newCitySessionResolver returns a resolver for one city.
func newCitySessionResolver(opts citySessionResolverOptions) *citySessionResolver {
	r := &citySessionResolver{
		cityPath:   strings.TrimSpace(opts.CityPath),
		snapshot:   opts.Snapshot,
		openStore:  opts.OpenStore,
		idPrefixes: normalizeSessionIDPrefixes(opts.IDPrefixes),
		now:        opts.Now,
		hits:       make(map[string]ownSessionRecord),
		nameHits:   make(map[string]ownSessionNameHit),
		misses:     make(map[string]time.Time),
		inflight:   make(map[string]*ownSessionCall),
	}
	if r.now == nil {
		r.now = time.Now
	}
	if opts.Getenv != nil {
		r.envFastPath = true
		r.self = selfSessionFromEnv(r.cityPath, opts.Getenv)
	}
	return r
}

// newControllerSessionResolver returns the controller's resolver for one
// city: its open-session snapshot, then reads of its sessions store. It
// never reads the process environment. snapshot and sessionStore must come
// from the city's sessions store only (see citySessionResolverOptions);
// idPrefixes are its session id prefixes. Build one per city, and a new one
// when the city's sessions store changes on reload.
func newControllerSessionResolver(cityPath string, snapshot func() sessionRecordIndex, sessionStore func() (beads.SessionStore, error), idPrefixes ...string) *citySessionResolver {
	return newCitySessionResolver(citySessionResolverOptions{CityPath: cityPath, Snapshot: snapshot, OpenStore: sessionStore, IDPrefixes: idPrefixes})
}

// newCLISessionResolver returns a one-shot command's resolver: the caller's
// own session from the environment, then the city's sessions store, opened
// lazily. openSessionStore must open the city's sessions store, never a rig
// store. The supervisor and the controller never use it.
func newCLISessionResolver(cityPath string, openSessionStore func() (beads.SessionStore, error), idPrefixes ...string) *citySessionResolver {
	return newCitySessionResolver(citySessionResolverOptions{CityPath: cityPath, Getenv: os.Getenv, OpenStore: openSessionStore, IDPrefixes: idPrefixes})
}

// selfSessionFromEnv returns the caller's own session record from its
// runtime environment, or nil when the environment names no session or
// belongs to another city.
func selfSessionFromEnv(cityPath string, getenv func(string) string) *ownSessionRecord {
	if cityPath == "" {
		return nil
	}
	envCity := strings.TrimSpace(getenv("GC_CITY_PATH"))
	if envCity == "" {
		envCity = strings.TrimSpace(getenv("GC_CITY"))
	}
	if envCity == "" || !samePath(envCity, cityPath) {
		return nil
	}
	rec := ownSessionRecord{
		ID:          strings.TrimSpace(getenv("GC_SESSION_ID")),
		SessionName: strings.TrimSpace(getenv("GC_SESSION_NAME")),
		Template:    strings.TrimSpace(getenv("GC_TEMPLATE")),
	}
	if rec.ID == "" || rec.Template == "" {
		return nil
	}
	return &rec
}

// beginPass ends an error backoff, so the next lookup that needs the
// session store tries it again. The controller calls it at the start of a
// pass.
func (r *citySessionResolver) beginPass() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.failUntil = time.Time{}
	r.failErr = nil
	r.mu.Unlock()
}

// invalidateStore drops the cached session store handle, so the next lookup
// reopens it (after a reload replaced it, for example).
func (r *citySessionResolver) invalidateStore() {
	if r == nil {
		return
	}
	r.openMu.Lock()
	r.store = beads.SessionStore{}
	r.openMu.Unlock()
}

// lookupOwnSession implements ownSessionResolver.
func (r *citySessionResolver) lookupOwnSession(v string) (ownSessionRecord, bool, error) {
	if r == nil {
		return ownSessionRecord{}, false, nil
	}
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxOwnSessionIdentityLen || !embedsPoolSessionID(v) {
		return ownSessionRecord{}, false, nil
	}
	if r.self != nil && ownSessionAnswersTo(*r.self, v) {
		return *r.self, true, nil
	}
	if idx := r.index(); idx != nil {
		if info, ok := idx.FindInfoByID(v); ok {
			return ownSessionRecordFromInfo(info), true, nil
		}
		if info, ok := idx.FindInfoBySessionName(v); ok {
			return ownSessionRecordFromInfo(info), true, nil
		}
	}
	if rec, ok := r.poolHit(v); ok {
		return rec, true, nil
	}
	recs, err := r.cached("id\x00"+v, func(store beads.SessionStore, prefixes []string) ([]ownSessionRecord, error) {
		return readPoolSessionRecord(store, v, prefixes)
	}, func(recs []ownSessionRecord) {
		if len(r.hits) >= ownSessionHitLimit {
			r.hits = make(map[string]ownSessionRecord)
		}
		for _, rec := range recs {
			r.hits[rec.ID] = rec
		}
	})
	if err != nil || len(recs) == 0 {
		return ownSessionRecord{}, false, err
	}
	return recs[0], true, nil
}

// poolHit answers v from the remembered pool session records: a record
// whose id is one of v's candidate ids and that answers to v.
func (r *citySessionResolver) poolHit(v string) (ownSessionRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ownPoolSessionIDCandidates(v, r.prefixesLocked()) {
		if rec, ok := r.hits[id]; ok && ownSessionAnswersTo(rec, v) {
			return rec, true
		}
	}
	return ownSessionRecord{}, false
}

// prefixesLocked returns the configured session id prefixes plus the ones
// learned from the opened store; callers hold mu.
func (r *citySessionResolver) prefixesLocked() []string {
	if len(r.learned) == 0 {
		return r.idPrefixes
	}
	return normalizeSessionIDPrefixes(append(append([]string(nil), r.idPrefixes...), r.learned...))
}

// lookupOwnSessionsByName implements ownSessionResolver.
func (r *citySessionResolver) lookupOwnSessionsByName(name string) ([]ownSessionRecord, error) {
	if r == nil {
		return nil, nil
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxOwnSessionIdentityLen {
		return nil, nil
	}
	if r.self != nil && r.self.SessionName != "" && ownSessionNameIs(*r.self, name) {
		return []ownSessionRecord{*r.self}, nil
	}
	if idx := r.index(); idx != nil {
		if info, ok := idx.FindInfoBySessionName(name); ok {
			return []ownSessionRecord{ownSessionRecordFromInfo(info)}, nil
		}
	}
	r.mu.Lock()
	if h, ok := r.nameHits[name]; ok {
		if r.now().Sub(h.at) < ownSessionNegativeTTL {
			r.mu.Unlock()
			return h.recs, nil
		}
		delete(r.nameHits, name)
	}
	r.mu.Unlock()
	return r.cached("name\x00"+name, func(store beads.SessionStore, _ []string) ([]ownSessionRecord, error) {
		return readSessionRecordsByName(store, name)
	}, func(recs []ownSessionRecord) {
		if len(r.nameHits) >= ownSessionNameHitLimit {
			r.nameHits = make(map[string]ownSessionNameHit)
		}
		r.nameHits[name] = ownSessionNameHit{recs: recs, at: r.now()}
	})
}

func (r *citySessionResolver) index() sessionRecordIndex {
	if r.snapshot == nil {
		return nil
	}
	return r.snapshot()
}

func ownSessionRecordFromInfo(info sessionpkg.Info) ownSessionRecord {
	// The recorded session_name, not Info.SessionName's derived fallback.
	return ownSessionRecord{ID: info.ID, SessionName: info.SessionNameMetadata, Template: info.Template}
}

// cached answers key from the miss memo, or runs read against the session
// store once for all concurrent callers. A non-empty answer is handed to
// rememberHit (under mu), an empty one remembered as a miss, and an error
// (or a panic in read, which is re-raised) starts the backoff.
func (r *citySessionResolver) cached(key string, read func(beads.SessionStore, []string) ([]ownSessionRecord, error), rememberHit func([]ownSessionRecord)) ([]ownSessionRecord, error) {
	if r.openStore == nil {
		return nil, nil
	}
	r.mu.Lock()
	if at, ok := r.misses[key]; ok {
		if r.now().Sub(at) < ownSessionNegativeTTL {
			r.mu.Unlock()
			return nil, nil
		}
		delete(r.misses, key)
	}
	if r.failErr != nil && r.now().Before(r.failUntil) {
		err := r.failErr
		r.mu.Unlock()
		return nil, fmt.Errorf("session records unavailable: %w", err)
	}
	if c, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		<-c.done
		return c.recs, c.err
	}
	c := &ownSessionCall{done: make(chan struct{})}
	r.inflight[key] = c
	r.mu.Unlock()

	defer func() {
		p := recover()
		if p != nil {
			c.recs = nil
			c.err = fmt.Errorf("reading session records panicked: %v", p)
			r.invalidateStore()
		}
		r.mu.Lock()
		delete(r.inflight, key)
		switch {
		case c.err != nil:
			r.failErr = c.err
			r.failUntil = r.now().Add(ownSessionErrorBackoff)
		case len(c.recs) > 0:
			rememberHit(c.recs)
		default:
			r.rememberMissLocked(key)
		}
		r.mu.Unlock()
		close(c.done)
		if p != nil {
			panic(p)
		}
	}()
	c.recs, c.err = r.read(read)
	return c.recs, c.err
}

func (r *citySessionResolver) read(read func(beads.SessionStore, []string) ([]ownSessionRecord, error)) ([]ownSessionRecord, error) {
	store, err := r.sessionStore()
	if err != nil {
		return nil, fmt.Errorf("reading session records: %w", err)
	}
	// Ask the store outside mu: a panic in it must not leave mu held.
	learned := normalizeSessionIDPrefixes([]string{beads.IDPrefixFor(store)})
	r.mu.Lock()
	if learned != nil {
		r.learned = learned
	}
	prefixes := r.prefixesLocked()
	r.mu.Unlock()
	recs, err := read(store, prefixes)
	if err != nil {
		// The handle may be stale (closed by a reload): reopen next time.
		r.invalidateStore()
		return nil, err
	}
	return recs, nil
}

// rememberMissLocked records a miss, evicting expired misses first and then
// all misses when the bound is reached. Pool session records are never
// evicted by misses.
func (r *citySessionResolver) rememberMissLocked(key string) {
	now := r.now()
	if len(r.misses) >= ownSessionNegativeLimit {
		for k, at := range r.misses {
			if now.Sub(at) >= ownSessionNegativeTTL {
				delete(r.misses, k)
			}
		}
		if len(r.misses) >= ownSessionNegativeLimit {
			r.misses = make(map[string]time.Time)
		}
	}
	r.misses[key] = now
}

func (r *citySessionResolver) sessionStore() (beads.SessionStore, error) {
	r.openMu.Lock()
	defer r.openMu.Unlock()
	if !noSessionStore(r.store) {
		return r.store, nil
	}
	s, err := r.openStore()
	if err != nil {
		return beads.SessionStore{}, err
	}
	if noSessionStore(s) {
		return beads.SessionStore{}, errors.New("no session store")
	}
	r.store = s
	return s, nil
}

// noSessionStore reports whether s is no store: the zero handle (none opened
// yet), or a handle over a nil store (beads.IsNilStore).
func noSessionStore(s beads.SessionStore) bool { return beads.IsNilStore(s) }

// readPoolSessionRecord reads each candidate id of v (own prefixes only) and
// returns the session record that answers to v. A candidate that is absent,
// is not a session record, or comes back under another id (a store that
// resolves partial ids) is a miss; any other read error fails the lookup.
func readPoolSessionRecord(store beads.SessionStore, v string, prefixes []string) ([]ownSessionRecord, error) {
	for _, id := range ownPoolSessionIDCandidates(v, prefixes) {
		b, err := store.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading session record %s: %w", id, err)
		}
		if !sessionpkg.IsSessionBeadOrRepairable(b) || strings.TrimSpace(b.ID) != id {
			continue
		}
		rec := ownSessionRecordFromBead(b)
		if ownSessionAnswersTo(rec, v) {
			return []ownSessionRecord{rec}, nil
		}
	}
	return nil, nil
}

// readSessionRecordsByName queries the session records, open or closed,
// whose recorded session_name is name.
func readSessionRecordsByName(store beads.SessionStore, name string) ([]ownSessionRecord, error) {
	rows, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"session_name": name},
		IncludeClosed: true,
		Limit:         ownSessionNameLookupLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("reading session records named %q: %w", name, err)
	}
	var out []ownSessionRecord
	for _, b := range rows {
		if !sessionpkg.IsSessionBeadOrRepairable(b) {
			continue
		}
		rec := ownSessionRecordFromBead(b)
		if strings.TrimSpace(rec.SessionName) == name {
			out = append(out, rec)
		}
	}
	return out, nil
}

func ownSessionRecordFromBead(b beads.Bead) ownSessionRecord {
	return ownSessionRecord{
		ID:          strings.TrimSpace(b.ID),
		SessionName: strings.TrimSpace(b.Metadata["session_name"]),
		Template:    strings.TrimSpace(b.Metadata["template"]),
	}
}
