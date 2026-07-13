package session

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// infoFromPersistedBeadFrozen is a verbatim copy of the pre-S09b struct-literal
// projection of infoFromPersistedBead, carrying THIS tree's full key set (the
// ~19 keys beyond the original commit: pool_alias_conflict*, PackWorkspace /
// WorkDirCanonical / WorkerDir, awake_started_at, usage_compute_emitted_at,
// SessionCircuitState, live_hash, startup_dialog_verified, builtin_ancestor,
// and the 7-key sleep-policy cluster). It is the INDEPENDENT oracle for the
// table-driven codec: TestInfoCodecProjectionParity asserts the new table loop
// reproduces this frozen reference byte-for-byte. It must NOT be refactored to
// call the table — its whole value is being written a different way. If a
// genuine projection change is ever intended, this frozen copy fails loudly and
// forces an explicit decision.
func infoFromPersistedBeadFrozen(b beads.Bead) Info {
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(b.ID)
	}
	closed := b.Status == "closed"

	state := normalizeInfoState(State(b.Metadata["state"]))
	if closed {
		state = ""
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

		// Canonical-identity record mirrors (verbatim). S19 Stage 2 (write-only).
		CanonicalInstanceNameMetadata: b.Metadata[CanonicalInstanceNameMetadata],
		CanonicalPoolSlotMetadata:     b.Metadata[CanonicalPoolSlotMetadata],
		// Priming-marker mirrors (verbatim). S19 Stage 2 (write-only).
		PrimedAtMetadata:           b.Metadata[PrimedAtMetadataKey],
		PrimingAttemptedAtMetadata: b.Metadata[PrimingAttemptedAtMetadataKey],
		PromptHashMetadata:         b.Metadata[PromptHashMetadataKey],
		MCPIdentity:                b.Metadata[MCPIdentityMetadataKey],
		MCPServersSnapshot:         b.Metadata[MCPServersSnapshotMetadataKey],

		ProviderTerminalError: b.Metadata["provider_terminal_error"],
		HealthState:           b.Metadata["session_health"],
		HealthReason:          b.Metadata["session_health_reason"],
		Drainable:             strings.TrimSpace(b.Metadata["session_drainable"]) == "true",

		TriggerBeadID:       b.Metadata[beadmeta.TriggerBeadIDMetadataKey],
		TriggerBeadStoreRef: b.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey],
		BrainParentSID:      b.Metadata[beadmeta.BrainParentSIDMetadataKey],
		Pack:                b.Metadata[beadmeta.PackMetadataKey],
		PackWorkspace:       b.Metadata[beadmeta.PackWorkspaceMetadataKey],
		WorkDirCanonical:    b.Metadata[beadmeta.WorkDirMetadataKey],
		WorkerDir:           b.Metadata[beadmeta.WorkerDirMetadataKey],

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
		UnknownStateFirstSeen:          b.Metadata["unknown_state_first_seen"],
		UnknownStateValue:              b.Metadata["unknown_state_value"],
		UnknownStateEscalatedAt:        b.Metadata["unknown_state_escalated_at"],
		SessionNameExplicit:            b.Metadata["session_name_explicit"],
		WakeRequest:                    b.Metadata["wake_request"],
		RestartRequested:               b.Metadata["restart_requested"],
		SessionIDFlag:                  b.Metadata["session_id_flag"],
		TemplateOverrides:              b.Metadata["template_overrides"],
		WakeAttemptsMetadata:           b.Metadata["wake_attempts"],
		ProviderKind:                   b.Metadata["provider_kind"],
		BuiltinAncestor:                b.Metadata["builtin_ancestor"],

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

// TestInfoCodecProjectionParity (T2) is the independent projection oracle: the
// new table-driven infoFromPersistedBead must equal the frozen pre-S09b
// struct-literal projection byte-for-byte, over the diverse oracle base beads
// (populated/closed/no-name/acp/sparse) plus per-key edge fixtures that reach
// the parsed/coupled branches (Atoi failure, RFC3339 garbage, alias
// normalization, whitespace bools, awake/drained state remap). Unlike the
// fold==reprojection oracle (which compares two table-driven paths), this pins
// the table against a copy of the OLD code, so a shared table bug is caught.
func TestInfoCodecProjectionParity(t *testing.T) {
	beadsToCheck := oracleBaseBeads()

	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	edgeMeta := []map[string]string{
		{"wake_attempts": "not-an-int"},
		{"wake_attempts": "12"},
		{"wake_attempts": " 3 "}, // Atoi rejects whitespace -> 0
		{MetadataLastNudgeDeliveredAt: "garbage"},
		{MetadataLastNudgeDeliveredAt: " 2025-06-01T00:00:00Z "},
		{aliasHistoryMetadataKey: " a , b ,a, c "},
		{aliasHistoryMetadataKey: ""},
		{"pool_managed": " true ", "dependency_only": " true ", "manual_session": "TRUE"},
		{"state": "awake"},
		{"state": "drained"},
		{"provider": "acp"},                     // provider fallback -> transport "acp"
		{"provider": "acp", "transport": ""},    // explicit empty transport, provider fallback
		{"provider": "claude", "transport": ""}, // no fallback -> transport ""
		{"session_name": ""},                    // sessionNameFor fallback
	}
	for i, m := range edgeMeta {
		beadsToCheck = append(beadsToCheck,
			beads.Bead{ID: "edge-" + strconv.Itoa(i), Type: "gc:session", Status: "open", Title: "E", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: m},
			beads.Bead{ID: "edge-closed-" + strconv.Itoa(i), Type: "gc:session", Status: "closed", Title: "E", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: m},
		)
	}

	for _, b := range beadsToCheck {
		got := infoFromPersistedBead(b)
		want := infoFromPersistedBeadFrozen(b)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("bead=%s: table projection diverged from frozen reference\n got=%+v\nwant=%+v", b.ID, got, want)
		}
	}
}

// TestInfoCodecKeysMatchProjectedList (T1) asserts the table's key set equals
// the hand-maintained allProjectedMetadataKeys list used by the fold oracle.
// A silently dropped or extra table entry is caught here even if the fold
// oracle's key list drifts in lockstep.
func TestInfoCodecKeysMatchProjectedList(t *testing.T) {
	tableKeys := map[string]bool{}
	for i := range infoKeyCodec {
		k := infoKeyCodec[i].key
		if tableKeys[k] {
			t.Errorf("duplicate key %q in infoKeyCodec", k)
		}
		tableKeys[k] = true
	}
	listKeys := map[string]bool{}
	for _, k := range allProjectedMetadataKeys {
		listKeys[k] = true
	}
	for k := range tableKeys {
		if !listKeys[k] {
			t.Errorf("infoKeyCodec key %q missing from allProjectedMetadataKeys", k)
		}
	}
	for k := range listKeys {
		if !tableKeys[k] {
			t.Errorf("allProjectedMetadataKeys key %q missing from infoKeyCodec", k)
		}
	}
	if len(infoKeyIndex) != len(infoKeyCodec) {
		t.Errorf("infoKeyIndex size %d != infoKeyCodec size %d (duplicate key collapsed?)", len(infoKeyIndex), len(infoKeyCodec))
	}
}

// TestInfoCodecEmptyStringClears (T3) drives the empty-string-clear invariant
// (I3) off the table: for every key, folding {key: ""} onto a fully-populated
// projection must equal projecting the same bead with that key deleted.
func TestInfoCodecEmptyStringClears(t *testing.T) {
	base := oracleBaseBeads()[0] // fully-populated open bead
	baseInfo := infoFromPersistedBead(base)
	for i := range infoKeyCodec {
		key := infoKeyCodec[i].key
		cleared := baseInfo.ApplyPatch(MetadataPatch{key: ""})

		deletedMeta := make(map[string]string, len(base.Metadata))
		for k, v := range base.Metadata {
			if k == key {
				continue
			}
			deletedMeta[k] = v
		}
		deleted := base
		deleted.Metadata = deletedMeta
		want := infoFromPersistedBead(deleted)

		if !reflect.DeepEqual(cleared, want) {
			t.Errorf("key=%q: empty-string clear diverged from key-deleted projection\n got=%+v\nwant=%+v", key, cleared, want)
		}
	}
}

// TestInfoCodecFieldsDisjoint (T4) locks invariant I6: every pair of table
// setters writes a disjoint set of Info fields, EXCEPT the documented
// provider/transport pair (both derive Transport). It also asserts
// provider precedes transport in the table so the derived Transport is
// finalized with both raw mirrors in scope.
func TestInfoCodecFieldsDisjoint(t *testing.T) {
	// touchedFields applies a setter with a sentinel value to a zero Info and
	// returns the set of struct field indices it changed.
	touchedFields := func(set func(*Info, string), v string) map[int]bool {
		var info Info
		set(&info, v)
		changed := map[int]bool{}
		zero := Info{}
		rv, rz := reflect.ValueOf(info), reflect.ValueOf(zero)
		for f := 0; f < rv.NumField(); f++ {
			if !reflect.DeepEqual(rv.Field(f).Interface(), rz.Field(f).Interface()) {
				changed[f] = true
			}
		}
		return changed
	}

	// sentinelFor returns a per-key value that actually moves every field the
	// key's setter writes off its zero value, so each setter contributes a
	// non-empty touched-field set to the pairwise check. The default "1" trims
	// to a truthy int and a non-empty string, but the `== "true"` boolean
	// setters only flip their bool on "true", and the RFC3339-only
	// last_nudge_delivered_at setter only moves on a valid timestamp; without
	// these overrides those setters report an empty set and silently drop out of
	// the disjointness assertion — the exact invariant this test exists to prove.
	sentinelFor := func(key string) string {
		switch key {
		case NamedSessionMetadataKey, "pool_managed", "dependency_only",
			"manual_session", "session_drainable", "pending_create_claim":
			return "true"
		case MetadataLastNudgeDeliveredAt:
			return "2025-06-01T00:00:00Z"
		default:
			return "1"
		}
	}

	fields := make([]map[int]bool, len(infoKeyCodec))
	for i := range infoKeyCodec {
		fields[i] = touchedFields(infoKeyCodec[i].set, sentinelFor(infoKeyCodec[i].key))
	}

	providerIdx, transportIdx := -1, -1
	for i := range infoKeyCodec {
		switch infoKeyCodec[i].key {
		case "provider":
			providerIdx = i
		case "transport":
			transportIdx = i
		}
	}
	if providerIdx == -1 || transportIdx == -1 {
		t.Fatal("provider/transport keys not found in table")
	}
	if providerIdx >= transportIdx {
		t.Errorf("provider (idx %d) must precede transport (idx %d) in infoKeyCodec", providerIdx, transportIdx)
	}

	isProviderTransportPair := func(a, b int) bool {
		return (a == providerIdx && b == transportIdx) || (a == transportIdx && b == providerIdx)
	}

	for a := range infoKeyCodec {
		for b := a + 1; b < len(infoKeyCodec); b++ {
			if isProviderTransportPair(a, b) {
				continue
			}
			for f := range fields[a] {
				if fields[b][f] {
					t.Errorf("keys %q and %q both write Info field index %d (non-disjoint)", infoKeyCodec[a].key, infoKeyCodec[b].key, f)
				}
			}
		}
	}
}

// TestInfoCodecProviderTransportOrderConverges (part of R2 mitigation) applies
// a two-key provider+transport patch in BOTH iteration orders and asserts the
// final Transport matches the from-scratch projection either way. Guards the
// E-5 convergence property against a future reorder.
func TestInfoCodecProviderTransportOrderConverges(t *testing.T) {
	base := oracleBaseBeads()[3] // acp base: provider fallback is live
	baseInfo := infoFromPersistedBead(base)

	quadrants := []struct{ provider, transport string }{
		{"gemini", "tmux"},
		{"acp", ""},
		{"claude", ""},
		{"", "acp"},
		{"", ""},
	}
	for _, q := range quadrants {
		// Apply as separate single-key patches in each order (single-key
		// ApplyPatch calls make the ordering explicit and deterministic).
		fwd := baseInfo.ApplyPatch(MetadataPatch{"provider": q.provider}).ApplyPatch(MetadataPatch{"transport": q.transport})
		rev := baseInfo.ApplyPatch(MetadataPatch{"transport": q.transport}).ApplyPatch(MetadataPatch{"provider": q.provider})

		wantMeta := make(map[string]string, len(base.Metadata))
		for k, v := range base.Metadata {
			wantMeta[k] = v
		}
		wantMeta["provider"] = q.provider
		wantMeta["transport"] = q.transport
		wantBead := base
		wantBead.Metadata = wantMeta
		want := infoFromPersistedBead(wantBead)

		if fwd.Transport != want.Transport {
			t.Errorf("provider=%q transport=%q: fwd Transport=%q, want %q", q.provider, q.transport, fwd.Transport, want.Transport)
		}
		if rev.Transport != want.Transport {
			t.Errorf("provider=%q transport=%q: rev Transport=%q, want %q", q.provider, q.transport, rev.Transport, want.Transport)
		}
	}
}
