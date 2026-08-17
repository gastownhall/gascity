package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// SessionHookActivityGateMetadataKey is a shared-store, single-key CAS gate.
// It is deliberately separate from host-local lifecycle flocks: hooks can run
// in K8s or over SSH while their controller mutates the same shared Dolt store.
const SessionHookActivityGateMetadataKey = "session_hook_activity_gate"

const (
	sessionHookActivityGateVersion  = 1
	sessionHookActivityKindHook     = "hook"
	sessionHookActivityKindReleased = "hook-released"
	sessionHookActivityKindReceipt  = "receipt-consume"
	sessionHookActivityKindStalled  = "execution-stalled"
)

var (
	// ErrSessionHookActivityBlocked reports the single-key gate is currently
	// owned by another lease or fence kind; callers defer, never steal.
	ErrSessionHookActivityBlocked = errors.New("session hook activity gate is busy")
	// ErrSessionHookActivitySuperseded reports the lease's runtime identity or
	// lifecycle authority no longer matches the live session row.
	ErrSessionHookActivitySuperseded = errors.New("session hook activity lease superseded")
	// ErrSessionHookActivityUnsupported reports the backing store exposes no
	// metadata value-CAS; the gate refuses rather than falling back.
	ErrSessionHookActivityUnsupported = errors.New("session hook activity gate requires metadata CAS")
)

// sessionHookActivityGateValue is encoded as canonical struct JSON. The exact
// bytes are the CAS ownership token; fields also make a durable stalled fence
// independently auditable and reconstructable after a controller crash.
type sessionHookActivityGateValue struct {
	Version            int    `json:"v"`
	Kind               string `json:"kind"`
	Nonce              string `json:"nonce"`
	SessionID          string `json:"session_id"`
	Generation         string `json:"generation"`
	ContinuationEpoch  string `json:"continuation_epoch"`
	InstanceToken      string `json:"instance_token"`
	AwakeStartedAt     string `json:"awake_started_at,omitempty"`
	LifecycleAuthority string `json:"lifecycle_authority,omitempty"`
	WorkID             string `json:"work_id,omitempty"`
	WorkStoreRef       string `json:"work_store_ref,omitempty"`
	WorkRevision       int64  `json:"work_revision,omitempty"`
	WorkClaimFence     int64  `json:"work_claim_fence,omitempty"`
	Assignee           string `json:"assignee,omitempty"`
}

// HookActivityCoordinates are the immutable runtime identity a remote
// hook must prove before it may perform or emit any live-input side effect.
type HookActivityCoordinates struct {
	SessionID         string
	Generation        string
	ContinuationEpoch string
	InstanceToken     string
}

// ExecutionStalledActivityFenceCoordinates bind the shared-store fence to the
// exact session and work authority that exhausted its nudge budget.
type ExecutionStalledActivityFenceCoordinates struct {
	HookActivityCoordinates
	AwakeStartedAt     string
	LifecycleAuthority string
	WorkID             string
	WorkStoreRef       string
	WorkRevision       int64
	WorkClaimFence     int64
	Assignee           string
}

// HookActivityLease owns one exact CAS value. Release can only clear its
// own value, so a delayed owner can never erase a successor's lease or fence.
type HookActivityLease struct {
	store              beads.Store
	sessionID          string
	value              string
	kind               string
	coordinates        HookActivityCoordinates
	lifecycleAuthority string
}

// Value returns the exact canonical gate bytes this lease owns; the empty
// string for a nil or released lease.
func (l *HookActivityLease) Value() string {
	if l == nil {
		return ""
	}
	return l.value
}

// IsExecutionStalledFence reports whether this lease is the durable stalled
// kind rather than a transient hook or receipt-consume window.
func (l *HookActivityLease) IsExecutionStalledFence() bool {
	return l != nil && l.kind == sessionHookActivityKindStalled
}

// AcquireHookActivityLease claims the distributed hook gate and then
// revalidates the exact authoritative lifecycle. Missing CAS is a hard refusal:
// a cacheable or unconditional fallback would recreate the remote output race.
// evictStaleSessionHookActivityOccupant clears a transient gate occupant that
// PROVABLY no longer matches the live row. This is the janitor that keeps a
// stranded lease from jamming the gate forever:
//
//   - hook / hook-released kinds whose coordinates (session, generation,
//     continuation epoch, instance token) differ from the live row belong to a
//     dead incarnation — the runtime that held them can never return.
//   - receipt-consume kinds whose nonce is not the CURRENT pending token
//     describe a snapshot that is over: the token was consumed or rotated.
//     An empty pending token makes every receipt fence stale by definition,
//     because the nonce IS the token it fenced.
//   - execution-stalled kinds are never evicted here: they are owned by the
//     durable latch regime and released only through its terminal paths.
//
// The exact-value CAS means a live, still-matching occupant can never be
// cleared by this path.
func evictStaleSessionHookActivityOccupant(store beads.Store, current Info) (bool, error) {
	gate := strings.TrimSpace(current.SessionHookActivityGate)
	if gate == "" || current.Closed {
		return false, nil
	}
	wire, ok := decodeSessionHookActivityGate(gate)
	if !ok {
		return false, nil
	}
	switch wire.Kind {
	case sessionHookActivityKindHook, sessionHookActivityKindReleased:
		occupant := normalizeHookActivityCoordinates(HookActivityCoordinates{
			SessionID:         wire.SessionID,
			Generation:        wire.Generation,
			ContinuationEpoch: wire.ContinuationEpoch,
			InstanceToken:     wire.InstanceToken,
		})
		if hookActivityCoordinatesMatch(current, occupant) {
			return false, nil
		}
	case sessionHookActivityKindReceipt:
		if strings.TrimSpace(wire.Nonce) == strings.TrimSpace(current.ProviderSessionKeyReceiptToken) &&
			strings.TrimSpace(current.ProviderSessionKeyReceiptToken) != "" {
			return false, nil
		}
	default:
		return false, nil
	}
	return compareAndSetSessionHookActivityGate(store, current.ID, gate, "")
}

// AcquireHookActivityLease claims the distributed hook gate and then
// revalidates the exact authoritative lifecycle. Missing CAS is a hard refusal:
// a cacheable or unconditional fallback would recreate the remote output race.
// A transient occupant that provably belongs to a dead incarnation or a
// consumed receipt snapshot is evicted once before the refusal.
func AcquireHookActivityLease(store beads.Store, coordinates HookActivityCoordinates) (*HookActivityLease, Info, error) {
	coordinates = normalizeHookActivityCoordinates(coordinates)
	if !coordinates.complete() {
		return nil, Info{}, fmt.Errorf("%w: incomplete runtime coordinates", ErrSessionHookActivitySuperseded)
	}
	current, err := liveSessionInfo(store, coordinates.SessionID)
	if err != nil {
		return nil, Info{}, err
	}
	if !hookActivityCoordinatesMatch(current, coordinates) || current.Closed || HasExecutionClaimNudgeStalled(current) {
		return nil, current, ErrSessionHookActivitySuperseded
	}
	if strings.TrimSpace(current.SessionHookActivityGate) != "" {
		evicted, evictErr := evictStaleSessionHookActivityOccupant(store, current)
		if evictErr != nil {
			return nil, current, evictErr
		}
		if !evicted {
			return nil, current, ErrSessionHookActivityBlocked
		}
		current.SessionHookActivityGate = ""
	}
	wire := sessionHookActivityGateValue{
		Version:           sessionHookActivityGateVersion,
		Kind:              sessionHookActivityKindHook,
		Nonce:             NewInstanceToken(),
		SessionID:         coordinates.SessionID,
		Generation:        coordinates.Generation,
		ContinuationEpoch: coordinates.ContinuationEpoch,
		InstanceToken:     coordinates.InstanceToken,
	}
	value, err := encodeSessionHookActivityGate(wire)
	if err != nil {
		return nil, current, err
	}
	lease := &HookActivityLease{
		store:              store,
		sessionID:          coordinates.SessionID,
		value:              value,
		kind:               wire.Kind,
		coordinates:        coordinates,
		lifecycleAuthority: ExecutionStalledLifecycleAuthority(current),
	}
	owned, err := compareAndSetSessionHookActivityGate(store, coordinates.SessionID, "", value)
	if err != nil {
		return nil, current, err
	}
	if !owned {
		return nil, current, ErrSessionHookActivityBlocked
	}
	validated, err := lease.Validate()
	if err != nil {
		_ = lease.Release()
		return nil, validated, err
	}
	return lease, validated, nil
}

// acquireProviderSessionKeyReceiptFence closes the append-only receipt
// snapshot. Its value is deterministic for the pending token, so a controller
// that crashes anywhere in List + Update leaves a state another controller can
// adopt and finish idempotently; no owner-death or wall-clock lease theft is
// needed.
func acquireProviderSessionKeyReceiptFence(store beads.Store, sessionID string) (*HookActivityLease, Info, error) {
	current, err := liveSessionInfo(store, sessionID)
	if err != nil {
		return nil, Info{}, err
	}
	if current.Closed || HasExecutionClaimNudgeStalled(current) {
		return nil, current, ErrSessionHookActivitySuperseded
	}
	token := strings.TrimSpace(current.ProviderSessionKeyReceiptToken)
	if token == "" {
		return nil, current, nil
	}
	coordinates := coordinatesFromInfo(current)
	wire := sessionHookActivityGateValue{
		Version: sessionHookActivityGateVersion,
		Kind:    sessionHookActivityKindReceipt,
		// The pending token is already random and makes the recoverable receipt
		// fence unique to this exact launch.
		Nonce:              token,
		SessionID:          current.ID,
		Generation:         coordinates.Generation,
		ContinuationEpoch:  coordinates.ContinuationEpoch,
		InstanceToken:      coordinates.InstanceToken,
		LifecycleAuthority: strings.TrimSpace(current.ProviderSessionKeyReceiptConsumeAuthority),
	}
	value, err := encodeSessionHookActivityGate(wire)
	if err != nil {
		return nil, current, err
	}
	gate := strings.TrimSpace(current.SessionHookActivityGate)
	if gate != "" {
		existing, ok := decodeSessionHookActivityGate(gate)
		// A released tombstone whose coordinates match this exact runtime is
		// the hook's own completion announcement: its store writes are done,
		// so the consume fence may clear it exactly and proceed. An identical
		// receipt fence is crash-adoptable. Anything else that provably
		// belongs to a dead incarnation or a consumed snapshot is evicted;
		// a still-live hook lease (the hook may be mid-write) still blocks.
		releaseAnnounced := ok && existing.Kind == sessionHookActivityKindReleased &&
			existing.SessionID == current.ID &&
			existing.Generation == wire.Generation &&
			existing.ContinuationEpoch == wire.ContinuationEpoch &&
			existing.InstanceToken == wire.InstanceToken
		recoverableReceipt := ok && sameRecoverableReceiptFence(existing, wire)
		switch {
		case releaseAnnounced:
			cleared, clearErr := compareAndSetSessionHookActivityGate(store, current.ID, gate, "")
			if clearErr != nil {
				return nil, current, clearErr
			}
			if !cleared {
				return nil, current, ErrSessionHookActivityBlocked
			}
			current.SessionHookActivityGate = ""
			gate = ""
		case recoverableReceipt:
			value = gate
		default:
			evicted, evictErr := evictStaleSessionHookActivityOccupant(store, current)
			if evictErr != nil {
				return nil, current, evictErr
			}
			if !evicted {
				return nil, current, ErrSessionHookActivityBlocked
			}
			current.SessionHookActivityGate = ""
			gate = ""
		}
	}
	lease := &HookActivityLease{
		store:              store,
		sessionID:          current.ID,
		value:              value,
		kind:               wire.Kind,
		coordinates:        coordinates,
		lifecycleAuthority: strings.TrimSpace(current.ProviderSessionKeyReceiptConsumeAuthority),
	}
	if gate == "" {
		owned, casErr := compareAndSetSessionHookActivityGate(store, current.ID, "", value)
		if casErr != nil {
			return nil, current, casErr
		}
		if !owned {
			return nil, current, ErrSessionHookActivityBlocked
		}
	}
	validated, err := lease.Validate()
	if err != nil {
		_ = lease.Release()
		return nil, validated, err
	}
	return lease, validated, nil
}

// AcquireExecutionStalledActivityFence installs (or recovers) the durable
// distributed fence for one exact session/work authority. Unlike a hook lease,
// it remains held after a successful stalled latch and is released only by the
// exact marker-clear or terminal-close path.
func AcquireExecutionStalledActivityFence(store beads.Store, coordinates ExecutionStalledActivityFenceCoordinates) (*HookActivityLease, Info, error) {
	coordinates.HookActivityCoordinates = normalizeHookActivityCoordinates(coordinates.HookActivityCoordinates)
	coordinates.AwakeStartedAt = strings.TrimSpace(coordinates.AwakeStartedAt)
	coordinates.LifecycleAuthority = strings.TrimSpace(coordinates.LifecycleAuthority)
	coordinates.WorkID = strings.TrimSpace(coordinates.WorkID)
	coordinates.WorkStoreRef = strings.TrimSpace(coordinates.WorkStoreRef)
	coordinates.Assignee = strings.TrimSpace(coordinates.Assignee)
	if !coordinates.complete() {
		return nil, Info{}, fmt.Errorf("%w: incomplete stalled authority", ErrSessionHookActivitySuperseded)
	}
	current, err := liveSessionInfo(store, coordinates.SessionID)
	if err != nil {
		return nil, Info{}, err
	}
	wire := sessionHookActivityGateValue{
		Version:            sessionHookActivityGateVersion,
		Kind:               sessionHookActivityKindStalled,
		Nonce:              NewInstanceToken(),
		SessionID:          coordinates.SessionID,
		Generation:         coordinates.Generation,
		ContinuationEpoch:  coordinates.ContinuationEpoch,
		InstanceToken:      coordinates.InstanceToken,
		AwakeStartedAt:     coordinates.AwakeStartedAt,
		LifecycleAuthority: coordinates.LifecycleAuthority,
		WorkID:             coordinates.WorkID,
		WorkStoreRef:       coordinates.WorkStoreRef,
		WorkRevision:       coordinates.WorkRevision,
		WorkClaimFence:     coordinates.WorkClaimFence,
		Assignee:           coordinates.Assignee,
	}
	gate := strings.TrimSpace(current.SessionHookActivityGate)
	if gate != "" {
		adopted := false
		if existing, ok := decodeSessionHookActivityGate(gate); ok && sameExecutionStalledActivityFence(existing, wire) {
			// Exact recovery: adopt the prior fence value (nonce aside).
			adopted = true
		}
		if !adopted {
			// A transient occupant that provably belongs to a dead incarnation
			// or consumed receipt snapshot is evicted and the fresh fence is
			// installed; another stalled authority still refuses — a live
			// stall regime is never evicted by this path.
			evicted, evictErr := evictStaleSessionHookActivityOccupant(store, current)
			if evictErr != nil {
				return nil, current, evictErr
			}
			if !evicted {
				return nil, current, ErrSessionHookActivityBlocked
			}
			if current.Closed || strings.TrimSpace(current.ExecutionClaimNudgeStalled) != "" ||
				!hookActivityCoordinatesMatch(current, coordinates.HookActivityCoordinates) ||
				ExecutionStalledLifecycleAuthority(current) != coordinates.LifecycleAuthority {
				return nil, current, ErrSessionHookActivitySuperseded
			}
			encoded, encErr := encodeSessionHookActivityGate(wire)
			if encErr != nil {
				return nil, current, encErr
			}
			owned, casErr := compareAndSetSessionHookActivityGate(store, current.ID, "", encoded)
			if casErr != nil {
				return nil, current, casErr
			}
			if !owned {
				return nil, current, ErrSessionHookActivityBlocked
			}
			gate = encoded
		}
	} else {
		if current.Closed || strings.TrimSpace(current.ExecutionClaimNudgeStalled) != "" ||
			!hookActivityCoordinatesMatch(current, coordinates.HookActivityCoordinates) ||
			ExecutionStalledLifecycleAuthority(current) != coordinates.LifecycleAuthority {
			return nil, current, ErrSessionHookActivitySuperseded
		}
		var encErr error
		gate, encErr = encodeSessionHookActivityGate(wire)
		if encErr != nil {
			return nil, current, encErr
		}
		owned, casErr := compareAndSetSessionHookActivityGate(store, current.ID, "", gate)
		if casErr != nil {
			return nil, current, casErr
		}
		if !owned {
			return nil, current, ErrSessionHookActivityBlocked
		}
	}
	lease := &HookActivityLease{
		store:              store,
		sessionID:          current.ID,
		value:              gate,
		kind:               sessionHookActivityKindStalled,
		coordinates:        coordinates.HookActivityCoordinates,
		lifecycleAuthority: coordinates.LifecycleAuthority,
	}
	validated, err := lease.Validate()
	if err != nil {
		return nil, validated, err
	}
	return lease, validated, nil
}

// Validate renews exact ownership by CASing the value to itself, then proves
// that the runtime/lifecycle authority did not change while the lease was held.
// Call this immediately before externally visible hook output.
func (l *HookActivityLease) Validate() (Info, error) {
	if l == nil || l.store == nil || strings.TrimSpace(l.value) == "" {
		return Info{}, ErrSessionHookActivitySuperseded
	}
	owned, err := compareAndSetSessionHookActivityGate(l.store, l.sessionID, l.value, l.value)
	if err != nil {
		return Info{}, err
	}
	if !owned {
		return Info{}, ErrSessionHookActivitySuperseded
	}
	current, err := liveSessionInfo(l.store, l.sessionID)
	if err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(current.SessionHookActivityGate) != l.value || current.Closed ||
		!hookActivityCoordinatesMatch(current, l.coordinates) {
		return current, ErrSessionHookActivitySuperseded
	}
	if l.kind == sessionHookActivityKindHook {
		if strings.TrimSpace(current.ExecutionClaimNudgeStalled) != "" ||
			ExecutionStalledLifecycleAuthority(current) != l.lifecycleAuthority {
			return current, ErrSessionHookActivitySuperseded
		}
	} else if l.kind == sessionHookActivityKindReceipt &&
		strings.TrimSpace(current.ProviderSessionKeyReceiptConsumeAuthority) != l.lifecycleAuthority {
		return current, ErrSessionHookActivitySuperseded
	}
	return current, nil
}

// Release transfers a hook lease into a durable released tombstone rather than
// clearing it. Hook stdout is consumed only after the child exits; a controller
// may clear/promote that tombstone only after a provider idle boundary proves
// the hook has finished. Controller receipt fences clear exactly. Stalled
// fences are retained unless their explicit recovery owner calls Release.
func (l *HookActivityLease) Release() error {
	if l == nil || l.store == nil || strings.TrimSpace(l.value) == "" {
		return nil
	}
	next := ""
	if l.kind == sessionHookActivityKindHook {
		wire, ok := decodeSessionHookActivityGate(l.value)
		if !ok {
			return ErrSessionHookActivitySuperseded
		}
		wire.Kind = sessionHookActivityKindReleased
		var err error
		next, err = encodeSessionHookActivityGate(wire)
		if err != nil {
			return err
		}
	}
	cleared, err := compareAndSetSessionHookActivityGate(l.store, l.sessionID, l.value, next)
	if err != nil {
		return err
	}
	if !cleared {
		return ErrSessionHookActivitySuperseded
	}
	return nil
}

// HookActivityNeedsProviderBoundary reports an active/released remote
// hook gate. The caller must prove the exact runtime has reached a provider idle
// boundary (or is stopped) before acknowledging it; elapsed time alone is never
// sufficient because a remote hook process may merely be paused.
func HookActivityNeedsProviderBoundary(value string) bool {
	wire, ok := decodeSessionHookActivityGate(value)
	return ok && (wire.Kind == sessionHookActivityKindHook || wire.Kind == sessionHookActivityKindReleased)
}

// AcknowledgeSessionHookActivityAfterProviderBoundary clears one exact hook
// value after its caller has proved the provider boundary documented above.
// The live coordinates are rechecked before CAS, and a successor gate is never
// cleared.
func AcknowledgeSessionHookActivityAfterProviderBoundary(store beads.Store, sessionID, expectedValue string) (bool, error) {
	wire, ok := decodeSessionHookActivityGate(expectedValue)
	if !ok || (wire.Kind != sessionHookActivityKindHook && wire.Kind != sessionHookActivityKindReleased) ||
		strings.TrimSpace(wire.SessionID) != strings.TrimSpace(sessionID) {
		return false, ErrSessionHookActivitySuperseded
	}
	current, err := liveSessionInfo(store, sessionID)
	if err != nil {
		return false, err
	}
	coordinates := HookActivityCoordinates{
		SessionID:         wire.SessionID,
		Generation:        wire.Generation,
		ContinuationEpoch: wire.ContinuationEpoch,
		InstanceToken:     wire.InstanceToken,
	}
	if current.Closed || !hookActivityCoordinatesMatch(current, coordinates) ||
		strings.TrimSpace(current.SessionHookActivityGate) != strings.TrimSpace(expectedValue) {
		return false, ErrSessionHookActivitySuperseded
	}
	return compareAndSetSessionHookActivityGate(store, sessionID, expectedValue, "")
}

// IsExecutionStalledActivityFence reports only the persistent stalled kind;
// transient hook/controller leases do not suppress ordinary managed lifecycle.
func IsExecutionStalledActivityFence(value string) bool {
	wire, ok := decodeSessionHookActivityGate(value)
	return ok && wire.Kind == sessionHookActivityKindStalled
}

// IsReleasedSessionHookActivityTombstone reports a hook lease that its owning
// hook process already released. The hook has announced completion of its
// shared-store writes; the remaining acknowledgment proof is that the provider
// stopped consuming the hook (runtime stopped, or no activity for an idle
// grace window). An unreleased "hook" kind needs the harder stopped proof.
func IsReleasedSessionHookActivityTombstone(value string) bool {
	wire, ok := decodeSessionHookActivityGate(value)
	return ok && wire.Kind == sessionHookActivityKindReleased
}

// ReleaseExecutionStalledActivityFenceValue clears one exact durable stalled
// fence. It is the terminal-close and marker-clear recovery path for a fence
// whose owning latch regime has ended; hook and receipt-consume kinds are
// refused because only their live owners may release those leases. The exact
// value CAS means a successor's fence or lease installed after the caller's
// read can never be erased here.
func ReleaseExecutionStalledActivityFenceValue(store beads.Store, sessionID, value string) error {
	sessionID = strings.TrimSpace(sessionID)
	wire, ok := decodeSessionHookActivityGate(value)
	if !ok || wire.Kind != sessionHookActivityKindStalled ||
		strings.TrimSpace(wire.SessionID) != sessionID {
		return ErrSessionHookActivitySuperseded
	}
	lease := &HookActivityLease{
		store:     store,
		sessionID: sessionID,
		value:     strings.TrimSpace(value),
		kind:      sessionHookActivityKindStalled,
	}
	return lease.Release()
}

// ExecutionStalledActivityFenceMatches verifies that value is the exact
// durable fence for coordinates, ignoring only the random nonce.
func ExecutionStalledActivityFenceMatches(value string, coordinates ExecutionStalledActivityFenceCoordinates) bool {
	wire, ok := decodeSessionHookActivityGate(value)
	if !ok {
		return false
	}
	coordinates.HookActivityCoordinates = normalizeHookActivityCoordinates(coordinates.HookActivityCoordinates)
	want := sessionHookActivityGateValue{
		Version:            sessionHookActivityGateVersion,
		Kind:               sessionHookActivityKindStalled,
		SessionID:          strings.TrimSpace(coordinates.SessionID),
		Generation:         strings.TrimSpace(coordinates.Generation),
		ContinuationEpoch:  strings.TrimSpace(coordinates.ContinuationEpoch),
		InstanceToken:      strings.TrimSpace(coordinates.InstanceToken),
		AwakeStartedAt:     strings.TrimSpace(coordinates.AwakeStartedAt),
		LifecycleAuthority: strings.TrimSpace(coordinates.LifecycleAuthority),
		WorkID:             strings.TrimSpace(coordinates.WorkID),
		WorkStoreRef:       strings.TrimSpace(coordinates.WorkStoreRef),
		WorkRevision:       coordinates.WorkRevision,
		WorkClaimFence:     coordinates.WorkClaimFence,
		Assignee:           strings.TrimSpace(coordinates.Assignee),
	}
	return sameExecutionStalledActivityFence(wire, want)
}

func (c HookActivityCoordinates) complete() bool {
	return c.SessionID != "" && c.Generation != "" && c.ContinuationEpoch != "" && c.InstanceToken != ""
}

func (c ExecutionStalledActivityFenceCoordinates) complete() bool {
	return c.HookActivityCoordinates.complete() && c.AwakeStartedAt != "" &&
		ValidExecutionStalledLifecycleAuthority(c.LifecycleAuthority) && c.WorkID != "" &&
		c.WorkStoreRef != "" && c.Assignee != "" && (c.WorkRevision != 0 || c.WorkClaimFence != 0)
}

func normalizeHookActivityCoordinates(c HookActivityCoordinates) HookActivityCoordinates {
	c.SessionID = strings.TrimSpace(c.SessionID)
	c.Generation = strings.TrimSpace(c.Generation)
	c.ContinuationEpoch = strings.TrimSpace(c.ContinuationEpoch)
	if c.ContinuationEpoch == "" {
		// Same convention as the runtime identity fence (chat.go): an absent
		// epoch is the first conversation epoch, not an unprovable identity.
		c.ContinuationEpoch = strconv.Itoa(DefaultContinuationEpoch)
	}
	c.InstanceToken = strings.TrimSpace(c.InstanceToken)
	return c
}

func coordinatesFromInfo(info Info) HookActivityCoordinates {
	return normalizeHookActivityCoordinates(HookActivityCoordinates{
		SessionID:         strings.TrimSpace(info.ID),
		Generation:        strings.TrimSpace(info.Generation),
		ContinuationEpoch: strings.TrimSpace(info.ContinuationEpoch),
		InstanceToken:     strings.TrimSpace(info.InstanceToken),
	})
}

func hookActivityCoordinatesMatch(info Info, coordinates HookActivityCoordinates) bool {
	return coordinatesFromInfo(info) == normalizeHookActivityCoordinates(coordinates)
}

func liveSessionInfo(store beads.Store, sessionID string) (Info, error) {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return Info{}, errors.New("session hook activity store unavailable")
	}
	raw, err := beads.HandlesFor(store).Live.Get(strings.TrimSpace(sessionID))
	if err != nil {
		return Info{}, err
	}
	if !IsSessionBeadOrRepairable(raw) {
		return Info{}, ErrSessionNotFound
	}
	return InfoFromPersistedBead(raw), nil
}

func compareAndSetSessionHookActivityGate(store beads.Store, sessionID, expected, next string) (bool, error) {
	writer, ok := beads.MetadataCASWriterFor(store)
	if !ok {
		return false, ErrSessionHookActivityUnsupported
	}
	swapped, err := writer.CompareAndSetMetadataKey(sessionID, SessionHookActivityGateMetadataKey, expected, next)
	if err == nil {
		return swapped, nil
	}
	// The write may have committed before a transport error. Only an
	// authoritative exact self-value proves success; every other result remains
	// fail closed and is never retried as an unconditional mutation.
	raw, readErr := beads.HandlesFor(store).Live.Get(sessionID)
	if readErr == nil && strings.TrimSpace(raw.Metadata[SessionHookActivityGateMetadataKey]) == next {
		return true, nil
	}
	if readErr != nil {
		return false, errors.Join(err, readErr)
	}
	return false, err
}

func encodeSessionHookActivityGate(value sessionHookActivityGateValue) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeSessionHookActivityGate(value string) (sessionHookActivityGateValue, bool) {
	var wire sessionHookActivityGateValue
	value = strings.TrimSpace(value)
	if value == "" || json.Unmarshal([]byte(value), &wire) != nil ||
		wire.Version != sessionHookActivityGateVersion || strings.TrimSpace(wire.Nonce) == "" ||
		strings.TrimSpace(wire.SessionID) == "" {
		return sessionHookActivityGateValue{}, false
	}
	return wire, true
}

func sameExecutionStalledActivityFence(a, b sessionHookActivityGateValue) bool {
	return a.Version == sessionHookActivityGateVersion && a.Kind == sessionHookActivityKindStalled &&
		b.Version == sessionHookActivityGateVersion && b.Kind == sessionHookActivityKindStalled &&
		a.SessionID == b.SessionID && a.Generation == b.Generation &&
		a.ContinuationEpoch == b.ContinuationEpoch && a.InstanceToken == b.InstanceToken &&
		a.AwakeStartedAt == b.AwakeStartedAt && a.LifecycleAuthority == b.LifecycleAuthority &&
		a.WorkID == b.WorkID && a.WorkStoreRef == b.WorkStoreRef &&
		a.WorkRevision == b.WorkRevision && a.WorkClaimFence == b.WorkClaimFence &&
		a.Assignee == b.Assignee
}

func sameRecoverableReceiptFence(a, b sessionHookActivityGateValue) bool {
	return a.Version == sessionHookActivityGateVersion && a.Kind == sessionHookActivityKindReceipt &&
		b.Version == sessionHookActivityGateVersion && b.Kind == sessionHookActivityKindReceipt &&
		a.Nonce == b.Nonce && a.SessionID == b.SessionID && a.Generation == b.Generation &&
		a.ContinuationEpoch == b.ContinuationEpoch && a.InstanceToken == b.InstanceToken &&
		a.LifecycleAuthority == b.LifecycleAuthority
}
