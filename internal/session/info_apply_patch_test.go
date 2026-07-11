package session

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// allProjectedMetadataKeys is every metadata key InfoFromPersistedBead reads.
// It is maintained by hand: exercising a single-key patch for each key catches
// an ApplyPatch case that is missing or maps to the wrong field. It does NOT
// mechanically guard completeness — a future codec key added to
// InfoFromPersistedBead must ALSO be added here (and to ApplyPatch) or its
// round-trip goes unexercised. Key-driven cases alone are not enough: the
// oracle also needs base beads / patch values that reach the non-identity
// branches of the coupled normalizers (normalizeInfoState awake/drained,
// normalizeTransport's provider fallback), which oracleBaseBeads/oraclePatches
// supply below.
var allProjectedMetadataKeys = []string{
	"session_name", "state", "template", "alias", "agent_name", "provider",
	"transport", "command", "work_dir", "session_key", "resume_flag",
	"resume_style", "resume_command", "continuation_epoch", "sleep_reason",
	NamedSessionIdentityMetadata, NamedSessionMetadataKey, NamedSessionModeMetadata,
	"common_name", "pool_slot", "pool_managed", "session_origin",
	"dependency_only", "manual_session", MCPIdentityMetadataKey,
	MCPServersSnapshotMetadataKey, "provider_terminal_error", "session_health",
	"session_health_reason", "session_drainable", beadmeta.TriggerBeadIDMetadataKey,
	beadmeta.TriggerBeadStoreRefMetadataKey, beadmeta.BrainParentSIDMetadataKey,
	beadmeta.PackMetadataKey,
	"pending_create_claim", "pending_create_started_at", "quarantined_until",
	aliasHistoryMetadataKey, "continuity_eligible", "last_woke_at", "state_reason",
	"creation_complete_at", "continuation_reset_pending", ResetCommittedAtKey,
	"generation", "started_config_hash", "pin_awake", "held_until", "wait_hold",
	"churn_count", "wake_mode", "sleep_intent", "instance_token", "detached_at",
	CurrentBeadIDKey, "core_hash_breakdown", "started_provision_hash",
	"started_launch_hash", "started_live_hash", "config_drift_deferred_at",
	"config_drift_deferred_key", "attached_config_drift_deferred_at",
	"attached_config_drift_deferred_key", "stranded_event_emitted_at",
	"session_name_explicit", "wake_request", "restart_requested",
	"session_id_flag", "template_overrides", "wake_attempts",
	MetadataLastNudgeDeliveredAt, "provider_kind",
}

// oracleBaseBeads returns diverse session beads: a fully-populated open bead, the
// same populated bead but closed (so State-blanking and Closed carry-forward are
// exercised), a bead with no persisted session_name (so the sessionNameFor
// fallback is exercised), and a sparse bead.
func oracleBaseBeads() []beads.Bead {
	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	populated := map[string]string{
		"session_name": "sess-name-1", "state": "creating", "template": "worker",
		"alias": "al-1", "agent_name": "agent-1", "provider": "claude",
		"transport": "acp", "command": "run", "work_dir": "/w", "session_key": "sk",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "{{.SessionKey}}",
		"continuation_epoch": "ep", "sleep_reason": "idle",
		NamedSessionIdentityMetadata: "ident", NamedSessionMetadataKey: "true",
		NamedSessionModeMetadata: "solo", "common_name": "cn", "pool_slot": "2",
		"pool_managed": "true", "session_origin": "named", "dependency_only": "true",
		"manual_session": "true", MCPIdentityMetadataKey: "mcp", MCPServersSnapshotMetadataKey: "snap",
		"provider_terminal_error": "", "session_health": "healthy", "session_health_reason": "",
		"session_drainable": "true", beadmeta.TriggerBeadIDMetadataKey: "tb",
		beadmeta.TriggerBeadStoreRefMetadataKey: "ref", beadmeta.BrainParentSIDMetadataKey: "bp",
		beadmeta.PackMetadataKey: "pk",
		"pending_create_claim":   "true", "pending_create_started_at": "2026-01-01T00:00:00Z",
		"quarantined_until": "2026-01-05T00:00:00Z", aliasHistoryMetadataKey: "old-a,old-b",
		"continuity_eligible": "true", "last_woke_at": "2026-01-02T00:00:00Z",
		"state_reason": "creation_complete", "creation_complete_at": "2026-01-02T01:00:00Z",
		"continuation_reset_pending": "true", ResetCommittedAtKey: "2026-01-02T02:00:00Z",
		"generation": "4", "started_config_hash": "cfg", "pin_awake": "true",
		"held_until": "2026-01-03T00:00:00Z", "wait_hold": "op", "churn_count": "2",
		"wake_mode": "fresh", "sleep_intent": "idle-stop-pending", "instance_token": "it",
		"detached_at": "2026-01-04T00:00:00Z", CurrentBeadIDKey: "bead-9",
		"core_hash_breakdown": `{"a":1}`, "started_provision_hash": "ph",
		"started_launch_hash": "lh", "started_live_hash": "lvh",
		"config_drift_deferred_at": "2026-01-06T00:00:00Z", "config_drift_deferred_key": "k",
		"attached_config_drift_deferred_at":  "2026-01-07T00:00:00Z",
		"attached_config_drift_deferred_key": "ak", "stranded_event_emitted_at": "2026-01-08T00:00:00Z",
		"session_name_explicit": "true", "wake_request": "explicit", "restart_requested": "true",
		"session_id_flag": "--session-id", "template_overrides": `{"x":"y"}`, "wake_attempts": "3",
		MetadataLastNudgeDeliveredAt: "2026-01-09T00:00:00Z", "provider_kind": "claude",
	}
	clone := func(m map[string]string) map[string]string {
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	noName := clone(populated)
	delete(noName, "session_name")

	// acp base: provider "acp" with NO transport metadata, so Info.Transport
	// derives from the provider fallback in normalizeTransport (transport==""
	// && provider=="acp" -> "acp"). This is the one input region where the
	// provider argument to normalizeTransport matters; a provider/transport
	// patch here must re-derive Transport through that fallback.
	acp := clone(populated)
	acp["provider"] = "acp"
	delete(acp, "transport")

	return []beads.Bead{
		{ID: "sess-open", Type: "gc:session", Status: "open", Title: "T", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: clone(populated)},
		{ID: "sess-closed", Type: "gc:session", Status: "closed", Title: "T", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: clone(populated)},
		{ID: "sess-noname", Type: "gc:session", Status: "open", Title: "T", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: noName},
		{ID: "sess-acp", Type: "gc:session", Status: "open", Title: "T", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: acp},
		{ID: "sess-sparse", Type: "gc:session", Status: "open", Title: "", Labels: []string{"gc:session"}, CreatedAt: created, Metadata: map[string]string{}},
	}
}

// oraclePatches returns the patches applied to every base bead: a single-key
// set and a single-key clear for every projected key, plus edge cases that
// stress the coupled/parsed projections (State-blanking, sessionNameFor
// fallback, provider/transport co-recompute, int/time parsing, alias history
// normalization, bool trimming) and an unknown key that must be ignored.
func oraclePatches() []MetadataPatch {
	var patches []MetadataPatch
	patches = append(patches, MetadataPatch{}) // empty patch is a no-op
	for _, k := range allProjectedMetadataKeys {
		patches = append(patches, MetadataPatch{k: "patched-" + k})
		patches = append(patches, MetadataPatch{k: ""})
	}
	edge := []MetadataPatch{
		{"provider": "gemini", "transport": "tmux"},
		{"provider": "gemini"}, // on the acp base, Transport must flip "acp" -> ""
		{"provider": "acp"},    // reaches normalizeTransport's provider fallback
		{"transport": ""},      // on the acp base, provider fallback yields "acp"
		{"state": "idle"},
		{"state": "draining"},
		{"state": "awake"},   // normalizeInfoState remaps awake -> active
		{"state": "drained"}, // normalizeInfoState remaps drained -> asleep
		{"session_name": ""},
		{"session_name": "renamed"},
		{"wake_attempts": "7"},
		{"wake_attempts": "not-an-int"},
		{"wake_attempts": ""},
		{MetadataLastNudgeDeliveredAt: "2025-06-01T00:00:00Z"},
		{MetadataLastNudgeDeliveredAt: "garbage"},
		{MetadataLastNudgeDeliveredAt: ""},
		{aliasHistoryMetadataKey: " a , b ,a, c "},
		{aliasHistoryMetadataKey: ""},
		{"pool_managed": " true "},
		{"pool_managed": "TRUE"},
		{"pending_create_claim": "yes"},    // non-canonical: Metadata mirror verbatim, bool false
		{"pending_create_claim": " true "}, // untrimmed mirror vs trimmed bool
		{"manual_session": "1"},
		{"session_drainable": "true"},
		{"live_hash": "ignored"},         // unknown key: must not change Info
		{"startup_dialog_verified": "z"}, // unknown key
		{"state": "idle", "session_name": "", "provider": "codex", "wake_attempts": "9", "held_until": ""}, // multi-key mix
	}
	return append(patches, edge...)
}

// reprojectBead returns base with its metadata replaced by patch.Apply(base) —
// the full-re-projection ground truth ApplyPatch must equal.
func reprojectBead(base beads.Bead, patch MetadataPatch) beads.Bead {
	out := base
	out.Metadata = patch.Apply(base.Metadata)
	return out
}

// TestInfoApplyPatchMatchesReprojection is the byte-identity oracle: for every
// (base, patch), folding the patch onto the projected Info via ApplyPatch must
// equal projecting the patched metadata from scratch. This guards ApplyPatch
// against drift from InfoFromPersistedBead — the write-returns-Info contract the
// Step-6d snapshot refresh depends on.
func TestInfoApplyPatchMatchesReprojection(t *testing.T) {
	for _, base := range oracleBaseBeads() {
		baseInfo := InfoFromPersistedBead(base)
		for _, patch := range oraclePatches() {
			got := baseInfo.ApplyPatch(patch)
			want := InfoFromPersistedBead(reprojectBead(base, patch))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("base=%s patch=%v: ApplyPatch diverged from full reprojection\n got=%+v\nwant=%+v", base.ID, patch, got, want)
			}
		}
	}
}

// TestPendingCreateClaimMetadataIsVerbatim pins the contract of the raw mirror
// added for the wakeTargets-loop trace reads: PendingCreateClaimMetadata keeps
// the pending_create_claim value verbatim (untrimmed, non-canonical values and
// all), while the PendingCreateClaim bool stays the trimmed=="true" verdict.
// Both projection paths (InfoFromPersistedBead and ApplyPatch) must agree.
func TestPendingCreateClaimMetadataIsVerbatim(t *testing.T) {
	cases := []struct {
		raw      string
		wantMeta string
		wantBool bool
	}{
		{"true", "true", true},
		{" true ", " true ", true},
		{"yes", "yes", false},
		{"", "", false},
	}
	for _, tc := range cases {
		b := beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: map[string]string{"pending_create_claim": tc.raw}}
		fromBead := InfoFromPersistedBead(b)
		fromPatch := InfoFromPersistedBead(beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: map[string]string{}}).
			ApplyPatch(MetadataPatch{"pending_create_claim": tc.raw})
		for name, got := range map[string]Info{"InfoFromPersistedBead": fromBead, "ApplyPatch": fromPatch} {
			if got.PendingCreateClaimMetadata != tc.wantMeta {
				t.Errorf("%s(%q): PendingCreateClaimMetadata = %q, want %q", name, tc.raw, got.PendingCreateClaimMetadata, tc.wantMeta)
			}
			if got.PendingCreateClaim != tc.wantBool {
				t.Errorf("%s(%q): PendingCreateClaim = %v, want %v", name, tc.raw, got.PendingCreateClaim, tc.wantBool)
			}
		}
	}
}

// TestDependencyOnlyMetadataIsVerbatim pins the raw dependency_only mirror: the
// pin-awake wake-reason display path (cmd/gc) compares dependency_only untrimmed
// (== "true"), so DependencyOnlyMetadata must carry the value verbatim while the
// DependencyOnly bool stays the trimmed=="true" verdict. Both projection paths
// (InfoFromPersistedBead and ApplyPatch) must agree. Mirrors
// TestPendingCreateClaimMetadataIsVerbatim.
func TestDependencyOnlyMetadataIsVerbatim(t *testing.T) {
	cases := []struct {
		raw      string
		wantMeta string
		wantBool bool
	}{
		{"true", "true", true},
		{" true ", " true ", true},
		{"yes", "yes", false},
		{"", "", false},
	}
	for _, tc := range cases {
		b := beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: map[string]string{"dependency_only": tc.raw}}
		fromBead := InfoFromPersistedBead(b)
		fromPatch := InfoFromPersistedBead(beads.Bead{ID: "s", Type: "gc:session", Status: "open", Labels: []string{"gc:session"}, Metadata: map[string]string{}}).
			ApplyPatch(MetadataPatch{"dependency_only": tc.raw})
		for name, got := range map[string]Info{"InfoFromPersistedBead": fromBead, "ApplyPatch": fromPatch} {
			if got.DependencyOnlyMetadata != tc.wantMeta {
				t.Errorf("%s(%q): DependencyOnlyMetadata = %q, want %q", name, tc.raw, got.DependencyOnlyMetadata, tc.wantMeta)
			}
			if got.DependencyOnly != tc.wantBool {
				t.Errorf("%s(%q): DependencyOnly = %v, want %v", name, tc.raw, got.DependencyOnly, tc.wantBool)
			}
		}
	}
}

// TestInfoMarkClosedMatchesReprojection is the byte-identity oracle for the
// status-close refresh: for every base bead forced open, folding a status close
// onto the projected Info via MarkClosed must equal projecting the same bead
// with Status "closed" from scratch. This guards MarkClosed against drift from
// the Status-derived facts in InfoFromPersistedBead (Closed set true, State
// blanked) — the counterpart to TestInfoApplyPatchMatchesReprojection for the
// close half of the Step-6d write-returns-Info snapshot refresh.
func TestInfoMarkClosedMatchesReprojection(t *testing.T) {
	for _, base := range oracleBaseBeads() {
		open := base
		open.Status = "open" // force open so MarkClosed has a status change to fold

		closed := open
		closed.Status = "closed"

		got := InfoFromPersistedBead(open).MarkClosed()
		want := InfoFromPersistedBead(closed)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("base=%s: MarkClosed diverged from full reprojection of the closed bead\n got=%+v\nwant=%+v", base.ID, got, want)
		}

		// Idempotent: MarkClosed on an already-closed projection is a no-op (State
		// is already blanked and Closed already true), so re-marking a closed
		// session's snapshot entry never diverges.
		if again := want.MarkClosed(); !reflect.DeepEqual(again, want) {
			t.Errorf("base=%s: MarkClosed not idempotent on a closed projection\n got=%+v\nwant=%+v", base.ID, again, want)
		}
	}
}

// TestInfoApplyPatchDoesNotMutateReceiver guards that ApplyPatch's value
// receiver never mutates the caller's Info (its slices in particular): the
// reconciler reuses the snapshot Info across reads within a tick.
func TestInfoApplyPatchDoesNotMutateReceiver(t *testing.T) {
	base := oracleBaseBeads()[0]
	before := InfoFromPersistedBead(base)
	snapshot := InfoFromPersistedBead(base) // independent copy to compare against
	_ = before.ApplyPatch(MetadataPatch{
		aliasHistoryMetadataKey: "brand,new,history",
		"state":                 "idle",
		"session_name":          "changed",
	})
	if !reflect.DeepEqual(before, snapshot) {
		t.Fatalf("ApplyPatch mutated its receiver\n after=%+v\nbefore=%+v", before, snapshot)
	}
}

// TestInfoApplyPatchEmptyIsIdentity guards the no-op fast path shape: an empty
// patch returns the Info unchanged.
func TestInfoApplyPatchEmptyIsIdentity(t *testing.T) {
	info := InfoFromPersistedBead(oracleBaseBeads()[0])
	if got := info.ApplyPatch(MetadataPatch{}); !reflect.DeepEqual(got, info) {
		t.Fatalf("empty patch changed Info\n got=%+v\nwant=%+v", got, info)
	}
}
