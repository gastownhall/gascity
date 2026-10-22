package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The two-city matrix of the rig namespace guard.
//
// Cities A and B are real directories that import the same pack, "pk": a
// city-scoped pool pk.intake, a city-scoped named agent pk.lead and a
// rig-scoped pool pk.worker. City A attaches rig "alpha", city B rig "beta".
// Both rigs have the SAME bead prefix ("sh") and the same store identity
// (dolt_database and project_id), and both cities' controllers are handed ONE
// shared store handle through the controller's rig opener and the by-path
// open body, keyed on (city path, scope root). Both HQs use the same id
// prefix, so the cities' store-minted session ids collide ("gcx-17"); only the
// 128-bit pool tokens tell their pool sessions apart.
//
// Each row of tcRows is one colliding-spelling shape (labeled r1–r12 below,
// with ids sh-r1 and up), with its owner: city A, city B, neither (another
// city's or a person's, in every colliding spelling), or nobody can tell
// (Unattributed). Each column is one guarded path of the store guard, run in
// each city. With federation = "rig" on both attachments, a city acts only on
// its own rows (and, where the path keeps today's behavior for them,
// Unattributed ones). With federation = "city" the same columns pin today's
// leak.
//
// Three things keep a column from passing vacuously:
//   - the same prefix, so no by-id path can tell the rigs apart by prefix;
//   - a positive control per column (each city does act on a row of its own);
//   - the raw shared engine and each controller's unfiltered cache are checked
//     to hold every row a column filters, so a stale cache cannot fake a
//     filter.

const (
	tcPrefix     = "sh"
	tcHQPrefix   = "gcx"
	tcDatabase   = "shared_db"
	tcProjectID  = "prj-0000-shared"
	tcCityPool   = "pk.intake"
	tcCityAlias  = "pk.intake-2"
	tcNamed      = "pk.lead"
	tcShortID    = "gcx-17"
	tcPerson     = "alice"
	tcPoolTokenA = "aaaaaaaaaaaaaaaa0000000000000001"
	tcPoolTokenB = "bbbbbbbbbbbbbbbb0000000000000001"
	tcDeadTokenA = "aaaaaaaaaaaaaaaa0000000000000002"
	tcDeadTokenB = "bbbbbbbbbbbbbbbb0000000000000002"
)

type tcOwner int

const (
	tcNeither tcOwner = iota // owned by no city: another city's spelling, a person, a bare route
	tcA
	tcB
	tcUnattributed
)

func (o tcOwner) String() string {
	return [...]string{"neither", "A", "B", "unattributed"}[o]
}

// tcCity is one city of the fixture.
type tcCity struct {
	label   string // "A" or "B"
	owner   tcOwner
	name    string
	path    string
	rig     string
	rigDir  string
	cfg     *config.City
	hq      *rnsCountingStore // the city's own store: HQ rows and session records
	ep      *events.Fake
	cs      *controllerState
	pool    string // this city's live pool session (128-bit)
	dead    string // this city's closed pool session (128-bit)
	worker  string // <rig>/pk.worker
	workerN string // <rig>/pk.worker-1, a qualified pool alias
}

func (c *tcCity) rigStore(t *testing.T) beads.Store {
	t.Helper()
	s := c.cs.BeadStore(c.rig)
	if s == nil {
		t.Fatalf("city %s has no store for rig %s", c.label, c.rig)
	}
	return s
}

// actors are the identities this city's own agents claim and query under:
// its pool sessions and qualified alias, and the city-scoped spellings both
// cities share (the pool alias, the named agent and the short session id).
func (c *tcCity) actors() []string {
	return []string{c.pool, c.workerN, tcCityAlias, tcNamed, tcShortID}
}

// routes are the routes this city's work queries serve: the rig pool's
// qualified route and the city pool's bare one.
func (c *tcCity) routes() []string { return []string{c.worker, tcCityPool} }

type tcFixture struct {
	mode   string // config.RigFederationRig or config.RigFederationCity
	root   string
	engine *beads.MemStore // the one shared store both rigs open
	a, b   *tcCity

	mu    sync.Mutex
	opens map[string]int // "city|scope" -> rig opens
}

func (f *tcFixture) cities() []*tcCity { return []*tcCity{f.a, f.b} }

// tcPackFiles is the pack both cities import.
var tcPackFiles = map[string]string{
	"pack.toml":                  "[pack]\nname = \"pk\"\nversion = \"0.1.0\"\nschema = 2\n",
	"agents/intake/agent.toml":   "scope = \"city\"\nmin_active_sessions = 0\nmax_active_sessions = 3\n",
	"agents/intake/prompt.md":    "You take work in.\n",
	"agents/lead/agent.toml":     "scope = \"city\"\n",
	"agents/lead/prompt.md":      "You lead.\n",
	"agents/worker/agent.toml":   "scope = \"rig\"\nmin_active_sessions = 0\nmax_active_sessions = 3\n",
	"agents/worker/prompt.md":    "You work.\n",
	"agents/dispatch/agent.toml": "scope = \"rig\"\n",
	"agents/dispatch/prompt.md":  "You dispatch.\n",
}

func tcWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tcWriteCity lays out one city on disk: pack.toml importing the shared pack,
// city.toml with the rig and its federation, the site binding, and the bd
// metadata of the HQ and the rig. The rig's metadata names the shared
// database and project id, as both cities' rigs do in a shared store.
func tcWriteCity(t *testing.T, root, name, rig, federation string) (cityPath, rigDir string) {
	t.Helper()
	cityPath = filepath.Join(root, name)
	rigDir = filepath.Join(cityPath, rig)
	tcWrite(t, filepath.Join(cityPath, "pack.toml"), fmt.Sprintf("[pack]\nname = %q\nversion = \"0.1.0\"\nschema = 2\n\n[imports.pk]\nsource = \"../pk\"\n", name))
	fed := ""
	if federation != "" {
		fed = fmt.Sprintf("federation = %q\n", federation)
	}
	tcWrite(t, filepath.Join(cityPath, "city.toml"), fmt.Sprintf("[beads]\nprovider = \"bd\"\n\n[[rigs]]\nname = %q\nprefix = %q\n%s\n[rigs.imports.pk]\nsource = \"../pk\"\n", rig, tcPrefix, fed))
	tcWrite(t, filepath.Join(cityPath, ".gc", "site.toml"), fmt.Sprintf("workspace_name = %q\nworkspace_prefix = %q\n\n[[rig]]\nname = %q\npath = %q\n", name, tcHQPrefix, rig, rigDir))
	tcWrite(t, filepath.Join(cityPath, ".beads", "metadata.json"), fmt.Sprintf(`{"backend":"dolt","dolt_database":"hq_%s","project_id":"prj-hq-%s"}`, rig, rig))
	tcWrite(t, filepath.Join(rigDir, ".beads", "metadata.json"), fmt.Sprintf(`{"backend":"dolt","dolt_database":%q,"project_id":%q}`, tcDatabase, tcProjectID))
	return cityPath, rigDir
}

// tcSessionRecords are a city's own session records: its live and closed
// pool sessions (template <rig>/pk.worker) and a city-pool session whose
// store-minted id collides with the other city's.
func tcSessionRecords(rig, pool, dead string, shortOpen bool) []beads.Bead {
	live := rnsSessionBead(pool, rig+"/pk.worker", "pk.worker-"+pool)
	closed := rnsSessionBead(dead, rig+"/pk.worker", "pk.worker-"+dead)
	closed.Status = "closed"
	short := rnsSessionBead(tcShortID, tcCityPool, "pk.intake-"+tcShortID)
	short.Metadata["alias"] = tcCityAlias
	if !shortOpen {
		short.Status = "closed"
	}
	return []beads.Bead{live, closed, short}
}

// newTCFixture builds both cities over one shared engine seeded with rows,
// before either controller state is built, so each cache primes them.
func newTCFixture(t *testing.T, mode string) *tcFixture {
	t.Helper()
	isolateRigNamespaceResolvers(t)
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("BEADS_ACTOR", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_TEMPLATE", "")
	// Not t.TempDir: the caches' background prime may still be running when
	// the test ends, and TempDir fails on a busy directory.
	root, err := os.MkdirTemp("", "tcfix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for rel, body := range tcPackFiles {
		tcWrite(t, filepath.Join(root, "pk", rel), body)
	}
	fed := mode
	if mode == config.RigFederationCity {
		fed = "" // today's default
	}
	f := &tcFixture{mode: mode, root: root, opens: map[string]int{}}
	mk := func(label string, owner tcOwner, name, rig, poolToken, deadToken string, shortOpen bool) *tcCity {
		cityPath, rigDir := tcWriteCity(t, root, name, rig, fed)
		cfg, err := loadCityConfig(cityPath, io.Discard)
		if err != nil {
			t.Fatalf("load city %s: %v", name, err)
		}
		pool := tcHQPrefix + "-session-" + poolToken
		dead := tcHQPrefix + "-session-" + deadToken
		return &tcCity{
			label: label, owner: owner, name: name, path: cityPath, rig: rig, rigDir: rigDir, cfg: cfg,
			hq:     rnsNewCountingStore(tcSessionRecords(rig, pool, dead, shortOpen)...),
			ep:     events.NewFake(),
			pool:   pool,
			dead:   dead,
			worker: rig + "/pk.worker", workerN: rig + "/pk.worker-1",
		}
	}
	// City A's short-id session is live; city B's (same id) is closed.
	f.a = mk("A", tcA, "city-a", "alpha", tcPoolTokenA, tcDeadTokenA, true)
	f.b = mk("B", tcB, "city-b", "beta", tcPoolTokenB, tcDeadTokenB, false)
	for _, c := range f.cities() {
		tcAssertPack(t, c)
	}

	f.engine = beads.NewMemStoreFrom(0, tcRowBeads(f), nil)
	f.engine.IDPrefix = tcPrefix
	f.engine.HonorExplicitIDs = true
	f.installSeams(t)

	// A context with no Done: the caches run no background prime or
	// reconciler, so the test decides when each cache absorbs the shared
	// store's changes (syncCache), and an async prime can never absorb a
	// change silently in the middle of a column.
	ctx := context.Background()
	sp := runtime.NewFake()
	for _, c := range f.cities() {
		c.cs = newControllerState(ctx, c.cfg, sp, c.ep, c.name, c.path)
	}
	for _, c := range f.cities() {
		f.requireShared(t, c)
	}
	return f
}

// tcAssertPack pins the identities the pack gives each city, so the rows
// below spell them the way gc does.
func tcAssertPack(t *testing.T, c *tcCity) {
	t.Helper()
	got := map[string]bool{}
	for _, a := range c.cfg.Agents {
		got[a.QualifiedName()] = true
	}
	for _, want := range []string{tcCityPool, tcNamed, c.worker} {
		if !got[want] {
			t.Fatalf("city %s: the pack gave no agent %q (agents: %v)", c.label, want, got)
		}
	}
	if len(c.cfg.Rigs) != 1 || c.cfg.Rigs[0].EffectivePrefix() != tcPrefix {
		t.Fatalf("city %s: rigs = %+v, want one rig with prefix %q", c.label, c.cfg.Rigs, tcPrefix)
	}
}

// installSeams points both seams at the fixture: the controller's rig opener
// and the by-path open body hand the shared engine to each city's rig scope
// and each city's own store to its HQ scope, keyed on (city path, scope
// root). Any other key is an error, so a city opening the other city's scope
// fails loudly instead of reaching the shared engine.
func (f *tcFixture) installSeams(t *testing.T) {
	t.Helper()
	open := func(opts beads.StoreOpenOptions, rigOpen bool) (beads.StoreOpenResult, error) {
		city := normalizePathForCompare(opts.CityPath)
		scope := normalizePathForCompare(opts.ScopeRoot)
		for _, c := range f.cities() {
			if city != normalizePathForCompare(c.path) {
				continue
			}
			if scope != normalizePathForCompare(c.rigDir) && scope != normalizePathForCompare(c.path) && tcNamesSharedDatabase(opts.ScopeRoot) {
				// Another rig of this city whose metadata names the shared
				// database opens the same store.
				return beads.StoreOpenResult{Store: f.engine}, nil
			}
			switch scope {
			case normalizePathForCompare(c.rigDir):
				f.mu.Lock()
				f.opens[c.label+"|"+c.rig]++
				f.mu.Unlock()
				return beads.StoreOpenResult{Store: f.engine}, nil
			case normalizePathForCompare(c.path):
				if rigOpen {
					break
				}
				return beads.StoreOpenResult{Store: c.hq}, nil
			}
		}
		return beads.StoreOpenResult{}, fmt.Errorf("tc fixture: no store for city %s scope %s", opts.CityPath, opts.ScopeRoot)
	}
	prevRig := controllerStateOpenRigStoreAtForCity
	controllerStateOpenRigStoreAtForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return open(opts, true)
	}
	prevOpen := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return open(opts, false)
	}
	// The city-store stub is city-aware: each controller gets its own HQ.
	// (stubControllerCityStore hands every city one store and would merge
	// the two HQs, and with them the cities' session records.)
	prevCity := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(cityPath string, _ gate.Mode, _ beads.NativeTransportMode) (beads.StoreOpenResult, error) {
		for _, c := range f.cities() {
			if normalizePathForCompare(cityPath) == normalizePathForCompare(c.path) {
				return beads.StoreOpenResult{Store: c.hq}, nil
			}
		}
		return beads.StoreOpenResult{}, fmt.Errorf("tc fixture: no city store for %s", cityPath)
	}
	t.Cleanup(func() {
		controllerStateOpenRigStoreAtForCity = prevRig
		openStoreFactoryForCity = prevOpen
		newControllerStateOpenCityStore = prevCity
	})
}

func tcNamesSharedDatabase(scope string) bool {
	data, err := os.ReadFile(filepath.Join(scope, ".beads", "metadata.json"))
	return err == nil && bytes.Contains(data, []byte(tcDatabase))
}

// requireShared checks the city's rig store sits over the one shared engine,
// guarded with its own resolver in rig mode and unguarded in city mode, and
// that its cache holds every row (no stale cache can fake a filter).
func (f *tcFixture) requireShared(t *testing.T, c *tcCity) {
	t.Helper()
	store := c.rigStore(t)
	base, _, _ := unwrapBeadPolicyStore(splitStrictFrontOf(store))
	inner, ns, guarded := unwrapRigNamespaceStore(base)
	if f.mode == config.RigFederationRig {
		if !guarded {
			t.Fatalf("city %s: rig %s with federation = \"rig\" opened unguarded (%T)", c.label, c.rig, base)
		}
		if ns.rigName() != c.rig || ns.sessions != ownSessionResolver(c.cs.rigSessions) {
			t.Fatalf("city %s: the guard is rig %q's with resolver %T, want rig %q's with the city's own", c.label, ns.rigName(), ns.sessions, c.rig)
		}
	} else if guarded {
		t.Fatalf("city %s: an unguarded rig opened behind a guard", c.label)
	}
	cache, ok := inner.(*beads.CachingStore)
	if !ok || cache.Backing() == nil {
		t.Fatalf("city %s: rig store under the guard is %T, want the cache", c.label, inner)
	}
	if backing, _, _ := unwrapBeadPolicyStore(cache.Backing()); backing != beads.Store(f.engine) {
		t.Fatalf("city %s: the cache is over %T, not the shared engine", c.label, cache.Backing())
	}
	f.mu.Lock()
	n := f.opens[c.label+"|"+c.rig]
	f.mu.Unlock()
	if n == 0 {
		t.Fatalf("city %s never opened its rig through the controller seam", c.label)
	}
	f.syncCache(t, c, true)
}

// cache returns the city's unfiltered rig cache.
func (c *tcCity) cache(t *testing.T) *beads.CachingStore {
	t.Helper()
	base, _, _ := unwrapBeadPolicyStore(splitStrictFrontOf(c.rigStore(t)))
	inner, _, _ := unwrapRigNamespaceStore(base)
	cache, ok := inner.(*beads.CachingStore)
	if !ok {
		t.Fatalf("city %s: no cache under the rig store (%T)", c.label, inner)
	}
	return cache
}

// syncCache brings the city's cache up to the engine and checks it holds
// every engine row. prime re-primes the cache first (a prime absorbs rows
// silently); without it the cache's reconcile absorbs them and notifies, as
// it does for another process's writes.
func (f *tcFixture) syncCache(t *testing.T, c *tcCity, prime bool) {
	t.Helper()
	cache := c.cache(t)
	if prime {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("city %s: prime: %v", c.label, err)
		}
	}
	cache.ReconcileNowForTest()
	raw, err := f.engine.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := cache.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tcIDs(cached), tcIDs(raw); !slices.Equal(got, want) {
		t.Fatalf("city %s: the cache holds %v, the engine %v; a stale cache would let a filter pass vacuously", c.label, got, want)
	}
}

// ---------------------------------------------------------------------------
// Rows.

type tcRow struct {
	id    string
	what  string
	owner tcOwner
	bead  beads.Bead
}

// tcRowsFor returns every row of the fixture. Rows owned by one city are
// mirrored for the other, so each city's matrix has both an own row and the
// other city's twin in every shape.
func tcRowsFor(f *tcFixture) []tcRow {
	md := rnsMD
	var rows []tcRow
	add := func(id, what string, owner tcOwner, status, assignee string, meta map[string]string) {
		if status == "" {
			status = "open"
		}
		rows = append(rows, tcRow{id: id, what: what, owner: owner, bead: beads.Bead{
			ID: id, Title: what, Type: "task", Status: status, Assignee: assignee, Metadata: meta,
		}})
	}
	// Owned by neither city, in every colliding spelling (r1, r4, r5, r7,
	// r10, r12, and a person's claim on a routed row).
	add("sh-r1", "r1 routed to the bare city pool", tcNeither, "", "", md(beadmeta.RoutedToMetadataKey, tcCityPool))
	add("sh-r4", "r4 open, assigned to the shared pool alias", tcNeither, "", tcCityAlias, nil)
	add("sh-r5", "r5 in progress under the shared pool alias", tcNeither, "in_progress", tcCityAlias, nil)
	add("sh-r7", "r7 in progress under a person", tcNeither, "in_progress", tcPerson, nil)
	add("sh-r10", "r10 in progress under the colliding short session id", tcNeither, "in_progress", tcShortID, nil)
	add("sh-r12a", "r12 routed to the case twin of alpha", tcNeither, "", "", md(beadmeta.RoutedToMetadataKey, "Alpha/pk.worker"))
	add("sh-r12b", "r12 routed to the case twin of beta", tcNeither, "", "", md(beadmeta.RoutedToMetadataKey, "Beta/pk.worker"))
	add("sh-named", "in progress under the shared named agent", tcNeither, "in_progress", tcNamed, nil)
	add("sh-sweep-alias", "pool claim: routed to the bare city pool, held by the shared alias", tcNeither, "in_progress", tcCityAlias, md(beadmeta.RoutedToMetadataKey, tcCityPool))
	add("sh-sweep-short", "pool claim: routed to the bare city pool, held by the short id", tcNeither, "in_progress", tcShortID, md(beadmeta.RoutedToMetadataKey, tcCityPool))
	// Unattributed: no owner key, no claim stamp, no creation stamp (r8, and
	// r9 per city: gc.run_target is not an owner key).
	add("sh-r8", "r8 unrouted, run target the bare city pool", tcUnattributed, "", "", md(beadmeta.RunTargetMetadataKey, tcCityPool))
	add("sh-plain", "unrouted row from a writer other than gc", tcUnattributed, "", "", nil)
	for _, c := range f.cities() {
		s := strings.ToLower(c.label)
		other := f.a
		if c == f.a {
			other = f.b
		}
		add("sh-r9"+s, "r9 unrouted, run target "+c.worker, tcUnattributed, "", "", md(beadmeta.RunTargetMetadataKey, c.worker))
		add("sh-r23"+s, "r2/r3 routed to "+c.worker, c.owner, "", "", md(beadmeta.RoutedToMetadataKey, c.worker))
		add("sh-r6"+s, "r6 in progress under "+c.workerN, c.owner, "in_progress", c.workerN, nil)
		add("sh-r11"+s, "r11 routed to the session spelling of "+c.worker, c.owner, "", "", md(beadmeta.RoutedToMetadataKey, c.rig+"--pk__worker"))
		add("sh-pool"+s, "in progress under city "+c.label+"'s live pool session", c.owner, "in_progress", c.pool, nil)
		add("sh-mint"+s, "graph-first mint of city "+c.label, c.owner, "", "", md(beadmeta.DeferredRoutedToMetadataKey, c.worker, beadmeta.RunTargetMetadataKey, "pk.worker"))
		add("sh-orphan"+s, "detached orphan of city "+c.label+"'s pool session", c.owner, "", "", md(beadmeta.SessionIDMetadataKey, c.pool))
		add("sh-sname"+s, "detached orphan stamped with city "+c.label+"'s pool session name", c.owner, "", "", md(beadmeta.SessionNameMetadataKey, "pk.worker-"+c.pool))
		add("sh-death"+s, "on_death row: in progress under "+c.worker, c.owner, "in_progress", c.worker, nil)
		add("sh-boot"+s, "on_boot row: in progress, unassigned, routed to "+c.worker, c.owner, "in_progress", "", md(beadmeta.RoutedToMetadataKey, c.worker))
		add("sh-stamp"+s, "order-tracking row city "+c.label+" created through gc", c.owner, "", "", md(rigNamespaceOwnerStampKey, c.rig+"/"))
		add("sh-sweep-dead"+s, "pool claim of city "+c.label+"'s closed pool session", c.owner, "in_progress", c.dead, md(beadmeta.RoutedToMetadataKey, c.worker))
		add("sh-person"+s, "a person's claim on a row routed to "+c.worker, tcNeither, "in_progress", tcPerson, md(beadmeta.RoutedToMetadataKey, c.worker))
		add("sh-other"+s, "city "+c.label+"'s pool id held on a row routed to "+other.worker, tcNeither, "in_progress", c.pool, md(beadmeta.RoutedToMetadataKey, other.worker))
	}
	return rows
}

func tcRowBeads(f *tcFixture) []beads.Bead {
	var out []beads.Bead
	for _, r := range tcRowsFor(f) {
		out = append(out, r.bead)
	}
	return out
}

func (f *tcFixture) rows() []tcRow { return tcRowsFor(f) }

func (f *tcFixture) row(id string) tcRow {
	for _, r := range f.rows() {
		if r.id == id {
			return r
		}
	}
	panic("no row " + id)
}

func tcIDs(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// select returns the ids of the rows keep admits, sorted.
func (f *tcFixture) selectIDs(keep func(tcRow) bool) []string {
	var out []string
	for _, r := range f.rows() {
		if keep(r) {
			out = append(out, r.id)
		}
	}
	sort.Strings(out)
	return out
}

// mayActOn reports whether city c may act on r under the fixture's mode: in
// rig mode only its own rows and Unattributed ones; in city mode every row.
func (f *tcFixture) mayActOn(c *tcCity, r tcRow) bool {
	if f.mode == config.RigFederationCity {
		return true
	}
	return r.owner == c.owner || r.owner == tcUnattributed
}

// tcColumn is one guarded path, run for one city: the ids it acted on (or
// returned), and the ids it should have.
type tcColumn struct {
	name string
	run  func(t *testing.T, f *tcFixture, c *tcCity) []string
	want func(f *tcFixture, c *tcCity) []string
	// active is the subset of rows the column could act on at all (its own
	// query or input); the vacuity guard requires it to hold a row the city
	// must not act on and, in rig mode, one it must.
	active func(f *tcFixture, c *tcCity) []string
	// leaks marks a column whose federation = "city" run must act on a row
	// that is not the acting city's own in at least one city.
	leaks bool
}

func tcCheckColumn(t *testing.T, f *tcFixture, col tcColumn) {
	t.Helper()
	leaks := 0
	for _, c := range f.cities() {
		got := col.run(t, f, c)
		sort.Strings(got)
		got = slices.Compact(got)
		want := col.want(f, c)
		if !slices.Equal(got, want) {
			t.Errorf("%s, city %s (federation = %q):\n got  %v\n want %v\n extra %v\n missing %v",
				col.name, c.label, f.mode, got, want, tcMinus(got, want), tcMinus(want, got))
		}
		if len(want) == 0 {
			t.Errorf("%s, city %s: the column expects nothing; it needs a positive control", col.name, c.label)
		}
		if col.active != nil && f.mode == config.RigFederationRig && len(tcMinus(col.active(f, c), want)) == 0 {
			t.Errorf("%s, city %s: every row the column reaches is one the city may act on; it would pass vacuously", col.name, c.label)
		}
		for _, id := range want {
			if f.row(id).owner != c.owner {
				leaks++
				break
			}
		}
	}
	// With federation = "city" the column must pin a leak in at least one
	// city: today's behavior acting on a row that is not the city's own.
	if f.mode == config.RigFederationCity && col.leaks && leaks == 0 {
		t.Errorf("%s: today's behavior acts only on each city's own rows here; there is no leak to pin", col.name)
	}
}

func tcMinus(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func tcAll(f *tcFixture, _ *tcCity) []string {
	return f.selectIDs(func(tcRow) bool { return true })
}

func tcNotClosed(r tcRow) bool { return r.bead.Status != "closed" }

// cliResolver is the city's one-shot resolver over its own store, as gc hook,
// gc bd and gc agent is-foreign build it.
func (c *tcCity) cliResolver() *citySessionResolver {
	return newCityCLISessionResolver(c.path, c.cfg, func() (beads.SessionStore, error) { return cliStores(c.path, c.hq).Sessions(), nil })
}

func (c *tcCity) hookGuard() *hookRigGuard {
	return newHookRigGuard(c.path, c.cfg, c.cliResolver())
}

// tcSnapshot is every shared row's ownership-relevant state.
func (f *tcFixture) snapshot(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.engine.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(rows))
	for _, b := range rows {
		md, _ := json.Marshal(b.Metadata)
		out[b.ID] = fmt.Sprintf("status=%s assignee=%q title=%q md=%s", b.Status, b.Assignee, b.Title, md)
	}
	return out
}

// tcRuntime is a city runtime over the fixture city's controller state, with
// the production desired-state builder.
func (f *tcFixture) runtime(t *testing.T, c *tcCity) (*CityRuntime, *runtime.Fake) {
	t.Helper()
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:                c.path,
		CityName:                c.name,
		TomlPath:                filepath.Join(c.path, "city.toml"),
		Cfg:                     c.cfg,
		SP:                      sp,
		BuildFn:                 supervisorBuildAgentsFn(c.path, c.name, io.Discard),
		BuildFnWithSessionBeads: supervisorBuildAgentsFnWithSessionBeads(c.path, c.name, io.Discard),
		Dops:                    newDrainOps(sp),
		Rec:                     c.ep,
		Stdout:                  io.Discard,
		Stderr:                  io.Discard,
	})
	cr.setControllerState(c.cs)
	return cr, sp
}

// tcSessionTemplates returns the templates of the city's session records
// created since before (by id).
func tcSessionTemplates(t *testing.T, c *tcCity, before map[string]bool) []string {
	t.Helper()
	rows, err := c.hq.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Label: "gc:session"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range rows {
		if !before[b.ID] {
			out = append(out, b.Metadata["template"])
		}
	}
	sort.Strings(out)
	return out
}
