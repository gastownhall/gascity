package session

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// infoKeySpec is one metadata key's codec: how a raw metadata value becomes
// Info fields. The SAME closure drives both directions of the metadata⇄Info
// codec — projection (InfoFromPersistedBead) and fold (Info.ApplyPatch) — so a
// fold is a re-projection of that one key by construction, and the two can no
// longer drift apart.
//
// Contract of set: it is total over the empty string and correct ONLY when
// applied to a fresh (zero-valued) Info in projection order, OR folded onto a
// coherent Info snapshot in patch semantics. In both regimes an absent key
// reads as "" and every setter produces the correctly-cleared state for "".
// Do not reuse a setter against an arbitrary partially-mutated Info outside
// those two regimes — the projection/patch equivalence assumes one of them.
type infoKeySpec struct {
	key string                     // exact on-store metadata key (byte-identical to today)
	set func(info *Info, v string) // typed setter: writes ALL Info fields derived from this key
}

// infoKeyCodec is the single source of truth for the metadata-derived half of
// the Info projection. Ordering is irrelevant for correctness EXCEPT for two
// documented dependencies (invariant I6):
//   - the bead-level prologue (ID, Closed, …) in InfoFromPersistedBead runs
//     before this table, because session_name reads info.ID and state reads
//     info.Closed;
//   - provider is listed before transport, so both raw mirrors are in scope
//     when the derived Transport is finalized (they converge to the same
//     value in either order — see the provider/transport entries).
//
// Every other pair of entries writes a disjoint set of Info fields (asserted by
// TestInfoCodecFieldsDisjoint). The clustering mirrors the old struct literal
// so review provenance survives.
var infoKeyCodec = []infoKeySpec{
	// core / identity cluster
	{"template", func(i *Info, v string) { i.Template = v }},
	{"alias", func(i *Info, v string) { i.Alias = v }},
	{"agent_name", func(i *Info, v string) { i.AgentName = v }},
	{"command", func(i *Info, v string) { i.Command = v }},
	{"work_dir", func(i *Info, v string) { i.WorkDir = v }},
	{"session_key", func(i *Info, v string) { i.SessionKey = v }},
	{"resume_flag", func(i *Info, v string) { i.ResumeFlag = v }},
	{"resume_style", func(i *Info, v string) { i.ResumeStyle = v }},
	{"resume_command", func(i *Info, v string) { i.ResumeCommand = v }},
	{"continuation_epoch", func(i *Info, v string) { i.ContinuationEpoch = v }},
	{"sleep_reason", func(i *Info, v string) { i.SleepReason = v }},

	// session_name: fallback-defaulted; reads info.ID (set in the prologue).
	{"session_name", func(i *Info, v string) {
		i.SessionNameMetadata = v
		if v == "" {
			i.SessionName = sessionNameFor(i.ID)
		} else {
			i.SessionName = v
		}
	}},

	// state: normalize + closed-blank; reads info.Closed (set in the prologue).
	{"state", func(i *Info, v string) {
		i.MetadataState = v
		if i.Closed {
			i.State = "" // closed beads have no runtime state
		} else {
			i.State = normalizeInfoState(State(v))
		}
	}},

	// provider/transport cross-field pair: each setter re-derives Transport from
	// the sibling's current raw mirror. provider MUST precede transport so both
	// raw values are in scope when Transport is finalized; the two converge to
	// normalizeTransport(provider, transport) regardless of arrival order.
	{"provider", func(i *Info, v string) {
		i.Provider = v
		i.Transport = normalizeTransport(v, i.TransportMetadata)
	}},
	{"transport", func(i *Info, v string) {
		i.TransportMetadata = v
		i.Transport = normalizeTransport(i.Provider, v)
	}},

	// identity / pool / named-session cluster
	{NamedSessionIdentityMetadata, func(i *Info, v string) { i.ConfiguredNamedIdentity = v }},
	{NamedSessionMetadataKey, func(i *Info, v string) { i.ConfiguredNamedSession = strings.TrimSpace(v) == "true" }},
	{NamedSessionModeMetadata, func(i *Info, v string) { i.ConfiguredNamedMode = v }},
	{"common_name", func(i *Info, v string) { i.CommonName = v }},
	{"pool_slot", func(i *Info, v string) { i.PoolSlot = v }},
	{"pool_managed", func(i *Info, v string) { i.PoolManaged = strings.TrimSpace(v) == "true" }},
	{"session_origin", func(i *Info, v string) { i.SessionOrigin = v }},
	{"dependency_only", func(i *Info, v string) {
		i.DependencyOnly = strings.TrimSpace(v) == "true"
		i.DependencyOnlyMetadata = v
	}},
	{"manual_session", func(i *Info, v string) {
		i.ManualSession = strings.TrimSpace(v) == "true"
		i.ManualSessionMetadata = v
	}},

	// Canonical-identity record mirrors (verbatim). The typed record is derived
	// on demand via Info.CanonicalIdentity(); these keep the raw values so the
	// fold copies them per-key. S19 Stage 2 is WRITE-ONLY: stamped at
	// create/adoption but read by no decision path yet.
	{CanonicalInstanceNameMetadata, func(i *Info, v string) { i.CanonicalInstanceNameMetadata = v }},
	{CanonicalPoolSlotMetadata, func(i *Info, v string) { i.CanonicalPoolSlotMetadata = v }},

	{MCPIdentityMetadataKey, func(i *Info, v string) { i.MCPIdentity = v }},
	{MCPServersSnapshotMetadataKey, func(i *Info, v string) { i.MCPServersSnapshot = v }},

	// health / provider-terminal-error cluster
	{"provider_terminal_error", func(i *Info, v string) { i.ProviderTerminalError = v }},
	{"session_health", func(i *Info, v string) { i.HealthState = v }},
	{"session_health_reason", func(i *Info, v string) { i.HealthReason = v }},
	{"session_drainable", func(i *Info, v string) { i.Drainable = strings.TrimSpace(v) == "true" }},

	// trigger / brain-parent cluster (canonical gc.* keys via beadmeta)
	{beadmeta.TriggerBeadIDMetadataKey, func(i *Info, v string) { i.TriggerBeadID = v }},
	{beadmeta.TriggerBeadStoreRefMetadataKey, func(i *Info, v string) { i.TriggerBeadStoreRef = v }},
	{beadmeta.BrainParentSIDMetadataKey, func(i *Info, v string) { i.BrainParentSID = v }},
	{beadmeta.PackMetadataKey, func(i *Info, v string) { i.Pack = v }},

	// state / bookkeeping cluster
	{"pending_create_claim", func(i *Info, v string) {
		i.PendingCreateClaim = strings.TrimSpace(v) == "true"
		i.PendingCreateClaimMetadata = v
	}},
	{"pending_create_started_at", func(i *Info, v string) { i.PendingCreateStartedAt = v }},
	{"quarantined_until", func(i *Info, v string) { i.QuarantinedUntil = v }},
	{aliasHistoryMetadataKey, func(i *Info, v string) {
		i.AliasHistory = normalizeAliasList(strings.Split(v, ","), "")
	}},
	{"continuity_eligible", func(i *Info, v string) { i.ContinuityEligible = v }},
	{"last_woke_at", func(i *Info, v string) { i.LastWokeAt = v }},
	{"state_reason", func(i *Info, v string) { i.StateReason = v }},
	{"creation_complete_at", func(i *Info, v string) { i.CreationCompleteAt = v }},
	{"continuation_reset_pending", func(i *Info, v string) { i.ContinuationResetPending = v }},
	{ResetCommittedAtKey, func(i *Info, v string) { i.ResetCommittedAt = v }},
	{"generation", func(i *Info, v string) { i.Generation = v }},
	{"started_config_hash", func(i *Info, v string) { i.StartedConfigHash = v }},
	{"pin_awake", func(i *Info, v string) { i.PinAwake = v }},

	// reconciler decision-read cluster (front-door Phase 5)
	{"held_until", func(i *Info, v string) { i.HeldUntil = v }},
	{"wait_hold", func(i *Info, v string) { i.WaitHold = v }},
	{"churn_count", func(i *Info, v string) { i.ChurnCount = v }},
	{"wake_mode", func(i *Info, v string) { i.WakeMode = v }},
	{"sleep_intent", func(i *Info, v string) { i.SleepIntent = v }},
	{"instance_token", func(i *Info, v string) { i.InstanceToken = v }},
	{"detached_at", func(i *Info, v string) { i.DetachedAt = v }},
	{CurrentBeadIDKey, func(i *Info, v string) { i.CurrentlyProcessingBeadID = v }},
	{"core_hash_breakdown", func(i *Info, v string) { i.CoreHashBreakdown = v }},
	{"started_provision_hash", func(i *Info, v string) { i.StartedProvisionHash = v }},
	{"started_launch_hash", func(i *Info, v string) { i.StartedLaunchHash = v }},
	{"started_live_hash", func(i *Info, v string) { i.StartedLiveHash = v }},
	{"config_drift_deferred_at", func(i *Info, v string) { i.ConfigDriftDeferredAt = v }},
	{"config_drift_deferred_key", func(i *Info, v string) { i.ConfigDriftDeferredKey = v }},
	{"attached_config_drift_deferred_at", func(i *Info, v string) { i.AttachedConfigDriftDeferredAt = v }},
	{"attached_config_drift_deferred_key", func(i *Info, v string) { i.AttachedConfigDriftDeferredKey = v }},
	{"stranded_event_emitted_at", func(i *Info, v string) { i.StrandedEventEmittedAt = v }},
	{"session_name_explicit", func(i *Info, v string) { i.SessionNameExplicit = v }},
	{"wake_request", func(i *Info, v string) { i.WakeRequest = v }},
	{"restart_requested", func(i *Info, v string) { i.RestartRequested = v }},
	{"session_id_flag", func(i *Info, v string) { i.SessionIDFlag = v }},
	{"template_overrides", func(i *Info, v string) { i.TemplateOverrides = v }},
	{"provider_kind", func(i *Info, v string) { i.ProviderKind = v }},

	// wake_attempts: int + raw mirror. The total form (explicit = 0 on parse
	// failure) matches ApplyPatch and, on a fresh Info, agrees with the old
	// projection's no-set-on-failure. Atoi accepts leading +/- but not
	// whitespace — no trimming, to stay byte-identical.
	{"wake_attempts", func(i *Info, v string) {
		i.WakeAttemptsMetadata = v
		if n, err := strconv.Atoi(v); err == nil {
			i.WakeAttempts = n
		} else {
			i.WakeAttempts = 0
		}
	}},

	// last_nudge_delivered_at: RFC3339 time. Reset-to-zero first (clears a
	// carried-forward value in the patch direction; a no-op on a fresh Info).
	{MetadataLastNudgeDeliveredAt, func(i *Info, v string) {
		i.LastNudgeDeliveredAt = time.Time{}
		if raw := strings.TrimSpace(v); raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				i.LastNudgeDeliveredAt = parsed
			}
		}
	}},
}

// infoKeyIndex maps each metadata key to its codec spec for O(1) ApplyPatch
// lookup. Built once in init() and never mutated afterward, so concurrent
// reads by reconciler goroutines are race-free by construction.
var infoKeyIndex = func() map[string]*infoKeySpec {
	idx := make(map[string]*infoKeySpec, len(infoKeyCodec))
	for i := range infoKeyCodec {
		spec := &infoKeyCodec[i]
		idx[spec.key] = spec
	}
	return idx
}()
