package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

const hookClaimCommandName = "hook"

// The two doors the drain fence guards, as an operator sees them named on a
// pane. Both refuse through the same writeHookClaimDrain contract and emit the
// same JSON record (command "hook" either way), so the label is the only thing
// that says which one answered.
const (
	hookClaimLabel     = "gc hook --claim"
	hookDiscoveryLabel = "gc hook"
)

// Drain-action reasons for the gc hook --claim result contract
// (schemas/hook/result.schema.json). Every value here is a valid reason when
// action is "drain": an idle store, an operational claim-write failure, a
// refused stale session, a refused non-turn invocation, or a seat whose session
// row is already draining.
const (
	hookClaimReasonNoWork         = "no_work"
	hookClaimReasonClaimsErrored  = "claims_errored"
	hookClaimReasonStaleSession   = "stale_session"
	hookClaimReasonNonTurnContext = "non_turn_context"
	hookClaimReasonDrainPending   = "drain_pending"
)

// Reasons carried on a bead.claim_released event: which unwind gave the claim
// back. Both describe a claim this process WON and could not hand to a live
// consumer.
const (
	hookClaimReleaseReasonUndelivered = "result_undelivered"
	hookClaimReleaseReasonStraddled   = "claim_window_straddled"
)

var hookClaimMutationTimeout = 10 * time.Second

// hookClaimWindowDefault bounds how long after a `gc hook --claim` invocation
// began a claim mutation may still run. Past it, the turn that invoked the
// command is assumed gone and the claim would be born into nothing.
//
// It is DERIVED from the work-query budget rather than a flat constant. A fresh
// `gc hook --claim` first spends up to hookWorkQueryTimeout finding routed work —
// a loaded multi-rig city's federated probe legitimately runs tens of seconds —
// and only then reaches the claim CAS, which needs up to hookClaimMutationTimeout.
// A flat 45s window anchored at invocation start charged that read latency
// against the claim, so raising hookWorkQueryTimeout was inert on the --claim
// path: the query now succeeds at ~t=70s but the fence refused the claim at 45s,
// relocating the starvation from session.work_query_failed to
// execution.claim_window_expired. Summing the two budgets keeps the turn-binding
// intent — a claim reaching the CAS later than an honest full-budget
// query-plus-mutation could is treated as orphaned — while giving a genuinely
// slow-but-alive read the headroom to claim the work the raise now surfaces.
//
// Ceilings: a provider CALLBACK lane is refused earlier by hookClaimNonTurnMarker
// and never reaches this window, so the 15s callback budget is not the bound here;
// the bound is the DIRECT turn's patience, which the hookWorkQueryTimeout raise
// already assumes is at least this budget. The window can now exceed
// idleClaimNudgeGrace (90s); in the measured worst case (~70s) the claim still
// lands before the backstop's first nudge, and a pathological slow read only earns
// the idle-claim backstop's next idempotent (NDI) re-nudge, never a double claim.
// GC_HOOK_CLAIM_WINDOW (resolveHookClaimWindow) still overrides this default.
var hookClaimWindowDefault = hookWorkQueryTimeout + hookClaimMutationTimeout

// hookClaimNonTurnEnvMarkers are the environment markers that prove a
// `gc hook --claim` process is a provider CALLBACK rather than an agent turn.
// gc sets all three itself: GC_HOOK_CALLBACK_LANE on every child of the managed
// `gc hook run` wrapper, and GC_MANAGED_SESSION_HOOK / GC_HOOK_EVENT_NAME on the
// rendered per-provider hook commands (internal/hooks, and the pack overlays'
// hooks.json). They are per-command prefixes on those callback lanes, never part
// of a session's own turn environment, so a turn carries none of them.
var hookClaimNonTurnEnvMarkers = []string{
	"GC_HOOK_CALLBACK_LANE",
	"GC_MANAGED_SESSION_HOOK",
	"GC_HOOK_EVENT_NAME",
}

var hookClaimCommandRunnerWithEnvContext = beads.ExecCommandRunnerWithExactEnvContext

// hookClaimNonTurnMarker returns the first non-turn marker present in env, or ""
// when this invocation looks like a real agent turn.
//
// An explicitly falsy value is not a marker: a shell that exports
// GC_HOOK_CALLBACK_LANE=0 must not fence its own turn.
func hookClaimNonTurnMarker(env []string) string {
	for _, key := range hookClaimNonTurnEnvMarkers {
		switch strings.ToLower(hookClaimEnvValue(env, key)) {
		case "", "0", "false":
			continue
		default:
			return key
		}
	}
	return ""
}

// resolveHookClaimWindow returns this invocation's claim window, honoring the
// GC_HOOK_CLAIM_WINDOW operator escape hatch (a Go duration). An unparseable or
// non-positive override falls back to the default rather than disabling the
// fence, which is the direction that stays safe.
func resolveHookClaimWindow() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GC_HOOK_CLAIM_WINDOW")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return hookClaimWindowDefault
}

// hookClaimWindowExpiry is the observation an expired claim window reports: how
// old the invocation was, and whether its parent is still alive. A dead parent
// (reparented to init) is the process-table signature of the orphaned tool call
// this fence exists to stop.
type hookClaimWindowExpiry struct {
	BeadID        string
	InvocationAge time.Duration
	ParentAlive   bool
}

// hookClaimReleaseRecord is one claim given back because it could not be
// delivered to a live consumer.
type hookClaimReleaseRecord struct {
	BeadID   string
	Assignee string
	Reason   string
}

type hookClaimOptions struct {
	Assignee string
	// SessionID is this session's durable bead ID. Assignee is deliberately the
	// alias/agent form that read paths query through GC_AGENT, but a continuation
	// pin means "run this on THIS session", which only a session identity can
	// express. See continuationPinAssignee.
	SessionID          string
	IdentityCandidates []string
	RouteTargets       []string
	Env                []string
	DrainAck           bool
	JSON               bool
}

// continuationPinAssignee returns the identity a continuation sibling is pinned
// to. It prefers the session's durable bead ID because that is the only value
// the consumers of an assignee agree on: ComputeAwakeSet matches it via
// sessionAssigneeMatches (assignee == bead.ID), and the session's own re-poll
// queries $GC_SESSION_ID first.
//
// Assignee cannot serve here. With no alias or agent in the environment it falls
// through to the runtime session-name form — GC_SESSION_NAME, resolved via
// hookSessionAgentForQuery in the pool-worker path where this fix is load-bearing
// (gascity--gc__implementation-worker-5-pool) — which is not what a session bead
// records as session_name
// (gc__implementation-worker-gcs-session-<id>). Beads pinned to that form matched
// no session identity at all, so wake demand could never reach them and the
// molecule stalled permanently.
//
// The reconciler's continuation-claim CANDIDATE gate is NOT one of those
// consumers: evaluateReadyContinuationClaimCandidate (build_desired_state.go)
// admits a row only when the root's gc.session_name equals the sibling's
// assignee, and that key only ever holds a session name / alias
// (sessionBeadIdentifier), never a bead ID — so a bead-ID pin is absent
// before currentSessionAssigneeIdentities is ever consulted. That is
// follow-up, not a regression: the slot-label form failed the same gate.
func continuationPinAssignee(opts hookClaimOptions) string {
	if id := strings.TrimSpace(opts.SessionID); id != "" {
		return id
	}
	return opts.Assignee
}

type hookClaimOps struct {
	Runner             WorkQueryRunner
	Claim              hookClaimFunc
	ListContinuation   hookListContinuationFunc
	AssignContinuation hookAssignContinuationFunc
	DrainAck           hookDrainAckFunc
	// DrainPending reports whether the session bead named by sessionID is
	// already draining, i.e. whether this seat has been told to stop. It is the
	// F-D fence's only input and is read from the SESSION row rather than from
	// provider meta: reconciler-tracked drains never set GC_DRAIN, so provider
	// meta does not cover the population this fence exists for.
	DrainPending hookDrainPendingFunc
	// EmitClaimRejected publishes a bead.claim_rejected event when a claim is
	// lost to a different live claimant (ADR-0009). Best-effort.
	EmitClaimRejected hookEmitClaimRejectedFunc
	// ResolveWorkBranch returns the git branch of the worker's worktree (dir),
	// stamped onto the bead as gc.work_branch at claim time. Empty result (no
	// repo / detached HEAD) omits the branch key — the session back-reference is
	// still stamped.
	ResolveWorkBranch hookResolveWorkBranchFunc
	// StampWorkMeta writes the claim-time execution-identity metadata patch
	// (gc.work_branch and/or the durable session back-reference gc.session_id /
	// gc.session_name) onto the claimed bead in ONE update. Best-effort.
	StampWorkMeta hookStampWorkMetaFunc
	// StampSessionClaim records the claimed bead id on the CLAIMING SESSION's own
	// bead — the reverse direction from StampWorkMeta, and the only route by
	// which the step's shell can later learn which bead it is running.
	// Best-effort.
	StampSessionClaim hookStampSessionClaimFunc
	// ReadWorkMeta is the post-stamp authoritative readback used only to
	// establish the durable lifecycle-start emission point.
	ReadWorkMeta             func(context.Context, string, []string, string, string) (beads.Bead, error)
	EmitExecutionStepStarted func(beads.Bead, string, []string, string)
	// PublishRunMap writes best-effort session-to-run correlation without
	// mutating the session bead after a successful work claim.
	PublishRunMap hookPublishRunMapFunc
	// Release gives back a claim this invocation won but could not deliver. It
	// is compare-and-swap on the assignee (release-if-current), so a claim that
	// legitimately changed hands in the meantime is left alone. It reports
	// whether the release actually landed.
	Release hookClaimReleaseFunc
	// EmitClaimWindowExpired and EmitClaimReleased publish the two turn-binding
	// facts. Best-effort, like EmitClaimRejected.
	EmitClaimWindowExpired func(hookClaimWindowExpiry)
	EmitClaimReleased      func(hookClaimReleaseRecord)
	Now                    func() time.Time
	// InvokedAt is when this `gc hook --claim` invocation began, and ClaimWindow
	// is how long after it a claim mutation may still run. Together they are the
	// turn-binding fence: a claim reaching a CAS past InvokedAt+ClaimWindow has
	// outlived the turn that asked for it. applyDefaults fills both, once per
	// invocation, so every federated leg shares ONE window rather than getting a
	// fresh one each time the loop copies the ops.
	InvokedAt   time.Time
	ClaimWindow time.Duration
	// ClassRoute is the relocated coordination-class binding these seams
	// escalate to, or nil on a city that relocates nothing. It is not a seam:
	// it is here so claimHookWorkWithRunner — the only caller that knows the
	// whole work fan-out — can hand the route its leg set, which is what lets a
	// not-found from ONE leg be checked against the others before it opens the
	// escalation. See claim_class_route.go.
	ClassRoute *hookClaimClassRoute
}

type (
	hookClaimFunc              func(context.Context, string, []string, string, string) (beads.Bead, bool, error)
	hookListContinuationFunc   func(context.Context, string, []string, string, string) ([]beads.Bead, error)
	hookAssignContinuationFunc func(context.Context, string, []string, string, string) error
	hookDrainAckFunc           func(io.Writer) error
	hookDrainPendingFunc       func(sessionID string) (bool, error)
	hookEmitClaimRejectedFunc  func(beadID, existingClaimant, attemptedClaimant string)
	hookResolveWorkBranchFunc  func(dir string) string
	hookStampWorkMetaFunc      func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error
	hookStampSessionClaimFunc  func(sessionID, beadID string) error
	hookPublishRunMapFunc      func(runID, beadID string, sessionKeys ...string) error
	hookClaimReleaseFunc       func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error)
)

type hookClaimJSONResult struct {
	SchemaVersion        string   `json:"schema_version"`
	OK                   bool     `json:"ok"`
	Command              string   `json:"command"`
	Action               string   `json:"action"`
	Reason               string   `json:"reason,omitempty"`
	BeadID               string   `json:"bead_id,omitempty"`
	Assignee             string   `json:"assignee,omitempty"`
	Route                string   `json:"route,omitempty"`
	RootBeadID           string   `json:"root_bead_id,omitempty"`
	ContinuationGroup    string   `json:"continuation_group,omitempty"`
	ContinuationAssigned []string `json:"continuation_assigned,omitempty"`
	DrainAcknowledged    bool     `json:"drain_acknowledged,omitempty"`
}

// hookClaimResult is the outcome of attempting a claim against one store's
// captured work-query output. A terminal result has already written its final
// output — a claim, an existing assignment, or a hard error — and the caller
// must return code as-is. A non-terminal result means the store yielded no
// claimable work (it was empty/unready, every claimable candidate was lost to
// another claimant, or every claimable candidate's claim mutation errored and was
// skipped) and NO terminal output was written, so a federated caller may try a
// later store before writing the single no-work drain.
type hookClaimResult struct {
	terminal bool
	code     int
	// claimsErrored is set on a NON-terminal result when one or more eligible
	// candidates' claim mutations errored and nothing was ultimately claimed. It
	// lets the shared no-work drain report a distinct "claims_errored" reason
	// instead of a healthy "no_work", so an operational write failure (store
	// contention or a controller-socket flap in the read→write window) — or an
	// assigned candidate this store cannot resolve at all, the split-city
	// see-but-cannot-claim shape — is not laundered into an idle signal.
	// Meaningless on a terminal result.
	claimsErrored bool
}

func doHookClaim(workQuery, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	res := tryHookClaim(workQuery, dir, &opts, &ops, stdout, stderr)
	if res.terminal {
		return res.code
	}
	return writeHookClaimNoWork(opts, ops, res.claimsErrored, dir, stdout, stderr)
}

// tryHookClaim runs the work query for one store (dir, via ops.Runner) and
// attempts to claim a ready candidate. It returns a terminal result once a
// claim, existing assignment, or hard error has been written, or a non-terminal
// result — with NO output written — when the store yielded no claimable work, so
// a federated caller can try a later store before draining. opts and ops are
// normalized in place so a non-terminal caller can reuse the normalized ops
// (defaults applied) for the shared drain.
func tryHookClaim(workQuery, dir string, opts *hookClaimOptions, ops *hookClaimOps, stdout, stderr io.Writer) hookClaimResult {
	opts.Assignee = strings.TrimSpace(opts.Assignee)
	opts.IdentityCandidates = hookClaimIdentityCandidates(append([]string{opts.Assignee}, opts.IdentityCandidates...)...)
	opts.RouteTargets = hookClaimRouteTargets(opts.RouteTargets...)
	if opts.Assignee == "" {
		fmt.Fprintln(stderr, "gc hook --claim: assignee not specified (set $GC_SESSION_NAME or $GC_SESSION_ID)") //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	if ops.Runner == nil {
		fmt.Fprintln(stderr, "gc hook --claim: missing work query runner") //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	ops.applyDefaults()
	now := ops.Now

	// F-A. A provider callback is not a turn: its stdout goes to the hook
	// runner, not to a model, so a claim minted here is parked the instant it is
	// won. Refuse before any mutation, and refuse WITHOUT consuming --drain-ack —
	// a callback must never acknowledge the session's drain on the session's
	// behalf. Exit 0 so the provider does not retry the refusal every prompt.
	//
	// Only --claim is fenced. A callback's read-only hook uses (--inject, plain
	// discovery, nudge drain, mail check) never reach here.
	if marker := hookClaimNonTurnMarker(opts.Env); marker != "" {
		return hookClaimResult{terminal: true, code: writeHookClaimNonTurnDrain(marker, *opts, stdout, stderr)}
	}

	// F-D. A seat whose session row already says `draining` must not take new
	// work. The hook's drain protocol was "drain only when idle" by
	// construction — --drain-ack is consumed ONLY on the no-work path — so on a
	// busy repo a draining seat kept finding claimable work and postponed its
	// own drain indefinitely. That is the working-while-draining wedge: the
	// specimen seat claimed and executed a preflight three hours into its drain
	// while the graph store recorded that step under a fresher seat.
	//
	// PRE-MUTATION, NOT PRE-QUERY. In this function the fence precedes
	// ops.Runner, but the production caller (claimHookWorkWithRunner) has
	// already run the federated work query to SELECT a store and passes a
	// pre-captured runner, so on that path the probe runs after the query. The
	// guarantee this fence actually makes is the one that matters: no claim CAS,
	// no adoption, no stamp. A draining seat still pays the query it no longer
	// needs — waste, bounded by its remaining polls, not a correctness gap.
	//
	// Refusing here also puts the fence ahead of hookClaimExistingAssignment.
	// Adoption is deliberately fenced too: letting a draining seat resume its
	// own in-progress bead re-parks the identical wedge. That work belongs to
	// the dead-assignee and reopen lanes once the drain completes.
	//
	// The refusal rides the existing "drain" action — the agent protocol already
	// treats it as "wind down" — and, unlike F-A, it CONSUMES --drain-ack. That
	// is the entire point: it converts a wedge into a prompt self-drain.
	if sessionID := hookClaimSessionID(opts.Env); sessionID != "" {
		pending, err := ops.DrainPending(sessionID)
		switch {
		case err != nil:
			// Fail open. This probe is one store read on every agent turn in the
			// city, so a store hiccup here must not idle every healthy seat; the
			// drain lanes remain the backstop exactly as they are today. Name the
			// fault without the alarming refusal wording, the same shape the
			// runtime-identity fence's store-unavailable arm uses.
			//
			// The event is what makes failing open survivable. A persistent probe
			// fault switches this fence AND the runtime-identity fence off
			// fleet-wide, and stderr inside an agent pane is not an operator
			// signal; this record is the only thing off-pane that distinguishes
			// "fence acting" from "fence inert".
			fmt.Fprintf(stderr, "gc hook --claim: drain-pending probe unavailable for %s: %v; proceeding to claim\n", sessionID, err) //nolint:errcheck
			hookEmitDrainFenceUnavailable(stderr, sessionID, hookClaimEnvValue(opts.Env, "GC_TEMPLATE"), err)
		case pending:
			return hookClaimResult{terminal: true, code: writeHookClaimDrainPending(hookClaimLabel, sessionID, *opts, *ops, stdout, stderr)}
		}
	}

	output, err := ops.Runner(workQuery, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}

	normalized := normalizeWorkQueryOutput(strings.TrimSpace(output))
	normalized = filterUnreadyHookCandidates(normalized, now())
	if !workQueryHasReadyWork(normalized) {
		return hookClaimResult{}
	}
	candidates, skipped, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	for _, skip := range skipped {
		fmt.Fprintf(stderr, "gc hook --claim: skipping undecodable bead %s: %v\n", skip.ID, skip.Err) //nolint:errcheck
	}
	if len(candidates) == 0 {
		return hookClaimResult{}
	}

	if result, bead, ok := hookClaimExistingAssignment(candidates, *opts); ok {
		// Adoption mints no CAS, so until now it minted its receipt on the word
		// of the work query alone — and a stale caching-store row survives long
		// enough to re-serve a bead the dispatcher already gave to a fresher
		// seat. Certify against the canonical store before promising it.
		if certifyHookAdoption(bead, *opts, *ops, dir, stderr) != hookAdoptionRefused {
			// minted=false: adoption returns work this session already owned.
			return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, bead, *opts, *ops, dir, false, stdout, stderr)}
		}
		// Refused: fall through to the claim tiers. The seat is healthy — its
		// cache was not — and neither tier can match this row anyway (ready
		// requires open, eligible requires an empty assignee), so this ends in
		// the shared drain unless there is other work to do.
	}

	readyResult := claimFirstReadyHookAssignment(candidates, *opts, *ops, dir, stdout, stderr)
	if readyResult.terminal {
		return readyResult
	}
	eligibleResult := claimFirstEligibleHookCandidate(candidates, *opts, *ops, dir, stdout, stderr)
	// A skipped assigned-tier claim error must survive the handoff to the routed
	// tier: both tiers feed ONE shared drain, and dropping the flag here would
	// launder an assigned-tier write failure into a healthy no_work.
	if !eligibleResult.terminal && readyResult.claimsErrored {
		eligibleResult.claimsErrored = true
	}
	return eligibleResult
}

// applyDefaults fills any unset op seam with its production implementation, so
// callers (and tests) only override the seams they care about. Runner has no
// default — a missing work-query runner is a caller error handled in doHookClaim.
func (ops *hookClaimOps) applyDefaults() {
	if ops.Claim == nil {
		ops.Claim = hookClaimWithBdStore
	}
	if ops.ListContinuation == nil {
		ops.ListContinuation = hookListContinuationWithBdStore
	}
	if ops.AssignContinuation == nil {
		ops.AssignContinuation = hookAssignContinuationWithBdStore
	}
	if ops.DrainAck == nil {
		ops.DrainAck = hookRuntimeDrainAck
	}
	if ops.DrainPending == nil {
		ops.DrainPending = hookSessionDrainPending
	}
	if ops.EmitClaimRejected == nil {
		ops.EmitClaimRejected = hookEmitClaimRejected
	}
	if ops.ResolveWorkBranch == nil {
		ops.ResolveWorkBranch = hookResolveWorkBranch
	}
	if ops.StampWorkMeta == nil {
		ops.StampWorkMeta = hookStampWorkMetaWithBdStore
	}
	if ops.StampSessionClaim == nil {
		ops.StampSessionClaim = hookStampSessionCurrentClaim
	}
	if ops.PublishRunMap == nil {
		ops.PublishRunMap = writeRunMap
	}
	if ops.ReadWorkMeta == nil {
		ops.ReadWorkMeta = hookReadClaimedBeadWithBdStore
	}
	if ops.EmitExecutionStepStarted == nil {
		ops.EmitExecutionStepStarted = hookEmitExecutionStepStarted
	}
	if ops.Release == nil {
		ops.Release = hookClaimReleaseWithBdStore
	}
	if ops.EmitClaimWindowExpired == nil {
		ops.EmitClaimWindowExpired = hookEmitClaimWindowExpired
	}
	if ops.EmitClaimReleased == nil {
		ops.EmitClaimReleased = hookEmitClaimReleased
	}
	if ops.Now == nil {
		ops.Now = time.Now
	}
	// Stamped once per invocation and never refreshed: every federated leg the
	// claim loop tries shares the window the FIRST one opened, which is what
	// makes the fence bound the whole command rather than each attempt.
	if ops.InvokedAt.IsZero() {
		ops.InvokedAt = ops.Now()
	}
	if ops.ClaimWindow <= 0 {
		ops.ClaimWindow = resolveHookClaimWindow()
	}
}

// claimWindowSpent reports whether this invocation's claim window has elapsed.
func (ops *hookClaimOps) claimWindowSpent() bool {
	return ops.invocationAge() > ops.claimWindowOrDefault()
}

// invocationAge is how long this `gc hook --claim` invocation has been running.
//
// A zero InvokedAt means no invocation window was ever opened, which happens
// only for a caller driving a claim tier directly rather than through
// doHookClaim / claimHookWorkWithRunner (both of which stamp it in
// applyDefaults). Such a caller has no turn for the claim to outlive, so it
// reports age zero and the fence never fires — fail-open by construction, not
// by accident.
func (ops *hookClaimOps) invocationAge() time.Duration {
	if ops.InvokedAt.IsZero() {
		return 0
	}
	return ops.nowOrWallClock().Sub(ops.InvokedAt)
}

// nowOrWallClock is ops.Now with its production default applied inline, so a
// direct-seam caller that never ran applyDefaults cannot nil-panic the fence.
func (ops *hookClaimOps) nowOrWallClock() time.Time {
	if ops.Now != nil {
		return ops.Now()
	}
	return time.Now()
}

// claimWindowOrDefault is ops.ClaimWindow with its default applied inline, for
// the same reason nowOrWallClock exists.
func (ops *hookClaimOps) claimWindowOrDefault() time.Duration {
	if ops.ClaimWindow > 0 {
		return ops.ClaimWindow
	}
	return resolveHookClaimWindow()
}

// claimMutationContext bounds a claim-write child by whichever is sooner: the
// flat mutation timeout, or what remains of the claim window.
//
// Bounding by the window is half of F-B. The CAS runs in a bd child with its own
// 120s ceiling (bdCommandTimeout), so without this a claim started at the last
// second of the window keeps writing long past the fence — and a claim that
// lands late is exactly the parked claim the fence exists to prevent.
func (ops *hookClaimOps) claimMutationContext() (context.Context, context.CancelFunc) {
	budget := hookClaimMutationTimeout
	if remaining := ops.claimWindowOrDefault() - ops.invocationAge(); remaining < budget {
		budget = remaining
	}
	if budget <= 0 {
		// Already spent. The tier's own fence refuses before using this, but an
		// already-expired context keeps the contract honest for any path that
		// does not.
		budget = time.Nanosecond
	}
	return context.WithTimeout(context.Background(), budget)
}

// refuseExpiredHookClaimWindow reports the spent-window refusal and returns the
// terminal result for it: exit 1, no claim, and deliberately NO drain record.
//
// A spent window is not an idle store. Writing a no-work drain here would tell
// the caller the store was empty — the same laundering that makes a killed claim
// command indistinguishable from a clean drain, which is the confusion this whole
// fence exists to end. The read-error arm refuses for the same reason.
func refuseExpiredHookClaimWindow(candidateID string, ops hookClaimOps, stderr io.Writer) hookClaimResult {
	age := ops.invocationAge()
	parentAlive := os.Getppid() != 1
	// The typed event is the durable record and the stderr line is commentary on
	// it, so the event goes first — same rule as the unwind above, for the same
	// reason: this path can be reached with a closed stderr.
	ops.EmitClaimWindowExpired(hookClaimWindowExpiry{
		BeadID:        candidateID,
		InvocationAge: age,
		ParentAlive:   parentAlive,
	})
	_, _ = fmt.Fprintf(stderr,
		"gc hook --claim: refusing to claim %s: the %s claim window is spent (invocation age %s, parent alive %t); the turn that invoked this claim is gone\n",
		candidateID, ops.claimWindowOrDefault(), age.Round(time.Millisecond), parentAlive)
	return hookClaimResult{terminal: true, code: 1}
}

// claimFirstReadyHookAssignment atomically promotes the first open candidate
// already assigned to this session. Continuation preassignment deliberately
// leaves later group members open, so a resumed session must still run the
// store's idempotent claim mutation before it reports the bead as workable.
//
// A candidate whose claim errors because THIS store cannot resolve the id is
// skipped rather than fatal (see hookClaimBeadIsElsewhere), so the federated
// caller can try the store that actually holds it; the returned result's
// claimsErrored flag carries the skip to the shared drain. Every other claim
// error still fails closed: ownership is unresolved on a bead this session
// already owns, and claiming unrelated fresh work would strand it.
func claimFirstReadyHookAssignment(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) hookClaimResult {
	ctx, cancel := ops.claimMutationContext()
	defer cancel()
	claimsErrored := false
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" ||
			hookClaimCandidateIsMessage(candidate) ||
			!strings.EqualFold(strings.TrimSpace(candidate.Status), "open") ||
			!hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			continue
		}
		// F-B. Promoting a ready assignment is a status CAS — a mutation — so it
		// is fenced like a fresh claim. Adoption of an ALREADY in_progress bead
		// runs earlier, in hookClaimExistingAssignment, and is deliberately
		// exempt: it mints no new obligation.
		if ops.claimWindowSpent() {
			return refuseExpiredHookClaimWindow(candidate.ID, ops, stderr)
		}
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "gc hook --claim: ready assignment %s claim deadline exhausted: %v\n", candidate.ID, ctx.Err()) //nolint:errcheck
			return hookClaimResult{terminal: true, code: 1}
		}
		// Use the bead's current own-identity assignee as the claim actor.
		// BEADS_ACTOR may be represented by the runtime name, session bead id,
		// or alias; bd's idempotent --claim path requires the actor to match the
		// existing assignee exactly.
		claimActor := strings.TrimSpace(candidate.Assignee)
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, claimActor)
		if err != nil {
			if !ok && (hookClaimBeadIsElsewhere(err) || hookClaimBindingRefusedTheClaim(err)) {
				// The read federated and the write did not: the assigned tier
				// reads city-wide, so a graph step in a relocated class store
				// arrives here while the claim runs against this store's bd
				// context, which cannot resolve it. That is not an unresolved
				// mutation on a bead we own here — this store holds no such bead —
				// so skip it and let the federated caller try the store that does.
				//
				// A binding that refuses the claim CAS outright carries the same
				// proof and is skipped for the same reason: the escalation only
				// ran because a work store returned not-found, and the refusal
				// lands before any write (hookClaimBindingRefusedTheClaim). One
				// bead no store can claim must not stop this session claiming
				// the work that other stores can.
				fmt.Fprintf(stderr, "gc hook --claim: skipping ready assignment %s: %v\n", candidate.ID, err) //nolint:errcheck
				claimsErrored = true
				continue
			}
			if ok {
				fmt.Fprintf(stderr, "gc hook --claim: claimed %s but loading canonical bead failed: %v\n", candidate.ID, err) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc hook --claim: promoting ready assignment %s: %v\n", candidate.ID, err) //nolint:errcheck
			}
			// This session already owns the bead. Do not skip it and claim
			// unrelated fresh work after an operational mutation failure.
			return hookClaimResult{terminal: true, code: 1}
		}
		// Deliberately unlike the err != nil branch above: a rejected claim is a
		// lost race, not an operational failure. Another claimant genuinely owns
		// the bead, so ownership is resolved and this session is free to fall
		// through to other routed work. A mutation failure leaves ownership
		// unresolved, so that branch fails closed instead.
		if !ok {
			reportHookClaimRejected(candidate, claimed, opts, ops)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") ||
			strings.TrimSpace(claimed.Assignee) != claimActor {
			_, _ = fmt.Fprintf(
				stderr,
				"gc hook --claim: ready assignment %s claim readback remained status=%q assignee=%q; want in_progress owned by this session\n",
				candidate.ID,
				claimed.Status,
				claimed.Assignee,
			)
			return hookClaimResult{terminal: true, code: 1}
		}
		claimed = mergeHookClaimCandidateMetadata(candidate, claimed)
		result := hookClaimJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       hookClaimCommandName,
			Action:        "work",
			Reason:        "ready_assignment",
			BeadID:        claimed.ID,
			Assignee:      claimed.Assignee,
			Route:         hookClaimRoute(claimed),
		}
		if result.BeadID == "" {
			result.BeadID = candidate.ID
		}
		if result.Assignee == "" {
			result.Assignee = claimActor
		}
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, true, stdout, stderr)}
	}
	return hookClaimResult{claimsErrored: claimsErrored}
}

// hookClaimBeadIsElsewhere reports whether a failed claim proves the bead is not
// in the store this claim ran against, rather than that the write itself failed.
//
// It exists because the assigned-ready work-query tier reads city-wide on a
// split city (`gc ready --assignee`) while the claim still runs against the
// agent's work-directory bd context, so a graph step in a relocated class store
// is visible-but-unclaimable until the claim is class-routed (ga-601v2).
// beads.ErrNotFound is the ONLY error that carries that proof: BdStore.Claim
// wraps it when bd reports no such issue, and every other failure — a write
// timeout, store contention, a controller-socket flap — leaves ownership
// unresolved and must keep failing closed.
func hookClaimBeadIsElsewhere(err error) bool {
	return errors.Is(err, beads.ErrNotFound)
}

// claimFirstEligibleHookCandidate claims the first unassigned, route-matched
// candidate and returns a terminal result carrying the exit code of the
// work-result write. A claim lost to a different live claimant is surfaced as a
// bead.claim_rejected event before moving on. A candidate whose claim mutation
// errors is logged and skipped so one unclaimable id cannot wedge the hook. When
// no candidate can be claimed — none match this session, every claimable one was
// lost to another claimant, or every claimable one errored — it returns a
// non-terminal result (no output written) so a federated caller can try a later
// store before the shared no-work drain; the result's claimsErrored flag records
// whether any skip was an error so that drain stays distinguishable from idle.
func claimFirstEligibleHookCandidate(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) hookClaimResult {
	ctx, cancel := ops.claimMutationContext()
	defer cancel()
	claimsErrored := false
	for _, candidate := range candidates {
		if !hookCandidateClaimable(candidate, opts.RouteTargets) {
			continue
		}
		// F-B. The fresh-claim CAS is the mutation that mints a new obligation,
		// so it is the one the turn-binding window most directly guards.
		if ops.claimWindowSpent() {
			return refuseExpiredHookClaimWindow(candidate.ID, ops, stderr)
		}
		if ctx.Err() != nil {
			// The shared claim budget is spent (an earlier slow-failing claim
			// consumed it). Stop rather than attempting the remaining candidates
			// with an already-expired context, which would only manufacture
			// deadline-exceeded skips on ids never really tried; they are reclaimed
			// next tick (NDI).
			break
		}
		claimed, ok, err := ops.Claim(ctx, dir, opts.Env, candidate.ID, opts.Assignee)
		if err != nil {
			if ok {
				// The atomic mutation committed, but its canonical readback failed.
				// Stop immediately: trying another candidate or draining would strand
				// the assignment while falsely reporting idle work.
				fmt.Fprintf(stderr, "gc hook --claim: claimed %s but loading canonical bead failed: %v\n", candidate.ID, err) //nolint:errcheck
				return hookClaimResult{terminal: true, code: 1}
			}
			// A single unclaimable candidate (a routed id whose bead was deleted,
			// one that no longer resolves in the store this context can reach, or a
			// transient write failure) must not wedge the whole hook. Record it and
			// try the next candidate. If none claim, claimsErrored makes the shared
			// drain report claims_errored instead of a healthy no_work so the write
			// failure stays visible; the work is reclaimed next tick (NDI) either way.
			fmt.Fprintf(stderr, "gc hook --claim: skipping %s: %v\n", candidate.ID, err) //nolint:errcheck
			claimsErrored = true
			continue
		}
		if !ok {
			reportHookClaimRejected(candidate, claimed, opts, ops)
			continue
		}
		// Ownership readback, the same invariant the ready tier asserts one
		// function up. The guarantee already exists inside every production op
		// (hookClaimThroughStore screens both the mutation projection and the
		// canonical re-read), so this costs a healthy claim nothing — but it is
		// the tier that MINTS the receipt, and a receipt is a promise that the
		// session reading it owns the bead. Leaving the only check in the op
		// meant a new op, or a regression in the shared one, would ship a
		// receipt naming another seat's work. Ownership disputes are this
		// program's recurring bug class; fail closed rather than drain, so a
		// store that disagrees about ownership is never laundered into no_work.
		//
		// Status is deliberately NOT asserted here. The fresh-claim CAS's status
		// transition is the store op's contract, and the hazard the specimen
		// exhibited was ownership: a seat executing a step recorded under a
		// different seat.
		if !hookClaimHasIdentity(claimed.Assignee, opts.IdentityCandidates) {
			_, _ = fmt.Fprintf(
				stderr,
				"gc hook --claim: %s claim readback returned assignee=%q; want this session (%s)\n",
				candidate.ID,
				claimed.Assignee,
				opts.Assignee,
			)
			return hookClaimResult{terminal: true, code: 1}
		}
		claimed = mergeHookClaimCandidateMetadata(candidate, claimed)
		result := hookClaimJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       hookClaimCommandName,
			Action:        "work",
			Reason:        "claimed",
			BeadID:        claimed.ID,
			Assignee:      claimed.Assignee,
			Route:         hookClaimRoute(claimed),
		}
		if result.BeadID == "" {
			result.BeadID = candidate.ID
		}
		if result.Assignee == "" {
			result.Assignee = opts.Assignee
		}
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, true, stdout, stderr)}
	}

	return hookClaimResult{claimsErrored: claimsErrored}
}

// mergeHookClaimCandidateMetadata retains work-query metadata when bd update
// --claim returns only a partial projection, while preferring canonical values
// returned by the mutation.
func mergeHookClaimCandidateMetadata(candidate, claimed beads.Bead) beads.Bead {
	if len(candidate.Metadata) == 0 {
		return claimed
	}
	metadata := maps.Clone(candidate.Metadata)
	maps.Copy(metadata, claimed.Metadata)
	claimed.Metadata = metadata
	return claimed
}

// hookCandidateClaimable reports whether a work-query candidate is eligible for a
// fresh claim: it has an id, is currently unassigned, and matches one of this
// session's route targets.
func hookCandidateClaimable(candidate beads.Bead, routeTargets []string) bool {
	return strings.TrimSpace(candidate.ID) != "" &&
		strings.TrimSpace(candidate.Assignee) == "" &&
		hookClaimMatchesRoute(candidate, routeTargets)
}

// reportHookClaimRejected publishes a bead.claim_rejected event (ADR-0009) when a
// claim was lost to a *different* live claimant. An empty or own-identity assignee
// means the winner is unknown or is us, so there is no rejection to report.
func reportHookClaimRejected(candidate, claimed beads.Bead, opts hookClaimOptions, ops hookClaimOps) {
	existing := strings.TrimSpace(claimed.Assignee)
	if existing == "" || hookClaimHasIdentity(claimed.Assignee, opts.IdentityCandidates) {
		return
	}
	ops.EmitClaimRejected(candidate.ID, existing, opts.Assignee)
}

// hookAdoptionVerdict is what a canonical ownership readback said about a bead
// the work query offered up for adoption.
type hookAdoptionVerdict int

const (
	// hookAdoptionOwned: the canonical store agrees this session holds it.
	hookAdoptionOwned hookAdoptionVerdict = iota
	// hookAdoptionUnverified: the readback could not be MADE. Not evidence of
	// foreign ownership, so the bead is still served.
	hookAdoptionUnverified
	// hookAdoptionRefused: the canonical store names a different owner. The
	// receipt is withheld.
	hookAdoptionRefused
)

// certifyHookAdoption checks a bead the work query says this session already
// holds against the canonical store before an existing_assignment receipt
// promises it.
//
// The two failure arms are deliberately different, and the difference is the
// whole design. A MISMATCH is positive evidence that someone else owns the bead
// — the stale-cache shape that re-serves a re-slung step to the seat that lost
// it — and it is refused. An unreadable readback is evidence of nothing; the
// stamp path already treats that case as "proceed, but emit no durable
// lifecycle record", and failing closed here would idle every seat behind one
// store hiccup, the same trade the F-D probe makes for the same reason.
func certifyHookAdoption(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) hookAdoptionVerdict {
	beadID := strings.TrimSpace(bead.ID)
	if beadID == "" || ops.ReadWorkMeta == nil {
		return hookAdoptionUnverified
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	canonical, err := ops.ReadWorkMeta(ctx, dir, opts.Env, beadID, opts.Assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: adopting %s without a canonical ownership readback: %v\n", beadID, err) //nolint:errcheck
		return hookAdoptionUnverified
	}
	if !hookClaimHasIdentity(canonical.Assignee, opts.IdentityCandidates) {
		_, _ = fmt.Fprintf(stderr,
			"gc hook --claim: refusing to re-serve %s: the canonical store records assignee=%q, not this session (%s)\n",
			beadID, strings.TrimSpace(canonical.Assignee), opts.Assignee)
		return hookAdoptionRefused
	}
	return hookAdoptionOwned
}

func hookClaimExistingAssignment(candidates []beads.Bead, opts hookClaimOptions) (hookClaimJSONResult, beads.Bead, bool) {
	for _, candidate := range candidates {
		if hookClaimCandidateIsMessage(candidate) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Status), "in_progress") &&
			hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			result := hookClaimJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Command:       hookClaimCommandName,
				Action:        "work",
				Reason:        "existing_assignment",
				BeadID:        candidate.ID,
				Assignee:      candidate.Assignee,
				Route:         hookClaimRoute(candidate),
			}
			return result, candidate, true
		}
	}
	return hookClaimJSONResult{}, beads.Bead{}, false
}

// hookClaimCandidateIsMessage reports whether candidate is a mail message
// bead (issue_type="message"). Mail is read, not claimed as work: a message
// bead addressed to this session's identity has the same
// assignee-matches-identity shape as a real existing/ready assignment, so
// without this check it was returned by the existing/ready-assignment paths as work
// ahead of any real routed work waiting in the same batch (#4419) -- not by
// race, by construction, since this function runs before
// claimFirstEligibleHookCandidate ever sees the routed candidates.
func hookClaimCandidateIsMessage(candidate beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(candidate.Type), "message")
}

// writeHookClaimWorkResultForBead stamps, correlates and reports one claimed or
// adopted bead.
//
// minted distinguishes a claim this invocation WON from one it merely adopted,
// and only a minted claim is unwound: adoption returns work the session already
// owned, so releasing it on a delivery failure would give away a claim an earlier
// turn legitimately made. A held claim that goes undelivered is re-served to the
// next turn by the existing-assignment tier instead.
func writeHookClaimWorkResultForBead(result hookClaimJSONResult, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, minted bool, stdout, stderr io.Writer) int {
	// F-B straddle. The CAS was STARTED inside the window and LANDED outside it:
	// the claim-write child carries its own ceiling, so a claim can commit after
	// the invoking turn is already gone. That is the same parked claim by another
	// route, so it takes the same unwind as an undelivered one.
	if minted && ops.claimWindowSpent() {
		cause := fmt.Sprintf("claim of %s landed after the %s claim window closed (invocation age %s); releasing it rather than parking it",
			bead.ID, ops.claimWindowOrDefault(), ops.invocationAge().Round(time.Millisecond))
		return unwindUndeliveredHookClaim(hookClaimReleaseReasonStraddled, cause, bead, opts, ops, dir, stderr)
	}
	result.RootBeadID = strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	result.ContinuationGroup = strings.TrimSpace(bead.Metadata[beadmeta.ContinuationGroupMetadataKey])
	durable, stamped := stampHookClaimIdentity(bead, opts, ops, dir, stderr)
	if stamped && hookClaimLifecycleCandidate(durable, opts) {
		ops.EmitExecutionStepStarted(durable, dir, opts.Env, opts.Assignee)
	}
	stampHookSessionCurrentClaim(bead, opts, ops, stderr)
	publishHookClaimRunMap(bead, opts, ops, stderr)
	assigned, err := preassignHookContinuationGroup(bead, opts, ops, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: preassigning continuation group for %s: %v\n", bead.ID, err) //nolint:errcheck
		return 1
	}
	result.ContinuationAssigned = assigned
	if writeErr := writeHookClaimResultLine(result, opts.JSON, stdout); writeErr != nil {
		// F-C. The claim is won but its result never left the process — the
		// orphaned tool call's signature is EPIPE on a stdout whose reader the
		// provider already closed. A closed pipe cannot deliver, so nobody will
		// execute this claim; give it back instead of parking it.
		cause := fmt.Sprintf("writing result for %s: %v", bead.ID, writeErr)
		if !minted {
			fmt.Fprintf(stderr, "gc hook --claim: %s\n", cause) //nolint:errcheck
			return 1
		}
		// stampHookSessionCurrentClaim above advertised this bead as the session's
		// current claim before the result write; a minted claim is now being given
		// back, so clear the stamp as part of the same rollback surface as
		// ops.Release. Clear BEFORE the release (matching the session_beads.go
		// cascade order) so `gc hook current` can never hand a later formula step a
		// bead this session no longer owns — the "close somebody else's bead" hazard
		// this back-channel exists to prevent. The straddle path (F-B) needs no clear
		// because it returns before the stamp.
		clearHookSessionCurrentClaim(opts, ops, stderr)
		return unwindUndeliveredHookClaim(hookClaimReleaseReasonUndelivered, cause, bead, opts, ops, dir, stderr)
	}
	return 0
}

// writeHookClaimResultLine writes the one line that carries a claim result to
// its consumer, and — unlike the plain-text path it replaces — reports whether
// that write actually landed. The non-JSON form used to discard the error, which
// is precisely the shape a dead tool pipe takes.
func writeHookClaimResultLine(result hookClaimJSONResult, jsonOut bool, stdout io.Writer) error {
	if jsonOut {
		return writeCLIJSONLine(stdout, result)
	}
	_, err := fmt.Fprintln(stdout, result.BeadID)
	return err
}

// unwindUndeliveredHookClaim gives back a claim this invocation won but could
// not hand to a live consumer, and returns the terminal exit code (always 1 —
// the caller asked for work and is getting none).
//
// The release is compare-and-swap on the assignee through the same ops seam the
// claim ran against, so it reaches the class binding on a split city exactly
// where the claim landed, and a bead that legitimately changed hands in the
// meantime is left alone. A release that fails or finds the bead already moved is
// surfaced, never swallowed: the claim is then still parked and the operator must
// be able to see the one residue this fence could not clear.
//
// Known residue: a claim carrying a continuation group has already preassigned
// its open siblings by the time the result write fails (the assigned ids are part
// of the result payload, so they cannot be computed after it). Those siblings
// stay open and assigned, which is the dead-assignee release lane's shape, and
// the next turn of the same session re-claims them.
func unwindUndeliveredHookClaim(reason, cause string, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) int {
	assignee := strings.TrimSpace(bead.Assignee)
	if assignee == "" {
		assignee = opts.Assignee
	}
	// This emits bead.claim_released for a bead that may ALREADY have an
	// execution.step_started from this same invocation (the stamp runs before the
	// result write). That pair is the compensation record: the step never
	// executed, and a consumer reading the lifecycle as monotonic would otherwise
	// leave it in flight forever. See the BeadClaimReleased constant.
	//
	// RELEASE FIRST, DIAGNOSE SECOND, and the order is load-bearing.
	//
	// This path runs precisely when a descriptor turned out to be unwritable, and
	// stderr can be closed for the same reason stdout was. gc ignores SIGPIPE at
	// startup so such a write returns EPIPE instead of killing the process
	// (ignoreSIGPIPE) — but the release is the compensating action and the
	// diagnostic is only commentary on it, so the compensation must never sit
	// behind a write that can fail. If ignoreSIGPIPE ever regresses, this
	// ordering still gets the claim back.
	//
	// Deliberately NOT the window-bounded context: in the straddle case the
	// window is already spent, and the unwind must still be allowed to run.
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	released, err := ops.Release(ctx, dir, opts.Env, bead.ID, assignee)
	if released && err == nil {
		ops.EmitClaimReleased(hookClaimReleaseRecord{BeadID: bead.ID, Assignee: assignee, Reason: reason})
	}
	fmt.Fprintf(stderr, "gc hook --claim: %s\n", cause) //nolint:errcheck
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "gc hook --claim: releasing undelivered claim %s: %v\n", bead.ID, err) //nolint:errcheck
	case !released:
		fmt.Fprintf(stderr, "gc hook --claim: undelivered claim %s was no longer ours to release\n", bead.ID) //nolint:errcheck
	}
	return 1
}

// writeHookClaimNoWork writes the single drain result for a hook that claimed
// nothing. The reason is "no_work" for a genuinely idle store; it is
// "claims_errored" when claimsErrored is set — ready work existed but every
// eligible claim mutation errored — so an operational write failure stays
// distinguishable from idle even though both still drain and reclaim next tick.
//
// dir is the store context the diagnostics classification reads through; it is
// used ONLY after the drain has been written. See recordDemandClaimDivergence:
// a demand-spawned seat draining empty is either correct pull or a broken
// agreement invariant, and the drain itself cannot tell an operator which.
func writeHookClaimNoWork(opts hookClaimOptions, ops hookClaimOps, claimsErrored bool, dir string, stdout, stderr io.Writer) int {
	reason := hookClaimReasonNoWork
	if claimsErrored {
		reason = hookClaimReasonClaimsErrored
	}
	code := writeHookClaimDrain(hookClaimLabel, reason, opts.JSON, opts.DrainAck, ops.DrainAck, stdout, stderr)
	// Strictly after the result: the drain is already written and its exit code
	// is already decided, so nothing below can influence either.
	if reason == hookClaimReasonNoWork {
		hookRecordDemandClaimDivergence(reason, dir, opts, ops, stderr)
	}
	return code
}

// writeHookClaimNonTurnDrain emits the terminal result for a claim refused
// because it was invoked from a provider callback lane rather than an agent turn
// (F-A). marker names the environment marker that proved it.
//
// It deliberately does NOT take the shared writeHookClaimDrain exit contract:
// --drain-ack is never consumed (a callback must not acknowledge the session's
// drain on its behalf) and the exit code is 0 regardless, so a provider does not
// retry the refusal on every prompt submit. Only a failed JSON write is an error.
func writeHookClaimNonTurnDrain(marker string, opts hookClaimOptions, stdout, stderr io.Writer) int {
	_, _ = fmt.Fprintf(stderr,
		"gc hook --claim: refusing to claim from a non-turn context (%s is set); a provider callback's result reaches no agent turn, so a claim minted here would be parked the instant it is won\n",
		marker)
	if !opts.JSON {
		return 0
	}
	if err := writeCLIJSONLine(stdout, hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        hookClaimReasonNonTurnContext,
	}); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

// writeHookClaimDrainPending emits the terminal result for a claim refused
// because this seat's session row is already draining (F-D).
//
// It takes the SHARED writeHookClaimDrain contract rather than F-A's: --drain-ack
// is consumed, and the exit code is 0 once it is. A callback must not
// acknowledge a drain on the session's behalf (F-A), but a TURN acknowledging
// its own session's drain is precisely the outcome this refusal exists to
// produce — the seat is being told to stop, and the drain-ack is how it says it
// heard.
//
// The stderr line names the EXPLICIT-argument ack command. `gc runtime drain-ack`
// with no argument binds through the caller's own GC_SESSION_ID/GC_INSTANCE_TOKEN,
// and an adopted pane whose environment did not survive a restart acks as nobody
// — which reads downstream as no acknowledgement at all. Naming the id lets the
// agent run the form that resolves the target from the store instead.
// label names the door the refusal came through ("gc hook --claim" or
// "gc hook"), because both are fenced and an operator reading a pane needs to
// know which one answered. The JSON record is identical either way: the command
// is "hook" for both, and a consumer should not have to care.
func writeHookClaimDrainPending(label, sessionID string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	_, _ = fmt.Fprintf(stderr,
		"%s: drain pending for this session; run: gc runtime drain-ack %s — then exit\n",
		label, sessionID)

	return writeHookClaimDrain(label, hookClaimReasonDrainPending, opts.JSON, opts.DrainAck, ops.DrainAck, stdout, stderr)
}

// writeHookClaimStaleSessionDrain emits the terminal result for a refused stale
// session (closed, superseded instance token, or a dormant/terminal state) that
// must stop instead of claiming. It preserves the gc hook --claim result
// contract: a --json caller gets a schema-backed drain record (action "drain",
// reason "stale_session"), and --drain-ack is honored, so a startup wrapper
// acknowledges drain and exits cleanly rather than seeing a bare exit 1 and
// retrying the refusal forever.
func writeHookClaimStaleSessionDrain(opts hookCommandOptions, stdout, stderr io.Writer) int {
	return writeHookClaimDrain(hookClaimLabel, hookClaimReasonStaleSession, opts.JSON, opts.DrainAck, hookRuntimeDrainAck, stdout, stderr)
}

// writeHookClaimDrain writes the single structured drain result shared by every
// terminal no-claim outcome: an idle no-work store, a claims-errored store, and a
// refused stale session. For a --json caller it emits the schema-backed drain
// line; when drainAck is set it first runs drainAckFn and marks the result
// acknowledged. The exit code mirrors the historical contract — 0 once drain is
// acknowledged, else 1 — so a non-drain-ack caller still reports action=drain
// (a completed drain) rather than a bare failure.
//
// label names the door that answered ("gc hook --claim" or "gc hook"). Every
// caller but the drain-pending fence is claim-only, but that fence is reachable
// through the DISCOVERY door too, and a hardcoded prefix would report the wrong
// command to the operator reading the pane.
func writeHookClaimDrain(label, reason string, jsonOut, drainAck bool, drainAckFn hookDrainAckFunc, stdout, stderr io.Writer) int {
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        reason,
	}
	// A FAILED ack no longer swallows the drain record.
	//
	// Returning here before the JSON write handed a --json caller no action at
	// all — the bare-exit-1 shape a startup wrapper retries forever, on exactly
	// the seat that is trying to leave. The exit code still reports that nothing
	// was acknowledged; the record still reports that the answer was "drain".
	// Those are different facts and the consumer needs both.
	ackFailed := false
	if drainAck {
		if err := drainAckFn(stderr); err != nil {
			fmt.Fprintf(stderr, "%s: drain-ack failed: %v\n", label, err) //nolint:errcheck
			ackFailed = true
		} else {
			result.DrainAcknowledged = true
		}
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "%s: writing JSON: %v\n", label, err) //nolint:errcheck
			return 1
		}
	}
	if drainAck && !ackFailed {
		return 0
	}
	return 1
}

func preassignHookContinuationGroup(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string) ([]string, error) {
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	group := strings.TrimSpace(bead.Metadata[beadmeta.ContinuationGroupMetadataKey])
	if rootID == "" || group == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	siblings, err := ops.ListContinuation(ctx, dir, opts.Env, rootID, group)
	if err != nil {
		return nil, err
	}
	pinAssignee := continuationPinAssignee(opts)
	assigned := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		if strings.TrimSpace(sibling.ID) == "" ||
			sibling.ID == bead.ID ||
			strings.TrimSpace(sibling.Assignee) != "" ||
			!strings.EqualFold(strings.TrimSpace(sibling.Status), "open") ||
			!hookClaimMatchesRoute(sibling, opts.RouteTargets) {
			continue
		}
		if err := ops.AssignContinuation(ctx, dir, opts.Env, sibling.ID, pinAssignee); err != nil {
			return assigned, fmt.Errorf("assigning %s: %w", sibling.ID, err)
		}
		assigned = append(assigned, sibling.ID)
	}
	return assigned, nil
}

func hookClaimWithBdStore(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
	store := hookClaimBdStoreContext(ctx, dir, env, assignee)
	return hookClaimThroughStore(beadID, assignee,
		func() (beads.Bead, bool, error) { return store.Claim(beadID) },
		store.Get)
}

// hookClaimThroughStore is the post-mutation classification shared by every
// store a claim can run against: the work-directory bd context here, and the
// relocated class binding in claim_class_route.go.
//
// It is factored out rather than duplicated because the classification IS the
// contract the caller reads — a lost race must be reported as a rejection and
// not as an error, a stale projection must not be treated as ours, and a failed
// canonical readback must be surfaced with ok=true so the caller stops instead
// of draining. Two copies of that would be two chances to disagree, and the
// paths that disagree about ownership are this program's recurring bug class.
//
// claim and get are the store's own operations: the bd context's Claim takes the
// assignee implicitly (BEADS_ACTOR in the subprocess env) while the class
// binding's takes it explicitly, so the caller binds them and this function
// never has to know which shape it is holding.
func hookClaimThroughStore(beadID, assignee string, claim func() (beads.Bead, bool, error), get func(string) (beads.Bead, error)) (beads.Bead, bool, error) {
	claimed, ok, err := claim()
	if err != nil {
		return beads.Bead{}, false, err
	}
	if !ok {
		// Claim conflict: re-read the bead so the caller can surface who won
		// the race in the bead.claim_rejected event (ADR-0009). Best-effort —
		// a read error degrades to a silent no-op (empty bead, no event).
		current, getErr := get(beadID)
		if getErr != nil {
			return beads.Bead{}, false, nil
		}
		return current, false, nil
	}
	if !hookClaimHasIdentity(claimed.Assignee, []string{assignee}) {
		// The store reported a successful mutation but the bead is owned by
		// another claimant (stale projection / lost race). Return it as a
		// non-claim so the caller can report the rejection rather than treat it
		// as ours.
		return claimed, false, nil
	}
	canonical, err := get(beadID)
	if err != nil {
		return claimed, true, fmt.Errorf("reloading claimed bead %q: %w", beadID, err)
	}
	if !hookClaimHasIdentity(canonical.Assignee, []string{assignee}) {
		return canonical, false, nil
	}
	return canonical, true, nil
}

// stampHookClaimIdentity records the claiming worker's execution identity on the
// claimed bead in ONE metadata write: gc.work_branch (the durable handle from the
// bead to its work that the close gate later reads, ADR-0009) plus the durable
// session back-reference gc.session_id / gc.session_name (#2843) so the dashboard
// run-detail can resolve which session executed a pool step after the transient
// Assignee is cleared on close, plus gc.claimed_at (OBS-001), the write-once claim
// timestamp that feeds the created→claimed and claimed→started latency-watch
// transitions. graphroute leaves pool steps unbound at route time, deferring the
// session binding to this claim (graphroute.go:200-203).
//
// The patch is compare-and-skipped against the bead's current metadata and the
// write is issued only when at least one key actually changes: this runs again on
// every hook tick via the existing_assignment / ready_assignment adoption paths, so
// an unconditional write would emit a bead.updated per tick per in-progress bead
// (the cache-reconcile flood class). gc.claimed_at is the one key in this patch
// that cannot use "differs from current → overwrite" — time.Now() differs from any
// stored value on every tick by construction, so it would defeat the compare-and-
// skip guard by itself. hookClaimIdentityPatch instead treats it as write-once:
// stamped only when absent, never touched again once set. Best-effort: a missing
// repo, detached HEAD, absent session, or write error never blocks the claim.
func stampHookClaimIdentity(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) (beads.Bead, bool) {
	patch := hookClaimIdentityPatch(bead, opts, ops, dir)
	sessionID := hookClaimSessionID(opts.Env)
	needsLifecycleIdentity := sessionID != "" && !beadmeta.IsControlKind(strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]))
	if len(patch) == 0 {
		return bead, needsLifecycleIdentity && strings.EqualFold(strings.TrimSpace(bead.Status), "in_progress") &&
			strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey]) == sessionID
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.StampWorkMeta(ctx, dir, opts.Env, bead.ID, opts.Assignee, patch); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: stamping execution identity on %s: %v\n", bead.ID, err) //nolint:errcheck
		return beads.Bead{}, false
	}
	if !needsLifecycleIdentity {
		return beads.Bead{}, false
	}
	readback, err := ops.ReadWorkMeta(ctx, dir, opts.Env, bead.ID, opts.Assignee)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: reading stamped execution identity on %s: %v\n", bead.ID, err) //nolint:errcheck
		return beads.Bead{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(readback.Status), "in_progress") ||
		strings.TrimSpace(readback.Metadata[beadmeta.SessionIDMetadataKey]) != sessionID {
		return beads.Bead{}, false
	}
	return readback, true
}

// hookClaimLifecycleCandidate reports whether a bead can be a session-owned
// graph step whose started fact is safe to reconcile. EmitLifecycle performs the
// authoritative graph-root validation; this cheaper gate avoids opening the
// graph-store path for ordinary hook work.
func hookClaimLifecycleCandidate(bead beads.Bead, opts hookClaimOptions) bool {
	sessionID := hookClaimSessionID(opts.Env)
	if sessionID == "" ||
		!strings.EqualFold(strings.TrimSpace(bead.Status), "in_progress") ||
		beadmeta.IsControlKind(strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey])) ||
		strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey]) == "" ||
		strings.TrimSpace(bead.Metadata[beadmeta.StepIDMetadataKey]) == "" ||
		strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey]) != sessionID {
		return false
	}
	if sessionName := hookClaimSessionName(opts.Env); sessionName != "" &&
		strings.TrimSpace(bead.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
		return false
	}
	return true
}

// hookClaimIdentityPatch builds the compare-and-skipped claim-time metadata patch.
// It carries gc.work_branch when the worktree resolves a branch that differs from
// the bead's, and the session back-reference gc.session_id / gc.session_name when
// this is a session-run claim (GC_SESSION_ID present) of a non-control bead and the
// values differ. Session identity is stamped even when the branch is empty — a
// session with no worktree still needs its back-reference — but never on control
// beads, which stay session-free by graphroute's design
// (ApplyGraphControlRouteBinding), even when a control-dispatcher session claims one
// through this same hook path.
//
// gc.claimed_at (OBS-001) is a fourth, differently-shaped entry: unlike the three
// keys above, it is WRITE-ONCE, stamped only when absent from the bead's current
// metadata and never touched again. A naive claimed_at = now() would differ from
// the stored value on every tick by construction and defeat the compare-and-skip
// protection the rest of this function relies on (see stampHookClaimIdentity's doc
// comment on the flood-class risk). It is also unconditional across control and
// non-control beads alike: a claim timestamp answers "when was this claimed",
// which is meaningful regardless of session identity, so it is not gated on
// IsControlKind, GC_SESSION_ID, or a resolvable worktree branch the way the other
// three keys are.
//
// An empty result means every key is already current, so the caller issues no
// write.
func hookClaimIdentityPatch(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string) map[string]string {
	patch := map[string]string{}
	if branch := strings.TrimSpace(ops.ResolveWorkBranch(dir)); branch != "" &&
		strings.TrimSpace(bead.Metadata[beadmeta.WorkBranchMetadataKey]) != branch {
		patch[beadmeta.WorkBranchMetadataKey] = branch
	}
	if sessionID := hookClaimSessionID(opts.Env); sessionID != "" &&
		!beadmeta.IsControlKind(strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey])) {
		if strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey]) != sessionID {
			patch[beadmeta.SessionIDMetadataKey] = sessionID
		}
		if sessionName := hookClaimSessionName(opts.Env); sessionName != "" &&
			strings.TrimSpace(bead.Metadata[beadmeta.SessionNameMetadataKey]) != sessionName {
			patch[beadmeta.SessionNameMetadataKey] = sessionName
		}
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.ClaimedAtMetadataKey]) == "" {
		patch[beadmeta.ClaimedAtMetadataKey] = time.Now().UTC().Format(time.RFC3339)
	}
	return patch
}

func hookStampWorkMetaWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Metadata: patch})
}

func hookReadClaimedBeadWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
	return hookClaimBdStore(dir, env, assignee).Get(beadID)
}

func hookEmitExecutionStepStarted(step beads.Bead, dir string, env []string, assignee string) {
	rec := openCityRecorder(io.Discard)
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // lifecycle events are best-effort
	}
	// The hook's bd context owns both the claimed graph step and its workflow
	// root; EmitLifecycle verifies the root is graph.v2 before recording.
	_ = executionevent.EmitLifecycle(rec, hookClaimBdStore(dir, env, assignee), events.ExecutionStepStarted, step, eventActor())
}

// stampHookSessionCurrentClaim records the claimed bead id on the CLAIMING
// SESSION's own bead (beadmeta.CurrentClaimBeadIDMetadataKey), the reverse
// direction from stampHookClaimIdentity's work-bead back-reference.
//
// It exists because a claimed step id is otherwise UNREACHABLE from the step's
// own shell: GC_BEAD_ID is set only in the dispatch condition-script
// environment (internal/convergence/condition.go), and GC_TRIGGER_BEAD_ID —
// exported to demand-spawned pool seats as a pool-level spawn marker
// (build_desired_state.go) — is absent on other seats and is a presence
// signal, not a claim directive, so a formula step that must close the bead it
// is running had no reliable way to name it and silently skipped its own close
// — work that did nothing reported green. `gc hook current` reads this stamp
// back and closes that gap.
//
// Unlike the work-bead session back-reference this is stamped for CONTROL beads
// too: that exclusion exists because a control step must stay session-free by
// graphroute's design, which is a statement about the WORK bead's metadata. A
// control-dispatcher session running a control step needs to name its own bead
// exactly as much as any other worker does.
//
// Best-effort: the write is guarded and compare-and-skipped inside
// session.Store.SetCurrentClaim, and a failure is reported on stderr but never
// fails the claim. The loud refusal for a step that cannot name its bead belongs
// at the point of use, not here.
func stampHookSessionCurrentClaim(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	sessionID := hookClaimSessionID(opts.Env)
	beadID := strings.TrimSpace(bead.ID)
	if sessionID == "" || beadID == "" {
		return
	}
	if err := ops.StampSessionClaim(sessionID, beadID); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: recording current claim %s on session %s: %v\n", beadID, sessionID, err) //nolint:errcheck
	}
}

// clearHookSessionCurrentClaim removes the session-side current-claim stamp when
// a claim this invocation recorded is being given back, so a released bead is
// never left advertised as the session's current claim. It is the inverse of
// stampHookSessionCurrentClaim and deliberately routes the clear through the same
// ops.StampSessionClaim seam — an empty bead id, which session.Store.SetCurrentClaim
// treats as a clear — so it reaches the SAME relocation-aware session front door the
// stamp used. Clearing through the store-cascade helper instead
// (clearSessionCurrentClaim, sessionFrontDoor(store)) would risk missing a relocated
// session binding the stamp wrote to.
//
// Best-effort with the stamp's own error handling: a failure is reported on stderr
// but changes no exit code, because on this path the compensating action is
// ops.Release and the stamp clear is part of that same rollback surface — the caller
// clears BEFORE releasing so a freed bead is never simultaneously claimable by
// another seat and still named by this session (the ordering session_beads.go's
// cascade already relies on).
func clearHookSessionCurrentClaim(opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	sessionID := hookClaimSessionID(opts.Env)
	if sessionID == "" {
		return
	}
	if err := ops.StampSessionClaim(sessionID, ""); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: clearing current claim on session %s: %v\n", sessionID, err) //nolint:errcheck
	}
}

// publishHookClaimRunMap publishes the claimed bead's resolved run ID for the
// external proxy correlation path. It deliberately does not decorate the
// session bead: bd's fuzzy ID resolver can redirect a post-claim update to a
// prefix-colliding session if the intended session disappears concurrently.
// The run map is independent, best-effort telemetry and preserves useful
// correlation without issuing that unsafe second store mutation. (The one
// session-bead write the claim does make, stampHookSessionCurrentClaim, goes
// through the session front door's exact-id/session-bead-validated
// SetCurrentClaim, which refuses the fuzzy redirect this comment describes
// rather than risking it.)
func publishHookClaimRunMap(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	sessionBeadID := hookClaimSessionID(opts.Env)
	if sessionBeadID == "" {
		return
	}
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, sessionBeadID)
	if err := ops.PublishRunMap(runID, bead.ID,
		hookClaimEnvValue(opts.Env, "GC_SESSION_NAME"),
		sessionBeadID,
		hookClaimEnvValue(opts.Env, "BEADS_ACTOR")); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: publishing run-map for session %s: %v\n", sessionBeadID, err) //nolint:errcheck
	}
}

// hookClaimSessionID returns the session bead id (GC_SESSION_ID) from the claim
// env, the override-sanitized value the rest of the claim path uses; it is empty
// for a non-session run (cmd_hook.go blanks GC_SESSION_ID outside a session).
func hookClaimSessionID(env []string) string {
	return hookClaimEnvValue(env, "GC_SESSION_ID")
}

// hookClaimEnvValue returns the last value of key in the claim env (trimmed),
// the same KEY=VALUE scan the rest of the claim path uses.
func hookClaimEnvValue(env []string, key string) string {
	val := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			val = v
		}
	}
	return strings.TrimSpace(val)
}

// sanitizeRunMapKey maps a session key to its run-map filename stem: keep
// [A-Za-z0-9._-], replace every other rune with '_'. It is byte-identical to
// the manifold proxy's sanitizeSession (gc-manifold-proxy.go) — the
// cross-process contract: runMapFileName appends ".json" to this stem and the
// proxy opens exactly that name. The stem is intentionally lossy (distinct keys
// such as "a/b" and "a_b" share it), which is safe because the proxy resolves a
// session by a single structured key — the x-manifold-affinity gc session name
// — whose realistic collision surface is nil, not by the wider key set the
// writer also publishes.
func sanitizeRunMapKey(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// runMapFileName is the filename (no directory) a session key publishes under.
// It is the cross-process contract with the manifold proxy: the proxy reads
// sanitizeSession(affinity)+".json" (gc-manifold-proxy.go), so this MUST be
// sanitizeRunMapKey(key)+".json" byte-for-byte or the proxy's ReadFile misses
// and X-Gc-Run-Id is never stamped. The proxy resolves exactly one key per
// request — the x-manifold-affinity header, i.e. the gc session name — so the
// only collision that could clobber run attribution is two live sessions whose
// names sanitize identically, which does not happen for real structured session
// names. A consumer that looks a session up by key MUST apply this identical
// transform.
func runMapFileName(key string) string {
	return sanitizeRunMapKey(key) + ".json"
}

// runMapEntry is the session→run-id mapping payload published per session key.
// It is a cross-process contract: the external manifold proxy decodes the same
// JSON shape and consumes run_id to stamp X-Gc-Run-Id. Keep the field tags
// (run_id/bead_id/ts) in lock-step with that reader (GC_PROXY_RUNMAP_DIR in
// gc-manifold-proxy.go).
type runMapEntry struct {
	RunID  string `json:"run_id"`
	BeadID string `json:"bead_id"`
	TS     int64  `json:"ts"`
}

// runMapProxyDefaultDir is the zero-config run-map directory, kept
// byte-identical to the manifold proxy's own default (gc-manifold-proxy.go's
// runmapDir). The two sides MUST share a directory or the proxy never finds the
// mapping and X-Gc-Run-Id is never stamped. The proxy runs as root and
// provisions this path sticky 0o1777 before any agent cell starts, so a
// non-root worker's os.MkdirAll(0o755) no-ops on it and CreateTemp succeeds
// there; runMapDirSafeToPublish trusts that sticky root-owned handoff. Both
// sides override in lock-step via GC_RUNMAP_DIR / GC_PROXY_RUNMAP_DIR.
const runMapProxyDefaultDir = "/run/gc-manifold-runmap"

// defaultRunMapDir returns the zero-config run-map directory used when
// GC_RUNMAP_DIR is unset. It is the proxy-aligned default: with the proxy
// present the dir already exists sticky 0o1777 and is worker-writable; with no
// proxy present a non-root worker cannot create it, and writeRunMap treats that
// absent, uncreatable default as a silent no-proxy no-op — there is no proxy
// reading the map in that case, so nothing is lost and the hot claim path stays
// quiet. Only an explicit GC_RUNMAP_DIR that cannot be created is surfaced.
func defaultRunMapDir() string {
	return runMapProxyDefaultDir
}

// runMapDirSafeToPublish reports whether the resolved run-map dir is safe to
// publish a proxy-trusted <session>.json into. os.MkdirAll self-provisions an
// owner-only 0o755 dir but is a no-op on a pre-existing one, so an externally
// provisioned dir keeps its own mode and must be re-checked here.
//
// A dir writable by neither group nor other is always safe: owner-only (0o755),
// or a read-only shared-group dir (0o750), where non-owners cannot create or
// replace entries. Directory write permission is what lets a non-owner create,
// rename, or delete entries, so group-write is gated exactly like other-write:
// a group- or other-writable dir is trusted only as a sticky handoff owned by
// root or this user — the manifold proxy's deliberate multi-user contract, where
// root provisions /run/gc-manifold-runmap as 0o1777 so each agent cell drops its
// own <session>.json and the proxy reads them (the /tmp trust model). A
// non-sticky group- or other-writable dir, or a sticky one owned by another
// user, is refused (CWE-732).
//
// This gate bounds the DIRECTORY's provisioner; it does NOT by itself make the
// shared handoff forgery-proof. The sticky bit only stops a non-owner from
// deleting or renaming over an EXISTING file — it does not stop first-writer
// squatting of a not-yet-existing, predictable <session>.json. So in the shared
// 0o1777 handoff a hostile co-uid can pre-plant a victim's file, and per-file
// run-map authenticity is therefore the READER's responsibility: the manifold
// proxy MUST authenticate each <session>.json by owner (st_uid), mode, and link
// state before trusting run_id — a hard precondition of using a shared handoff.
// writeRunMap additionally refuses to publish over a symlink or foreign-owned
// target (see publishRunMapKey) so the writer never silently blesses a squat, but
// a world-writable handoff cannot be made forgery-proof by the writer alone.
//
// Deployment trust model (verified against the deployed gc-manifold-proxy, which
// reads <session>.json with an unauthenticated os.ReadFile and provisions the dir
// 0o1777): the handoff lives inside a single fleet uid — every agent cell writes
// as that uid and root reads — so the residual forgery is intra-trust-domain.
// Exploiting it needs an already-compromised same-uid cell, and the asset is only
// a best-effort spend-correlation header that degrades safely (a forged or missing
// mapping mis-stamps or omits X-Gc-Run-Id; it never affects code, data, or
// privilege). Because every cell shares that uid, even reader-side owner
// authentication (st_uid == fleet uid) cannot separate a genuine publish from a
// forged one; real per-session anti-forgery needs a proxy-side control the writer
// cannot supply alone — an unguessable per-cell filename/nonce or a
// root-authenticated private channel. That out-of-repo reader/deploy hardening is
// tracked in ga-zzvsuls.
func runMapDirSafeToPublish(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	// Group- and other-write are gated identically: either bit lets a non-owner
	// create, rename, or delete the <session>.json the proxy trusts, so a
	// group-writable dir is no safer than a world-writable one (CWE-732).
	if info.Mode().Perm()&0o022 == 0 {
		return true
	}
	if info.Mode()&os.ModeSticky == 0 {
		return false
	}
	return runMapDirOwnedByTrustedUser(info)
}

// writeRunMap publishes a session→run-id map file,
// ${GC_RUNMAP_DIR:-<defaultRunMapDir>}/<runMapFileName(session)> =
// {"run_id":...,"bead_id":...,"ts":...}, so an external tool can correlate a
// session's activity to the run it is working. One file is written per distinct
// non-empty session key (the session may be addressed as GC_SESSION_NAME,
// GC_SESSION_ID, or BEADS_ACTOR). Best-effort and atomic (tmp + rename): a
// per-key write failure is skipped and never blocks the claim.
//
// SECURITY CONTRACT — the run-map, and the X-Gc-Run-Id header the manifold proxy
// stamps from it, are UNAUTHENTICATED best-effort telemetry: a spend-correlation
// hint, never an authoritative signal. Downstream systems MUST NOT feed
// X-Gc-Run-Id or the run-map into billing, authorization, audit, or any other
// trust decision. The handoff is a single-fleet-uid, /tmp-style trust domain, so
// a compromised same-uid cell can pre-plant a predictable <session>.json that the
// proxy reads unauthenticated; because every cell shares the uid, neither this
// writer nor reader-side owner authentication can distinguish a forgery from a
// genuine publish (see runMapDirSafeToPublish). It degrades safely — a forged or
// missing mapping only mis-stamps or omits the header and never affects code,
// data, privilege, or routing. Real per-session anti-forgery needs a proxy-side
// nonce or private channel this writer cannot supply alone, tracked in
// ga-zzvsuls. TestRunMapEntryIsUnauthenticatedBestEffortTelemetry pins this.
//
// It returns a non-nil error whenever run attribution is compromised or the
// whole map is dropped, so the caller can surface an otherwise silent symptom:
// an unsafe directory, every attempted per-key publish failing, or a squatted
// target (a symlink or foreign-owned <session>.json the proxy would trust). A
// per-key hiccup that still leaves at least one file published is not reported —
// except a squat, which is surfaced even when other keys published, because the
// squatted session's run attribution is forged.
//
// When GC_RUNMAP_DIR is unset (zero-config default) and the default proxy dir is
// absent and uncreatable by a non-root worker, no proxy is reading the map, so
// publication is a silent no-op rather than a per-claim stderr diagnostic on the
// hottest control-plane operation.
func writeRunMap(runID, beadID string, sessionKeys ...string) error {
	dir := strings.TrimSpace(os.Getenv("GC_RUNMAP_DIR"))
	explicit := dir != ""
	if dir == "" {
		dir = defaultRunMapDir()
	}
	return writeRunMapTo(dir, explicit, runID, beadID, sessionKeys...)
}

// writeRunMapTo is writeRunMap with the directory resolution lifted out so both
// the explicit-override and zero-config-default branches are testable. explicit
// is true when the operator set GC_RUNMAP_DIR: only then is an uncreatable dir
// surfaced as an error; a zero-config default that cannot be created means no
// proxy is present and the publish is a silent no-op.
func writeRunMapTo(dir string, explicit bool, runID, beadID string, sessionKeys ...string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	// Self-provision the dir owned by this session user (0o755). This creates a
	// GC_RUNMAP_DIR override under a writable parent and no-ops on any
	// pre-existing dir — including the default /run/gc-manifold-runmap, which
	// the root proxy provisions sticky 0o1777 before workers run. A world-writable
	// dir is deliberately NOT self-provisioned here — it would let any local user
	// plant a forged <session>.json the proxy would trust — and 0o1777 could not
	// be produced anyway (os.FileMode drops the sticky bit and umask strips
	// other-write, so a self-provisioned dir is 0o755 regardless). The shared
	// multi-uid handoff is the proxy's / systemd-tmpfiles' job.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Zero-config default with no proxy present: the default dir does not
		// exist and a non-root worker cannot create it. Nothing reads the map in
		// that case, so stay silent instead of emitting an error-shaped
		// diagnostic on every claim. Surface the failure only when the operator
		// explicitly opted in via GC_RUNMAP_DIR.
		if !explicit {
			return nil
		}
		return fmt.Errorf("creating run-map dir %q: %w", dir, err)
	}
	// MkdirAll no-ops on a pre-existing dir, so gate publish on the resolved
	// dir's safety: a non-sticky group- or other-writable handoff would let any
	// local process forge or clobber run attribution.
	if !runMapDirSafeToPublish(dir) {
		return fmt.Errorf("run-map dir %q is group/other-writable without a sticky trusted-owner handoff; refusing to publish (CWE-732)", dir)
	}
	body, err := json.Marshal(runMapEntry{RunID: runID, BeadID: beadID, TS: time.Now().Unix()})
	if err != nil {
		return fmt.Errorf("marshaling run-map entry: %w", err)
	}
	seen := map[string]bool{}
	// Track publish outcomes so a dropped map or a squatted target is surfaced,
	// not silent: per-key hiccups are best-effort as long as one file lands, but
	// an all-keys-failed run returns its first failure, and a squat (symlink or
	// foreign-owned target) is surfaced even when other keys published.
	var firstErr error
	attempted, published, squats := 0, 0, 0
	for _, k := range sessionKeys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		attempted++
		ok, squat, err := publishRunMapKey(dir, k, body)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if squat {
			squats++
		}
		if ok {
			published++
		}
	}
	// Reap dead sessions' entries so a writer-owned dir doesn't leak one stale
	// file per ended session on a long-uptime box (tmpfs clears /run only on
	// reboot). pruneRunMap self-limits — it skips a shared proxy handoff and
	// bounds its scan — so this stays cheap on the claim hot path.
	pruneRunMap(dir, time.Now(), runMapTTL())
	// A squatted proxy-read target forges the session's run attribution, so it is
	// surfaced even when other keys published, never folded into best-effort nil.
	if squats > 0 {
		return firstErr
	}
	if published == 0 && attempted > 0 {
		return firstErr
	}
	return nil
}

// publishRunMapKey atomically publishes body at <dir>/<runMapFileName(key)> via a
// unique temp + rename. It returns published=true only when the file landed, and
// squat=true when the target is a pre-existing symlink or foreign-owned file — a
// run-attribution squat the proxy's ReadFile would trust — which the writer
// refuses rather than following or reporting as best-effort success. In the
// sticky handoff the writer cannot overwrite a foreign file anyway (sticky yields
// EPERM); refusing here turns a would-be silent forgery into a surfaced error.
func publishRunMapKey(dir, key string, body []byte) (published, squat bool, err error) {
	fileName := runMapFileName(key)
	finalPath := filepath.Join(dir, fileName)
	// Lstat (does NOT follow the link) before writing: a pre-planted symlink (the
	// proxy's os.ReadFile would follow it to an attacker-controlled file) or a
	// foreign-owned file at the predictable name is a squat. Refuse it — surfacing
	// a distinct error — rather than renaming over the name and reporting success.
	// This catches the documented pre-plant attack; a squat that races in after
	// this Lstat instead fails the sticky rename below and is reported per-key.
	if li, lerr := os.Lstat(finalPath); lerr == nil {
		if li.Mode()&os.ModeSymlink != 0 || !runMapExistingFileIsOurs(li) {
			return false, true, fmt.Errorf("run-map target for %q (%s) is a symlink or foreign-owned; refusing to publish (possible run-attribution squat)", key, finalPath)
		}
	}
	// Unique temp name (not a predictable "<file>.tmp"): os.CreateTemp opens
	// O_CREATE|O_EXCL on an unpredictable name, so a pre-planted symlink at the
	// temp path can't be followed on write. The "*" expands before the trailing
	// ".tmp", so any leftover still ends in ".tmp" and pruneRunMap reaps it by age.
	f, err := os.CreateTemp(dir, fileName+".*.tmp")
	if err != nil {
		return false, false, fmt.Errorf("creating run-map temp for %q: %w", key, err)
	}
	tmp := f.Name()
	_, werr := f.Write(body)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return false, false, fmt.Errorf("writing run-map for %q: %w", key, werr)
		}
		return false, false, fmt.Errorf("closing run-map for %q: %w", key, cerr)
	}
	_ = os.Chmod(tmp, 0o644)
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp)
		return false, false, fmt.Errorf("publishing run-map for %q: %w", key, err)
	}
	return true, false, nil
}

// runMapTTL bounds how long a run-map file survives without a refreshing claim
// before pruneRunMap reaps it. The file's mtime is refreshed on every claim, so
// only sessions that have STOPPED claiming go stale; the default is generous
// enough to exceed the longest a live session goes between claims (one
// long-running work bead) so a working session is never pruned out from under
// the proxy. The in-process reap only bounds a writer-owned dir; a shared
// multi-uid proxy handoff is cleaned by its provisioner (systemd-tmpfiles / the
// root proxy / tmpfs reboot), not by pruneRunMap. Overridable via GC_RUNMAP_TTL
// (Go duration).
func runMapTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("GC_RUNMAP_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 48 * time.Hour
}

// runMapPruneScanBudget caps how many directory entries a single claim-path
// prune scans and stats, so the reap cost is bounded regardless of how many
// files sit in the run-map dir. Removing reaped entries frees their slots, so a
// dir holding more than one budget of stale files drains opportunistically over
// consecutive claims rather than stalling any single claim. Sized well above the
// live-session count a single host realistically reaches.
const runMapPruneScanBudget = 256

// runMapDirPrunable reports whether the writer may safely reap stale files from
// dir on the claim hot path: true only for a dir writable by neither group nor
// other — one this user provisioned (0o755) or a read-only shared-group dir
// (0o750), where every entry is created by this uid (or root) and os.Remove can
// actually unlink it. It is false for the shared manifold-proxy handoff (a
// group- or other-writable sticky dir, canonically root-owned 0o1777): a
// non-root writer cannot unlink another uid's <session>.json there (the sticky
// bit yields EPERM), so an in-process reap is a no-op — and scanning an
// attacker-fillable directory on every claim would let a local co-tenant inflate
// claim latency by filling it (CWE-400). Cleanup of the shared handoff is the
// provisioner's job (systemd-tmpfiles / the root proxy / tmpfs reboot).
func runMapDirPrunable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o022 == 0
}

// runMapFileIsOwnedEntry reports whether the .json file at path is one this
// writer published: it decodes as a runMapEntry carrying a non-empty run_id AND
// bead_id. pruneRunMap uses it so a reap only ever unlinks the writer's own
// <session>.json files. publishHookClaimRunMap always publishes both
// fields (the resolved run id and the claimed bead id are both non-empty), so a
// genuine entry is never mistaken for foreign; an unrelated config.json an
// operator's explicit GC_RUNMAP_DIR happens to share a directory with fails to
// decode or lacks the fields and is left untouched. Published files are written
// atomically (CreateTemp + rename), so the read here always sees a complete old
// or new entry, never a partial write.
func runMapFileIsOwnedEntry(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var entry runMapEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	return strings.TrimSpace(entry.RunID) != "" && strings.TrimSpace(entry.BeadID) != ""
}

// runMapTempOrphanName reports whether name has the shape publishRunMapKey's
// os.CreateTemp produces — "<stem>.json.<random>.tmp" — so a prune reaps only
// this writer's own crash-left temp orphans, never an unrelated cache.tmp an
// explicit GC_RUNMAP_DIR happens to share a directory with. CreateTemp expands
// the "*" in runMapFileName+".*.tmp", so a genuine orphan is the writer's
// "<stem>.json" filename followed by a ".<random>.tmp" suffix.
func runMapTempOrphanName(name string) bool {
	rest, ok := strings.CutSuffix(name, ".tmp")
	if !ok {
		return false
	}
	// Drop the ".<random>" CreateTemp inserted before ".tmp"; what precedes it
	// must be the "<stem>.json" run-map filename the writer built the pattern from.
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return false
	}
	return strings.HasSuffix(rest[:i], ".json")
}

// pruneRunMap best-effort removes run-map files not refreshed within ttl — the
// files of sessions that have stopped claiming — so a writer-owned dir stays
// bounded by the live session set rather than growing one file per session ever
// seen. It also reaps crash-left temp orphans older than ttl: a live writer's
// temp exists only between CreateTemp and rename, so one that old is a dead-write
// orphan (a process killed mid-publish) the .json-only match used to leak forever.
//
// It reaps ONLY files this writer provably owns, never an unrelated file an
// operator's explicit GC_RUNMAP_DIR happens to share a directory with: a stale
// .json must decode as a runMapEntry with a non-empty run_id and bead_id
// (runMapFileIsOwnedEntry) and a stale .tmp must have the writer's
// "<stem>.json.<rand>.tmp" temp shape (runMapTempOrphanName). Without this an
// owner-only GC_RUNMAP_DIR pointed at a directory that also holds a stale
// config.json or cache.tmp would silently delete it on the claim hot path.
//
// It runs on the claim hot path, so it is deliberately self-limiting: it skips
// the shared group/other-writable proxy handoff entirely (see runMapDirPrunable)
// and, in a writer-owned dir, scans at most runMapPruneScanBudget entries per
// call so a claim's cost never scales with the directory size. Never blocks or
// fails the claim.
func pruneRunMap(dir string, now time.Time, ttl time.Duration) {
	if !runMapDirPrunable(dir) {
		return
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // read-only dir handle; close error is irrelevant
	// ReadDir(n>0) returns at most n entries from the directory stream, so the
	// scan+stat cost is capped at the budget rather than the directory size; an
	// empty dir reports io.EOF, which is not a failure.
	entries, err := f.ReadDir(runMapPruneScanBudget)
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isJSON := strings.HasSuffix(name, ".json")
		isTmp := strings.HasSuffix(name, ".tmp")
		if !isJSON && !isTmp {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Only stale files are reap candidates: a live session's .json is
		// refreshed on every claim and a live writer's temp exists for
		// microseconds, so neither is ever this old.
		if now.Sub(info.ModTime()) <= ttl {
			continue
		}
		// Reap only the writer's own files. The ownership check is last so the
		// .json read is paid only for the few stale candidates, not every entry.
		path := filepath.Join(dir, name)
		switch {
		case isTmp:
			if !runMapTempOrphanName(name) {
				continue
			}
		case isJSON:
			if !runMapFileIsOwnedEntry(path) {
				continue
			}
		}
		_ = os.Remove(path)
	}
}

// hookClaimSessionName returns the session display name (GC_SESSION_NAME) from the
// claim env — the pool slot's session/tmux name (e.g. "gc__role-mc-xxxxx") — stamped
// onto the work bead as the durable gc.session_name back-reference so the dashboard's
// byName index can resolve the step's session even when the raw id fails the
// resolver's prefix gate. Empty when the env carries no session name.
func hookClaimSessionName(env []string) string {
	sessionName := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == "GC_SESSION_NAME" {
			sessionName = v
		}
	}
	return strings.TrimSpace(sessionName)
}

// hookResolveWorkBranch returns the current git branch of dir, or "" when dir
// is not a worktree or HEAD is detached (no meaningful branch to stamp).
func hookResolveWorkBranch(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" { // detached HEAD
		return ""
	}
	return branch
}

// hookClaimReleaseWithBdStore is the unrouted release: compare-and-swap on the
// assignee through the agent's own work-directory bd context. It is the release
// dual of hookClaimWithBdStore, and claim_class_route.go wraps it for a split
// city so the release reaches the ledger the claim actually landed in.
func hookClaimReleaseWithBdStore(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
	return hookClaimBdStoreContext(ctx, dir, env, assignee).ReleaseIfCurrent(beadID, assignee)
}

// hookEmitClaimWindowExpired publishes a best-effort
// execution.claim_window_expired event so the fleet reports its own orphaned
// claimers rather than leaving the class invisible.
func hookEmitClaimWindowExpired(expiry hookClaimWindowExpiry) {
	payload, err := json.Marshal(events.ExecutionClaimWindowExpiredPayload{
		BeadID:          expiry.BeadID,
		InvocationAgeMS: expiry.InvocationAge.Milliseconds(),
		ParentAlive:     expiry.ParentAlive,
	})
	if err != nil {
		return
	}
	rec := openCityRecorder(io.Discard)
	rec.Record(events.Event{
		Type:    events.ExecutionClaimWindowExpired,
		Actor:   eventActor(),
		Subject: expiry.BeadID,
		Payload: payload,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
}

// hookEmitClaimReleased publishes a best-effort bead.claim_released event so an
// unwound claim is observable rather than looking like a claim that never
// happened.
func hookEmitClaimReleased(release hookClaimReleaseRecord) {
	payload, err := json.Marshal(events.BeadClaimReleasedPayload{
		BeadID:   release.BeadID,
		Assignee: release.Assignee,
		Reason:   release.Reason,
	})
	if err != nil {
		return
	}
	rec := openCityRecorder(io.Discard)
	rec.Record(events.Event{
		Type:    events.BeadClaimReleased,
		Actor:   release.Assignee,
		Subject: release.BeadID,
		Payload: payload,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
}

// hookEmitClaimRejected publishes a best-effort bead.claim_rejected event to the
// city event log so a lost-claim race is observable for eval/audit (ADR-0009).
func hookEmitClaimRejected(beadID, existingClaimant, attemptedClaimant string) {
	payload, err := json.Marshal(events.BeadClaimRejectedPayload{
		BeadID:            beadID,
		ExistingClaimant:  existingClaimant,
		AttemptedClaimant: attemptedClaimant,
	})
	if err != nil {
		return
	}
	rec := openCityRecorder(io.Discard)
	rec.Record(events.Event{
		Type:    events.BeadClaimRejected,
		Actor:   attemptedClaimant,
		Subject: beadID,
		Payload: payload,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}
}

func hookListContinuationWithBdStore(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
	store := hookClaimBdStore(dir, env, "")
	return store.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
		},
		TierMode: beads.TierBoth,
	})
}

func hookAssignContinuationWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Assignee: &assignee})
}

func hookRuntimeDrainAck(stderr io.Writer) error {
	if code := cmdRuntimeDrainAck(nil, false, io.Discard, stderr); code != 0 {
		return errors.New("runtime drain-ack returned non-zero")
	}
	return nil
}

func hookClaimBdStore(dir string, env []string, actor string) *beads.BdStore {
	return hookClaimBdStoreContext(context.Background(), dir, env, actor)
}

// hookClaimBdStoreContext is hookClaimBdStore with its bd commands bound to ctx,
// so a best-effort claim-time write cannot outlast the caller's deadline even if
// the underlying bd update stalls.
func hookClaimBdStoreContext(ctx context.Context, dir string, env []string, actor string) *beads.BdStore {
	return beads.NewBdStore(dir, hookClaimCommandRunnerWithEnvContext(ctx, hookClaimEnvMap(env, dir, actor)))
}

// hookClaimEnvMap projects the query environment into the exact environment the
// claim mutation runs in. Because hookClaimCommandRunnerWithEnvContext REPLACES
// the child environment rather than layering onto the parent, whatever this
// returns is all the child bd sees: a nil env yields no BEADS_DIR, leaving the
// child to fall back to cwd discovery. Production never takes that path —
// claimHookWorkWithRunner always supplies the query env or the selected store's
// env — but a caller passing nil gets cwd discovery, not the ambient selector.
func hookClaimEnvMap(env []string, dir string, actor string) map[string]string {
	env = workQueryEnvForDir(env, dir)
	out := make(map[string]string, len(env)+1)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = value
	}
	if strings.TrimSpace(actor) != "" {
		out["BEADS_ACTOR"] = actor
	}
	return out
}

// hookClaimSkip records a work_query element that parsed as JSON but could not
// be unmarshaled into a claim candidate — e.g. a bead a buggy filer wrote with
// a value whose type does not match beads.Bead (a numeric "status", say). The
// scan reports these so the caller can log and skip them instead of failing
// wholesale: one malformed bead must not halt dispatch city-wide.
type hookClaimSkip struct {
	ID  string
	Err error
}

// decodeHookClaimBeads parses work_query output into claim candidates. It is
// resilient to individual malformed beads: the array is split into raw elements
// first, then each is typed-decoded independently, so a single undecodable bead
// is collected into skipped rather than failing the whole scan. A top-level
// value that is not a JSON array still returns an error, preserving the
// "requires JSON work_query output" contract for non-JSON command output.
//
// Non-string *metadata* values are already tolerated one layer down:
// beads.Bead.Metadata is a StringMap that coerces them to their JSON text form,
// so a nested-object or boolean metadata value decodes fine and is never
// skipped. The per-element split guards the batch against type errors OUTSIDE
// metadata (e.g. a numeric "status"), which that coercion does not repair and
// which would otherwise fail the whole-slice unmarshal and drop every bead.
func decodeHookClaimBeads(output string) ([]beads.Bead, []hookClaimSkip, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil, nil
	}
	if !json.Valid([]byte(output)) {
		extracted, ok := firstHookJSONValue(output)
		if !ok {
			return nil, nil, errors.New("output is not JSON")
		}
		output = extracted
	}
	output = normalizeWorkQueryOutput(output)
	// Split into raw elements before typed decoding so one malformed bead
	// cannot fail the whole batch. json.RawMessage accepts any valid JSON
	// value, so the array split never trips on a bead that a direct
	// []beads.Bead unmarshal would reject.
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(output), &raws); err != nil {
		return nil, nil, err
	}
	candidates := make([]beads.Bead, 0, len(raws))
	var skipped []hookClaimSkip
	for _, raw := range raws {
		var bead beads.Bead
		if err := json.Unmarshal(raw, &bead); err != nil {
			skipped = append(skipped, hookClaimSkip{ID: hookClaimBeadIDForLog(raw), Err: err})
			continue
		}
		candidates = append(candidates, bead)
	}
	return candidates, skipped, nil
}

// hookClaimBeadIDForLog best-effort extracts a bead id from a raw work_query
// element for skip diagnostics. A malformed bead is typically malformed only in
// one field; its id remains a decodable string, so logging it keeps the skip
// actionable (the offending bead can be traced to fix the upstream filer).
// Returns "<unknown>" when even the id cannot be read.
func hookClaimBeadIDForLog(raw json.RawMessage) string {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if id := strings.TrimSpace(probe.ID); id != "" {
			return id
		}
	}
	return "<unknown>"
}

func firstHookJSONValue(output string) (string, bool) {
	for idx, r := range output {
		if r != '[' && r != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(output[idx:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return string(raw), true
		}
	}
	return "", false
}

func hookClaimHasIdentity(assignee string, identities []string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, identity := range identities {
		if assignee == strings.TrimSpace(identity) {
			return true
		}
	}
	return false
}

// hookRouteIdentitiesEqual reports whether two route/identity strings refer
// to the same qualified agent, tolerating the tmux-safe session-name
// encoding (/ -> --, . -> __) alongside the canonical slash-qualified form,
// and case (config-sourced and session-derived spellings of the same agent
// are not guaranteed identical case - ga-lmy6yj). This is the single
// route-spelling matcher shared by the claim path (hookClaimMatchesRoute)
// and the display path (hookCandidateVisible) per ga-1xaqgo.2 - do not fork
// a second one.
//
// This deliberately does NOT collapse the legacy bound-template spelling
// ("dir/binding.name") onto its unbound form ("dir/name"): that migration is
// owned by canonicalizeLegacyBoundUnassignedRoutedWork (build_desired_state.go),
// which rewrites the bead's persisted route as an explicit, auditable step.
// Treating the two spellings as always-already-equal here would let a claim
// bypass that migration instead of triggering it (see
// TestCanonicalizeLegacyBoundUnassignedRoutedWorkCanonicalWorkerClaims).
func hookRouteIdentitiesEqual(a, b string) bool {
	if a == b {
		return true
	}
	return strings.EqualFold(
		agent.UnsanitizeQualifiedNameFromSession(a),
		agent.UnsanitizeQualifiedNameFromSession(b),
	)
}

func hookClaimMatchesRoute(candidate beads.Bead, routeTargets []string) bool {
	if len(routeTargets) == 0 {
		return false
	}
	routedTo := strings.TrimSpace(candidate.Metadata[beadmeta.RoutedToMetadataKey])
	runTarget := strings.TrimSpace(candidate.Metadata[beadmeta.RunTargetMetadataKey])
	kind := strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey])
	for _, target := range routeTargets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if hookRouteIdentitiesEqual(routedTo, target) {
			return true
		}
		if routedTo == "" && kind == beadmeta.KindWorkflow && hookRouteIdentitiesEqual(runTarget, target) {
			return true
		}
	}
	return false
}

// hookCandidateVisible reports whether a work_query candidate should be
// shown to this identity at all. An already-assigned candidate is visible
// only when the assignee is one of this session's own identities. An
// unassigned candidate is visible when it carries no route at all (legacy
// and unrouted work is always claimable - the fail-open default the legacy
// workflow-target path depends on) or when its route matches one of
// routeTargets. This is deliberately more permissive than the claim path's
// eligibility check, which additionally requires a positive route match
// even for unrouted work; that stricter rule is correct for claiming but
// would wrongly hide legitimately unrouted display candidates (ga-1xaqgo.2).
func hookCandidateVisible(candidate beads.Bead, identities, routeTargets []string) bool {
	if assignee := strings.TrimSpace(candidate.Assignee); assignee != "" {
		return hookClaimHasIdentity(assignee, identities)
	}
	if hookClaimRoute(candidate) == "" {
		return true
	}
	return hookClaimMatchesRoute(candidate, routeTargets)
}

func hookClaimRoute(candidate beads.Bead) string {
	if routedTo := strings.TrimSpace(candidate.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		return routedTo
	}
	if strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow {
		return strings.TrimSpace(candidate.Metadata[beadmeta.RunTargetMetadataKey])
	}
	return ""
}

func hookClaimIdentityCandidates(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if legacy := hookLegacyWorkflowControlName(value); legacy != "" && !seen[legacy] {
			seen[legacy] = true
			out = append(out, legacy)
		}
	}
	return out
}

func hookClaimRouteTargets(values ...string) []string {
	return hookClaimIdentityCandidates(values...)
}

func hookLegacyWorkflowControlName(value string) string {
	value = strings.TrimSpace(value)
	const suffix = "control-dispatcher"
	if !strings.HasSuffix(value, suffix) {
		return ""
	}
	return strings.TrimSuffix(value, suffix) + "workflow-control"
}
