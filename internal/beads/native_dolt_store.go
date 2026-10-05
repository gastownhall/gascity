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
	"sync/atomic"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

const nativeDoltStoreActor = "gascity"

// The per-status ready loop this file used to run is gone with the raw
// GetReadyWork door; issueops.Reader publishes no status and workapi's builder
// stamps open for every caller. NativeDoltStore.Ready carries what became of
// each of its two passes, ga-3mv5d3's investigation included.

var (
	nativeDoltOpenBestAvailable = beadslib.OpenBestAvailable
	nativeDoltOpenEnvMu         sync.Mutex
	errNativeIssueMetadataParse = ErrMetadataParse
)

var nativeDoltOpenEnvKeys = []string{
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
// BDAllowRemoteMigrateEnvKey is projected from env as well, so the linked
// library sees it only when the caller decided it.
func withNativeDoltOpenEnvAndCredentialCommandLocked(env map[string]string, credentialCommand string) (func(), error) {
	restoreScoped, err := withProjectedOpenEnvLocked(nativeDoltOpenEnvKeys, env, credentialCommand)
	if err != nil {
		return nil, err
	}
	restoreRemoteMigrate, err := withProjectedOpenEnvLocked(bdRemoteMigrateOpenEnvKeys, env, "")
	if err != nil {
		restoreScoped()
		return nil, err
	}
	return func() {
		restoreRemoteMigrate()
		restoreScoped()
	}, nil
}

// BDAllowRemoteMigrateEnvKey is the beads library's opt-in for letting a
// writable open migrate a shared or remote database's schema forward. A direct
// native open takes it only from the env its caller passes, never from the
// ambient process environment: gc sets it for a city that opted in through
// beads.allow_schema_behind_migrate, the opt-in the native-store preflight
// requires before it passes a database whose schema is behind the library's.
// The proxied lane decides this key itself (proxiedOnlyOpenEnvKeys).
const BDAllowRemoteMigrateEnvKey = "BD_ALLOW_REMOTE_MIGRATE"

// bdRemoteMigrateOpenEnvKeys is the projection list for
// BDAllowRemoteMigrateEnvKey. It stays out of nativeDoltOpenEnvKeys, which
// lists the BEADS_ keys a direct open decides.
var bdRemoteMigrateOpenEnvKeys = []string{BDAllowRemoteMigrateEnvKey}

// withProjectedOpenEnvLocked is the projection itself, parameterised by the key
// list it decides.
//
// The list is a parameter rather than the package-level default because the
// proxied-native window projects a DIFFERENT set (see
// proxiedNativeOpenEnvKeys): it must reach the author pair and explicitly unset
// bd's migration unlocks, neither of which belongs in the list every direct and
// hosted open uses. Growing the shared list instead would change what a
// flag-off open does — a key listed here but absent from env is UNSET, so
// adding GIT_AUTHOR_NAME would silently strip an operator's git identity from
// every direct native open in the process.
func withProjectedOpenEnvLocked(openEnvKeys []string, env map[string]string, credentialCommand string) (func(), error) {
	keys := openEnvKeys
	if credentialCommand != "" {
		keys = append(append([]string(nil), openEnvKeys...), "BEADS_DOLT_CREDENTIAL_COMMAND")
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
//
// It withholds BEADS_ and nothing else, on purpose. Direct and hosted opens are
// the callers, and their contract is unchanged by the proxied lane: a second
// prefix here would alter what every one of them projects.
func withWithheldBeadsEnvLocked() (func(), error) {
	return withWithheldPrefixesLocked(beadsEnvPrefix)
}

// withWithheldPrefixesLocked unsets every ambient variable under any of the
// given prefixes and returns the restore, with nativeDoltOpenEnvMu already
// held.
//
// It is prefix-parameterised rather than fixed at BEADS_ because the
// proxied-native window has a second namespace to answer for: bd's own BD_
// variables configure migration and schema-gate behavior that gc must not
// inherit from whatever shell it was launched from when it opens the linked
// library against a database bd owns. A restore is registered per key before
// any unset fails, so a partial withholding cannot leave the process env in a
// state no caller asked for.
func withWithheldPrefixesLocked(prefixes ...string) (func(), error) {
	type withheld struct{ key, value string }
	var previous []withheld
	restore := func() {
		for _, entry := range previous {
			_ = os.Setenv(entry.key, entry.value)
		}
	}
	hasPrefix := func(key string) bool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
		return false
	}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !hasPrefix(key) {
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

	storage, err := openNativeDoltStorage(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, "", err
	}
	var prefix string
	if readPrefix {
		prefix, err = storage.GetConfig(ctx, nativeIssuePrefixConfigKey)
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
// that clears only the variables gc itself projects. BDAllowRemoteMigrateEnvKey
// is withheld too, so an inherited unlock never migrates the workspace.
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

	// sawRows latches when the backend answered a read of this store with at
	// least one row, counted as the backend returned it and before
	// ApplyListQuery narrows it. It is the RowWitness evidence for this
	// backend; see row_witness.go for what a caller may conclude from it.
	sawRows atomic.Bool

	// poolStale marks a handle whose pooled connections point at a proxy
	// generation that has already been replaced, so the next read must reconnect
	// BEFORE it is served rather than after it fails.
	//
	// It exists for the one hazard a read cannot see: bd's proxy root is moved
	// and recreated at the same path, and the old pooled socket keeps serving the
	// MOVED database without any error at all (design U22). The proxied guard
	// tick notices the generation change from its own goroutine and sets this;
	// the reconnect itself happens on the next READER's goroutine, through the
	// injected reopen hook, because opening the library means mutating the
	// process environment under nativeDoltOpenEnvMu and, while a reader exists
	// to carry it, a background ticker is the wrong place for that.
	//
	// "While a reader exists" is the whole rule, not a hedge (council pr2
	// D-F12). A handle that has already stood down non-terminally has no native
	// leaf, so no read reaches this mark or the reopen hook, and the guard tick
	// DOES open the library there itself — see recoverNative in
	// proxied_guard_tick.go, whose header states the exception and its budget.
	// This mark is only ever the serving handle's mechanism.
	//
	// It is false on every direct and hosted handle — only the proxied guard tick
	// sets it — so the read path's extra atomic load is the whole cost of the
	// mechanism on those lanes.
	//
	// It is an EPOCH rather than a flag (council A-F4). A flag was cleared
	// unconditionally once the reconnect returned, with no compare-and-swap
	// against the mark being serviced, and the window that opens is the exact
	// hazard the mechanism exists for. Tick T1 sees generation A->B, adopts B's
	// pin and marks the pool stale. Reader R enters withReadRetry, sees the
	// mark and calls repinStalePool, which can sit inside the reopen hook for
	// hundreds of milliseconds — the re-admission, the ladder's sleeps, the
	// library open. While R is in there, tick T2 sees B->C, adopts C's pin and
	// marks again; the flag is already true, so nothing happens. R installs
	// B's storage and clears the flag. The handle now serves reads from
	// generation B's socket while the pin says C, and no later tick marks it
	// again because checkGeneration compares the record against pin C and
	// reports Held. On the root-move shape that is silent wrong-database reads
	// for the life of the handle.
	//
	// That epoch was still not enough (council pr2 D-F6): it was cleared with
	// CompareAndSwap(seen, 0), so the counter went back to zero and the swap
	// was an ABA test, not a watermark. Two readers R1 and R2 both load mark 1;
	// R2 reconnects and clears to 0 while R1 is parked on the reconnect gate;
	// tick T2 marks — the counter is 1 AGAIN; R1 wakes, finds another reader
	// already reconnected, and its CAS(1, 0) succeeds and erases T2's mark. The
	// same loss happened when the install that satisfied R1 came from a
	// transient-error reconnect whose reopen began BEFORE the mark.
	//
	// So poolStale is now MONOTONIC — markPoolStale increments it and nothing
	// ever decrements it — and poolServiced is a separate watermark: the mark
	// value snapshotted immediately before the reopen whose storage is now
	// installed began (see reconnect). A re-point is owed exactly while
	// poolStale > poolServiced. A reader no longer clears anything on its own
	// say-so; only an install can advance the watermark, and only to the marks
	// that install's reopen could actually have seen.
	poolStale    atomic.Uint64
	poolServiced atomic.Uint64

	// reservedPrefixes is the pinned-id fence: the id namespaces this store's
	// binding claims. Empty leaves the store unfenced, which is the shipped
	// default everywhere it is not opened as a class binding — including a
	// binding serving the work class, whose beads carry whatever prefix an
	// operator configured. See WithNativeDoltStoreReservedIDPrefixes.
	reservedPrefixes []string

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
	// single wall-clock bound on a read's whole reconnect-and-retry chain.
	//
	// Tests set it directly to exercise budget exhaustion without a real 90s
	// wait. Production sets it through WithNativeReadRetryBudget on the
	// proxied-native path, where the 90s default is the wrong number by an
	// order of magnitude: that budget exists to span a MANAGED Dolt hard-kill
	// and rebind, which gc performs itself and can therefore wait out. A
	// bd-owned proxy is not gc's to restart, so a read that cannot reach it
	// should demote to the bd leaf in seconds rather than hold a caller
	// through a minute and a half of mysql i/o timeouts.
	readRetryBudgetOverride time.Duration

	// readOnlyReason, when non-empty, latches this handle read-only: every
	// mutating method refuses with ErrProxiedNativeReadOnly before it reaches
	// storage. It is set once at open by WithProxiedReadOnly and never cleared
	// in PR2. See native_dolt_readonly.go for why the fence lives here rather
	// than in a hand-written read-only leaf type.
	readOnlyReason string

	// proxiedReadVerdicts latches that this handle was opened against a
	// database bd's proxy serves, so the read path may name an endpoint fact as
	// a typed *ProxiedVerdictError for the ProxiedStore wrapper to demote on.
	//
	// It is set structurally by OpenNativeDoltStoreAtProxied rather than by a
	// caller option: a proxied handle without it would classify its failures
	// correctly and then hand the wrapper an untyped error, which is the one
	// shape that makes a dead handle invisible. A direct or hosted handle leaves
	// it false and its callers keep receiving byte-identical errors — the shared
	// classification table still decides transient-vs-terminal for both lanes,
	// but only this lane renders a verdict. See native_dolt_errors.go.
	proxiedReadVerdicts bool

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

// WithNativeReadRetryBudget replaces the 90s default wall-clock bound on one
// read's whole reconnect-and-retry chain.
//
// It is a production option, not a test hook. The default is sized for a
// managed-Dolt rebind gc performs itself: a read that spans one should recover
// rather than fail, so it waits out ~40-56s of mysql i/o timeouts plus the
// restart. A proxied-native handle is in the opposite situation — bd owns the
// proxy and its Dolt child, gc never restarts either, and the recovery for an
// unreachable endpoint is to demote this handle to the bd leaf. Holding a
// caller for 90s first buys nothing and hides the demotion behind a timeout
// nobody can attribute.
//
// A non-positive duration is ignored, so a misread knob leaves the default
// rather than producing a store whose every read fails instantly.
func WithNativeReadRetryBudget(budget time.Duration) NativeDoltStoreOption {
	return func(s *NativeDoltStore) {
		if budget > 0 {
			s.readRetryBudgetOverride = budget
		}
	}
}

// WithNativeDoltStoreReservedIDPrefixes fences Create to the id namespaces the
// binding this store serves claims, mirroring
// WithSQLiteStoreReservedIDPrefixes. It is what makes a workspace binding's
// namespace claim hold rather than be a convention.
//
// More than one prefix, because a binding holds more than it mints — the nudge
// queue's records live in the nudges store under their own namespace. An empty
// set leaves the store unfenced, which is the shipped default everywhere the
// store is not a class binding.
//
// The pinned upstream library does not fence for us: its single-issue create
// path sets SkipPrefixValidation for an explicit id on purpose, so this is the
// only place the claim can be enforced.
//
// CreateWithForeignID deliberately bypasses the fence: carrying a preserved
// foreign id across is the store-migration copy path's entire job.
func WithNativeDoltStoreReservedIDPrefixes(prefixes ...string) NativeDoltStoreOption {
	return func(s *NativeDoltStore) {
		s.reservedPrefixes = collectReservedIDPrefixes(s.reservedPrefixes, prefixes)
	}
}

var (
	_ Store                         = (*NativeDoltStore)(nil)
	_ ConditionalAssignmentReleaser = (*NativeDoltStore)(nil)
	_ AtomicTxStore                 = (*NativeDoltStore)(nil)
	_ GraphApplyStore               = (*NativeDoltStore)(nil)
	_ StorageGraphApplyStore        = (*NativeDoltStore)(nil)
	_ EphemeralGraphApplyStore      = (*NativeDoltStore)(nil)
	_ conditionalWritesModeCarrier  = (*NativeDoltStore)(nil)
	_ ForeignIDCreator              = (*NativeDoltStore)(nil)
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

// nativeIssuePrefixConfigKey is the upstream config key naming the namespace a
// Dolt-backed ledger mints under.
const nativeIssuePrefixConfigKey = "issue_prefix"

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
	storage, err := openNativeDoltStorage(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, "", err
	}
	var prefix string
	if readPrefix {
		prefix, err = storage.GetConfig(ctx, nativeIssuePrefixConfigKey)
		if err != nil {
			_ = storage.Close()
			return nil, "", fmt.Errorf("reading native issue prefix: %w", err)
		}
	}
	return storage, prefix, nil
}

// NewNativeDoltStoreOverStorageForTest wraps a caller-supplied storage handle in
// a NativeDoltStore, for tests in OTHER packages that need a real leaf — one
// whose CloseStore reaches the storage's Close. cmd/gc's proxied opener tests
// use it without a Dolt server to prove a refused open closes the leaf it
// opened (council pr2 E-I5). The proxied-native safety acceptance row uses it
// over OpenNativeStorageAtProxied's storage to WRITE through the proxied
// window's author latch, because every handle OpenNativeDoltStoreAtProxied
// returns is read-only latched (council B-F5) and PR2 ships no writable one.
// The handle it returns carries no read-only latch, no reopen hook and no
// issue prefix. Production opens only through OpenNativeDoltStoreAt* and
// OpenNativeDoltStoreAtProxied.
func NewNativeDoltStoreOverStorageForTest(storage NativeStorage) *NativeDoltStore {
	return newNativeDoltStoreForTest(storage)
}

func newNativeDoltStoreForTest(storage beadslib.Storage, opts ...NativeDoltStoreOption) *NativeDoltStore {
	store := newNativeDoltStoreWithStorage(storage, "native-test")
	for _, opt := range opts {
		opt(store)
	}
	return store
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
// What "transient" means is decided by classifyNativeDoltReadError, which runs
// a typed table (indeterminate commit, serialization conflict, open circuit,
// MySQL 1049/1045, sentinel connection-level failures) AHEAD of the substring
// signatures, so a fact a retry cannot move stops the loop instead of being
// returned as if it were an endpoint state. It is classified for THIS handle's
// lane: the serialization, open-circuit and connection-level rungs (2, 3 and 6)
// are proxied-lane only, so a direct or hosted handle's control flow and
// returned error are the ones it has on main — except for the two
// mixed-signature errors native_dolt_errors.go states (an indeterminate commit,
// or a 1049/1045, whose text also carries a transient substring). See that file
// for the order, and for the three rungs that carry a lane gate and why.
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
		if marked, owed := s.poolStaleOwed(); owed {
			// The guard tick saw this handle's proxy generation replaced. Re-point
			// the pool BEFORE serving: a moved root serves the old database
			// without an error, so waiting for a failure would wait forever.
			release()
			rcErr := s.repinStalePool(ctx, gen, marked)
			if rcErr == nil {
				continue
			}
			if _, typed := ProxiedVerdictOf(rcErr); typed {
				// The re-admission inside the hook refused with a verdict. It is
				// already the answer; looping would bury it under a budget error.
				return rcErr
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return s.proxiedReadBudgetVerdict(nativeReadRetryBudgetError(ctxErr, rcErr))
			}
			if classifyNativeDoltReadError(rcErr, s.readLane()).disposition != nativeReadTransient {
				return rcErr
			}
			// A proxy mid-restart is worth another pass while the budget remains.
			select {
			case <-ctx.Done():
				return s.proxiedReadBudgetVerdict(nativeReadRetryBudgetError(ctx.Err(), rcErr))
			case <-time.After(nativeReadRetryBackoff):
			}
			continue
		}
		opErr := fn(ctx, storage)
		release()
		if opErr == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return s.proxiedReadBudgetVerdict(nativeReadRetryBudgetError(ctxErr, opErr))
		}
		reopen, closed := s.reopenState()
		if closed {
			return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
		}
		class := classifyNativeDoltReadError(opErr, s.readLane())
		backoff := nativeReadRetryBackoff
		switch class.disposition {
		case nativeReadTerminal:
			// The endpoint answered about the database or the credentials. A
			// fresh pool would ask the same question and get the same answer.
			return s.proxiedReadVerdict(class.verdict, opErr)
		case nativeReadNonReplayable, nativeReadUnclassified:
			return opErr
		case nativeReadCircuitOpen:
			// Nothing reached a socket, so there is nothing to reconnect: wait
			// for the breaker to re-arm and ask the SAME handle again. The wait
			// is inside the read's own budget, so a breaker that stays open
			// costs the budget rather than an unbounded hold.
			//
			// A handle with no reopen hook keeps the documented fail-fast
			// contract: a test store built straight from a storage value must
			// not start spending a 90s budget on a cooldown loop.
			if reopen == nil {
				return opErr
			}
			backoff = class.cooldown
		case nativeReadTransient:
			if reopen == nil {
				return opErr
			}
			if rcErr := s.reconnect(ctx, gen); rcErr != nil {
				reconnectErr := fmt.Errorf("native Dolt reconnect after transient read error (%w): %w", opErr, rcErr)
				// A verdict the reopen hook produced is already the answer: the
				// proxied ladder has been walked inside the hook, and looping
				// here would bury a typed refusal under the budget error the
				// wrapper cannot demote on. Return it on this pass, wrapped so
				// the cause survives and errors.As still recovers the verdict.
				if _, ok := ProxiedVerdictOf(rcErr); ok {
					return reconnectErr
				}
				// A reconnect that itself fails transiently (server mid-restart) is
				// worth another pass while the budget remains; a non-transient
				// reconnect failure or an exhausted budget is terminal.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return s.proxiedReadBudgetVerdict(nativeReadRetryBudgetError(ctxErr, reconnectErr))
				}
				if classifyNativeDoltReadError(rcErr, s.readLane()).disposition != nativeReadTransient {
					return reconnectErr
				}
			}
		}
		// Cancellable backoff: budget expiry during the wait aborts the chain
		// instead of sleeping past the wall.
		select {
		case <-ctx.Done():
			return s.proxiedReadBudgetVerdict(nativeReadRetryBudgetError(ctx.Err(), opErr))
		case <-time.After(backoff):
		}
	}
}

func nativeReadRetryBudgetError(ctxErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("native Dolt read retry budget exhausted: %w", ctxErr)
	}
	return fmt.Errorf("native Dolt read retry budget exhausted (%w), last error: %w", ctxErr, lastErr)
}

// markPoolStale asks this handle to reconnect before it serves another read,
// and reports whether the request can be honored.
//
// It is the proxied guard tick's re-pin, minus the library open. A handle with no
// reopen hook, or one already closed, CANNOT re-point its pool — marking it would
// leave every later read reconnecting through a nil hook — so it reports false
// and the caller stands the handle down instead.
func (s *NativeDoltStore) markPoolStale() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	closed, reopen := s.closed, s.reopen
	s.mu.RUnlock()
	if closed || reopen == nil {
		return false
	}
	s.poolStale.Add(1)
	return true
}

// poolStaleOwed reports the current mark count and whether a re-point is owed:
// a mark exists that no installed storage's reopen could have seen.
func (s *NativeDoltStore) poolStaleOwed() (uint64, bool) {
	marked := s.poolStale.Load()
	return marked, marked > s.poolServiced.Load()
}

// notePoolServiced advances the watermark to covered, never backwards. It is a
// max rather than a store because two installs can finish out of the order
// their reopens started in.
func (s *NativeDoltStore) notePoolServiced(covered uint64) {
	for {
		current := s.poolServiced.Load()
		if covered <= current || s.poolServiced.CompareAndSwap(current, covered) {
			return
		}
	}
}

// repinStalePool swaps the pool a guard tick invalidated for one bound to the
// current generation, on the CALLER's goroutine.
//
// The reconnect is the existing single-flight path, so concurrent readers
// re-point once and the reopen hook — which re-runs admission and re-projects the
// current endpoint — is what decides whether the new generation may be served at
// all. It clears NOTHING itself (council pr2 D-F6): a successful install
// advances the watermark to the marks its own reopen could see (reconnect), and
// a reconnect that returned nil because another reader installed first leaves
// the watermark wherever THAT install put it. The read loop then asks
// poolStaleOwed again, so a mark neither install covered — one a tick set while
// this reader was parked on the gate, or before a transient reconnect's reopen
// began — is re-pointed rather than erased.
func (s *NativeDoltStore) repinStalePool(ctx context.Context, observedGen, servicing uint64) error {
	reopen, closed := s.reopenState()
	if closed {
		return fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	if reopen == nil {
		// markPoolStale refuses a hook-less handle, so this is only reachable if
		// the hook went away afterwards. There is nothing to re-point with, so
		// the marks this reader saw are declared serviced rather than spun on.
		s.notePoolServiced(servicing)
		return nil
	}
	if err := s.reconnect(ctx, observedGen); err != nil {
		return fmt.Errorf("native Dolt re-pin after a proxy generation change: %w", err)
	}
	return nil
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

	// The stale-pool marks this reopen can honor are the ones already set
	// before it starts: a guard tick adopts the new pin BEFORE it marks, so a
	// reopen that begins after a mark re-admits against that pin or a newer
	// one. A mark set while the reopen is in flight is not covered, and stays
	// owed (council pr2 D-F6). On the direct and hosted lanes nothing ever
	// marks, so this is a load of zero and the watermark stays at zero.
	coversMarks := s.poolStale.Load()

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
	// Advanced under the lock, with the install, so no reader can acquire the
	// fresh storage and still see the marks this reopen covered as owed.
	s.notePoolServiced(coversMarks)
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
//
// At or under issueops.MaxApplyBatchItems, it composes the whole plan into
// ONE issueops.BatchApplier request — never chunked, because this route's
// entire value over a sequence of ordinary writes is that it lands as a
// single transaction. BatchApplier's own end gate re-validates every dep_add
// item against the parent-child closure the whole request produced after
// every item lands, raising *issueops.DependencyHierarchyConflictError or
// issueops.ErrDependencyCycle.
//
// issueops.MaxApplyBatchItems is BatchApplier's own cap, not a wire limit, so
// it binds a local (embedded) backend exactly as it binds a served one. A
// plan over the cap therefore cannot go through BatchApplier at all, on any
// backend — but a LOCAL backend still has a second atomic route: its own
// beadslib.RunInTransaction, which this method drives by hand
// (applyGraphPlanOverCapInTransaction) exactly as the pre-BatchApplier
// implementation did before #383c38552f. A served backend has no transaction
// to retry against — RunInTransaction refuses it with a typed
// *beadslib.ErrUnsupported before the callback ever runs — and that refusal
// is where *GraphApplyTooLargeError belongs: there genuinely is no atomic
// path left for a plan this large on that backend.
func (s *NativeDoltStore) ApplyGraphPlanWithStorage(parent context.Context, plan *GraphApplyPlan, storageClass StorageClass) (*GraphApplyResult, error) {
	if err := s.readOnlyGuard(); err != nil {
		return nil, err
	}
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

	req, err := nativeGraphApplyBatchRequest(plan, s.actor, ephemeral, noHistory)
	if err != nil {
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

	if len(req.Items) <= issueops.MaxApplyBatchItems {
		applier, err := storage.BatchApplier()
		if err != nil {
			return nil, fmt.Errorf("native graph apply: %w", err)
		}
		applyResult, err := applier.ApplyBatch(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("native graph apply: %w", err)
		}
		result := &GraphApplyResult{IDs: applyResult.Keys}
		if err := ValidateGraphApplyResult(plan, result); err != nil {
			return nil, fmt.Errorf("native graph apply: %w", err)
		}
		return result, nil
	}

	result, entered, err := s.applyGraphPlanOverCapInTransaction(ctx, storage, plan, ephemeral, noHistory)
	if err == nil {
		if verr := ValidateGraphApplyResult(plan, result); verr != nil {
			return nil, fmt.Errorf("native graph apply: %w", verr)
		}
		return result, nil
	}
	var unsupported *beadslib.ErrUnsupported
	if !entered && errors.As(err, &unsupported) {
		// No transaction was ever opened — ask, don't handshake — so this
		// backend has no atomic route left for a plan this large at all.
		return nil, fmt.Errorf("native graph apply: %w", &GraphApplyTooLargeError{Items: len(req.Items), Max: issueops.MaxApplyBatchItems})
	}
	return nil, fmt.Errorf("native graph apply: %w", err)
}

// applyGraphPlanOverCapInTransaction is the over-cap fallback for a LOCAL
// (embedded) backend: it drives the same plan by hand through
// beadslib.RunInTransaction instead of issueops.BatchApplier, exactly as this
// store did before #383c38552f ported the at-or-under-cap path onto
// BatchApplier. Every tx.* call below is a LOCAL FALLBACK call: it lives only
// on this route, reached only after BatchApplier's own cap has already ruled
// out the batch path for this plan.
//
// entered reports whether the RunInTransaction callback ran at all, the same
// distinction Tx() draws: a refusal raised AFTER the callback ran is a FAILED
// transaction (the callback may have partially written), never a signal that
// this backend lacks transactions; only a refusal raised before entry means
// "no transaction route here."
func (s *NativeDoltStore) applyGraphPlanOverCapInTransaction(ctx context.Context, storage beadslib.Storage, plan *GraphApplyPlan, ephemeral, noHistory bool) (result *GraphApplyResult, entered bool, err error) {
	keyToID := make(map[string]string, len(plan.Nodes))
	commitMsg := plan.CommitMessage
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("gc: graph-apply %d nodes", len(plan.Nodes))
	}

	err = storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
		entered = true

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

		// local fallback: tx.CreateIssues mints every id for this transaction
		// in one call, bypassing issueops.BatchApplier entirely (its CreateItem
		// is what this route exists to avoid).
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
			refs := make(map[string]string, len(node.MetadataRefs))
			for metaKey, refKey := range node.MetadataRefs {
				refs[metaKey] = keyToID[refKey]
			}
			raw, err := metadataRawWithOverrides(issues[i].Metadata, refs)
			if err != nil {
				return fmt.Errorf("node %q: marshaling updated metadata: %w", node.Key, err)
			}
			// local fallback: the map-based tx.UpdateIssue splices resolved
			// metadata refs the same way the native route always has; it skips
			// the role's metadata-key validation on purpose, the same documented
			// asymmetry Tx() relies on (TestNativeDoltStoreMetadataKeyRuleSplitsByRoute).
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
			// local fallback: tx.AddDependency, the pre-BatchApplier route's own
			// end gate — the manual parentDepPairs check above is this route's
			// substitute for BatchApplier's hierarchy-closure re-validation,
			// since nothing else re-checks a hand-run transaction's edges.
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
			// local fallback: see tx.AddDependency comment above.
			if err := tx.AddDependency(ctx, dep, s.actor); err != nil {
				return fmt.Errorf("node %q: adding parent-child dep: %w", node.Key, err)
			}
		}

		for i, assignee := range pendingAssignees {
			// local fallback: deferred post-create assignment, in place of
			// BatchApplier's UpdateItem pass over AssignAfterCreate nodes.
			if err := tx.UpdateIssue(ctx, issues[i].ID, map[string]interface{}{"assignee": assignee}, s.actor); err != nil {
				return fmt.Errorf("node %q: setting assignee: %w", plan.Nodes[i].Key, err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, entered, err
	}
	return &GraphApplyResult{IDs: keyToID}, entered, nil
}

// SupportsEphemeralGraphApply reports whether this store can apply a whole
// graph directly into ephemeral storage.
func (s *NativeDoltStore) SupportsEphemeralGraphApply() bool {
	return true
}

// Create persists a new bead through the upstream issue-operations facade. The
// facade commits the issue and every edge in one transaction, so a partial
// create can no longer be observed and needs no compensation.
func (s *NativeDoltStore) Create(b Bead) (Bead, error) {
	return s.create(b, false)
}

// CreateWithForeignID persists a new bead KEEPING its explicit id whatever
// prefix it carries, for the store-migration copy path. Refusing a preserved id
// there would leave the beads it carries nowhere at all. It satisfies
// ForeignIDCreator.
func (s *NativeDoltStore) CreateWithForeignID(b Bead) (Bead, error) {
	// Ahead of the id validation, not after it: a read-only handle must refuse
	// for the reason it is read-only, not report an argument problem it would
	// never have acted on anyway.
	if err := s.readOnlyGuard(); err != nil {
		return Bead{}, err
	}
	if strings.TrimSpace(b.ID) == "" {
		return Bead{}, fmt.Errorf("creating bead with foreign id: empty id")
	}
	return s.create(b, true)
}

// create is the shared body. allowForeign is the CreateWithForeignID exemption.
//
// The fence runs FIRST — before the bead is converted, before the storage
// handle is acquired, before any read. That ordering is the contract: a refused
// id must reach nothing, so it cannot write a row, cannot move the mint
// sequence, and cannot reveal through its refusal whether the store already
// holds a relic under that id.
func (s *NativeDoltStore) create(b Bead, allowForeign bool) (Bead, error) {
	if err := s.readOnlyGuard(); err != nil {
		return Bead{}, err
	}
	if !allowForeign {
		if err := checkPinnedIDNamespace("native dolt create", b.ID, s.reservedPrefixes); err != nil {
			return Bead{}, err
		}
	}
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	request, err := nativeCreateRequestFromIssue(s.actor, issue)
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
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return Bead{}, nativeStoreError(issue.ID, err)
	}
	result, err := ops.Create(ctx, request)
	if err != nil {
		return Bead{}, nativeStoreError(issue.ID, err)
	}
	return beadFromNativeIssue(result.Issue)
}

// nativeCreateRequestFromIssue moves a bead's dependency edges off the issue and
// onto the facade request, which is where the facade takes them: a create whose
// issue still carries edges is refused outright.
//
// The parent edge stays an ordinary parent-child entry in Dependencies rather
// than moving to CreateRequest.ParentID. Both produce the same edge, but
// ParentID also switches ID minting to the hierarchical "<parent>.N" scheme,
// which would silently change the IDs Gas City mints for every child bead.
func nativeCreateRequestFromIssue(actor string, issue *beadslib.Issue) (issueops.CreateRequest, error) {
	dependencies := issue.Dependencies
	issue.Dependencies = nil
	request := issueops.CreateRequest{
		Actor: actor,
		Issue: issue,
		// The storage-layer create this replaced skipped prefix validation for
		// an explicit ID, and Gas City mints several prefixes (gc-, gcg-, gcs-)
		// against one store. Without this an explicit off-prefix ID that used
		// to create fine is refused with ErrPrefixMismatch.
		ForceIDPrefix: true,
	}
	for _, dependency := range dependencies {
		if dependency == nil {
			continue
		}
		target := strings.TrimSpace(dependency.DependsOnID)
		if target == "" {
			return issueops.CreateRequest{}, fmt.Errorf("creating bead %q: dependency depends_on_id is empty", issue.ID)
		}
		source := strings.TrimSpace(dependency.IssueID)
		switch {
		case source == "" || source == issue.ID:
			request.Dependencies = append(request.Dependencies, issueops.CreateDependency{
				TargetID: target,
				Type:     dependency.Type,
			})
		case target == issue.ID:
			request.Dependencies = append(request.Dependencies, issueops.CreateDependency{
				TargetID: source,
				Type:     dependency.Type,
				Reverse:  true,
			})
		default:
			return issueops.CreateRequest{}, fmt.Errorf("creating bead %q: dependency %q -> %q names neither end of the new bead", issue.ID, source, target)
		}
	}
	return request, nil
}

// Update modifies an existing bead through the upstream issue-operations facade.
//
// The facade route and the Store.Tx route do not validate the same things, and
// the split does not run in one direction. The facade route is stricter about
// the patch: it validates metadata keys (see beadmeta.ValidKey), requires a
// non-empty title, bounds priority to 0-4, and reports ErrNotFound for a
// no-field update against a missing bead (updateOnce reads the row itself
// rather than send the facade an empty patch) — the map-based Tx write accepts
// all four. The Tx route is stricter about the close policy: a status write that
// crosses into the done category is refused there while children are open, and
// the override that would waive it (beads' "_force_close_policy" update-map key)
// has no exported spelling for the map path, so Store.Tx cannot express it.
// Update waives that policy here because the storage-layer write it replaced
// applied none, and a molecule root routinely closes over open children.
// Treat Store.Tx as unsupported for a done-crossing status write until beads
// exports the override; use Close, or the standalone Update, instead.
//
// Update is NOT atomic when ParentID names a foreign row. The facade cannot
// resolve that parent, so the other fields commit through it first and the
// parent edge is rewritten afterward in a write of its own (see
// rewriteExternalParent). A failure between the two returns its error with the
// new fields written and the old parent edge still standing, at worst beside
// the new one, and replaying the Update converges because both writes are
// idempotent.
//
// Only two of Update's writes label their Dolt history entry. The plain door
// sends the facade no Provenance, so that entry carries the facade's default
// ("bd: update <id>"). The batch door a force member takes passes "gc: update
// <id>", and the edge rewrite a foreign ParentID takes labels its transaction
// "gc: update bead <id>" where the backing has one (see rewriteExternalParent);
// without one, the edge roles record their own default labels.
func (s *NativeDoltStore) Update(id string, opts UpdateOpts) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	patch, err := nativeIssuePatchFromUpdateOpts(opts)
	if err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()

	// The patch is pure, so only the write needs replaying.
	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return s.updateOnce(ctx, storage, id, patch, opts)
	})
}

// updateOnce performs one complete update attempt, so a replay re-picks the
// door from the same fields rather than reusing an earlier decision.
func (s *NativeDoltStore) updateOnce(ctx context.Context, storage beadslib.Storage, id string, patch issueops.IssuePatch, opts UpdateOpts) error {
	// A reparent onto (or off of) a foreign id is a weak reference this store
	// cannot resolve -- see validateUpdateParent's doc comment, which states the
	// same rule for the Store.Tx route. The facade's Update role resolves
	// ParentID like any other local write and refuses a target it cannot find,
	// so a foreign value has to come OUT of the patch before it reaches either
	// door below, and go in afterward as an edge rewrite of its own.
	var externalParentID string
	hasExternalParent := false
	if opts.ParentID != nil && !nativeParentIsLocal(id, *opts.ParentID, s.idPrefix) {
		hasExternalParent = true
		externalParentID = *opts.ParentID
		patch.ParentID = issueops.Field[string]{}
	}
	if nativeIssuePatchIsEmpty(patch) {
		// Nothing is left for the facade: a no-field update, or a pure reparent
		// whose foreign parent came out above. The served wire refuses an empty
		// patch outright, where the in-process role answers one with a row read
		// and no write. That read is all a no-field update reports — ErrNotFound
		// for a missing bead, as MemStore does — so it is made here instead, and
		// it stops a missing bead before the edge rewrite writes anything.
		current, err := storage.GetIssue(ctx, id)
		if err != nil {
			return nativeStoreError(id, err)
		}
		if current == nil {
			return fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
	} else if err := s.updateOnceThroughFacade(ctx, storage, id, patch, opts); err != nil {
		return err
	}
	if hasExternalParent {
		return s.rewriteExternalParent(ctx, storage, id, externalParentID)
	}
	return nil
}

// rewriteExternalParent replaces id's parent-child edge with one to parentID,
// or clears it when parentID is empty, for a parent nativeParentIsLocal has
// already called foreign.
//
// The rewrite is two edge writes, the old edge out and the new one in, and the
// bead must never be left between them: nothing reports a bead that lost its
// parent, so the molecule or convoy just stops listing it. Where the backing
// has a transaction, both writes run in it and a failure rolls both back.
// Where it has none, updateParentThroughEditor orders them so that a failure
// between the two leaves a second parent edge standing, never no edge at all.
//
// It is a write of its own rather than part of the facade's. The facade opens
// its own transaction and takes no extra edge into it. Routing the whole
// Update through the Store.Tx body instead would hand a foreign-parent update
// that route's validation (see Update's doc comment), so the same fields would
// be accepted or refused depending on whether the parent happened to be
// foreign. The cost lands only on an update that carries other fields too.
// Those commit first, so a field the facade refuses stops the update before
// any edge is touched, and a failed rewrite returns its error with the new
// fields written and the old parent intact; replaying the update converges
// because both writes are idempotent. A pure reparent, which is what convoy
// membership and molecule attach send, leaves the facade no field to patch, so
// updateOnce dials it nothing and the edge rewrite is its only write.
func (s *NativeDoltStore) rewriteExternalParent(ctx context.Context, storage beadslib.Storage, id, parentID string) error {
	err := runInNativeTransaction(ctx, storage, fmt.Sprintf("gc: update bead %s", id), func(tx beadslib.Transaction) error {
		return s.updateParentInTransaction(ctx, tx, id, parentID)
	}, func() error {
		return s.updateParentThroughEditor(ctx, storage, id, parentID)
	})
	return nativeStoreError(id, err)
}

// updateOnceThroughFacade carries out the facade-role write updateOnce picked
// a door for, once any foreign ParentID has already been taken out of patch.
func (s *NativeDoltStore) updateOnceThroughFacade(ctx context.Context, storage beadslib.Storage, id string, patch issueops.IssuePatch, opts UpdateOpts) error {
	// Store.Update is the low-level projection verb: the storage-layer write it
	// replaced applied no claim fence and no close policy, and the callers that
	// need those guards (ReleaseIfCurrent, the dispatcher's compare-and-set
	// claims) enforce them a layer up. So an assignee or status edit waives the
	// corresponding guard — and WHICH DOOR CARRIES THAT WAIVER IS NOT THE SAME
	// ON EVERY BACKEND.
	//
	// updateIssue publishes neither force member: the http client refuses
	// ForceAssigneeTransfer and ForceClosePolicy outright
	// (W-UpdateRequest.ForceAssigneeTransfer, W-UpdateRequest.ForceClosePolicy).
	// The APPLY item publishes both. So a patch that needs a waiver goes
	// through a single-item batch, and one that does not goes through the plain
	// door carrying no force member at all — which is what lets an ordinary
	// title edit cross the wire, where an unconditional force refused it.
	//
	// The split is decided by the FIELDS rather than by retrying a refusal,
	// because the two doors disagree in the other direction too: the apply
	// patch refuses ParentID (W-ApplyPatch.ParentID) where updateIssue
	// publishes it. A reparent alone therefore stays on the plain door. A patch
	// that reparents AND edits assignee or status can be served by neither over
	// the wire; it takes the batch door and refuses there, naming the member.
	if opts.Assignee != nil || opts.Status != nil {
		return s.updateThroughBatch(ctx, storage, id, patch, opts)
	}
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return nativeStoreError(id, err)
	}
	if _, err := ops.Update(ctx, issueops.UpdateRequest{
		Actor:   s.actor,
		IssueID: id,
		Patch:   patch,
	}); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// updateThroughBatch applies one patch as a single-item batch so it can carry
// the force members updateIssue does not publish. Both forces stay conditioned
// on the field that arms the corresponding guard, because
// ForceAssigneeTransfer without an assignee edit is a hard validation error
// rather than a no-op.
func (s *NativeDoltStore) updateThroughBatch(ctx context.Context, storage beadslib.Storage, id string, patch issueops.IssuePatch, opts UpdateOpts) error {
	applier, err := storage.BatchApplier()
	if err != nil {
		return nativeStoreError(id, err)
	}
	if _, err := applier.ApplyBatch(ctx, issueops.ApplyBatchRequest{
		Actor: s.actor,
		Items: []issueops.ApplyItem{{
			Kind: issueops.ItemUpdate,
			Update: &issueops.UpdateItem{
				Target:                issueops.Ref{ID: id},
				Patch:                 patch,
				ForceAssigneeTransfer: opts.Assignee != nil,
				ForceClosePolicy:      opts.Status != nil,
			},
		}},
		Provenance: "gc: update " + id,
	}); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// nativeIssuePatchFromUpdateOpts maps Gas City update options onto the facade's
// issue patch. Metadata keys merge rather than replace, matching the
// read-modify-write the storage-layer path performed.
func nativeIssuePatchFromUpdateOpts(opts UpdateOpts) (issueops.IssuePatch, error) {
	var patch issueops.IssuePatch
	if opts.Title != nil {
		patch.Title = issueops.Field[string]{Set: true, Value: *opts.Title}
	}
	if opts.Status != nil {
		patch.Status = issueops.Field[issueops.Status]{Set: true, Value: issueops.Status(*opts.Status)}
	}
	if opts.Type != nil {
		patch.IssueType = issueops.Field[issueops.IssueType]{Set: true, Value: issueops.IssueType(*opts.Type)}
	}
	if opts.Priority != nil {
		patch.Priority = issueops.Field[int]{Set: true, Value: *opts.Priority}
	}
	if opts.Description != nil {
		patch.Description = issueops.Field[string]{Set: true, Value: *opts.Description}
	}
	if opts.Assignee != nil {
		patch.Assignee = issueops.Field[string]{Set: true, Value: *opts.Assignee}
	}
	if opts.ParentID != nil {
		patch.ParentID = issueops.Field[string]{Set: true, Value: *opts.ParentID}
	}
	patch.Labels.Add = append([]string(nil), opts.Labels...)
	patch.Labels.Remove = append([]string(nil), opts.RemoveLabels...)
	if len(opts.Metadata) > 0 {
		patch.Metadata.Set = make(map[string]json.RawMessage, len(opts.Metadata))
		for key, value := range opts.Metadata {
			raw, err := json.Marshal(value)
			if err != nil {
				return issueops.IssuePatch{}, fmt.Errorf("marshaling metadata value for %q: %w", key, err)
			}
			patch.Metadata.Set[key] = raw
		}
	}
	return patch, nil
}

// applyUpdateInTx applies an Update against an open beadslib transaction. Its two
// callers each need a transaction the facade's own per-operation one cannot be:
// the multi-write Store.Tx path (nativeDoltTx.Update) coalesces several writes
// into one commit, and updateLabelsIfMatch checks the row version in the same
// transaction that writes the labels.
//
// This route is not the unvalidated one. It skips the facade's patch validation
// (metadata keys, title, priority) but enforces the close policy the facade
// route waives — see the Update doc comment for the whole asymmetry. An
// UpdateIfMatch that carries labels inherits it exactly as Store.Tx does.
func (s *NativeDoltStore) applyUpdateInTx(ctx context.Context, tx beadslib.Transaction, id string, opts UpdateOpts) error {
	if opts.ParentID != nil {
		if err := s.validateUpdateParent(ctx, tx, id, *opts.ParentID); err != nil {
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
	raw, err := metadataRawWithOverrides(issue.Metadata, kvs)
	if err != nil {
		return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
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
//
// It fences the same way the standalone Create does. A transaction is not an
// exemption: the bead it writes is as resident, and as unreachable by an
// id-shaped lookup of the namespace it lands in, as one written outside a
// transaction. There is no foreign-id variant here on purpose — the migration
// copy that needs the exemption runs through CreateWithForeignID on the store,
// not inside a caller's transaction.
func (s *NativeDoltStore) applyCreateInTx(ctx context.Context, tx beadslib.Transaction, b Bead) (Bead, error) {
	if err := checkPinnedIDNamespace("native dolt tx create", b.ID, s.reservedPrefixes); err != nil {
		return Bead{}, err
	}
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

// ReleaseIfCurrent clears an assignment only while the bead still has the
// expected assignee.
//
// It routes through issueops.Releaser rather than the hand-composed
// read-check-write it used to run inside RunInTransaction, for the reason
// every other re-point in this package has: RunInTransaction is off the v0
// served surface, so the hand-composed version was a hard failure on any store
// reached over the wire. The role is a required member of the Storage
// contract, and its guard is evaluated inside the releasing transaction, so
// the atomicity the old shape built by hand is the role's own promise.
//
// ExpectedAssignee is ALWAYS supplied, and that is the whole method. A nil
// expectation selects the role's unconditional path, whose ownership fence's
// subject is the ACTOR — and gc's actor is the city, never the holder — so a
// nil expectation would release a claim this front door was told to leave
// alone. An empty holder is therefore not a release at all: it dials nothing
// and reports "not released", which is also what the role would say about it
// (a row nobody holds is ErrNotClaimed).
//
// A CONDITIONAL REFUSAL IS A VALUE, NOT AN ERROR, which is this front door's
// contract and the reason the mapping is not a pass-through. The role refuses
// with a typed sentinel for each of the four ways a conditional release can
// fail to happen — a different holder, no claim, a status that accepts no
// release, a bead that is gone — and every one of them is the same fact to a
// reconciler: the claim is not ours to give back. Everything else travels,
// because a transport failure reported as "not released" would leave a caller
// believing a claim is still held.
//
// TWO THINGS THE ROLE DOES THAT THE HAND-BUILT VERSION DID NOT, both improvements
// and both worth knowing:
//
//   - THE HOLDER COMPARISON IS SEPARATOR-INSENSITIVE. A run of ".", "_" or "-"
//     matches any other such run, so "agent-a", "agent_a" and "agent.a" are one
//     holder where the old byte-exact comparison saw three. gc composes its
//     expectation from an assignee a read gave it, so this widens nothing gc
//     asks for; it forgives a caller that reached the same identity through a
//     layer that spells separators differently. Nothing else is forgiven — the
//     value is neither trimmed nor case-folded.
//   - THE RELEASE DROPS THE LEASE AND REMINTS THE ROW VERSION. The old
//     two-field status/assignee write left the lease row behind and moved no
//     version, so a concurrent reclaim could silently merge with it.
//
// TWO FAMILY DIVERGENCES THIS OPENED, both recorded on
// ConditionalAssignmentReleaser in beads.go rather than here, because they are
// facts about the store FAMILY and not about this implementation: the role
// releases from open as well as in_progress where MemStore, SQLiteStore and
// the pre-role version of this method released only from in_progress; and an
// empty expected holder over an in_progress row with an empty assignee is
// (false, nil) here where MemStore answers true. Neither is pinned by a
// conformance suite today.
func (s *NativeDoltStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if err := s.readOnlyGuard(); err != nil {
		return false, err
	}
	if expectedAssignee == "" {
		return false, nil
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	releaser, err := storage.Releaser()
	if err != nil {
		return false, nativeStoreError(id, err)
	}
	result, err := releaser.Release(ctx, issueops.ReleaseRequest{
		Actor:            s.actor,
		IssueID:          id,
		ExpectedAssignee: &expectedAssignee,
	})
	if err != nil {
		if nativeReleaseRefused(err) {
			return false, nil
		}
		return false, nativeStoreError(id, err)
	}
	return result.Changed, nil
}

// nativeReleaseRefused reports the refusals that mean "this claim was not ours
// to give back". Each is a fact about the row rather than a failure to reach
// it, and this front door reports all four as a false verdict.
func nativeReleaseRefused(err error) bool {
	return errors.Is(err, issueops.ErrAssigneeMismatch) ||
		errors.Is(err, issueops.ErrNotClaimed) ||
		errors.Is(err, issueops.ErrNotReleasable) ||
		errors.Is(err, issueops.ErrNotFound)
}

// Claim atomically claims a bead for assignee through issueops.Claimer. It
// routes through the role rather than a hand-composed read-check-write for the
// same reason ReleaseIfCurrent does: the role is the required, wire-served
// member of the Storage contract, and its CAS plus retry-on-commit-conflict
// are the role's own promise rather than something this front door has to
// rebuild.
//
// The (Bead, bool, error) idiom matches SQLiteStore.Claim and BdStore.Claim: a
// conflict — someone else holds the bead, or its status is not claimable — is
// reported as ok=false with a nil error, never as an error value, because a
// losing claim attempt is an expected, non-exceptional outcome every caller
// of this capability already treats as a value (claim_class_route.go,
// class_store_emit.go). issueops.ErrAlreadyClaimed and issueops.ErrNotClaimable
// (and the *issueops.ClaimConflictError that wraps either one over the wire)
// are both folded into that false verdict by nativeClaimConflict.
//
// A MISSING BEAD IS ErrNotFound, NOT A CONFLICT — this is the one place Claim
// deliberately parts ways with ReleaseIfCurrent, which folds a not-found row
// into its own false-verdict idiom because "nobody holds it" is the same fact
// release already reports for an unclaimed row. A claim has no equivalent
// benign reading of "the bead does not exist": it is a caller error every
// other Store.Claim implementation already surfaces as an error (see
// SQLiteStore.Claim, BdStore.Claim), so this front door does too, via the
// ordinary nativeStoreError not-found translation every other role accessor
// in this file uses.
//
// A WISP ID GETS A NAMED REFUSAL, ErrWispNotClaimable, NOT A BARE ErrNotFound.
// issueops.Claimer's CAS addresses only the issues table, so the claimer
// reports a wisp id with the same ErrNotFound sentinel it uses for an id that
// does not exist anywhere. The two are not the same fact for a caller: "this
// id is a wisp, no front door serving this role can ever claim it" calls for
// a different response (log it once and move on) than "this id does not
// exist" (a caller bug or a stale reference worth surfacing loudly).
//
// Telling them apart takes a second read. When the claimer's error resolves
// to issueops.ErrNotFound, nativeClaimTargetIsWisp asks the reader role for
// id; unlike the claimer, the reader resolves the wisp table too
// (issueops.Reader.Get: "A miss — for both the issue and the wisp table — is
// ErrNotFound"). Claim reports ErrWispNotClaimable only when that read returns
// a row carrying the wisp-plane marker (Ephemeral or NoHistory). The two reads
// are not atomic, so the verdict describes the row the second read observed:
// an ordinary issue created or promoted between them carries no marker and
// keeps the claimer's ErrNotFound. A miss or a failed read keeps it too, so
// the disambiguating read never manufactures a wisp refusal. Either way the
// window is benign: the claim wrote nothing, and the caller's next attempt
// sees the current row.
//
// THE DISAMBIGUATING READ GOES THROUGH THE storage HANDLE AND ctx CLAIM
// ALREADY HOLDS, NEVER THROUGH s.Get. Claim still holds s.mu.RLock here
// (acquireStorage above, released only on return via the deferred release()).
// s.Get funnels through withReadRetry, which re-takes s.mu via
// acquireStorageGen and, on a transient read error, reconnects via
// s.mu.Lock() — both of which are a second, nested lock request from the SAME
// goroutine that already holds the outer RLock. Go's sync.RWMutex is not
// reentrant and gives a blocked Lock() writer priority over new readers, so
// either shape self-deadlocks the goroutine permanently: a pending writer (a
// reconnect swap or CloseStore racing in from elsewhere) blocks the nested
// RLock forever behind itself, and a transient failure on the disambiguating
// read alone reaches reconnect's s.mu.Lock() while this same goroutine's outer
// RLock is still held, which can never be granted. Reading directly off the
// storage and ctx Claim already has avoids taking s.mu a second time at all,
// so neither shape can occur; a transient error here keeps the claimer's
// ErrNotFound (see the paragraph above) rather than opening a retry/reconnect
// path.
func (s *NativeDoltStore) Claim(id, assignee string) (Bead, bool, error) {
	if err := s.readOnlyGuard(); err != nil {
		return Bead{}, false, err
	}
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return Bead{}, false, fmt.Errorf("claiming bead %q: empty assignee", id)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, false, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	claimer, err := storage.IssueClaimer()
	if err != nil {
		return Bead{}, false, nativeStoreError(id, err)
	}
	result, err := claimer.Claim(ctx, issueops.ClaimRequest{Actor: assignee, IssueID: id})
	if err != nil {
		if nativeClaimConflict(err) {
			return Bead{}, false, nil
		}
		if errors.Is(err, issueops.ErrNotFound) {
			if nativeClaimTargetIsWisp(ctx, storage, id) {
				return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, ErrWispNotClaimable)
			}
		}
		return Bead{}, false, nativeStoreError(id, err)
	}
	if result.Issue == nil {
		return Bead{}, false, fmt.Errorf("claiming bead %q: claim role returned no issue", id)
	}
	bead, err := beadFromNativeIssue(result.Issue)
	if err != nil {
		return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, err)
	}
	return bead, true, nil
}

// nativeClaimTargetIsWisp asks the reader role for id using the storage handle
// and ctx Claim already holds, and reports whether it read a row carrying the
// wisp-plane marker: Ephemeral or NoHistory, the flags the beads library infers
// a stored row's plane from. It deliberately does NOT go through
// s.Get/s.withReadRetry: see the "THE DISAMBIGUATING READ" paragraph on
// Claim's doc comment above for why a second, nested acquisition of s.mu from
// the same goroutine self-deadlocks. Any failure here (including one that
// would ordinarily reconnect and retry) reports false, so Claim keeps the
// claimer's ErrNotFound rather than guessing.
func nativeClaimTargetIsWisp(ctx context.Context, storage beadslib.Storage, id string) bool {
	reader, err := storage.IssueReader()
	if err != nil {
		return false
	}
	details, err := reader.Get(ctx, issueops.GetRequest{ID: id})
	return err == nil && details != nil && (details.Ephemeral || details.NoHistory)
}

// ErrWispNotClaimable names the refusal Claim reports for an id that exists
// only in the wisp plane: see the WISP ID note on Claim above for why this is
// distinguished from a bare ErrNotFound.
var ErrWispNotClaimable = errors.New("wisp not claimable through this role")

// nativeClaimConflict reports the refusals that mean "this claim was not won"
// — a different actor already holds the bead, or its status is not eligible
// to be claimed from. Both issueops.ErrAlreadyClaimed and
// issueops.ErrNotClaimable match through errors.Is whether err is the bare
// sentinel or a *issueops.ClaimConflictError wrapping it (ClaimConflictError
// unwraps to its Err field), which is how the http client reconstructs a 409.
// Neither is issueops.ErrNotFound: see the not-found note on Claim above.
func nativeClaimConflict(err error) bool {
	return errors.Is(err, issueops.ErrAlreadyClaimed) ||
		errors.Is(err, issueops.ErrNotClaimable)
}

// Close sets a bead's status to closed through the upstream issue-operations
// facade. The close reason is still read from the bead's own metadata, which is
// where Gas City stamps it before closing.
func (s *NativeDoltStore) Close(id string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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

// closeOnce performs one complete close attempt, read included, so a retry
// decides from freshly read state instead of replaying a stale one.
func (s *NativeDoltStore) closeOnce(ctx context.Context, storage beadslib.Storage, id string) error {
	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	// A replay re-reads, so a bead another actor closed during the backoff
	// must not be closed again.
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return nativeStoreError(id, err)
	}
	// Force keeps the storage-layer close's policy-free semantics: the caller
	// that decided to close is the orchestrator, and a molecule root routinely
	// closes with children still open.
	if _, err := ops.Close(ctx, issueops.CloseRequest{
		Actor:   s.actor,
		IssueID: id,
		Reason:  nativeCloseReasonFromIssue(current),
		Force:   true,
	}); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// Reopen sets a closed bead's status back to open through the upstream
// issue-operations facade.
func (s *NativeDoltStore) Reopen(id string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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

// reopenOnce performs one complete reopen attempt, read included, so the
// already-open short-circuit reflects the state this attempt observed.
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
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return nativeStoreError(id, err)
	}
	_, err = ops.Reopen(ctx, issueops.ReopenRequest{Actor: s.actor, IssueID: id})
	return nativeStoreError(id, err)
}

// CloseAll closes multiple beads and sets metadata on each bead it is given.
//
// The batch route is the one it takes; see native_dolt_store_batch_close.go for
// what it costs and what it preserves — including that the metadata stamp
// reaches a bead that is ALREADY CLOSED, on either route. A caller that must
// leave a finished bead's metadata alone drops its closed ids before calling,
// as the workflow skip and delete paths do.
//
// The per-bead route below it is the fallback for a backing that cannot apply a
// batch, and the route is decided by ASKING rather than by a capability
// handshake, exactly as Tx decides its own: such a backend says so and writes
// nothing. batchRouteUnavailable is what recognizes that answer — see its own
// doc.
//
// `closed > 0` is that fallback's safety fence, the `entered` of Tx's. A
// refusal is a fact about the backing, so it arrives on the first chunk or not
// at all; a refusal raised AFTER a chunk has landed is something else, and
// re-walking the whole input under the loop would report only what the
// remaining chunks closed.
func (s *NativeDoltStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if err := s.readOnlyGuard(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	closed, err := s.closeAllAsBatch(ids, metadata)
	if err == nil {
		return closed, nil
	}
	if closed > 0 || !batchRouteUnavailable(err) {
		return closed, err
	}
	return s.closeAllOneAtATime(ids, metadata)
}

// closeAllOneAtATime closes each bead with its own read and its own writes.
//
// The metadata stamp and the close are a CHAIN: gc puts close_reason into that
// metadata and the close carries the reason. So the second write needs a value
// the first one wrote, and this loop used to recover it with a follow-up read —
// Close's own GetIssue, dialed on top of the status read the loop already
// makes, to fetch back what the update had just committed.
//
// That read is gone. The lifecycle role answers a write with its post-state
// snapshot, and issueops.UpdateResult.Issue is the member it rides on — the
// same one that carries the post-write RowVersion a guarded chain composes its
// next ExpectedVersion from. Reading the reason
// there is also the more correct of the two: it is the state the write
// committed, where a re-read is whatever the row holds by the time it lands.
//
// The no-metadata arm keeps Close's own read, because there is no chain in it:
// nothing in the call wrote a reason, so the row is the only place one can come
// from and that read is the only one Close makes.
//
// An already-closed row is stamped and not re-closed, which is what the batch
// route does to it (see "THE STAMP REACHES ALREADY-CLOSED ROWS" there): the
// same call must not rewrite a closed bead's metadata only when the backing
// can batch.
func (s *NativeDoltStore) closeAllOneAtATime(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		current, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if current.Status == "closed" {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return closed, err
			}
			continue
		}
		if len(metadata) == 0 {
			if err := s.Close(id); err != nil {
				return closed, err
			}
			closed++
			continue
		}
		if err := s.stampAndClose(id, metadata); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// stampAndClose merges metadata onto a bead and closes it, taking the close
// reason from the update's own post-state answer.
//
// Both writes share one storage acquisition and one lifecycle accessor, which
// is what makes the pair a chain rather than two unrelated calls that happen to
// run in order. Each write replays its own serialization conflict, on a fresh
// operation context per attempt, and never the other's: a conflicted stamp
// committed nothing, so it replays exactly as SetMetadataBatch replays the same
// merge, and a conflicted close follows a stamp that did commit.
//
// A backend that answers no post-state issue is violating the role contract
// (every leg — create, update, close, reopen — is held to hydrating its
// result's post-state Issue), and the fallback for it re-reads rather than
// closing on a reason nothing produced. Closing on the pre-write value would
// be the one wrong answer available here: it is a real string that looks like
// a reason, and it is the reason this call was told to replace.
//
// THE FALLBACK RE-READS ON THE HELD HANDLE, and must, which is why it is spelled
// out here instead of delegating to Close. acquireStorage hands back
// s.mu.RUnlock as its release, so this whole body runs under a READ lock, and
// Close acquires that same lock again. Go's RWMutex forbids recursive read
// locking: a writer arriving between the two — the reconnect handle swap, or
// CloseStore — parks in front of the inner RLock and all three deadlock. The
// path is unreachable while both pinned legs hydrate, but it exists precisely
// for the backend that does not, and hanging is a worse answer than the one it
// was written to give. nativeBatchTx.Close reads on its held handle for the
// same reason.
func (s *NativeDoltStore) stampAndClose(id string, metadata map[string]string) error {
	patch, err := nativeIssuePatchFromUpdateOpts(UpdateOpts{Metadata: metadata})
	if err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ops, err := storage.IssueLifecycle()
	if err != nil {
		return nativeStoreError(id, err)
	}
	var updated issueops.UpdateResult
	if err := retryOnNativeDoltSerializationConflict(func() error {
		attemptCtx, attemptCancel := nativeDoltOperationContext(context.TODO())
		defer attemptCancel()
		var attemptErr error
		updated, attemptErr = ops.Update(attemptCtx, issueops.UpdateRequest{
			Actor:   s.actor,
			IssueID: id,
			Patch:   patch,
		})
		return attemptErr
	}); err != nil {
		return nativeStoreError(id, err)
	}
	closing := updated.Issue
	if closing == nil {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		current, err := storage.GetIssue(ctx, id)
		if err != nil {
			return nativeStoreError(id, err)
		}
		if current == nil {
			return fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
		closing = current
	}
	// Force keeps the storage-layer close's policy-free semantics, exactly as
	// Close does: a molecule root routinely closes with children still open.
	//
	// The close replays alone. The stamp above it already committed, so replaying
	// the pair would re-run a write that won its race to recover one that lost.
	reason := nativeCloseReasonFromIssue(closing)
	if err := retryOnNativeDoltSerializationConflict(func() error {
		attemptCtx, attemptCancel := nativeDoltOperationContext(context.TODO())
		defer attemptCancel()
		_, err := ops.Close(attemptCtx, issueops.CloseRequest{
			Actor:   s.actor,
			IssueID: id,
			Reason:  reason,
			Force:   true,
		})
		return err
	}); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
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

// Children returns all beads whose parent-child dependency points at parentID.
//
// It stays a composition of List rather than a door of its own: the parent walk
// is one of the two SearchIssues shapes a served store still answers, but the
// seam decision is one path per read and List is that path. The role's ParentID
// is a RECURSIVE descendant filter where gc means direct children, so the
// backing answer is a superset and ListQuery.Matches cuts it to the exact set —
// which is what it did for the raw filter's parent predicate too.
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

// SetMetadataBatch sets multiple metadata keys on a bead and leaves every other
// key as the row holds it. The store reads nothing before it writes: the facade
// merges the keys against the row inside its own write transaction. Another
// writer's commit can therefore land only inside that transaction, where Dolt's
// commit-time merge either combines the two edits or refuses the later
// transaction with a serialization conflict, which is replayed here. The
// real-store proofs are
// TestNativeDoltStoreSetMetadataBatchKeepsAnotherSessionsUpdate and
// TestNativeDoltStoreMetadataMergeSurvivesAConcurrentUpdateLoop.
func (s *NativeDoltStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if len(kvs) == 0 {
		return nil
	}
	patch, err := nativeIssuePatchFromUpdateOpts(UpdateOpts{Metadata: kvs})
	if err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()

	// The facade merges the keys inside the write transaction, so a replay
	// cannot clobber a competing writer's keys the way a stale read would.
	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		ops, err := storage.IssueLifecycle()
		if err != nil {
			return nativeStoreError(id, err)
		}
		if _, err := ops.Update(ctx, issueops.UpdateRequest{
			Actor:   s.actor,
			IssueID: id,
			Patch:   patch,
		}); err != nil {
			return nativeStoreError(id, err)
		}
		return nil
	})
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
	return retryNativeDoltWrite(attempt, isNativeDoltSerializationConflict)
}

// retryNativeDoltWrite runs attempt up to nativeWriteAttempts times, sleeping a
// growing nativeWriteRetryBackoff after each error retryable accepts. The first
// error retryable rejects, and the last attempt's error, are returned as they
// are.
func retryNativeDoltWrite(attempt func() error, retryable func(error) bool) error {
	var err error
	for n := 1; n <= nativeWriteAttempts; n++ {
		err = attempt()
		if err == nil || !retryable(err) || n == nativeWriteAttempts {
			return err
		}
		time.Sleep(time.Duration(n) * nativeWriteRetryBackoff)
	}
	return err
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
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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
	return runInNativeTransaction(ctx, storage, commitMsg, func(tx beadslib.Transaction) error {
		return fn(&nativeDoltTx{store: s, ctx: ctx, tx: tx})
	}, func() error {
		return s.runTxAsBatch(ctx, storage, commitMsg, fn)
	})
}

// runInNativeTransaction runs body inside one storage transaction, or runs
// fallback instead when the backing has no transaction to offer.
//
// The route is decided by ASKING, not by a capability handshake, because the
// question has an authoritative answer that costs nothing: a backend with no
// transaction refuses RunInTransaction with a typed *beadslib.ErrUnsupported
// and never invokes the callback. `entered` is what makes the fallback safe —
// a refusal raised after body ran is a failed transaction, not an unsupported
// one, and running fallback after it would double the writes.
func runInNativeTransaction(ctx context.Context, storage beadslib.Storage, commitMsg string, body func(beadslib.Transaction) error, fallback func() error) error {
	entered := false
	err := storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
		entered = true
		return body(tx)
	})
	var unsupported *beadslib.ErrUnsupported
	if err != nil && !entered && errors.As(err, &unsupported) {
		return fallback()
	}
	return err
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
		raw, err := metadataRawWithOverrides(issue.Metadata, opts.Metadata)
		if err != nil {
			return nil, fmt.Errorf("parsing metadata for bead %q: %w", id, err)
		}
		updates["metadata"] = raw
	}
	return updates, nil
}

// validateUpdateParent resolves a reparent target, for the ids this store could
// have minted.
//
// A foreign one is left alone for the same reason Create leaves it alone: it is
// a weak reference (beads.Bead.ParentID) and this store cannot see the row, so
// resolving it reports not-found for a bead that exists. Create and Update have
// to agree here — a store that admits a cross-store parent and then refuses to
// write the same value back is worse than one that refuses both, because the
// refusal only appears on the reparent, long after the shape was accepted.
func (s *NativeDoltStore) validateUpdateParent(ctx context.Context, storage nativeIssueGetter, id, parentID string) error {
	if strings.TrimSpace(parentID) == "" {
		return nil
	}
	if !nativeParentIsLocal(id, parentID, s.idPrefix) {
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
	// Same rule as validateUpdateParent, on the transactional path: resolve the
	// parents this store could have minted, leave a foreign one weak.
	if strings.TrimSpace(parentID) != "" && nativeParentIsLocal(id, parentID, s.idPrefix) {
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

// updateParentThroughEditor is updateParentInTransaction's twin for a backing
// with no transaction, which is the served wire's shape: it asserts id's edge
// to parentID, if parentID names one, and then clears id's other parent-child
// edges. Only rewriteExternalParent calls it, and only after
// nativeParentIsLocal has already said parentID is not a row this store
// resolves -- so unlike updateParentInTransaction, there is no
// local-existence check to run first.
//
// With nothing to roll back, the ORDER is the guarantee. The new edge goes in
// before any old one comes out, so a failure between the writes leaves a
// second parent edge a reader can see, where the reverse order left a bead
// with no parent and nothing to say so. Asserting an edge that already exists
// is a no-op upstream, which is why the sweep has to spare the edge to
// parentID: removing it would undo the assertion that just ran. The sweep
// also skips an edge of any other type, because RemoveDependency deletes the
// id pair whatever its type.
//
// EdgeReader and DependencyEditor are the facade's own front doors for this —
// the same pair DepAdd, DepRemove and DepList's DOWN leg already ride — so the
// edge write takes the same role path over a served city that every other
// graph mutation does.
func (s *NativeDoltStore) updateParentThroughEditor(ctx context.Context, storage beadslib.Storage, id, parentID string) error {
	reader, err := storage.EdgeReader()
	if err != nil {
		return nativeStoreError(id, err)
	}
	result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{
		IDs:   []string{id},
		Types: []issueops.DependencyType{issueops.DepParentChild},
	})
	if err != nil {
		return nativeStoreError(id, err)
	}
	editor, err := storage.DependencyEditor()
	if err != nil {
		return nativeStoreError(id, err)
	}
	if parentID != "" {
		if _, err := editor.AddDependencies(ctx, issueops.AddDependenciesRequest{
			Actor: s.actor,
			Edges: []issueops.DependencyEdge{{
				IssueID:     id,
				DependsOnID: parentID,
				Type:        issueops.DepParentChild,
			}},
		}); err != nil {
			return nativeStoreError(id, err)
		}
	}
	for _, anchor := range result.Anchors {
		if anchor.ID != id || anchor.Missing {
			continue
		}
		for _, edge := range anchor.Edges {
			if edge == nil || edge.Type != issueops.DepParentChild || edge.DependsOnID == parentID {
				continue
			}
			if _, err := editor.RemoveDependency(ctx, issueops.RemoveDependencyRequest{
				Actor:       s.actor,
				IssueID:     id,
				DependsOnID: edge.DependsOnID,
			}); err != nil {
				return nativeStoreError(id, err)
			}
		}
	}
	return nil
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

// nativeParentIsLocal reports whether a parent id names a row THIS store would
// hold — the only case in which resolving it is a legitimate refusal rather
// than a blind spot.
//
// Two prefixes make a parent local, and they answer different questions. The
// STORE's own mint prefix is the namespace it owns: an absent row there is an
// absence this store can see, whatever prefix the CHILD carries — a pinned id
// or a relic a storage migration copied in carries another ledger's, and
// reading the question off the child alone would call the store's own namespace
// foreign and let the reparent land dangling. The child's prefix is local too,
// because that is where the upstream library draws the line: issueops resolves
// a same-prefix dependency target itself, post-commit, with no embedder knob to
// weaken it, so agreeing with it here keeps the refusal in front of the write
// instead of behind a compensating delete.
//
// Everything else is weak. A store that declares no namespace, asked about a
// child whose own id names none, cannot tell its rows from another ledger's,
// and the weak reading is the only one that cannot refuse a bead that exists.
func nativeParentIsLocal(issueID, parentID, storePrefix string) bool {
	target := beadIDPrefix(parentID)
	if target == "" {
		return false
	}
	if store := normalizeIDPrefix(storePrefix); store != "" && target == store {
		return true
	}
	return target == beadIDPrefix(issueID)
}

// beadIDPrefix extracts the prefix segment (before the first "-") from a
// bead ID, normalized via normalizeIDPrefix. Shared across store backends
// that need to decide whether two bead IDs belong to the same store.
func beadIDPrefix(id string) string {
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

// The four helpers below are used ONLY by
// applyGraphPlanOverCapInTransaction, the over-cap local-backend fallback
// that drives a graph-apply plan by hand through beadslib.RunInTransaction
// instead of issueops.BatchApplier. BatchApplier re-validates every dep_add
// item against the parent-child closure it builds itself once every item has
// landed, raising *issueops.DependencyHierarchyConflictError or
// issueops.ErrDependencyCycle; a hand-run transaction has no such end gate,
// so this pairwise check is this route's own substitute for it.

// nativeGraphApplyCycleRelevantDependencyType reports whether depType can
// form a cycle with a parent-child edge the way DepBlocks and
// DepConditionalBlocks can — the two ordering-sensitive types this pairwise
// check cares about.
func nativeGraphApplyCycleRelevantDependencyType(depType beadslib.DependencyType) bool {
	return depType == beadslib.DepBlocks || depType == beadslib.DepConditionalBlocks
}

// nativeGraphApplyParentDepPairs collects every (child, parent) id pair this
// plan's node-level ParentKey/ParentID links will create, keyed the same way
// nativeGraphApplyDepPairKey keys an edge, so the edge loop below can detect a
// plan edge that duplicates or reverses one of them.
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

// nativeGraphApplyDepPairKey keys a directed (issueID, dependsOnID) pair for
// nativeGraphApplyParentDepPairs' set.
func nativeGraphApplyDepPairKey(issueID, dependsOnID string) string {
	return issueID + "\x00" + dependsOnID
}

// nativeGraphApplyResolveRef resolves one edge endpoint to a concrete id,
// preferring an explicit id over a key: an id names a row structurally
// (possibly one outside this plan entirely, e.g. an "external:" reference),
// while a key only ever reaches into this same plan's keyToID map.
func nativeGraphApplyResolveRef(key, id string, keyToID map[string]string) string {
	if id != "" {
		return id
	}
	if key != "" {
		return keyToID[key]
	}
	return ""
}

// nativeGraphApplyRef resolves one edge endpoint to an issueops.Ref,
// preferring an explicit id over a key exactly as the pre-BatchApplier
// implementation's nativeGraphApplyResolveRef did: an id names a row
// structurally (possibly one outside this plan entirely, e.g. an
// "external:" reference), while a key only ever reaches into this same plan.
func nativeGraphApplyRef(key, id string) issueops.Ref {
	if id != "" {
		return issueops.Ref{ID: id}
	}
	return issueops.Ref{Key: key}
}

// nativeGraphApplyParentRef resolves a node's parent link, when it has one,
// preferring ParentKey over ParentID — the same precedence the
// pre-BatchApplier implementation gave the two fields.
func nativeGraphApplyParentRef(node GraphApplyNode) (issueops.Ref, bool) {
	if node.ParentKey != "" {
		return issueops.Ref{Key: node.ParentKey}, true
	}
	if node.ParentID != "" {
		return issueops.Ref{ID: node.ParentID}, true
	}
	return issueops.Ref{}, false
}

// nativeGraphApplyBatchRequest builds the single issueops.ApplyBatchRequest a
// graph-apply plan expands to. It is PURE — no storage, no context — so its
// item count can be checked against issueops.MaxApplyBatchItems before any
// backend is touched.
//
// Item order:
//
//  1. One CreateItem per node, in plan order, carrying MetadataRefs directly
//     from node.MetadataRefs — the splice BatchApplier performs as its
//     documented second write, in place of the manual tx.UpdateIssue
//     metadata-ref pass this replaces.
//  2. One DepAddItem per plan edge.
//  3. One DepAddItem per node that names a parent, of type parent-child —
//     PARENT-ON-CREATE's one spelling, in place of the old implicit
//     node.ParentID/ParentKey handling.
//  4. One UpdateItem per node with AssignAfterCreate set and a non-empty
//     Assignee, patching Assignee — in place of the old deferred
//     pendingAssignees pass.
//
// Every CreateItem precedes every item that can reference it by key, which is
// what Ref's backward-only resolution requires; plan.Nodes' own relative
// order among themselves does not matter for this, since ALL creates land
// before any edge, parent link or assignment update regardless of which node
// produced which.
func nativeGraphApplyBatchRequest(plan *GraphApplyPlan, actor string, ephemeral, noHistory bool) (issueops.ApplyBatchRequest, error) {
	items := make([]issueops.ApplyItem, 0, 2*len(plan.Nodes)+len(plan.Edges))

	for _, node := range plan.Nodes {
		metadata, err := metadataRawFromMap(node.Metadata)
		if err != nil {
			return issueops.ApplyBatchRequest{}, fmt.Errorf("node %q: marshaling metadata: %w", node.Key, err)
		}
		issueType := beadslib.IssueType(node.Type)
		if issueType == "" {
			issueType = beadslib.TypeTask
		}
		priority := 2
		if node.Priority != nil {
			priority = *node.Priority
		}
		issue := &issueops.Issue{
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
		if node.Assignee != "" && !node.AssignAfterCreate {
			issue.Assignee = node.Assignee
		}

		create := &issueops.CreateItem{Key: node.Key, Issue: issue}
		if len(node.MetadataRefs) > 0 {
			refs := make(map[string]issueops.Ref, len(node.MetadataRefs))
			for metaKey, refKey := range node.MetadataRefs {
				refs[metaKey] = issueops.Ref{Key: refKey}
			}
			create.MetadataRefs = refs
		}
		items = append(items, issueops.ApplyItem{Kind: issueops.ItemCreate, Create: create})
	}

	for _, edge := range plan.Edges {
		items = append(items, issueops.ApplyItem{
			Kind: issueops.ItemDepAdd,
			DepAdd: &issueops.DepAddItem{
				Source:   nativeGraphApplyRef(edge.FromKey, edge.FromID),
				Target:   nativeGraphApplyRef(edge.ToKey, edge.ToID),
				Type:     nativeGraphApplyDependencyType(edge.Type),
				Metadata: edge.Metadata,
			},
		})
	}

	for _, node := range plan.Nodes {
		parentRef, ok := nativeGraphApplyParentRef(node)
		if !ok {
			continue
		}
		items = append(items, issueops.ApplyItem{
			Kind: issueops.ItemDepAdd,
			DepAdd: &issueops.DepAddItem{
				Source: issueops.Ref{Key: node.Key},
				Target: parentRef,
				Type:   beadslib.DepParentChild,
			},
		})
	}

	for _, node := range plan.Nodes {
		if !node.AssignAfterCreate || node.Assignee == "" {
			continue
		}
		items = append(items, issueops.ApplyItem{
			Kind: issueops.ItemUpdate,
			Update: &issueops.UpdateItem{
				Target: issueops.Ref{Key: node.Key},
				Patch:  issueops.IssuePatch{Assignee: issueops.Field[string]{Set: true, Value: node.Assignee}},
			},
		})
	}

	provenance := plan.CommitMessage
	if provenance == "" {
		provenance = fmt.Sprintf("gc: graph-apply %d nodes", len(plan.Nodes))
	}

	return issueops.ApplyBatchRequest{
		Actor:      actor,
		Items:      items,
		Provenance: provenance,
	}, nil
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
		CloseReason: b.CloseReason,
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
		CloseReason:          bdCloseReason(status, issue.CloseReason),
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
//
// It serves the RAW filter below, which after the read re-point has exactly one
// caller left: Count, which still dials CountIssues. The listing's own limit
// decision is nativeListLimitPushdown, and the two are not the same question —
// this one asks whether the backing's ORDER can cut an exact prefix, and that
// one also has to ask whether the backing's SET is the query's, because the
// role's request cannot carry the status, type or tier predicates.
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

// nativeIssueFilterFromListQuery renders a ListQuery as the RAW storage filter.
//
// The listing no longer uses it — List builds an issueops.ListRequest and the
// role builds the filter itself — so its one remaining caller is Count, which
// still dials the raw CountIssues. It stays a faithful rendering of the query
// rather than a mirror of the role's request on purpose: the count's own
// re-point is a separate slice, blocked on issueops.CountRequest having no
// ExcludeStatus and no IncludeClosed, and until then this is what "everything
// not closed" is spelled as.
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

// metadataRawWithOverrides merges overrides into a bead's stored metadata
// document, leaving every value the caller did not name byte-identical.
//
// Metadata is a single JSON column, so setting one key means rewriting the
// whole document. Decoding it into map[string]string first (as
// metadataMapFromNative does, correctly, for callers that want Go strings)
// renders each non-string value as JSON text, and writing that back persists
// the rendering: a bead holding {"n": 42} became {"n": "42"} once any
// unrelated key was set. Decoding into json.RawMessage keeps untouched values
// exactly as stored. Named keys are written as JSON strings, matching the
// map[string]string that the store's write API accepts.
func metadataRawWithOverrides(raw json.RawMessage, overrides map[string]string) (json.RawMessage, error) {
	values := make(map[string]json.RawMessage)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("unmarshaling metadata: %w", err)
		}
		if values == nil {
			values = make(map[string]json.RawMessage, len(overrides))
		}
	}
	for key, value := range overrides {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata value %q: %w", key, err)
		}
		values[key] = encoded
	}
	if len(values) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	return encoded, nil
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
		text, err := metadataValueText(v)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata value %q: %w", k, err)
		}
		metadata[k] = text
	}
	return metadata, nil
}

// metadataValueText renders one decoded metadata value the way the
// string-valued Store projection shows it: a JSON string as its own text, and
// anything else as its JSON re-encoding (so a stored null reads as "null" and a
// stored 1.50 as "1.5").
func metadataValueText(value interface{}) (string, error) {
	if s, ok := value.(string); ok {
		return s, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
