package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// QueryTopology is the pair of CITY facts a generated query has to be built
// against, as opposed to the agent facts the *Agent receiver already carries.
//
// They have different sources and neither can be derived from the other. Beads
// comes from [beads] in city.toml. FederatedReady comes from the storage routes
// the process resolved — cmd/gc asks graphClassBinding, the same question
// resolveClassStore asks to choose its branch — and a city that has served a
// split can answer yes with no [storage] section left to read.
type QueryTopology struct {
	// Beads carries the bd CLI semantics this city may rely on.
	Beads BeadsConfig
	// FederatedReady is true when the city serves a coordination class from a
	// store `bd` in the agent's work directory cannot reach. Generated queries
	// then read claimable work through the city-wide federated reader instead
	// of single-store `bd ready`, which on such a city answers with the copies
	// the migration retained and nothing else — an authoritative-looking short
	// array that a work query cannot tell apart from "no work".
	//
	// The zero value is the single-store city every deployment has today, and
	// the generated command for it is byte-for-byte the one it already runs.
	FederatedReady bool
}

// includeEphemeralReady reports whether generated ready reads may ask for the
// wisp/ephemeral tier.
func (t QueryTopology) includeEphemeralReady() bool {
	return t.Beads.UsesBD105ReadySemantics()
}

// Ready reader commands. bdReadyCommand reads exactly one store — whichever one
// `bd` resolves from the working directory — and is what every city runs today.
// gcReadyCommand is the in-process federation over every store a city spreads
// work across (cmd/gc's `gc ready`); it accepts every flag the queries in this
// file generate, so the swap is the command word and nothing else. That is a
// claim about THESE queries, not about all of `bd ready`: `gc ready` registers
// the generated subset and rejects the rest of bd's ready surface, which is why
// the operator-facing refusals that steer at it say so
// (internal/beads/bdsql_relocation.go RelocatedClassFrontierRefusal).
const (
	bdReadyCommand = "bd ready"
	gcReadyCommand = "gc ready"
	// bdListInProgressCommand is the crash-recovery tier's single-store read.
	// It is a fifth swap site alongside the four `bd ready` ones: it reads a
	// STATUS rather than the ready set, but it is just as blind to a claim
	// living in a relocated class binding, which is what made a session unable
	// to see work it already owned.
	bdListInProgressCommand = "bd list --status in_progress"
)

// readyReaderCommand returns the reader a generated query asks for claimable
// work with.
func readyReaderCommand(federated bool) string {
	if federated {
		return gcReadyCommand
	}
	return bdReadyCommand
}

// readyReaderStderrSink returns the stderr redirect a probe tier wraps its ready
// read in.
//
// The single-store tiers discard it, and that is deliberate: `bd ready` chatters
// on a store it cannot open, and the tier has a later tier to fall through to.
// The federated reader's stderr is the opposite — it is the ONE place a dead leg
// is named ("gc ready: rig \"rig-A\" store: ..."), and the whole reason the
// reader exits non-zero instead of serving a short array. Swallowing it would
// leave the operator with an exit code and no rig name.
func readyReaderStderrSink(federated bool) string {
	if federated {
		return ""
	}
	return ` 2>/dev/null`
}

// readyReaderFailurePropagation returns the failure clause a probe tier appends
// to a federated ready read.
//
// This is the load-bearing half of the swap. `gc ready` fails LOUD by design —
// a leg it could not open or read is a non-zero exit naming the rig, never a
// short array, because a short array is indistinguishable from "no work"
// (cmd/gc/ready_federation.go's header). A probe tier that captured that exit
// into an empty $r and fell through to the next tier would re-create the exact
// fail-open one layer up, and the whole federation would buy nothing: the query
// would still answer "no work" while claimable beads sat in the graph store.
//
// So the federated tiers exit with the reader's own status. `gc hook` turns a
// non-zero work query into an error (doHook / claimHookWork), and the
// reconciler's count-form already did (poolDemandCountShell's && chain), so the
// failure reaches a human instead of being spent as an idle signal.
//
// The single-store tiers keep their fall-through: `bd ready` failing in one work
// directory says nothing about the tiers after it, and changing that would not
// be byte-identical.
func readyReaderFailurePropagation(federated bool) string {
	if federated {
		return ` || exit $?`
	}
	return ""
}

// bdReadyPoolDemandShell returns the canonical bd ready predicate for
// unassigned, non-epic pool demand routed to target. gc.routed_to is the
// canonical persisted routing key: the graph.v2 stamper and the legacy stamper
// both stamp it on every routable bead, including the workflow root (ga-eld2x
// retired the short-lived gc.run_target wire field). This predicate is the main
// source of truth for "is there work on this routed queue?" that both the
// worker (via EffectiveWorkQuery Tier 3) and the reconciler (via
// EffectivePoolDemandQuery, count-form) ask; diverging the two re-introduces
// the protocol-mismatch class (see the "scale_check ↔ work_query
// correspondence" note in engdocs/architecture/dispatch.md).
//
// target is passed as a positional argument to the outer sh -c command, not
// interpolated into the nested shell body. That keeps routes containing shell
// metacharacters as data instead of executable syntax.
func bdReadyIncludeEphemeralArg(includeEphemeralReady bool) string {
	if includeEphemeralReady {
		return " --include-ephemeral"
	}
	return ""
}

// holdLabelMatchCondsJQ renders the jq boolean expression that tests whether
// the label currently in scope (`.`) is one of the beadmeta.DispatchHoldLabels
// values, e.g. `. == "hold:mayor" or . == "hold:external"`. Shared by every
// jq-based hold filter so the label set is spelled exactly once.
func holdLabelMatchCondsJQ() string {
	conds := make([]string, len(beadmeta.DispatchHoldLabels))
	for i, label := range beadmeta.DispatchHoldLabels {
		conds[i] = `. == "` + label + `"`
	}
	return strings.Join(conds, " or ")
}

// excludeHoldLabelsJQClause returns a jq select(...) clause dropping beads
// that carry any beadmeta.DispatchHoldLabels value, for jq-based pool-demand
// filters that have no bd-side --exclude-label flag to lean on. Mirrors the
// bracketed-count style of the dependency-blocking select above it so both
// clauses read the same way (ga-x9kptu / ga-5736js).
func excludeHoldLabelsJQClause() string {
	return ` | select(([ (.labels // [])[] | select(` + holdLabelMatchCondsJQ() + `) ] | length) == 0)`
}

// heldLabelCountJQ counts how many beadmeta.DispatchHoldLabels values the FIRST
// row of a work-query result carries. The in_progress serve gate reads it off
// the candidate row it already holds, so the hold check costs no extra bd call.
// `.[0].labels // []` absorbs both an absent row and bd's null-valued labels
// field; a non-JSON payload makes jq fail and the gate falls back to 0, which is
// the fail-open behavior TestInProgressTierServesUnparseableHeldCandidateFailOpen
// pins.
func heldLabelCountJQ() string {
	return `[ (.[0].labels // [])[] | select(` + holdLabelMatchCondsJQ() + `) ] | length`
}

// jqMeta renders the jq expression that reads a bead-metadata key with an
// empty-string default, e.g. (.metadata["gc.routed_to"] // ""). Shell/jq
// builders use it so embedded key spellings stay anchored to the beadmeta
// vocabulary constants.
func jqMeta(key string) string {
	return `(.metadata["` + key + `"] // "")`
}

// PoolDemandServeRules names, in Go, exactly what the generated Tier-3
// pool-demand query will and will not serve a worker for a template.
//
// It exists because the CONTROLLER has to answer the same question — "would a
// T-worker be served this row?" — before it counts the row as capacity demand,
// and it cannot run the query to find out. Two hand-maintained copies of that
// answer is how the pool came to spawn seats for rows their own hooks are
// forbidden to claim: a routed epic, or a bead parked on a dispatch hold, was
// permanent demand and every worker it spawned saw an empty query.
//
// The shell builder below renders its flags FROM this value, so the descriptor
// is not a description of the query — it is the query's source. A flag added
// there without a field here cannot exist.
type PoolDemandServeRules struct {
	// RequireUnassigned mirrors --unassigned: a row carrying any assignee is
	// not routed pool demand.
	RequireUnassigned bool
	// ExcludeTypes mirrors --exclude-type: issue types the tier never serves.
	ExcludeTypes []string
	// ExcludeLabels mirrors the repeated --exclude-label flags: the dispatch
	// holds a worker is deliberately forbidden to claim through.
	ExcludeLabels []string
}

// PoolDemandServeRulesForQuery returns the serving rules of the generated
// routed pool-demand tier. Callers that classify a row (rather than build a
// query) consume this; see the controller demand loop in cmd/gc.
func PoolDemandServeRulesForQuery() PoolDemandServeRules {
	return PoolDemandServeRules{
		RequireUnassigned: true,
		ExcludeTypes:      []string{"epic"},
		ExcludeLabels:     append([]string(nil), beadmeta.DispatchHoldLabels...),
	}
}

// ShellArgs renders the rules as the bd flag string a routed pool-demand tier
// carries. It is the single rendering path — every generated demand query, in
// this package and in the control dispatcher's own probe, is built from it, so
// the descriptor and the queries cannot drift.
func (r PoolDemandServeRules) ShellArgs() string {
	var args string
	if r.RequireUnassigned {
		args += " --unassigned"
	}
	for _, typ := range r.ExcludeTypes {
		args += " --exclude-type=" + typ
	}
	for _, label := range r.ExcludeLabels {
		args += ` --exclude-label "` + label + `"`
	}
	return args
}

func bdReadyPoolDemandShell(limitFlag string, topo QueryTopology) string {
	return readyReaderCommand(topo.FederatedReady) + bdReadyIncludeEphemeralArg(topo.includeEphemeralReady()) + ` --metadata-field "` + beadmeta.RoutedToMetadataKey + `=$target"` + PoolDemandServeRulesForQuery().ShellArgs() + ` --json ` + limitFlag
}

// bdReadyPoolDemandMigrationShell is a temporary raw compatibility probe for
// graph.v2 workflow roots created before gc.routed_to root stamping shipped.
// It is scoped to workflow roots so gc.run_target remains an authoring hint
// everywhere else. Callers must pass its output through
// poolDemandMigrationFilterJQ so a stale divergent gc.run_target cannot remain
// visible once a root carries gc.routed_to. This retirement-window fallback
// requires jq in the default worker/reconciler environment; remove it with the
// Go-side legacy candidates after the backfill completion tracked by ga-dhf44.
func bdReadyPoolDemandMigrationShell(limitFlag string, topo QueryTopology) string {
	return readyReaderCommand(topo.FederatedReady) + bdReadyIncludeEphemeralArg(topo.includeEphemeralReady()) + ` --metadata-field "` + beadmeta.RunTargetMetadataKey + `=$target" --metadata-field "` + beadmeta.KindMetadataKey + `=` + beadmeta.KindWorkflow + `"` + PoolDemandServeRulesForQuery().ShellArgs() + ` --json --sort oldest ` + limitFlag
}

func poolDemandMigrationFilterJQ(limit int) string {
	filter := `[.[] | select(` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "")]`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return shellquote.Join([]string{"jq", filter})
}

func bdQueryEphemeralStatusShell(status string) string {
	return `bd query --json ` + shellquote.Quote("ephemeral=true AND status="+status) + ` --limit=0`
}

func bdQueryEphemeralStatusQuietShell(status string) string {
	return bdQueryEphemeralStatusShell(status) + ` 2>/dev/null`
}

// ephemeralReadyBaseSelectorJQ composes the selector clauses shared by every
// ephemeral ready-tier filter: the caller's own assignee/routing selector,
// plus the epic exclusion and optional hold-label exclusion every variant
// applies alike. Dependency gating is layered on top by each caller, since
// `bd query --json` exposes only a `dependency_count` scalar — never a
// `dependencies` array — which is precise enough to prove "definitely no
// dependencies" but not to resolve whether a nonzero count is still open.
func ephemeralReadyBaseSelectorJQ(selector string, excludeHoldLabels bool) string {
	body := selector + ` | select(((.issue_type // .type // "") != "epic"))`
	if excludeHoldLabels {
		body += excludeHoldLabelsJQClause()
	}
	return body
}

// legacyEphemeralReadyFilterJQ is the fast path: it withholds any candidate
// with a nonzero dependency_count outright, since bd query --json carries no
// detail on whether those dependencies are still open. dependency_count == 0
// is a belt-and-braces prefilter, not the sole gate — ephemeralAssignedReadyProbeScript
// pairs it with ephemeralReadyDependencyCandidateFilterJQ's real bd show
// enrichment for the dependency_count > 0 case, so a step whose dependencies
// have since all closed is still reachable instead of withheld forever.
func legacyEphemeralReadyFilterJQ(selector string, limit int, excludeHoldLabels bool) string {
	body := ephemeralReadyBaseSelectorJQ(selector, excludeHoldLabels) +
		` | select(((.dependency_count // 0) == 0))`
	filter := `[.[] | ` + body + `]` + ` | sort_by(.created_at // "", .id // "")`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return filter
}

// ephemeralReadyDependencyCandidateFilterJQ is the slow-path complement to
// legacyEphemeralReadyFilterJQ: it surfaces the candidates the fast path
// withheld (dependency_count > 0) so the caller can enrich them with a real
// bd show --json call via inProgressBlockedByEnrichmentScript and decide
// readiness precisely, instead of leaving them permanently unservable.
func ephemeralReadyDependencyCandidateFilterJQ(selector string, limit int, excludeHoldLabels bool) string {
	body := ephemeralReadyBaseSelectorJQ(selector, excludeHoldLabels) +
		` | select(((.dependency_count // 0) > 0))`
	filter := `[.[] | ` + body + `]` + ` | sort_by(.created_at // "", .id // "")`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return filter
}

func legacyEphemeralPoolDemandShell(limit int, topo QueryTopology, quiet bool) string {
	if topo.includeEphemeralReady() {
		return `printf "[]"`
	}
	filter := legacyEphemeralReadyFilterJQ(
		`select((.assignee // "") == "")`+
			` | select((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == $target) or ((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == "") and (`+jqMeta(beadmeta.RunTargetMetadataKey)+` == $target) and (`+jqMeta(beadmeta.KindMetadataKey)+` == "`+beadmeta.KindWorkflow+`")))`,
		limit,
		true,
	)
	query := bdQueryEphemeralStatusShell("open")
	if quiet {
		query = bdQueryEphemeralStatusQuietShell("open")
	}
	jqStderr := ""
	if quiet {
		jqStderr = ` 2>/dev/null`
	}
	return `{ ` + query + ` | jq --arg target "$target" ` + shellquote.Quote(filter) + jqStderr + `; } || printf "[]"`
}

// poolDemandFirstRowFunctionScript emits the work_query Tier 3 function: it
// reads the first ready, unassigned, routed bead for the supplied target,
// prints it, and exits 0. The caller appends a terminal fallthrough
// (printf "[]") for the empty case.
func poolDemandFirstRowFunctionScript(topo QueryTopology) string {
	fed := topo.FederatedReady
	return `probe_pool_demand() { ` +
		`target="$1"; ` +
		`[ -z "$target" ] && return 1; ` +
		`r=$(` + routedReadyTierCommand(topo) + `)` + readyReaderFailurePropagation(fed) + `; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`legacy_candidates=$(` + bdReadyPoolDemandMigrationShell("--limit=20", topo) + readyReaderStderrSink(fed) + `)` + readyReaderFailurePropagation(fed) + `; ` +
		`r=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(1) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`legacy_ephemeral_candidates=$(` + legacyEphemeralPoolDemandShell(20, topo, true) + `); ` +
		`r=$(printf "%s" "$legacy_ephemeral_candidates" | jq '.[0:1]' 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`return 1; ` +
		`}; `
}

func routedReadyTierCommand(topo QueryTopology) string {
	// The shared predicate stays order-free so the count-form does no wasted
	// sorting; the worker first-row path rides the reader's canonical
	// (priority, created_at, id) default order — both readers default to it
	// (bd ready's default --sort priority; the federated reader's merged
	// beads.SortBeadsReadyOrder). An explicit --sort oldest here makes the
	// claim window priority-blind: a routed P0 behind more than --limit older
	// lower-priority rows is never served at all, so pool workers
	// deterministically starve the highest-priority routed work (measured on a
	// live 14-seat city, 2026-08-25: 13 P0 rows parked behind 34 older wave
	// rows — the bounded window contained zero P0s until the older rows
	// drained). FIFO fairness survives within a priority band via the
	// created_at term. The tier stays widened past a single row (limit=20, not
	// limit=1) so a self-blocked head (is_blocked / status==blocked) has Ready
	// routed work behind it to fall through to instead of idle-exiting; the
	// hook layer (filterUnreadyHookCandidates) strips the blocked head from
	// the result.
	return bdReadyPoolDemandShell("--limit=20", topo) + readyReaderStderrSink(topo.FederatedReady)
}

// poolDemandCountShell emits the reconciler count-form for target: it counts
// ready, unassigned, routed demand and prints the array length. It shares the
// canonical and migration predicates with poolDemandFirstRowFunctionScript so
// the reconciler's spawn decision and the worker's claim decision read the
// same demand shape.
//
// Unlike the work_query probe, this form must NOT redirect the reader's stderr
// or default to zero: a failed ready read has to surface as an error rather than
// masquerade as "no demand", which would silently stop the pool from spawning.
// The && chain ensures any non-zero reader exit short-circuits the whole
// expression (TestEffectiveScaleCheckUsesReadyOnly). That discipline is why this
// form needed no new failure clause when the federated reader arrived — it is
// the shape readyReaderFailurePropagation gives the worker-side tiers.
func poolDemandCountShell(target string, topo QueryTopology) string {
	script := `target="$1"; ` +
		`ready_json=$(` + bdReadyPoolDemandShell("--limit 0", topo) + `) || exit $?; ` +
		`legacy_candidates=$(` + bdReadyPoolDemandMigrationShell("--limit 0", topo) + `) || exit $?; ` +
		`legacy_json=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(0) + `) || exit $?; ` +
		`legacy_ephemeral_json=$(` + legacyEphemeralPoolDemandShell(0, topo, false) + `); ` +
		`printf "%s\n%s\n%s\n" "$ready_json" "$legacy_json" "$legacy_ephemeral_json" | jq -s "(add // []) | unique_by(.id) | length"`
	return shellquote.Join([]string{"sh", "-c", script, "--", target})
}

func (a *Agent) poolDemandTarget() string {
	target := a.QualifiedName()
	if a.PoolName != "" {
		target = a.PoolName
	}
	return target
}

func standardAssignedWorkQueryScript(topo QueryTopology) string {
	return standardAssignedInProgressWorkQueryScript(topo) +
		standardAssignedReadyWorkQueryScript(topo)
}

// assignedInProgressTierCommand is the crash-recovery read for one identity.
//
// This tier WAS the one read the federation swap left on single-store `bd list`,
// and leaving it there is what made a session blind to its own claim. On a split
// city a graph step's claim lands in the relocated class binding
// (cmd/gc/claim_class_route.go), which no `bd` workspace leg can reach, so this
// tier came back empty on every leg for a bead the session itself owned — and
// the fresh-claim tier behind it then handed the same session ANOTHER bead. That
// is the 2-3-claims-per-session accumulation, and it is a property of the reader,
// not of the claimant.
//
// The two couplings that deferred the swap are both closed now:
//
//   - Enrichment. The row this tier returns is fed to
//     inProgressBlockedByEnrichmentScript, which resolved blockers with `bd show`
//     in the agent's work directory and so resolved nothing for a relocated id.
//     The federated reader now carries `blocked_by` computed in-process from the
//     leg that served the row (cmd/gc/cmd_ready.go readyBlockedByForRows), and
//     the shell enrichment is presence-keyed, so it steps aside for a row that
//     already carries the field and is byte-unchanged for one that does not.
//   - Claimability. "Its recovered bead would still be unclaimable" was true at
//     deferral time. Adoption of an in_progress row performs no status CAS, and
//     the claim-time metadata stamp already routes through the binding.
//     on_death/on_boot stay single-store; those remain ga-601v2's slice.
func assignedInProgressTierCommand(shellVar string, topo QueryTopology) string {
	fed := topo.FederatedReady
	reader := bdListInProgressCommand
	if fed {
		reader = gcReadyCommand + ` --status in_progress`
	}
	return `r=$(` + reader + ` --assignee="$` + shellVar + `" --json --limit=1` +
		readyReaderStderrSink(fed) + `)` + readyReaderFailurePropagation(fed) + `; `
}

// standardAssignedInProgressWorkQueryScript is the crash-recovery tier.
func standardAssignedInProgressWorkQueryScript(topo QueryTopology) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		assignedInProgressTierCommand("id", topo) +
		`if [ -n "$r" ] && [ "$r" != "[]" ]; then ` +
		inProgressBlockedByEnrichmentScript(topo.FederatedReady, true) +
		`fi; ` +
		ephemeralAssignedInProgressProbeScript("id", topo) +
		`done; `
}

// inProgressBlockedByEnrichmentScript hardens the in_progress "crash recovery"
// work-query tier against re-serving a bead that cannot progress.
//
// `bd list --status in_progress` does no readiness computation: unlike
// `bd ready` it emits neither blocked_by nor is_blocked. That makes the
// defensive hook-side filter (filterUnreadyHookCandidates ->
// isDepBlockedHookCandidate) a structural no-op for this tier, because an
// absent blocked_by is correctly read as "not blocked". A step that is
// in_progress + assigned but held by an open gate or an unclosed blocking
// dependency is therefore re-served on every hook tick, forever.
//
// `bd ready` cannot be substituted here: it excludes in_progress by design,
// so it would return nothing and defeat crash recovery entirely. Instead we
// read the candidate's own dependency rows and attach the blocked_by array
// the rest of the pipeline already knows how to interpret. When the candidate
// is blocked we skip it and fall through to the ready-gated tier, so a session
// holding one blocked step can still be served its other ready assigned work.
//
// Only ready-blocking dependency types are considered, matching
// beads.IsReadyBlockingDependencyType; parent-child and tracks edges never
// block readiness. Status interpretation is left to the shared Go filter:
// any non-closed blocker counts.
//
// When checkHold is true, the same serve gate also skips a candidate parked
// on a canonical dispatch hold (beadmeta.DispatchHoldLabels), read off the
// candidate row this tier already holds — no extra bd call (gas-kg6). A held
// bead has no blocking dependency, so the blocked_by gate alone let it
// through and the tier re-served it on every tick; because the tier
// short-circuits with `exit 0`, that also starved the ready tiers below it,
// and a worker that correctly parked its bead could never reach its own
// ready queue. The hold dimension is the same defect class as the dependency
// dimension above, so it shares the same fall-through.
//
// Scope (ga-5736js): the hold check is the WORK-SERVING decision only. The
// assignee-scoped probes that answer "does a session still need to exist" —
// ephemeralAssignedReadyProbeScript here and filterReadyByAssignee in
// cmd/gc/dispatch_control_ready.go — stay hold-transparent by design so a held
// assignment still keeps its owner visible to demand and recovery accounting.
// Those callers pass checkHold=false, which omits every `.labels` reference
// from the generated script — TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels
// pins the omission, not just the outcome.
//
// Enrichment is fail-open: a failed or unparseable `bd show` / `bd list`
// degrades to the stock behavior of serving the candidate unchanged, never to
// dropping it, so a malformed or log-prefixed bd stdout can never disable
// crash recovery. The hold count fails open the same way when checkHold is
// true.
//
// # The presence key, and why only the federated form has it
//
// On a federated topology the reader is `gc ready --status in_progress`, which
// already carries `blocked_by` resolved in-process from the leg that served the
// row. Re-deriving it here with `bd show` would resolve nothing for a relocated
// id and overwrite a correct answer with an empty one, so the federated form
// reads the field off the row first and only falls back to `bd show` when the
// row does not carry it.
//
// The single-store form is left byte-identical, deliberately. Its reader is
// `bd list`, whose rows never carry `blocked_by`, so the presence branch could
// never fire there — it would be pure added shell on the path every existing
// deployment runs, for zero behavior change. Same zero-risk scoping the ready
// tiers used for their own swap.
func inProgressBlockedByEnrichmentScript(federated bool, checkHold bool) string {
	// The tier stores its candidate row in $r and its enriched copy in
	// $r_enriched; both callers use those names.
	const shellVar = "r"
	const blockingDepsJQ = `[.[0].dependencies[]? | ` +
		`select(.dependency_type == "blocks" or .dependency_type == "waits-for" or ` +
		`.dependency_type == "conditional-blocks") | {id, status}]`
	const openBlockerCountJQ = `[.[] | select(((.status // "") | ascii_downcase) != "closed")] | length`

	const enrichJQ = `map(. + {blocked_by: $bb})`
	const carriedBlockedByJQ = `.[0].blocked_by // empty`

	v := `$` + shellVar
	// The enriched payload lands in a scratch var derived from shellVar so the
	// candidate itself is never clobbered: if jq fails (non-JSON or
	// log-prefixed `bd list` stdout) the original is served unchanged.
	enrichedVar := shellVar + `_enriched`
	e := `$` + enrichedVar
	carried := ``
	if federated {
		carried = `bb=$(printf "%s" "` + v + `" | jq -c ` + shellquote.Quote(carriedBlockedByJQ) + ` 2>/dev/null); ` +
			`[ "$bb" = "null" ] && bb=""; ` +
			`if [ -z "$bb" ]; then `
	}
	fallback := `[ -n "$bid" ] && bb=$(bd show "$bid" --json 2>/dev/null | ` +
		`jq -c ` + shellquote.Quote(blockingDepsJQ) + ` 2>/dev/null); `
	if federated {
		fallback += `fi; `
	}

	holdCheck := ""
	holdCond := ""
	if checkHold {
		holdCheck = `nheld=$(printf "%s" "` + v + `" | jq -r ` + shellquote.Quote(heldLabelCountJQ()) + ` 2>/dev/null); ` +
			`[ -z "$nheld" ] && nheld=0; `
		holdCond = ` && [ "$nheld" = "0" ]`
	}

	return `bid=$(printf "%s" "` + v + `" | jq -r ".[0].id // empty" 2>/dev/null); ` +
		carried +
		`bb="[]"; ` +
		fallback +
		`[ -z "$bb" ] && bb="[]"; ` +
		`nblocked=$(printf "%s" "$bb" | jq -r ` + shellquote.Quote(openBlockerCountJQ) + ` 2>/dev/null); ` +
		`[ -z "$nblocked" ] && nblocked=0; ` +
		holdCheck +
		`if [ "$nblocked" = "0" ]` + holdCond + `; then ` +
		enrichedVar + `=$(printf "%s" "` + v + `" | jq -c --argjson bb "$bb" ` +
		shellquote.Quote(enrichJQ) + ` 2>/dev/null); ` +
		`[ -n "` + e + `" ] && [ "` + e + `" != "[]" ] && ` + shellVar + `="` + e + `"; ` +
		`printf "%s" "` + v + `" && exit 0; ` +
		`fi; `
}

// assignedReadyTierCommand is the assigned-ready read for one identity: the
// pre-assigned tier, and the tier a graph step assigned to this worker arrives
// on. It is one of the four ready reads the federation swap covers.
func assignedReadyTierCommand(shellVar string, topo QueryTopology) string {
	fed := topo.FederatedReady
	return `r=$(` + readyReaderCommand(fed) + bdReadyIncludeEphemeralArg(topo.includeEphemeralReady()) +
		` --assignee="$` + shellVar + `" --json --limit=1` + readyReaderStderrSink(fed) + `)` +
		readyReaderFailurePropagation(fed) + `; `
}

func standardAssignedReadyWorkQueryScript(topo QueryTopology) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		assignedReadyTierCommand("id", topo) +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("id", topo) +
		`done; `
}

func legacyControlAssignedWorkQueryScript(topo QueryTopology) string {
	return legacyControlAssignedInProgressWorkQueryScript(topo) +
		legacyControlAssignedReadyWorkQueryScript(topo)
}

func legacyControlAssignedInProgressWorkQueryScript(topo QueryTopology) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		assignedInProgressTierCommand("cand", topo) +
		`if [ -n "$r" ] && [ "$r" != "[]" ]; then ` +
		inProgressBlockedByEnrichmentScript(topo.FederatedReady, true) +
		`fi; ` +
		ephemeralAssignedInProgressProbeScript("cand", topo) +
		`done; ` +
		`done; `
}

func legacyControlAssignedReadyWorkQueryScript(topo QueryTopology) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		assignedReadyTierCommand("cand", topo) +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("cand", topo) +
		`done; ` +
		`done; `
}

// ephemeralAssignedInProgressProbeScript is the ephemeral-store sibling of the
// `bd list --status in_progress` crash-recovery query, in the same tier-1 slot.
// It excludes candidates parked on a canonical dispatch hold
// (beadmeta.DispatchHoldLabels) inside its own jq selection, the equivalent
// fall-through to the bd-list tier's nheld serve gate (gas-kg6, #5114 review):
// without it a held ephemeral bead exited the script as the sole candidate, the
// Go-side filter then stripped it, and the hook reported no_work while ready
// work sat unqueried below — the same starvation, wearing a drain label.
// Filtering before the `.[:1]` truncation also lets a second, unheld
// in_progress candidate be served instead of being shadowed by a held first.
//
// Failure mode differs from the bd-list tier's fail-open serve, deliberately:
// this probe's jq consumes `bd query` stdout directly, so unparseable output
// yields an empty candidate and the probe falls through — it never serves raw
// stdout, with or without the hold filter. The hold-transparent existence
// probes (ephemeralAssignedReadyProbeScript) are out of scope per ga-5736js.
//
// It must also apply the same inProgressBlockedByEnrichmentScript gate before
// serving a surviving candidate, or a blocked in_progress wisp step is
// re-served on every hook tick (ga-qjozkw).
func ephemeralAssignedInProgressProbeScript(shellVar string, topo QueryTopology) string {
	_ = topo
	filter := `[.[] | select((.assignee // "") == $id)` + excludeHoldLabelsJQClause() + `] | .[:1]`
	// federated=false: this row comes from `bd query`, which never carries a
	// resolved blocked_by, so the carried-lookup branch would only ever fall
	// through to bd show — skip straight to it. checkHold=false: the filter
	// above already excludes held candidates before the `.[:1]` truncation, so
	// the post-truncation nheld check here would always read zero.
	return `r=$(` + bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq --arg id "$` + shellVar + `" ` + shellquote.Quote(filter) + ` 2>/dev/null); ` +
		`if [ -n "$r" ] && [ "$r" != "[]" ]; then ` +
		inProgressBlockedByEnrichmentScript(false, false) +
		`fi; `
}

// ephemeralAssignedReadyProbeScript is the bd-1.0.4 wisp tier. It stays on
// `bd query` because there is no federated form of it and it needs none: a
// relocated class store has no bead-policy layer, so an orchestration wisp lands
// there as a DURABLE row that the plain federated ready read already returns
// (see splitEnv.mintWispWith). The ephemeral tier only exists where the policy
// front door put the wisp somewhere a plain read cannot see it, which is the
// single-store city.
//
// It tries the fast dependency_count == 0 candidate first, then falls back to
// a dependency_count > 0 candidate enriched via a real bd show --json call,
// so a step whose dependencies have since all closed is still reachable —
// see ephemeralReadyDependencyCandidateFilterJQ. The enrichment call passes
// checkHold=false: this is an assignee-scoped existence probe, not the
// in_progress work-serving gate, so it must stay hold-transparent (ga-5736js;
// pinned by TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels).
func ephemeralAssignedReadyProbeScript(shellVar string, topo QueryTopology) string {
	if topo.includeEphemeralReady() {
		return ""
	}
	fastFilter := legacyEphemeralReadyFilterJQ(`select((.assignee // "") == $id)`, 1, false)
	slowFilter := ephemeralReadyDependencyCandidateFilterJQ(`select((.assignee // "") == $id)`, 1, false)
	return `open_ephemeral=$(` + bdQueryEphemeralStatusQuietShell("open") + `); ` +
		`r=$(printf "%s" "$open_ephemeral" | jq --arg id "$` + shellVar + `" ` + shellquote.Quote(fastFilter) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`r=$(printf "%s" "$open_ephemeral" | jq --arg id "$` + shellVar + `" ` + shellquote.Quote(slowFilter) + ` 2>/dev/null); ` +
		`if [ -n "$r" ] && [ "$r" != "[]" ]; then ` +
		// federated=false: this row comes from `bd query`, which never
		// carries a resolved blocked_by, so skip straight to the bd show
		// fallback. checkHold=false: ga-5736js, this probe must stay
		// hold-transparent.
		inProgressBlockedByEnrichmentScript(false, false) +
		`fi; `
}

func poolDemandOriginGateScript() string {
	return `case "$GC_SESSION_ORIGIN" in ` +
		`ephemeral|"") ;; ` +
		`*) exit 0 ;; ` +
		`esac; `
}

func routedPoolWorkQueryProbeScript(topo QueryTopology, targetCount int) string {
	script := poolDemandOriginGateScript() + poolDemandFirstRowFunctionScript(topo)
	for i := 1; i <= targetCount; i++ {
		script += fmt.Sprintf(`probe_pool_demand "$%d"; `, i)
	}
	return script + `printf "[]"`
}

func routedPoolWorkQueryCommand(topo QueryTopology, targets ...string) string {
	args := []string{"sh", "-c", routedPoolWorkQueryProbeScript(topo, len(targets)), "--"}
	args = append(args, targets...)
	return shellquote.Join(args)
}

// queryKind names one of the built-in agent query shapes.
type queryKind int

const (
	queryWork queryKind = iota
	queryAssignedInProgress
	queryAssignedReady
	queryRoutedPool
	queryPoolDemand
	queryOnDeath
	queryOnBoot
)

// querySpec describes how one query kind resolves: which user override
// field short-circuits the default, and how the default script is built.
type querySpec struct {
	// override returns the user-supplied command that replaces the
	// default entirely, or "" when the default applies.
	override func(*Agent) string
	// build returns the default command for a city topology. The onDeath/onBoot
	// builders ignore BOTH of the topology's fields today and MUST keep ignoring
	// them: the ephemeral flag by S04b invariant I6, and FederatedReady because
	// they are WRITE hooks (`bd list` + `bd update`), which the federated READER
	// has no form of — routing those is ga-601v2.
	build func(a *Agent, topo QueryTopology) string
}

// queryTable maps every query kind to its override field and default
// builder. It is populated once at init and only read afterward.
var queryTable = map[queryKind]querySpec{
	queryWork:               {override: func(a *Agent) string { return a.WorkQuery }, build: buildWorkQuery},
	queryAssignedInProgress: {override: func(a *Agent) string { return a.WorkQuery }, build: buildAssignedInProgressQuery},
	queryAssignedReady:      {override: func(a *Agent) string { return a.WorkQuery }, build: buildAssignedReadyQuery},
	queryRoutedPool:         {override: func(a *Agent) string { return a.WorkQuery }, build: buildRoutedPoolQuery},
	queryPoolDemand:         {override: func(a *Agent) string { return a.ScaleCheck }, build: buildPoolDemandQuery},
	queryOnDeath:            {override: func(a *Agent) string { return a.OnDeath }, build: buildOnDeath},
	queryOnBoot:             {override: func(a *Agent) string { return a.OnBoot }, build: buildOnBoot},
}

// effectiveQuery is the single resolver behind every Effective*Query
// accessor: the kind's user override verbatim if set, else the kind's
// default builder.
func (a *Agent) effectiveQuery(kind queryKind, topo QueryTopology) string {
	spec := queryTable[kind]
	if o := spec.override(a); o != "" {
		return o
	}
	return spec.build(a, topo)
}

// EffectiveWorkQuery returns the work query command for this agent.
// If WorkQuery is set, returns it as-is. Otherwise returns the default
// three-tier query with multi-identifier assignee resolution.
//
// Assignee resolution order: $GC_SESSION_ID (bead ID) > $GC_SESSION_NAME
// (tmux session name) > $GC_ALIAS (named identity / qualified name).
// All three are checked so work is found regardless of which identifier
// was used when assigning.
//
// State priority: in_progress+assigned (crash recovery) >
// ready+assigned (pre-assigned) > ready+unassigned+routed_to (pool).
// Executable formula roots can be epic-typed; the bead storage policy decides
// whether those roots are history-backed, no-history, or ephemeral for the
// configured bd compatibility mode. Molecule containers are not routable
// demand.
//
// Parent epics are excluded from the routed (pool) tier only
// (--exclude-type=epic). An unassigned parent epic has no executable spec —
// its semantic is "all children done" — so a pool worker claiming one does
// undefined work (gc-udx; the repro is a routed parent epic, see
// TestEffectiveWorkQuerySkipsEpicLeafScenario). The assigned tiers do NOT
// exclude epics: work already assigned to this agent is owned, and the
// patrol-loop pattern (gastown witness/refinery/deacon) can self-assign an
// epic wisp that the agent must resume after a session restart. Excluding
// epics there silently stranded those wisps (gc hook exited 1 with empty
// output). Roles that need different behavior still opt in via an explicit
// work_query in their agent config; that custom query is returned unchanged
// above.
//
// When the reconciler runs the query for demand detection (no session
// context), all identity vars are empty → assignee tiers skip → only
// the routed_to tier fires to detect new demand.
//
// Tier 3's canonical and migration predicates are shared with
// EffectivePoolDemandQuery so reconciler spawn decisions and worker claim
// decisions stay symmetric.
func (a *Agent) EffectiveWorkQuery() string {
	return a.effectiveQuery(queryWork, QueryTopology{})
}

// EffectiveWorkQueryFor returns the default work query built for a city
// topology: the configured bd semantics, and — on a city that serves a
// coordination class from a store `bd` in the work directory cannot reach — the
// federated ready reader in place of `bd ready`. A zero QueryTopology is the
// single-store city, and its command is byte-for-byte the one already deployed.
func (a *Agent) EffectiveWorkQueryFor(topo QueryTopology) string {
	return a.effectiveQuery(queryWork, topo)
}

func buildWorkQuery(a *Agent, topo QueryTopology) string {
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

// EffectiveAssignedInProgressQuery returns the assigned-in-progress-only command
// for prompt templates that spell out crash recovery as a separate startup tier.
// A custom WorkQuery is treated as the caller-owned full discovery contract, so
// split-tier prompts may run that same custom command in each query slot.
func (a *Agent) EffectiveAssignedInProgressQuery() string {
	return a.effectiveQuery(queryAssignedInProgress, QueryTopology{})
}

// EffectiveAssignedInProgressQueryFor returns the assigned-in-progress query
// built for a city topology. It is topology-BLIND today: the crash-recovery tier
// reads `bd list`, not the ready set — see
// standardAssignedInProgressWorkQueryScript for why federating it is ga-601v2's
// slice and not this one's.
func (a *Agent) EffectiveAssignedInProgressQueryFor(topo QueryTopology) string {
	return a.effectiveQuery(queryAssignedInProgress, topo)
}

func buildAssignedInProgressQuery(a *Agent, topo QueryTopology) string {
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedInProgressWorkQueryScript(topo) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedInProgressWorkQueryScript(topo) + `printf "[]"`})
}

// EffectiveAssignedReadyQuery returns the assigned-ready-only command for
// prompt templates that spell out claim-first startup in separate tiers. A
// custom WorkQuery is treated as the caller-owned full discovery contract, so
// split-tier prompts may run that same custom command in each query slot.
func (a *Agent) EffectiveAssignedReadyQuery() string {
	return a.effectiveQuery(queryAssignedReady, QueryTopology{})
}

// EffectiveAssignedReadyQueryFor returns the assigned-ready-only query built for
// a city topology.
func (a *Agent) EffectiveAssignedReadyQueryFor(topo QueryTopology) string {
	return a.effectiveQuery(queryAssignedReady, topo)
}

func buildAssignedReadyQuery(a *Agent, topo QueryTopology) string {
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", legacyControlAssignedReadyWorkQueryScript(topo) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", standardAssignedReadyWorkQueryScript(topo) + `printf "[]"`})
}

// EffectiveRoutedPoolQuery returns the routed-pool-only command for prompt
// templates that spell out claim-first startup in separate tiers. It is the
// prompt-side counterpart to EffectiveWorkQuery's routed pool tier.
func (a *Agent) EffectiveRoutedPoolQuery() string {
	return a.effectiveQuery(queryRoutedPool, QueryTopology{})
}

// EffectiveRoutedPoolQueryFor returns the routed-pool-only command built for a
// city topology.
func (a *Agent) EffectiveRoutedPoolQueryFor(topo QueryTopology) string {
	return a.effectiveQuery(queryRoutedPool, topo)
}

func buildRoutedPoolQuery(a *Agent, topo QueryTopology) string {
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return routedPoolWorkQueryCommand(topo, target)
	}
	return routedPoolWorkQueryCommand(topo, target, legacyTarget)
}

func legacyWorkflowControlQualifiedName(target string) string {
	target = strings.TrimSpace(target)
	if target == ControlDispatcherAgentName {
		return "workflow-control"
	}
	const suffix = "/" + ControlDispatcherAgentName
	if strings.HasSuffix(target, suffix) {
		return strings.TrimSuffix(target, suffix) + "/workflow-control"
	}
	return ""
}

// EffectiveSlingQuery returns the sling query command template for this agent.
// The template uses {} as a placeholder for the bead ID.
// If SlingQuery is set, returns it as-is. Otherwise returns the default:
// "bd update {} --set-metadata gc.routed_to=<template>"
//
// All agents use metadata-based routing. The reconciler and scale_check
// handle session creation; sling just stamps the target template.
func (a *Agent) EffectiveSlingQuery() string {
	if a.SlingQuery != "" {
		return a.SlingQuery
	}
	return a.DefaultSlingQuery()
}

// DefaultSlingQuery returns the built-in metadata-routing sling query for
// this agent. Callers outside config should prefer this helper over rebuilding
// the command string to preserve the bd boundary invariant.
func (a *Agent) DefaultSlingQuery() string {
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	return "bd update {} --set-metadata " + beadmeta.RoutedToMetadataKey + "=" + route
}

// EffectivePoolDemandQuery returns the count-form pool-demand query the
// reconciler runs to detect new unassigned routed work. It is the
// reconciler-side counterpart to EffectiveWorkQuery's Tier 3 (the worker
// claim path): both derive their predicates from the same helpers so
// any future change to the pool-demand shape flows to both paths
// simultaneously.
//
// If ScaleCheck is set (user override), it takes precedence and is
// returned as-is. Otherwise the default count-form is returned.
//
// Assigned in-progress work is resumed from session beads, so it must
// not create additional generic pool demand here.
//
// See engdocs/architecture/dispatch.md "scale_check ↔ work_query
// correspondence" and the protocol-mismatch class regression addressed
// by PR #1516.
func (a *Agent) EffectivePoolDemandQuery() string {
	return a.effectiveQuery(queryPoolDemand, QueryTopology{})
}

// EffectivePoolDemandQueryFor returns the count-form demand query built for a
// city topology. It reads through the same reader as EffectiveWorkQueryFor's
// routed tier — diverging the two is the protocol-mismatch class the
// "scale_check ↔ work_query correspondence" note names.
func (a *Agent) EffectivePoolDemandQueryFor(topo QueryTopology) string {
	return a.effectiveQuery(queryPoolDemand, topo)
}

func buildPoolDemandQuery(a *Agent, topo QueryTopology) string {
	target := a.poolDemandTarget()
	return poolDemandCountShell(target, topo)
}

// Query-override names as they are spelled in pack.toml / city.toml, for
// diagnostics that have to tell an operator WHICH key is the problem.
const (
	workQueryOverrideKey  = "work_query"
	scaleCheckOverrideKey = "scale_check"
)

// FederationBlindOverrides names the user-supplied query overrides that will not
// see a relocated coordination class on this topology.
//
// A custom work_query or scale_check is returned VERBATIM — that is the
// contract, and rewriting an operator's shell would be substring surgery on a
// script this package did not write. But on a split city a verbatim `bd ready`
// reads one store, so the operator's own command is silently blind to the graph
// class while the generated one is not. Silence is the defect: the whole point
// of the federated reader is that a short array cannot be told apart from "no
// work", and an override reintroduces exactly that, invisibly.
//
// So the fact is returned instead of guessed at. Callers that hold a real city
// print it; a single-store city returns nil and nothing is printed anywhere.
func (a *Agent) FederationBlindOverrides(topo QueryTopology) []string {
	if !topo.FederatedReady {
		return nil
	}
	var keys []string
	if strings.TrimSpace(a.WorkQuery) != "" {
		keys = append(keys, workQueryOverrideKey)
	}
	if strings.TrimSpace(a.ScaleCheck) != "" {
		keys = append(keys, scaleCheckOverrideKey)
	}
	return keys
}

// EffectiveScaleCheck returns the scale check command for this agent.
// Pass-through to EffectivePoolDemandQuery for back-compat with code and
// configs that name the predicate "scale_check"; new call sites should
// prefer EffectivePoolDemandQuery to make the dependency on the
// work_query predicate explicit.
func (a *Agent) EffectiveScaleCheck() string {
	return a.EffectivePoolDemandQuery()
}

// RecoveryHookMarker prefixes every diagnostic the DEFAULT on_death/on_boot
// recovery hooks print to stdout when a bd release fails. It is the contract
// between the generated templates (which emit it) and the controller callers
// (which surface only marked output): a user-supplied on_death/on_boot override
// is passed through verbatim and carries no marker, so its stdout is not
// mislabeled or spammed into the recovery log.
const RecoveryHookMarker = "gc-recovery:"

// EffectiveOnDeath returns the on_death command for this agent.
// If OnDeath is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work assigned to this concrete agent identity.
func (a *Agent) EffectiveOnDeath() string {
	return a.effectiveQuery(queryOnDeath, QueryTopology{})
}

// EffectiveOnDeathFor returns the default on_death command for a city topology.
// It is topology-blind: on_death is a write hook, and the federated reader has
// no write form (ga-601v2).
func (a *Agent) EffectiveOnDeathFor(topo QueryTopology) string {
	return a.effectiveQuery(queryOnDeath, topo)
}

func buildOnDeath(a *Agent, topo QueryTopology) string {
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	_ = topo
	ephemeralRead := bdQueryEphemeralStatusQuietShell("in_progress") + ` | ` +
		`jq -r --arg assignee ` + shellquote.Quote(a.QualifiedName()) + ` '.[] | select((.assignee // "") == $assignee) | [.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv' 2>/dev/null; `
	// Reset both assignee and status: clearing assignee alone leaves the bead
	// invisible to every work_query tier (Tier 1 needs assignee match, Tiers
	// 2/3 only match "ready" status). The next worker re-claims via Tier 3.
	// If routed metadata is missing entirely, backfill the canonical
	// gc.run_target route so reopened direct-assigned work does not stay
	// invisible.
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

// EffectiveOnBoot returns the on_boot command for this agent.
// If OnBoot is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work routed to this backing config.
func (a *Agent) EffectiveOnBoot() string {
	return a.effectiveQuery(queryOnBoot, QueryTopology{})
}

// EffectiveOnBootFor returns the default on_boot command for a city topology.
// It is topology-blind: on_boot is a write hook, and the federated reader has no
// write form (ga-601v2).
func (a *Agent) EffectiveOnBootFor(topo QueryTopology) string {
	return a.effectiveQuery(queryOnBoot, topo)
}

func buildOnBoot(a *Agent, topo QueryTopology) string {
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	_ = topo
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
