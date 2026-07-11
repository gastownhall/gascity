// Package session manages persistent, resumable chat sessions.
//
// A chat session is a conversation between a human and an agent template
// that can be started, suspended (freeing runtime resources), and resumed
// later. Sessions are backed by beads (type "session") for persistence
// and use runtime.Provider for runtime management.
package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// State represents the runtime state of a chat session.
type State string

const (
	// StateActive means the conversation has a live runtime session.
	StateActive State = "active"
	// StateAsleep means the session is dormant with no live runtime.
	StateAsleep State = "asleep"
	// StateSuspended means the conversation is paused with no runtime resources.
	StateSuspended State = "suspended"
	// StateStartPending means the controller has reserved a session identity
	// and should start it, but no provider Start call is currently in flight.
	StateStartPending State = "start-pending"
	// StateCreating means the provider Start call is in flight and the runtime
	// process has not yet been confirmed alive. Counts against pool occupancy.
	StateCreating State = "creating"
	// StateFailedCreate means create rollback wrote terminal metadata but the
	// bead status close did not complete. It is eligible for cleanup/replacement.
	StateFailedCreate State = "failed-create"
	// StateDraining means the session is being gracefully stopped (in-flight
	// work completing). The pool routing label has been removed so no new
	// work is routed to this session.
	StateDraining State = "draining"
	// StateDrained marks an acknowledged drain that should remain dormant
	// until an explicit compatible wake reason appears.
	StateDrained State = "drained"
	// StateAwake is equivalent to StateActive. Written by the reconciler's
	// healState when a session transitions from asleep to running.
	StateAwake State = "awake"
	// StateArchived means the session completed its drain and is retained
	// for history. Does NOT count against pool occupancy.
	StateArchived State = "archived"
	// StateQuarantined means the session hit the crash-loop threshold and
	// is temporarily blocked from waking. Counts against pool occupancy.
	StateQuarantined State = "quarantined"
)

// BeadType is the bead type for chat sessions.
const BeadType = "session"

// LabelSession is the label applied to all session beads for filtering.
const LabelSession = "gc:session"

// MetadataLastNudgeDeliveredAt is the session-bead metadata key that records
// the wall-clock time of the most recent successful queued-nudge delivery.
const MetadataLastNudgeDeliveredAt = "last_nudge_delivered_at"

// Info holds the user-facing details of a chat session.
type Info struct {
	ID string
	// Type is the raw bead type (BeadType for a proper session bead, or empty
	// for a crash/migration-damaged bead that still carries the gc:session
	// label). IsSessionBeadOrRepairableInfo reads it to classify repairable
	// beads without touching the raw bead. Additive, internal-only (absent from
	// the HTTP wire).
	Type          string
	Template      string
	State         State
	Closed        bool
	Title         string
	Alias         string
	AgentName     string // persisted concrete identity for MCP materialization
	Provider      string
	Transport     string
	Command       string // resolved command stored at creation
	WorkDir       string
	SessionName   string // tmux session name
	SessionKey    string // provider-specific resume handle (UUID)
	ResumeFlag    string // stored provider resume flag (e.g., "--resume")
	ResumeStyle   string // "flag" or "subcommand"
	ResumeCommand string // explicit resume command template ({{.SessionKey}})
	CreatedAt     time.Time
	LastActive    time.Time
	// LastNudgeDeliveredAt records the wall-clock time of the most recent
	// successful nudge delivery to this session. Zero when no nudge has
	// been delivered yet (or the metadata predates the stamping path).
	// Surfaced in `gc session list` so operators can spot warm sessions
	// whose delivery loop has stalled.
	LastNudgeDeliveredAt time.Time
	Attached             bool
	// ContinuationEpoch is the persisted continuation_epoch marker, used by the
	// wait registration/retry paths to stamp registered_epoch on wait beads.
	// Additive, internal-only: it is NOT emitted on the HTTP session-response
	// wire (the response builder maps a fixed field set).
	ContinuationEpoch string
	// SleepReason is the persisted sleep_reason marker, read by the wait-hold
	// clear path to decide whether to clear sleep_reason. Additive,
	// internal-only: NOT emitted on the HTTP session-response wire.
	SleepReason string

	// --- identity / pool / named-session cluster (controller read surface) ---
	//
	// These complete the codec so the session reconciler, the bead snapshot,
	// and the classifier predicates read typed Info fields instead of raw bead
	// metadata/labels. Additive, internal-only (absent from the HTTP wire).
	// Each is the raw projected value; the *semantics* (is-pool-managed,
	// resolved origin, agent identity with the agent:<name> label fallback) are
	// predicate methods on Info, not these fields.
	ConfiguredNamedIdentity string // configured_named_identity
	ConfiguredNamedSession  bool   // configured_named_session == "true"
	ConfiguredNamedMode     string // configured_named_mode
	CommonName              string // common_name
	PoolSlot                string // pool_slot (raw; pool helpers parse it)
	PoolManaged             bool   // pool_managed == "true"
	SessionOrigin           string // session_origin (raw; resolved origin is a method)
	DependencyOnly          bool   // dependency_only == "true"
	// DependencyOnlyMetadata is the RAW dependency_only metadata, verbatim and
	// UNTRIMMED. The pin-awake wake-reason display path (cmd/gc) compares it
	// exactly (== "true") WITHOUT trimming, a distinction the trimmed
	// DependencyOnly bool cannot reproduce on whitespace-padded input; the mirror
	// keeps the raw value so that read stays byte-identical. Additive,
	// internal-only mirror; see ManualSessionMetadata for the precedent.
	DependencyOnlyMetadata string // dependency_only (raw)
	ManualSession          bool   // manual_session (trimmed) == "true"
	// ManualSessionMetadata is the RAW manual_session metadata, verbatim.
	// isManualSessionBead compares it WITHOUT trimming, so the Info mirror
	// keeps the raw value to stay byte-identical on whitespace-padded inputs.
	ManualSessionMetadata string
	Labels                []string // bead labels (agent:<name> identity fallback + canonical checks)

	// MCPIdentity / MCPServersSnapshot mirror the raw mcp_identity and
	// mcp_servers_snapshot metadata (verbatim). The ACP-transport classifier
	// treats a non-empty value on either key as evidence the session speaks ACP,
	// so the Info form must carry them to stay byte-identical. Additive,
	// internal-only (absent from the HTTP wire).
	MCPIdentity        string // mcp_identity (raw)
	MCPServersSnapshot string // mcp_servers_snapshot (raw)

	// --- health / provider-terminal-error cluster (controller read surface) ---
	//
	// The pool-demand and reconciler paths treat a session with a persisted
	// provider terminal error (or an unhealthy+drainable+reasoned health record)
	// as spent, excluding it from resume and in-flight demand. These mirror the
	// raw markers so the Info form of that classifier stays byte-identical.
	// Additive, internal-only (absent from the HTTP wire).
	ProviderTerminalError string // provider_terminal_error (raw)
	HealthState           string // session_health (raw)
	HealthReason          string // session_health_reason (raw)
	Drainable             bool   // session_drainable == "true"

	// --- trigger / brain-parent cluster (controller read surface) ---
	//
	// poolInFlightNewRequests stamps these onto the new-tier SessionRequest it
	// emits for a pool-managed creating session. Raw mirrors of the gc.* keys.
	// Additive, internal-only (absent from the HTTP wire).
	TriggerBeadID       string // gc.trigger_bead_id (raw)
	TriggerBeadStoreRef string // gc.trigger_bead_store_ref (raw)
	BrainParentSID      string // gc.brain_parent_sid (raw)
	Pack                string // gc.pack (raw); resolveTemplateForSessionBead threads it into GC_PACKER_PACK

	// --- state / bookkeeping cluster (controller read surface) ---
	//
	// These complete the codec for the classifier predicates that read raw
	// state and create/wake/quarantine bookkeeping keys. Additive,
	// internal-only (absent from the HTTP wire).
	//
	// MetadataState is the RAW persisted state metadata (untrimmed, not
	// normalized, and NOT blanked on closed beads), distinct from the
	// liveness-shaped Info.State. The reconciler's known-state, failed-create,
	// drained, and metadata-state classifiers key off the raw value, so it must
	// be carried verbatim.
	MetadataState string // raw state metadata (verbatim; see State for the normalized form)
	// SessionNameMetadata is the RAW session_name metadata, verbatim and
	// WITHOUT the sessionNameFor(ID) fallback that Info.SessionName applies.
	// Classifiers that branch on "no session_name was persisted" (pool-name
	// ownership, ephemeral pool-slot detection, assignee identities) must read
	// this raw value, not the always-populated Info.SessionName.
	SessionNameMetadata string
	PendingCreateClaim  bool // pending_create_claim == "true"
	// PendingCreateClaimMetadata is the RAW pending_create_claim metadata string,
	// kept verbatim (untrimmed) so trace payloads reproduce a non-canonical raw
	// value (e.g. "yes") that the PendingCreateClaim bool cannot. Additive,
	// internal-only mirror; see WakeAttemptsMetadata for the precedent.
	PendingCreateClaimMetadata string
	PendingCreateStartedAt     string   // pending_create_started_at (raw RFC3339; stale-create sweep parses it)
	WakeAttempts               int      // wake_attempts parsed as int (0 on missing/invalid)
	QuarantinedUntil           string   // quarantined_until (raw RFC3339; quarantine check parses it)
	AliasHistory               []string // prior aliases (alias_history, normalized via session.AliasHistory)
	// ContinuityEligible is the RAW continuity_eligible metadata, verbatim.
	// NamedSessionContinuityEligibleInfo compares it (trimmed) against "false"/
	// "true", so the Info mirror keeps the raw value. Additive, internal-only.
	ContinuityEligible string // continuity_eligible (raw)
	// TransportMetadata is the RAW transport metadata, verbatim and WITHOUT the
	// normalizeTransport(provider, …) derivation that Info.Transport applies.
	// The nudge-target resolver reads the raw value (it falls back to the agent's
	// configured transport when the metadata is empty), so a consumer replacing
	// that raw read must use this field, not the normalized Info.Transport (which
	// would be non-empty even when no transport was persisted). Additive,
	// internal-only (absent from the HTTP wire).
	TransportMetadata string // transport (raw)
	// LastWokeAt is the RAW last_woke_at metadata (RFC3339 or empty). The
	// pending-create lease helpers branch on its emptiness (never-started vs
	// start-in-flight) and parse it for the in-flight deadline, so the Info
	// mirror keeps the raw value.
	LastWokeAt string // last_woke_at (raw)
	// StateReason is the RAW state_reason metadata. The pool sweep's
	// post-create-protection window matches state_reason == "creation_complete".
	StateReason string // state_reason (raw)
	// CreationCompleteAt is the RAW creation_complete_at metadata (RFC3339 or
	// empty). The pool sweep parses it to age out the post-create protection
	// window; a missing/zero value is treated as stale (sweepable).
	CreationCompleteAt string // creation_complete_at (raw)
	// ContinuationResetPending is the RAW continuation_reset_pending metadata.
	// The reconciler's restart-handoff path branches on it (trimmed) == "true"
	// via resetPendingCommittedAt; the Info mirror keeps the raw value.
	ContinuationResetPending string // continuation_reset_pending (raw)
	// ResetCommittedAt is the RAW reset_committed_at metadata (RFC3339 or empty),
	// the durable marker for when a restart handoff committed. resetPendingCommittedAt
	// parses it; the Info mirror keeps the raw value.
	ResetCommittedAt string // reset_committed_at (raw)
	// Generation is the RAW generation metadata, verbatim. The drain/wake
	// staleness checks read it BOTH as strconv.Atoi (numeric compare against the
	// in-memory drain generation) AND strings.TrimSpace (string compare against
	// the persisted GC_DRAIN_GENERATION ack). A parsed int would lose the
	// whitespace fidelity the TrimSpace path relies on, so the mirror keeps the
	// raw string. Additive, internal-only (absent from the HTTP wire).
	Generation string // generation (raw)
	// StartedConfigHash is the RAW started_config_hash metadata, verbatim — the
	// Core fingerprint captured when the session last started. The reconciler's
	// config-drift detection reads it both as a direct string compare (stored
	// hash vs the recomputed Core fingerprint) and via strings.TrimSpace (the
	// emptiness gate that forces firstStart), so the mirror keeps the raw bytes
	// exactly as the drift path relies on. Additive, internal-only (absent from
	// the HTTP wire).
	StartedConfigHash string // started_config_hash (raw)
	// PinAwake is the RAW pin_awake metadata, verbatim. The reconciler's wake
	// pass suppresses config-driven wake only when it is != "true", an exact
	// string compare, so the mirror keeps the raw value. Additive, internal-only
	// (absent from the HTTP wire).
	PinAwake string // pin_awake (raw)

	// --- reconciler decision-read cluster (front-door migration, Phase 5) ---
	//
	// These complete the codec for the raw session-bead metadata the reconciler
	// decision paths still crack inline (held/wait/churn/wake/sleep/config-drift/
	// detach bookkeeping). Each is the RAW projected value, verbatim, so the
	// eventual Info-routed read stays byte-identical to the current
	// session.Metadata[...] read (several are compared both trimmed and untrimmed,
	// or parsed as RFC3339/int, so an int/bool mirror could not preserve fidelity).
	// Additive, internal-only (absent from the HTTP wire). The classifier-
	// equivalence oracle guards these against codec drift.

	// HeldUntil is the RAW held_until metadata. evaluateWakeReasons suppresses ALL
	// wake reasons while it is non-empty; healExpiredTimers clears it once elapsed.
	HeldUntil string // held_until (raw)
	// WaitHold is the RAW wait_hold metadata. The reconcile hold path branches on
	// its emptiness; compute_awake_bridge maps it to LifecycleInput.WaitHold via an
	// exact == "true" compare, so the mirror keeps the raw value.
	WaitHold string // wait_hold (raw)
	// ChurnCount is the RAW churn_count metadata. The death-spiral quarantine path
	// reads it BOTH via strconv.Atoi (numeric threshold) AND as == "" / == "0"
	// (clear/first-increment gates), so the mirror keeps the raw string.
	ChurnCount string // churn_count (raw)
	// WakeMode is the RAW wake_mode metadata. The wake and drain-finalize paths
	// branch on an exact == "fresh" compare.
	WakeMode string // wake_mode (raw)
	// SleepIntent is the RAW sleep_intent metadata. The sleep-intent branch reads
	// it as != "" and == "idle-stop-pending".
	SleepIntent string // sleep_intent (raw)
	// InstanceToken is the RAW instance_token metadata. The wake path compares it
	// against the live instance token to detect a superseded session.
	InstanceToken string // instance_token (raw)
	// DetachedAt is the RAW detached_at metadata (RFC3339 or empty). The detach
	// gate reads it as != "" and parses it via time.Parse, so the mirror keeps the
	// raw bytes.
	DetachedAt string // detached_at (raw)
	// CurrentlyProcessingBeadID is the RAW currently_processing_bead_id metadata
	// (CurrentBeadIDKey). compute_awake_bridge maps it (trimmed) onto
	// LifecycleInput.CurrentlyProcessingBeadID.
	CurrentlyProcessingBeadID string // currently_processing_bead_id (raw)
	// CoreHashBreakdown is the RAW core_hash_breakdown metadata (a JSON blob). The
	// config-drift path feeds it verbatim to runtime.CoreFingerprintDriftFieldsFromJSON
	// / LogCoreFingerprintDrift for the drift trace payload; the mirror keeps the
	// raw JSON exactly.
	CoreHashBreakdown string // core_hash_breakdown (raw)
	// StartedProvisionHash / StartedLaunchHash / StartedLiveHash are the RAW
	// provision/launch/live sub-fingerprints captured at start. The launch-only-
	// drift decision compares StartedProvisionHash against the recomputed provision
	// fingerprint and StartedLaunchHash against the launch fingerprint (both exact
	// string compares, both gated on != ""); the live-hash drift path compares
	// StartedLiveHash. Mirrors keep the raw values.
	StartedProvisionHash string // started_provision_hash (raw)
	StartedLaunchHash    string // started_launch_hash (raw)
	StartedLiveHash      string // started_live_hash (raw)
	// ConfigDriftDeferredAt / ConfigDriftDeferredKey mirror the named-session
	// config-drift deferral timer (config_drift_deferred_at / _key). The deferral
	// path compares the stored key against the current drift key (exact compare)
	// and parses the timestamp (RFC3339). Mirrors keep the raw values.
	ConfigDriftDeferredAt  string // config_drift_deferred_at (raw)
	ConfigDriftDeferredKey string // config_drift_deferred_key (raw)
	// AttachedConfigDriftDeferredAt / AttachedConfigDriftDeferredKey mirror the
	// attached-session config-drift deferral timer (attached_config_drift_deferred_at
	// / _key), the same shape as the named pair above but for the attached path.
	AttachedConfigDriftDeferredAt  string // attached_config_drift_deferred_at (raw)
	AttachedConfigDriftDeferredKey string // attached_config_drift_deferred_key (raw)
	// StrandedEventEmittedAt is the RAW stranded_event_emitted_at metadata, the
	// idempotency marker the stranded-diagnostic emitter checks (trimmed != "")
	// before firing once.
	StrandedEventEmittedAt string // stranded_event_emitted_at (raw)
	// SessionNameExplicit is the RAW session_name_explicit metadata. The lifecycle
	// projection's LifecycleIdentifiersReleased predicate reads it (trimmed == "")
	// alongside alias / session_name, and build_desired_state / the parallel
	// lifecycle path branch on it (trimmed == "true"). Mirror keeps the raw value.
	SessionNameExplicit string // session_name_explicit (raw)
	// WakeRequest is the RAW wake_request metadata. ProjectLifecycle's wake-cause
	// projection reads it (trimmed == string(WakeCauseExplicit)) to raise the
	// explicit-wake cause. Mirror keeps the raw value so a typed LifecycleInput can
	// be populated from Info without touching the bead.
	WakeRequest string // wake_request (raw)
	// RestartRequested is the RAW restart_requested metadata, the §5.2 intra-tick
	// restart marker compute_awake_bridge reads (trimmed == "true") to surface a
	// pending restart on the awake scan. Under raw-refresh coexistence the mirror
	// reflects the in-memory value; Step 6 handles the Get-cutover intra-tick carrier.
	RestartRequested string // restart_requested (raw)
	// SessionIDFlag is the RAW session_id_flag metadata. freshRestartSessionKey
	// (cmd/gc) reads it (trimmed != "") to decide whether the provider can inject a
	// fresh session ID on a restart handoff. Additive mirror so that read can move off
	// the raw bead in Step 6b. (Distinct from the resume-time SessionIDFlag field
	// above, which is the CLI flag string resolved from config, not bead metadata.)
	SessionIDFlag string // session_id_flag (raw)
	// TemplateOverrides is the RAW template_overrides metadata (a JSON object string).
	// ParseTemplateOverrides decodes it on the config-drift hash path; the mirror keeps
	// the verbatim string so that decode can be fed from Info instead of the bead map
	// in Step 6b.
	TemplateOverrides string // template_overrides (raw JSON)
	// WakeAttemptsMetadata is the RAW wake_attempts metadata string, kept verbatim
	// alongside the int-parsed WakeAttempts above. clearWakeFailures (cmd/gc) gates on
	// the raw string (!= "" && != "0"), which the int form cannot reproduce (it collapses
	// missing/"0"/malformed all to 0); the mirror preserves that distinction for Step 6b.
	WakeAttemptsMetadata string // wake_attempts (raw)
	// ProviderKind is the RAW provider_kind metadata, verbatim — the provider
	// FAMILY marker (claude/codex/gemini) stamped from ResolvedProvider, distinct
	// from Provider (the concrete provider name). The session-logs / mcp-integration
	// CLI paths and the worker invocation-telemetry path read it as a family value
	// (TrimSpace, with a fall-back to provider when empty), so the mirror keeps the
	// raw value. Additive, internal-only (absent from the HTTP wire). Session-class
	// periphery front-door migration.
	ProviderKind string // provider_kind (raw)
}

// RuntimeObservation reports the provider-backed live runtime state for a
// persisted session.
type RuntimeObservation struct {
	Running     bool
	Alive       bool
	Attached    bool
	LastActive  time.Time
	SessionName string
}

func normalizeInfoState(state State) State {
	switch state {
	case "awake":
		return StateActive
	case "drained":
		return StateAsleep
	}
	return state
}

// canonicalLifecycleState maps a bead's stored state metadata onto the State
// the transition table understands, before the state machine is consulted. A
// pre-metadata legacy bead carries an empty state (StateNone); treat it as
// StateActive so transitions work during upgrade. StateAwake is the
// reconciler's alias for StateActive; the table only knows StateActive, so
// normalize it too, keeping already-awake beads accepting Suspend/Drain/
// Archive/Quarantine/Close. Callers own their own closed-bead and terminal
// pre-checks; this handles only the none/awake canonicalization shared by
// Suspend, CloseDetailed, and checkTransition.
func canonicalLifecycleState(rawState State) State {
	switch rawState {
	case StateNone, StateAwake:
		return StateActive
	}
	return rawState
}

// ProviderResume describes a provider's session resume capabilities.
// Populated from config.ResolvedProvider's resume fields.
type ProviderResume struct {
	// ResumeFlag is the CLI flag for resuming (e.g., "--resume").
	// Empty means the provider doesn't support resume.
	ResumeFlag string
	// ResumeStyle is "flag" (--resume <key>) or "subcommand" (command resume <key>).
	ResumeStyle string
	// ResumeCommand is the full shell command template for resuming.
	// Supports {{.SessionKey}}. When set, takes precedence over ResumeFlag/ResumeStyle.
	ResumeCommand string
	// SessionIDFlag is the CLI flag for creating with a specific ID (e.g., "--session-id").
	// Enables Generate & Pass strategy.
	SessionIDFlag string
}

// Manager orchestrates chat session lifecycle using beads for persistence
// and runtime.Provider for runtime.
type Manager struct {
	store             beads.Store
	sp                runtime.Provider
	cityPath          string
	transportResolver func(template, provider string) transportResolution
	clk               clock.Clock
}

// PruneResult reports which sessions were pruned and which queued wait nudges
// should be eagerly withdrawn afterward.
type PruneResult struct {
	Count        int
	SessionIDs   []string
	WaitNudgeIDs []string
}

// CloseResult reports session-close cleanup artifacts needed by callers.
type CloseResult struct {
	WaitNudgeIDs []string
}

type acpRouteRegistrar interface {
	RouteACP(name string)
	Unroute(name string)
}

type transportDetector interface {
	DetectTransport(name string) string
}

type transportResolution struct {
	transport            string
	allowStoppedFallback bool
}

func normalizeTransport(provider, transport string) string {
	if transport != "" {
		return transport
	}
	if provider == "acp" {
		return "acp"
	}
	return ""
}

func transportFromMetadata(b beads.Bead) string {
	return normalizeTransport(b.Metadata["provider"], b.Metadata["transport"])
}

func (m *Manager) resolveConfiguredTransport(template, provider string) (string, bool) {
	if m.transportResolver == nil {
		return "", false
	}
	resolution := m.transportResolver(strings.TrimSpace(template), strings.TrimSpace(provider))
	return normalizeTransport(provider, resolution.transport), resolution.allowStoppedFallback
}

func (m *Manager) transportForBead(b beads.Bead, sessName string) (string, bool) {
	transport := transportFromMetadata(b)
	if transport != "" {
		return transport, false
	}
	if strings.TrimSpace(b.Metadata[MCPIdentityMetadataKey]) != "" ||
		strings.TrimSpace(b.Metadata[MCPServersSnapshotMetadataKey]) != "" {
		return "acp", false
	}
	if strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true" {
		transport, _ = m.resolveConfiguredTransport(b.Metadata["template"], b.Metadata["provider"])
		if transport != "" {
			return transport, true
		}
		return "", false
	}
	if detector, ok := m.sp.(transportDetector); ok {
		transport = normalizeTransport(b.Metadata["provider"], detector.DetectTransport(sessName))
		if transport != "" {
			return transport, true
		}
	}
	if m.sp != nil && m.sp.IsRunning(sessName) {
		return "", false
	}
	return "", false
}

func (m *Manager) persistTransport(id, provider, transport string) {
	transport = normalizeTransport(provider, transport)
	if transport == "" {
		return
	}
	_ = m.store.SetMetadata(id, "transport", transport)
}

// killExistingOrphans terminates any untracked runtime whose session ID and
// city match the session about to start, then confirms each is dead. It returns
// a non-nil error only when an orphan could not be confirmed dead, so callers
// gating a Start can refuse rather than race a survivor for the same work. A
// scan error is logged and treated as fail-closed (see FindRuntimesBySessionID):
// the roots the scan did surface are still killed, and matching the started
// replacement is impossible because it does not exist yet.
func (m *Manager) killExistingOrphans(ctx context.Context, sessionID string) error {
	_ = ctx
	scanner, ok := m.sp.(runtime.ProcessTableScanner)
	if !ok || sessionID == "" {
		return nil
	}
	found, err := scanner.FindRuntimesBySessionID(sessionID)
	if err != nil {
		log.Printf("session: scanning for orphaned runtimes for %s (failing closed): %v", sessionID, err)
	}
	cityPath := pathutil.NormalizePathForCompare(strings.TrimSpace(m.cityPath))
	var termErrs []error
	for _, live := range found {
		if live.IsTracked || live.SessionID != sessionID {
			continue
		}
		if cityPath != "" && pathutil.NormalizePathForCompare(strings.TrimSpace(live.City)) != cityPath {
			continue
		}
		if err := scanner.TerminateRuntime(live); err != nil {
			log.Printf("session: terminating orphaned runtime for %s pid=%d provider_name=%q: %v", sessionID, live.PID, live.ProviderName, err)
			termErrs = append(termErrs, fmt.Errorf("orphan pid=%d provider_name=%q: %w", live.PID, live.ProviderName, err))
		}
	}
	if len(termErrs) > 0 {
		return fmt.Errorf("%d orphaned runtime(s) not confirmed dead: %w", len(termErrs), errors.Join(termErrs...))
	}
	return nil
}

func (m *Manager) now() time.Time {
	if m != nil && m.clk != nil {
		return m.clk.Now()
	}
	return time.Now()
}

func (m *Manager) routeACPIfNeeded(provider, transport, sessName string) func() {
	if normalizeTransport(provider, transport) != "acp" {
		return nil
	}
	router, ok := m.sp.(acpRouteRegistrar)
	if !ok {
		return nil
	}
	router.RouteACP(sessName)
	return func() { router.Unroute(sessName) }
}

// NewManager creates a Manager backed by the given bead store and session provider.
func NewManager(store beads.Store, sp runtime.Provider) *Manager {
	return &Manager{store: store, sp: sp}
}

// NewManagerWithTransportResolver creates a Manager that can infer session
// transport from template or provider config when older beads do not have
// transport metadata.
func NewManagerWithTransportResolver(store beads.Store, sp runtime.Provider, resolver func(template, provider string) string) *Manager {
	return &Manager{
		store: store,
		sp:    sp,
		transportResolver: func(template, provider string) transportResolution {
			if resolver == nil {
				return transportResolution{}
			}
			return transportResolution{transport: resolver(template, provider)}
		},
	}
}

// NewManagerWithCityPath creates a Manager that can persist deferred submits
// into the city's nudge queue.
func NewManagerWithCityPath(store beads.Store, sp runtime.Provider, cityPath string) *Manager {
	return &Manager{store: store, sp: sp, cityPath: cityPath}
}

// NewManagerWithTransportResolverAndCityPath creates a Manager that can infer
// session transport from template or provider config and persist deferred
// submits into the city's nudge queue.
func NewManagerWithTransportResolverAndCityPath(store beads.Store, sp runtime.Provider, cityPath string, resolver func(template, provider string) string) *Manager {
	return &Manager{
		store:    store,
		sp:       sp,
		cityPath: cityPath,
		transportResolver: func(template, provider string) transportResolution {
			if resolver == nil {
				return transportResolution{}
			}
			return transportResolution{transport: resolver(template, provider)}
		},
	}
}

// NewManagerWithTransportPolicyResolverAndCityPath creates a Manager that can
// infer transport from config and, when the resolver marks it safe, continue
// using that transport for stopped legacy sessions without persisted
// transport metadata.
func NewManagerWithTransportPolicyResolverAndCityPath(
	store beads.Store,
	sp runtime.Provider,
	cityPath string,
	resolver func(template, provider string) (string, bool),
) *Manager {
	return &Manager{
		store:    store,
		sp:       sp,
		cityPath: cityPath,
		transportResolver: func(template, provider string) transportResolution {
			if resolver == nil {
				return transportResolution{}
			}
			transport, allowStoppedFallback := resolver(template, provider)
			return transportResolution{
				transport:            transport,
				allowStoppedFallback: allowStoppedFallback,
			}
		},
	}
}

// Create creates a new chat session bead and starts the runtime session.
// The command is the full provider command to execute (e.g., "claude --dangerously-skip-permissions").
// The resume parameter carries provider resume capabilities; if the provider
// supports SessionIDFlag, a UUID session key is generated and injected.
// The caller is responsible for attaching after Create returns.
func (m *Manager) Create(ctx context.Context, template, title, command, workDir, provider string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", "", template, title, command, workDir, provider, "", env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateWithTransport creates a new chat session bead and starts the runtime
// session, preserving the transport override separately from the provider name
// so ACP-routed sessions can be resumed correctly.
func (m *Manager) CreateWithTransport(ctx context.Context, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", "", template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateAliasedNamedWithTransport creates a new chat session bead with an
// optional public alias and optional explicit runtime session_name.
func (m *Manager) CreateAliasedNamedWithTransport(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.createAliasedNamedWithTransport(ctx, alias, explicitName, template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

// CreateAliasedNamedWithTransportAndMetadata creates a new chat session bead
// with additional metadata published atomically at bead creation time.
func (m *Manager) CreateAliasedNamedWithTransportAndMetadata(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config, extraMeta map[string]string) (Info, error) {
	return m.createAliasedNamedWithTransport(ctx, alias, explicitName, template, title, command, workDir, provider, transport, env, resume, hints, extraMeta)
}

func (m *Manager) createAliasedNamedWithTransport(ctx context.Context, alias, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config, extraMeta map[string]string) (Info, error) {
	alias, err := ValidateAlias(alias)
	if err != nil {
		return Info{}, err
	}
	explicitName, err = ValidateExplicitName(explicitName)
	if err != nil {
		return Info{}, err
	}
	if title == "" {
		title = template
	}
	aliasOwner := ""
	if extraMeta["configured_named_session"] == "true" && extraMeta["configured_named_identity"] == alias {
		aliasOwner = alias
	}
	var info Info
	err = withSessionIdentifierReservationLocks([]string{alias, explicitName}, func() error {
		if err := ensureSessionAliasAvailable(m.store, nil, alias, "", aliasOwner); err != nil {
			return err
		}
		if err := ensureSessionNameAvailableForSelfAndOwner(m.store, explicitName, "", aliasOwner); err != nil {
			return err
		}

		// Generate session key only when the provider supports Generate & Pass
		// (has SessionIDFlag). Otherwise the key would never be passed to the
		// provider and BuildResumeCommand would produce invalid resume commands.
		var sessionKey string
		if resume.SessionIDFlag != "" {
			generatedKey, genErr := GenerateSessionKey()
			if genErr != nil {
				return fmt.Errorf("generating session key: %w", genErr)
			}
			sessionKey = generatedKey
		}

		// Create the bead first to get the ID.
		meta := map[string]string{
			"template":           template,
			"state":              string(StateActive),
			"provider":           provider,
			"work_dir":           workDir,
			"command":            command,
			"resume_flag":        resume.ResumeFlag,
			"resume_style":       resume.ResumeStyle,
			"resume_command":     resume.ResumeCommand,
			"generation":         fmt.Sprintf("%d", DefaultGeneration),
			"continuation_epoch": fmt.Sprintf("%d", DefaultContinuationEpoch),
			"instance_token":     NewInstanceToken(),
		}
		// provider_kind may be injected via extraMeta when the caller has
		// resolved the canonical builtin kind for a custom provider alias.
		if alias != "" {
			meta["alias"] = alias
		}
		if normalizedTransport := normalizeTransport(provider, transport); normalizedTransport != "" {
			meta["transport"] = normalizedTransport
		}
		if sessionKey != "" {
			meta["session_key"] = sessionKey
		}
		if explicitName != "" {
			meta["session_name"] = explicitName
			meta["session_name_explicit"] = "true"
		}
		for k, v := range extraMeta {
			meta[k] = v
		}
		if meta["session_origin"] == "" {
			meta["session_origin"] = "manual"
		}
		createdBead, createErr := m.store.Create(beads.Bead{
			Title: title,
			Type:  BeadType,
			Labels: []string{
				LabelSession,
				"template:" + template,
			},
			Metadata: meta,
		})
		if createErr != nil {
			return fmt.Errorf("creating session bead: %w", createErr)
		}
		b := createdBead

		sessName := explicitName
		if sessName == "" {
			sessName = sessionNameFor(b.ID)
			if err := m.store.SetMetadata(b.ID, "session_name", sessName); err != nil {
				_ = m.store.Close(b.ID)
				return fmt.Errorf("storing session name: %w", err)
			}
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["session_name"] = sessName
		if explicitName != "" {
			b.Metadata["session_name_explicit"] = "true"
		}
		if err := m.syncStoredMCPServers(b.ID, &b, hints.MCPServers); err != nil {
			_ = m.store.Close(b.ID)
			return err
		}

		unroute := m.routeACPIfNeeded(provider, transport, sessName)
		rollbackFailedCreate := func() error {
			if unroute != nil {
				unroute()
			}
			if explicitName != "" {
				if err := m.store.SetMetadata(b.ID, "session_name", ""); err != nil {
					return fmt.Errorf("clearing session name during rollback: %w", err)
				}
				if err := m.store.SetMetadata(b.ID, "session_name_explicit", ""); err != nil {
					return fmt.Errorf("clearing explicit session name flag during rollback: %w", err)
				}
				b.Metadata["session_name"] = ""
				b.Metadata["session_name_explicit"] = ""
			}
			if err := m.store.Close(b.ID); err != nil {
				return fmt.Errorf("closing rolled-back session bead: %w", err)
			}
			return nil
		}

		// If the provider supports Generate & Pass, inject --session-id into command.
		startCommand := command
		if resume.SessionIDFlag != "" && sessionKey != "" {
			startCommand = command + " " + resume.SessionIDFlag + " " + sessionKey
		}

		// Build the session config from the hints, overriding command/workdir/env.
		cfg := hints
		cfg.Command = startCommand
		cfg.WorkDir = workDir
		runtimeAlias := alias
		if runtimeAlias == "" {
			runtimeAlias = strings.TrimSpace(extraMeta["agent_name"])
		}
		cfg.Env = mergeEnv(mergeEnv(cfg.Env, env), RuntimeEnvWithSessionContext(
			b.ID,
			sessName,
			runtimeAlias,
			template,
			meta["session_origin"],
			DefaultGeneration,
			DefaultContinuationEpoch,
			meta["instance_token"],
		))
		if gcProvider := ProviderFamilyFromMetadata(meta, provider); gcProvider != "" {
			cfg.Env = mergeEnv(cfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
		}
		cfg = runtime.SyncWorkDirEnv(cfg)

		// Start the runtime session. Refuse to start if a prior escaped process
		// for this session could not be confirmed dead: a survivor would race
		// the replacement for the same work bead (duplicate bd close).
		if orphanErr := m.killExistingOrphans(ctx, b.ID); orphanErr != nil {
			if rbErr := rollbackFailedCreate(); rbErr != nil {
				return errors.Join(fmt.Errorf("pre-start orphan cleanup: %w", orphanErr), rbErr)
			}
			return fmt.Errorf("pre-start orphan cleanup: %w", orphanErr)
		}
		if err := m.sp.Start(ctx, sessName, cfg); err != nil {
			if runtimeSessionMatchesBead(m.sp, sessName, b.ID, meta["instance_token"]) {
				if metaErr := m.confirmStartedRuntimeMetadata(b.ID, &b); metaErr != nil {
					return metaErr
				}
				info = m.infoFromBead(b)
				return nil
			}
			if errors.Is(err, runtime.ErrSessionExists) && m.sp.IsRunning(sessName) {
				if rbErr := rollbackFailedCreate(); rbErr != nil {
					return errors.Join(fmt.Errorf("%w: %q already active in runtime", ErrSessionNameExists, sessName), rbErr)
				}
				return fmt.Errorf("%w: %q already active in runtime", ErrSessionNameExists, sessName)
			}
			if rbErr := rollbackFailedCreate(); rbErr != nil {
				return errors.Join(fmt.Errorf("starting session: %w", err), rbErr)
			}
			return fmt.Errorf("starting session: %w", err)
		}
		if metaErr := m.confirmStartedRuntimeMetadata(b.ID, &b); metaErr != nil {
			if stopErr := m.sp.Stop(sessName); stopErr != nil {
				metaErr = errors.Join(metaErr, fmt.Errorf("stopping runtime after metadata failure: %w", stopErr))
			}
			if rbErr := rollbackFailedCreate(); rbErr != nil {
				return errors.Join(metaErr, rbErr)
			}
			return metaErr
		}

		info = m.infoFromBead(b)
		return nil
	})
	if err != nil {
		return Info{}, err
	}
	return info, nil
}

func (m *Manager) confirmStartedRuntimeMetadata(id string, b *beads.Bead) error {
	metadata := ConfirmStartedPatch(time.Now().UTC())
	if err := m.store.SetMetadataBatch(id, metadata); err != nil {
		return fmt.Errorf("storing started runtime metadata: %w", err)
	}
	if b != nil {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(metadata))
		}
		for k, v := range metadata {
			b.Metadata[k] = v
		}
	}
	return nil
}

// CreateNamedWithTransport creates a new chat session bead with an optional
// explicit session_name and starts the runtime session.
//
// WARNING: withSessionNameReservationLock only serializes callers inside this
// process. Callers MUST also hold WithCitySessionNameLock(cityPath, explicitName)
// when explicitName is non-empty so duplicate names cannot race across processes.
func (m *Manager) CreateNamedWithTransport(ctx context.Context, explicitName, template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume, hints runtime.Config) (Info, error) {
	return m.CreateAliasedNamedWithTransportAndMetadata(ctx, "", explicitName, template, title, command, workDir, provider, transport, env, resume, hints, map[string]string{
		"session_origin": "manual",
	})
}

func runtimeSessionMatchesBead(sp runtime.Provider, sessionName, beadID, instanceToken string) bool {
	if sp == nil {
		return false
	}
	if liveID, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
		liveID = strings.TrimSpace(liveID)
		if liveID != "" {
			return liveID == beadID
		}
	}
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return false
	}
	liveToken, err := sp.GetMeta(sessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		return false
	}
	return strings.TrimSpace(liveToken) == instanceToken
}

// CreateBeadOnly creates a session bead without starting the runtime process.
// The bead is created with state "start-pending" — the controller's
// reconciler will detect it in buildDesiredState and start the process on its
// next tick.
//
// This is the Phase 2 path: CLI creates intent (bead), reconciler executes.
func (m *Manager) CreateBeadOnly(template, title, command, workDir, provider, transport string, env map[string]string, resume ProviderResume) (Info, error) {
	return m.CreateBeadOnlyNamed("", template, title, command, workDir, provider, transport, env, resume)
}

// CreateAliasedBeadOnlyNamed creates a session bead without starting the
// runtime process, preserving an optional public alias and explicit runtime
// session_name for the reconciler.
func (m *Manager) CreateAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport string, _ map[string]string, resume ProviderResume) (Info, error) {
	return m.createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport, resume, nil)
}

// CreateAliasedBeadOnlyNamedWithMetadata creates a session bead without
// starting the runtime process, publishing extra metadata atomically.
func (m *Manager) CreateAliasedBeadOnlyNamedWithMetadata(alias, explicitName, template, title, command, workDir, provider, transport string, resume ProviderResume, extraMeta map[string]string) (Info, error) {
	return m.createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport, resume, extraMeta)
}

func (m *Manager) createAliasedBeadOnlyNamed(alias, explicitName, template, title, command, workDir, provider, transport string, resume ProviderResume, extraMeta map[string]string) (Info, error) {
	alias, err := ValidateAlias(alias)
	if err != nil {
		return Info{}, err
	}
	explicitName, err = ValidateExplicitName(explicitName)
	if err != nil {
		return Info{}, err
	}
	if title == "" {
		title = template
	}
	aliasOwner := ""
	if extraMeta["configured_named_session"] == "true" && extraMeta["configured_named_identity"] == alias {
		aliasOwner = alias
	}
	var info Info
	err = withSessionIdentifierReservationLocks([]string{alias, explicitName}, func() error {
		if err := ensureSessionAliasAvailable(m.store, nil, alias, "", aliasOwner); err != nil {
			return err
		}
		if err := ensureSessionNameAvailableForSelfAndOwner(m.store, explicitName, "", aliasOwner); err != nil {
			return err
		}

		var sessionKey string
		if resume.SessionIDFlag != "" {
			generatedKey, genErr := GenerateSessionKey()
			if genErr != nil {
				return fmt.Errorf("generating session key: %w", genErr)
			}
			sessionKey = generatedKey
		}

		meta := map[string]string{
			"template":           template,
			"state":              string(StateStartPending),
			"provider":           provider,
			"work_dir":           workDir,
			"command":            command,
			"resume_flag":        resume.ResumeFlag,
			"resume_style":       resume.ResumeStyle,
			"resume_command":     resume.ResumeCommand,
			"generation":         fmt.Sprintf("%d", DefaultGeneration),
			"continuation_epoch": fmt.Sprintf("%d", DefaultContinuationEpoch),
			"instance_token":     NewInstanceToken(),
		}
		if alias != "" {
			meta["alias"] = alias
		}
		if normalizedTransport := normalizeTransport(provider, transport); normalizedTransport != "" {
			meta["transport"] = normalizedTransport
		}
		if sessionKey != "" {
			meta["session_key"] = sessionKey
		}
		meta["pending_create_claim"] = "true"
		meta["pending_create_started_at"] = pendingCreateStartedAt(time.Now().UTC())
		if explicitName != "" {
			meta["session_name"] = explicitName
			meta["session_name_explicit"] = "true"
		}
		for k, v := range extraMeta {
			meta[k] = v
		}
		if meta["session_origin"] == "" {
			meta["session_origin"] = "ephemeral"
		}
		createdBead, createErr := m.store.Create(beads.Bead{
			Title: title,
			Type:  BeadType,
			Labels: []string{
				LabelSession,
				"template:" + template,
			},
			Metadata: meta,
		})
		if createErr != nil {
			return fmt.Errorf("creating session bead: %w", createErr)
		}
		b := createdBead

		sessName := explicitName
		if sessName == "" {
			sessName = sessionNameFor(b.ID)
			if err := m.store.SetMetadata(b.ID, "session_name", sessName); err != nil {
				_ = m.store.Close(b.ID)
				return fmt.Errorf("storing session name: %w", err)
			}
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata["session_name"] = sessName

		info = m.infoFromBead(b)
		return nil
	})
	if err != nil {
		return Info{}, err
	}
	return info, nil
}

// CreateBeadOnlyNamed creates a session bead without starting the runtime
// process, preserving an optional explicit session_name for the reconciler.
//
// WARNING: withSessionNameReservationLock only serializes callers inside this
// process. Callers MUST also hold WithCitySessionNameLock(cityPath, explicitName)
// when explicitName is non-empty so duplicate names cannot race across processes.
func (m *Manager) CreateBeadOnlyNamed(explicitName, template, title, command, workDir, provider, transport string, _ map[string]string, resume ProviderResume) (Info, error) {
	return m.CreateAliasedBeadOnlyNamed("", explicitName, template, title, command, workDir, provider, transport, nil, resume)
}

// Attach attaches the user's terminal to the session. If the session is
// suspended, it is resumed first using resumeCommand. If the tmux session
// died (active bead but no process), it is restarted.
func (m *Manager) Attach(ctx context.Context, id string, resumeCommand string, hints runtime.Config) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if err := m.ensureRunning(ctx, id, b, sessName, resumeCommand, hints); err != nil {
			return err
		}

		return m.sp.Attach(sessName)
	})
}

// Suspend saves session state and kills the runtime session.
func (m *Manager) Suspend(id string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		// Closed beads are terminal; mutating lifecycle metadata after
		// close produces impossible status=closed + live-state rows.
		if b.Status == "closed" {
			return &IllegalTransitionError{From: StateClosed, Command: CmdSuspend}
		}
		current := State(b.Metadata["state"])
		if current == StateSuspended {
			return nil // idempotent: already suspended
		}
		// failed-create is a create-rollback terminal state: the create never
		// reached creation_complete, so there is no live turn to suspend — only
		// a possibly-leaked runtime process to tear down. `gc stop` issues
		// suspend on every session bead (no state pre-filter), and under a
		// backing-store outage the reconciler cannot reap failed-create beads
		// (its close path requires a reachable store), so suspend is the only
		// thing that can clear the leaked process. Tear the runtime down
		// best-effort and report success rather than rejecting with an
		// illegal-transition error that blocks `gc stop` city-wide (#2597). The
		// bead is left in failed-create for the reconciler to reap once the
		// store is reachable again.
		//
		// Limitation: explicit-named beads whose rollback already cleared
		// session_name (rollbackPendingCreate in
		// cmd/gc/session_lifecycle_parallel.go) fall back to the synthetic
		// sessionNameFor(id) here, so this Stop targets the synthetic name
		// rather than the original explicit name and the leak under the
		// original name persists. That is a strict improvement over the pre-fix
		// state (suspend rejected outright, runtime leaked, `gc stop` blocked
		// city-wide); preserving the original name across rollback for cleanup
		// is tracked as follow-up.
		if current == StateFailedCreate {
			if strings.TrimSpace(sessName) != "" {
				_ = m.sp.Stop(sessName) // best-effort: tear down any leaked runtime
			}
			return nil
		}
		// Normalize legacy/aliased states (empty and awake both mean active)
		// after the failed-create pre-check above, preserving closed-guard-
		// first ordering.
		current = canonicalLifecycleState(current)
		if _, err := Transition(current, CmdSuspend); err != nil {
			return err
		}

		// Kill the runtime session. Stop is provider-idempotent, so call it
		// even when liveness already reports false; tmux remain-on-exit panes
		// can be non-running but still need their session artifact removed.
		if strings.TrimSpace(sessName) != "" {
			running := m.sp.IsRunning(sessName)
			err := m.sp.Stop(sessName)
			if err != nil && !running {
				// Preserve historical Suspend semantics for already-dead
				// sessions: cleanup is best-effort when the runtime did not
				// report a live process before Stop.
				err = nil
			}
			if err != nil {
				return fmt.Errorf("stopping runtime session: %w", err)
			}
		}

		// Update state and suspension timestamp together so stores with a
		// write-through cache preserve one coherent lifecycle transition.
		if err := m.store.Update(id, beads.UpdateOpts{Metadata: map[string]string{
			"state":        string(StateSuspended),
			"suspended_at": time.Now().UTC().Format(time.RFC3339),
		}}); err != nil {
			return fmt.Errorf("updating suspension state: %w", err)
		}

		return nil
	})
}

// RequestFreshRestart marks a session for a controller-owned fresh restart
// without closing its bead or clearing resume metadata immediately.
func (m *Manager) RequestFreshRestart(id string) error {
	return withSessionMutationLock(id, func() error {
		if _, _, err := m.sessionBead(id); err != nil {
			return err
		}
		return m.store.SetMetadataBatch(id, map[string]string{
			"restart_requested":          "true",
			"continuation_reset_pending": "true",
		})
	})
}

// Close ends a conversation permanently.
func (m *Manager) Close(id string) error {
	_, err := m.CloseDetailed(id)
	return err
}

// CloseDetailed ends a conversation permanently and reports cleanup artifacts.
func (m *Manager) CloseDetailed(id string) (CloseResult, error) {
	result := CloseResult{}
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.loadSessionBead(id, true)
		if err != nil {
			return err
		}
		if b.Status == "closed" {
			_ = clearRuntimeMCPServersSnapshot(m.cityPath, id)
			return nil // idempotent: already closed
		}
		// CmdClose is legal from any non-none state; this is effectively a
		// documentation check that will catch future table changes. The
		// canonicalizer treats empty metadata state as StateActive for
		// bootstrap beads and the reconciler's StateAwake alias as StateActive
		// so already-awake beads can close cleanly.
		current := canonicalLifecycleState(State(b.Metadata["state"]))
		if _, err := Transition(current, CmdClose); err != nil {
			return err
		}

		// Stop the live runtime before marking the bead closed. Stop is
		// idempotent for an already-gone session (returns nil), which also lets
		// auto.Provider discard stale ACP route entries for suspended sessions.
		// A genuine terminate failure must propagate and leave the bead open
		// rather than report a "closed but still running" session — swallowing
		// it here previously masked exactly that wedge.
		if err := m.sp.Stop(sessName); err != nil {
			return fmt.Errorf("stopping runtime for session %s: %w", id, err)
		}
		nudgeIDs, capped, err := CancelWaitsAndCollectNudgeIDs(m.store, id, time.Now().UTC())
		if err != nil {
			log.Printf("session %s: closing after wait cancellation lookup failed: %v", id, err)
		}
		if capped {
			log.Printf("session %s: closing after capped wait cancellation lookup", id)
		}
		result.WaitNudgeIDs = append(result.WaitNudgeIDs, nudgeIDs...)
		if err := m.clearWakeAndHoldOverrides(id); err != nil {
			return err
		}
		if err := m.retireConfiguredNamedSessionIdentifiers(id, b); err != nil {
			return err
		}

		if err := m.store.Close(id); err != nil {
			return err
		}
		_ = clearRuntimeMCPServersSnapshot(m.cityPath, id)
		return nil
	})
	return result, err
}

func (m *Manager) clearWakeAndHoldOverrides(id string) error {
	update := map[string]string{
		"pin_awake":    "",
		"held_until":   "",
		"sleep_intent": "",
	}
	if err := m.store.SetMetadataBatch(id, update); err != nil {
		return fmt.Errorf("clearing wake and hold overrides: %w", err)
	}
	return nil
}

func (m *Manager) retireConfiguredNamedSessionIdentifiers(id string, b beads.Bead) error {
	// Recognize configured named sessions by flag OR identity so a
	// partially-tagged bead (identity recorded, boolean flag absent) still
	// releases its reserved runtime name on close instead of stranding the
	// name and blocking respawn (ga-841).
	if !wasConfiguredNamedSession(b) {
		return nil
	}
	update := beads.UpdateOpts{
		Metadata: UpdatedAliasMetadata(b.Metadata, ""),
	}
	update.Metadata["session_name"] = ""
	update.Metadata["session_name_explicit"] = ""
	update.Metadata["pending_create_claim"] = ""
	update.Metadata["pending_create_started_at"] = ""
	if err := m.store.Update(id, update); err != nil {
		return fmt.Errorf("retiring configured named session identifiers: %w", err)
	}
	return nil
}

// Kill force-kills the runtime process for a session without changing bead
// state. This is intended for manual intervention; the reconciler will detect
// the dead process and restart it according to the session's lifecycle rules.
func (m *Manager) Kill(id string) error {
	b, sessName, err := m.sessionBead(id)
	if err != nil {
		return err
	}
	// Accept any state where a runtime process could plausibly exist.
	// The reconciler uses "awake" as equivalent to "active", and metadata
	// state can lag behind reality, so also check provider liveness.
	state := State(b.Metadata["state"])
	switch state {
	case StateActive, StateStartPending, StateCreating, StateDraining, StateAwake:
		// Known live states — proceed.
	default:
		if !m.sp.IsRunning(sessName) {
			return fmt.Errorf("session %s is not active", id)
		}
	}
	return m.sp.Stop(sessName)
}

// BeginDrain transitions a session to the draining state. The caller is
// responsible for signaling the runtime process to finish its work.
// Idempotent: returns nil if the session is already draining.
func (m *Manager) BeginDrain(id, reason string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdDrain, StateDraining)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already draining
		}
		return m.store.SetMetadataBatch(id, BeginDrainPatch(time.Now().UTC(), reason))
	})
}

// Archive transitions a session from draining to archived. Idempotent:
// returns nil if the session is already archived.
func (m *Manager) Archive(id, reason string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdArchive, StateArchived)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already archived
		}
		return m.store.SetMetadataBatch(id, ArchivePatch(time.Now().UTC(), reason, false))
	})
}

// Quarantine marks a session as crash-quarantined until the given time.
// Idempotent: returns nil if the session is already quarantined.
func (m *Manager) Quarantine(id string, until time.Time, cycle int) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdQuarantine, StateQuarantined)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already quarantined
		}
		return m.store.SetMetadataBatch(id, QuarantinePatch(until, cycle))
	})
}

// Reactivate clears archive/quarantine blockers and returns a session to
// asleep so normal wake machinery owns the next runtime start. Idempotent:
// returns nil if the session is already in an awake-eligible state.
func (m *Manager) Reactivate(id string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdWake, StateAsleep)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already in target state
		}
		b, err := m.store.Get(id)
		if err != nil {
			return err
		}
		view := ProjectLifecycle(LifecycleInputFromMetadata(b.Status, b.Metadata))
		// Note: quarantine_cycle is intentionally preserved across reactivations.
		// It tracks how many quarantine rounds the session has been through,
		// enabling eviction after quarantine_max_attempts.
		return m.store.SetMetadataBatch(id, ReactivatePatch(view.ContinuityEligible))
	})
}

// ConfirmCreation transitions a session from creating to active after the
// runtime process has been confirmed alive. Idempotent: returns nil if the
// session is already active.
func (m *Manager) ConfirmCreation(id string) error {
	return withSessionMutationLock(id, func() error {
		cmdLegal, err := m.checkTransition(id, CmdReady, StateActive)
		if err != nil {
			return err
		}
		if !cmdLegal {
			return nil // idempotent: already active
		}
		return m.store.SetMetadataBatch(id, ConfirmStartedPatch(time.Now()))
	})
}

// checkTransition reads the current state of session id and reports whether
// cmd is legal. Empty state metadata is treated as StateActive for legacy
// bootstrap beads (pre-metadata upgrades). Closed beads are terminal and
// reject any lifecycle mutation (callers should use the dedicated Close
// idempotency branch, not a lifecycle transition). Returns:
//   - cmdLegal: true if the command produces a real transition, false if
//     the session is already in targetState (idempotent no-op)
//   - err: *IllegalTransitionError wrapping ErrIllegalTransition when the
//     command is neither legal nor a no-op
//
// MUST be called while holding withSessionMutationLock(id).
func (m *Manager) checkTransition(id string, cmd TransitionCommand, targetState State) (bool, error) {
	b, _, err := m.sessionBead(id)
	if err != nil {
		return false, err
	}
	// Closed beads are terminal. Mutating lifecycle metadata after close
	// would produce impossible status=closed + live-state combinations
	// that the reconciler misreads. Surface a clear illegal-transition
	// error instead of silently mutating.
	if b.Status == "closed" {
		return false, &IllegalTransitionError{From: StateClosed, Command: cmd}
	}
	current := canonicalLifecycleState(State(b.Metadata["state"]))
	if current == targetState {
		return false, nil
	}
	if _, err := Transition(current, cmd); err != nil {
		return false, err
	}
	return true, nil
}

// Rename updates the title of a chat session.
func (m *Manager) Rename(id, title string) error {
	return m.UpdatePresentation(id, &title, nil)
}

// UpdatePresentation updates user-facing session attributes.
func (m *Manager) UpdatePresentation(id string, title *string, alias *string) error {
	return withSessionMutationLock(id, func() error {
		b, sessName, err := m.loadSessionBead(id, true)
		if err != nil {
			return err
		}
		currentAlias := strings.TrimSpace(b.Metadata["alias"])
		var nextAlias string
		if alias != nil {
			validated, err := ValidateAlias(*alias)
			if err != nil {
				return err
			}
			nextAlias = validated
			if strings.TrimSpace(b.Metadata["configured_named_session"]) == "true" && nextAlias != currentAlias {
				return fmt.Errorf("configured named session alias is immutable while config-managed")
			}
		}
		update := beads.UpdateOpts{}
		if title != nil {
			update.Title = title
		}
		if alias != nil {
			return withSessionAliasReservationLock(nextAlias, func() error {
				if nextAlias != currentAlias {
					if err := ensureSessionAliasAvailable(m.store, nil, nextAlias, id, ""); err != nil {
						return err
					}
				}
				update.Metadata = UpdatedAliasMetadata(b.Metadata, nextAlias)
				runtimeRunning := sessName != "" && m.sp != nil && m.sp.IsRunning(sessName)
				if runtimeRunning {
					if err := SyncRuntimeAlias(m.sp, sessName, nextAlias); err != nil {
						return fmt.Errorf("updating runtime alias: %w", err)
					}
				}
				if err := m.store.Update(id, update); err != nil {
					if runtimeRunning {
						if rollbackErr := SyncRuntimeAlias(m.sp, sessName, currentAlias); rollbackErr != nil {
							log.Printf("session %s: restoring runtime alias %q on %s failed: %v", id, currentAlias, sessName, rollbackErr)
						}
					}
					return err
				}
				return nil
			})
		}
		return m.store.Update(id, update)
	})
}

// UpdateTemplateOverrides merges option overrides into the session metadata.
func (m *Manager) UpdateTemplateOverrides(id string, updates map[string]string) (map[string]string, error) {
	var merged map[string]string
	err := withSessionMutationLock(id, func() error {
		b, sessName, err := m.loadSessionBead(id, true)
		if err != nil {
			return err
		}
		state := State(b.Metadata["state"])
		if IsTemplateOverrideRuntimeActive(state) || templateOverrideWakeInFlight(b.Metadata, state, m.now()) || (strings.TrimSpace(sessName) != "" && m.sp != nil && m.sp.IsRunning(sessName)) {
			return fmt.Errorf("%w: template overrides apply only before the next launch", ErrSessionActive)
		}
		overrides, err := ParseTemplateOverrides(b.Metadata)
		if err != nil {
			log.Printf("session %s: repairing malformed template_overrides: %v", id, err)
			overrides = nil
		}
		if overrides == nil {
			overrides = make(map[string]string, len(updates))
		}
		for key, value := range updates {
			overrides[key] = value
		}
		raw, err := json.Marshal(overrides)
		if err != nil {
			return fmt.Errorf("marshal template_overrides: %w", err)
		}
		metadata := map[string]string{"template_overrides": string(raw)}
		for key, value := range updates {
			if key == "initial_message" {
				continue
			}
			metadata[beadmeta.OptionMetadataPrefix+key] = value
		}
		if err := m.store.SetMetadataBatch(id, metadata); err != nil {
			return err
		}
		merged = make(map[string]string, len(overrides))
		for key, value := range overrides {
			merged[key] = value
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// IsTemplateOverrideRuntimeActive reports whether a session state is too live
// for template override changes that only apply on the next launch.
func IsTemplateOverrideRuntimeActive(state State) bool {
	switch state {
	case StateActive, StateAwake, StateStartPending, StateCreating, StateDraining, StateQuarantined:
		return true
	default:
		return false
	}
}

func templateOverrideWakeInFlightGrace() time.Duration {
	return time.Minute + staleKeyDetectDelay + 5*time.Second
}

func templateOverrideWakeInFlight(metadata map[string]string, state State, now time.Time) bool {
	if metadata == nil {
		return false
	}
	switch state {
	case StateFailedCreate, StateDrained, StateArchived:
		return false
	}
	if strings.TrimSpace(metadata["pending_create_claim"]) == "true" {
		return true
	}
	lastWoke := strings.TrimSpace(metadata["last_woke_at"])
	if lastWoke == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	// PreWakePatch records last_woke_at before the reconciler can observe
	// runtime liveness; keep overrides locked out through that startup window.
	return now.UTC().Before(started.UTC().Add(templateOverrideWakeInFlightGrace()))
}

// pruneStateTimestamp returns the timestamp that PruneDetailed compares
// against its cutoff for a session in the given state. Suspended sessions keep
// the historical CreatedAt fallback for legacy beads. Asleep sessions normally
// require slept_at, except legacy drained-asleep beads without slept_at can use
// the bead update timestamp because sleep_reason=drained is terminal.
func pruneStateTimestamp(b beads.Bead, state State) (time.Time, bool) {
	switch state {
	case StateSuspended:
		if raw := b.Metadata["suspended_at"]; raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				return parsed, true
			}
		}
		return b.CreatedAt, true
	case StateAsleep:
		if ts, ok := parsePruneMetadataTimestamp(b.Metadata, "slept_at"); ok {
			return ts, true
		}
		if strings.TrimSpace(b.Metadata["slept_at"]) != "" {
			return time.Time{}, false
		}
		if strings.TrimSpace(b.Metadata["sleep_reason"]) != "drained" {
			return time.Time{}, false
		}
		if !b.UpdatedAt.IsZero() {
			return b.UpdatedAt, true
		}
		if !b.CreatedAt.IsZero() {
			return b.CreatedAt, true
		}
		return time.Time{}, false
	case StateDrained:
		return parsePruneMetadataTimestamp(b.Metadata, "drain_at")
	default:
		return time.Time{}, false
	}
}

func parsePruneMetadataTimestamp(metadata map[string]string, key string) (time.Time, bool) {
	if metadata == nil {
		return time.Time{}, false
	}
	raw := metadata[key]
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// Prune closes suspended sessions whose suspension time is before the given
// cutoff. Active and already-closed sessions are never pruned.
// Returns the number of sessions pruned.
func (m *Manager) Prune(before time.Time) (int, error) {
	result, err := m.PruneDetailed(before)
	return result.Count, err
}

// PruneDetailed closes terminal-state sessions whose state timestamp is before
// the given cutoff and reports the affected session IDs and queued wait nudges.
// When no states are supplied it defaults to [StateSuspended] for backward
// compatibility. Callers may opt in to asleep or drained cleanup by passing
// StateAsleep or StateDrained. StateDrained also matches legacy
// state=asleep/sleep_reason=drained beads.
func (m *Manager) PruneDetailed(before time.Time, states ...State) (PruneResult, error) {
	if len(states) == 0 {
		states = []State{StateSuspended}
	}
	allowed := make(map[State]struct{}, len(states))
	for _, s := range states {
		allowed[s] = struct{}{}
	}
	all, err := m.store.List(beads.ListQuery{
		Label: LabelSession,
	})
	if err != nil {
		return PruneResult{}, fmt.Errorf("listing sessions: %w", err)
	}
	result := PruneResult{}
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.Status == "closed" {
			continue // already closed
		}
		state := State(b.Metadata["state"])
		if !pruneStateAllowed(state, b.Metadata, allowed) {
			continue
		}
		ts, ok := pruneStateTimestamp(b, state)
		if !ok {
			continue
		}
		if !ts.Before(before) {
			continue
		}
		nudgeIDs, capped, err := CancelWaitsAndCollectNudgeIDs(m.store, b.ID, time.Now().UTC())
		if err != nil && !beads.IsLookupLimitError(err) {
			return result, fmt.Errorf("canceling waits for session %s: %w", b.ID, err)
		}
		if capped || beads.IsLookupLimitError(err) {
			log.Printf("session %s: pruning after capped wait nudge lookup: %v", b.ID, err)
		}
		result.WaitNudgeIDs = append(result.WaitNudgeIDs, nudgeIDs...)
		if err := m.store.Close(b.ID); err != nil {
			return result, fmt.Errorf("closing session %s: %w", b.ID, err)
		}
		result.Count++
		result.SessionIDs = append(result.SessionIDs, b.ID)
	}
	return result, nil
}

func pruneStateAllowed(state State, metadata map[string]string, allowed map[State]struct{}) bool {
	if _, ok := allowed[state]; ok {
		return true
	}
	if state != StateAsleep {
		return false
	}
	if strings.TrimSpace(metadata["sleep_reason"]) != "drained" {
		return false
	}
	_, ok := allowed[StateDrained]
	return ok
}

// Get returns info about a single session.
func (m *Manager) Get(id string) (Info, error) {
	info, _, err := m.GetWithBead(id)
	return info, err
}

// GetWithBead returns session info and the underlying bead in a single
// store fetch, for callers that need both views (e.g. spec build plus
// metadata lookup) without a redundant store.Get.
func (m *Manager) GetWithBead(id string) (Info, beads.Bead, error) {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return Info{}, beads.Bead{}, err
	}
	return m.infoFromBead(b), b, nil
}

// GetWithPersistedResponse returns the runtime-enriched session Info plus the
// persisted-response projection (status + metadata) in a single store fetch.
// It is the domain-typed read the API response path routes through: the caller
// gets session.Info for the scalar/runtime fields and session.PersistedResponse
// for the status/metadata-derived fields, without a raw *beads.Bead crossing the
// boundary or a redundant second store.Get beside Get. Bead serialization stays
// confined here via PersistedResponseFromBead.
func (m *Manager) GetWithPersistedResponse(id string) (Info, PersistedResponse, error) {
	info, b, err := m.GetWithBead(id)
	if err != nil {
		return Info{}, PersistedResponse{}, err
	}
	return info, PersistedResponseFromBead(b), nil
}

// SessionInfoFromBead converts an already-loaded session bead to Info,
// applying the same enrichment as Get. Callers that have just resolved
// the bead can use this to avoid a second store.Get.
func (m *Manager) SessionInfoFromBead(b beads.Bead) Info {
	return m.infoFromBead(b)
}

// ObserveRuntimeForInfo reports live provider state for a session whose Info
// has already been loaded by the caller, avoiding a redundant store fetch.
func (m *Manager) ObserveRuntimeForInfo(info Info, processNames []string) RuntimeObservation {
	obs := RuntimeObservation{SessionName: info.SessionName}
	if strings.TrimSpace(info.SessionName) == "" || m.sp == nil {
		return obs
	}
	liveness := runtime.ObserveLiveness(m.sp, info.SessionName, processNames)
	obs.Running = liveness.Running
	obs.Alive = liveness.Alive
	if obs.Running {
		obs.Attached = m.sp.IsAttached(info.SessionName)
		if lastActive, err := m.sp.GetLastActivity(info.SessionName); err == nil {
			obs.LastActive = lastActive
		}
	}
	return obs
}

// ListResult holds the results of a ListFull call, including the raw beads
// to avoid redundant store queries.
type ListResult struct {
	Sessions []Info
	Beads    []beads.Bead // All session beads (unfiltered by state/template)
}

// List returns all chat sessions, optionally filtered by state and template.
func (m *Manager) List(stateFilter string, templateFilter string) ([]Info, error) {
	r, err := m.ListFull(stateFilter, templateFilter)
	if err != nil {
		return nil, err
	}
	return r.Sessions, nil
}

// ListFull is like List but also returns the raw session beads to avoid
// redundant store queries by the caller (e.g., for building a bead index).
func (m *Manager) ListFull(stateFilter string, templateFilter string) (*ListResult, error) {
	all, err := m.store.List(beads.ListQuery{
		Label: LabelSession,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return m.ListFullFromBeads(all, stateFilter, templateFilter), nil
}

// ListFullFromBeads is like ListFull but reuses a caller-supplied slice of
// session-labeled beads. Callers that already loaded session beads can avoid
// a second store scan by passing the same slice here.
func (m *Manager) ListFullFromBeads(all []beads.Bead, stateFilter string, templateFilter string) *ListResult {
	result := make([]Info, 0, len(all))
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if !sessionMatchesFilters(b, stateFilter, templateFilter) {
			continue
		}
		result = append(result, m.infoFromBead(b))
	}
	return &ListResult{Sessions: result, Beads: all}
}

// Peek captures the last N lines of output from the session.
func (m *Manager) Peek(id string, lines int) (string, error) {
	b, sessName, err := m.loadSessionBead(id, true)
	if err != nil {
		return "", err
	}
	if b.Status == "closed" || State(b.Metadata["state"]) == StateSuspended {
		return "", fmt.Errorf("%w: %s", ErrSessionInactive, id)
	}
	return m.sp.Peek(sessName, lines)
}

// infoFromBead converts a bead to an Info struct, enriching the persisted
// projection (InfoFromPersistedBead) with live runtime state. The persisted
// fields come from the shared codec so the manager and the Info-typed domain
// store agree on the storage projection; only the runtime overlay (transport
// detection, ACP routing, stale-state downgrade, attachment/last-active) lives
// here, where the runtime provider is available.
func (m *Manager) infoFromBead(b beads.Bead) Info {
	info := InfoFromPersistedBead(b)
	sessName := info.SessionName

	if !info.Closed {
		transport, _ := m.transportForBead(b, sessName)
		info.Transport = transport
		_ = m.routeACPIfNeeded(b.Metadata["provider"], transport, sessName)

		// Surface stale "awake" / "active" beads as dormant immediately.
		// The controller also heals metadata on the next tick.
		if m.sp != nil && info.State == StateActive && !m.sp.IsRunning(sessName) {
			info.State = StateAsleep
		}
	}

	// Enrich with live runtime state if active.
	if info.State == StateActive && m.sp != nil {
		info.Attached = m.sp.IsAttached(sessName)
		if t, err := m.sp.GetLastActivity(sessName); err == nil && !t.IsZero() {
			info.LastActive = t
		}
	}

	return info
}

// PersistSessionKey stores a provider resume key on an existing session when
// the key is learned after creation (for example from transcript evidence).
// Existing non-empty keys are preserved.
func (m *Manager) PersistSessionKey(id, sessionKey string) error {
	sessionKey = strings.TrimSpace(sessionKey)
	if id == "" || sessionKey == "" {
		return nil
	}
	return withSessionMutationLock(id, func() error {
		b, _, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(b.Metadata["session_key"]) != "" {
			return nil
		}
		if err := m.store.SetMetadata(id, "session_key", sessionKey); err != nil {
			return fmt.Errorf("storing session key: %w", err)
		}
		return nil
	})
}

// MetadataKeyInvocationUsageCursor stores the identity of the most recently
// telemetry-recorded invocation for a session: the provider message id
// (msg_*) when the transcript carries one, otherwise the transcript entry
// UUID. It prevents double-counting of the gc.agent.tokens.* counters
// across prompt operations: handles are rebuilt per gc process, so the
// dedup cursor must live on the session bead, not in memory.
const MetadataKeyInvocationUsageCursor = "invocation_usage_cursor"

// PersistInvocationUsageCursor stores the invocation-usage telemetry cursor
// on an existing session. Unlike PersistSessionKey it overwrites any
// existing value — the cursor advances with every recorded invocation.
// Empty id or cursor is a no-op.
func (m *Manager) PersistInvocationUsageCursor(id, cursor string) error {
	cursor = strings.TrimSpace(cursor)
	if id == "" || cursor == "" {
		return nil
	}
	return withSessionMutationLock(id, func() error {
		if _, _, err := m.sessionBead(id); err != nil {
			return err
		}
		if err := m.store.SetMetadata(id, MetadataKeyInvocationUsageCursor, cursor); err != nil {
			return fmt.Errorf("storing invocation usage cursor: %w", err)
		}
		return nil
	})
}

// sessionNameFor derives the tmux session name from a bead ID.
// Uses the "s-" prefix to avoid collision with agent sessions.
func sessionNameFor(beadID string) string {
	return "s-" + strings.ReplaceAll(beadID, "/", "--")
}

// BuildResumeCommand constructs the resume command from stored session info.
// Priority: explicit ResumeCommand (with {{.SessionKey}} expansion) >
// ResumeFlag/ResumeStyle auto-construction > stored command as-is.
func BuildResumeCommand(info Info) string {
	// Explicit resume_command takes precedence.
	if info.ResumeCommand != "" && info.SessionKey != "" {
		return strings.ReplaceAll(info.ResumeCommand, "{{.SessionKey}}", info.SessionKey)
	}

	if info.ResumeFlag == "" || info.SessionKey == "" {
		// Provider doesn't support resume or no key — use stored command.
		cmd := info.Command
		if cmd == "" {
			cmd = info.Provider
		}
		return cmd
	}

	// Build resume command based on style.
	cmd := info.Command
	if cmd == "" {
		cmd = info.Provider
	}
	switch info.ResumeStyle {
	case "subcommand":
		// Insert subcommand after the binary name:
		//   "codex --model o3" → "codex resume <key> --model o3"
		parts := strings.SplitN(cmd, " ", 2)
		if len(parts) == 2 {
			return parts[0] + " " + info.ResumeFlag + " " + info.SessionKey + " " + parts[1]
		}
		return cmd + " " + info.ResumeFlag + " " + info.SessionKey
	default: // "flag"
		// command --resume <key> (e.g., claude --resume <uuid>)
		return cmd + " " + info.ResumeFlag + " " + info.SessionKey
	}
}

// mergeEnv merges two env maps, with override taking precedence.
func mergeEnv(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string)
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// GenerateSessionKey creates a random UUID v4 for session identification.
func GenerateSessionKey() (string, error) {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}
