package main

import (
	"path/filepath"
	"sync"
	"sync/atomic"

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
// stored, so no caller may modify it. What a cached answer gives up is a
// re-read of files edited during the few hundred milliseconds between two of
// the helpers' loads, which the uncached path never guaranteed either (each
// load already reads its sources one by one).
//
// The scope also carries the builtin readiness deferral: the cities whose
// readiness pass the gc bd passthrough loader skipped
// (deferBuiltinReadinessForInvocation). It lives here so it dies with the
// scope and can never leak into a long-lived process.
//
// The scope also answers the city.toml [beads] peek (peekCityTOMLBeads): the
// first peek of a file decodes it and later peeks in the scope get that answer.
// A peek that could not read or parse the file, or decode one of its fields, is
// not remembered.
type invocationConfigMemo struct {
	mu      sync.Mutex
	entries map[string]invocationConfigMemoEntry
	// readinessDeferred holds normalized city paths whose builtin readiness
	// pass the passthrough loader deferred in this invocation.
	readinessDeferred map[string]struct{}
	// beadsPeeks are the decoded [beads] tables of the city.toml files this
	// scope peeked (peekCityTOMLBeads), by normalized path.
	beadsPeeks map[string]cityTOMLBeadsPeek
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
// scope deferred cityPath's builtin readiness pass. It is false outside a
// scope.
func builtinReadinessDeferredInInvocation(cityPath string) bool {
	memo := currentInvocationConfigMemo()
	if memo == nil {
		return false
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	_, ok := memo.readinessDeferred[normalizePathForCompare(cityPath)]
	return ok
}

type invocationConfigMemoEntry struct {
	cfg  *config.City
	prov *config.Provenance
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
	memo := &invocationConfigMemo{entries: map[string]invocationConfigMemoEntry{}}
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
	key := normalizePathForCompare(filepath.Clean(tomlPath))
	m.mu.Lock()
	defer m.mu.Unlock()
	if peek, ok := m.beadsPeeks[key]; ok {
		return peek
	}
	peek, ok := decodeCityTOMLBeadsPeek(tomlPath)
	if ok {
		if m.beadsPeeks == nil {
			m.beadsPeeks = map[string]cityTOMLBeadsPeek{}
		}
		m.beadsPeeks[key] = peek
	}
	return peek
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
	key := normalizePathForCompare(filepath.Clean(tomlPath))
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if entry, ok := memo.entries[key]; ok {
		return callerCopyOfCityConfig(entry.cfg), entry.prov, nil
	}
	composedCityConfigLoads.Add(1)
	cfg, prov, err := config.LoadWithIncludesOptions(fs, tomlPath, opts)
	if err != nil {
		return cfg, prov, err
	}
	memo.entries[key] = invocationConfigMemoEntry{cfg: callerCopyOfCityConfig(cfg), prov: prov}
	return cfg, prov, nil
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
