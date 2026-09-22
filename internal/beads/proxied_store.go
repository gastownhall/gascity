package beads

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// ProxiedStore is the split store of the proxied-native lane: every READ on a
// native library handle over bd's proxy data port, every MUTATION on the bd CLI
// leaf.
//
// # Why a wrapper and not a leaf
//
// The obvious shape — one hand-written read-only leaf — is the shape
// splittest/strict_store.go's package doc warns about, and the concrete loss is
// measurable: CachingStore discovers three of its backing's capabilities through
// PACKAGE-PRIVATE interfaces (listDependencyCompletenessStore,
// cacheDependencySnapshotStore, readyProjectionEnrichmentStore, caching_store.go
// 1440-1450), so a leaf that forgot one would silently turn a complete dependency
// snapshot into N per-bead DepList calls, or drop the is_blocked projection and
// send every readiness read live. A wrapper cannot forget them by accident,
// because the capability set is a compile-time contract right below this comment.
//
// # What the two leaves disagree about
//
// The split is not "the same store twice". Reads and writes land on different
// code with different semantics, and the plan's hazard register enumerates where.
// Each bound is implemented here and named at its call site:
//
//   - H1 wisps: a native MISS on a bead-shaped id falls through to the bd leaf's
//     Get, which has the `bd query --ephemeral` fallback. One fork, only on a
//     miss.
//
//     The MECHANISM this register used to state was wrong, and correcting it
//     matters because PR3 inherits the model (council B-F6). It said "the
//     native Get does not set IncludeEphemeral, so a wisp-tier bead is
//     invisible to it". IncludeEphemeral is not a field of types.IssueFilter at
//     all — it belongs to types.WorkFilter, GetReadyWork's filter, which is why
//     Ready is the only native path that sets it. SearchIssues routes on
//     filter.Ephemeral and filter.SkipWisps, and the native Get
//     (native_dolt_store.go's Get: IDs plus IncludeDependencies) sets NEITHER,
//     so searchInTx takes the nil-Ephemeral branch and MERGES the wisps table.
//     gc never sets SkipWisps anywhere outside a test.
//
//     So H1 is not a regression on this lane: the native Get can see a wisp,
//     and the fallback below is a safe no-op on a hit and matches BdStore's own
//     behavior on a miss. It is kept because it costs nothing when the native
//     read answers, and because "the bd leaf can answer a not-found that the
//     native leaf could not" stays true for reasons other than wisps — a
//     relocated class, a bead the pool's generation cannot see. See Get.
//
//   - H2 one sidecar: localSidecar.ensureLoadedLocked latches loaded=true on
//     first use and never re-reads, so two instances over one file diverge for the
//     process lifetime. The wrapper owns ONE — the bd leaf's — and points the
//     native leaf at the same object at construction. See NewProxiedStore.
//
//   - H3 AtomicTx: the native leaf answers true and the bd leaf is staged and
//     non-atomic. Tx runs on the bd leaf, so AtomicTx follows the bd leaf. See
//     AtomicTx.
//
//   - H4 graph-apply: *NativeDoltStore declares StorageGraphApplyStore and
//     EphemeralGraphApplyStore, and cmd/gc's wrapStoreWithBeadPolicies picks
//     beads.GraphApplyFor(store) at WRAP time — so a claim here would route
//     policy-selected ephemeral graph creates into a native write. The wrapper
//     deliberately implements neither and hands out the bd leaf's applier. See
//     proxied_store_capabilities.go.
//
//   - H5 relocated classes: BdStore.guardRelocatedClassIDs turns an id-scoped
//     read of a class this ledger no longer serves into a typed refusal;
//     native_dolt_store.go has no equivalent, so the same read would come back a
//     silent not-found. The guard runs before any id-scoped read is forwarded to
//     the native leaf. See guardNativeIDs.
//
//   - H6 generation split across a write: bd restarts a stopped proxy as a NEW
//     generation while the native pool still points at the old port. A mutation
//     is bracketed by a 200-byte record read, and a generation change stands the
//     native leaf down. See withMutation.
//
//     "A mutation", not "every mutation", and the difference is council B-F3
//     and, one level finer, council pr2 D-F8.
//
//     INSIDE the bracket: every method on the beads.Store surface; every write
//     capability this wrapper implements as a method (ReleaseIfCurrent,
//     DeleteBatch, CreateWithForeignID, CreateWithStorage, …); and ONE
//     capability handle, ConditionalWriterHandle — but only when it is reached
//     by beads.ConditionalWriterFor on the *ProxiedStore itself, through which
//     UpdateIfMatch, CloseIfMatch, DeleteIfMatch and CompareAndSetMetadataKey
//     are bracketed.
//
//     OUTSIDE it, deliberately: every conditional write reached through a
//     resolver that follows ConditionalWritesResolveTarget first. That is
//     ResolveConditionalWriter (internal/molecule, internal/dispatch,
//     cmd/gc/api_state.go), MetadataCASWriterFor (ApplyMetadataCAS,
//     internal/storebinding's metadata CAS), AtomicConditionalCloserFor — so
//     CloseWithMetadataIfMatch is outside on EVERY path — and every
//     conditional method of a CachingStore over this wrapper, via
//     conditionalBacking(). The target must stay the bd leaf: a wrapper that
//     answered "me" would have to carry the conditional-writes stamp, the
//     capability prober and the state inspector too — a hand-written
//     capability leaf, which is the shape this store is a wrapper to avoid.
//     B-F3 added bracketing adapters for MetadataCASWriterHandle and
//     AtomicConditionalCloserHandle as well; no resolver could reach them, so
//     they were deleted rather than left to be counted as coverage. Also
//     outside: GraphApplyHandle, which must hand out the bd leaf's own applier,
//     because H4 forbids this wrapper claiming any graph-apply interface and
//     cmd/gc's wrapStoreWithBeadPolicies caches the applier at WRAP time.
//
//     On every path outside, the ROUTING is correct (the write is the bd
//     leaf's) and the read-only latch still refuses a native write; what is
//     lost is the staleness half, bounded by the guard tick on a long-lived
//     store and by the next bracketed mutation otherwise.
//     TestProxiedStoreConditionalResolveTargetIsTheDocumentedGap pins both
//     lists.
//
// # Demotion
//
// A demoted store is one whose native leaf is closed and nil; its reads serve
// from the bd leaf, which is exactly the store a proxied scope has today. The
// wrapper never promotes itself: a handle that dropped to bd stays there for its
// own lifetime, and recovery is a fresh open (or, for a long-lived store, the
// guard tick's re-pin — which is admitted only after a NON-terminal stand-down;
// see standDown).
type ProxiedStore struct {
	mu sync.RWMutex
	// native is the read leaf. Nil means demoted: reads go to bd.
	native *NativeDoltStore
	// bd is the write leaf, and the read leaf after a demotion.
	bd Store
	// pin is the admission pass this store opened against.
	pin Pin
	// verdict is why the native leaf stood down, nil while it is serving.
	verdict *ProxiedVerdictError
	// terminal latches a stand-down that must never be re-pinned. It is what
	// makes demotion one-way for a fact (schema skew, a foreign record) while
	// leaving the door open for the guard tick's re-pin after a generation
	// change, which is not a defect at all.
	terminal bool
	// sidecar is the ONE clone-local sidecar both leaves share (H2).
	sidecar *localSidecar
	// root and database are the record identity H6's bracket compares against.
	// They are copied out of the pin so the bracket never takes mu.
	root     string
	database string
	// guard is the long-lived guard ticker, stopped by CloseStore and replaced
	// (after stopping the old one) by a second StartGuard. Nil for a one-shot
	// store.
	guard *proxiedGuard
	// reopenNative is the ONLY recovery path a non-terminally demoted handle
	// has. See NativeLeafReopener.
	reopenNative NativeLeafReopener
}

// NativeLeafReopener re-runs admission and opens a FRESH native read leaf.
//
// It exists because a non-terminal stand-down had no recovery at all (council
// A-F1). The wrapper's read path recovers through the NATIVE handle's reopen
// hook, and that hook is reachable only from a read served by the native leaf —
// so once the leaf is nil every read goes to bd, the hook is unreachable, and
// the store forks bd for the rest of the process. That is the outcome design
// U22 forbids for the endpoint-in-motion verdicts (proxy_gone, draining,
// budget_exhausted), which are exactly the ones a loaded box produces.
//
// It returns a leaf and the pin it was admitted against, so the caller can
// install both atomically: a leaf without its pin would leave the generation
// the mutation bracket compares against pointing at the old proxy.
//
// The caller (cmd/gc's opener) supplies one only for a LONG-LIVED store, which
// is the only store with a guard to call it.
type NativeLeafReopener func(ctx context.Context) (*NativeDoltStore, Pin, error)

// ProxiedStoreOption configures a split store at construction.
type ProxiedStoreOption func(*ProxiedStore)

// WithNativeLeafReopener installs the recovery path for a non-terminal
// stand-down. See NativeLeafReopener.
func WithNativeLeafReopener(reopen NativeLeafReopener) ProxiedStoreOption {
	return func(s *ProxiedStore) { s.reopenNative = reopen }
}

// nativeReopener returns the recovery path, or nil when none was installed.
func (s *ProxiedStore) nativeReopener() NativeLeafReopener {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reopenNative
}

// localSidecarCarrier is how the wrapper reaches the ONE clone-local sidecar
// both leaves must share.
//
// It is unexported and its method is unexported, so only internal/beads types can
// satisfy it — the same protection conditionalWritesModeCarrier relies on. A
// leaf from another package cannot smuggle a second sidecar into the split.
type localSidecarCarrier interface {
	localSidecarHandle() *localSidecar
}

// NewProxiedStore builds the split store from an admitted pin, a native read leaf
// and the bd write leaf.
//
// It refuses an unadmitted pin. The pin's fields are unexported and only Admit
// mints a non-zero one, so this is the second half of "the opener cannot be
// reached without a gate pass": a caller who built the env map by hand has
// nothing to pass here.
//
// The single act of surgery is H2's: the native leaf is re-pointed at the bd
// leaf's sidecar. Both leaves are constructed with their own
// newLocalSidecar(<scope>/.beads/local-strings.json) over the SAME path, and
// ensureLoadedLocked never re-reads after the first use — so a SetLocalString
// through one and a GetLocalString through the other would disagree for the life
// of the process. Sharing the object is the only fix that does not require
// re-reading a file on every access.
func NewProxiedStore(native *NativeDoltStore, bd Store, pin Pin, opts ...ProxiedStoreOption) (*ProxiedStore, error) {
	if native == nil {
		return nil, errors.New("proxied store: native read leaf is nil")
	}
	if bd == nil {
		return nil, errors.New("proxied store: bd write leaf is nil")
	}
	if !pin.Admitted() {
		return nil, errors.New("proxied store: pin was not admitted; only beads.Admit mints one")
	}
	store := &ProxiedStore{
		native:   native,
		bd:       bd,
		pin:      pin,
		root:     pin.Root(),
		database: pin.Database(),
	}
	// H2. The bd leaf owns the sidecar because it is the leaf that WRITES
	// local strings, and because it is the leaf that survives a demotion.
	if carrier, ok := bd.(localSidecarCarrier); ok {
		if sidecar := carrier.localSidecarHandle(); sidecar != nil {
			store.sidecar = sidecar
			native.localStrings = sidecar
		}
	}
	if store.sidecar == nil {
		// A bd leaf that carries no sidecar (a test double) leaves the native
		// leaf's own in place rather than losing local strings entirely.
		store.sidecar = native.localStrings
	}
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

// ProxiedPrefixAgreement is H10, and it is exported because it is checked in
// two places: once by the opener with both handles fresh, and again by the
// guard tick's recovery when it installs a replacement native leaf.
//
// The bd leaf's prefix comes from the scope's config; the native leaf's comes
// from the database's own issue_prefix row. CachingStore filters foreign bead
// events by the backing store's prefix, and the two leaves of a split that
// disagreed would classify the same bead differently depending on which leaf
// answered. An empty prefix on either side is not a disagreement: an unfenced
// store is the shipped default for scopes an operator never configured.
//
// One implementation rather than two, because a re-pin that applied a DIFFERENT
// rule from the open is a split whose halves can disagree only after a proxy
// restart — the hardest shape to reproduce and the least likely to be noticed.
func ProxiedPrefixAgreement(database string, native *NativeDoltStore, bd Store) error {
	if native == nil {
		return nil
	}
	nativePrefix := native.IDPrefix()
	bdPrefix := ""
	if reporter, ok := bd.(interface{ IDPrefix() string }); ok {
		bdPrefix = reporter.IDPrefix()
	}
	if nativePrefix == "" || bdPrefix == "" || nativePrefix == bdPrefix {
		return nil
	}
	return NewProxiedVerdictError(ProxiedVerdictPrefixMismatch, fmt.Sprintf(
		"database %s mints %q and the bd front door mints %q", database, nativePrefix, bdPrefix), nil)
}

// Pin returns the admission pass this store opened against.
func (s *ProxiedStore) Pin() Pin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pin
}

// Demoted reports whether the native read leaf has stood down, so reads and
// writes are both on the bd leaf.
//
// It is exported for cmd/gc's scopedStoreLike (P2-13): a DEMOTED wrapper's
// session reads fork bd, and those forks must be rebuilt ctx-bound or a
// command's deadline abandons a live child instead of killing it. While native,
// the same code must answer "nothing to clone" so the reads stay on this store
// and cost zero forks.
func (s *ProxiedStore) Demoted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.native == nil
}

// BdLeaf returns the bd CLI leaf. It is exported for the same caller as
// Demoted: rebuilding a ctx-bound clone needs the store that does the forking.
func (s *ProxiedStore) BdLeaf() Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bd
}

// Verdict returns why the native leaf stood down, or nil while it is serving.
func (s *ProxiedStore) Verdict() *ProxiedVerdictError {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.verdict
}

// Report projects this store's current state onto the factory's diagnostic
// shape, so doctor can see the pin AND the demotion in one place.
func (s *ProxiedStore) Report() ProxiedOpenReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	report := s.pin.Report()
	report.Demoted = s.native == nil
	return report
}

// readLeaf returns the store reads go to: the native leaf while it is serving,
// the bd leaf after a stand-down.
func (s *ProxiedStore) readLeaf() Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.native != nil {
		return s.native
	}
	return s.bd
}

// nativeLeaf returns the native read leaf, or nil when demoted.
func (s *ProxiedStore) nativeLeaf() *NativeDoltStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.native
}

// writeLeaf returns the bd leaf. Every mutation goes here, in both states: PR2's
// native handle is read-only latched (P2-06) and there is no promotion.
func (s *ProxiedStore) writeLeaf() Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bd
}

// standDown closes the native leaf and records why.
//
// It is idempotent and ONE-WAY for a terminal verdict. The distinction matters
// and it is the one place this file departs from a flat "never promote":
//
//   - A terminal verdict is a FACT about the database, the record or the policy
//     (schema_skew, not_ours, database_gone, access_denied, prefix_mismatch).
//     Re-pinning could only re-learn it, so the latch is permanent for this
//     handle and a fresh open is the only recovery.
//   - A non-terminal stand-down is the endpoint in MOTION (proxy_gone, draining,
//     budget_exhausted). The design's guard tick answers a generation change by
//     RE-PINNING, not demoting (U22, 648-654), so latching those permanently
//     would convert every ordinary bd proxy restart into a store that forks for
//     the rest of the process.
//
// The native handle is closed detached (closeStorageQuietly's reason: a handle
// whose server was hard-killed can wedge on Close). The pointer is dropped
// under mu, which is what makes the DECISION atomic; it does not mean no reader
// holds it — readLeaf and nativeLeaf hand the pointer out and release the lock
// before the call, so a read already in flight keeps its own reference. See
// classifyReadError for what that read's follow-on error is worth.
func (s *ProxiedStore) standDown(verdict *ProxiedVerdictError) {
	if verdict == nil {
		return
	}
	s.mu.Lock()
	native := s.native
	s.native = nil
	// The recorded REASON is latched with the handle (council A-F8). standDown
	// used to write s.verdict unconditionally and only OR the terminal flag, so
	// a later non-terminal stand-down overwrote the fact that had already
	// decided this handle's fate — reachable from withMutation's bracket, which
	// still runs after a demotion because it does not consult s.native, and from
	// checkGeneration's markPoolStale arm. The handle stayed demoted either way,
	// so this was never a resurrection; what it was is LiveProxiedDiagnostic and
	// the doctor payload telling an operator "verdict=proxy_gone
	// terminal=false" for a city that actually failed the schema gate.
	if !s.terminal {
		s.verdict = verdict
	}
	if verdict.Terminal() {
		s.terminal = true
	}
	s.mu.Unlock()
	if native != nil {
		// CloseStore is the terminal latch on the native handle, and it can
		// block on a wedged connection, so it runs off the caller's path.
		go func() { _ = native.CloseStore() }()
	}
}

// repin installs a freshly admitted native leaf after a NON-terminal stand-down.
//
// It is the guard tick's move (P2-11) and it refuses after a terminal one, which
// is what keeps "demotion is one-way" true for every verdict that is a fact. It
// reports whether the swap landed.
func (s *ProxiedStore) repin(native *NativeDoltStore, pin Pin) bool {
	if native == nil || !pin.Admitted() {
		return false
	}
	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return false
	}
	previous := s.native
	s.native = native
	s.pin = pin
	s.root = pin.Root()
	s.database = pin.Database()
	s.verdict = nil
	if s.sidecar != nil {
		native.localStrings = s.sidecar
	}
	s.mu.Unlock()
	if previous != nil && previous != native {
		go func() { _ = previous.CloseStore() }()
	}
	return true
}

// classifyReadError stands the native leaf down when a read produced a typed
// verdict, and returns the error either way.
//
// This is the whole demotion trigger on the read path, and it is why P2-08 exists:
// withReadRetry wraps a failed read twice with %w before returning it, so the
// verdict is recovered with errors.As and never with a type assertion. A read that
// failed for an ordinary reason (ErrNotFound, a decode failure) carries no verdict
// and changes nothing.
func (s *ProxiedStore) classifyReadError(err error) error {
	if err == nil {
		return nil
	}
	if verdict, ok := ProxiedVerdictOf(err); ok {
		// A head_moved verdict here came from the reopen hook's library open,
		// and it is an incident, not a refusal: without this it was one
		// caller's read error and a silent stand-down (council pr2 E-S2).
		logProxiedHeadMoved(nil, s.scopeRootForPin(), ProxiedIncidentSiteReadReopen, verdict)
		s.standDown(verdict)
	}
	return err
}

// guardRelocatedIDs applies the bd leaf's relocated-class refusal to an id-scoped
// read before it is forwarded to the native leaf (H5).
//
// The guard is the bd leaf's because the bd leaf is the one that KNOWS: the
// relocated classes are declared on it at construction
// (WithBdStoreRelocatedClasses), and native_dolt_store.go has no equivalent
// declaration and no equivalent refusal. Without this, relocating the graph class
// and then asking a proxied work store for a gcg- id would turn a typed "ask the
// other store" into a silent not-found — the exact conflation bdsql_relocation.go
// exists to prevent.
//
// It runs only on the native path. After a demotion the bd leaf answers with its
// OWN semantics, unmodified, which is the demotion contract: a demoted wrapper
// behaves like the BdStore a proxied scope has today, including where that store
// guards and where it does not.
func (s *ProxiedStore) guardRelocatedIDs(op string, ids ...string) error {
	guard, ok := s.writeLeaf().(relocatedClassGuard)
	if !ok {
		return nil
	}
	return guard.guardRelocatedClassIDs(op, ids...)
}

// relocatedClassGuard is BdStore's id-scoped relocated-class refusal, reached
// through an unexported interface so only internal/beads types can supply one.
type relocatedClassGuard interface {
	guardRelocatedClassIDs(op string, ids ...string) error
}

// withMutation runs a write on the bd leaf inside H6's generation bracket.
//
// The hazard is steady state, not a corner: bd restarts a stopped proxy — after
// an operator `bd dolt stop`, after an idle expiry — as a NEW generation on a new
// port, and the write that triggered the restart succeeds while the native pool
// keeps pointing at the old one. Every later read would then be served by a
// socket nobody validated, or fail with a transport error whose cause looks like
// load.
//
// The bracket is two 200-byte file reads around the write. On a change the native
// leaf stands down NON-terminally and the memoized pin is dropped, so the next
// open (or the guard tick) re-admits against whatever bd produced. No ping is
// spent: the write just proved bd owns a live generation, and asking bd to make
// its proxy healthy would be asking about something that demonstrably is.
//
// A record that cannot be read at all is NOT treated as a change. bd removes the
// record on an orderly stop and rewrites it on start, so an unreadable record
// mid-write is as likely to be that window as a real move; standing down on it
// would demote a healthy store for a file that reappears a millisecond later.
func (s *ProxiedStore) withMutation(op string, write func(Store) error) error {
	before, beforeOK := s.currentGeneration()
	err := write(s.writeLeaf())
	if after, afterOK := s.currentGeneration(); beforeOK && afterOK && !before.SameGeneration(after) {
		s.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone,
			fmt.Sprintf("the proxy generation changed across %s (%s -> %s); re-admitting on the next open",
				op, before.Generation(), after.Generation()), nil))
		ForgetProxiedPin(s.scopeRootForPin(), s.database)
	}
	return err
}

// currentGeneration reads bd's ownership record and derives the pool key. The
// bool reports whether the record was readable at all.
func (s *ProxiedStore) currentGeneration() (proxyendpoint.PoolKey, bool) {
	if s.root == "" {
		return proxyendpoint.PoolKey{}, false
	}
	record, err := proxyendpoint.Read(s.root)
	if err != nil {
		return proxyendpoint.PoolKey{}, false
	}
	return proxyendpoint.NewPoolKey(record, s.database), true
}

// scopeRootForPin is the key the pin memo is stored under. Admit keys the memo on
// the SCOPE root, not on bd's proxy root (one proxy root legitimately serves
// several scopes), so a mutation that invalidates the memo has to use the same
// key.
func (s *ProxiedStore) scopeRootForPin() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pin.ScopeRoot()
}

// ---------------------------------------------------------------------------
// Reads. Native while it is serving, bd after a stand-down.
// ---------------------------------------------------------------------------

// Get reads one bead, with H1's fallback.
//
// The fallback is gated on a MISS and on the same id shape BdStore gates its own
// wisp query on (isWispQueryableID), so the cost is one fork on a not-found and
// nothing at all on a hit.
//
// What it is NOT is a fix for a native Get that cannot see wisps, and this
// comment used to say it was (council B-F6). At beads v1.3.0 the native Get
// builds IssueFilter{IDs, IncludeDependencies} and sets neither Ephemeral nor
// SkipWisps, so issueops.searchInTx takes its `filter.Ephemeral == nil` branch
// and merges the wisps table: a wisp IS visible to it. IncludeEphemeral, the
// field the old comment named, is on types.WorkFilter — GetReadyWork's filter —
// which is why Ready is the only native path in this package that sets one.
//
// The fallback stays because it costs nothing on a hit and because a bd leaf
// can still answer a not-found the native leaf could not for reasons that have
// nothing to do with wisps. What goes is the false premise: H1 is not a
// regression this lane introduces, and a reader — or PR3 — must not inherit the
// belief that the native read path is blind to the wisps plane.
func (s *ProxiedStore) Get(id string) (Bead, error) {
	native := s.nativeLeaf()
	if native == nil {
		return s.bd.Get(id)
	}
	if err := s.guardRelocatedIDs("get "+id, id); err != nil {
		return Bead{}, err
	}
	bead, err := native.Get(id)
	if err == nil {
		return bead, nil
	}
	if errors.Is(err, ErrNotFound) && isWispQueryableID(id) {
		// H1: the native leaf cannot see the wisps table. Ask the leaf that can.
		if wisp, wispErr := s.writeLeaf().Get(id); wispErr == nil {
			return wisp, nil
		}
		return Bead{}, err
	}
	return Bead{}, s.classifyReadError(err)
}

// List reads beads matching a query.
func (s *ProxiedStore) List(query ListQuery) ([]Bead, error) {
	beads, err := s.readLeaf().List(query)
	return beads, s.classifyReadError(err)
}

// ListOpen reads non-closed beads (legacy helper).
func (s *ProxiedStore) ListOpen(status ...string) ([]Bead, error) {
	beads, err := s.readLeaf().ListOpen(status...)
	return beads, s.classifyReadError(err)
}

// Ready reads actionable work.
//
// H11 (bd's `ready` runs WakeExpiredDefers first and the native ready path does
// not, so a bead deferred into the past may be omitted until any bd command runs)
// is pre-existing on every direct native scope and is accepted and documented for
// this lane too. No code here: waking deferrals would be a WRITE, which is the one
// thing this lane must not do.
func (s *ProxiedStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	beads, err := s.readLeaf().Ready(query...)
	return beads, s.classifyReadError(err)
}

// Children reads a parent's children.
func (s *ProxiedStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	beads, err := s.readLeaf().Children(parentID, opts...)
	return beads, s.classifyReadError(err)
}

// ListByLabel reads beads carrying an exact label.
func (s *ProxiedStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	beads, err := s.readLeaf().ListByLabel(label, limit, opts...)
	return beads, s.classifyReadError(err)
}

// ListByAssignee reads one agent's beads at a status.
func (s *ProxiedStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	beads, err := s.readLeaf().ListByAssignee(assignee, status, limit)
	return beads, s.classifyReadError(err)
}

// ListByMetadata reads beads whose metadata matches every filter.
func (s *ProxiedStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	beads, err := s.readLeaf().ListByMetadata(filters, limit, opts...)
	return beads, s.classifyReadError(err)
}

// DepList reads one bead's dependency edges. Id-scoped, so H5's guard applies.
func (s *ProxiedStore) DepList(id, direction string) ([]Dep, error) {
	if native := s.nativeLeaf(); native != nil {
		if err := s.guardRelocatedIDs("dep list "+id, id); err != nil {
			return nil, err
		}
		deps, err := native.DepList(id, direction)
		return deps, s.classifyReadError(err)
	}
	return s.bd.DepList(id, direction)
}

// GetLocalString reads clone-local data through the ONE shared sidecar (H2).
//
// It goes to the bd leaf rather than to the read leaf on purpose: the bd leaf owns
// the sidecar object, and routing both halves of the pair through the same leaf
// makes the sharing invariant testable from the outside rather than an internal
// assumption.
func (s *ProxiedStore) GetLocalString(id, key string) (string, error) {
	return s.writeLeaf().GetLocalString(id, key)
}

// Ping verifies the store is operational, and it is the one read whose fork count
// doctor pays repeatedly.
//
// The native leaf's Ping is a GetStatistics over the already-open pool
// (native_dolt_store.go), so it costs zero subprocesses. BdStore.Ping is a
// `bd list --limit 0` fork, and doctor's beads-store check pings once per scope
// per run. After a demotion it is the bd fork again, which is correct: a demoted
// store's health IS the bd front door's health.
func (s *ProxiedStore) Ping() error {
	return s.classifyReadError(s.readLeaf().Ping())
}

// ---------------------------------------------------------------------------
// Mutations. Always the bd leaf. Inside H6's generation bracket for every
// method below and for the three bracketing capability adapters in
// proxied_store_capabilities.go; see the H6 note above for the two paths that
// are outside it and why they have to be.
// ---------------------------------------------------------------------------

// Create persists a new bead through the bd CLI.
func (s *ProxiedStore) Create(b Bead) (Bead, error) {
	var out Bead
	err := s.withMutation("create", func(leaf Store) error {
		created, err := leaf.Create(b)
		out = created
		return err
	})
	return out, err
}

// Update modifies an existing bead through the bd CLI.
func (s *ProxiedStore) Update(id string, opts UpdateOpts) error {
	return s.withMutation("update "+id, func(leaf Store) error { return leaf.Update(id, opts) })
}

// Close closes a bead through the bd CLI.
func (s *ProxiedStore) Close(id string) error {
	return s.withMutation("close "+id, func(leaf Store) error { return leaf.Close(id) })
}

// Reopen reopens a closed bead through the bd CLI.
func (s *ProxiedStore) Reopen(id string) error {
	return s.withMutation("reopen "+id, func(leaf Store) error { return leaf.Reopen(id) })
}

// CloseAll closes a batch through the bd CLI.
func (s *ProxiedStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	var closed int
	err := s.withMutation("close-all", func(leaf Store) error {
		n, err := leaf.CloseAll(ids, metadata)
		closed = n
		return err
	})
	return closed, err
}

// SetMetadata sets one metadata key through the bd CLI.
func (s *ProxiedStore) SetMetadata(id, key, value string) error {
	return s.withMutation("set-metadata "+id, func(leaf Store) error { return leaf.SetMetadata(id, key, value) })
}

// SetMetadataBatch sets several metadata keys through the bd CLI.
func (s *ProxiedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.withMutation("set-metadata-batch "+id, func(leaf Store) error { return leaf.SetMetadataBatch(id, kvs) })
}

// SetLocalString writes clone-local data through the ONE shared sidecar (H2).
//
// It is NOT bracketed by the generation check: the sidecar is a JSON file under
// .beads/ that never touches Dolt, the proxy or a subprocess, so a proxy restart
// across it is not a hazard — and bracketing it would put two file reads on the
// hottest write in the tree (synced_at, last_woke_at).
func (s *ProxiedStore) SetLocalString(id, key, value string) error {
	return s.writeLeaf().SetLocalString(id, key, value)
}

// Tx runs a batch of writes through the bd CLI.
//
// H3: the guarantee reported by AtomicTx follows THIS leaf, which stages rather
// than committing atomically. Reporting the native leaf's true would promise a
// rollback the split cannot perform.
func (s *ProxiedStore) Tx(commitMsg string, fn func(Tx) error) error {
	// Both leaves reject a nil callback before opening anything; the wrapper says
	// so first so the refusal does not depend on which leaf a future routing
	// change sends it to.
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	return s.withMutation("tx", func(leaf Store) error { return leaf.Tx(commitMsg, fn) })
}

// Delete removes a bead through the bd CLI, which also cleans up the shared
// sidecar's entry for it (H2: one object, so the cleanup is visible to both
// leaves).
func (s *ProxiedStore) Delete(id string) error {
	return s.withMutation("delete "+id, func(leaf Store) error { return leaf.Delete(id) })
}

// DepAdd records a dependency edge through the bd CLI.
func (s *ProxiedStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.withMutation("dep-add", func(leaf Store) error { return leaf.DepAdd(issueID, dependsOnID, depType) })
}

// DepRemove removes a dependency edge through the bd CLI.
func (s *ProxiedStore) DepRemove(issueID, dependsOnID string) error {
	return s.withMutation("dep-remove", func(leaf Store) error { return leaf.DepRemove(issueID, dependsOnID) })
}

// CloseStore releases both leaves.
//
// The native handle is closed first and synchronously here (unlike a stand-down,
// nobody is waiting on this path), then the bd leaf's own CloseStore if it has
// one. The guard ticker, when P2-11 has installed one, is stopped before either.
func (s *ProxiedStore) CloseStore() error {
	s.mu.Lock()
	native := s.native
	s.native = nil
	guard := s.guard
	s.guard = nil
	bd := s.bd
	s.mu.Unlock()

	if guard != nil {
		guard.stop()
	}
	var nativeErr error
	if native != nil {
		nativeErr = native.CloseStore()
	}
	if closer, ok := bd.(interface{ CloseStore() error }); ok {
		if err := closer.CloseStore(); err != nil {
			if nativeErr != nil {
				return fmt.Errorf("closing proxied store: native: %w; bd: %w", nativeErr, err)
			}
			return err
		}
	}
	return nativeErr
}
