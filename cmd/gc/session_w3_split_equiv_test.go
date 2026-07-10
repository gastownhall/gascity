package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// rawOpenSessionReachableStoreRefRef reimplements the pre-migration
// openSessionReachableStoreRef against the raw bead (via the still-live raw
// sessionAgentConfig), as ground truth for the Info form. Only the
// sessionAgentConfig -> sessionAgentConfigInfo swap differs between the two; the
// cross-store / store-ref resolution is identical, so byte-identity here is the
// per-parameter split's proof.
func rawOpenSessionReachableStoreRefRef(cityPath string, cfg *config.City, sb beads.Bead) string {
	agentCfg := sessionAgentConfig(cfg, sb)
	if agentCfg == nil {
		return unresolvedOpenSessionStoreRef
	}
	if agentIsCrossStoreEligible(agentCfg) {
		return crossStoreOpenSessionStoreRef
	}
	return assignedWorkStoreRefForAgent(cityPath, cfg, agentCfg)
}

// TestOpenSessionReachableStoreRefInfoMatchesRaw pins the §4 split site the
// red-team flagged: openSessionReachableStoreRefInfo must equal the raw
// resolution across every session-bead shape (resolved-scoped + unresolved arms).
func TestOpenSessionReachableStoreRefInfoMatchesRaw(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "mayor"}}}
	for _, sb := range oracleSessionBeadShapes() {
		info := session.InfoFromPersistedBead(sb)
		if got, want := openSessionReachableStoreRefInfo("", cfg, info), rawOpenSessionReachableStoreRefRef("", cfg, sb); got != want {
			t.Errorf("openSessionReachableStoreRef(%s): info=%q raw=%q", sb.ID, got, want)
		}
	}
}

// WI-5 W3 per-parameter-split oracles. These pin the Info forms of the
// mixed work/session helpers (spec §7): the SESSION parameter reads typed
// session.Info while the WORK bead slice / request stay raw. Each Info form
// must be byte-identical to reading the raw session bead.

// oracleSessionBeadShapes returns representative session beads covering the
// field regions the W3 session-side splits read: bare, pool-managed with a
// session_name, a named session with a configured identity, and one carrying a
// work_dir. Byte-identity must hold across every shape.
func oracleSessionBeadShapes() []beads.Bead {
	mk := func(id string, m map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: m}
	}
	return []beads.Bead{
		mk("ga-bare", map[string]string{"template": "worker"}),
		mk("ga-pool", map[string]string{
			"template": "worker", "session_name": "worker-ga-pool",
			"pool_managed": "true", "pool_slot": "1", "work_dir": "/w/pool",
		}),
		mk("ga-named", map[string]string{
			"template": "mayor", "configured_named_session": "true",
			"configured_named_identity": "mayor", "alias": "mayor",
			"session_name": "mayor", "alias_history": "mayor,boss",
		}),
		mk("ga-named-fallback", map[string]string{
			"template": "mayor", "configured_named_session": "true",
			"session_name": "mayor",
		}),
		mk("ga-noname", map[string]string{"template": "worker", "work_dir": "/w/x"}),
	}
}

// assignedWorkGolden is the captured golden for TestSessionBeadHasAssignedWorkInfo.
var assignedWorkGolden = map[string]bool{"ga-bare": false, "ga-named": true, "ga-named-fallback": true, "ga-noname": false, "ga-pool": true}

// TestSessionBeadHasAssignedWorkInfo characterizes the session-side split of the
// assigned-work check over a fixed work set and every session-bead shape, pinned
// against a golden. It replaced the raw-vs-Info equivalence oracle (the raw form
// sessionBeadHasAssignedWork retired with the snapshot raw half in WI-7 W-delete). A
// mutation of the Info form's identity/name/id matching flips a golden entry and fails.
func TestSessionBeadHasAssignedWorkInfo(t *testing.T) {
	work := []beads.Bead{
		{ID: "wb-open-id", Status: "open", Assignee: "ga-pool"},
		{ID: "wb-name", Status: "in_progress", Assignee: "worker-ga-pool"},
		{ID: "wb-ident", Status: "open", Assignee: "mayor"},
		{ID: "wb-closed", Status: "closed", Assignee: "ga-pool"},
		{ID: "wb-blank", Status: "open", Assignee: ""},
		{ID: "wb-unmatched", Status: "in_progress", Assignee: "nobody"},
	}
	got := map[string]bool{}
	for _, sb := range oracleSessionBeadShapes() {
		info := session.InfoFromPersistedBead(sb)
		got[sb.ID] = sessionBeadHasAssignedWorkInfo(work, info)
		// The empty work set is false for every shape (guards the has-work path is
		// gated on the work set, not the session alone).
		if sessionBeadHasAssignedWorkInfo(nil, info) {
			t.Errorf("sessionBeadHasAssignedWorkInfo(nil, %s) = true, want false", sb.ID)
		}
	}
	if len(assignedWorkGolden) == 0 || !reflect.DeepEqual(got, assignedWorkGolden) {
		t.Errorf("assigned-work characterization drift; got=%#v", got)
	}
}

// rawPoolTriggerBindingPatchRef is an independent reimplementation of the
// trigger/pack/workspace/work-dir key-diff that bindPoolSessionTriggerBead
// computed inline against sessionBead.Metadata, kept in the test as the ground
// truth computePoolTriggerBindingPatch must match. It reads the RAW bead
// metadata directly so the oracle proves the Info projection is byte-identical.
func rawPoolTriggerBindingPatchRef(sb beads.Bead, request SessionRequest, workDir string) session.MetadataPatch {
	workBeadID := strings.TrimSpace(request.WorkBeadID)
	metadata := session.MetadataPatch{}
	if workBeadID == "" {
		if strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadIDMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadIDMetadataKey] = ""
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
			metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.BrainParentSIDMetadataKey]) != "" {
			metadata[beadmeta.BrainParentSIDMetadataKey] = ""
		}
		return metadata
	}
	oldWorkBeadID := strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if oldWorkBeadID != workBeadID {
		metadata[beadmeta.TriggerBeadIDMetadataKey] = workBeadID
		newParentSID := strings.TrimSpace(request.BrainParentSID)
		if strings.TrimSpace(sb.Metadata[beadmeta.BrainParentSIDMetadataKey]) != newParentSID {
			metadata[beadmeta.BrainParentSIDMetadataKey] = newParentSID
		}
	}
	workStoreRef := strings.TrimSpace(request.WorkStoreRef)
	if workStoreRef != "" && strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != workStoreRef {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = workStoreRef
	} else if workStoreRef == "" && oldWorkBeadID != workBeadID && strings.TrimSpace(sb.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]) != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ""
	}
	if pack := strings.TrimSpace(request.WorkPack); strings.TrimSpace(sb.Metadata[beadmeta.PackMetadataKey]) != pack {
		metadata[beadmeta.PackMetadataKey] = pack
	}
	if workspace := packWorkspaceSlug(request); strings.TrimSpace(sb.Metadata[beadmeta.PackWorkspaceMetadataKey]) != workspace {
		metadata[beadmeta.PackWorkspaceMetadataKey] = workspace
	}
	if workDir != "" {
		if strings.TrimSpace(sb.Metadata[beadmeta.WorkDirMetadataKey]) != workDir {
			metadata[beadmeta.WorkDirMetadataKey] = workDir
		}
		if strings.TrimSpace(sb.Metadata[beadmeta.LegacyWorkDirMetadataKey]) != workDir {
			metadata[beadmeta.LegacyWorkDirMetadataKey] = workDir
		}
	}
	return metadata
}

// TestComputePoolTriggerBindingPatchMatchesRaw pins the extracted pure diff
// against the independent raw reference across the clear, reassign, store-ref,
// pack, workspace, and work-dir request shapes, on both a bare session bead and
// one already carrying a full trigger cluster.
func TestComputePoolTriggerBindingPatchMatchesRaw(t *testing.T) {
	bases := map[string]beads.Bead{
		"bare": {ID: "s-bare", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{}},
		"full": {ID: "s-full", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:       "wb-old",
			beadmeta.TriggerBeadStoreRefMetadataKey: "rig-old",
			beadmeta.BrainParentSIDMetadataKey:      "brain-old",
			beadmeta.PackMetadataKey:                "pack-old",
			beadmeta.PackWorkspaceMetadataKey:       "ws-old",
			beadmeta.WorkDirMetadataKey:             "/gc/old",
			beadmeta.LegacyWorkDirMetadataKey:       "/old",
		}},
	}
	requests := map[string]SessionRequest{
		"clear":             {WorkBeadID: ""},
		"reassign-same":     {WorkBeadID: "wb-old"},
		"reassign-diff":     {WorkBeadID: "wb-new", BrainParentSID: "brain-new"},
		"reassign-noparent": {WorkBeadID: "wb-new"},
		"store-ref":         {WorkBeadID: "wb-new", WorkStoreRef: "rig-new"},
		"pack":              {WorkBeadID: "wb-new", WorkPack: "pack-new"},
		"workspace":         {WorkBeadID: "wb-new", WorkPack: "pack-new", WorkWorkspace: "ws-new"},
	}
	workDirs := []string{"", "/gc/old", "/gc/new"}
	for bn, sb := range bases {
		info := session.InfoFromPersistedBead(sb)
		for rn, req := range requests {
			for _, wd := range workDirs {
				got := computePoolTriggerBindingPatch(info, req, wd)
				want := rawPoolTriggerBindingPatchRef(sb, req, wd)
				if !reflect.DeepEqual(map[string]string(got), map[string]string(want)) {
					t.Errorf("base=%s req=%s workDir=%q: got=%v want=%v", bn, rn, wd, got, want)
				}
			}
		}
	}
}
