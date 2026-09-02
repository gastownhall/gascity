package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
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

func oldEffectiveWorkQuery(a *Agent, topo QueryTopology) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		script := standardAssignedWorkQueryScript(topo) +
			poolDemandOriginGateScript() +
			poolDemandFirstRowFunctionScript(topo) +
			`probe_pool_demand "$1"; ` +
			`printf "[]"`
		return shellquote.Join([]string{"sh", "-c", script, "--", target})
	}
	script := legacyControlAssignedWorkQueryScript(topo) +
		poolDemandOriginGateScript() +
		poolDemandFirstRowFunctionScript(topo) +
		`probe_pool_demand "$1"; ` +
		`probe_pool_demand "$2"; ` +
		`printf "[]"`
	return shellquote.Join([]string{"sh", "-c", script, "--", target, legacyTarget})
}

func oldEffectiveAssignedInProgressQuery(a *Agent, topo QueryTopology) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedInProgressWorkQueryScript(topo) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedInProgressWorkQueryScript(topo) + `printf "[]"`})
}

func oldEffectiveAssignedReadyQuery(a *Agent, topo QueryTopology) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedReadyWorkQueryScript(topo) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedReadyWorkQueryScript(topo) + `printf "[]"`})
}

func oldEffectiveRoutedPoolQuery(a *Agent, topo QueryTopology) string {
	if a.WorkQuery != "" {
		return a.WorkQuery
	}
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return routedPoolWorkQueryCommand(topo, target)
	}
	return routedPoolWorkQueryCommand(topo, target, legacyTarget)
}

func oldEffectivePoolDemandQuery(a *Agent, topo QueryTopology) string {
	if a.ScaleCheck != "" {
		return a.ScaleCheck
	}
	target := a.poolDemandTarget()
	return poolDemandCountShell(target, topo)
}

func oldEffectiveOnDeath(a *Agent, topo QueryTopology) string {
	if a.OnDeath != "" {
		return a.OnDeath
	}
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	_ = topo
	ephemeralRead := bdQueryEphemeralInProgressQuietShell() + ` | ` +
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

func oldEffectiveOnBoot(a *Agent, topo QueryTopology) string {
	if a.OnBoot != "" {
		return a.OnBoot
	}
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	_ = topo
	ephemeralRead := bdQueryEphemeralInProgressQuietShell() + ` | ` +
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
	name string
	// federates reports whether this kind's script contains a read the
	// federation swap covers. Four are the `bd ready` call sites; the fifth is
	// the crash-recovery tier's `bd list --status in_progress`, which the
	// residency fix moved onto `gc ready --status in_progress` so a session can
	// see its OWN claim when that claim lives in a relocated class binding. The
	// rest are `bd query`/`bd update` and stay topology-blind
	// (TestQueryKindsWithoutAReadyReadAreTopologyBlind).
	federates bool
	plain     func(*Agent) string
	forTopo   func(*Agent, QueryTopology) string
	old       func(*Agent, QueryTopology) string
	// override returns the user-supplied command that replaces this kind's
	// generated script entirely, or "" when the default applies.
	override func(*Agent) string
}

func workQueryOverride(a *Agent) string  { return a.WorkQuery }
func scaleCheckOverride(a *Agent) string { return a.ScaleCheck }
func onDeathOverride(a *Agent) string    { return a.OnDeath }
func onBootOverride(a *Agent) string     { return a.OnBoot }

func parityVariants() []parityVariant {
	return []parityVariant{
		{"Work", true, (*Agent).EffectiveWorkQuery, (*Agent).EffectiveWorkQueryFor, oldEffectiveWorkQuery, workQueryOverride},
		{"AssignedInProgress", true, (*Agent).EffectiveAssignedInProgressQuery, (*Agent).EffectiveAssignedInProgressQueryFor, oldEffectiveAssignedInProgressQuery, workQueryOverride},
		{"AssignedReady", true, (*Agent).EffectiveAssignedReadyQuery, (*Agent).EffectiveAssignedReadyQueryFor, oldEffectiveAssignedReadyQuery, workQueryOverride},
		{"RoutedPool", true, (*Agent).EffectiveRoutedPoolQuery, (*Agent).EffectiveRoutedPoolQueryFor, oldEffectiveRoutedPoolQuery, workQueryOverride},
		{"PoolDemand", true, (*Agent).EffectivePoolDemandQuery, (*Agent).EffectivePoolDemandQueryFor, oldEffectivePoolDemandQuery, scaleCheckOverride},
		{"OnDeath", false, (*Agent).EffectiveOnDeath, (*Agent).EffectiveOnDeathFor, oldEffectiveOnDeath, onDeathOverride},
		{"OnBoot", false, (*Agent).EffectiveOnBoot, (*Agent).EffectiveOnBootFor, oldEffectiveOnBoot, onBootOverride},
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
				single104 := QueryTopology{Beads: bd104}
				single105 := QueryTopology{Beads: bd105}
				fed105 := QueryTopology{Beads: bd105, FederatedReady: true}
				if got, want := v.plain(shape.agent), v.old(shape.agent, single104); got != want {
					t.Fatalf("plain mismatch\n got=%q\nwant=%q", got, want)
				}
				if got, want := v.forTopo(shape.agent, single104), v.old(shape.agent, single104); got != want {
					t.Fatalf("for(bd104) mismatch\n got=%q\nwant=%q", got, want)
				}
				if got, want := v.forTopo(shape.agent, single105), v.old(shape.agent, single105); got != want {
					t.Fatalf("for(bd105) mismatch\n got=%q\nwant=%q", got, want)
				}
				if got, want := v.forTopo(shape.agent, fed105), v.old(shape.agent, fed105); got != want {
					t.Fatalf("for(bd105, federated) mismatch\n got=%q\nwant=%q", got, want)
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
// includeEphemeral flag, so their topology variant equals the plain variant.
func TestOnDeathOnBootFlagBlind(t *testing.T) {
	bd105 := BeadsConfig{BDCompatibility: BeadsBDCompatibility105}
	a := &Agent{Name: "worker"}
	if a.EffectiveOnDeathFor(QueryTopology{Beads: bd105}) != a.EffectiveOnDeath() {
		t.Error("EffectiveOnDeathFor must equal EffectiveOnDeath (flag-blind)")
	}
	if a.EffectiveOnBootFor(QueryTopology{Beads: bd105}) != a.EffectiveOnBoot() {
		t.Error("EffectiveOnBootFor must equal EffectiveOnBoot (flag-blind)")
	}
}

// TestQueryKindsWithoutAReadyReadAreTopologyBlind states, as an assertion
// rather than as prose, exactly which generated commands the federation swap
// does NOT reach — and therefore what a split city still runs single-store.
//
// The swap covers the four `bd ready` call sites. It does not cover
// AssignedInProgress (`bd list`, the crash-recovery tier), OnDeath (`bd list` +
// `bd update`) or OnBoot (the same): there is no federated form of a status list
// or a write, and building one is the claim-routing slice, ga-601v2. If a later
// change federates one of them, this test fails and the gap is re-decided
// deliberately instead of drifting shut.
func TestQueryKindsWithoutAReadyReadAreTopologyBlind(t *testing.T) {
	bd105 := BeadsConfig{BDCompatibility: BeadsBDCompatibility105}
	single := QueryTopology{Beads: bd105}
	federated := QueryTopology{Beads: bd105, FederatedReady: true}
	for _, shape := range parityAgentShapes() {
		for _, v := range parityVariants() {
			got := v.forTopo(shape.agent, federated)
			want := v.forTopo(shape.agent, single)
			// A user override is returned verbatim on every topology, by
			// contract. That blindness is reported, not rewritten — see
			// Agent.FederationBlindOverrides and
			// TestFederationBlindOverridesNamesTheBlindKeys.
			if v.override(shape.agent) != "" {
				if got != want {
					t.Errorf("%s/%s: a user override must be returned verbatim on both topologies", shape.name, v.name)
				}
				continue
			}
			if v.federates {
				if got == want {
					t.Errorf("%s/%s: the federated command is identical to the single-store one; this kind carries a ready read and must swap the reader", shape.name, v.name)
				}
				continue
			}
			if got != want {
				t.Errorf("%s/%s: federated command differs from the single-store one, but this kind carries no ready read\n got=%q\nwant=%q", shape.name, v.name, got, want)
			}
		}
	}
}

// TestFederatedSwapChangesOnlyTheReader is the mutation guard on the swap
// itself: every `bd ready` becomes `gc ready`, none is left behind, and nothing
// ELSE about the script changes. A swap that also moved a flag, dropped a hold
// label, or rewrote a `bd list` would pass the golden files (they would just be
// regenerated) but fails here.
func TestFederationBlindOverridesNamesTheBlindKeys(t *testing.T) {
	single := QueryTopology{}
	federated := QueryTopology{FederatedReady: true}
	for _, tt := range []struct {
		name  string
		agent *Agent
		want  []string
	}{
		{"no overrides", &Agent{Name: "worker"}, nil},
		{"work_query", &Agent{Name: "worker", WorkQuery: "bd ready --json"}, []string{"work_query"}},
		{"scale_check", &Agent{Name: "worker", ScaleCheck: "echo 1"}, []string{"scale_check"}},
		{"both", &Agent{Name: "worker", WorkQuery: "bd ready --json", ScaleCheck: "echo 1"}, []string{"work_query", "scale_check"}},
	} {
		if got := tt.agent.FederationBlindOverrides(single); got != nil {
			t.Errorf("%s: a single-store city reports %v; there is nothing for an override to be blind to", tt.name, got)
		}
		got := tt.agent.FederationBlindOverrides(federated)
		if len(got) != len(tt.want) {
			t.Fatalf("%s: FederationBlindOverrides = %v, want %v", tt.name, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: FederationBlindOverrides = %v, want %v", tt.name, got, tt.want)
			}
		}
	}
}

// singleQuoteEscaped rewrites a raw script fragment the way shellquote.Join
// does when it embeds the generated script as a single-quoted sh -c argument.
func singleQuoteEscaped(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}

// singleStoreReadCount counts the reads in a single-store command that the
// federation swap replaces: the `bd ready` call sites plus the crash-recovery
// tier's `bd list --status in_progress`.
func singleStoreReadCount(command string) int {
	return strings.Count(command, bdReadyCommand) +
		strings.Count(command, bdListInProgressCommand)
}

// renormalizeFederatedCommand maps a federated command back onto the
// single-store one by undoing ONLY the differences the swap is allowed to make:
// the reader words, their failure clauses, and the crash-recovery tier's
// presence-key prelude.
//
// The last one is undone by generating both forms from the production function
// itself rather than by pasting its text here. That keeps the guard honest as
// the enrichment evolves: a change INSIDE the enrichment renormalizes away (it
// is by definition part of the sanctioned difference), while any change outside
// the two generators still fails, which is the property this test exists for.
func renormalizeFederatedCommand(federated string) string {
	// The generated script is embedded in the final command by shellquote.Join,
	// which wraps it in single quotes and rewrites every inner ' as '\''. The
	// fragments below are generated raw, so they must be escaped the same way to
	// match — the enrichment carries single-quoted jq programs, so this is not
	// cosmetic.
	replaceFragment := func(in, from, to string) string {
		return strings.ReplaceAll(in, singleQuoteEscaped(from), singleQuoteEscaped(to))
	}
	federated = replaceFragment(federated,
		inProgressBlockedByEnrichmentScript(true, true),
		inProgressBlockedByEnrichmentScript(false, true))
	for _, shellVar := range []string{"id", "cand"} {
		federated = replaceFragment(federated,
			assignedInProgressTierCommand(shellVar, QueryTopology{FederatedReady: true}),
			assignedInProgressTierCommand(shellVar, QueryTopology{}))
	}
	federated = strings.ReplaceAll(federated, gcReadyCommand, bdReadyCommand)
	federated = strings.ReplaceAll(federated, `--json --limit=1) || exit $?`, `--json --limit=1 2>/dev/null)`)
	federated = strings.ReplaceAll(federated, `--sort oldest --limit=20) || exit $?`, `--sort oldest --limit=20 2>/dev/null)`)
	federated = strings.ReplaceAll(federated, `--sort oldest --limit=20 2>/dev/null) || exit $?`, `--sort oldest --limit=20 2>/dev/null)`)
	return federated
}

func TestFederatedSwapChangesOnlyTheReader(t *testing.T) {
	for _, shape := range parityAgentShapes() {
		for _, v := range parityVariants() {
			if !v.federates || v.override(shape.agent) != "" {
				continue
			}
			bd105 := BeadsConfig{BDCompatibility: BeadsBDCompatibility105}
			single := v.forTopo(shape.agent, QueryTopology{Beads: bd105})
			federated := v.forTopo(shape.agent, QueryTopology{Beads: bd105, FederatedReady: true})
			if n := singleStoreReadCount(single); n == 0 {
				t.Fatalf("%s/%s: single-store command contains no read to swap", shape.name, v.name)
			} else if got := strings.Count(federated, gcReadyCommand); got != n {
				t.Errorf("%s/%s: single-store command has %d swappable reads, federated has %d %q", shape.name, v.name, n, got, gcReadyCommand)
			}
			if strings.Contains(federated, bdReadyCommand) {
				t.Errorf("%s/%s: federated command still shells %q, so that tier stays blind on a split city: %q", shape.name, v.name, bdReadyCommand, federated)
			}
			if strings.Contains(federated, bdListInProgressCommand) {
				t.Errorf("%s/%s: federated command still shells %q, so a session stays blind to its OWN claim in a relocated binding: %q", shape.name, v.name, bdListInProgressCommand, federated)
			}
			// Everything outside the reader words, their failure handling, and the
			// crash-recovery presence key must be untouched. Normalizing the
			// federated form back onto the single-store one is what proves it.
			if renormalized := renormalizeFederatedCommand(federated); renormalized != single {
				t.Errorf("%s/%s: the federated command differs from the single-store one by more than the reader, its failure clause, and the crash-recovery presence key\n federated(normalized)=%q\n      single-store=%q", shape.name, v.name, renormalized, single)
			}
		}
	}
}

// TestWorkQueryGolden pins the literal generated shell per kind × flag ×
// {normal, pool, legacy-control} so accidental script drift shows up as
// golden churn in the diff. Run with -update to regenerate.
//
// The flag axis carries both topologies. The single-store rows are the
// deployed bytes; the _federated rows are what a city that relocates a
// coordination class runs instead, and they exist only for the kinds that
// carry a ready read.
func TestWorkQueryGolden(t *testing.T) {
	shapes := []parityShape{
		{"normal", &Agent{Name: "worker"}},
		{"pool", &Agent{Name: "worker", PoolName: "worker-pool"}},
		{"legacy", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
	}
	for _, shape := range shapes {
		for _, v := range parityVariants() {
			for _, flag := range []struct {
				name string
				topo QueryTopology
			}{
				{"bd104", QueryTopology{}},
				{"bd105", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}},
				// The federated rows are new files, and the single-store rows
				// above are deliberately left alone: a change that made a
				// non-relocated city emit anything but the command it emits today
				// reddens THOSE goldens, which is the byte-identity claim.
				{"bd104_federated", QueryTopology{FederatedReady: true}},
				{"bd105_federated", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, FederatedReady: true}},
			} {
				if strings.HasSuffix(flag.name, "_federated") && !v.federates {
					continue
				}
				got := v.forTopo(shape.agent, flag.topo)
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
