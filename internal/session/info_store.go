package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// InfoFromPersistedBead projects a persisted session bead onto session.Info
// using only data stored on the bead — no live runtime overlay (no liveness
// probe, transport detection, or ACP routing). It is the pure, side-effect-free
// half of the manager codec: Manager.infoFromBead applies this projection and
// then enriches it with runtime state.
//
// Because the projection reads only bead fields, it is invariant across storage
// backends: a bead persisted to bd, sqlite, or postgres round-trips to the same
// Info. Callers that need live runtime state (Attached, runtime-downgraded
// State, detected transport) must go through Manager, not this function.
func InfoFromPersistedBead(b beads.Bead) Info {
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(b.ID)
	}
	closed := b.Status == "closed"

	state := normalizeInfoState(State(b.Metadata["state"]))
	if closed {
		state = "" // closed beads have no runtime state
	}

	info := Info{
		ID:            b.ID,
		Type:          b.Type,
		Template:      b.Metadata["template"],
		State:         state,
		Closed:        closed,
		Title:         b.Title,
		Alias:         b.Metadata["alias"],
		AgentName:     b.Metadata["agent_name"],
		Provider:      b.Metadata["provider"],
		Transport:     transportFromMetadata(b),
		Command:       b.Metadata["command"],
		WorkDir:       b.Metadata["work_dir"],
		SessionName:   sessName,
		SessionKey:    b.Metadata["session_key"],
		ResumeFlag:    b.Metadata["resume_flag"],
		ResumeStyle:   b.Metadata["resume_style"],
		ResumeCommand: b.Metadata["resume_command"],
		CreatedAt:     b.CreatedAt,

		ContinuationEpoch: b.Metadata["continuation_epoch"],
		SleepReason:       b.Metadata["sleep_reason"],

		// identity / pool / named-session cluster
		ConfiguredNamedIdentity: b.Metadata[NamedSessionIdentityMetadata],
		ConfiguredNamedSession:  strings.TrimSpace(b.Metadata[NamedSessionMetadataKey]) == "true",
		ConfiguredNamedMode:     b.Metadata[NamedSessionModeMetadata],
		CommonName:              b.Metadata["common_name"],
		PoolSlot:                b.Metadata["pool_slot"],
		PoolManaged:             strings.TrimSpace(b.Metadata["pool_managed"]) == "true",
		SessionOrigin:           b.Metadata["session_origin"],
		DependencyOnly:          strings.TrimSpace(b.Metadata["dependency_only"]) == "true",
		DependencyOnlyMetadata:  b.Metadata["dependency_only"],
		ManualSession:           strings.TrimSpace(b.Metadata["manual_session"]) == "true",
		ManualSessionMetadata:   b.Metadata["manual_session"],
		Labels:                  b.Labels,
		MCPIdentity:             b.Metadata[MCPIdentityMetadataKey],
		MCPServersSnapshot:      b.Metadata[MCPServersSnapshotMetadataKey],

		// health / provider-terminal-error cluster. The key literals mirror the
		// cmd/gc session_reconcile constants (session_health, session_drainable,
		// …); the classifier-equivalence test guards against drift.
		ProviderTerminalError: b.Metadata["provider_terminal_error"],
		HealthState:           b.Metadata["session_health"],
		HealthReason:          b.Metadata["session_health_reason"],
		Drainable:             strings.TrimSpace(b.Metadata["session_drainable"]) == "true",

		// trigger / brain-parent cluster (canonical gc.* keys via beadmeta).
		TriggerBeadID:       b.Metadata[beadmeta.TriggerBeadIDMetadataKey],
		TriggerBeadStoreRef: b.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey],
		BrainParentSID:      b.Metadata[beadmeta.BrainParentSIDMetadataKey],
		Pack:                b.Metadata[beadmeta.PackMetadataKey],

		// state / bookkeeping cluster. MetadataState is the RAW state metadata,
		// kept verbatim so the reconciler classifiers read the same value the
		// bead carried (Info.State above is the normalized, closed-blanked form).
		MetadataState:              b.Metadata["state"],
		SessionNameMetadata:        b.Metadata["session_name"],
		PendingCreateClaim:         strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true",
		PendingCreateClaimMetadata: b.Metadata["pending_create_claim"],
		PendingCreateStartedAt:     b.Metadata["pending_create_started_at"],
		QuarantinedUntil:           b.Metadata["quarantined_until"],
		AliasHistory:               AliasHistory(b.Metadata),
		ContinuityEligible:         b.Metadata["continuity_eligible"],
		TransportMetadata:          b.Metadata["transport"],
		LastWokeAt:                 b.Metadata["last_woke_at"],
		StateReason:                b.Metadata["state_reason"],
		CreationCompleteAt:         b.Metadata["creation_complete_at"],
		ContinuationResetPending:   b.Metadata["continuation_reset_pending"],
		ResetCommittedAt:           b.Metadata[ResetCommittedAtKey],
		Generation:                 b.Metadata["generation"],
		StartedConfigHash:          b.Metadata["started_config_hash"],
		PinAwake:                   b.Metadata["pin_awake"],

		// reconciler decision-read cluster (front-door Phase 5). Raw mirrors of
		// the keys the reconciler decision paths still crack inline. The key
		// literals mirror the cmd/gc reconciler constants (config_drift_deferred_*,
		// attached_config_drift_deferred_*, stranded_event_emitted_at, …); the
		// classifier-equivalence oracle feeds those constants and so guards these
		// literals against drift. CurrentBeadIDKey is a session-package constant.
		HeldUntil:                      b.Metadata["held_until"],
		WaitHold:                       b.Metadata["wait_hold"],
		ChurnCount:                     b.Metadata["churn_count"],
		WakeMode:                       b.Metadata["wake_mode"],
		SleepIntent:                    b.Metadata["sleep_intent"],
		InstanceToken:                  b.Metadata["instance_token"],
		DetachedAt:                     b.Metadata["detached_at"],
		CurrentlyProcessingBeadID:      b.Metadata[CurrentBeadIDKey],
		CoreHashBreakdown:              b.Metadata["core_hash_breakdown"],
		StartedProvisionHash:           b.Metadata["started_provision_hash"],
		StartedLaunchHash:              b.Metadata["started_launch_hash"],
		StartedLiveHash:                b.Metadata["started_live_hash"],
		ConfigDriftDeferredAt:          b.Metadata["config_drift_deferred_at"],
		ConfigDriftDeferredKey:         b.Metadata["config_drift_deferred_key"],
		AttachedConfigDriftDeferredAt:  b.Metadata["attached_config_drift_deferred_at"],
		AttachedConfigDriftDeferredKey: b.Metadata["attached_config_drift_deferred_key"],
		StrandedEventEmittedAt:         b.Metadata["stranded_event_emitted_at"],
		SessionNameExplicit:            b.Metadata["session_name_explicit"],
		WakeRequest:                    b.Metadata["wake_request"],
		RestartRequested:               b.Metadata["restart_requested"],
		SessionIDFlag:                  b.Metadata["session_id_flag"],
		TemplateOverrides:              b.Metadata["template_overrides"],
		WakeAttemptsMetadata:           b.Metadata["wake_attempts"],
		ProviderKind:                   b.Metadata["provider_kind"],
	}
	if n, err := strconv.Atoi(b.Metadata["wake_attempts"]); err == nil {
		info.WakeAttempts = n
	}
	if raw := strings.TrimSpace(b.Metadata[MetadataLastNudgeDeliveredAt]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			info.LastNudgeDeliveredAt = parsed
		}
	}
	return info
}

// Store is the session-domain front door over a session-class bead store: the
// single typed seam through which callers read and write sessions without
// touching *beads.Bead. The read half (Get / List, projecting via
// InfoFromPersistedBead) lives here; the write half (ApplyPatch + the typed
// lifecycle methods) lives in store.go. Bead serialization — SetMetadataBatch,
// Update, Close, the metadata-key vocabulary — is confined inside this type.
// (Formerly named InfoStore, after its read return type, when it was read-only.)
//
// The Get/List projection is the persisted view only — no live runtime overlay.
// Callers that need live runtime enrichment (liveness, attachment, detected
// transport) still go through session.Manager. The API/response-building layer
// currently reads persisted state via Manager.GetWithPersistedResponse (same
// InfoFromPersistedBead codec); routing that read path through Store is a
// follow-up. The reconciler already routes its writes through this type.
type Store struct {
	store beads.SessionStore
}

// NewStore wraps a strongly-typed session-class store as the session-domain
// front door. The wrapper holds the typed beads.SessionStore by value; the
// embedded .Store is used for all bead access internally.
func NewStore(store beads.SessionStore) *Store {
	return &Store{store: store}
}

// Get returns the persisted session.Info for the given id. It returns
// ErrSessionNotFound when no session bead exists for the id.
func (s *Store) Get(id string) (Info, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return Info{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(b.ID) == "" || !IsSessionBeadOrRepairable(b) {
		return Info{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return InfoFromPersistedBead(b), nil
}

// List returns the persisted session.Info for all session beads, applying the
// same state and template filtering semantics as the catalog listing. An empty
// stateFilter excludes closed sessions; stateFilter "all" includes everything.
// Only session.Info is returned — no raw beads cross this boundary.
func (s *Store) List(stateFilter, templateFilter string) ([]Info, error) {
	// IncludeClosed so the in-memory filter below can honor state=closed and
	// state=all; sessionMatchesFilters drops closed beads for the default and
	// non-closed filters, matching Manager.ListFullFromBeads semantics.
	all, err := s.store.List(beads.ListQuery{
		Label:         LabelSession,
		Sort:          beads.SortCreatedDesc,
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	out := make([]Info, 0, len(all))
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if !sessionMatchesFilters(b, stateFilter, templateFilter) {
			continue
		}
		out = append(out, InfoFromPersistedBead(b))
	}
	return out, nil
}

// sessionMatchesFilters reports whether a session bead passes the state and
// template filters, using the same rules as Manager.ListFullFromBeads so the
// Info-typed listing stays projection-identical to the existing catalog path.
func sessionMatchesFilters(b beads.Bead, stateFilter, templateFilter string) bool {
	state := normalizeInfoState(State(b.Metadata["state"]))

	switch {
	case stateFilter != "" && stateFilter != "all":
		match := false
		for _, sf := range strings.Split(stateFilter, ",") {
			switch {
			case sf == "closed" && b.Status == "closed":
				match = true
			case sf == "open" && b.Status == "open":
				match = true
			case b.Status != "closed" && sf == string(state):
				match = true
			}
			if match {
				break
			}
		}
		if !match {
			return false
		}
	case stateFilter == "":
		if b.Status == "closed" {
			return false
		}
	}

	if templateFilter != "" && b.Metadata["template"] != templateFilter {
		return false
	}
	return true
}
