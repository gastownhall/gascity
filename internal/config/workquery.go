package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

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

// bdFatalSkewSignatures are stderr substrings that mark a bd failure as fatal
// (abort the script, preserve the message) rather than "try the next
// candidate". On the surface these signatures actually gate — the bd_or_fatal
// work-query / on_boot / on_death probes, which run `bd list/ready/update/
// query` — only "schema version mismatch" is reachable: those commands fail
// through beads' lowercase "failed to open database" (cmd/bd/main.go) or the
// schema-skew error, never the title-cased "Unable to open database", which
// beads emits only from its `bd doctor` checks (cmd/bd/doctor/*).
//
// "Unable to open database" is retained so this list stays byte-identical to
// the `bd doctor`-based surfaces that DO emit it — the SessionStart guard
// (internal/hooks.bdSchemaSkewHookSignatures) and the gc doctor schema-skew
// check (doctor_bd_schema_skew.go.bdSchemaSkewSignatures) — keeping one shared
// vocabulary across all three. It is simply a dead pattern on this surface,
// not a live detector.
//
// A genuinely unreachable store on this surface (bd's "failed to open
// database") is deliberately NOT treated as fatal: a transient mid-restart
// Dolt blip must still fall through silently as an ordinary rc=1, exactly as
// any other non-fatal bd failure does, so the reconciler is never hard-wedged
// by a momentary store hiccup.
var bdFatalSkewSignatures = []string{
	"schema version mismatch",
	"Unable to open database",
}

// bdFatalGuardFunctionName is the shell function name the guarded probe
// helpers below invoke. Keep in sync with bdFatalGuardFunctionScript.
const bdFatalGuardFunctionName = "bd_or_fatal"

// bdFatalGuardFunctionScript emits a `sh` function definition that wraps a
// bd invocation so a schema-skew hard failure aborts the work-query script
// (non-zero exit, stderr preserved) instead of being silently swallowed as
// an empty result — the bug behind ga-qyw3wn. On this surface the wrapped
// commands (bd list/ready/update/query) emit "schema version mismatch" on
// skew; an unreachable store is intentionally not fatal here (see
// bdFatalSkewSignatures for why). A genuine empty result (bd exit 0, any
// stdout) is returned unchanged. Any OTHER non-zero bd exit (a transient
// lock, an unreachable store, etc.) still falls through silently exactly as
// it did before this fix; only the named signature is treated as fatal.
//
// The function prints its bd invocation's stdout and returns 0 on success,
// returns 1 (no stdout) on an ordinary failure, and returns 2 (stderr
// printed to fd 2) on a fatal schema-skew failure. Callers must prepend
// this exactly once per generated `sh -c` script, before the first call
// site that references bdFatalGuardFunctionName (see bdOrFatalGuarded).
//
// On failure it re-runs the same bd invocation once more to classify the
// stderr text, since the primary invocation's stderr is discarded to avoid
// corrupting the captured stdout on success. This doubles bd's invocation
// count only on the (exceptional) failure path.
func bdFatalGuardFunctionScript() string {
	return bdFatalGuardFunctionName + `() { ` +
		`__bdf_out=$("$@" 2>/dev/null); __bdf_rc=$?; ` +
		`if [ $__bdf_rc -ne 0 ]; then ` +
		`__bdf_err=$("$@" 2>&1 >/dev/null); ` +
		`case "$__bdf_err" in ` + bdFatalSkewCaseClauses() + `) printf '%s\n' "$__bdf_err" >&2; return 2 ;; esac; ` +
		`return 1; ` +
		`fi; ` +
		`printf '%s' "$__bdf_out"; ` +
		`return 0; ` +
		`}; `
}

// bdFatalSkewCaseClauses renders bdFatalSkewSignatures as `sh` case-pattern
// clauses, e.g. `*"a"*|*"b"*`.
func bdFatalSkewCaseClauses() string {
	var b strings.Builder
	for i, sig := range bdFatalSkewSignatures {
		if i > 0 {
			b.WriteString("|")
		}
		b.WriteString(`*"` + sig + `"*`)
	}
	return b.String()
}

// bdOrFatalGuarded prefixes a bd command (without its own stderr redirect)
// with the bdFatalGuardFunctionName shell function, so callers wrap a probe
// as `r=$(` + bdOrFatalGuarded(cmd) + `); rc=$?` and then check
// `[ $rc -eq 2 ] && exit 1` immediately after — in a bare context, not
// nested inside another command substitution, so the exit actually
// terminates the script (see poolDemandFirstRowFunctionScript for the one
// call site that must relay this through a nested subshell instead).
func bdOrFatalGuarded(cmd string) string {
	return bdFatalGuardFunctionName + " " + cmd
}

func bdReadyIncludeEphemeralArg(includeEphemeralReady bool) string {
	if includeEphemeralReady {
		return " --include-ephemeral"
	}
	return ""
}

// jqMeta renders the jq expression that reads a bead-metadata key with an
// empty-string default, e.g. (.metadata["gc.routed_to"] // ""). Shell/jq
// builders use it so embedded key spellings stay anchored to the beadmeta
// vocabulary constants.
func jqMeta(key string) string {
	return `(.metadata["` + key + `"] // "")`
}

func bdReadyPoolDemandShell(limitFlag string, includeEphemeralReady bool) string {
	return `bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --metadata-field "` + beadmeta.RoutedToMetadataKey + `=$target" --unassigned --exclude-type=epic --json ` + limitFlag
}

// bdReadyPoolDemandMigrationShell is a temporary raw compatibility probe for
// graph.v2 workflow roots created before gc.routed_to root stamping shipped.
// It is scoped to workflow roots so gc.run_target remains an authoring hint
// everywhere else. Callers must pass its output through
// poolDemandMigrationFilterJQ so a stale divergent gc.run_target cannot remain
// visible once a root carries gc.routed_to. This retirement-window fallback
// requires jq in the default worker/reconciler environment; remove it with the
// Go-side legacy candidates after the backfill completion tracked by ga-dhf44.
func bdReadyPoolDemandMigrationShell(limitFlag string, includeEphemeralReady bool) string {
	return `bd ready` + bdReadyIncludeEphemeralArg(includeEphemeralReady) + ` --metadata-field "` + beadmeta.RunTargetMetadataKey + `=$target" --metadata-field "` + beadmeta.KindMetadataKey + `=` + beadmeta.KindWorkflow + `" --unassigned --exclude-type=epic --json --sort oldest ` + limitFlag
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

func legacyEphemeralReadyFilterJQ(selector string, limit int) string {
	filter := `[.[] | ` + selector +
		` | select(((.issue_type // .type // "") != "epic"))` +
		` | select(([ (.dependencies // [])[]` +
		` | select((.type // .dep_type // "") as $t | ($t == "blocks" or $t == "waits-for" or $t == "conditional-blocks"))` +
		` | select((.status // .depends_on_status // "") != "closed") ] | length) == 0)]` +
		` | sort_by(.created_at // "")`
	if limit > 0 {
		filter += ` | .[:` + strconv.Itoa(limit) + `]`
	}
	return filter
}

func legacyEphemeralPoolDemandShell(limit int, includeEphemeralReady, quiet bool) string {
	if includeEphemeralReady {
		return `printf "[]"`
	}
	filter := legacyEphemeralReadyFilterJQ(
		`select((.assignee // "") == "")`+
			` | select((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == $target) or ((`+jqMeta(beadmeta.RoutedToMetadataKey)+` == "") and (`+jqMeta(beadmeta.RunTargetMetadataKey)+` == $target) and (`+jqMeta(beadmeta.KindMetadataKey)+` == "`+beadmeta.KindWorkflow+`")))`,
		limit,
	)
	if !quiet {
		query := bdQueryEphemeralStatusShell("open")
		return `ephemeral_json=$(` + query + `) || exit $?; ` +
			`printf '%s' "$ephemeral_json" | jq --arg target "$target" ` + shellquote.Quote(filter)
	}
	// quiet: this snippet becomes the body of the caller's own command
	// substitution (legacy_ephemeral_candidates=$(...)), so a bare `exit`
	// here would only terminate that subshell, not the whole script. Exit
	// the subshell with a distinguishing status (2) on a fatal schema-skew
	// failure instead, and rely on the caller checking $? right after the
	// assignment (see poolDemandFirstRowFunctionScript) to relay it as a
	// real script-terminating exit. An ordinary (non-skew) bd failure still
	// resolves to a plain "[]", matching the non-quiet branch above.
	return `bdq=$(` + bdOrFatalGuarded(bdQueryEphemeralStatusShell("open")) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 2; ` +
		`[ $bd_rc -ne 0 ] && printf "[]" && exit 0; ` +
		`printf '%s' "$bdq" | jq --arg target "$target" ` + shellquote.Quote(filter) + ` 2>/dev/null || printf "[]"`
}

// poolDemandFirstRowFunctionScript emits the work_query Tier 3 function: it
// reads the first ready, unassigned, routed bead for the supplied target,
// prints it, and exits 0. The caller appends a terminal fallthrough
// (printf "[]") for the empty case.
func poolDemandFirstRowFunctionScript(includeEphemeralReady bool) string {
	return `probe_pool_demand() { ` +
		`target="$1"; ` +
		`[ -z "$target" ] && return 1; ` +
		`r=$(` + bdOrFatalGuarded(routedReadyTierCommand(includeEphemeralReady)) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`legacy_candidates=$(` + bdOrFatalGuarded(bdReadyPoolDemandMigrationShell("--limit=20", includeEphemeralReady)) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`r=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(1) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		// legacyEphemeralPoolDemandShell's quiet snippet runs inside this
		// assignment's own command substitution; it exits that subshell
		// with status 2 on fatal skew, which we relay here via $?.
		`legacy_ephemeral_candidates=$(` + legacyEphemeralPoolDemandShell(20, includeEphemeralReady, true) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`r=$(printf "%s" "$legacy_ephemeral_candidates" | jq '.[0:1]' 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`return 1; ` +
		`}; `
}

func routedReadyTierCommand(includeEphemeralReady bool) string {
	// The shared predicate stays order-free so the count-form does no wasted
	// sorting; the worker first-row path asks bd for the oldest candidates.
	// The tier is widened past a single row (limit=20, not limit=1) so a
	// self-blocked head (is_blocked / status==blocked) has Ready routed work
	// behind it to fall through to instead of idle-exiting; the hook layer
	// (filterUnreadyHookCandidates) strips the blocked head from the result.
	// No trailing redirect here: the caller wraps this with bdOrFatalGuarded,
	// which owns stderr handling.
	return bdReadyPoolDemandShell("--sort oldest --limit=20", includeEphemeralReady)
}

// poolDemandCountShell emits the reconciler count-form for target: it counts
// ready, unassigned, routed demand and prints the array length. It shares the
// canonical and migration predicates with poolDemandFirstRowFunctionScript so
// the reconciler's spawn decision and the worker's claim decision read the
// same demand shape.
//
// Unlike the work_query probe, this form must NOT redirect bd stderr or default
// to zero: a failed `bd ready` has to surface as an error rather than
// masquerade as "no demand", which would silently stop the pool from spawning.
// The && chain ensures any non-zero bd exit short-circuits the whole expression
// (TestEffectiveScaleCheckUsesReadyOnly).
func poolDemandCountShell(target string, includeEphemeralReady bool) string {
	script := `target="$1"; ` +
		`ready_json=$(` + bdReadyPoolDemandShell("--limit 0", includeEphemeralReady) + `) || exit $?; ` +
		`legacy_candidates=$(` + bdReadyPoolDemandMigrationShell("--limit 0", includeEphemeralReady) + `) || exit $?; ` +
		`legacy_json=$(printf "%s" "$legacy_candidates" | ` + poolDemandMigrationFilterJQ(0) + `) || exit $?; ` +
		`legacy_ephemeral_json=$(` + legacyEphemeralPoolDemandShell(0, includeEphemeralReady, false) + `) || exit $?; ` +
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

func standardAssignedWorkQueryScript(includeEphemeralReady bool) string {
	return standardAssignedInProgressWorkQueryScript(includeEphemeralReady) +
		standardAssignedReadyWorkQueryScript(includeEphemeralReady)
}

func standardAssignedInProgressWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`r=$(` + bdOrFatalGuarded(`bd list --status in_progress --assignee="$id" --json --limit=1`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedInProgressProbeScript("id", includeEphemeralReady) +
		`done; `
}

func standardAssignedReadyWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`r=$(` + bdOrFatalGuarded(`bd ready`+bdReadyIncludeEphemeralArg(includeEphemeralReady)+` --assignee="$id" --json --limit=1`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("id", includeEphemeralReady) +
		`done; `
}

func legacyControlAssignedWorkQueryScript(includeEphemeralReady bool) string {
	return legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady) +
		legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady)
}

func legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(` + bdOrFatalGuarded(`bd list --status in_progress --assignee="$cand" --json --limit=1`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedInProgressProbeScript("cand", includeEphemeralReady) +
		`done; ` +
		`done; `
}

func legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady bool) string {
	return `for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(` + bdOrFatalGuarded(`bd ready`+bdReadyIncludeEphemeralArg(includeEphemeralReady)+` --assignee="$cand" --json --limit=1`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		ephemeralAssignedReadyProbeScript("cand", includeEphemeralReady) +
		`done; ` +
		`done; `
}

func ephemeralAssignedInProgressProbeScript(shellVar string, includeEphemeralReady bool) string {
	_ = includeEphemeralReady
	return `ebdq=$(` + bdOrFatalGuarded(bdQueryEphemeralStatusShell("in_progress")) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`r=$(printf '%s' "$ebdq" | jq --arg id "$` + shellVar + `" '[.[] | select((.assignee // "") == $id)] | .[:1]' 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; `
}

func ephemeralAssignedReadyProbeScript(shellVar string, includeEphemeralReady bool) string {
	if includeEphemeralReady {
		return ""
	}
	filter := legacyEphemeralReadyFilterJQ(`select((.assignee // "") == $id)`, 1)
	return `ebdq=$(` + bdOrFatalGuarded(bdQueryEphemeralStatusShell("open")) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`r=$(printf '%s' "$ebdq" | jq --arg id "$` + shellVar + `" ` + shellquote.Quote(filter) + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; `
}

func poolDemandOriginGateScript() string {
	return `case "$GC_SESSION_ORIGIN" in ` +
		`ephemeral|"") ;; ` +
		`*) exit 0 ;; ` +
		`esac; `
}

func routedPoolWorkQueryProbeScript(includeEphemeralReady bool, targetCount int) string {
	script := bdFatalGuardFunctionScript() + poolDemandOriginGateScript() + poolDemandFirstRowFunctionScript(includeEphemeralReady)
	for i := 1; i <= targetCount; i++ {
		script += fmt.Sprintf(`probe_pool_demand "$%d"; `, i)
	}
	return script + `printf "[]"`
}

func routedPoolWorkQueryCommand(includeEphemeralReady bool, targets ...string) string {
	args := []string{"sh", "-c", routedPoolWorkQueryProbeScript(includeEphemeralReady, len(targets)), "--"}
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
	// build returns the default command. includeEphemeralReady carries
	// beads.UsesBD105ReadySemantics(); the onDeath/onBoot builders ignore
	// it today and MUST keep ignoring it (S04b invariant I6).
	build func(a *Agent, includeEphemeralReady bool) string
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
func (a *Agent) effectiveQuery(kind queryKind, includeEphemeralReady bool) string {
	spec := queryTable[kind]
	if o := spec.override(a); o != "" {
		return o
	}
	return spec.build(a, includeEphemeralReady)
}

// effectiveQueryForBeads resolves a kind using the bd compatibility
// semantics configured for the city.
func (a *Agent) effectiveQueryForBeads(kind queryKind, beads BeadsConfig) string {
	return a.effectiveQuery(kind, beads.UsesBD105ReadySemantics())
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
	return a.effectiveQuery(queryWork, false)
}

// EffectiveWorkQueryForBeads returns the default work query using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveWorkQueryForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryWork, beads)
}

func buildWorkQuery(a *Agent, includeEphemeralReady bool) string {
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		script := bdFatalGuardFunctionScript() +
			standardAssignedWorkQueryScript(includeEphemeralReady) +
			poolDemandOriginGateScript() +
			poolDemandFirstRowFunctionScript(includeEphemeralReady) +
			`probe_pool_demand "$1"; ` +
			`printf "[]"`
		return shellquote.Join([]string{"sh", "-c", script, "--", target})
	}
	script := bdFatalGuardFunctionScript() +
		legacyControlAssignedWorkQueryScript(includeEphemeralReady) +
		poolDemandOriginGateScript() +
		poolDemandFirstRowFunctionScript(includeEphemeralReady) +
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
	return a.effectiveQuery(queryAssignedInProgress, false)
}

// EffectiveAssignedInProgressQueryForBeads returns the assigned-in-progress
// query using the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveAssignedInProgressQueryForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryAssignedInProgress, beads)
}

func buildAssignedInProgressQuery(a *Agent, includeEphemeralReady bool) string {
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", bdFatalGuardFunctionScript() + legacyControlAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", bdFatalGuardFunctionScript() + standardAssignedInProgressWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

// EffectiveAssignedReadyQuery returns the assigned-ready-only command for
// prompt templates that spell out claim-first startup in separate tiers. A
// custom WorkQuery is treated as the caller-owned full discovery contract, so
// split-tier prompts may run that same custom command in each query slot.
func (a *Agent) EffectiveAssignedReadyQuery() string {
	return a.effectiveQuery(queryAssignedReady, false)
}

// EffectiveAssignedReadyQueryForBeads returns the assigned-ready-only query
// using the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveAssignedReadyQueryForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryAssignedReady, beads)
}

func buildAssignedReadyQuery(a *Agent, includeEphemeralReady bool) string {
	target := a.poolDemandTarget()
	if legacyWorkflowControlQualifiedName(target) != "" {
		return shellquote.Join([]string{"sh", "-c", bdFatalGuardFunctionScript() + legacyControlAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
	}
	return shellquote.Join([]string{"sh", "-c", bdFatalGuardFunctionScript() + standardAssignedReadyWorkQueryScript(includeEphemeralReady) + `printf "[]"`})
}

// EffectiveRoutedPoolQuery returns the routed-pool-only command for prompt
// templates that spell out claim-first startup in separate tiers. It is the
// prompt-side counterpart to EffectiveWorkQuery's routed pool tier.
func (a *Agent) EffectiveRoutedPoolQuery() string {
	return a.effectiveQuery(queryRoutedPool, false)
}

// EffectiveRoutedPoolQueryForBeads returns the routed-pool-only command using
// the bd compatibility semantics configured for the city.
func (a *Agent) EffectiveRoutedPoolQueryForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryRoutedPool, beads)
}

func buildRoutedPoolQuery(a *Agent, includeEphemeralReady bool) string {
	target := a.poolDemandTarget()
	legacyTarget := legacyWorkflowControlQualifiedName(target)
	if legacyTarget == "" {
		return routedPoolWorkQueryCommand(includeEphemeralReady, target)
	}
	return routedPoolWorkQueryCommand(includeEphemeralReady, target, legacyTarget)
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
	return a.effectiveQuery(queryPoolDemand, false)
}

// EffectivePoolDemandQueryForBeads returns the count-form demand query using
// the bd compatibility semantics configured for the city.
func (a *Agent) EffectivePoolDemandQueryForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryPoolDemand, beads)
}

func buildPoolDemandQuery(a *Agent, includeEphemeralReady bool) string {
	target := a.poolDemandTarget()
	return poolDemandCountShell(target, includeEphemeralReady)
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
	return a.effectiveQuery(queryOnDeath, false)
}

// EffectiveOnDeathForBeads returns the default on_death command using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveOnDeathForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryOnDeath, beads)
}

func buildOnDeath(a *Agent, includeEphemeralInProgress bool) string {
	route := a.QualifiedName()
	if a.PoolName != "" {
		route = a.PoolName
	}
	_ = includeEphemeralInProgress
	tsvFields := `[.id, ` + jqMeta(beadmeta.RunTargetMetadataKey) + `, ` + jqMeta(beadmeta.RoutedToMetadataKey) + `] | @tsv`
	// Reset both assignee and status: clearing assignee alone leaves the bead
	// invisible to every work_query tier (Tier 1 needs assignee match, Tiers
	// 2/3 only match "ready" status). The next worker re-claims via Tier 3.
	// If routed metadata is missing entirely, backfill the canonical
	// gc.run_target route so reopened direct-assigned work does not stay
	// invisible.
	//
	// Every bd read/write below hard-fails on schema skew (ga-ooka7o): a
	// schema-skew failure here doesn't just make an agent look idle
	// (visible) -- it makes fleet self-healing silently no-op (a dead
	// agent's claimed work never released). Reads are bd_or_fatal-guarded
	// and captured via command substitution first (so the `bd_rc -eq 2`
	// check runs in a bare, non-piped context and a real `exit` actually
	// terminates the script). Writes capture stderr directly instead
	// (`if ! err=$(... 2>&1 >/dev/null); then ...`, ga-7xzmtd) so an
	// ordinary write failure still surfaces a RecoveryHookMarker
	// diagnostic and lets the loop continue (exit 0); only a schema-skew
	// match escalates to a hard, non-zero exit. The update loop relays
	// that fatal exit out of the while-subshell by exiting with status 2
	// and checking $? immediately after the pipeline, mirroring
	// poolDemandFirstRowFunctionScript's nested-subshell relay.
	return bdFatalGuardFunctionScript() +
		`assigned_raw=$(` + bdOrFatalGuarded(`bd list --assignee=`+a.QualifiedName()+` --status=in_progress --json`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`assigned_rows=$(printf '%s' "$assigned_raw" | jq -r '.[] | ` + tsvFields + `' 2>/dev/null); ` +
		`ephemeral_raw=$(` + bdOrFatalGuarded(bdQueryEphemeralStatusShell("in_progress")) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`ephemeral_rows=$(printf '%s' "$ephemeral_raw" | jq -r --arg assignee ` + shellquote.Quote(a.QualifiedName()) + ` '.[] | select((.assignee // "") == $assignee) | ` + tsvFields + `' 2>/dev/null); ` +
		`{ printf '%s\n' "$assigned_rows"; printf '%s\n' "$ephemeral_rows"; } | ` +
		`while IFS="$(printf '\t')" read -r id run_target routed_to; do ` +
		`[ -z "$id" ] && continue; ` +
		`if [ -n "$run_target" ] || [ -n "$routed_to" ]; then ` +
		`if ! err=$(bd update "$id" --assignee "" --status open 2>&1 >/dev/null); then ` +
		`case "$err" in ` + bdFatalSkewCaseClauses() + `) printf '%s\n' "$err" >&2; exit 2 ;; esac; ` +
		`printf 'gc-recovery: on_death release failed for %s: %s\n' "$id" "$err"; ` +
		`fi; ` +
		`else if ! err=$(bd update "$id" --assignee "" --status open --set-metadata ` + shellquote.Quote(beadmeta.RunTargetMetadataKey+"="+route) + ` 2>&1 >/dev/null); then ` +
		`case "$err" in ` + bdFatalSkewCaseClauses() + `) printf '%s\n' "$err" >&2; exit 2 ;; esac; ` +
		`printf 'gc-recovery: on_death release failed for %s: %s\n' "$id" "$err"; ` +
		`fi; ` +
		`fi; ` +
		`done; death_rc=$?; if [ $death_rc -eq 2 ]; then exit 1; fi`
}

// EffectiveOnBoot returns the on_boot command for this agent.
// If OnBoot is set, returns it. Otherwise returns the default recovery hook
// that unclaims in-progress work routed to this backing config.
func (a *Agent) EffectiveOnBoot() string {
	return a.effectiveQuery(queryOnBoot, false)
}

// EffectiveOnBootForBeads returns the default on_boot command using the bd
// compatibility semantics configured for the city.
func (a *Agent) EffectiveOnBootForBeads(beads BeadsConfig) string {
	return a.effectiveQueryForBeads(queryOnBoot, beads)
}

func buildOnBoot(a *Agent, includeEphemeralInProgress bool) string {
	template := a.QualifiedName()
	if a.PoolName != "" {
		template = a.PoolName
	}
	_ = includeEphemeralInProgress
	// Every bd read below is bd_or_fatal-guarded (ga-ooka7o), mirroring
	// buildOnDeath: reads are captured via command substitution first so
	// the `bd_rc -eq 2` check runs in a bare context where a real `exit`
	// terminates the script. The final update loop captures stderr
	// directly instead (`if ! err=$(... 2>&1 >/dev/null); then ...`,
	// ga-7xzmtd) so an ordinary write failure still surfaces a
	// RecoveryHookMarker diagnostic and lets the loop continue (exit 0);
	// only a schema-skew match escalates to a hard, non-zero exit, relayed
	// out of the while-subshell the same way buildOnDeath's does. That
	// loop is a `while read`, not xargs, because xargs execs a real binary
	// per item and can't invoke the bd_or_fatal shell function the reads
	// above depend on.
	return `template=` + shellquote.Quote(template) + `; ` +
		bdFatalGuardFunctionScript() +
		`routed_raw=$(` + bdOrFatalGuarded(`bd list --metadata-field "`+beadmeta.RoutedToMetadataKey+`=$template" --status=in_progress --no-assignee --json`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`routed_ids=$(printf '%s' "$routed_raw" | jq -r '.[].id' 2>/dev/null); ` +
		`run_target_raw=$(` + bdOrFatalGuarded(`bd list --metadata-field "`+beadmeta.RunTargetMetadataKey+`=$template" --metadata-field "`+beadmeta.KindMetadataKey+`=`+beadmeta.KindWorkflow+`" --status=in_progress --no-assignee --json`) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`run_target_ids=$(printf '%s' "$run_target_raw" | jq -r '.[] | select(` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") | .id' 2>/dev/null); ` +
		`ephemeral_raw=$(` + bdOrFatalGuarded(bdQueryEphemeralStatusShell("in_progress")) + `); bd_rc=$?; ` +
		`[ $bd_rc -eq 2 ] && exit 1; ` +
		`ephemeral_ids=$(printf '%s' "$ephemeral_raw" | jq -r --arg template "$template" '.[] | select((.assignee // "") == "") | select((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == $template) or ((` + jqMeta(beadmeta.RoutedToMetadataKey) + ` == "") and (` + jqMeta(beadmeta.RunTargetMetadataKey) + ` == $template) and (` + jqMeta(beadmeta.KindMetadataKey) + ` == "` + beadmeta.KindWorkflow + `"))) | .id' 2>/dev/null); ` +
		`{ printf '%s\n' "$routed_ids"; printf '%s\n' "$run_target_ids"; printf '%s\n' "$ephemeral_ids"; } | ` +
		`awk 'NF && !seen[$0]++' | ` +
		`while IFS= read -r id; do ` +
		`[ -z "$id" ] && continue; ` +
		`if ! err=$(bd update "$id" --status open 2>&1 >/dev/null); then ` +
		`case "$err" in ` + bdFatalSkewCaseClauses() + `) printf '%s\n' "$err" >&2; exit 2 ;; esac; ` +
		`printf 'gc-recovery: on_boot reopen failed for %s: %s\n' "$id" "$err"; ` +
		`fi; ` +
		`done; boot_rc=$?; if [ $boot_rc -eq 2 ]; then exit 1; fi`
}
