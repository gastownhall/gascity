package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// invocationConfigMemo shares one composed city config load across the
// helpers a single short-lived command runs.
//
// A one-shot command reaches the composed city config from many loaders that
// cannot share a *config.City: the command's own load, the store open (the
// store config and the pinned bd binary), the bd environment builder, the
// hosted-credential probe and the mail provider lookup, which alone asks three
// times. Each load walks every pack, include and order directory; on a
// production-sized city that is 50-70 ms a time. Measured against a 9-rig city
// with 16 packs, `gc mail inbox` composed it 16 times, `gc session list` 11 and
// `gc bd list` 6, and composition was most of each command's CPU.
// A `gc bd` passthrough's guards and env builders (doBd itself, the by-id twin
// census, the pinned-bd resolver, twice, and the hosted-credential probe) did
// the same before it execs bd. A load that calls config.LoadWithIncludesOptions
// itself is not shared, and the one-shot storage funnel's routing read
// (resolveCLIStorageRoutes) stays one by choice: a command that enters the
// funnel composes there once more, at most once per process, so that the
// config storageBootGate routes from shares no state with the scope's callers,
// and its wide call graph is not bound by the no-write-through rule below.
//
// The memo is OFF unless a scope is open. run() opens one for every command
// except the long-lived entry points in invocationConfigMemoLongLivedCommands,
// which reload config on purpose, and doBd opens one for a passthrough reached
// any other way. The processes the long-lived commands start (the supervisor,
// the controller and the API server they host, the nudge poller) also leave the
// memo explicitly before their loops (leaveInvocationConfigMemo), so a loop
// reached some other way still composes every reload. Inside a scope the memo
// answers only the one load shape the one-shot loaders use — the OS
// filesystem, no extra includes, and exactly the one-shot options
// (SkipRevisionSnapshot, nothing else) — and only for a successful load; an
// error is never remembered, so a helper that failed to load still fails on
// its own terms.
//
// Inside a scope the memo serves every composed load of that shape: every
// loadCityConfigFS and loadPrematerializedCityConfig caller, and every direct
// loadComposedCityConfig caller. Each gets its own copy
// (callerCopyOfCityConfig), so a caller may reassign top-level fields and
// Rigs[i]/Agents[i] element fields, as resolveRigPaths does; everything deeper
// is shared by every caller in the scope, so a caller must not write through a
// map, any other slice, or a pointer reachable from the config. The
// *config.Provenance is not copied at all: every caller of a key, the first
// included, gets the one the memo stored, so no caller may modify it. Each
// wrapper still runs its own per-call side effects (warnings, pack runtime
// validation, feature flags) on its copy.
//
// An entry is dropped, so the next load composes again, when:
//   - a writer changes what the load reads: every config write helper calls
//     invalidateInvocationConfigMemo (writeCityConfigForEditFS,
//     writeCityPackManifest, gc rig remove, gc pack's lock write, doctor's
//     site-binding repair), and so does validateComposedConfigAfterInstall
//     after `gc import install` materializes packs;
//   - the builtin-pack refresh runs for the first time in this scope
//     (noteBuiltinPackRefresh): an entry composed before it may predate files
//     the refresh rewrote;
//   - any file the load reported as a source, packs.lock or .gc/site.toml no
//     longer has the size and mtime it had when the load ran, which catches a
//     rewrite by a writer that does not call the hook.
type invocationConfigMemo struct {
	mu      sync.Mutex
	entries map[string]invocationConfigMemoEntry
	// refreshed holds the city.toml paths whose builtin-pack refresh has run
	// in this scope.
	refreshed map[string]bool
}

type invocationConfigMemoEntry struct {
	cfg     *config.City
	prov    *config.Provenance
	sources []invocationConfigSource
}

// invocationConfigSource is one file a memoized load depends on, as it was
// when the load ran. A file that did not exist is recorded too, so creating
// it drops the entry.
type invocationConfigSource struct {
	path    string
	exists  bool
	size    int64
	modTime time.Time
}

// composedCityConfigLoads counts the composed loads loadComposedCityConfig
// actually performed (memo hits excluded), for tests that pin a command's
// config cost.
var composedCityConfigLoads atomic.Int64

var (
	invocationConfigMemoMu     sync.Mutex
	activeInvocationConfigMemo *invocationConfigMemo
)

// invocationMemoLoadOptions is the only option set the memo answers: the
// one-shot load (skipRevisionSnapshot, cliStorageRoutesLoad and
// hostedCredentialProbeLoad are all exactly this).
var invocationMemoLoadOptions = config.LoadOptions{SkipRevisionSnapshot: true}

// invocationConfigMemoLongLivedCommands are the command paths run() never
// opens a scope for: each runs a loop that reloads city config.
var invocationConfigMemoLongLivedCommands = map[string]bool{
	"gc dashboard serve": true,
	"gc nudge poll":      true,
	"gc start":           true,
	"gc supervisor run":  true,
}

// invocationConfigMemoEligible reports whether run() opens a scope for cmd.
func invocationConfigMemoEligible(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	return !invocationConfigMemoLongLivedCommands[cmd.CommandPath()]
}

// beginInvocationConfigMemo opens a memo scope for the rest of a one-shot
// command and returns the function that closes it. Scopes do not nest: an inner
// begin while one is open reuses the open scope and its end is a no-op.
func beginInvocationConfigMemo() func() {
	invocationConfigMemoMu.Lock()
	defer invocationConfigMemoMu.Unlock()
	if activeInvocationConfigMemo != nil {
		return func() {}
	}
	memo := &invocationConfigMemo{
		entries:   map[string]invocationConfigMemoEntry{},
		refreshed: map[string]bool{},
	}
	activeInvocationConfigMemo = memo
	return func() {
		invocationConfigMemoMu.Lock()
		defer invocationConfigMemoMu.Unlock()
		if activeInvocationConfigMemo == memo {
			activeInvocationConfigMemo = nil
		}
	}
}

// leaveInvocationConfigMemo closes any open scope for the rest of the process.
// A long-lived host calls it before its loop starts, so every reload composes
// the config from disk.
func leaveInvocationConfigMemo() {
	invocationConfigMemoMu.Lock()
	activeInvocationConfigMemo = nil
	invocationConfigMemoMu.Unlock()
}

func currentInvocationConfigMemo() *invocationConfigMemo {
	invocationConfigMemoMu.Lock()
	defer invocationConfigMemoMu.Unlock()
	return activeInvocationConfigMemo
}

// invalidateInvocationConfigMemo drops every entry of the open scope. Config
// writers call it so the next load in the same invocation reads what they
// wrote.
func invalidateInvocationConfigMemo() {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return
	}
	memo.mu.Lock()
	clear(memo.entries)
	memo.mu.Unlock()
}

// noteBuiltinPackRefresh records that the builtin-pack refresh for tomlPath's
// city has run. The first time it runs in a scope it drops that city's entry:
// it was composed before the refresh and may predate files the refresh
// rewrote. Later refreshes in the same scope find the packs ready and keep it.
func noteBuiltinPackRefresh(tomlPath string) {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return
	}
	key := invocationConfigMemoKey(tomlPath)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.refreshed[key] {
		return
	}
	memo.refreshed[key] = true
	delete(memo.entries, key)
}

// loadComposedCityConfig is config.LoadWithIncludesOptions for the cmd/gc
// loaders, answered from the open invocation memo when there is one and the
// load is the shape it holds.
func loadComposedCityConfig(fs fsys.FS, tomlPath string, opts config.LoadOptions) (*config.City, *config.Provenance, error) {
	memo := currentInvocationConfigMemo()
	if memo == nil || opts != invocationMemoLoadOptions || !usesOSFS(fs) {
		composedCityConfigLoads.Add(1)
		return config.LoadWithIncludesOptions(fs, tomlPath, opts)
	}
	key := invocationConfigMemoKey(tomlPath)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if entry, ok := memo.entries[key]; ok && entry.current() {
		return callerCopyOfCityConfig(entry.cfg), entry.prov, nil
	}
	delete(memo.entries, key)
	before := statInvocationConfigSource(tomlPath)
	composedCityConfigLoads.Add(1)
	cfg, prov, err := config.LoadWithIncludesOptions(fs, tomlPath, opts)
	if err != nil {
		return cfg, prov, err
	}
	// A city.toml rewritten while the load ran could pair the new file's stat
	// with the old file's content; leave such a load unmemoized.
	if before.unchanged() {
		memo.entries[key] = invocationConfigMemoEntry{
			cfg:     callerCopyOfCityConfig(cfg),
			prov:    prov,
			sources: invocationConfigSources(tomlPath, prov),
		}
	}
	return cfg, prov, nil
}

func invocationConfigMemoKey(tomlPath string) string {
	if abs, err := filepath.Abs(tomlPath); err == nil {
		tomlPath = abs
	}
	return normalizePathForCompare(filepath.Clean(tomlPath))
}

// callerCopyOfCityConfig gives one caller its own view of a memoized config:
// a fresh top-level City with its own Rigs and Agents slices. The memo keeps
// the copy it stores untouched and hands every caller, the first included, an
// independent one, so a caller may reassign top-level fields and
// Rigs[i]/Agents[i] element fields (resolveRigPaths turns each rig path
// absolute in place). Everything deeper — maps, other slices, pointees — is
// shared with every other caller in the scope, and a caller must not write
// through it.
func callerCopyOfCityConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if cfg.Rigs != nil {
		out.Rigs = append([]config.Rig(nil), cfg.Rigs...)
	}
	if cfg.Agents != nil {
		out.Agents = append([]config.Agent(nil), cfg.Agents...)
	}
	return &out
}

// invocationConfigSources lists the files a load depends on: every source the
// load reported (city.toml, the city pack.toml, includes, implicit imports),
// plus the lock file that pins imported packs and the machine-local site
// bindings, neither of which the load reports as a source.
func invocationConfigSources(tomlPath string, prov *config.Provenance) []invocationConfigSource {
	cityDir := filepath.Dir(tomlPath)
	paths := []string{tomlPath, filepath.Join(cityDir, "packs.lock"), filepath.Join(cityDir, ".gc", "site.toml")}
	if prov != nil {
		paths = append(paths, prov.Sources...)
	}
	seen := make(map[string]struct{}, len(paths))
	sources := make([]invocationConfigSource, 0, len(paths))
	for _, path := range paths {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		sources = append(sources, statInvocationConfigSource(path))
	}
	return sources
}

func statInvocationConfigSource(path string) invocationConfigSource {
	fi, err := os.Stat(path)
	if err != nil {
		return invocationConfigSource{path: path}
	}
	return invocationConfigSource{path: path, exists: true, size: fi.Size(), modTime: fi.ModTime()}
}

func (s invocationConfigSource) unchanged() bool {
	now := statInvocationConfigSource(s.path)
	return now.exists == s.exists && now.size == s.size && now.modTime.Equal(s.modTime)
}

// current reports whether every file the entry's load depended on is as it
// was when the load ran.
func (e invocationConfigMemoEntry) current() bool {
	for _, source := range e.sources {
		if !source.unchanged() {
			return false
		}
	}
	return true
}
