// Package storebinding defines the resolved storage-class front doors.
package storebinding

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

// StoreSet is the immutable resolved set of storage-class front doors. It has
// no constructor: the only function anywhere that yields a populated StoreSet is
// UnpublishedStoreSet.Publish, which demands publication authority. Assembly
// lives in builder_publication.go and yields the unpublishable candidate.
type StoreSet struct {
	work      WorkTopology
	graph     GraphStore
	sessions  SessionsStore
	messaging MessagingFrontDoors
	orders    OrdersStore
	nudges    NudgeFrontDoors
}

// Work returns the resolved Work topology.
func (s StoreSet) Work() WorkTopology { return s.work }

// Graph returns the graph front door.
func (s StoreSet) Graph() GraphStore { return s.graph }

// Sessions returns the sessions front door.
func (s StoreSet) Sessions() SessionsStore { return s.sessions }

// Messaging returns the messaging front doors.
func (s StoreSet) Messaging() MessagingFrontDoors { return s.messaging }

// Orders returns the orders front door.
func (s StoreSet) Orders() OrdersStore { return s.orders }

// Nudges returns the nudge front doors.
func (s StoreSet) Nudges() NudgeFrontDoors { return s.nudges }

// GraphStore is the closed graph-class persistence contract.
type GraphStore interface {
	Create(beads.Bead) (beads.Bead, error)
	CreateWithStorage(beads.Bead, beads.StorageClass) (beads.Bead, error)
	Get(string) (beads.Bead, error)
	List(beads.ListQuery) ([]beads.Bead, error)
	Ready(...beads.ReadyQuery) ([]beads.Bead, error)
	ReadyContext(context.Context, ...beads.ReadyQuery) ([]beads.Bead, error)
	Children(string, ...beads.QueryOpt) ([]beads.Bead, error)
	Update(string, beads.UpdateOpts) error
	SetMetadata(string, string, string) error
	SetMetadataBatch(string, map[string]string) error
	UpdateIfMatch(string, int64, beads.UpdateOpts) error
	CloseIfMatch(string, int64) error
	DeleteIfMatch(string, int64) error
	CompareAndSetMetadataKey(string, string, string, string) (bool, error)
	Close(string) error
	Reopen(string) error
	CloseAll([]string, map[string]string) (int, error)
	Delete(string) error
	// Claim is the acquire dual of ReleaseIfCurrent: a compare-and-swap on the
	// assignee that succeeds only when the bead is open or in_progress and
	// currently unassigned, is a true no-op when the same assignee already
	// holds it (no revision, no claim-fence bump), and reports a different
	// holder or a terminal status as ok=false — a conflict, not an error. It
	// is on the closed contract because `gc hook --claim` on gcg IDs is an
	// in-process by-ID operation the object model owns; without it the CLI
	// projection would have to re-implement these exact semantics.
	Claim(string, string) (beads.Bead, bool, error)
	ReleaseIfCurrent(string, string) (bool, error)
	DepAdd(string, string, string) error
	DepRemove(string, string) error
	DepList(string, string) ([]beads.Dep, error)
	// DepMetadata reads the opaque payload a graph-apply edge carried. The
	// stable Dep wire model deliberately has no metadata field, so without
	// this the payload is writable through ApplyGraphPlan and readable only by
	// reaching past the front door for a raw store.
	DepMetadata(string, string) (string, bool, error)
	Tx(string, func(GraphTx) error) error
	ApplyGraphPlan(context.Context, *beads.GraphApplyPlan) (*beads.GraphApplyResult, error)
	ApplyGraphPlanWithStorage(context.Context, *beads.GraphApplyPlan, beads.StorageClass) (*beads.GraphApplyResult, error)
	Count(context.Context, beads.ListQuery, ...string) (int, error)
	WaitForParentProjection(context.Context, string, string, string) error
	Ping() error
}

// GraphTx is the closed graph transaction contract.
type GraphTx interface {
	Create(beads.Bead) (beads.Bead, error)
	Update(string, beads.UpdateOpts) error
	SetMetadataBatch(string, map[string]string) error
	Close(string) error
}

// SemanticWitnessAlgorithm is the single witness algorithm version an
// attempt uses for every class (the witness contract). Any change to
// the canonical stream layout is a NEW algorithm string; digests produced
// under different algorithms never compare.
const SemanticWitnessAlgorithm = "gascity.storage-semantic-witness.v1"

// WitnessFamilyCount surfaces one hashed record-family count for diagnostics.
// The authority is the count inside the hashed stream; this is the copy
// status and doctor read, and it is what makes "the destination is a strict
// subset of the source" visible rather than merely digest-unequal.
type WitnessFamilyCount struct {
	Name  string
	Count int
}

// SemanticWitness is the provider-neutral half of a class migration witness
// (the witness contract): a digest over a canonical logical stream that carries no
// provider, format, path, or physical identity. Two stores holding the same
// logical data under different providers produce equal digests. The physical
// half is the pinned descriptor envelope and is never compared across
// providers.
type SemanticWitness struct {
	Version   uint16
	Class     coordclass.Class
	Contract  ContractVersion
	Algorithm string
	Digest    string
	Families  []WitnessFamilyCount
}

// Validate rejects a witness that could not have come from a real canonical
// export: a wrong structure version, an unknown algorithm, a non-canonical
// digest, an unnamed or duplicated family, or a negative count. A witness is
// an equality proof, so a malformed one must fail loudly rather than compare.
func (w SemanticWitness) Validate() error {
	if w.Version != 1 {
		return fmt.Errorf("semantic witness version %d is unsupported", w.Version)
	}
	if !w.Class.IsInfrastructure() {
		return fmt.Errorf("semantic witness class %s is not an infrastructure class", w.Class)
	}
	if w.Algorithm != SemanticWitnessAlgorithm {
		return fmt.Errorf("semantic witness algorithm %q is unsupported", w.Algorithm)
	}
	if strings.TrimSpace(string(w.Contract)) == "" {
		return errors.New("semantic witness is missing its class contract version")
	}
	if err := validateCanonicalSHA256Digest("semantic witness digest", w.Digest); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(w.Families))
	for _, family := range w.Families {
		if strings.TrimSpace(family.Name) == "" {
			return errors.New("semantic witness carries an unnamed record family")
		}
		if family.Count < 0 {
			return fmt.Errorf("semantic witness family %q has a negative count", family.Name)
		}
		if _, duplicate := seen[family.Name]; duplicate {
			return fmt.Errorf("semantic witness family %q appears twice", family.Name)
		}
		seen[family.Name] = struct{}{}
	}
	return nil
}

// SessionsAddressDirectory is the narrow selected Sessions authority needed by
// Messaging for address resolution and liveness. It exposes no persistence
// handle or provider lifecycle.
//
// It is session.AddressDirectory restated on this side of the boundary — the
// class contracts cannot import a class-owning package's interface without a
// cycle — so the two are held identical by a test rather than by convention.
// The semantics are session.AddressDirectory's: ResolveAddress answers session
// targeting (bool = include closed sessions), ResolveMailboxAddress answers
// mailbox ownership in ONE liveness pass (bool = the closed pass) and reports
// a contended address as ambiguous rather than picking a mailbox.
type SessionsAddressDirectory interface {
	ResolveAddress(selector string, includeClosed bool) (session.Info, error)
	ResolveMailboxAddress(selector string, closed bool) (session.Info, error)
	ListAddresses(includeClosed bool) ([]session.Info, error)
}

// SessionsStore is the closed typed sessions and durable-waits contract.
type SessionsStore interface {
	SessionsAddressDirectory
	// Tx runs one multi-write session mutation as a single store transaction,
	// exposing only session.Tx — the narrow transactional peer of this
	// contract's own write methods. It is on the closed contract for the same
	// reason GraphStore.Tx is: the pending-create rollback's "a bead that
	// reports closed always carries its failed-create terminal state" invariant
	// spans several writes, and without a transaction on the front door every
	// consumer of that invariant had to reach past it for a raw bead store.
	//
	// Availability never degrades at call time: every backing store has a
	// transaction, so this method is always answerable. What varies is
	// ATOMICITY, and that is declared up front as ClassCapability.Transactions
	// — a store whose Tx does not roll back says so in its capability
	// declaration and the class corpus holds it to that, rather than a caller
	// discovering mid-rollback that its all-or-nothing swap was never there.
	Tx(string, func(session.Tx) error) error
	CreateSessionInfo(session.CreateSpec) (session.Info, error)
	CreateSession(session.CreateSpec) (string, error)
	Get(string) (session.Info, error)
	GetPersistedResponse(string) (session.Info, session.PersistedResponse, error)
	List(string, string) ([]session.Info, error)
	ListAll(session.ListAllOptions) ([]session.Info, error)
	ListAllForReconcile(session.ListAllOptions) ([]session.ReconcileSession, error)
	ListAllForReconcileWithFingerprint(session.ListAllOptions) ([]session.ReconcileSession, string, error)
	ListAllWithResponses(session.ListAllOptions) ([]session.ListedSession, error)
	ListByMetadataInfos(map[string]string, int) ([]session.Info, error)
	ListLabeledSessionInfosUnfiltered() ([]session.Info, error)
	ApplyPatch(string, session.MetadataPatch) error
	ApplyPatchInfo(session.Info, session.MetadataPatch) (session.Info, error)
	UpdateMetadataInfo(session.Info, session.MetadataPatch) (session.Info, error)
	SetState(string, session.State, string) error
	Sleep(string, string, time.Time) error
	BeginDrainAckStopPending(string, time.Time) error
	RequestRestart(string, string, time.Time) error
	ResetConfigDrift(string, session.State, string, time.Time) error
	SetWaitHold(string, bool, string) error
	SetMarker(string, string, string) error
	RecordCurrentBead(string, string) error
	Close(string, string, time.Time) (bool, error)
	CloseWithoutReason(string) error
	SetStatusOpen(string) error
	RepairType(string) error
	// RepairTypeBestEffort is RepairType's logged, error-swallowing form, used
	// by read-only resolution lanes that must not fail a lookup because a
	// crash-damaged bead could not be repaired. It is on the contract because
	// its one consumer — qualified-alias-basename resolution — is a sessions
	// READ that otherwise holds no persistence handle at all.
	RepairTypeBestEffort(string)
	// The identifier RESOLUTION lane. A consumer that resolves a user-supplied
	// identifier (a bead id, an alias, a session_name, a configured named
	// session) used to need the raw bead store, because the resolution
	// algorithms are package functions over beads.Store in the class-owning
	// package — which is excluded from the closed contracts and can never take a contract. That forced
	// every such consumer to hold the CONCRETE front door purely to unwrap it,
	// which is the escape this contract exists to close. The front door carries
	// them now; these four are the reach, not a reimplementation.
	ResolveIDByExactID(string) (string, error)
	ResolveID(string) (string, error)
	ResolveIDAllowClosed(string) (string, error)
	LookupConfiguredNamed(session.NamedSessionSpec) (session.ConfiguredNamedSessionMatch, error)
	CircuitState(string) (session.CircuitState, error)
	CircuitResetGeneration(string) (string, error)
	PersistedMarkers(string) (session.PersistedMarkers, error)
	GetState(string) (session.State, bool, error)
	MailboxAddress(string) (string, error)
	MailboxAddresses(string) ([]string, error)
	ExtmsgHandleSource(string) (string, bool)
	SetLocalString(string, string, string) error
	GetLocalString(string, string) (string, error)
	HasOpenSessionNamed(string) (bool, error)
	GetWait(string) (session.WaitInfo, error)
	WaitsForSession(string) ([]session.WaitInfo, error)
	ListWaits(string, string) ([]session.WaitInfo, error)
	CreateWait(session.WaitSpec) (session.WaitInfo, error)
	RetryClosedWait(string, string, time.Time) (session.WaitInfo, error)
	CancelWait(string, time.Time, string) error
	ExpireWait(string, time.Time) error
	FailWait(string, time.Time, string) error
	CloseWaitFromNudge(string, time.Time, string, string) error
	FailWaitFromNudge(string, time.Time, string, string, string) error
	MarkWaitReady(string, time.Time) error
	MarkWaitReadyForRedelivery(string, string, time.Time) error
	SetWaitNudgeID(string, string) error
	WaitNudgeIDs(string) ([]string, error)
	CancelWaits(string, time.Time) ([]string, bool, error)
	WakeSession(string, time.Time, session.WakeOpts) (session.WakeResult, error)
	ReassignWaits(string, string) error
}

// MessagingFrontDoors owns the typed mail and external-message services.
type MessagingFrontDoors struct {
	Mail             mail.Provider
	Bindings         extmsg.BindingService
	DeliveryContexts extmsg.DeliveryContextService
	Groups           extmsg.GroupService
	Transcripts      extmsg.TranscriptService
}

var (
	// ErrInvalidMessagingBinding reports a missing directory, typed-nil binder,
	// or incomplete final Messaging front-door set.
	ErrInvalidMessagingBinding = errors.New("invalid deferred messaging binding")
	// ErrMessagingAlreadyBound reports a second attempt to consume one opened
	// Messaging persistence binder.
	ErrMessagingAlreadyBound = errors.New("deferred messaging persistence is already bound")
)

// MessagingFrontDoorBinder owns one already-open Messaging persistence edge
// and consumes the selected typed Sessions directory exactly once. It exposes
// neither raw persistence nor provider reopen operations.
type MessagingFrontDoorBinder interface {
	BindSessions(SessionsAddressDirectory) (MessagingFrontDoors, error)
}

// OrdersStore is the closed order-run tracking contract.
type OrdersStore interface {
	CreateRun(string, orders.RunOpts) (orders.OrderRun, error)
	SetOutcome(string, orders.RunOutcome) error
	SetCursor(string, string, orders.EventCursor) error
	CloseRun(string, string) error
	DeleteRun(string) error
	CreateRunClosed(string, orders.RunOutcome, *orders.EventCursor, string) (orders.OrderRun, error)
	Get(string) (orders.OrderRun, error)
	RunDetail(string) (orders.RunDetail, error)
	RecentRuns(string, int) ([]orders.OrderRun, error)
	RecentRunsAll(int) ([]orders.OrderRun, error)
	ListTracking() ([]orders.OrderRun, error)
	LatestOpenRun(string) (orders.OrderRun, bool, error)
	OpenRuns() ([]orders.OrderRun, error)
	StaleOpenRuns(time.Time) ([]orders.OrderRun, error)
	OrphanedOpenRuns() ([]orders.OrderRun, error)
	ClosedRunsForRetention() ([]orders.OrderRun, error)
	CloseRuns(context.Context, []string, string) (int, error)
	CloseRunsSwept(context.Context, []string, string, string) (int, error)
	MarkFailed(string, string, orders.RunOutcome, *orders.EventCursor) error
	LastRun(string) (time.Time, error)
	Cursor(string) orders.EventCursor
	HasOpenWork(string) (bool, error)
}

// OrdersGraphBinder is the composition seam that binds graph-owned work checks
// while keeping the runtime OrdersStore method set free of raw bead stores.
type OrdersGraphBinder interface {
	BindGraph(GraphStore) OrdersStore
}

// ClaimTarget is the data-only identity and session-generation fence for a nudge claim.
// It matches the the legacy combined layout queue contract and deliberately contains no store handle.
type ClaimTarget struct {
	QueueKeys         []string
	SessionID         string
	ContinuationEpoch string
}

// ErrNudgeSessionFenceMismatch marks a delivery failure caused by a queued
// nudge's session fence not matching the live target. It is CONTRACT
// vocabulary, not implementation detail: RecordFailure's cause is an input to
// the closed [NudgeQueue] contract, and whether a cause is permanent is a
// property of the delivery protocol rather than of the storage engine
// underneath. Two bindings that disagree about it retry on one substrate and
// dead-letter on the other, which is a substitution defect of exactly the kind
// the class corpus exists to catch — so it is declared once, here, beside the
// contract it belongs to.
//
// A caller reports a fence mismatch by returning this error (or one wrapping
// it) as RecordFailure's cause. Every NudgeQueue implementation must then
// dead-letter the item on that first failure: the fenced session generation is
// gone, so redelivery cannot succeed and the retry ceiling would only delay the
// dead letter by the whole backoff schedule. storebindingtest's Nudges suite asserts
// exactly that, against every implementation.
//
// An implementation whose underlying store carries its own equivalent
// sentinel must translate at its binding seam rather than aliasing across the
// layer: errors.Is is identity-based, so a binding that forwards this cause
// unchanged into a store matching on a different value silently downgrades
// permanent to retryable.
var ErrNudgeSessionFenceMismatch = errors.New("queued nudge session fence mismatch")

// NudgeQueue is the closed durable nudge queue contract.
type NudgeQueue interface {
	Enqueue(nudgequeue.Item) error
	EnqueueDeferred(nudgequeue.Item) error
	ClaimDue(ClaimTarget, time.Time) ([]nudgequeue.Item, error)
	ListForAgent(string, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error)
	ListFor(ClaimTarget, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error)
	Snapshot() (nudgequeue.State, error)
	Ack([]string, string, string, string) error
	ReleaseClaims([]string) error
	RecordFailure([]string, error, time.Time) ([]nudgequeue.Item, error)
	Rollback(nudgequeue.Item, string) error
	WithdrawQueuedWaitNudges([]string) error
}

// NudgeShadows is the closed nudge shadow projection contract.
type NudgeShadows interface {
	Save(nudgequeue.Item) (string, bool, error)
	Terminalize(nudgequeue.Item, string, string, string, time.Time) error
	RollbackEnqueue(string) error
	SweepStale(string, string, time.Time) error
	StaleShadowsBefore(time.Time, int, map[string]bool) ([]nudgequeue.NudgeShadow, error)
	ShadowHistorySince(time.Time) ([]nudgequeue.NudgeShadow, error)
	Find(string) (nudgequeue.NudgeShadow, bool, error)
	FindIncludingTerminal(string) (nudgequeue.NudgeShadow, bool, error)
}

// NudgeFrontDoors owns the queue and its shadow projection.
type NudgeFrontDoors struct {
	Queue   NudgeQueue
	Shadows NudgeShadows
}

// WorkScope identifies either HQ or one configured rig. It has no string alias.
type WorkScope struct {
	hq  bool
	rig string
}

// HQScope returns the HQ scope.
func HQScope() WorkScope { return WorkScope{hq: true} }

// RigScope returns the named rig scope.
func RigScope(name string) WorkScope { return WorkScope{rig: name} }

// IsHQ reports whether scope is HQ.
func (s WorkScope) IsHQ() bool { return s.hq }

// Rig returns the rig name and whether scope is rig-scoped.
func (s WorkScope) Rig() (string, bool) { return s.rig, !s.hq && s.rig != "" }

// String returns the stable diagnostic representation.
func (s WorkScope) String() string {
	if s.hq {
		return "hq"
	}
	return "rig:" + s.rig
}

var (
	// ErrWorkScopeNotFound reports a semantic Work scope that the topology
	// does not carry.
	ErrWorkScopeNotFound = errors.New("work scope not found")
	// ErrWorkResidenceNotFound reports a scope with no resolved physical
	// Work residence.
	ErrWorkResidenceNotFound = errors.New("work residence not found")
	// ErrDuplicateWorkResidence reports two scopes claiming the same
	// physical Work residence.
	ErrDuplicateWorkResidence = errors.New("duplicate work residence")
)

// WorkScopeNotFoundError reports an absent semantic Work scope.
type WorkScopeNotFoundError struct{ Scope WorkScope }

func (e *WorkScopeNotFoundError) Error() string {
	return fmt.Sprintf("%s: %s", ErrWorkScopeNotFound, e.Scope)
}
func (e *WorkScopeNotFoundError) Unwrap() error { return ErrWorkScopeNotFound }

// WorkResidenceNotFoundError reports an ID with no Work residence.
type WorkResidenceNotFoundError struct{ ID string }

func (e *WorkResidenceNotFoundError) Error() string {
	return fmt.Sprintf("%s: %s", ErrWorkResidenceNotFound, e.ID)
}
func (e *WorkResidenceNotFoundError) Unwrap() error { return ErrWorkResidenceNotFound }

// DuplicateWorkResidenceError reports an ID or prefix with multiple candidate scopes.
type DuplicateWorkResidenceError struct {
	ID         string
	Candidates []WorkScope
}

func (e *DuplicateWorkResidenceError) Error() string {
	return fmt.Sprintf("%s: %s", ErrDuplicateWorkResidence, e.ID)
}
func (e *DuplicateWorkResidenceError) Unwrap() error { return ErrDuplicateWorkResidence }

// Workspace is one semantically scoped Work store and its pinned identity.
type Workspace struct {
	Scope WorkScope
	Store beads.Store
	// Prefix is the validated immutable ID prefix resolved when this topology opens.
	// ScopeForID uses it instead of consulting mutable provider state at runtime.
	Prefix      string
	Suspended   bool
	OpenerID    string
	ComponentID string
	PhysicalID  string
}

// PhysicalWorkspace groups every semantic scope sharing one pinned handle.
type PhysicalWorkspace struct {
	Workspace Workspace
	Scopes    []WorkScope
}

// WorkTopology is the immutable resolved set of HQ and rig Work stores.
type WorkTopology struct {
	hq       Workspace
	rigs     []Workspace
	byScope  map[WorkScope]Workspace
	physical []PhysicalWorkspace
}

// NewWorkTopology validates and freezes a Work topology.
func NewWorkTopology(hq Workspace, rigs []Workspace) (WorkTopology, error) {
	if !validWorkspace(hq) || !hq.Scope.IsHQ() {
		return WorkTopology{}, fmt.Errorf("HQ workspace: %w", ErrWorkScopeNotFound)
	}
	t := WorkTopology{hq: hq, byScope: map[WorkScope]Workspace{HQScope(): hq}}
	t.rigs = append([]Workspace(nil), rigs...)
	for _, w := range t.rigs {
		if w.Scope.IsHQ() || w.Scope.rig == "" || !validWorkspace(w) {
			return WorkTopology{}, fmt.Errorf("invalid work scope %s", w.Scope)
		}
		if _, ok := t.byScope[w.Scope]; ok {
			return WorkTopology{}, fmt.Errorf("duplicate work scope %s", w.Scope)
		}
		t.byScope[w.Scope] = w
	}
	t.physical = groupPhysical(append([]Workspace{hq}, t.rigs...))
	return t, nil
}

func (t WorkTopology) validate() error {
	if !validWorkspace(t.hq) || !t.hq.Scope.IsHQ() {
		return fmt.Errorf("HQ workspace: %w", ErrWorkScopeNotFound)
	}
	if len(t.byScope) != len(t.rigs)+1 {
		return errors.New("work scope index is incomplete")
	}
	hq, ok := t.byScope[HQScope()]
	if !ok || !sameWorkspaceBinding(hq, t.hq) {
		return errors.New("work scope index has invalid HQ")
	}
	seen := map[WorkScope]struct{}{HQScope(): {}}
	for _, rig := range t.rigs {
		if rig.Scope.IsHQ() || rig.Scope.rig == "" || !validWorkspace(rig) {
			return fmt.Errorf("invalid work scope %s", rig.Scope)
		}
		if _, exists := seen[rig.Scope]; exists {
			return fmt.Errorf("duplicate work scope %s", rig.Scope)
		}
		seen[rig.Scope] = struct{}{}
		indexed, ok := t.byScope[rig.Scope]
		if !ok || !sameWorkspaceBinding(indexed, rig) {
			return fmt.Errorf("work scope index has invalid entry %s", rig.Scope)
		}
	}
	expectedPhysical := groupPhysical(append([]Workspace{t.hq}, t.rigs...))
	if len(t.physical) != len(expectedPhysical) {
		return errors.New("work physical index is incomplete")
	}
	for index := range expectedPhysical {
		if !sameWorkspaceBinding(t.physical[index].Workspace, expectedPhysical[index].Workspace) ||
			!reflect.DeepEqual(t.physical[index].Scopes, expectedPhysical[index].Scopes) {
			return errors.New("work physical index is invalid")
		}
	}
	return nil
}

func sameWorkspaceBinding(left, right Workspace) bool {
	return left.Scope == right.Scope &&
		left.Prefix == right.Prefix &&
		left.Suspended == right.Suspended &&
		left.OpenerID == right.OpenerID &&
		left.ComponentID == right.ComponentID &&
		left.PhysicalID == right.PhysicalID &&
		reflect.DeepEqual(left.Store, right.Store)
}

func validWorkspace(w Workspace) bool {
	return !nilInterface(w.Store) && validWorkPrefix(w.Prefix) && w.OpenerID != "" && w.ComponentID != "" && w.PhysicalID != ""
}

func validWorkPrefix(prefix string) bool {
	return prefix != "" && prefix == strings.ToLower(prefix) && prefix == strings.TrimSpace(prefix) && prefix == strings.Trim(prefix, "-")
}

func groupPhysical(workspaces []Workspace) []PhysicalWorkspace {
	type physicalIdentityKey struct {
		opener, component, physical string
	}
	byIdentity := map[physicalIdentityKey]int{}
	out := make([]PhysicalWorkspace, 0, len(workspaces))
	for _, w := range workspaces {
		key := physicalIdentityKey{w.OpenerID, w.ComponentID, w.PhysicalID}
		if index, ok := byIdentity[key]; ok {
			out[index].Scopes = append(out[index].Scopes, w.Scope)
		} else {
			byIdentity[key] = len(out)
			out = append(out, PhysicalWorkspace{Workspace: w, Scopes: []WorkScope{w.Scope}})
		}
	}
	return out
}

// ForScope returns one scoped Work store.
func (t WorkTopology) ForScope(scope WorkScope) (Workspace, error) {
	w, ok := t.byScope[scope]
	if !ok {
		return Workspace{}, &WorkScopeNotFoundError{Scope: scope}
	}
	return w, nil
}

// RigsInConfigOrder returns rig stores in config order, optionally including suspended rigs.
func (t WorkTopology) RigsInConfigOrder(includeSuspended bool) []Workspace {
	return filterSuspended(t.rigs, includeSuspended)
}

// RigsInLexicalOrder returns rig stores in lexical rig-name order, optionally including suspended rigs.
func (t WorkTopology) RigsInLexicalOrder(includeSuspended bool) []Workspace {
	out := filterSuspended(t.rigs, includeSuspended)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Scope.rig < out[j].Scope.rig })
	return out
}

func filterSuspended(in []Workspace, include bool) []Workspace {
	out := make([]Workspace, 0, len(in))
	for _, w := range in {
		if include || !w.Suspended {
			out = append(out, w)
		}
	}
	return out
}

// All returns HQ followed by all rigs in lexical order, including suspended rigs.
func (t WorkTopology) All() []Workspace {
	return append([]Workspace{t.hq}, t.RigsInLexicalOrder(true)...)
}

// PhysicalWorkspaces returns one representative per physical workspace in deterministic topology order.
func (t WorkTopology) PhysicalWorkspaces() []PhysicalWorkspace {
	out := make([]PhysicalWorkspace, len(t.physical))
	for i, group := range t.physical {
		out[i] = group
		out[i].Scopes = append([]WorkScope(nil), group.Scopes...)
	}
	return out
}

// MigrationWorkspaces returns one migration schedule entry per physical workspace.
func (t WorkTopology) MigrationWorkspaces() []PhysicalWorkspace { return t.PhysicalWorkspaces() }

// ScopeForID selects an exact unique prefix first, then probes residences deterministically.
func (t WorkTopology) ScopeForID(id string) (WorkScope, error) {
	var prefixed []Workspace
	for _, w := range t.All() {
		if strings.HasPrefix(id, w.Prefix+"-") {
			prefixed = append(prefixed, w)
		}
	}
	if len(prefixed) == 1 {
		return prefixed[0].Scope, nil
	}
	if len(prefixed) > 1 {
		return WorkScope{}, duplicateResidence(id, prefixed)
	}
	var found []Workspace
	for _, w := range t.All() {
		if _, err := w.Store.Get(id); err == nil {
			found = append(found, w)
		} else if !errors.Is(err, beads.ErrNotFound) {
			return WorkScope{}, err
		}
	}
	if len(found) == 0 {
		return WorkScope{}, &WorkResidenceNotFoundError{ID: id}
	}
	if len(found) > 1 {
		return WorkScope{}, duplicateResidence(id, found)
	}
	return found[0].Scope, nil
}

func duplicateResidence(id string, candidates []Workspace) *DuplicateWorkResidenceError {
	scopes := make([]WorkScope, len(candidates))
	for i, candidate := range candidates {
		scopes[i] = candidate.Scope
	}
	return &DuplicateWorkResidenceError{ID: id, Candidates: scopes}
}
