package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/worktree"
)

// The allocator's selection snapshot (CONTRACT §2, P3 spec §4.5): the
// immutable decision of one allocator pass, which session keys read. This
// slice computes desire, plans, bindings and identity verdicts; grants, start
// ranks, dependency views and the budget view are P3-5b's, and SelGen,
// publication and the diff enqueue are P3-7's.

// allocMode is the snapshot's mode (C2.1, C2.8, #41).
type allocMode uint8

const (
	modeNormal allocMode = iota
	// modePartial: a global partial cause holds (census incomplete,
	// store-query partial); nothing shrinks (P-3).
	modePartial
	// modeSuspended: the city is suspended. Every row is classified as
	// legacy classifies it with every agent suspended; never an empty
	// snapshot (POOL-001).
	modeSuspended
)

func (m allocMode) String() string {
	switch m {
	case modeNormal:
		return "normal"
	case modePartial:
		return "partial"
	case modeSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// desire is an entry's Desired value (CONTRACT §2.2).
type desire uint8

const (
	desireWake desire = iota + 1
	desireKeep
	desireSleep
	desireDrain
	desireNone
)

func (d desire) String() string {
	switch d {
	case desireWake:
		return "wake"
	case desireKeep:
		return "keep"
	case desireSleep:
		return "sleep"
	case desireDrain:
		return "drain"
	case desireNone:
		return "none"
	default:
		return "unset"
	}
}

// Global and leg partial causes (C2.8, POOL-018/047).
const (
	causeCensusIncomplete  = "census-incomplete"
	causeStoreQueryPartial = "store-query-partial"
	causeLegStale          = "leg-stale"
)

// Allocator reason codes (C2.2) and None classes (AM11, C2.11, C2.13).
const (
	reasonPartialRetain        = "partial-retain"
	reasonObservationUncertain = "observation-uncertain"
	reasonConfigSleep          = "config-sleep-suppressed"
	reasonSuspendedCity        = "suspended-city"
	reasonNoWake               = "no-wake-reason"
	reasonUnknownState         = "unknown-state"
	reasonRollbackCandidate    = "rollback-candidate"
	reasonFailedCreate         = "failed-create"
	reasonDuplicate            = "duplicate"
	reasonCensusOnly           = "census-only"
	reasonIdentityLoser        = "identity-loser"
	// reasonNameOccupied: another bead's runtime holds the row's runtime
	// name. None: no grant, no drain, no close (C11).
	reasonNameOccupied = "name-occupied"
	// reasonPendingCreate and reasonAssignedWork keep a row in a suspended
	// city that legacy's suspend drain leaves alone: a pending create within
	// its lease, and a row with open assigned work
	// (session_reconciler.go:2414-2436).
	reasonPendingCreate = "pending-create"
	reasonAssignedWork  = "assigned-work"
	drainOrphaned       = "orphaned"
	drainSuspended      = "suspended"
)

// partialState is the snapshot's partial causes (CONTRACT §2.1).
type partialState struct {
	Global    []string
	Legs      map[string][]string
	Templates map[string]templatePartial
}

// templatePartial is one template's partial read: Retain keeps its rows
// (Sleep and Drain become Keep); BlockCreate also refuses its fresh creates.
type templatePartial struct {
	Retain      bool
	BlockCreate bool
	Causes      []string
}

// selectionSnapshot is one pass's decision. Every open census row has
// exactly one entry (C2.8).
type selectionSnapshot struct {
	Epoch      string
	ConfigRev  string
	EnvGen     uint64
	DecisionAt time.Time
	Mode       allocMode
	Partial    partialState
	// PoolDesired is accepted requests per template, after partial retention
	// and the named merge (POOL-033/035/038).
	PoolDesired map[string]int
	// Floors holds each template's min_active_sessions members (C2.6).
	Floors     map[string][]rowKey
	Entries    map[rowKey]*selectionEntry
	Identities map[string]identityVerdict
}

// selectionEntry is one census row's decision (CONTRACT §2.1).
type selectionEntry struct {
	Key   rowKey
	Basis rowBasis
	// Template is the canonical template (normalizeAgentTemplateIdentity).
	Template    string
	Desired     desire
	Reason      string
	WakeReasons []WakeReason
	DemandClass string
	InDesired   bool
	// AssignedWork is the awake decision's assigned-work anchor.
	AssignedWork *assignedWorkView
	DrainReason  string
	Floor        *floorView
	Config       *desiredConfigRef
	// Liveness is the row's runtime as I3 reads it; P3-5b ranks only start
	// candidates (absent, absent-unconfirmed, dead).
	Liveness rowLiveness
	// Binding is the work a row that is not alive is bound to at start
	// (AM2); live rows never carry one.
	Binding *bindingTarget
	// Normalize marks a canonical singleton whose phantom identity must
	// collapse before start (POOL-046, applied by the session key).
	Normalize            bool
	Endpoint             endpointKey
	ObservationUncertain bool
	Identity             *identityView
}

// rowBasis is the row incarnation the pass saw.
type rowBasis struct {
	Incarnation   int64
	InstanceToken string
}

// assignedWorkView is the row's assigned work: AwakeDecision's anchor, or in
// a suspended city the first open or in-progress work legacy's suspend
// drain checks (sessionHasOpenAssignedWorkForConfigInfo).
type assignedWorkView struct {
	BeadID             string
	Claimed            bool
	RequiresFreshCycle bool
}

// floorView marks a min_active_sessions floor member (C2.6).
type floorView struct {
	Rank      int
	MinActive int
}

// Resolve kinds of a desiredConfigRef (C2.4).
const (
	resolveBase     = "base"
	resolveInstance = "instance"
	resolveNamed    = "named-identity"
	resolveManual   = "manual-instance"
)

// desiredConfigRef names the config a session key resolves TemplateParams
// from (C2.4): the pass never resolves (AM7).
type desiredConfigRef struct {
	ConfigRev         string
	AgentTemplate     string
	ResolveKind       string
	QualifiedInstance string
	PoolSlot          int
	Alias             string
	InstanceName      string
	ManualSession     bool
	DependencyOnly    bool
	NamedIdentity     string
	NamedMode         string
	TransientSlot     bool
}

// bindingTarget is the work a selected row that is not alive is bound to in
// its PreWake patch (AM2, C6). ID is sticky while the pairing holds. An empty
// WorkBeadID clears a stale trigger, as legacy's bind does for a request with
// no work.
type bindingTarget struct {
	ID             string
	WorkBeadID     string
	WorkStoreRef   string
	WorkPack       string
	WorkWorkspace  string
	BrainParentSID string
	// WorkDir is the plan-only work dir (no worktree.Verify); WorktreeSpec,
	// when set, must be verified by the session key before it applies.
	WorkDir      string
	WorktreeSpec *worktree.Spec
}

// identityView is a row's place in its named identity's verdict (C2.13) or
// its duplicate status (C2.11).
type identityView struct {
	Identity    string
	Canonical   bool
	Loser       bool
	VerdictID   string
	DuplicateOf string
}

// identityVerdict is one configured named identity's verdict (C2.13).
// Losers is empty under a partial read.
type identityVerdict struct {
	Canonical *rowKey
	Losers    []rowKey
	VerdictID string
}
