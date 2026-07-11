package session

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ApplyPatch returns a copy of info with a MetadataPatch applied to its
// metadata-derived fields. It is the typed "write-returns-Info" half of the
// session front door (front-door migration Step 6d): the reconciler applies a
// patch to the persisted bead via Store.ApplyPatch and, instead of re-reading
// and re-projecting the whole bead (the raw refreshSessionInfo path) or issuing
// a store Get, folds the SAME patch onto the coherent Info snapshot here.
//
// It is byte-identical to a full re-projection of the patched metadata:
//
//	info.ApplyPatch(p)  ==  InfoFromPersistedBead(bead{Status, Type, Title, ...,
//	                            Metadata: p.Apply(meta)})
//
// for the metadata-derived fields, where info == InfoFromPersistedBead(bead).
// Only fields whose source key appears in the patch are re-derived, from that
// key's raw patch value, using the same per-key logic as InfoFromPersistedBead;
// every other field carries forward unchanged. Bead-level fields (ID, Type,
// Title, Labels, CreatedAt) and the live runtime overlay (Attached, LastActive)
// are never touched by a metadata patch, so they carry forward. Status-derived
// facts (Closed, and the closed-blanking of State) are NOT reconstructable from
// a metadata patch — a status close is a separate refresh case (Store.Get) —
// so ApplyPatch reads the carried-forward Closed and never flips it.
//
// The mapping is deliberately parallel to InfoFromPersistedBead;
// TestInfoApplyPatchMatchesReprojection is the equivalence oracle that guards
// the two against drift, exactly as TestSessionClassifierInfoEquivalence guards
// the classifier siblings.
func (info Info) ApplyPatch(patch MetadataPatch) Info {
	for key, v := range patch {
		switch key {
		case "session_name":
			info.SessionNameMetadata = v
			if v == "" {
				info.SessionName = sessionNameFor(info.ID)
			} else {
				info.SessionName = v
			}
		case "state":
			info.MetadataState = v
			if info.Closed {
				info.State = "" // closed beads have no runtime state
			} else {
				info.State = normalizeInfoState(State(v))
			}
		case "template":
			info.Template = v
		case "alias":
			info.Alias = v
		case "agent_name":
			info.AgentName = v
		case "provider":
			info.Provider = v
			info.Transport = normalizeTransport(v, info.TransportMetadata)
		case "transport":
			info.TransportMetadata = v
			info.Transport = normalizeTransport(info.Provider, v)
		case "command":
			info.Command = v
		case "work_dir":
			info.WorkDir = v
		case "session_key":
			info.SessionKey = v
		case "resume_flag":
			info.ResumeFlag = v
		case "resume_style":
			info.ResumeStyle = v
		case "resume_command":
			info.ResumeCommand = v
		case "continuation_epoch":
			info.ContinuationEpoch = v
		case "sleep_reason":
			info.SleepReason = v
		case NamedSessionIdentityMetadata:
			info.ConfiguredNamedIdentity = v
		case NamedSessionMetadataKey:
			info.ConfiguredNamedSession = strings.TrimSpace(v) == "true"
		case NamedSessionModeMetadata:
			info.ConfiguredNamedMode = v
		case "common_name":
			info.CommonName = v
		case "pool_slot":
			info.PoolSlot = v
		case "pool_managed":
			info.PoolManaged = strings.TrimSpace(v) == "true"
		case "session_origin":
			info.SessionOrigin = v
		case "dependency_only":
			info.DependencyOnly = strings.TrimSpace(v) == "true"
			info.DependencyOnlyMetadata = v
		case "manual_session":
			info.ManualSession = strings.TrimSpace(v) == "true"
			info.ManualSessionMetadata = v
		case MCPIdentityMetadataKey:
			info.MCPIdentity = v
		case MCPServersSnapshotMetadataKey:
			info.MCPServersSnapshot = v
		case "provider_terminal_error":
			info.ProviderTerminalError = v
		case "session_health":
			info.HealthState = v
		case "session_health_reason":
			info.HealthReason = v
		case "session_drainable":
			info.Drainable = strings.TrimSpace(v) == "true"
		case beadmeta.TriggerBeadIDMetadataKey:
			info.TriggerBeadID = v
		case beadmeta.TriggerBeadStoreRefMetadataKey:
			info.TriggerBeadStoreRef = v
		case beadmeta.BrainParentSIDMetadataKey:
			info.BrainParentSID = v
		case beadmeta.PackMetadataKey:
			info.Pack = v
		case "pending_create_claim":
			info.PendingCreateClaim = strings.TrimSpace(v) == "true"
			info.PendingCreateClaimMetadata = v
		case "pending_create_started_at":
			info.PendingCreateStartedAt = v
		case "quarantined_until":
			info.QuarantinedUntil = v
		case aliasHistoryMetadataKey:
			info.AliasHistory = normalizeAliasList(strings.Split(v, ","), "")
		case "continuity_eligible":
			info.ContinuityEligible = v
		case "last_woke_at":
			info.LastWokeAt = v
		case "state_reason":
			info.StateReason = v
		case "creation_complete_at":
			info.CreationCompleteAt = v
		case "continuation_reset_pending":
			info.ContinuationResetPending = v
		case ResetCommittedAtKey:
			info.ResetCommittedAt = v
		case "generation":
			info.Generation = v
		case "started_config_hash":
			info.StartedConfigHash = v
		case "pin_awake":
			info.PinAwake = v
		case "held_until":
			info.HeldUntil = v
		case "wait_hold":
			info.WaitHold = v
		case "churn_count":
			info.ChurnCount = v
		case "wake_mode":
			info.WakeMode = v
		case "sleep_intent":
			info.SleepIntent = v
		case "instance_token":
			info.InstanceToken = v
		case "detached_at":
			info.DetachedAt = v
		case CurrentBeadIDKey:
			info.CurrentlyProcessingBeadID = v
		case "core_hash_breakdown":
			info.CoreHashBreakdown = v
		case "started_provision_hash":
			info.StartedProvisionHash = v
		case "started_launch_hash":
			info.StartedLaunchHash = v
		case "started_live_hash":
			info.StartedLiveHash = v
		case "config_drift_deferred_at":
			info.ConfigDriftDeferredAt = v
		case "config_drift_deferred_key":
			info.ConfigDriftDeferredKey = v
		case "attached_config_drift_deferred_at":
			info.AttachedConfigDriftDeferredAt = v
		case "attached_config_drift_deferred_key":
			info.AttachedConfigDriftDeferredKey = v
		case "stranded_event_emitted_at":
			info.StrandedEventEmittedAt = v
		case "session_name_explicit":
			info.SessionNameExplicit = v
		case "wake_request":
			info.WakeRequest = v
		case "restart_requested":
			info.RestartRequested = v
		case "session_id_flag":
			info.SessionIDFlag = v
		case "template_overrides":
			info.TemplateOverrides = v
		case "wake_attempts":
			info.WakeAttemptsMetadata = v
			if n, err := strconv.Atoi(v); err == nil {
				info.WakeAttempts = n
			} else {
				info.WakeAttempts = 0
			}
		case "provider_kind":
			info.ProviderKind = v
		case MetadataLastNudgeDeliveredAt:
			info.LastNudgeDeliveredAt = time.Time{}
			if raw := strings.TrimSpace(v); raw != "" {
				if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
					info.LastNudgeDeliveredAt = parsed
				}
			}
		default:
			// Keys InfoFromPersistedBead does not project (e.g. live_hash,
			// startup_dialog_verified, env.*) have no Info field, so a patch to
			// them changes no Info fact. Ignoring them keeps ApplyPatch
			// byte-identical to a full re-projection.
		}
	}
	return info
}

// MarkClosed returns a copy of info reflecting an in-memory status close. When
// the reconciler closes a session bead this tick it sets session.Status =
// "closed" on the working bead; the coherent Info snapshot must match without a
// store re-read. Status is the source of exactly the two facts
// InfoFromPersistedBead derives from b.Status == "closed": Closed becomes true,
// and State is blanked (a closed bead carries no runtime state). Every
// metadata-derived field is independent of Status, so it carries forward
// unchanged.
//
// MarkClosed is the status-close counterpart to ApplyPatch, which deliberately
// never flips Closed (a metadata patch cannot carry a status change). Together
// they are the write-returns-Info snapshot refresh (front-door migration Step
// 6d): ApplyPatch folds a metadata batch, MarkClosed folds an in-memory status
// close, each byte-identical to re-projecting the mutated bead — so the
// reconciler refreshes the snapshot from the mutation it just applied instead of
// re-projecting the raw working bead or issuing a store Get.
//
// TestInfoMarkClosedMatchesReprojection is the equivalence oracle: for any open
// bead b, InfoFromPersistedBead(b).MarkClosed() equals
// InfoFromPersistedBead(b with Status "closed").
func (info Info) MarkClosed() Info {
	info.Closed = true
	info.State = "" // closed beads have no runtime state
	return info
}
