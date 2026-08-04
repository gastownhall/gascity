package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// updateGolden regenerates the workquery golden fixtures when set.
var updateGolden = flag.Bool("update", false, "update workquery golden files")

// This file freezes the behavior of the seven private Effective*Query
// resolvers as they existed before S04b's table-driven refactor. The
// oldEffective* functions are verbatim copies of the pre-refactor private
// method bodies (override check + poolDemandTarget + build-script dance).
// TestEffectiveQueryParity asserts that every exported Effective*Query and
// Effective*QueryForBeads accessor produces byte-identical output versus its
// frozen oracle for a matrix of agent shapes and both flag values. When the
// oracle copies are eventually retired, TestWorkQueryGolden below remains as
// the permanent byte-identity pin.

func oldEffectiveWorkQuery(a *Agent, includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		script := standardAssignedWorkQueryScript(includeEphemeralReady) +
			poolDemandOriginGateScript() +
			poolDemandFirstRowFunctionScript(includeEphemeralReady) +
			`probe_pool_demand "$1"; ` +
			`printf "[]"`
		return shellquote.Join([]string{"sh", "-c", script, "--", target})
	}
	script := legacyControlAssignedWorkQueryScript(includeEphemeralReady) +
		poolDemandOriginGateScript() +
		poolDemandFirstRowFunctionScript(includeEphemeralReady) +
		`probe_pool_demand "$1"; ` +
		`probe_pool_demand "$2"; ` +
		`printf "[]"`
	return shellquote.Join([]string{"sh", "-c", script, "--", target, legacyTarget})
}

func oldEffectiveAssignedInProgressQuery(a *Agent, includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

func oldEffectiveAssignedReadyQuery(a *Agent, includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

func oldEffectiveRoutedPoolQuery(a *Agent, includeEphemeralReady bool) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return routedPoolWorkQueryCommand(includeEphemeralReady, target)
	}
	return routedPoolWorkQueryCommand(includeEphemeralReady, target, legacyTarget)
}

func oldEffectivePoolDemandQuery(a *Agent, includeEphemeralReady bool) string {
	if a.ScaleCheck != "" {
		return a.ScaleCheck
	}
	target := a.poolDemandTarget()
	return poolDemandCountShell(target, includeEphemeralReady)
}

func oldEffectiveOnDeath(a *Agent, includeEphemeralInProgress bool) string {
	if a.OnDeath != "" {
		return a.OnDeath
	}
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	_ = includeEphemeralInProgress
	ephemeralRead := bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq -r --arg assignee ` + shellquote.Quote(a.QualifiedName()) + ` '.[] | select((.assignee // "") == $assignee) | [.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv' 2>/dev/null; `
	return `{ ` +
		`bd list --assignee=` + a.QualifiedName() +
		` --status=in_progress --json 2>/dev/null | ` +
		`jq -r '.[] | [.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv' 2>/dev/null; ` +
		ephemeralRead +
		`} | ` +
		`while IFS="$(printf '\t')" read -r id run_target routed_to; do ` +
		`[ -z "$id" ] && continue; ` +
		`if [ -n "$run_target" ] || [ -n "$routed_to" ]; then ` +
		`if ! err=$(bd update "$id" --assignee "" --status open 2>&1 >/dev/null); then printf 'gc-recovery: on_death release failed for %s: %s\n' "$id" "$err"; fi; ` +
		`else if ! err=$(bd update "$id" --assignee "" --status open --set-metadata ` + shellquote.Quote(beadmeta.RunTargetMetadataKey+"="+route) + ` 2>&1 >/dev/null); then printf 'gc-recovery: on_death release failed for %s: %s\n' "$id" "$err"; fi; ` +
		`fi; ` +
		`done`
}

func oldEffectiveOnBoot(a *Agent, includeEphemeralInProgress bool) string {
	if a.OnBoot != "" {
		return a.OnBoot
	}
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	_ = includeEphemeralInProgress
	ephemeralRead := bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq -r --arg template "$template" '.[] | select((.assignee // "") == "") | select((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == $template) or ((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") and (` + jqMeta(beadmeta.RunTargetMetadataKey) + ` == $template) and (` + jqMeta(beadmeta.KindMetadataKey) + ` == "` + beadmeta.KindWorkflow + `"))) | .id' 2>/dev/null; `
	return `template=` + shellquote.Quote(template) + `; ` +
		`{ ` +
		`bd list --metadata-field "` + beadmeta.RoutedToMetadataKey + `=$template" --status=in_progress --no-assignee --json 2>/dev/null | ` +
		`jq -r '.[].id' 2>/dev/null; ` +
		`bd list --metadata-field "` + beadmeta.RunTargetMetadataKey + `=$template" --metadata-field "` + beadmeta.KindMetadataKey + `=` + beadmeta.KindWorkflow + `" --status=in_progress --no-assignee --json 2>/dev/null | ` +
		`jq -r '.[] | select(` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") | .id' 2>/dev/null; ` +
		ephemeralRead +
		`} | awk 'NF && !seen[$0]++' | ` +
		`xargs -rI{} sh -c 'if ! err=$(bd update "$1" --status open 2>&1 >/dev/null); then printf "gc-recovery: on_boot reopen failed for %s: %s\n" "$1" "$err"; fi' _ {}`
}

// parityVariant binds an exported query kind's accessors to its frozen oracle.
type parityVariant struct {
	name     string
	plain    func(*Agent) string
	forBeads func(*Agent, BeadsConfig) string
	old      func(*Agent, bool) string
}

func parityVariants() []parityVariant {
	return []parityVariant{
		{"Work", (*Agent).EffectiveWorkQuery, (*Agent).EffectiveWorkQueryForBeads, oldEffectiveWorkQuery},
		{"AssignedInProgress", (*Agent).EffectiveAssignedInProgressQuery, (*Agent).EffectiveAssignedInProgressQueryForBeads, oldEffectiveAssignedInProgressQuery},
		{"AssignedReady", (*Agent).EffectiveAssignedReadyQuery, (*Agent).EffectiveAssignedReadyQueryForBeads, oldEffectiveAssignedReadyQuery},
		{"RoutedPool", (*Agent).EffectiveRoutedPoolQuery, (*Agent).EffectiveRoutedPoolQueryForBeads, oldEffectiveRoutedPoolQuery},
		{"PoolDemand", (*Agent).EffectivePoolDemandQuery, (*Agent).EffectivePoolDemandQueryForBeads, oldEffectivePoolDemandQuery},
		{"OnDeath", (*Agent).EffectiveOnDeath, (*Agent).EffectiveOnDeathForBeads, oldEffectiveOnDeath},
		{"OnBoot", (*Agent).EffectiveOnBoot, (*Agent).EffectiveOnBootForBeads, oldEffectiveOnBoot},
	}
}

type parityShape struct {
	name  string
	agent *Agent
}

func parityAgentShapes() []parityShape {
	return []parityShape{
		{"plain", &Agent{Name: "worker"}},
		{"pool", &Agent{Name: "worker", PoolName: "worker-pool"}},
		{"legacyBare", &Agent{Name: ControlDispatcherAgentName}},
		{"legacyPrefixed", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
		{"overrideWorkQuery", &Agent{Name: "worker", WorkQuery: "custom-work"}},
		{"overrideScaleCheck", &Agent{Name: "worker", ScaleCheck: "custom-scale"}},
		{"overrideOnDeath", &Agent{Name: "worker", OnDeath: "custom-death"}},
		{"overrideOnBoot", &Agent{Name: "worker", OnBoot: "custom-boot"}},
		{"overrideWorkQueryEmptyScaleCheck", &Agent{Name: "worker", WorkQuery: "", ScaleCheck: ""}},
	}
}

func TestEffectiveQueryParity(t *testing.T) {
	bd104 := BeadsConfig{}
	bd105 := BeadsConfig{BDCompatibility: BeadsBDCompatibility105}
	if bd104.UsesBD105ReadySemantics() {
		t.Fatal("bd104 stub unexpectedly reports BD105 ready semantics")
	}
	if !bd105.UsesBD105ReadySemantics() {
		t.Fatal("bd105 stub must report BD105 ready semantics")
	}

	for _, shape := range parityAgentShapes() {
		for _, v := range parityVariants() {
			shape, v := shape, v
			t.Run(shape.name+"/"+v.name, func(t *testing.T) {
				if got, want := v.plain(shape.agent), v.old(shape.agent, false); got != want {
					t.Fatalf("plain mismatch\n got=%q\nwant=%q", got, want)
				}
				if got, want := v.forBeads(shape.agent, bd104), v.old(shape.agent, false); got != want {
					t.Fatalf("forBeads(bd104) mismatch\n got=%q\nwant=%q", got, want)
				}
				if got, want := v.forBeads(shape.agent, bd105), v.old(shape.agent, true); got != want {
					t.Fatalf("forBeads(bd105) mismatch\n got=%q\nwant=%q", got, want)
				}
			})
		}
	}
}

// TestQueryTableCoversAllKinds guards against a queryKind added to the enum
// but not the table: a missing row would panic via a nil spec.override at
// runtime. Every declared kind must have both funcs set.
func TestQueryTableCoversAllKinds(t *testing.T) {
	kinds := []queryKind{
		queryWork, queryAssignedInProgress, queryAssignedReady,
		queryRoutedPool, queryPoolDemand, queryOnDeath, queryOnBoot,
	}
	if len(queryTable) != len(kinds) {
		t.Fatalf("queryTable has %d rows, expected %d kinds", len(queryTable), len(kinds))
	}
	for _, k := range kinds {
		spec, ok := queryTable[k]
		if !ok {
			t.Errorf("queryKind %d missing from queryTable", k)
			continue
		}
		if spec.override == nil {
			t.Errorf("queryKind %d has nil override", k)
		}
		if spec.build == nil {
			t.Errorf("queryKind %d has nil build", k)
		}
	}
}

// TestOnDeathOnBootFlagBlind pins invariant I6: OnDeath/OnBoot ignore the
// includeEphemeral flag, so their ForBeads variant equals the plain variant.
func TestOnDeathOnBootFlagBlind(t *testing.T) {
	bd105 := BeadsConfig{BDCompatibility: BeadsBDCompatibility105}
	a := &Agent{Name: "worker"}
	if a.EffectiveOnDeathForBeads(bd105) != a.EffectiveOnDeath() {
		t.Error("EffectiveOnDeathForBeads must equal EffectiveOnDeath (flag-blind)")
	}
	if a.EffectiveOnBootForBeads(bd105) != a.EffectiveOnBoot() {
		t.Error("EffectiveOnBootForBeads must equal EffectiveOnBoot (flag-blind)")
	}
}

// TestWorkQueryGolden pins the literal generated shell per kind × flag ×
// {normal, pool, legacy-control} so accidental script drift shows up as
// golden churn in the diff. Run with -update to regenerate.
func TestWorkQueryGolden(t *testing.T) {
	shapes := []parityShape{
		{"normal", &Agent{Name: "worker"}},
		{"pool", &Agent{Name: "worker", PoolName: "worker-pool"}},
		{"legacy", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
	}
	for _, shape := range shapes {
		for _, v := range parityVariants() {
			for _, flag := range []struct {
				name  string
				beads BeadsConfig
			}{
				{"bd104", BeadsConfig{}},
				{"bd105", BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
			} {
				got := v.forBeads(shape.agent, flag.beads)
				name := shape.name + "_" + v.name + "_" + flag.name + ".golden"
				path := filepath.Join("testdata", "workquery", name)
				if *updateGolden {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden %s: %v (run with -update to create)", name, err)
				}
				if got != string(want) {
					t.Errorf("golden mismatch for %s\n got=%q\nwant=%q", name, got, string(want))
				}
			}
		}
	}
}
