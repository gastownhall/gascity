package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newBeadsPreflightChecker(cityPath, provider string) contract.PreflightChecker {
	return contract.PreflightChecker{
		FS:                        fsys.OSFS{},
		Provider:                  provider,
		BDContext:                 preflightBDContextReader(cityPath),
		DatabaseProjectID:         preflightDatabaseProjectIDReader(cityPath),
		DeferIdentityToNativeOpen: preflightIdentityDeferredReader(cityPath),
	}
}

// preflightBDContextTTL bounds how long one (city, scope) bd-context answer is
// reused. A one-shot CLI call probes once and exits well inside it; only a
// long-lived process (the controller, the API server) can outlive it, and that
// bound is what keeps a mid-process bd upgrade or backend switch from being
// answered with a stale probe until the next restart.
const preflightBDContextTTL = 5 * time.Minute

// preflightBDContextNow is the clock the memo ages its entries by. Tests
// advance it instead of sleeping.
var preflightBDContextNow = time.Now

// preflightBDCommandRunner is the test seam for the probe: when set it supplies
// the runner a bd-context read spawns through, so a counting fake can assert
// the hot path forks nothing once an answer is cached. Production leaves it nil
// and gets bdCommandRunnerForCity, resolved at call time rather than in a var
// initializer -- initializing on that function makes package main's
// initialization graph cyclic, because the store open it reaches back through
// is what eventually constructs this checker.
var preflightBDCommandRunner func(cityPath string) beads.CommandRunner

// preflightBDContextEntry is one (city, scope)'s bd-context answer, probed at
// most once per preflightBDContextTTL.
//
// The answer does not move inside that window: backend, dolt_mode, bd_version
// and schema_version change only when bd is replaced or the city's binding is
// rewritten, and gc already treats this probe as boot-scoped where it polls
// ("probe/latch memos, never a fresh probe - a status poll must not shell out
// to bd", api_state_conditional_writes.go). Holding the per-entry mutex across
// the probe also collapses concurrent readers of one scope into a single spawn.
type preflightBDContextEntry struct {
	mu    sync.Mutex
	value *contract.PreflightBDContext
	at    time.Time
}

// preflightBDContexts is keyed by cleaned cityPath + NUL + scope, so a process
// serving several cities or stores never answers one scope with another's
// backend. It is bounded by the scopes the process actually opens.
var (
	preflightBDContextsMu sync.Mutex
	preflightBDContexts   = map[string]*preflightBDContextEntry{}
)

// preflightBDContextReader returns the preflight's bd-context reader: bd's
// answer for the scope, forked at most once per TTL instead of once per store
// open. Every gc CLI call opens several stores, so the un-memoized read forked
// a bd child per open to ask what backend the process was about to open.
func preflightBDContextReader(cityPath string) func(scope string) (contract.PreflightBDContext, error) {
	return func(scope string) (contract.PreflightBDContext, error) {
		entry := preflightBDContextEntryFor(cityPath, scope)
		entry.mu.Lock()
		defer entry.mu.Unlock()
		if entry.value != nil && preflightBDContextNow().Sub(entry.at) < preflightBDContextTTL {
			return *entry.value, nil
		}
		ctx, err := probeBDContext(cityPath, scope)
		if err != nil {
			// A failed probe is never recorded: the next reader re-probes, so a
			// bd that is unreachable, uninstalled, or mid-upgrade still fails
			// the preflight closed exactly as it did before the memo.
			return contract.PreflightBDContext{}, err
		}
		value := ctx
		entry.value, entry.at = &value, preflightBDContextNow()
		return ctx, nil
	}
}

func preflightBDContextEntryFor(cityPath, scope string) *preflightBDContextEntry {
	key := filepath.Clean(cityPath) + "\x00" + filepath.Clean(scope)
	preflightBDContextsMu.Lock()
	defer preflightBDContextsMu.Unlock()
	if entry, ok := preflightBDContexts[key]; ok {
		return entry
	}
	entry := &preflightBDContextEntry{}
	preflightBDContexts[key] = entry
	return entry
}

// probeBDContext asks bd which backend a scope resolves to. This is the call
// that forks the child; the reader above is what keeps it to one per TTL.
func probeBDContext(cityPath, scope string) (contract.PreflightBDContext, error) {
	run := preflightBDCommandRunner
	if run == nil {
		run = bdCommandRunnerForCity
	}
	out, err := run(cityPath)(scope, "bd", "context", "--json")
	if err != nil {
		return contract.PreflightBDContext{}, err
	}
	var raw struct {
		Backend       string `json:"backend"`
		DoltMode      string `json:"dolt_mode"`
		BDVersion     string `json:"bd_version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return contract.PreflightBDContext{}, fmt.Errorf("parse bd context --json: %w", err)
	}
	return contract.PreflightBDContext{
		Backend:       raw.Backend,
		DoltMode:      raw.DoltMode,
		BDVersion:     raw.BDVersion,
		SchemaVersion: raw.SchemaVersion,
	}, nil
}

// preflightIdentityDeferredReader reports whether a scope resolves to an
// external Dolt endpoint (e.g. a hosted beads-gateway). The direct root/plaintext
// project_id probe cannot authenticate such endpoints, so when it comes back
// unconfirmed the identity check defers to beadslib's native-open verification
// (which authenticates via the credential command and refuses to connect on a
// _project_id mismatch) instead of degrading the scope off the native store.
func preflightIdentityDeferredReader(cityPath string) func(scope string) bool {
	return func(scope string) bool {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return false
		}
		return target.External
	}
}

func preflightDatabaseProjectIDReader(cityPath string) func(scope string) (string, bool, error) {
	return func(scope string) (string, bool, error) {
		target, ok, err := canonicalScopeDoltTarget(cityPath, scope)
		if err != nil || !ok {
			return "", false, err
		}
		// Pooled handle owned by internal/doltpool; do not Close.
		db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
		if err != nil {
			return "", false, err
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return "", false, err
		}
		return readDatabaseProjectID(ctx, db)
	}
}
