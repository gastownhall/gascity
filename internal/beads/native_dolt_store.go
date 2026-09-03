package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
)

const nativeDoltStoreActor = "gascity"

// nativeDoltOpenReadyStatuses lists the upstream bd statuses Ready() queries
// GetReadyWork for. This must match IsReadyCandidateForTier's contract of
// "open status ... and no future defer_until": only StatusOpen (bd's own
// status-category table marks it the sole "active" category status) and
// StatusDeferred (kept only because IsDeferred independently re-checks
// DeferUntil, so an expired deferral must still resurface) belong here.
// blocked/hooked are bd's "wip" category and pinned is "frozen" — bd's own
// ready semantics already exclude them, and Gas City has no analogous
// re-check for them the way it does for deferred, so querying for them let
// dependency-blocked beads erase their status to "open" via mapBdStatus and
// pass IsReadyCandidateForTier's status gate. See ga-3mv5d3 bead notes for
// the full investigation.
var nativeDoltOpenReadyStatuses = []beadslib.Status{
	beadslib.StatusOpen,
	beadslib.StatusDeferred,
}

var (
	nativeDoltOpenBestAvailable = beadslib.OpenBestAvailable
	nativeDoltOpenEnvMu         sync.Mutex
	errNativeIssueMetadataParse = ErrMetadataParse
)

var nativeDoltOpenEnvKeys = []string{
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DOLT_AUTO_START",
	"BEADS_DOLT_DATA_DIR",
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

func nativeDoltOperationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, bdCommandTimeout)
}

// nativeGraphApplyDeadline scales the graph-apply transaction budget with plan
// size. The library's AddDependency runs a recursive cycle-reachability query
// per blocking edge, so a large molecule (67 nodes / ~100 edges on the
// mol-adopt-pr-v2 shape) cannot finish inside the flat per-command budget: the
// batch died at the 120s deadline mid-edges, retried into the same wall, and
// fell back to per-bead creates — turning a single atomic pour into ~9 minutes
// of partial work (2026-07-17 code red). Until the per-edge check is replaced
// by one whole-graph CycleThroughEdges pass (needs a beads-side export of
// DependencyAddOptions), give each node and edge a slice of budget on top of
// the flat floor so the atomic path completes instead of falling back.
func nativeGraphApplyDeadline(plan *GraphApplyPlan) time.Duration {
	d := bdCommandTimeout
	if plan == nil {
		return d
	}
	const perItem = 2 * time.Second
	return d + time.Duration(len(plan.Nodes)+len(plan.Edges))*perItem
}

func nativeDoltCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), bdCommandTimeout)
}

// ProcessEnvSnapshotExcludingNativeDoltOpen returns a process environment
// snapshot after any in-flight native Dolt open has restored scoped BEADS_* env.
func ProcessEnvSnapshotExcludingNativeDoltOpen() []string {
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()
	return os.Environ()
}

// AmbientNativeDoltOpenEnv returns the ambient process-env value for key, read
// under nativeDoltOpenEnvMu so it reflects the restored ambient environment
// rather than a value a concurrent native Dolt open is temporarily projecting.
// withNativeDoltOpenEnv mutates the keys in nativeDoltOpenEnvKeys (which include
// BEADS_DOLT_SERVER_TLS) under this mutex, so a bare os.Getenv of one of those
// keys can observe another scope's transient projection; this guarded read
// cannot. It mirrors os.Getenv: an unset key returns "".
func AmbientNativeDoltOpenEnv(key string) string {
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()
	return os.Getenv(key)
}

func processEnvSnapshotExcludingNativeDoltOpen() []string {
	return ProcessEnvSnapshotExcludingNativeDoltOpen()
}

func withNativeDoltOpenEnv(env map[string]string) (func(), error) {
	return withNativeDoltOpenEnvAndCredentialCommand(env, "")
}

func withNativeDoltOpenEnvAndCredentialCommand(env map[string]string, credentialCommand string) (func(), error) {
	nativeDoltOpenEnvMu.Lock()
	restore, err := withNativeDoltOpenEnvAndCredentialCommandLocked(env, credentialCommand)
	if err != nil {
		nativeDoltOpenEnvMu.Unlock()
		return nil, err
	}
	return func() {
		restore()
		nativeDoltOpenEnvMu.Unlock()
	}, nil
}

// withNativeDoltOpenEnvAndCredentialCommandLocked projects the scoped native
// open environment while nativeDoltOpenEnvMu is already held. The lock-aware
// form is used by hermetic opens, which must withhold the whole BEADS_ namespace
// and project the selected keys as one indivisible environment transition.
func withNativeDoltOpenEnvAndCredentialCommandLocked(env map[string]string, credentialCommand string) (func(), error) {
	keys := nativeDoltOpenEnvKeys
	if credentialCommand != "" {
		keys = append(append([]string(nil), nativeDoltOpenEnvKeys...), "BEADS_DOLT_CREDENTIAL_COMMAND")
	}
	previous := make(map[string]*string, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			copied := value
			previous[key] = &copied
		} else {
			previous[key] = nil
		}
		value, ok := env[key]
		if key == "BEADS_DOLT_CREDENTIAL_COMMAND" {
			value, ok = credentialCommand, true
		}
		var err error
		if ok && strings.TrimSpace(value) != "" {
			err = os.Setenv(key, value)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			restoreNativeDoltOpenEnv(previous, keys)
			return nil, fmt.Errorf("projecting native Dolt open env %s: %w", key, err)
		}
	}
	return func() {
		restoreNativeDoltOpenEnv(previous, keys)
	}, nil
}

func restoreNativeDoltOpenEnv(previous map[string]*string, keys []string) {
	for _, key := range keys {
		if value := previous[key]; value != nil {
			_ = os.Setenv(key, *value)
			continue
		}
		_ = os.Unsetenv(key)
	}
}

// beadsEnvPrefix is the namespace the upstream library configures itself from.
const beadsEnvPrefix = "BEADS_"

// withWithheldBeadsEnv unsets every ambient BEADS_-prefixed variable and
// returns the restore.
//
// It is prefix-based rather than a key list on purpose. The scoped projection
// above names the thirteen variables gc itself sets; the library reads many
// more — a credential command, database and directory overrides, a central
// config path — and the list grows with the library. A caller that must be
// sure a workspace is served by its own configuration alone cannot maintain a
// mirror of somebody else's environment surface, so it withholds the namespace.
func withWithheldBeadsEnv() (func(), error) {
	nativeDoltOpenEnvMu.Lock()
	restore, err := withWithheldBeadsEnvLocked()
	if err != nil {
		nativeDoltOpenEnvMu.Unlock()
		return nil, err
	}
	return func() {
		restore()
		nativeDoltOpenEnvMu.Unlock()
	}, nil
}

// withWithheldBeadsEnvLocked performs whole-namespace withholding while
// nativeDoltOpenEnvMu is already held. Keeping this operation on the same lock
// as snapshots and ordinary native opens makes the process environment appear
// atomic to every caller that uses the guarded helpers.
func withWithheldBeadsEnvLocked() (func(), error) {
	type withheld struct{ key, value string }
	var previous []withheld
	restore := func() {
		for _, entry := range previous {
			_ = os.Setenv(entry.key, entry.value)
		}
	}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, beadsEnvPrefix) {
			continue
		}
		previous = append(previous, withheld{key: key, value: value})
		if err := os.Unsetenv(key); err != nil {
			restore()
			return nil, fmt.Errorf("withholding ambient %s: %w", key, err)
		}
	}
	return func() {
		restore()
	}, nil
}

// openNativeStorageWithoutAmbientEnvWithCredentialCommand opens native
// storage while holding the same process-environment lock used by snapshots
// and ordinary native opens. The whole BEADS_ namespace is withheld before
// the selected scoped values are projected, and both restorations happen
// before the lock is released.
func openNativeStorageWithoutAmbientEnvWithCredentialCommand(ctx context.Context, scopeRoot, credentialCommand string, readPrefix bool) (beadslib.Storage, string, error) {
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()

	restoreNamespace, err := withWithheldBeadsEnvLocked()
	if err != nil {
		return nil, "", err
	}
	defer restoreNamespace()

	restoreEnv, err := withNativeDoltOpenEnvAndCredentialCommandLocked(nil, credentialCommand)
	if err != nil {
		return nil, "", err
	}
	defer restoreEnv()

	storage, err := nativeDoltOpenBestAvailable(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, "", err
	}
	var prefix string
	if readPrefix {
		prefix, err = storage.GetConfig(ctx, "issue_prefix")
		if err != nil {
			_ = storage.Close()
			return nil, "", fmt.Errorf("reading native issue prefix: %w", err)
		}
	}
	return storage, prefix, nil
}

// OpenNativeDoltStoreAtWithoutAmbientEnv opens a native store at scopeRoot with
// every ambient BEADS_-prefixed variable withheld for the duration of the open.
//
// It is for a caller whose whole contract is that the workspace's own
// configuration decides how the workspace is served — so an inherited variable
// naming another database, another directory, or a credential command must not
// be able to re-point it. Passing an empty scoped environment is not enough:
// that clears only the variables gc itself projects.
func OpenNativeDoltStoreAtWithoutAmbientEnv(ctx context.Context, scopeRoot string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	return newNativeDoltStoreAtWithoutAmbientEnv(ctx, scopeRoot, "", opts...)
}

// OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand opens a native
// Dolt-backed store with the ambient BEADS_ namespace withheld and one
// explicitly selected credential command projected for the duration of the
// open.
//
// The command is deliberately supplied by the caller rather than read from
// the process environment. This keeps a workspace open hermetic while still
// allowing a binding that explicitly selected a remote credential provider to
// authenticate its server connection. The command is restored with the rest
// of the withheld namespace before this function returns.
func OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand(ctx context.Context, scopeRoot, credentialCommand string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	if strings.TrimSpace(credentialCommand) == "" {
		return nil, errors.New("native Dolt open credential command is empty")
	}
	return newNativeDoltStoreAtWithoutAmbientEnv(ctx, scopeRoot, credentialCommand, opts...)
}

// NativeDoltStore is a Store implementation backed by the upstream beads
// library over Dolt. It is constructed by the store factory after native-store
// preflight gates pass.
type NativeDoltStore struct {
	mu      sync.RWMutex
	storage beadslib.Storage
	// generation increments on every successful reconnect. A read that fails
	// with a transient connection error records the generation it observed and
	// asks reconnect to swap the dead handle only if no other reader already did.
	generation uint64
	actor      string
	idPrefix   string

	// reopen re-establishes the managed Dolt connection after a transient
	// connection failure (a :3307 hard-kill/rebind). It MUST re-resolve the
	// CURRENT managed Dolt port and return a fresh storage handle bound to the
	// live server — the store's original open env pins the now-dead port, so a
	// naive re-open of the cached env would keep dialing it. It is injected by
	// the store factory (which owns managed-Dolt port discovery + restart); a
	// nil reopen disables reconnect and preserves fail-fast behavior for test
	// handles built directly from a storage value.
	reopen NativeReopenFunc
	// reconnectGate is a single token used to serialize reconnects. Readers wait
	// on it with their retry context, so a reconnect already in progress cannot
	// make another read outlive its wall-clock budget.
	reconnectGate chan struct{}
	// closed is the one-way terminal latch. CloseStore sets it (under mu) so an
	// in-flight reconnect's post-reopen re-check discards its fresh handle instead
	// of installing it after the store is permanently closed.
	closed bool
	// readRetryBudgetOverride, when non-zero, replaces nativeReadRetryBudget as the
	// single wall-clock bound on a read's whole reconnect-and-retry chain. Only
	// tests set it (to exercise budget exhaustion without a real 90s wait).
	readRetryBudgetOverride time.Duration

	// condWritesStamp carries the factory-stamped conditional-writes mode. The
	// pinned upstream Storage contract requires row-version checked update and
	// close plus transactions; DeleteIfMatch composes the matching transaction
	// from GetIssue, RowVersion equality, and DeleteIssue.
	condWritesStamp

	localStrings *localSidecar // clone-local data; see Store.SetLocalString
}

// NativeStorage is the upstream beads storage handle a NativeDoltStore wraps.
// It is aliased so a caller (e.g. the store factory) can build a WithNativeReopen
// hook without importing the upstream beads package directly.
type NativeStorage = beadslib.Storage

// NativeReopenFunc re-establishes a native Dolt storage handle after a transient
// connection failure. See WithNativeReopen and NativeDoltStore.reopen.
type NativeReopenFunc func(context.Context) (NativeStorage, error)

// NativeDoltStoreOption configures a NativeDoltStore at open.
type NativeDoltStoreOption func(*NativeDoltStore)

// WithNativeReopen injects the reconnect hook the read path uses to recover the
// managed Dolt connection after a transient failure. The hook must re-resolve
// the current managed port (the cached open env pins the old one) and return a
// fresh storage handle. See NativeDoltStore.reopen.
func WithNativeReopen(reopen NativeReopenFunc) NativeDoltStoreOption {
	return func(s *NativeDoltStore) { s.reopen = reopen }
}

var (
	_ Store                         = (*NativeDoltStore)(nil)
	_ ConditionalAssignmentReleaser = (*NativeDoltStore)(nil)
	_ AtomicTxStore                 = (*NativeDoltStore)(nil)
	_ GraphApplyStore               = (*NativeDoltStore)(nil)
	_ StorageGraphApplyStore        = (*NativeDoltStore)(nil)
	_ EphemeralGraphApplyStore      = (*NativeDoltStore)(nil)
	_ conditionalWritesModeCarrier  = (*NativeDoltStore)(nil)
)

func newNativeDoltStoreWithStorage(storage beadslib.Storage, actor string) *NativeDoltStore {
	if actor == "" {
		actor = nativeDoltStoreActor
	}
	return &NativeDoltStore{storage: storage, actor: actor, localStrings: newLocalSidecar("")}
}

func newNativeDoltStoreWithStorageAndPrefix(storage beadslib.Storage, actor, idPrefix string) *NativeDoltStore {
	store := newNativeDoltStoreWithStorage(storage, actor)
	store.idPrefix = normalizeIDPrefix(idPrefix)
	return store
}

// OpenNativeDoltStoreAt opens a native Dolt-backed beads store at scopeRoot
// while projecting the supplied scoped Dolt environment for upstream beads.
// Pass WithNativeReopen to arm transparent reconnect across a managed-Dolt
// rebind.
func OpenNativeDoltStoreAt(ctx context.Context, scopeRoot string, env map[string]string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	return newNativeDoltStoreAt(ctx, scopeRoot, env, opts...)
}

func newNativeDoltStoreAt(parent context.Context, scopeRoot string, env map[string]string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	return newNativeDoltStoreAtWithCredentialCommand(parent, scopeRoot, env, "", opts...)
}

func newNativeDoltStoreAtWithCredentialCommand(parent context.Context, scopeRoot string, env map[string]string, credentialCommand string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	storage, prefix, err := openNativeStorageWithCredentialCommand(ctx, scopeRoot, env, credentialCommand, true)
	if err != nil {
		return nil, err
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, nativeDoltStoreActor, prefix)
	store.localStrings = newLocalSidecar(filepath.Join(scopeRoot, ".beads", "local-strings.json"))
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

func newNativeDoltStoreAtWithoutAmbientEnv(parent context.Context, scopeRoot, credentialCommand string, opts ...NativeDoltStoreOption) (*NativeDoltStore, error) {
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	storage, prefix, err := openNativeStorageWithoutAmbientEnvWithCredentialCommand(ctx, scopeRoot, credentialCommand, true)
	if err != nil {
		return nil, err
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, nativeDoltStoreActor, prefix)
	store.localStrings = newLocalSidecar(filepath.Join(scopeRoot, ".beads", "local-strings.json"))
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

// OpenNativeStorage opens a native Dolt storage handle for the given scope and
// projected env. It is the building block for a NativeDoltStore reopen hook: a
// caller that has re-resolved the CURRENT managed Dolt env (fresh port) passes
// it here to get a fresh handle bound to the live server.
func OpenNativeStorage(ctx context.Context, scopeRoot string, env map[string]string) (NativeStorage, error) {
	storage, _, err := openNativeStorage(ctx, scopeRoot, env, false)
	return storage, err
}

// OpenNativeStorageAtWithoutAmbientEnvWithCredentialCommand opens native
// storage with the ambient BEADS_ namespace withheld and the selected
// credential command projected for this one open. It is intended for a
// NativeReopenFunc so a hosted workspace resolves a fresh short-lived
// credential on every bounded reconnect attempt.
func OpenNativeStorageAtWithoutAmbientEnvWithCredentialCommand(ctx context.Context, scopeRoot, credentialCommand string) (NativeStorage, error) {
	if strings.TrimSpace(credentialCommand) == "" {
		return nil, errors.New("native Dolt open credential command is empty")
	}
	storage, _, err := openNativeStorageWithoutAmbientEnvWithCredentialCommand(ctx, scopeRoot, credentialCommand, false)
	return storage, err
}

// openNativeStorage projects the scoped Dolt env, opens the best-available
// native storage, and (when readPrefix) reads the configured issue prefix while
// the env is still projected. It is shared by the initial open and the
// read-path reconnect that recovers from a managed-Dolt hard-kill/rebind.
func openNativeStorage(ctx context.Context, scopeRoot string, env map[string]string, readPrefix bool) (beadslib.Storage, string, error) {
	return openNativeStorageWithCredentialCommand(ctx, scopeRoot, env, "", readPrefix)
}

func openNativeStorageWithCredentialCommand(ctx context.Context, scopeRoot string, env map[string]string, credentialCommand string, readPrefix bool) (beadslib.Storage, string, error) {
	restoreEnv, err := withNativeDoltOpenEnvAndCredentialCommand(env, credentialCommand)
	if err != nil {
		return nil, "", err
	}
	defer restoreEnv()
	storage, err := nativeDoltOpenBestAvailable(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, "", err
	}
	var prefix string
	if readPrefix {
		prefix, err = storage.GetConfig(ctx, "issue_prefix")
		if err != nil {
			_ = storage.Close()
			return nil, "", fmt.Errorf("reading native issue prefix: %w", err)
		}
	}
	return storage, prefix, nil
}

func newNativeDoltStoreForTest(storage beadslib.Storage) *NativeDoltStore {
	return newNativeDoltStoreWithStorage(storage, "native-test")
}

// IDPrefix returns the bead ID prefix owned by this store, without trailing "-".
func (s *NativeDoltStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.idPrefix
}

func (s *NativeDoltStore) listIncludesCompleteDependencies() bool {
	return true
}

func (s *NativeDoltStore) acquireStorage() (beadslib.Storage, func(), error) {
	if s == nil {
		return nil, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	s.mu.RLock()
	if s.closed || s.storage == nil {
		s.mu.RUnlock()
		return nil, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	return s.storage, s.mu.RUnlock, nil
}

// acquireStorageGen is acquireStorage plus the current reconnect generation, so
// the read-retry path can ask reconnect to swap only the exact handle it saw
// fail (single-flight across concurrent readers).
func (s *NativeDoltStore) acquireStorageGen() (beadslib.Storage, uint64, func(), error) {
	if s == nil {
		return nil, 0, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	s.mu.RLock()
	if s.closed || s.storage == nil {
		s.mu.RUnlock()
		return nil, 0, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	return s.storage, s.generation, s.mu.RUnlock, nil
}

const (
	// nativeReadRetryBudget bounds the total reconnect-and-retry time for a
	// single read. It must comfortably exceed the managed-Dolt hard-kill/rebind
	// window (~40-56s of mysql i/o timeouts before the dead handle surfaces the
	// error, plus the restart) so a read spanning a rebind recovers rather than
	// failing, while still failing fast for a genuinely down server.
	nativeReadRetryBudget = 90 * time.Second
	// nativeReadRetryBackoff spaces reconnect-and-retry passes.
	nativeReadRetryBackoff = 200 * time.Millisecond
)

// withReadRetry runs a read against the native storage handle, transparently
// reconnecting and retrying when the handle fails with a transient connection
// error — the :3307 hard-kill/rebind class ("invalid connection", "i/o timeout",
// "broken pipe", "dial tcp", "unexpected EOF", "use of closed network
// connection"). Retrying the same handle is pointless: its *sql.DB pool points
// at the killed server's port, so each retry first reconnects via the injected
// reopen hook, which re-resolves the CURRENT managed Dolt port (restarting the
// server if needed) and returns a fresh handle bound to the live server.
// Reconnect is single-flight across concurrent readers via the generation guard.
// The loop is deadline-bounded (nativeReadRetryBudget) rather than a fixed
// attempt count so it spans the whole rebind window. Non-transient errors
// (ErrNotFound, decode failures) return immediately, and a store without a
// reopen hook (test handle built directly from a storage value) keeps the prior
// fail-fast behavior.
//
// This closes the gap #4188 left: runBDTransientRead hardened the bd-CLI read
// path (each bd subprocess re-resolves the port and restarts Dolt), but
// factory.go prefers NativeDoltStore when native preflight passes, and that
// long-lived provider-store handle had no equivalent recovery — so a rig store's
// reconcile scan / Get surfaced "begin read tx: dial tcp <old-port>: i/o
// timeout" after a managed-Dolt rebind instead of recovering.
func (s *NativeDoltStore) withReadRetry(fn func(context.Context, beadslib.Storage) error) error {
	if s == nil {
		return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	budget := nativeReadRetryBudget
	if s.readRetryBudgetOverride > 0 {
		budget = s.readRetryBudgetOverride
	}
	// One wall-clock context bounds the WHOLE chain — the read, the reconnect
	// (env re-resolution + recovery + reopen), the retried read, and the backoff.
	// Every step derives its deadline from this ctx and is canceled in-flight
	// when it expires, so the total cannot stack per-call timeouts past budget.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for {
		storage, gen, release, err := s.acquireStorageGen()
		if err != nil {
			return err
		}
		opErr := fn(ctx, storage)
		release()
		if opErr == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nativeReadRetryBudgetError(ctxErr, opErr)
		}
		reopen, closed := s.reopenState()
		if closed {
			return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
		}
		if !isNativeDoltTransientReadError(opErr) || reopen == nil {
			return opErr
		}
		if rcErr := s.reconnect(ctx, gen); rcErr != nil {
			reconnectErr := fmt.Errorf("native Dolt reconnect after transient read error (%w): %w", opErr, rcErr)
			// A reconnect that itself fails transiently (server mid-restart) is
			// worth another pass while the budget remains; a non-transient
			// reconnect failure or an exhausted budget is terminal.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nativeReadRetryBudgetError(ctxErr, reconnectErr)
			}
			if !isNativeDoltTransientReadError(rcErr) {
				return reconnectErr
			}
		}
		// Cancellable backoff: budget expiry during the wait aborts the chain
		// instead of sleeping past the wall.
		select {
		case <-ctx.Done():
			return nativeReadRetryBudgetError(ctx.Err(), opErr)
		case <-time.After(nativeReadRetryBackoff):
		}
	}
}

func nativeReadRetryBudgetError(ctxErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("native Dolt read retry budget exhausted: %w", ctxErr)
	}
	return fmt.Errorf("native Dolt read retry budget exhausted (%w), last error: %w", ctxErr, lastErr)
}

// reopenState returns the reconnect hook and terminal-close state atomically.
func (s *NativeDoltStore) reopenState() (NativeReopenFunc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reopen, s.closed
}

// acquireReconnectGate waits for the single reconnect token or the caller's
// deadline. Lazy initialization keeps zero-value test stores safe without a
// second constructor-only invariant.
func (s *NativeDoltStore) acquireReconnectGate(ctx context.Context) (chan struct{}, error) {
	if s == nil {
		return nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	s.mu.Lock()
	if s.reconnectGate == nil {
		s.reconnectGate = make(chan struct{}, 1)
		s.reconnectGate <- struct{}{}
	}
	gate := s.reconnectGate
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
		if err := ctx.Err(); err != nil {
			gate <- struct{}{}
			return nil, err
		}
		return gate, nil
	}
}

func (s *NativeDoltStore) releaseReconnectGate(gate chan struct{}) {
	gate <- struct{}{}
}

// nativeDoltTransientReadErrorSignatures are the substrings that mark a native
// read failure as a transient managed-Dolt connection error worth reconnecting
// and retrying for. It mirrors and extends the bd read path's connection-error
// set (#4188) with the mysql/net signatures a :3307 hard-kill/rebind emits.
var nativeDoltTransientReadErrorSignatures = []string{
	"invalid connection",
	"bad connection",
	"connection reset",
	"broken pipe",
	"i/o timeout",
	"dial tcp",
	"unexpected eof",
	"use of closed network connection",
	"connection refused",
}

// isNativeDoltTransientReadError reports whether err is a transient managed-Dolt
// connection error worth reconnecting-and-retrying for.
func isNativeDoltTransientReadError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range nativeDoltTransientReadErrorSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// reconnect swaps the dead storage handle for a freshly opened one after a
// transient connection failure, single-flighted so concurrent readers reconnect
// once. observedGen is the generation the failing read ran under; if another
// reader already reconnected (generation advanced), this is a no-op and the
// caller simply retries against the new handle. The fresh handle comes from the
// injected reopen hook, which re-resolves the CURRENT managed port (the cached
// open env pins the old, now-dead port) and re-opens against the live server.
func (s *NativeDoltStore) reconnect(ctx context.Context, observedGen uint64) error {
	gate, err := s.acquireReconnectGate(ctx)
	if err != nil {
		return err
	}
	defer s.releaseReconnectGate(gate)

	s.mu.RLock()
	curGen := s.generation
	closed := s.closed
	old := s.storage
	reopen := s.reopen
	s.mu.RUnlock()

	if closed || reopen == nil {
		return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	if curGen != observedGen {
		return nil // another reader already reconnected
	}

	// The reopen hook re-resolves the current managed port and re-opens under the
	// caller's wall context, so a stuck env-resolution/recovery is canceled at
	// the budget rather than running under its own separate timeout.
	fresh, err := reopen(ctx)
	if err != nil {
		closeStorageQuietly(fresh)
		return err
	}
	if fresh == nil {
		return fmt.Errorf("native Dolt reopen returned nil storage")
	}

	s.mu.Lock()
	// Void the install if the store was closed while we were reopening (terminal
	// latch — never resurrect a closed store) or another reader reconnected first
	// (generation advanced). Either way the fresh handle is discarded, not leaked.
	if s.closed {
		s.mu.Unlock()
		closeStorageQuietly(fresh)
		return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	if s.generation != observedGen {
		s.mu.Unlock()
		closeStorageQuietly(fresh)
		return nil
	}
	s.storage = fresh
	s.generation++
	s.mu.Unlock()

	closeStorageQuietly(old)
	return nil
}

// closeStorageQuietly closes a (possibly dead) storage handle without blocking
// the caller: a handle whose server was hard-killed can wedge on Close, so it is
// closed on a detached goroutine and any error is ignored. The handle is
// unreferenced by the time this is called (the swap took the write lock, so no
// reader still holds it), making the detached close safe.
func closeStorageQuietly(storage beadslib.Storage) {
	if storage == nil {
		return
	}
	go func() { _ = storage.Close() }()
}

// CloseStore permanently releases the underlying native beads storage handle.
// It is a one-way terminal latch that must win any race with an in-flight
// reconnect: after it returns no reconnect may install a fresh handle (which
// would resurrect a closed store and leak a live Dolt connection).
func (s *NativeDoltStore) CloseStore() error {
	if s == nil {
		return nil
	}
	// Phase 1 — latch closed immediately under mu before waiting on the reconnect
	// gate, so a reconnect currently blocked in its reopen observes the close on
	// its post-reopen re-check, while new operations fail with ErrStoreClosed.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Phase 2 — serialize the teardown with any in-flight reconnect,
	// then advance generation + drop the storage handle + drop the reopen hook
	// atomically under mu. Combined with the phase-1 latch, no fresh handle can be
	// installed after this point.
	gate, err := s.acquireReconnectGate(context.Background())
	if err != nil {
		return err
	}
	s.mu.Lock()
	storage := s.storage
	s.storage = nil
	s.reopen = nil
	s.generation++
	s.mu.Unlock()
	s.releaseReconnectGate(gate)

	if storage == nil {
		return nil
	}
	return storage.Close()
}

// ApplyGraphPlan creates a bead graph atomically through the native beads
// storage layer.
func (s *NativeDoltStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage creates a bead graph atomically in the selected
// storage tier through the native beads storage layer.
func (s *NativeDoltStore) ApplyGraphPlanWithStorage(parent context.Context, plan *GraphApplyPlan, storageClass StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storageClass)
	if err != nil {
		return nil, fmt.Errorf("native graph apply: %w", err)
	}
	if err := validateGraphApplyPlan(plan); err != nil {
		return nil, fmt.Errorf("native graph apply: %w", err)
	}

	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, nativeGraphApplyDeadline(plan))
	defer cancel()

	keyToID := make(map[string]string, len(plan.Nodes))
	commitMsg := plan.CommitMessage
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("gc: graph-apply %d nodes", len(plan.Nodes))
	}

	if err := storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
		issues := make([]*beadslib.Issue, 0, len(plan.Nodes))
		pendingAssignees := make(map[int]string)

		for i, node := range plan.Nodes {
			metadata, err := metadataRawFromMap(node.Metadata)
			if err != nil {
				return fmt.Errorf("node %q: marshaling metadata: %w", node.Key, err)
			}
			issueType := beadslib.IssueType(node.Type)
			if issueType == "" {
				issueType = beadslib.TypeTask
			}
			priority := 2
			if node.Priority != nil {
				priority = *node.Priority
			}
			issue := &beadslib.Issue{
				Title:       node.Title,
				Description: node.Description,
				Status:      beadslib.StatusOpen,
				Priority:    priority,
				IssueType:   issueType,
				Sender:      node.From,
				Labels:      append([]string(nil), node.Labels...),
				Metadata:    metadata,
				Ephemeral:   ephemeral,
				NoHistory:   noHistory,
			}
			if node.Assignee != "" {
				if node.AssignAfterCreate {
					pendingAssignees[i] = node.Assignee
				} else {
					issue.Assignee = node.Assignee
				}
			}
			issues = append(issues, issue)
		}

		if err := tx.CreateIssues(ctx, issues, s.actor); err != nil {
			return fmt.Errorf("batch create: %w", err)
		}
		for i, node := range plan.Nodes {
			keyToID[node.Key] = issues[i].ID
		}

		for i, node := range plan.Nodes {
			if len(node.MetadataRefs) == 0 {
				continue
			}
			mergedMeta, err := metadataMapFromNative(issues[i].Metadata)
			if err != nil {
				return fmt.Errorf("node %q: re-parsing metadata: %w", node.Key, err)
			}
			if mergedMeta == nil {
				mergedMeta = make(map[string]string, len(node.MetadataRefs))
			}
			for metaKey, refKey := range node.MetadataRefs {
				mergedMeta[metaKey] = keyToID[refKey]
			}
			raw, err := metadataRawFromMap(mergedMeta)
			if err != nil {
				return fmt.Errorf("node %q: marshaling updated metadata: %w", node.Key, err)
			}
			if err := tx.UpdateIssue(ctx, issues[i].ID, map[string]interface{}{"metadata": raw}, s.actor); err != nil {
				return fmt.Errorf("node %q: updating metadata refs: %w", node.Key, err)
			}
		}

		parentDepPairs := nativeGraphApplyParentDepPairs(plan.Nodes, keyToID)
		for i, edge := range plan.Edges {
			fromID := nativeGraphApplyResolveRef(edge.FromKey, edge.FromID, keyToID)
			toID := nativeGraphApplyResolveRef(edge.ToKey, edge.ToID, keyToID)
			depType := nativeGraphApplyDependencyType(edge.Type)
			if parentDepPairs[nativeGraphApplyDepPairKey(fromID, toID)] {
				if depType == beadslib.DepParentChild {
					continue
				}
				return fmt.Errorf("edge %d %s->%s duplicates a parent-child relationship with dependency type %q", i, fromID, toID, depType)
			}
			if parentDepPairs[nativeGraphApplyDepPairKey(toID, fromID)] && nativeGraphApplyCycleRelevantDependencyType(depType) {
				return fmt.Errorf("edge %d %s->%s creates a blocking reverse of a parent-child relationship", i, fromID, toID)
			}
			dep := &beadslib.Dependency{
				IssueID:     fromID,
				DependsOnID: toID,
				Type:        depType,
				Metadata:    edge.Metadata,
			}
			if err := tx.AddDependency(ctx, dep, s.actor); err != nil {
				return fmt.Errorf("adding edge %s->%s: %w", fromID, toID, err)
			}
		}

		for i, node := range plan.Nodes {
			parentID := node.ParentID
			if node.ParentKey != "" {
				parentID = keyToID[node.ParentKey]
			}
			if parentID == "" {
				continue
			}
			dep := &beadslib.Dependency{
				IssueID:     issues[i].ID,
				DependsOnID: parentID,
				Type:        beadslib.DepParentChild,
			}
			if err := tx.AddDependency(ctx, dep, s.actor); err != nil {
				return fmt.Errorf("node %q: adding parent-child dep: %w", node.Key, err)
			}
		}

		for i, assignee := range pendingAssignees {
			if err := tx.UpdateIssue(ctx, issues[i].ID, map[string]interface{}{"assignee": assignee}, s.actor); err != nil {
				return fmt.Errorf("node %q: setting assignee: %w", plan.Nodes[i].Key, err)
			}
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("native graph apply: %w", err)
	}

	result := &GraphApplyResult{IDs: keyToID}
	if err := ValidateGraphApplyResult(plan, result); err != nil {
		return nil, fmt.Errorf("native graph apply: %w", err)
	}
	return result, nil
}

// SupportsEphemeralGraphApply reports whether this store can apply a whole
// graph directly into ephemeral storage.
func (s *NativeDoltStore) SupportsEphemeralGraphApply() bool {
	return true
}

// Create persists a new bead through the upstream beads storage layer.
func (s *NativeDoltStore) Create(b Bead) (Bead, error) {
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	pendingDependencies := cloneNativeDependencies(issue.Dependencies)
	if err := s.validateCreatedDependencies(ctx, storage, issue.ID, pendingDependencies); err != nil {
		return Bead{}, err
	}
	if err := storage.CreateIssue(ctx, issue, s.actor); err != nil {
		return Bead{}, err
	}
	createdDependencies, err := s.persistCreatedDependencies(ctx, storage, issue.ID, pendingDependencies)
	if err != nil {
		cleanupCtx, cleanupCancel := nativeDoltCleanupContext()
		cleanupErr := s.compensateFailedCreate(cleanupCtx, storage, issue.ID, createdDependencies)
		cleanupCancel()
		if cleanupErr != nil {
			return Bead{}, errors.Join(err, cleanupErr)
		}
		return Bead{}, err
	}
	issue.Dependencies = createdDependencies
	return beadFromNativeIssue(issue)
}

// Get retrieves a bead by ID from the upstream beads storage layer.
func (s *NativeDoltStore) Get(id string) (Bead, error) {
	var out Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		issues, err := storage.SearchIssues(ctx, "", beadslib.IssueFilter{
			IDs:                 []string{id},
			IncludeDependencies: true,
		})
		if err != nil {
			return nativeStoreError(id, err)
		}
		for _, issue := range issues {
			if issue != nil && issue.ID == id {
				bead, err := beadFromNativeIssue(issue)
				if err != nil {
					return err
				}
				out = bead
				return nil
			}
		}
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	})
	return out, err
}

// Update modifies an existing bead through the upstream beads storage layer.
func (s *NativeDoltStore) Update(id string, opts UpdateOpts) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	// Retry a lost serialization race rather than surfacing it to the caller:
	// a concurrent writer to the same bead store is normal (the supervisor,
	// the reconciler and an operator request all write during city startup),
	// and without this an ordinary conflict fails the write permanently and
	// reaches the API as a 500.
	err = retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: update bead %s", id), func(tx beadslib.Transaction) error {
			return s.applyUpdateInTx(ctx, tx, id, opts)
		})
	})
	if err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// applyUpdateInTx applies an Update against an open beadslib transaction. It is
// shared by the standalone Update (one op, one commit) and the multi-write
// Store.Tx path (many ops, one commit) so both routes have identical semantics.
func (s *NativeDoltStore) applyUpdateInTx(ctx context.Context, tx beadslib.Transaction, id string, opts UpdateOpts) error {
	if opts.ParentID != nil {
		if err := s.validateUpdateParent(ctx, tx, *opts.ParentID); err != nil {
			return err
		}
	}
	updates, err := s.nativeUpdates(ctx, tx, id, opts)
	if err != nil {
		return err
	}
	if len(updates) > 0 {
		if err := tx.UpdateIssue(ctx, id, updates, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
	}
	for _, label := range opts.Labels {
		if err := tx.AddLabel(ctx, id, label, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
	}
	for _, label := range opts.RemoveLabels {
		if err := tx.RemoveLabel(ctx, id, label, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
	}
	if opts.ParentID != nil {
		if err := s.updateParentInTransaction(ctx, tx, id, *opts.ParentID); err != nil {
			return err
		}
	}
	return nil
}

// applySetMetadataBatchInTx merges metadata onto a bead within an open
// transaction. Mirrors SetMetadataBatch, sharing the read-modify-write path so
// the Store.Tx route coalesces with sibling writes into a single commit.
func (s *NativeDoltStore) applySetMetadataBatchInTx(ctx context.Context, tx beadslib.Transaction, id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	issue, err := tx.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if issue == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
	}
	if metadata == nil {
		metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		metadata[k] = v
	}
	raw, err := metadataRawFromMap(metadata)
	if err != nil {
		return err
	}
	return nativeStoreError(id, tx.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor))
}

// applyCloseInTx closes a bead within an open transaction, mirroring Close.
// Closing an already-closed bead is a no-op; a missing bead is ErrNotFound.
func (s *NativeDoltStore) applyCloseInTx(ctx context.Context, tx beadslib.Transaction, id string) error {
	current, err := tx.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	reason := nativeCloseReasonFromIssue(current)
	return nativeStoreError(id, tx.CloseIssue(ctx, id, reason, s.actor, ""))
}

// applyCreateInTx creates a bead and its dependencies within an open
// transaction. Unlike the standalone Create, no compensation is needed: a
// mid-create failure rolls the whole transaction back.
func (s *NativeDoltStore) applyCreateInTx(ctx context.Context, tx beadslib.Transaction, b Bead) (Bead, error) {
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	deps := cloneNativeDependencies(issue.Dependencies)
	issue.Dependencies = nil
	if err := tx.CreateIssue(ctx, issue, s.actor); err != nil {
		return Bead{}, err
	}
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		persisted := *dep
		if strings.TrimSpace(persisted.IssueID) == "" {
			persisted.IssueID = issue.ID
		}
		if err := tx.AddDependency(ctx, &persisted, s.actor); err != nil {
			return Bead{}, fmt.Errorf("persisting native create dependency %q -> %q: %w", persisted.IssueID, persisted.DependsOnID, nativeStoreError(persisted.IssueID, err))
		}
	}
	issue.Dependencies = deps
	return beadFromNativeIssue(issue)
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still
// has the expected assignee inside one native Dolt transaction.
func (s *NativeDoltStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	released := false
	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: release bead %s if current", id), func(tx beadslib.Transaction) error {
		issue, err := tx.GetIssue(ctx, id)
		if err != nil {
			err = nativeStoreError(id, err)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if issue == nil || issue.Status != beadslib.StatusInProgress || issue.Assignee != expectedAssignee {
			return nil
		}
		if err := tx.UpdateIssue(ctx, id, map[string]interface{}{
			"status":   "open",
			"assignee": "",
		}, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
		released = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// Close sets a bead's status to closed through the upstream beads storage layer.
func (s *NativeDoltStore) Close(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()

	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return s.closeOnce(ctx, storage, id)
	})
}

// closeOnce performs one complete close attempt, read included. A retry calls
// this whole operation rather than just the write, so every attempt decides
// from freshly read state.
//
// Replaying the write is safe because a serialization conflict guarantees the
// write did not land, and because every CloseIssue path commits exactly once.
// There are three such paths and they do not share a mechanism, so the property
// is stated per branch rather than asserted for "both backends":
//
//   - dolt.DoltStore, permanent bead: withRetryTx/withWriteTx (issues.go
//     CloseIssue).
//   - dolt.DoltStore, active wisp: CloseIssue branches to closeWisp, which uses
//     a bare BeginTx/Commit with a deferred Rollback and deliberately no
//     withRetryTx. Its own comment says not to add one, which is precisely why
//     the retry belongs out here: that path has no internal retry at all.
//   - embeddeddolt.EmbeddedDoltStore: one withConn transaction.
//
// Single-commit is the whole argument, so contrast it with a real two-commit
// caller rather than a guessed one: beadslib's RunInTransaction commits the
// regular tx and the ignored tx separately and can fail after the first landed,
// which is why its callers cannot simply replay. SetMetadataBatch is NOT such a
// caller despite the shape of its name; it calls storage.UpdateIssue directly,
// which branches the same three ways Close does: permanent Dolt under
// withRetryTx, active wisps through updateWisp's bare BeginTx/Commit, and
// embedded under withConn. Close has no two-commit window on any branch.
//
// The re-read is what makes an attempt correct in the presence of OTHER
// writers, which is a live case rather than a hypothetical: the retry sleeps
// between attempts, so a concurrent actor can close the bead in that gap. The
// short-circuit returns nil instead of issuing a redundant CloseIssue, and
// recomputing the reason per attempt keeps it consistent with the state the
// attempt actually observed.
func (s *NativeDoltStore) closeOnce(ctx context.Context, storage beadslib.Storage, id string) error {
	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	reason := nativeCloseReasonFromIssue(current)
	if err := storage.CloseIssue(ctx, id, reason, s.actor, ""); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// Reopen sets a closed bead's status back to open.
func (s *NativeDoltStore) Reopen(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()

	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return s.reopenOnce(ctx, storage, id)
	})
}

// reopenOnce is closeOnce's mirror and is safe to replay for the same reason:
// a serialization conflict means the write did not land, and the re-read
// short-circuits an already-open bead. The two short-circuits are separate
// lines testing separate statuses, so each is proven by its own test rather
// than by symmetry with the other.
//
// The empty reason below is load-bearing, not incidental. beadslib's
// ReopenIssue performs UpdateIssue and then, ONLY when reason is non-empty, a
// separate AddComment in its own transaction. Passing a real reason would
// therefore split this into two independent writes and reintroduce exactly the
// window this function does not otherwise have: if the update commits and the
// comment conflicts, the replay re-reads, sees StatusOpen, short-circuits, and
// the comment is silently dropped. Anything that starts passing a reason has to
// move the comment inside the retried unit or make its loss explicit.
func (s *NativeDoltStore) reopenOnce(ctx context.Context, storage beadslib.Storage, id string) error {
	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if current.Status == beadslib.StatusOpen {
		return nil
	}
	return nativeStoreError(id, storage.ReopenIssue(ctx, id, "", s.actor))
}

// CloseAll closes multiple beads and sets metadata on each newly closed bead.
func (s *NativeDoltStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		current, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if current.Status == "closed" {
			continue
		}
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return closed, err
			}
		}
		if err := s.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *NativeDoltStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	var out []Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		filter := nativeIssueFilterFromListQuery(query)
		issues, err := storage.SearchIssues(ctx, "", filter)
		if err != nil {
			return err
		}
		beads := make([]Bead, 0, len(issues))
		for _, issue := range issues {
			bead, err := beadFromNativeIssue(issue)
			if err != nil {
				if isNativeIssueMetadataParseError(err) {
					continue
				}
				return err
			}
			beads = append(beads, bead)
		}
		out = ApplyListQuery(beads, query)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListOpen returns non-closed beads by default, or beads with the given status.
func (s *NativeDoltStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable beads.
func (s *NativeDoltStore) Ready(queries ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(queries)
	var out []Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		var beads []Bead
		seen := make(map[string]bool)
		now := time.Now().UTC()
		// One GetReadyWork call covers every open-class backing status via
		// WorkFilter.Statuses. The previous one-call-per-status loop re-paid
		// the deferred-parents pre-query, the wisp arm, and the transaction
		// round trips seven times per Ready() (sr-5rz: ~30-70ms per call on
		// a live server-mode store). The backing Limit stays 0 because the
		// gc-side post-filter below (tier, excluded types/labels, defer)
		// discards rows the store cannot, so a server-side limit could
		// under-fill the result.
		filter := beadslib.WorkFilter{Statuses: nativeDoltOpenReadyStatuses}
		if q.TierMode == TierBoth || q.TierMode == TierWisps {
			filter.IncludeEphemeral = true
		}
		if q.Assignee != "" {
			filter.Assignee = &q.Assignee
		}
		issues, err := storage.GetReadyWork(ctx, filter)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			// The StatusDeferred branch exists so an expired time-bound
			// deferral (defer_until in the past) can resurface. An issue
			// with no defer_until at all was never time-bound — it's bd
			// defer's status-based indefinite deferral — and must stay
			// hidden. beadFromNativeIssue now records that case as
			// Bead.IndefinitelyDeferred and IsDeferred honors it, so the
			// readiness filter already excludes such a row; this skip keeps
			// it from being materialized at all. The per-status loop keyed
			// this on the filter status it was querying for; with the whole
			// set in one call the row's own raw status is the equivalent
			// discriminator.
			if issue.Status == beadslib.StatusDeferred && issue.DeferUntil == nil {
				continue
			}
			bead, err := beadFromNativeIssue(issue)
			if err != nil {
				return err
			}
			if !IsReadyCandidateForTier(bead, now, q.TierMode) || seen[bead.ID] {
				continue
			}
			seen[bead.ID] = true
			beads = append(beads, bead)
			if q.Limit > 0 && len(beads) >= q.Limit {
				break
			}
		}
		out = beads
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Children returns all beads whose parent-child dependency points at parentID.
func (s *NativeDoltStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// WaitForParentProjection blocks until native dependency queries reflect a
// successful reparent from oldParentID to newParentID for id.
func (s *NativeDoltStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	ticker := time.NewTicker(bdParentProjectionPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		current, err := s.Get(id)
		if err == nil {
			switch current.ParentID {
			case newParentID:
				matches, matchErr := s.parentProjectionMatches(id, oldParentID, newParentID)
				if matchErr == nil && matches {
					return nil
				}
				lastErr = matchErr
			case oldParentID:
				lastErr = nil
			default:
				return fmt.Errorf("updating bead %q: %w", id, ErrParentProjectionSuperseded)
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w (last check error: %w)", id, oldParentID, newParentID, ctx.Err(), lastErr)
			}
			return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w", id, oldParentID, newParentID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *NativeDoltStore) parentProjectionMatches(id, oldParentID, newParentID string) (bool, error) {
	if oldParentID != "" {
		oldChildren, err := s.Children(oldParentID)
		if err != nil {
			return false, fmt.Errorf("listing old parent %q children: %w", oldParentID, err)
		}
		if beadSliceContains(oldChildren, id) {
			return false, nil
		}
	}
	if newParentID != "" {
		newChildren, err := s.Children(newParentID)
		if err != nil {
			return false, fmt.Errorf("listing new parent %q children: %w", newParentID, err)
		}
		if !beadSliceContains(newChildren, id) {
			return false, nil
		}
	}
	return true, nil
}

// ListByLabel returns beads with an exact label match.
func (s *NativeDoltStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to assignee with the requested status.
func (s *NativeDoltStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true})
}

// ListByMetadata returns beads whose metadata contains all filters.
func (s *NativeDoltStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a single metadata key on a bead.
func (s *NativeDoltStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

const (
	nativeWriteAttempts     = 3
	nativeWriteRetryBackoff = 25 * time.Millisecond
)

// retryOnNativeDoltSerializationConflict runs attempt until it succeeds, fails
// with something other than a Dolt serialization conflict, or exhausts
// nativeWriteAttempts.
//
// Re-running the whole attempt is safe: it re-reads inside a fresh transaction
// and therefore builds on the competing transaction's committed rows rather
// than overwriting them from a stale read. In the conflict this guards against,
// the regular-table commit is the one that loses the race and nothing lands.
// isNativeDoltSerializationConflict classifies on error text and cannot tell
// which of beadslib's commit points failed, so a conflict reported after the
// regular commit already succeeded would replay the attempt; callers must
// therefore keep each attempt idempotent under replay, which the metadata
// merge, label add/remove and reparent operations are.
//
// Each attempt gets its own operation context, so an earlier attempt's deadline
// cannot doom the retries.
//
// Every other error is returned on the first try. Retrying a genuine fault only
// multiplies write load and hides the cause behind a slower failure.
//
// Callers hold the storage read lock across every attempt, so the backoff sleeps
// inside this loop delay anything waiting to take s.mu for writing (store close
// and the reconnect handle swap) by at most the total backoff. Holding it is
// deliberate: releasing between attempts would let the handle be swapped
// mid-retry, so a retry could run against a different storage than the one whose
// transaction it is repeating.
func retryOnNativeDoltSerializationConflict(attempt func() error) error {
	var err error
	for n := 1; n <= nativeWriteAttempts; n++ {
		err = attempt()
		if err == nil || !isNativeDoltSerializationConflict(err) || n == nativeWriteAttempts {
			return err
		}
		time.Sleep(time.Duration(n) * nativeWriteRetryBackoff)
	}
	return err
}

// SetMetadataBatch sets multiple metadata keys on a bead.
func (s *NativeDoltStore) SetMetadataBatch(id string, kvs map[string]string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()

	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return s.setMetadataBatchOnce(ctx, storage, id, kvs)
	})
}

// setMetadataBatchOnce performs one complete metadata read-merge-write attempt.
// A retry must call this whole operation again so metadata committed by the
// competing transaction is included rather than overwritten from a stale read.
func (s *NativeDoltStore) setMetadataBatchOnce(ctx context.Context, storage beadslib.Storage, id string, kvs map[string]string) error {
	issue, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if issue == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
	}
	if metadata == nil {
		metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		metadata[k] = v
	}
	raw, err := metadataRawFromMap(metadata)
	if err != nil {
		return err
	}
	return nativeStoreError(id, storage.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor))
}

// isNativeDoltSerializationConflict reports only Dolt/MySQL transaction
// serialization conflicts, which are known not to have committed and are safe
// to retry. Ambiguous connection failures intentionally remain fail-fast.
func isNativeDoltSerializationConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "error 1213") ||
		strings.Contains(msg, "error 1205") ||
		(strings.Contains(msg, "sqlstate") && strings.Contains(msg, "40001")) ||
		strings.Contains(msg, "(40001)") ||
		strings.Contains(msg, "this transaction conflicts with a committed transaction")
}

// SetLocalString sets a clone-local string value for a bead. See
// Store.SetLocalString. Persisted to a sidecar JSON file under this store's
// .beads/ directory rather than through Dolt storage: unlike SetMetadata,
// this never touches the Dolt DB or commits. Does not validate that id
// refers to an existing bead — see the interface doc comment for why.
func (s *NativeDoltStore) SetLocalString(id, key, value string) error {
	if err := s.localStrings.Set(id, key, value); err != nil {
		return fmt.Errorf("setting local string on %q: %w", id, err)
	}
	return nil
}

// GetLocalString returns the clone-local string value for a bead. See
// Store.GetLocalString.
func (s *NativeDoltStore) GetLocalString(id, key string) (string, error) {
	value, err := s.localStrings.Get(id, key)
	if err != nil {
		return "", fmt.Errorf("getting local string on %q: %w", id, err)
	}
	return value, nil
}

// Tx executes fn inside a single native Dolt transaction so every write in the
// callback shares one DOLT_COMMIT. This is the coalescing path that lets a
// caller (e.g. an extmsg bind) issue several bead writes at the cost of one
// commit instead of one per write.
func (s *NativeDoltStore) Tx(commitMsg string, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	if strings.TrimSpace(commitMsg) == "" {
		commitMsg = "gc: tx"
	}
	return storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
		return fn(&nativeDoltTx{store: s, ctx: ctx, tx: tx})
	})
}

// AtomicTx reports that Tx is backed by a native Dolt transaction that rolls
// back every write when the callback returns an error.
func (s *NativeDoltStore) AtomicTx() bool { return true }

// nativeDoltTx adapts the Store.Tx write surface onto an open beadslib
// transaction. Every method routes through the store's applyXInTx helpers so
// transactional and standalone writes share one implementation.
type nativeDoltTx struct {
	store *NativeDoltStore
	ctx   context.Context
	tx    beadslib.Transaction
}

func (t *nativeDoltTx) Create(b Bead) (Bead, error) {
	return t.store.applyCreateInTx(t.ctx, t.tx, b)
}

func (t *nativeDoltTx) Update(id string, opts UpdateOpts) error {
	if err := t.store.applyUpdateInTx(t.ctx, t.tx, id, opts); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

func (t *nativeDoltTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.store.applySetMetadataBatchInTx(t.ctx, t.tx, id, kvs)
}

func (t *nativeDoltTx) Close(id string) error {
	return t.store.applyCloseInTx(t.ctx, t.tx, id)
}

// Delete permanently removes a bead from the upstream beads storage layer.
func (s *NativeDoltStore) Delete(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	if err := nativeStoreError(id, storage.DeleteIssue(ctx, id)); err != nil {
		return err
	}
	if sidecarErr := s.localStrings.DeleteBead(id); sidecarErr != nil {
		return fmt.Errorf("deleting bead %q: cleaning up local strings: %w", id, sidecarErr)
	}
	return nil
}

// Ping verifies that the upstream storage is reachable.
func (s *NativeDoltStore) Ping() error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	_, err = storage.GetStatistics(ctx)
	return err
}

// DepAdd records a dependency between two beads.
func (s *NativeDoltStore) DepAdd(issueID, dependsOnID, depType string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return nativeStoreError(issueID, storage.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beadslib.DependencyType(depType),
	}, s.actor))
}

// DepRemove removes a dependency between two beads.
func (s *NativeDoltStore) DepRemove(issueID, dependsOnID string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return nativeStoreError(issueID, storage.RemoveDependency(ctx, issueID, dependsOnID, s.actor))
}

// DepList returns dependencies for a bead.
func (s *NativeDoltStore) DepList(id, direction string) ([]Dep, error) {
	var out []Dep
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		deps, err := s.depList(ctx, storage, id, direction)
		if err != nil {
			return err
		}
		out = deps
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *NativeDoltStore) depList(ctx context.Context, storage beadslib.Storage, id, direction string) ([]Dep, error) {
	if direction == "up" {
		issues, err := storage.GetDependentsWithMetadata(ctx, id)
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		deps := make([]Dep, 0, len(issues))
		for _, issue := range issues {
			deps = append(deps, Dep{
				IssueID:     issue.ID,
				DependsOnID: id,
				Type:        string(issue.DependencyType),
			})
		}
		return deps, nil
	}
	issues, err := storage.GetDependenciesWithMetadata(ctx, id)
	if err != nil {
		return nil, nativeStoreError(id, err)
	}
	deps := make([]Dep, 0, len(issues))
	for _, issue := range issues {
		deps = append(deps, Dep{
			IssueID:     id,
			DependsOnID: issue.ID,
			Type:        string(issue.DependencyType),
		})
	}
	return deps, nil
}

type nativeIssueGetter interface {
	GetIssue(context.Context, string) (*beadslib.Issue, error)
}

func (s *NativeDoltStore) nativeUpdates(ctx context.Context, storage nativeIssueGetter, id string, opts UpdateOpts) (map[string]interface{}, error) {
	updates := make(map[string]interface{})
	if opts.Title != nil {
		updates["title"] = *opts.Title
	}
	if opts.Status != nil {
		updates["status"] = *opts.Status
	}
	if opts.Type != nil {
		updates["issue_type"] = *opts.Type
	}
	if opts.Priority != nil {
		updates["priority"] = *opts.Priority
	}
	if opts.Description != nil {
		updates["description"] = *opts.Description
	}
	if opts.Assignee != nil {
		updates["assignee"] = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		issue, err := storage.GetIssue(ctx, id)
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		if issue == nil {
			return nil, fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
		metadata, err := metadataMapFromNative(issue.Metadata)
		if err != nil {
			return nil, fmt.Errorf("parsing metadata for bead %q: %w", id, err)
		}
		if metadata == nil {
			metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			metadata[k] = v
		}
		raw, err := metadataRawFromMap(metadata)
		if err != nil {
			return nil, err
		}
		updates["metadata"] = raw
	}
	return updates, nil
}

func (s *NativeDoltStore) validateUpdateParent(ctx context.Context, storage nativeIssueGetter, parentID string) error {
	if strings.TrimSpace(parentID) == "" {
		return nil
	}
	issue, err := storage.GetIssue(ctx, parentID)
	if err != nil {
		return nativeStoreError(parentID, err)
	}
	if issue == nil {
		return fmt.Errorf("bead %q: %w", parentID, ErrNotFound)
	}
	return nil
}

func (s *NativeDoltStore) updateParentInTransaction(ctx context.Context, tx beadslib.Transaction, id, parentID string) error {
	if strings.TrimSpace(parentID) != "" {
		issue, err := tx.GetIssue(ctx, parentID)
		if err != nil {
			return nativeStoreError(parentID, err)
		}
		if issue == nil {
			return fmt.Errorf("bead %q: %w", parentID, ErrNotFound)
		}
	}
	deps, err := tx.GetDependencyRecords(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	for _, dep := range deps {
		if dep == nil || dep.Type != beadslib.DepParentChild {
			continue
		}
		if err := tx.RemoveDependency(ctx, id, dep.DependsOnID, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
	}
	if parentID == "" {
		return nil
	}
	if err := tx.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     id,
		DependsOnID: parentID,
		Type:        beadslib.DepParentChild,
	}, s.actor); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

func (s *NativeDoltStore) persistCreatedDependencies(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) ([]*beadslib.Dependency, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(issueID) == "" {
		return nil, fmt.Errorf("persisting native create dependencies: upstream create did not assign an issue ID")
	}
	created := make([]*beadslib.Dependency, 0, len(deps))
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		persisted := *dep
		if strings.TrimSpace(persisted.IssueID) == "" {
			persisted.IssueID = issueID
		}
		if err := storage.AddDependency(ctx, &persisted, s.actor); err != nil {
			return created, fmt.Errorf("persisting native create dependency %q -> %q: %w", persisted.IssueID, persisted.DependsOnID, nativeStoreError(persisted.IssueID, err))
		}
		depCopy := persisted
		created = append(created, &depCopy)
	}
	return created, nil
}

func (s *NativeDoltStore) validateCreatedDependencies(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) error {
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		targetID := strings.TrimSpace(dep.DependsOnID)
		if targetID == "" {
			return fmt.Errorf("validating native create dependency for %q: depends_on_id is empty", issueID)
		}
		if !shouldPrevalidateNativeDependency(issueID, targetID, s.idPrefix) {
			continue
		}
		issue, err := storage.GetIssue(ctx, targetID)
		if err != nil {
			return fmt.Errorf("validating native create dependency %q -> %q: %w", issueID, targetID, nativeStoreError(targetID, err))
		}
		if issue == nil {
			return fmt.Errorf("validating native create dependency %q -> %q: bead %q: %w", issueID, targetID, targetID, ErrNotFound)
		}
	}
	return nil
}

func (s *NativeDoltStore) compensateFailedCreate(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) error {
	if strings.TrimSpace(issueID) == "" {
		return nil
	}
	var errs []error
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		if err := storage.RemoveDependency(ctx, issueID, dep.DependsOnID, s.actor); err != nil {
			errs = append(errs, fmt.Errorf("removing partial native dependency %q -> %q: %w", issueID, dep.DependsOnID, nativeStoreError(issueID, err)))
		}
	}
	if err := storage.DeleteIssue(ctx, issueID); err != nil {
		errs = append(errs, fmt.Errorf("deleting partial native issue %q: %w", issueID, nativeStoreError(issueID, err)))
	}
	return errors.Join(errs...)
}

func nativeCloseReasonFromIssue(issue *beadslib.Issue) string {
	if issue == nil {
		return ""
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(metadata["close_reason"])
}

func shouldPrevalidateNativeDependency(issueID, targetID, storePrefix string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(targetID)), "external:") {
		return false
	}
	sourcePrefix := nativeBeadIDPrefix(issueID)
	if sourcePrefix == "" {
		sourcePrefix = normalizeIDPrefix(storePrefix)
	}
	targetPrefix := nativeBeadIDPrefix(targetID)
	return sourcePrefix == "" || targetPrefix == "" || sourcePrefix == targetPrefix
}

func nativeBeadIDPrefix(id string) string {
	before, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(id)), "-")
	if !ok {
		return ""
	}
	return normalizeIDPrefix(before)
}

func nativeGraphApplyDependencyType(depType string) beadslib.DependencyType {
	if depType == "" {
		return beadslib.DepBlocks
	}
	return beadslib.DependencyType(depType)
}

func nativeGraphApplyCycleRelevantDependencyType(depType beadslib.DependencyType) bool {
	return depType == beadslib.DepBlocks || depType == beadslib.DepConditionalBlocks
}

func nativeGraphApplyParentDepPairs(nodes []GraphApplyNode, keyToID map[string]string) map[string]bool {
	pairs := make(map[string]bool)
	for _, node := range nodes {
		childID := keyToID[node.Key]
		parentID := node.ParentID
		if node.ParentKey != "" {
			parentID = keyToID[node.ParentKey]
		}
		if childID != "" && parentID != "" {
			pairs[nativeGraphApplyDepPairKey(childID, parentID)] = true
		}
	}
	return pairs
}

func nativeGraphApplyDepPairKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}

func nativeGraphApplyResolveRef(key, id string, keyToID map[string]string) string {
	if id != "" {
		return id
	}
	if key != "" {
		return keyToID[key]
	}
	return ""
}

func cloneNativeDependencies(deps []*beadslib.Dependency) []*beadslib.Dependency {
	if len(deps) == 0 {
		return nil
	}
	cloned := make([]*beadslib.Dependency, 0, len(deps))
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		depCopy := *dep
		cloned = append(cloned, &depCopy)
	}
	return cloned
}

func nativeIssueFromBead(b Bead) (*beadslib.Issue, error) {
	status := b.Status
	if status == "" {
		status = "open"
	}
	issueType := b.Type
	if issueType == "" {
		issueType = "task"
	}
	issue := &beadslib.Issue{
		ID:          b.ID,
		Title:       b.Title,
		Description: b.Description,
		Status:      beadslib.Status(status),
		IssueType:   beadslib.IssueType(issueType),
		Assignee:    b.Assignee,
		Sender:      b.From,
		CreatedAt:   b.CreatedAt,
		Labels:      append([]string(nil), b.Labels...),
		Ephemeral:   b.Ephemeral,
		NoHistory:   b.NoHistory,
		DeferUntil:  cloneTimePtr(b.DeferUntil),
		RowVersion:  b.Revision,
	}
	if b.Priority != nil {
		issue.Priority = *b.Priority
	} else {
		issue.Priority = 2
	}
	raw, err := metadataRawFromMap(b.Metadata)
	if err != nil {
		return nil, err
	}
	issue.Metadata = raw
	for _, dep := range b.Dependencies {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        beadslib.DependencyType(dep.Type),
		})
	}
	if b.ParentID != "" {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: b.ParentID,
			Type:        beadslib.DepParentChild,
		})
	}
	for _, need := range b.Needs {
		depType := "blocks"
		dependsOnID := need
		if before, after, ok := strings.Cut(need, ":"); ok && before != "" && after != "" {
			depType = before
			dependsOnID = after
		}
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: dependsOnID,
			Type:        beadslib.DependencyType(depType),
		})
	}
	return issue, nil
}

func beadFromNativeIssue(issue *beadslib.Issue) (Bead, error) {
	if issue == nil {
		return Bead{}, nil
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return Bead{}, fmt.Errorf("parsing metadata for bead %q: %w: %w", issue.ID, errNativeIssueMetadataParse, err)
	}
	status, indefinitelyDeferred := normalizedBdReadState(string(issue.Status), issue.DeferUntil)
	b := Bead{
		ID:                   issue.ID,
		Title:                issue.Title,
		Status:               status,
		Type:                 string(issue.IssueType),
		Priority:             nativePriorityFromIssue(issue),
		CreatedAt:            issue.CreatedAt,
		Assignee:             issue.Assignee,
		From:                 issue.Sender,
		Description:          issue.Description,
		Labels:               append([]string(nil), issue.Labels...),
		Metadata:             metadata,
		Ephemeral:            issue.Ephemeral,
		NoHistory:            issue.NoHistory,
		DeferUntil:           cloneTimePtr(issue.DeferUntil),
		IndefinitelyDeferred: indefinitelyDeferred,
		Revision:             issue.RowVersion,
	}
	for _, dep := range issue.Dependencies {
		if dep == nil {
			continue
		}
		converted := Dep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		}
		b.Dependencies = append(b.Dependencies, converted)
		if dep.Type == beadslib.DepParentChild && b.ParentID == "" {
			b.ParentID = dep.DependsOnID
		}
	}
	return b, nil
}

func isNativeIssueMetadataParseError(err error) bool {
	return errors.Is(err, errNativeIssueMetadataParse)
}

func nativePriorityFromIssue(issue *beadslib.Issue) *int {
	// Upstream beads stores omitted priority as P2. Gas City's Store surface
	// represents that unset/default state as nil, matching BdStore's sparse
	// JSON decode semantics for callers that distinguish unset from explicit.
	if issue.Priority == 2 {
		return nil
	}
	priority := issue.Priority
	return &priority
}

// nativeCreatedLimitPushdown reports the row limit to forward to the backing
// search for a ListQuery, or 0 to fetch the full candidate set and let
// ApplyListQuery cut the exact page client-side. Created-order sorts push down
// to the backing search (IssueFilter.SortBy drives sqlbuild.OrderBy) so the
// caller's limit survives and the store pages instead of materializing +
// hydrating the whole corpus (sr-dp9o: the dispatcher's RecentRunsAll(2048) was
// scanning ~22k closed order-tracking wisps per call with the limit stripped).
// A backing limit is exact only when the backing's ordering and tie-break match
// the query's client-side semantics; the guards below keep every shape whose
// exact result needs client-side work from truncating the page early.
func nativeCreatedLimitPushdown(query ListQuery) int {
	if query.Limit <= 0 {
		return 0
	}
	// The wisp tier still needs the gc-side post-filter over the full candidate
	// set (it can discard rows), so a backing limit would cut the page short.
	if query.TierMode == TierWisps {
		return 0
	}
	// SeekAfter, UpdatedBefore, and plural Assignees are enforced only Go-side in
	// ApplyListQuery (q.Matches); they are not pushed to the backing search, so a
	// backing limit applied before them would cut rows before the residual filter
	// runs and silently drop page rows. Fetch the full candidate set for those
	// shapes, mirroring the sibling gates (doltliteCanSelectBoundedTopN,
	// exec.go, bdstore canApplyWispsServerLimit).
	if query.SeekAfter != nil || !query.UpdatedBefore.IsZero() || len(query.Assignees) > 0 {
		return 0
	}
	switch query.Sort {
	case SortCreatedAsc:
		// The backing renders created-asc ties as `id ASC`, matching the
		// canonical (created_at ASC, id ASC) order, so a bounded asc read is exact.
		return query.Limit
	case SortCreatedDesc:
		// The backing renders created-desc ties as `id ASC` (upstream
		// sqlbuild.OrderBy hardcodes the id tie-break), but Gas City's canonical
		// order and cursor continuation break created_at ties by `id DESC`
		// (sortBeadsForQuery / SeekBoundary.After). A bounded desc read therefore
		// keeps the smaller-id tie members at the boundary and drops the larger-id
		// ties, so an exact or cursor-paginated caller loses rows across the page
		// seam. Only push the limit when the caller opted into a bounded
		// newest-by-created_at sample (aggregates); otherwise fetch the full set
		// and let ApplyListQuery cut the exact (created_at DESC, id DESC) prefix.
		if query.AllowBackingCreatedLimit {
			return query.Limit
		}
		return 0
	case SortDefault:
		// The default backing order (priority, created_at DESC, id ASC) is
		// deterministic, so a bounded default read cuts a stable prefix.
		return query.Limit
	default:
		// Non-mappable sorts can't page server-side; fetch unbounded and sort
		// client-side in ApplyListQuery.
		return 0
	}
}

func nativeIssueFilterFromListQuery(query ListQuery) beadslib.IssueFilter {
	var sortBy string
	var sortDesc bool
	switch query.Sort {
	case SortCreatedDesc:
		sortBy, sortDesc = "created", false // SortDefs["created"] defaults DESC
	case SortCreatedAsc:
		sortBy, sortDesc = "created", true // flip the DESC default
	}
	filter := beadslib.IssueFilter{
		Limit:               nativeCreatedLimitPushdown(query),
		SortBy:              sortBy,
		SortDesc:            sortDesc,
		MetadataFields:      query.Metadata,
		CreatedBefore:       zeroTimePtr(query.CreatedBefore),
		IncludeDependencies: true,
	}
	switch query.TierMode {
	case TierWisps:
		// Upstream can filter only ephemeral rows, while Gas City's wisp tier
		// includes both ephemeral and no-history rows. Let ApplyListQuery apply
		// the final tier filter after all candidates are returned.
	case TierBoth:
		// no tier filter
	default:
		ephemeral := false
		filter.Ephemeral = &ephemeral
	}
	if query.Status != "" {
		if query.Status == "open" {
			filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed, beadslib.StatusInProgress}
		} else {
			status := beadslib.Status(query.Status)
			filter.Status = &status
		}
	} else if !query.IncludeClosed {
		filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed}
	}
	if query.Type != "" {
		issueType := beadslib.IssueType(query.Type)
		filter.IssueType = &issueType
	}
	if query.Label != "" {
		filter.Labels = []string{query.Label}
	}
	if query.Assignee != "" {
		filter.Assignee = &query.Assignee
	}
	if query.ParentID != "" {
		filter.ParentID = &query.ParentID
	}
	return filter
}

func nativeStoreError(id string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	if !nativeUpstreamNotFound(err) {
		return err
	}
	if id == "" {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return fmt.Errorf("bead %q: %w: %w", id, ErrNotFound, err)
}

func nativeUpstreamNotFound(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return msg == "not found" ||
		strings.Contains(msg, "not found: issue ") ||
		strings.Contains(msg, "issue not found: ") ||
		((strings.HasPrefix(msg, "issue ") || strings.Contains(msg, " issue ")) && strings.HasSuffix(msg, " not found")) ||
		strings.HasSuffix(msg, ": not found") ||
		msg == "no rows in result set" ||
		strings.HasSuffix(msg, ": no rows in result set")
}

func zeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func metadataRawFromMap(metadata map[string]string) (json.RawMessage, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	return raw, nil
}

func metadataMapFromNative(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}
	metadata := make(map[string]string, len(values))
	for k, v := range values {
		if s, ok := v.(string); ok {
			metadata[k] = s
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata value %q: %w", k, err)
		}
		metadata[k] = string(raw)
	}
	return metadata, nil
}
