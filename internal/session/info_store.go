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
		PoolAliasConflict:       b.Metadata["pool_alias_conflict"],
		PoolAliasConflictCount:  b.Metadata["pool_alias_conflict_count"],
		PoolAliasConflictAt:     b.Metadata["pool_alias_conflict_at"],
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
		PackWorkspace:       b.Metadata[beadmeta.PackWorkspaceMetadataKey],
		WorkDirCanonical:    b.Metadata[beadmeta.WorkDirMetadataKey],
		WorkerDir:           b.Metadata[beadmeta.WorkerDirMetadataKey],

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
		AwakeStartedAt:             b.Metadata["awake_started_at"],
		UsageComputeEmittedAt:      b.Metadata["usage_compute_emitted_at"],
		StateReason:                b.Metadata["state_reason"],
		CreationCompleteAt:         b.Metadata["creation_complete_at"],
		ContinuationResetPending:   b.Metadata["continuation_reset_pending"],
		SessionCircuitState:        b.Metadata[SessionCircuitStateMetadataKey],
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
		LiveHash:                       b.Metadata["live_hash"],
		StartupDialogVerified:          b.Metadata["startup_dialog_verified"],
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
		BuiltinAncestor:                b.Metadata["builtin_ancestor"],

		// sleep-policy cluster (raw mirrors; see Info doc). The 7 key literals
		// match the inline keys in cmd/gc/session_sleep.go today (they are inline
		// literals there, not shared constants). The ApplyPatch reprojection oracle
		// pins only the in-package InfoFromPersistedBead↔ApplyPatch parallelism —
		// it does NOT catch a cmd/gc-side key rename, which surfaces when the sleep
		// helpers migrate onto these fields (W6).
		SleepPolicyFingerprint:       b.Metadata["sleep_policy_fingerprint"],
		RequestedSleepAfterIdle:      b.Metadata["requested_sleep_after_idle"],
		EffectiveSleepAfterIdle:      b.Metadata["effective_sleep_after_idle"],
		SleepPolicySource:            b.Metadata["sleep_policy_source"],
		SleepCapability:              b.Metadata["sleep_capability"],
		SleepPolicyAdjustmentReason:  b.Metadata["sleep_policy_adjustment_reason"],
		ConfigWakeSuppressedMetadata: b.Metadata["config_wake_suppressed"],
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
// reads persisted state through this type's GetPersistedResponse and pairs it
// with Manager.EnrichInfo for the runtime overlay (see the api sessionGetEnriched
// composition and worker.SessionCatalog.GetWithPersistedResponse). The reconciler
// already routes its writes through this type.
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
// ErrSessionNotFound when the bead EXISTS but is not a session bead (or carries
// an empty id); an ABSENT id surfaces the store's not-found error wrapped as
// `loading session %q` (NOT ErrSessionNotFound). Callers must not
// errors.Is(err, ErrSessionNotFound) to detect absence — check for the wrapped
// beads.ErrNotFound instead. See validatedBead.
func (s *Store) Get(id string) (Info, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return Info{}, err
	}
	return InfoFromPersistedBead(b), nil
}

// GetPersistedResponse returns the persisted session.Info paired with the
// persisted-response projection (status + metadata) for id, in a single store
// fetch. It is the persisted-read half of the session Get read model — pair it
// with Manager.EnrichInfo for the runtime overlay (the api sessionGetEnriched
// composition and worker.SessionCatalog.GetWithPersistedResponse do exactly
// that): the caller gets both projections without a raw *beads.Bead crossing the
// boundary and without a second store.Get. It shares Get's exact
// error contract (both route through validatedBead): ErrSessionNotFound for a
// present-but-non-session bead, and the wrapped store not-found error (NOT
// ErrSessionNotFound) for an absent id.
func (s *Store) GetPersistedResponse(id string) (Info, PersistedResponse, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return Info{}, PersistedResponse{}, err
	}
	return InfoFromPersistedBead(b), PersistedResponseFromBead(b), nil
}

// validatedBead loads the session bead for id. A load failure (including an
// absent id) is wrapped with `loading session %q` context; a loaded bead that is
// not a session bead (or has an empty id) is rejected with ErrSessionNotFound. It
// is the shared read behind Get and GetPersistedResponse so the two agree on
// validation and error text (a single source of truth for "is this a session
// bead"). Note the split: absence yields the wrapped store error, NOT
// ErrSessionNotFound — that sentinel is reserved for a present non-session bead.
func (s *Store) validatedBead(id string) (beads.Bead, error) {
	// Nil-inner-store short-circuit, mirroring ListAll's listAllBeads guard
	// (s == nil || s.store.Store == nil): a nil backing store cannot produce the
	// bead, so treat it as absence — the wrapped store not-found error, matching
	// the absent-id path below and NOT the ErrSessionNotFound sentinel (reserved
	// for a present non-session bead). Without this a non-nil *Store wrapping a
	// nil inner store would panic in s.store.Get.
	if s == nil || s.store.Store == nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, beads.ErrNotFound)
	}
	b, err := s.store.Get(id)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(b.ID) == "" || !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return b, nil
}

// List returns the persisted session.Info for all session beads, applying the
// same state and template filtering semantics as the catalog listing. An empty
// stateFilter excludes closed sessions; stateFilter "all" includes everything.
// Only session.Info is returned — no raw beads cross this boundary.
func (s *Store) List(stateFilter, templateFilter string) ([]Info, error) {
	// IncludeClosed so the in-memory filter below can honor state=closed and
	// state=all; sessionMatchesFilters drops closed beads for the default and
	// non-closed filters, matching the shared session-list filtering semantics.
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

// ListByMetadataInfos returns the Info projection of every bead matching the given
// metadata filters, keeping the raw-bead codec confined to this edge. It is the typed
// front door for the session-log workdir fallback's ListByMetadata scans (the callers
// need only Info fields). limit is passed through to the store; a zero limit is
// unbounded. No raw bead escapes.
func (s *Store) ListByMetadataInfos(filters map[string]string, limit int) ([]Info, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	found, err := s.store.ListByMetadata(filters, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(found))
	for _, b := range found {
		out = append(out, InfoFromPersistedBead(b))
	}
	return out, nil
}

// ListLabeledSessionInfosUnfiltered returns the Info projection of every OPEN bead
// carrying the gc:session label, WITHOUT the IsSessionBeadOrRepairable narrowing that
// List applies. It is the label-only, closed-excluded, unfiltered lister the
// city-stop sleep-reason sweep needs: that sweep marks possibly-damaged
// gc:session-labeled beads whose type is a non-empty non-"session" value, which
// List's IsSessionBeadOrRepairable filter would drop, and it must NOT widen to the
// ListAll type+label union (which would also mark label-lost type-only beads — a
// behavior change). ListByLabel already excludes closed beads by default; the
// explicit closed skip keeps the closed-excluded contract byte-stable across store
// backends. No raw bead escapes.
func (s *Store) ListLabeledSessionInfosUnfiltered() ([]Info, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	labeled, err := s.store.ListByLabel(LabelSession, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(labeled))
	for _, b := range labeled {
		if b.Status == "closed" {
			continue
		}
		out = append(out, InfoFromPersistedBead(b))
	}
	return out, nil
}

// sessionMatchesFilters reports whether a session bead passes the state and
// template filters. It is the single predicate for session-list filtering,
// shared by the Store.List projection and (via sessionMatchesFiltersInfo) the
// Info-fed listing.
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

// sessionMatchesFiltersInfo is the Info-taking twin of sessionMatchesFilters. It
// recomputes the state from MetadataState (the RAW state metadata, NOT the
// closed-blanked/normalized Info.State — the bead form derives `state` from
// b.Metadata["state"] regardless of close), reads Closed for the open/closed
// status compares, and Template for the template filter.
//
// ACCEPTED DELTA: the bead form compares the exact status string
// (b.Status=="open"), while this twin has only Closed (== b.Status=="closed") to
// work with — Info carries no raw Status. For the SDK's binary open/closed
// invariant the two are identical; for a hypothetical out-of-band status
// (e.g. "archived") the twin's `sf=="open"` (== !Closed) is WIDER than the bead
// form's exact `b.Status=="open"`. They diverge ONLY on the "open" filter for a
// non-open, non-closed status; everywhere else they agree.
// TestSessionMatchesFiltersInfoEquivalence pins the byte-identity across the
// open/closed corpus (including the closed-with-raw-state trap) AND pins this
// documented open-filter delta against an exotic-status row.
func sessionMatchesFiltersInfo(info Info, stateFilter, templateFilter string) bool {
	state := normalizeInfoState(State(info.MetadataState))

	switch {
	case stateFilter != "" && stateFilter != "all":
		match := false
		for _, sf := range strings.Split(stateFilter, ",") {
			switch {
			case sf == "closed" && info.Closed:
				match = true
			case sf == "open" && !info.Closed:
				match = true
			case !info.Closed && sf == string(state):
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
		if info.Closed {
			return false
		}
	}

	if templateFilter != "" && info.Template != templateFilter {
		return false
	}
	return true
}
