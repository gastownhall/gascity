package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
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
		info := sessiontest.SeedBead(t, sb)
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

// coreConfigHashGolden is the captured golden for TestSessionCoreConfigForHashInfoGolden.
var coreConfigHashGolden = map[string]string{"empty/ga-bare": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "empty/ga-effort-override": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "empty/ga-named": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "empty/ga-named-fallback": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "empty/ga-noname": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "empty/ga-pool": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker-cmd/ga-bare": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-cmd/ga-effort-override": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-cmd/ga-named": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-cmd/ga-named-fallback": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-cmd/ga-noname": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-cmd/ga-pool": "v4:fc83c0f3d669dfb8c48ddad730f0d62fef9c9a4d9094db93be8c0bef11c3ba4b", "worker-provider/ga-bare": "v4:ac80250a8849174aa18812eeb671ac92720a4b981015873d9405d3e348f72da1", "worker-provider/ga-effort-override": "v4:f4922fa899cca0571515e4568d09101f08aa5ae3b737a050ded89bb0b56ca11f", "worker-provider/ga-named": "v4:ac80250a8849174aa18812eeb671ac92720a4b981015873d9405d3e348f72da1", "worker-provider/ga-named-fallback": "v4:ac80250a8849174aa18812eeb671ac92720a4b981015873d9405d3e348f72da1", "worker-provider/ga-noname": "v4:ac80250a8849174aa18812eeb671ac92720a4b981015873d9405d3e348f72da1", "worker-provider/ga-pool": "v4:ac80250a8849174aa18812eeb671ac92720a4b981015873d9405d3e348f72da1", "worker/ga-bare": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker/ga-effort-override": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker/ga-named": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker/ga-named-fallback": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker/ga-noname": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34", "worker/ga-pool": "v4:26a75e3704c256abbb0719e6274cd69ab5953792c0d08d1ecf4eda085849bc34"}

// TestSessionCoreConfigForHashInfoGolden is the DEDICATED pin for
// sessionCoreConfigForHashInfo — the config-drift core-hash input builder. Its retired
// raw-vs-Info equivalence oracle was self-consistent (the raw form was a thin projection
// wrapper), so this replaces it with a CoreFingerprint golden over the corpus × three
// TemplateParams shapes, INCLUDING a template_overrides shape that exercises the Info
// override-application branch. A silent change to how the core config is assembled (or to
// applyTemplateOverridesToConfigInfo) repartitions drift keys fleet-wide; perturbing any
// hashed field changes a fingerprint and fails this golden.
func TestSessionCoreConfigForHashInfoGolden(t *testing.T) {
	shapes := oracleSessionBeadShapes()
	// A shape carrying template_overrides that resolve against the provider schema below,
	// so the Info override-application branch (not just the pass-through tp fields)
	// participates in the hash: under the worker-provider tp its --effort flag flips
	// low→high, producing a DISTINCT fingerprint from the non-override shapes.
	shapes = append(shapes, beads.Bead{
		ID: "ga-effort-override", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
		Metadata: map[string]string{"template": "worker", "template_overrides": `{"effort":"high"}`},
	})
	effortProvider := &config.ResolvedProvider{
		OptionsSchema: []config.ProviderOption{{
			Key:  "effort",
			Type: "select",
			Choices: []config.OptionChoice{
				{Value: "low", FlagArgs: []string{"--effort", "low"}},
				{Value: "high", FlagArgs: []string{"--effort", "high"}},
			},
		}},
		EffectiveDefaults: map[string]string{"effort": "low"},
	}
	tps := []struct {
		name string
		tp   TemplateParams
	}{
		{"empty", TemplateParams{}},
		{"worker", TemplateParams{TemplateName: "worker"}},
		{"worker-cmd", TemplateParams{TemplateName: "worker", Command: "claude --model x"}},
		{"worker-provider", TemplateParams{TemplateName: "worker", Command: "agent --effort low", ResolvedProvider: effortProvider}},
	}

	got := map[string]string{}
	for _, tc := range tps {
		for _, sb := range shapes {
			info := sessiontest.SeedBead(t, sb)
			got[tc.name+"/"+sb.ID] = runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tc.tp, info))
		}
	}
	// The override path must actually fire: under worker-provider the override shape
	// differs from a non-override shape (guards against a vacuous, override-inert golden).
	if got["worker-provider/ga-effort-override"] == got["worker-provider/ga-bare"] {
		t.Fatal("template_overrides did not affect the core hash; the override branch is not exercised")
	}
	if len(coreConfigHashGolden) == 0 || !reflect.DeepEqual(got, coreConfigHashGolden) {
		t.Errorf("core-config hash characterization drift; got=%#v", got)
	}
}

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
		info := sessiontest.SeedBead(t, sb)
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
		info := sessiontest.SeedBead(t, sb)
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
