package main

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// invocationConfigMemo shares one composed city config load across the
// helpers a single short-lived command runs.
//
// `gc bd` is the case it exists for. Before it execs bd, the passthrough's
// guards and env builders each loaded the composed config of the same city on
// their own: doBd itself, the by-id twin census, the pinned-bd resolver
// (twice, once through the storage-binding env) and the hosted-credential
// probe. Each load walks every pack, include and order directory; on a
// production-sized city that is 50-70 ms a time, and the repeats were most of
// gc's own share of an operator `gc bd show`. A load that calls
// config.LoadWithIncludesOptions itself is not shared, and the one-shot storage
// funnel's routing read (resolveCLIStorageRoutes) stays one by choice: a by-id
// `gc bd` that enters the funnel composes there once more, at most once per
// process, so that the config storageBootGate routes from shares no state with
// the scope's callers, and its wide call graph is not bound by the
// no-write-through rule below.
//
// The memo is OFF unless a command opens a scope with beginInvocationConfigMemo,
// so a long-running process (the supervisor, the API server) never sees it and
// keeps reloading what it reloads today. Inside a scope it answers a composed
// load only for the one load shape every one of those helpers uses — the OS
// filesystem, no extra includes, and exactly the one-shot options
// (SkipRevisionSnapshot, nothing else) — and only for a successful load; an
// error is never remembered, so a helper that failed to load still fails on its
// own terms.
//
// Inside a scope the memo serves every composed load of that shape, not only
// the helpers named above: every loadCityConfigFS and
// loadPrematerializedCityConfig caller, and every direct loadComposedCityConfig
// caller. Each gets its own copy (callerCopyOfCityConfig), so a caller may
// reassign top-level fields and Rigs[i]/Agents[i] element fields, as
// resolveRigPaths does; everything deeper is shared by every caller in the
// scope, so a caller must not write through a map, any other slice, or a
// pointer reachable from the config. The *config.Provenance is not copied at
// all: every caller of a key, the first included, gets the one the memo
// stored, so no caller may modify it. A cached answer is reused only while
// its sources are unchanged; the freshness rules below say when the next
// helper's load reads the files again.
//
// The scope also carries the builtin readiness deferral: the cities whose
// readiness pass the gc bd passthrough loader skipped
// (deferBuiltinReadinessForInvocation). It lives here so it dies with the
// scope and can never leak into a long-lived process. It stays only while the
// composition it was taken from is still current; a stale composition retires
// it, so a later load does not skip readiness on files the command has since
// changed.
//
// The scope also answers the city.toml [beads] peek (peekCityTOMLBeads): the
// first peek of a file decodes it and later peeks in the scope get that answer
// while the file's size and mtime are unchanged. A peek that could not read or
// parse the file, or decode one of its fields, is not remembered.
//
// Freshness. A remembered composition, peek, or readiness deferral is reused
// only while every file that composition reported, plus packs.lock and
// .gc/site.toml, still has the size and mtime it had when the load ran. A
// missing file is part of that record, so creating it is a change. That is
// how a city.toml write, an edited include, an imported pack, or a rig
// binding in site.toml is visible to the next read in the same command.
// Three events drop answers even when no stat moved, because the writer may
// have preserved size and mtime or changed a file the load did not report:
//   - invalidateInvocationConfigMemo, from the city.toml edit writer and from
//     the post-install validation load;
//   - the first builtin-pack refresh of a city in the scope
//     (noteBuiltinPackRefresh), which can rewrite caches composed earlier.
//
// A later refresh in the same scope keeps entries composed after the first.
// Errors are still never remembered.
type invocationConfigMemo struct {
	mu      sync.Mutex
	entries map[string]invocationConfigMemoEntry
	// readinessDeferred holds normalized city paths whose builtin readiness
	// pass the passthrough loader deferred in this invocation.
	readinessDeferred map[string]struct{}
	// beadsPeeks are the decoded [beads] tables of the city.toml files this
	// scope peeked (peekCityTOMLBeads), by normalized path.
	beadsPeeks map[string]memoizedBeadsPeek
	// refreshed holds city.toml paths whose builtin-pack refresh has already
	// run in this scope. The first refresh drops answers composed before it.
	refreshed map[string]struct{}
}

// invocationMemoSource is one file a remembered load depends on, as it was
// when the load or peek ran. A file that did not exist is recorded too.
type invocationMemoSource struct {
	path    string
	exists  bool
	size    int64
	modTime time.Time
}

// memoizedBeadsPeek is one remembered [beads] decode plus the stat of the
// file it was decoded from.
type memoizedBeadsPeek struct {
	peek   cityTOMLBeadsPeek
	source invocationMemoSource
}

// deferBuiltinReadinessForInvocation records that this invocation composed
// cityPath's config without the builtin readiness pass and does not want one
// for the rest of the scope. It is a no-op outside a scope.
func deferBuiltinReadinessForInvocation(cityPath string) {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.readinessDeferred == nil {
		memo.readinessDeferred = map[string]struct{}{}
	}
	memo.readinessDeferred[normalizePathForCompare(cityPath)] = struct{}{}
}

// builtinReadinessDeferredInInvocation reports whether the open invocation
// scope deferred cityPath's builtin readiness pass and that composition's
// sources are still unchanged. It is false outside a scope, and a stale
// deferral is dropped rather than honored.
func builtinReadinessDeferredInInvocation(cityPath string) bool {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return false
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	cityKey := normalizePathForCompare(cityPath)
	if _, ok := memo.readinessDeferred[cityKey]; !ok {
		return false
	}
	// The deferral is the passthrough's promise that nothing after its
	// composition needs the readiness pass. That promise ends when the
	// composition's sources have changed, or when the composition itself
	// was dropped.
	tomlKey := invocationMemoKey(filepath.Join(cityPath, "city.toml"))
	if entry, ok := memo.entries[tomlKey]; ok && entry.current() {
		return true
	}
	delete(memo.readinessDeferred, cityKey)
	return false
}

type invocationConfigMemoEntry struct {
	cfg     *config.City
	prov    *config.Provenance
	sources []invocationMemoSource
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
		refreshed: map[string]struct{}{},
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

func currentInvocationConfigMemo() *invocationConfigMemo {
	invocationConfigMemoMu.Lock()
	defer invocationConfigMemoMu.Unlock()
	return activeInvocationConfigMemo
}

// cityTOMLBeadsPeek answers peekCityTOMLBeads for tomlPath from this scope,
// decoding the file on the first ask. Only an answer that decoded both fields
// is remembered: a peek that could not read or parse the file, or decode one of
// its fields, is retried by the next caller, the rule the composed-config
// entries keep for errors.
func (m *invocationConfigMemo) cityTOMLBeadsPeek(tomlPath string) cityTOMLBeadsPeek {
	key := invocationMemoKey(tomlPath)
	m.mu.Lock()
	defer m.mu.Unlock()
	if peeked, ok := m.beadsPeeks[key]; ok && peeked.source.unchanged() {
		return peeked.peek
	}
	delete(m.beadsPeeks, key)
	before := statInvocationMemoSource(key)
	peek, ok := decodeCityTOMLBeadsPeek(tomlPath)
	if ok && before.unchanged() {
		if m.beadsPeeks == nil {
			m.beadsPeeks = map[string]memoizedBeadsPeek{}
		}
		m.beadsPeeks[key] = memoizedBeadsPeek{peek: peek, source: before}
	}
	return peek
}

// invalidateInvocationConfigMemo drops every composition, beads peek, and
// readiness deferral in the open scope. City.toml writers and the import
// install validation load call it so a later read in the same command sees
// what they wrote, including a write that preserved size and mtime and a
// pack tree the earlier load did not report. It is a no-op outside a scope.
func invalidateInvocationConfigMemo() {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	memo.entries = map[string]invocationConfigMemoEntry{}
	memo.beadsPeeks = nil
	memo.readinessDeferred = nil
}

// noteBuiltinPackRefresh records that the builtin-pack refresh for tomlPath's
// city has run. The first refresh in the scope drops that city's composition,
// beads peek, and readiness deferral: they may predate files the refresh
// rewrote. A later refresh in the same scope keeps answers composed after the
// first. It is a no-op outside a scope.
func noteBuiltinPackRefresh(tomlPath string) {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return
	}
	key := invocationMemoKey(tomlPath)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.refreshed == nil {
		memo.refreshed = map[string]struct{}{}
	}
	if _, already := memo.refreshed[key]; already {
		return
	}
	memo.refreshed[key] = struct{}{}
	delete(memo.entries, key)
	delete(memo.beadsPeeks, key)
	delete(memo.readinessDeferred, invocationMemoCityKey(tomlPath))
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
	key := invocationMemoKey(tomlPath)
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if entry, ok := memo.entries[key]; ok {
		if entry.current() {
			return callerCopyOfCityConfig(entry.cfg), entry.prov, nil
		}
		delete(memo.entries, key)
		delete(memo.readinessDeferred, invocationMemoCityKey(tomlPath))
	}
	// A city.toml rewritten while the load runs could pair the new file's
	// stat with the old file's content. Leave that load unmemoized.
	before := statInvocationMemoSource(key)
	composedCityConfigLoads.Add(1)
	cfg, prov, err := config.LoadWithIncludesOptions(fs, tomlPath, opts)
	if err != nil {
		return cfg, prov, err
	}
	if before.unchanged() {
		memo.entries[key] = invocationConfigMemoEntry{
			cfg:     callerCopyOfCityConfig(cfg),
			prov:    prov,
			sources: invocationMemoSources(tomlPath, prov),
		}
	}
	return cfg, prov, nil
}

func invocationMemoKey(path string) string {
	return normalizePathForCompare(filepath.Clean(path))
}

func invocationMemoCityKey(tomlPath string) string {
	return normalizePathForCompare(filepath.Dir(tomlPath))
}

// invocationMemoSources lists the files a composition depends on: every source
// the load reported (city.toml, includes, pack files, rig fragments) plus
// packs.lock and .gc/site.toml, which the load does not report. The lock pins
// imported packs; site.toml holds the machine-local rig bindings.
func invocationMemoSources(tomlPath string, prov *config.Provenance) []invocationMemoSource {
	cityDir := filepath.Dir(tomlPath)
	paths := []string{
		tomlPath,
		filepath.Join(cityDir, "packs.lock"),
		filepath.Join(cityDir, ".gc", "site.toml"),
	}
	if prov != nil {
		paths = append(paths, prov.Sources...)
	}
	seen := make(map[string]struct{}, len(paths))
	sources := make([]invocationMemoSource, 0, len(paths))
	for _, path := range paths {
		key := invocationMemoKey(path)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		sources = append(sources, statInvocationMemoSource(key))
	}
	return sources
}

func statInvocationMemoSource(path string) invocationMemoSource {
	fi, err := os.Stat(path)
	if err != nil {
		return invocationMemoSource{path: path}
	}
	return invocationMemoSource{path: path, exists: true, size: fi.Size(), modTime: fi.ModTime()}
}

func (s invocationMemoSource) unchanged() bool {
	now := statInvocationMemoSource(s.path)
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
