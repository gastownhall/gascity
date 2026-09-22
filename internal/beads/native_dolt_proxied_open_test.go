package beads

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint/proxyendpointtest"
)

// proxiedUnlockKeys are the five variables that must be UNSET inside the
// proxied window: four bd unlocks that bypass the very migration and
// schema-skew gate admission runs, and the library's own test-mode relaxation.
var proxiedUnlockKeys = []string{
	"BD_ALLOW_REMOTE_MIGRATE",
	"BD_IGNORE_SCHEMA_SKEW",
	"BD_SMART_GATE",
	"BD_EVENTS_JOURNAL",
	"BEADS_TEST_MODE",
}

// TestProxiedHermeticOpenScrubsUnlocksAndProjectsAuthorPair is the safety
// property of the proxied window, in one test.
//
// gc opens the linked library against a database bd owns and administers. Two
// things must be true for that to be safe, and both are about the environment
// rather than about any code in the open path:
//
//   - no ambient variable may unlock a migration. bd reads
//     BD_ALLOW_REMOTE_MIGRATE and BD_IGNORE_SCHEMA_SKEW to bypass exactly the
//     gate gc runs before admitting a proxied database, and an operator's shell
//     that exported one for an unrelated bd command must not be able to make gc
//     migrate somebody else's shared database;
//   - the author pair must be gc's, not the shell's. beads' applyConfigDefaults
//     reads GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL DURING THE OPEN and latches them
//     onto the store instance, which commitAuthorString later feeds to
//     DOLT_COMMIT --author. The author is pinned at open and never re-read, so
//     a window that let the ambient pair through would attribute a gc write to
//     whoever last ran git in that terminal.
//
// And the window must leave no trace: the process environment is shared, so a
// key this open failed to restore is a key every later command in the process
// reads wrong.
func TestProxiedHermeticOpenScrubsUnlocksAndProjectsAuthorPair(t *testing.T) {
	for _, key := range proxiedUnlockKeys {
		t.Setenv(key, "ambient-"+key)
	}
	t.Setenv("GIT_AUTHOR_NAME", "op")
	t.Setenv("GIT_AUTHOR_EMAIL", "op@laptop")
	// An ambient endpoint pointing somewhere else entirely: the window must
	// project the pin's port, not this one.
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_FUTURE_AUTHORITY", "ambient-authority")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen map[string]string
	nativeDoltOpenBestAvailable = func(ctx context.Context, _ string) (beadslib.Storage, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("proxied native open context has no deadline")
		}
		seen = map[string]string{}
		for _, key := range append(append([]string(nil), proxiedUnlockKeys...),
			"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "BEADS_DOLT_SERVER_PORT", "BEADS_FUTURE_AUTHORITY") {
			seen[key] = os.Getenv(key)
		}
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	scopeRoot := filepath.Join(t.TempDir(), "scope")
	store, err := OpenNativeDoltStoreAtProxied(context.Background(), scopeRoot, map[string]string{
		"BEADS_DOLT_SERVER_PORT": "44561",
		"GIT_AUTHOR_NAME":        "gc",
		"GIT_AUTHOR_EMAIL":       "gc@demo-city",
	})
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtProxied: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if seen == nil {
		t.Fatal("the open seam never ran")
	}
	for _, key := range proxiedUnlockKeys {
		if got := seen[key]; got != "" {
			t.Errorf("%s during the proxied open = %q, want unset: an ambient unlock must not reach a database bd owns", key, got)
		}
	}
	// The whole BEADS_ namespace is withheld, not just the keys gc names --
	// the library reads more variables than gc maintains a mirror of.
	if got := seen["BEADS_FUTURE_AUTHORITY"]; got != "" {
		t.Errorf("BEADS_FUTURE_AUTHORITY during the proxied open = %q, want the namespace withheld", got)
	}
	if got := seen["GIT_AUTHOR_NAME"]; got != "gc" {
		t.Errorf("GIT_AUTHOR_NAME during the proxied open = %q, want gc: beads latches the author at open, not at commit", got)
	}
	if got := seen["GIT_AUTHOR_EMAIL"]; got != "gc@demo-city" {
		t.Errorf("GIT_AUTHOR_EMAIL during the proxied open = %q, want gc@demo-city", got)
	}
	if got := seen["BEADS_DOLT_SERVER_PORT"]; got != "44561" {
		t.Errorf("BEADS_DOLT_SERVER_PORT during the proxied open = %q, want the pinned 44561", got)
	}
	if store.IDPrefix() != "gc" {
		t.Errorf("IDPrefix = %q, want gc read through the window", store.IDPrefix())
	}

	// Nothing survives the window.
	restored := map[string]string{
		"GIT_AUTHOR_NAME":        "op",
		"GIT_AUTHOR_EMAIL":       "op@laptop",
		"BEADS_DOLT_SERVER_PORT": "9999",
		"BEADS_FUTURE_AUTHORITY": "ambient-authority",
	}
	for _, key := range proxiedUnlockKeys {
		restored[key] = "ambient-" + key
	}
	for key, want := range restored {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s after the proxied open = %q, want the ambient %q restored", key, got, want)
		}
	}
}

// TestDirectNativeOpenEnvKeyListUnchanged is the flag-off fence.
//
// "Flag off is byte-identical" cannot mean only that the new arm is
// unreachable, because the projection is shared state: a key listed in an
// open-env key list whose value the caller's map omits is actively UNSET for
// the window. So growing nativeDoltOpenEnvKeys to reach the author pair would
// strip an operator's git identity from every DIRECT native open in the
// process, and adding BEADS_TEST_MODE would unset it under the library's own
// suites -- with the whole test suite green, because nothing asserts on a
// variable nobody projected.
//
// Hence the golden: the direct list is exactly these fourteen BEADS_ keys, and
// every proxied-only addition lives in the separate list.
func TestDirectNativeOpenEnvKeyListUnchanged(t *testing.T) {
	want := []string{
		"BEADS_CREDENTIALS_FILE",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_DATA_DIR",
		"BEADS_DOLT_MAX_CONNS",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_SERVER_DATABASE",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_MODE",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_SOCKET",
		"BEADS_DOLT_SERVER_TLS",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_SHARED_SERVER",
	}
	if !slices.Equal(nativeDoltOpenEnvKeys, want) {
		t.Fatalf("nativeDoltOpenEnvKeys = %v,\nwant the pinned direct list %v.\n"+
			"A proxied-only key belongs in proxiedOnlyOpenEnvKeys: adding it here changes what a FLAG-OFF direct open projects, and an omitted-but-listed key is unset.",
			nativeDoltOpenEnvKeys, want)
	}
	for _, key := range nativeDoltOpenEnvKeys {
		if len(key) < len(beadsEnvPrefix) || key[:len(beadsEnvPrefix)] != beadsEnvPrefix {
			t.Errorf("direct open key %q is outside the BEADS_ namespace the direct window withholds", key)
		}
	}

	// The proxied list is a superset and adds exactly the seven documented keys.
	for _, key := range nativeDoltOpenEnvKeys {
		if !slices.Contains(proxiedNativeOpenEnvKeys, key) {
			t.Errorf("proxiedNativeOpenEnvKeys drops direct key %q", key)
		}
	}
	for _, key := range proxiedOnlyOpenEnvKeys {
		if slices.Contains(nativeDoltOpenEnvKeys, key) {
			t.Errorf("%q is in BOTH lists; the proxied-only list must not leak into the direct one", key)
		}
		if !slices.Contains(proxiedNativeOpenEnvKeys, key) {
			t.Errorf("proxiedNativeOpenEnvKeys is missing proxied-only key %q", key)
		}
	}
	if len(proxiedNativeOpenEnvKeys) != len(nativeDoltOpenEnvKeys)+len(proxiedOnlyOpenEnvKeys) {
		t.Errorf("proxiedNativeOpenEnvKeys has %d keys, want %d + %d",
			len(proxiedNativeOpenEnvKeys), len(nativeDoltOpenEnvKeys), len(proxiedOnlyOpenEnvKeys))
	}
	for _, key := range proxiedUnlockKeys {
		if !slices.Contains(proxiedNativeOpenEnvKeys, key) {
			t.Errorf("unlock key %q is not in the proxied list, so the window does not state that it unsets it", key)
		}
	}
}

// TestProcessEnvSnapshotSeesAmbientDuringProxiedWindow extends the existing
// snapshot fence to the proxied window, in both directions.
//
// The snapshot exists so that a bd child's environment is built from the true
// ambient process env rather than from another scope's transient projection --
// gc opens scopes concurrently, and a bd child launched with a peer scope's
// port would talk to the wrong database. The proxied window withholds MORE than
// any previous window (the whole BD_ namespace as well as BEADS_, plus the
// author pair), so it is exactly the window in which an unguarded reader would
// see the most wrong.
//
// Both directions matter: the snapshot must BLOCK while the window is open (a
// snapshot taken mid-window would report gc's identity as the operator's and
// bd's unlocks as absent), and it must see the ambient values once it is
// restored.
func TestProcessEnvSnapshotSeesAmbientDuringProxiedWindow(t *testing.T) {
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "1")
	t.Setenv("BEADS_FUTURE_AUTHORITY", "ambient-authority")
	t.Setenv("GIT_AUTHOR_NAME", "op")

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	inWindow := make(chan struct{})
	release := make(chan struct{})
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		close(inWindow)
		<-release
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	openDone := make(chan error, 1)
	go func() {
		store, err := OpenNativeDoltStoreAtProxied(context.Background(), t.TempDir(), map[string]string{
			"GIT_AUTHOR_NAME": "gc",
		})
		if store != nil {
			_ = store.CloseStore()
		}
		openDone <- err
	}()
	<-inWindow

	envCh := make(chan []string, 1)
	go func() { envCh <- ProcessEnvSnapshotExcludingNativeDoltOpen() }()
	select {
	case env := <-envCh:
		t.Fatalf("the snapshot completed inside the proxied window: BD_ALLOW_REMOTE_MIGRATE=%v GIT_AUTHOR_NAME=%v",
			envValues(env, "BD_ALLOW_REMOTE_MIGRATE"), envValues(env, "GIT_AUTHOR_NAME"))
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-openDone; err != nil {
		t.Fatalf("OpenNativeDoltStoreAtProxied: %v", err)
	}

	select {
	case env := <-envCh:
		for key, want := range map[string]string{
			"BD_ALLOW_REMOTE_MIGRATE": "1",
			"BEADS_FUTURE_AUTHORITY":  "ambient-authority",
			"GIT_AUTHOR_NAME":         "op",
		} {
			got := envValues(env, key)
			if len(got) != 1 || got[0] != want {
				t.Errorf("%s in the post-window snapshot = %v, want [%s]", key, got, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("the snapshot did not complete after the proxied window restored")
	}
}

// TestWithNativeReadRetryBudgetIsAProductionOption pins that the proxied lane
// can shorten a read's whole reconnect-and-retry chain without reaching into
// the struct, and that a nonsense value leaves the default rather than
// producing a store whose every read fails instantly.
func TestWithNativeReadRetryBudgetIsAProductionOption(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{}, WithNativeReadRetryBudget(10*time.Second))
	if store.readRetryBudgetOverride != 10*time.Second {
		t.Errorf("read retry budget = %v, want 10s", store.readRetryBudgetOverride)
	}
	if store.readRetryBudgetOverride >= nativeReadRetryBudget {
		t.Errorf("the proxied budget %v does not shorten the %v default; gc cannot restart a bd-owned proxy, so it must not wait out one",
			store.readRetryBudgetOverride, nativeReadRetryBudget)
	}
	for _, bad := range []time.Duration{0, -time.Second} {
		store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{}, WithNativeReadRetryBudget(bad))
		if store.readRetryBudgetOverride != 0 {
			t.Errorf("WithNativeReadRetryBudget(%v) set the override to %v, want the default left in place",
				bad, store.readRetryBudgetOverride)
		}
	}
}

// TestProxiedOpenIsReadOnlyWithoutBeingAsked is council B-F5.
//
// OpenNativeDoltStoreAtProxied sets proxiedReadVerdicts structurally, with a
// comment explaining that a caller must not be able to forget it — and two
// lines later left readOnlyReason, the OTHER half of "no native write reaches a
// bd-owned database", to an option exactly one call site passed. A second
// proxied open site (PR3's write arm, a new rig path, an acceptance seam) that
// omitted it would get a fully writable native handle against a database bd
// owns, with no compile error, no runtime signal, and a green
// TestNativeDoltStoreReadOnlyLatchRefusesEveryMutation — which builds its own
// latched store.
//
// This opens with NO options at all, which is the shape of the caller the
// finding is about.
func TestProxiedOpenIsReadOnlyWithoutBeingAsked(t *testing.T) {
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAtProxied(context.Background(), filepath.Join(t.TempDir(), "scope"), nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtProxied: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	if !store.ReadOnly() {
		t.Fatal("a proxied native handle opened with no options is WRITABLE against a database bd " +
			"owns; the read-only fence must not be something a second call site can forget")
	}
	if _, err := store.Create(Bead{Title: "a write that must not land", Type: "task"}); err == nil {
		t.Fatal("Create succeeded on a proxied native handle")
	}
	// The verdict fence beside it, so the two halves are asserted together and
	// neither can drift back to opt-in alone.
	if !store.proxiedReadVerdicts {
		t.Fatal("a proxied native handle does not classify its read failures as verdicts")
	}
}

// poolStorage is a library handle whose only real surface is the pool it
// exports through UnderlyingDB — the accessor server-mode *dolt.DoltStore
// carries, and the only thing the post-open observation touches.
type poolStorage struct {
	beadslib.Storage
	db *sql.DB
}

func (s *poolStorage) UnderlyingDB() *sql.DB { return s.db }

// TestProxiedOpenedHeadReadsOverTheLibrarysOwnPool is council pr2 D-F3's
// re-read half: the post-open HEAD comes from the handle the open just built,
// and not from a session of gc's own, which would be one more accepted
// connection on bd's proxy per open.
//
// And council pr2 E-S3: the pool models beads' be-itm5 — a connection answers
// its first statement from the pre-open root — so the observation must be the
// SECOND statement on one pinned connection. A reader that trusted the first
// answer reports "before0000" here, which is the hash the probe saw, and an
// open that committed would pass the check.
func TestProxiedOpenedHeadReadsOverTheLibrarysOwnPool(t *testing.T) {
	pool := proxyendpointtest.NewPostOpenDB(
		proxyendpointtest.State{Head: "before0000"},
		proxyendpointtest.State{Head: " after11111\n"})
	t.Cleanup(func() { _ = pool.DB.Close() })

	head, err := ProxiedOpenedHead(context.Background(), &poolStorage{db: pool.DB})
	if err != nil {
		t.Fatalf("ProxiedOpenedHead: %v", err)
	}
	if head != "after11111" {
		t.Fatalf("head = %q, want the state AFTER the open, trimmed: the first statement on a connection "+
			"the open's checks ran on reads the pre-open root (be-itm5)", head)
	}
	if conns, statements := pool.Conns(), len(pool.Statements()); conns != 1 || statements != 2 {
		t.Fatalf("the re-read used %d connection(s) and %d statement(s), want 1 and 2: "+
			"the advancing statement and the answer must share a connection", conns, statements)
	}

	// The same question through the wrapped leaf reaches the same pool.
	leafPool := proxyendpointtest.NewPostOpenDB(
		proxyendpointtest.State{Head: "before0000"}, proxyendpointtest.State{Head: "after11111"})
	t.Cleanup(func() { _ = leafPool.DB.Close() })
	leaf := newNativeDoltStoreForTest(&poolStorage{db: leafPool.DB})
	if head, err := ProxiedLeafHead(context.Background(), leaf); err != nil || head != "after11111" {
		t.Fatalf("ProxiedLeafHead = (%q, %v), want the leaf's own pool's post-open answer", head, err)
	}

	// A handle that cannot answer is an error, never "": the caller must be
	// able to tell "not observed" from a value.
	if _, err := ProxiedOpenedHead(context.Background(), &nativeDoltStorageSpy{}); err == nil {
		t.Fatal("a handle with no UnderlyingDB produced a HEAD")
	}
	failing := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{}, proxyendpointtest.State{}).
		Failing(errors.New("invalid connection"))
	t.Cleanup(func() { _ = failing.DB.Close() })
	if _, err := ProxiedOpenedHead(context.Background(), &poolStorage{db: failing.DB}); err == nil {
		t.Fatal("a failed re-read produced a HEAD")
	}
	empty := proxyendpointtest.NewPostOpenDB(proxyendpointtest.State{Head: "x"}, proxyendpointtest.State{Head: ""})
	t.Cleanup(func() { _ = empty.DB.Close() })
	if _, err := ProxiedOpenedHead(context.Background(), &poolStorage{db: empty.DB}); err == nil {
		t.Fatal("an empty hash was reported as an observation instead of a failure to observe")
	}
	// A closed leaf is refused before anything is asked.
	if _, err := ProxiedLeafHead(context.Background(), nil); err == nil {
		t.Fatal("a nil leaf produced a HEAD")
	}
}
