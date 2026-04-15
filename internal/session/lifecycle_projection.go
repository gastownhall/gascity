package session

import (
	"strings"
	"time"
)

// BaseState is the lifecycle state projected from persisted session metadata.
// It intentionally includes compatibility states observed in historical beads
// so callers can reason about them without scattering raw string checks.
type BaseState string

const (
	BaseStateNone        BaseState = ""
	BaseStateCreating    BaseState = "creating"
	BaseStateActive      BaseState = "active"
	BaseStateAsleep      BaseState = "asleep"
	BaseStateSuspended   BaseState = "suspended"
	BaseStateDraining    BaseState = "draining"
	BaseStateDrained     BaseState = "drained"
	BaseStateArchived    BaseState = "archived"
	BaseStateOrphaned    BaseState = "orphaned"
	BaseStateClosed      BaseState = "closed"
	BaseStateClosing     BaseState = "closing"
	BaseStateQuarantined BaseState = "quarantined"
	BaseStateStopped     BaseState = "stopped"
)

// DesiredState describes what the controller should want for an identity,
// separate from the persisted bead state.
type DesiredState string

const (
	DesiredStateUndesired DesiredState = "undesired"
	DesiredStateAsleep    DesiredState = "desired-asleep"
	DesiredStateRunning   DesiredState = "desired-running"
	DesiredStateBlocked   DesiredState = "desired-blocked"
)

// RuntimeProjection describes what observed runtime liveness means for the
// persisted advisory state.
type RuntimeProjection string

const (
	RuntimeProjectionUnknown        RuntimeProjection = ""
	RuntimeProjectionAlive          RuntimeProjection = "alive"
	RuntimeProjectionMissing        RuntimeProjection = "missing"
	RuntimeProjectionFreshCreating  RuntimeProjection = "fresh-creating"
	RuntimeProjectionStaleCreating  RuntimeProjection = "stale-creating"
	RuntimeProjectionStartRequested RuntimeProjection = "start-requested"
)

// IdentityProjection describes whether a configured or concrete session
// identity is currently materialized and usable.
type IdentityProjection string

const (
	IdentityNone                   IdentityProjection = ""
	IdentityConcrete               IdentityProjection = "concrete"
	IdentityCanonical              IdentityProjection = "canonical"
	IdentityHistorical             IdentityProjection = "historical"
	IdentityReservedUnmaterialized IdentityProjection = "reserved-unmaterialized"
	IdentityConflict               IdentityProjection = "conflict"
)

// LifecycleBlocker is a hard condition that suppresses an otherwise runnable
// desired state.
type LifecycleBlocker string

const (
	BlockerHeld               LifecycleBlocker = "held"
	BlockerQuarantined        LifecycleBlocker = "quarantined"
	BlockerMissingConfig      LifecycleBlocker = "missing-config"
	BlockerIdentityConflict   LifecycleBlocker = "identity-conflict"
	BlockerDuplicateCanonical LifecycleBlocker = "duplicate-canonical"
)

// WakeCause is a durable or one-shot reason a session identity should run.
type WakeCause string

const (
	WakeCausePendingCreate WakeCause = "pending-create"
	WakeCausePinned        WakeCause = "pin"
	WakeCauseAttached      WakeCause = "attached"
	WakeCausePending       WakeCause = "pending"
	WakeCauseNamedAlways   WakeCause = "named-always"
	WakeCauseWork          WakeCause = "work"
	WakeCauseScaleDemand   WakeCause = "scale-demand"
	WakeCauseExplicit      WakeCause = "explicit"
)

// RuntimeFacts contains already-observed runtime facts. ProjectLifecycle does
// not perform runtime I/O.
type RuntimeFacts struct {
	Observed bool
	Alive    bool
	Attached bool
	Pending  bool
}

// NamedIdentityInput describes a configured named identity even when no bead
// has been materialized for it yet.
type NamedIdentityInput struct {
	Identity           string
	Configured         bool
	HasCanonicalBead   bool
	Conflict           bool
	DuplicateCanonical bool
}

// LifecycleInput is the read-only fact set for projecting lifecycle state.
type LifecycleInput struct {
	Status             string
	Metadata           map[string]string
	Runtime            RuntimeFacts
	NamedIdentity      NamedIdentityInput
	WakeCauses         []WakeCause
	PreserveIdentity   bool
	ConfigMissing      bool
	CreatedAt          time.Time
	StaleCreatingAfter time.Duration
	Now                time.Time
}

// LifecycleView is the typed lifecycle interpretation of stored metadata and
// runtime/config facts.
type LifecycleView struct {
	BaseState          BaseState
	CompatState        State
	StoredState        string
	DesiredState       DesiredState
	RuntimeProjection  RuntimeProjection
	Identity           IdentityProjection
	NamedIdentity      string
	Blockers           []LifecycleBlocker
	WakeCauses         []WakeCause
	HeldUntil          time.Time
	QuarantinedUntil   time.Time
	ContinuityEligible bool
	Terminal           bool
	CountsAgainstCap   bool
	RuntimeAlive       bool
	RuntimeAttached    bool
	ReconciledState    State
	ResetContinuation  bool
}

// HasBlocker reports whether the view contains the blocker.
func (v LifecycleView) HasBlocker(blocker LifecycleBlocker) bool {
	for _, got := range v.Blockers {
		if got == blocker {
			return true
		}
	}
	return false
}

// HasWakeCause reports whether the view contains the wake cause.
func (v LifecycleView) HasWakeCause(cause WakeCause) bool {
	for _, got := range v.WakeCauses {
		if got == cause {
			return true
		}
	}
	return false
}

// ProjectLifecycle projects raw session metadata plus external facts into the
// lifecycle vocabulary from the session model design.
func ProjectLifecycle(input LifecycleInput) LifecycleView {
	meta := input.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	storedState := strings.TrimSpace(meta["state"])
	sleepReason := strings.TrimSpace(meta["sleep_reason"])
	baseState := projectBaseState(input.Status, storedState, sleepReason)
	compatState := compatStateForBase(baseState)
	terminal := baseState == BaseStateClosed || baseState == BaseStateClosing
	continuityEligible := projectContinuityEligibility(baseState, strings.TrimSpace(meta["continuity_eligible"]))

	namedIdentity := strings.TrimSpace(input.NamedIdentity.Identity)
	if namedIdentity == "" {
		namedIdentity = strings.TrimSpace(meta[NamedSessionIdentityMetadata])
	}
	identity := projectIdentity(input, namedIdentity, baseState, continuityEligible)

	wakeCauses := projectWakeCauses(input, meta)
	runtimeProjection, reconciledState, resetContinuation := projectRuntimeProjection(input, baseState, compatState, sleepReason, wakeCauses)
	blockers, heldUntil, quarantinedUntil := projectBlockers(input, meta, now, baseState, identity)
	desired := projectDesiredState(input, terminal, blockers, wakeCauses)

	return LifecycleView{
		BaseState:          baseState,
		CompatState:        compatState,
		StoredState:        storedState,
		DesiredState:       desired,
		RuntimeProjection:  runtimeProjection,
		Identity:           identity,
		NamedIdentity:      namedIdentity,
		Blockers:           blockers,
		WakeCauses:         wakeCauses,
		HeldUntil:          heldUntil,
		QuarantinedUntil:   quarantinedUntil,
		ContinuityEligible: continuityEligible,
		Terminal:           terminal,
		CountsAgainstCap:   countsAgainstCapacity(baseState),
		RuntimeAlive:       input.Runtime.Alive,
		RuntimeAttached:    input.Runtime.Attached,
		ReconciledState:    reconciledState,
		ResetContinuation:  resetContinuation,
	}
}

func projectBaseState(status, storedState, sleepReason string) BaseState {
	if strings.TrimSpace(status) == "closed" {
		return BaseStateClosed
	}
	switch strings.TrimSpace(storedState) {
	case "":
		return BaseStateNone
	case string(StateCreating):
		return BaseStateCreating
	case string(StateActive), string(StateAwake):
		return BaseStateActive
	case string(StateAsleep):
		if sleepReason == "drained" {
			return BaseStateDrained
		}
		return BaseStateAsleep
	case string(StateSuspended):
		return BaseStateSuspended
	case string(StateDraining):
		return BaseStateDraining
	case "drained":
		return BaseStateDrained
	case string(StateArchived):
		return BaseStateArchived
	case "orphaned":
		return BaseStateOrphaned
	case "closed":
		return BaseStateClosed
	case "closing":
		return BaseStateClosing
	case string(StateQuarantined):
		return BaseStateQuarantined
	case "stopped":
		return BaseStateStopped
	default:
		return BaseState(strings.TrimSpace(storedState))
	}
}

func compatStateForBase(base BaseState) State {
	switch base {
	case BaseStateCreating:
		return StateCreating
	case BaseStateActive:
		return StateActive
	case BaseStateAsleep, BaseStateDrained, BaseStateStopped:
		return StateAsleep
	case BaseStateSuspended:
		return StateSuspended
	case BaseStateDraining:
		return StateDraining
	case BaseStateArchived:
		return StateArchived
	case BaseStateQuarantined:
		return StateQuarantined
	case BaseStateClosed:
		return State("closed")
	case BaseStateClosing:
		return State("closing")
	case BaseStateOrphaned:
		return State("orphaned")
	default:
		return State(base)
	}
}

func projectContinuityEligibility(base BaseState, raw string) bool {
	if base == BaseStateNone || base == BaseStateClosed || base == BaseStateClosing {
		return false
	}
	if raw == "false" {
		return false
	}
	if base == BaseStateArchived {
		return raw == "true"
	}
	// The accepted session model keeps orphaned beads continuity-eligible
	// while missing config is the only blocker.
	return true
}

func projectIdentity(input LifecycleInput, namedIdentity string, base BaseState, continuityEligible bool) IdentityProjection {
	hasBead := base != BaseStateNone
	if namedIdentity != "" && hasBead {
		if continuityEligible {
			return IdentityCanonical
		}
		return IdentityHistorical
	}
	if input.NamedIdentity.Configured && input.NamedIdentity.Conflict && !input.NamedIdentity.HasCanonicalBead {
		return IdentityConflict
	}
	if input.NamedIdentity.Configured && !input.NamedIdentity.HasCanonicalBead {
		return IdentityReservedUnmaterialized
	}
	if hasBead {
		return IdentityConcrete
	}
	return IdentityNone
}

func projectBlockers(input LifecycleInput, meta map[string]string, now time.Time, base BaseState, identity IdentityProjection) ([]LifecycleBlocker, time.Time, time.Time) {
	var blockers []LifecycleBlocker
	if input.ConfigMissing || base == BaseStateOrphaned {
		blockers = appendUniqueBlocker(blockers, BlockerMissingConfig)
	}
	if identity == IdentityConflict || (input.NamedIdentity.Configured && input.NamedIdentity.Conflict) {
		blockers = appendUniqueBlocker(blockers, BlockerIdentityConflict)
	}
	if input.NamedIdentity.DuplicateCanonical {
		blockers = appendUniqueBlocker(blockers, BlockerDuplicateCanonical)
	}

	var heldUntil time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["held_until"])); err == nil && !t.IsZero() {
		heldUntil = t
		if now.Before(t) {
			blockers = appendUniqueBlocker(blockers, BlockerHeld)
		}
	}

	var quarantinedUntil time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(meta["quarantined_until"])); err == nil && !t.IsZero() {
		quarantinedUntil = t
		if now.Before(t) {
			blockers = appendUniqueBlocker(blockers, BlockerQuarantined)
		}
	}

	return blockers, heldUntil, quarantinedUntil
}

func projectRuntimeProjection(input LifecycleInput, base BaseState, compat State, sleepReason string, wakeCauses []WakeCause) (RuntimeProjection, State, bool) {
	if !input.Runtime.Observed {
		return RuntimeProjectionUnknown, compat, false
	}
	if input.Runtime.Alive {
		return RuntimeProjectionAlive, StateAwake, false
	}
	if base == BaseStateNone || base == BaseStateClosed || base == BaseStateClosing {
		return RuntimeProjectionMissing, compat, false
	}
	if hasWakeCause(wakeCauses, WakeCausePendingCreate) {
		return RuntimeProjectionStartRequested, StateCreating, false
	}
	if base == BaseStateCreating {
		if !creatingStateIsStale(input) {
			return RuntimeProjectionFreshCreating, StateCreating, false
		}
		return RuntimeProjectionStaleCreating, StateAsleep, shouldResetContinuation(base, input.Metadata, sleepReason)
	}
	return RuntimeProjectionMissing, StateAsleep, shouldResetContinuation(base, input.Metadata, sleepReason)
}

func creatingStateIsStale(input LifecycleInput) bool {
	if input.StaleCreatingAfter <= 0 {
		return false
	}
	if input.CreatedAt.IsZero() {
		return true
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return !now.Before(input.CreatedAt.Add(input.StaleCreatingAfter))
}

func shouldResetContinuation(base BaseState, meta map[string]string, sleepReason string) bool {
	if meta == nil {
		return false
	}
	if strings.TrimSpace(meta["session_key"]) == "" && strings.TrimSpace(meta["started_config_hash"]) == "" {
		return false
	}
	switch strings.TrimSpace(sleepReason) {
	case "idle", "idle-timeout", "no-wake-reason", "config-drift", "drained", "user-hold", "wait-hold":
		return false
	}
	return base == BaseStateActive || base == BaseStateCreating
}

func projectWakeCauses(input LifecycleInput, meta map[string]string) []WakeCause {
	var causes []WakeCause
	for _, cause := range input.WakeCauses {
		causes = appendUniqueWakeCause(causes, cause)
	}
	if strings.TrimSpace(meta["pending_create_claim"]) == "true" {
		causes = appendUniqueWakeCause(causes, WakeCausePendingCreate)
	}
	if strings.TrimSpace(meta["pin_awake"]) == "true" {
		causes = appendUniqueWakeCause(causes, WakeCausePinned)
	}
	if input.Runtime.Attached {
		causes = appendUniqueWakeCause(causes, WakeCauseAttached)
	}
	if input.Runtime.Pending {
		causes = appendUniqueWakeCause(causes, WakeCausePending)
	}
	return causes
}

func projectDesiredState(input LifecycleInput, terminal bool, blockers []LifecycleBlocker, wakeCauses []WakeCause) DesiredState {
	if terminal {
		return DesiredStateUndesired
	}
	if len(wakeCauses) > 0 {
		if len(blockers) > 0 {
			return DesiredStateBlocked
		}
		return DesiredStateRunning
	}
	if input.PreserveIdentity {
		return DesiredStateAsleep
	}
	return DesiredStateUndesired
}

func countsAgainstCapacity(base BaseState) bool {
	switch base {
	case BaseStateCreating, BaseStateActive, BaseStateDraining, BaseStateQuarantined:
		return true
	default:
		return false
	}
}

func appendUniqueBlocker(blockers []LifecycleBlocker, blocker LifecycleBlocker) []LifecycleBlocker {
	if blocker == "" {
		return blockers
	}
	for _, existing := range blockers {
		if existing == blocker {
			return blockers
		}
	}
	return append(blockers, blocker)
}

func appendUniqueWakeCause(causes []WakeCause, cause WakeCause) []WakeCause {
	if cause == "" {
		return causes
	}
	for _, existing := range causes {
		if existing == cause {
			return causes
		}
	}
	return append(causes, cause)
}

func hasWakeCause(causes []WakeCause, cause WakeCause) bool {
	for _, existing := range causes {
		if existing == cause {
			return true
		}
	}
	return false
}
