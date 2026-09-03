//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

// setupDiscoveryLockCity builds a city that imports a bundled pack under a
// private, cold GC_HOME, and returns the city path plus its repo-cache root.
// Resolving that import hydrates the pack's synthetic repo cache under the
// repo-cache write lock, which is what puts an ordinary config load in
// contention with a concurrent `gc import install`.
//
// The cache is deliberately left cold: a hydrated cache short-circuits ahead
// of the lock (ValidateSyntheticRepoFast), so warming the fixture would make
// every assertion below vacuous.
func setupDiscoveryLockCity(t *testing.T) (cityPath, cacheRoot string) {
	t.Helper()
	source, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal("no bundled source for the core pack")
	}
	config.ResetRemoteCacheValidationCache()
	t.Cleanup(config.ResetRemoteCacheValidationCache)

	base := t.TempDir()
	// GC_HOME deliberately sits outside HOME: supervisor.Registry panics on any
	// test write under $HOME/.gc, and this fixture registers a city.
	gcHome := filepath.Join(base, "gchome")
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("GC_HOME", gcHome)

	cityPath = filepath.Join(base, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := fmt.Sprintf("[workspace]\nname = \"discovery-lock\"\n\n[imports.core]\nsource = %q\n", source)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	// Test binaries refuse the ambient upward city walk, so name the city
	// explicitly instead of relying on cwd.
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")
	// These are package globals, not env: a value left behind by another test
	// would divert resolution before it ever reaches the lock, and no assertion
	// below would notice.
	restoreFlag(t, &cityFlag)
	restoreFlag(t, &rigFlag)

	cacheRoot = filepath.Join(gcHome, "cache", "repos")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return cityPath, cacheRoot
}

func restoreFlag(t *testing.T, flag *string) {
	t.Helper()
	prev := *flag
	*flag = ""
	t.Cleanup(func() { *flag = prev })
}

// holdExclusiveRepoCacheLock takes the repo-cache lock the way a `gc import
// install` holds it for the whole of its network clone. The returned release
// is idempotent; tests that leave a goroutine blocked on the lock must call it
// and then join, since a goroutine woken during t.TempDir's cleanup races the
// directory removal.
func holdExclusiveRepoCacheLock(t *testing.T, cacheRoot string) (release func()) {
	t.Helper()
	lockFile, err := os.OpenFile(filepath.Join(cacheRoot, ".packman-cache.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open repo cache lock: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		lockFile.Close() //nolint:errcheck
		t.Fatalf("flock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
			lockFile.Close()                                   //nolint:errcheck
		})
	}
	t.Cleanup(release)
	return release
}

// The control, and the reason this file exists: a blocking city load really
// does queue behind a held repo-cache lock. Eager pack-command discovery runs
// on every gc invocation, so before ga-r0epd one `gc import install` clone
// stalled every other gc command on the machine for its whole duration.
//
// It loads through loadCityConfigWithoutBuiltinPackRefresh, the blocking twin
// of loadCityConfigAdvisory: the two differ in exactly the one bit under test.
// A loader that refreshes builtin packs would instead block in the
// always-required builtin-ensure step, which every city reaches whatever its
// city.toml declares — so it would stay green even if the fixture stopped
// importing anything, and the assertions below would go quietly vacuous.
func TestBlockingCityLoadWaitsOnHeldRepoCacheLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	release := holdExclusiveRepoCacheLock(t, cacheRoot)

	done := make(chan struct{})
	go func() {
		defer close(done)
		loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard) //nolint:errcheck // only the timing matters
	}()

	select {
	case <-done:
		t.Fatal("blocking city load returned while the repo-cache lock was held; " +
			"the fixture no longer reaches the lock, so the advisory assertions in this file prove nothing")
	case <-time.After(500 * time.Millisecond):
	}
	// Join before the tempdir cleanup: the woken load writes into the city.
	release()
	<-done
}

func TestRegisterPackCommandsDoesNotWaitOnHeldRepoCacheLock(t *testing.T) {
	_, cacheRoot := setupDiscoveryLockCity(t)
	holdExclusiveRepoCacheLock(t, cacheRoot)

	root := &cobra.Command{Use: "gc"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		registerPackCommands(root, nil, io.Discard, io.Discard)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("eager pack-command discovery blocked on a held repo-cache lock")
	}
}

// Completion runs on a <TAB> keystroke, so a blocking load there stalls the
// user's shell rather than a command they submitted.
func TestCompletionCandidatesDoNotWaitOnHeldRepoCacheLock(t *testing.T) {
	_, cacheRoot := setupDiscoveryLockCity(t)
	holdExclusiveRepoCacheLock(t, cacheRoot)

	// loadSessionsForCompletion is deliberately absent: it opens the city store
	// first, and the store open loads config blockingly and readies builtin
	// packs, which is a second way into the same lock that this change does not
	// address. Tracked separately rather than left looking covered.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rigNameCandidates("")
		loadOrdersForCompletion()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("completion candidates blocked on a held repo-cache lock")
	}
}

// `gc <cmd> --rig <TAB>` resolves the rig before it can list anything, which
// reaches the registry scan rather than the city load the other completion
// paths take.
//
// It asserts the busy error rather than only the timing. Returning promptly is
// half the contract; the other half is refusing to answer, and a resolution
// that swallowed the busy error would return promptly too — from whichever
// city it could still reach.
func TestCompletionRigFlagResolutionDoesNotWaitOnHeldRepoCacheLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	registerRigInSiteBinding(t, cityPath, "hauler")
	rigFlag = "hauler"
	holdExclusiveRepoCacheLock(t, cacheRoot)

	assertFailsClosedPromptly(t, "completion rig-flag resolution", resolveCityForCompletion)
}

// Running gc from inside a rig directory is the ordinary way to use it, and it
// resolves through the registered-binding lookup rather than GC_CITY. That
// step carries its own resolution mode, so it needs its own guard.
func TestCwdRigDiscoveryDoesNotWaitOnHeldRepoCacheLock(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	rigDir := registerRigInSiteBinding(t, cityPath, "hauler")
	t.Setenv("GC_CITY", "")
	t.Chdir(rigDir)
	holdExclusiveRepoCacheLock(t, cacheRoot)

	assertFailsClosedPromptly(t, "cwd rig discovery", resolveCityForDiscovery)
}

// assertFailsClosedPromptly runs resolve off the test goroutine — the point is
// that it might block — and requires it to both return quickly and report the
// busy cache rather than an answer it reached by skipping the busy city.
func assertFailsClosedPromptly(t *testing.T, what string, resolve func() (string, error)) {
	t.Helper()
	type result struct {
		city string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		city, err := resolve()
		done <- result{city, err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, config.ErrRepoCacheBusy) {
			t.Fatalf("%s: err = %v, want config.ErrRepoCacheBusy (city %q)", what, got.err, got.city)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a held repo-cache lock", what)
	}
}

// Refusing to wait is only half of it. An advisory scan must also refuse to
// answer from whatever it could reach without waiting: silently skipping the
// busy city would let resolution settle on a different one, and eager
// discovery captures the city it picks into the pack-command closures the user
// later runs.
func TestAdvisoryRigLookupFailsClosedOnBusyRepoCache(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	registerRigInSiteBinding(t, cityPath, "hauler")
	holdExclusiveRepoCacheLock(t, cacheRoot)

	type result struct {
		matches []registeredRigBinding
		err     error
	}
	done := make(chan result, 1)
	go func() {
		matches, _, err := registeredRigBindingsByName("hauler", false, completionResolutionMode)
		done <- result{matches, err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, config.ErrRepoCacheBusy) {
			t.Fatalf("advisory rig lookup err = %v, want config.ErrRepoCacheBusy", got.err)
		}
		if len(got.matches) != 0 {
			t.Fatalf("advisory rig lookup returned %d matches alongside a busy cache, want none", len(got.matches))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("advisory rig lookup blocked on a held repo-cache lock")
	}
}

// An advisory scan reads .gc/site.toml to decide which cities are worth
// loading, but site.toml records only where a rig lives — not whether the city
// still declares it. Answering from site.toml alone would resurrect a rig that
// an authoritative resolution prunes, and hand eager discovery a city the user
// is not in.
func TestAdvisoryRigLookupPrunesSiteOnlyBindingsLikeAuthoritative(t *testing.T) {
	cityPath, _ := setupDiscoveryLockCity(t)
	registerRigInSiteBinding(t, cityPath, "hauler")
	registerRigInSiteBinding(t, cityPath, "ghost")
	declareRigInCityTOML(t, cityPath, "hauler")

	for _, tc := range []struct {
		rig  string
		want int
	}{
		{rig: "hauler", want: 1}, // declared and bound: the control
		{rig: "ghost", want: 0},  // bound but no longer declared
	} {
		for _, mode := range []struct {
			name string
			mode contextResolutionMode
		}{
			{"advisory", completionResolutionMode},
			{"authoritative", authoritativeResolution},
		} {
			t.Run(tc.rig+"/"+mode.name, func(t *testing.T) {
				matches, _, err := registeredRigBindingsByName(tc.rig, false, mode.mode)
				if err != nil {
					t.Fatalf("rig lookup: %v", err)
				}
				if len(matches) != tc.want {
					t.Fatalf("rig %q resolved to %d bindings, want %d", tc.rig, len(matches), tc.want)
				}
			})
		}
	}
}

// The sticky default is the tier below local discovery, so a local discovery
// that could not look is the one case where falling through changes the answer
// rather than supplying a missing one: the same cwd resolves to a remote city
// while another terminal happens to be cloning, and to the local one a moment
// later.
func TestBusyRepoCacheDoesNotFallThroughToTheStickyDefault(t *testing.T) {
	cityPath, cacheRoot := setupDiscoveryLockCity(t)
	rigDir := registerRigInSiteBinding(t, cityPath, "hauler")
	declareRigInCityTOML(t, cityPath, "hauler")
	t.Setenv("GC_CITY", "")
	t.Chdir(rigDir)
	writeStickyDefaultContext(t)

	// The control: without contention this resolves locally, so the assertion
	// below is about the busy cache and not about an unreachable sticky tier.
	if ctx, err := resolveContextAllowRemoteMode(authoritativeResolution); err != nil || ctx.Remote != nil {
		t.Fatalf("uncontended resolve = %+v, err %v; want the local city", ctx, err)
	}

	release := holdExclusiveRepoCacheLock(t, cacheRoot)
	defer release()

	ctx, err := resolveContextAllowRemoteMode(completionResolutionMode)
	if !errors.Is(err, config.ErrRepoCacheBusy) {
		t.Fatalf("err = %v, want config.ErrRepoCacheBusy (resolved %+v)", err, ctx)
	}
	if ctx.Remote != nil {
		t.Fatalf("busy local discovery fell through to remote %+v", ctx.Remote)
	}
}

// writeStickyDefaultContext registers a remote context and makes it the sticky
// default, so a local resolution that gives up has somewhere else to land.
func writeStickyDefaultContext(t *testing.T) {
	t.Helper()
	body := "default = \"prod\"\n\n[[context]]\nname = \"prod\"\nurl = \"https://box.internal:9443\"\ncity = \"example-city\"\n"
	if err := os.WriteFile(DefaultPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("writing contexts.toml: %v", err)
	}
}

// Whether a registered city that cannot contribute a match still affects the
// scan is settled by failOnLoadError alone. It must never depend on the
// resolution mode: eager discovery captures the city it picks into the
// pack-command closures the user later runs, so an advisory answer that
// differs from the authoritative one is a wrong city, not a stale one.
//
// The unmatchable city is broken rather than merely irrelevant because that is
// the only way its presence is observable at all: a healthy city that cannot
// match contributes nothing to any scan. Broken, it makes the two axes visible
// — a match-only scan skips it before loading, and a scan asked to report load
// errors loads it and fails — and this asserts both modes agree on which.
func TestRigLookupTreatsUnmatchableBrokenCityTheSameInBothModes(t *testing.T) {
	cityPath, _ := setupDiscoveryLockCity(t)
	registerRigInSiteBinding(t, cityPath, "hauler")
	declareRigInCityTOML(t, cityPath, "hauler")
	registerUnloadableCity(t, filepath.Join(filepath.Dir(cityPath), "broken"))

	for _, scan := range []struct {
		name            string
		failOnLoadError bool
		wantMatches     int
		wantErr         bool
	}{
		// Match-only: the broken city cannot match, so it is skipped unread.
		{name: "match-only", failOnLoadError: false, wantMatches: 1},
		// Diagnostics requested: the broken city is exactly what was asked
		// about, so it is loaded and its failure is the answer.
		{name: "report-load-errors", failOnLoadError: true, wantErr: true},
	} {
		for _, mode := range []struct {
			name string
			mode contextResolutionMode
		}{
			{"advisory", completionResolutionMode},
			{"authoritative", authoritativeResolution},
		} {
			t.Run(scan.name+"/"+mode.name, func(t *testing.T) {
				matches, _, err := registeredRigBindingsByName("hauler", scan.failOnLoadError, mode.mode)
				if gotErr := err != nil; gotErr != scan.wantErr {
					t.Fatalf("rig lookup err = %v, want error: %v", err, scan.wantErr)
				}
				if len(matches) != scan.wantMatches {
					t.Fatalf("rig %q resolved to %d bindings, want %d", "hauler", len(matches), scan.wantMatches)
				}
			})
		}
	}
}

// registerUnloadableCity registers a city whose city.toml is present but
// unparseable, and which binds no rigs at all. Present-but-broken matters:
// a missing city.toml is reported as a stale registry entry instead, which the
// scan tolerates.
func registerUnloadableCity(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\nname = \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "discovery-lock-broken"); err != nil {
		t.Fatalf("registering broken city: %v", err)
	}
}

// registerRigInSiteBinding registers cityPath with the supervisor so the
// registry scan visits it, and binds one rig name to a machine-local path in
// .gc/site.toml. It returns the rig directory.
func registerRigInSiteBinding(t *testing.T, cityPath, rigName string) string {
	t.Helper()
	// Registering the same path under the same name twice is idempotent, so
	// callers may bind more than one rig.
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "discovery-lock"); err != nil {
		t.Fatalf("registering city: %v", err)
	}
	rigPath := filepath.Join(cityPath, "rigs", rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	sitePath := config.SiteBindingPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sitePath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(sitePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := fmt.Fprintf(f, "[[rig]]\nname = %q\npath = %q\n\n", rigName, rigPath); err != nil {
		t.Fatal(err)
	}
	return rigPath
}

// declareRigInCityTOML adds the [[rigs]] declaration that turns a machine-local
// site binding into a rig the city actually has. The path here is the legacy
// one the site binding overrides; only the name decides whether the rig exists.
func declareRigInCityTOML(t *testing.T, cityPath, rigName string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := fmt.Fprintf(f, "\n[[rigs]]\nname = %q\npath = %q\n", rigName, filepath.Join(cityPath, "rigs", rigName)); err != nil {
		t.Fatal(err)
	}
}
