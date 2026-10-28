package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/spf13/cobra"
)

// One store open per one-shot invocation.
//
// A session-side hook (`gc mail check --inject`, `gc nudge drain --inject`,
// `gc prime --hook`, ...) reaches its ledger through several helpers, and each
// used to open the scope's store for itself. Against a remote ledger every open
// is a handshake, so a hook paid one to five handshakes for one ledger.
//
// While the memo is active, the shared open body (openStoreResultAtForCityScoped)
// opens each scope once and hands every later caller a BORROWED handle: a fresh
// bead-policy wrapper over the same opened store, whose close
// (closeBeadStoreHandle) is a no-op. The real handles are released when the
// invocation ends, by the same exit path that releases the one-shot storage
// routes. Owning frames such as openOwnedNudgeBeadStore keep their contract —
// they close what they were handed — and closing a borrowed handle leaves the
// shared store open for the next caller.
//
// One connection pool. Every caller in a memoized invocation shares the one
// opened store, and with it that store's connection pool — where without the
// memo each helper's open brought a pool of its own. A backend capped at one
// connection (the SQLite store's write connection and the DoltLite read store
// open with MaxOpenConns=1) then serializes those callers on that connection. So no caller may hold a
// transaction or a connection-scoped operation open while it calls code that
// opens the same scope: under the memo that open is the same pool, and the
// inner call would wait on the outer one's connection forever. The hook verbs
// in invocationStoreMemoCommands hold no transaction or connection-scoped
// operation across another store call. Where they make calls concurrently —
// the step reminder's per-identity listings (stepResolution.rootViaRoutedBridge)
// and the storage routes resolved alongside the city store open
// (prewarmCLIStorageRoutes) — each call stands alone, so a single-connection
// store runs them one after another.
//
// The memo is opt-in per command. run() enables it only for the one-shot verbs
// in invocationStoreMemoCommands; long-lived entry points (the supervisor and
// controller, `gc nudge poll`, the API server) never see it, so a store they
// close and reopen is really closed and reopened.

// invocationStoreMemoCommands are the command paths run() opens the memo for:
// the session-side hook verbs, each of which runs once and exits.
var invocationStoreMemoCommands = map[string]bool{
	"gc handoff":     true,
	"gc hook":        true,
	"gc mail check":  true,
	"gc mail send":   true,
	"gc nudge drain": true,
	"gc prime":       true,
}

// invocationStoreMemoEligible reports whether run() opens the memo for cmd.
func invocationStoreMemoEligible(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	return invocationStoreMemoCommands[cmd.CommandPath()]
}

// invocationStoreKey names one opened store. The provider and the authority of
// the provider choice decide which backend opens; the scope and city decide
// which ledger it reaches.
type invocationStoreKey struct {
	scopeRoot     string
	cityPath      string
	provider      string
	authoritative bool
}

// invocationStoreEntry is one scope's store, opened at most once while the
// memo is active. Its mutex is held across the open, so concurrent callers for
// the same scope wait for the first open instead of making their own.
type invocationStoreEntry struct {
	mu              sync.Mutex
	opened          bool
	result          beads.StoreOpenResult
	cfg             *config.City
	mode            gate.Mode
	nativeTransport beads.NativeTransportMode
}

// invocationStoreMemo is the memo of one invocation. A nil memo is disabled.
type invocationStoreMemo struct {
	mu      sync.Mutex
	entries map[invocationStoreKey]*invocationStoreEntry
}

var (
	invocationStoresMu sync.Mutex
	invocationStores   *invocationStoreMemo
)

// beginInvocationStoreMemo activates the memo for the rest of this invocation
// and returns the function that ends it: it closes every store the memo opened
// and deactivates the memo. Calling the end function more than once is safe.
func beginInvocationStoreMemo() (end func() error) {
	memo := &invocationStoreMemo{entries: make(map[invocationStoreKey]*invocationStoreEntry)}
	invocationStoresMu.Lock()
	previous := invocationStores
	invocationStores = memo
	invocationStoresMu.Unlock()
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() {
			invocationStoresMu.Lock()
			if invocationStores == memo {
				invocationStores = previous
			}
			invocationStoresMu.Unlock()
			closeErr = memo.close()
		})
		return closeErr
	}
}

// endInvocationStoreMemoLogged ends the memo for run(). The command's outcome
// is decided by then, so an error releasing a store does not change it, but
// it is shown on stderr under GC_DEBUG rather than dropped.
func endInvocationStoreMemoLogged(end func() error, stderr io.Writer) {
	if err := end(); err != nil && gcDebugEnabled() {
		fmt.Fprintf(stderr, "gc: releasing the invocation's stores: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// activeInvocationStoreMemo returns the active memo, or nil.
func activeInvocationStoreMemo() *invocationStoreMemo {
	invocationStoresMu.Lock()
	defer invocationStoresMu.Unlock()
	return invocationStores
}

// entry returns the memo slot for key, creating it under the memo lock.
func (m *invocationStoreMemo) entry(key invocationStoreKey) *invocationStoreEntry {
	key.scopeRoot = filepath.Clean(key.scopeRoot)
	key.cityPath = filepath.Clean(key.cityPath)
	key.provider = strings.TrimSpace(key.provider)
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		e = &invocationStoreEntry{}
		m.entries[key] = e
	}
	return e
}

// close releases every store the memo opened. The entries are detached under
// the lock first, so a close never races a caller into a closed store.
func (m *invocationStoreMemo) close() error {
	m.mu.Lock()
	entries := m.entries
	m.entries = make(map[invocationStoreKey]*invocationStoreEntry)
	m.mu.Unlock()
	var errs []error
	for _, e := range entries {
		e.mu.Lock()
		if e.opened {
			if err := closeBeadStoreHandle(e.result.Store); err != nil {
				errs = append(errs, err)
			}
			e.opened = false
			e.result = beads.StoreOpenResult{}
		}
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}

// borrow returns a borrowed handle over the entry's store, wrapped in the bead
// policies of cfg. The caller holds e.mu.
func (e *invocationStoreEntry) borrow(cfg *config.City) beads.StoreOpenResult {
	result := e.result
	result.Store = borrowedStoreWithBeadPolicies(result.Store, cfg)
	return result
}
