package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

const hookClaimCommandName = "hook"

// Drain-action reasons for the gc hook --claim result contract
// (schemas/hook/result.schema.json). Every value here is a valid reason when
// action is "drain": an idle store, an operational claim-write failure, or a
// refused stale session.
const (
	hookClaimReasonNoWork        = "no_work"
	hookClaimReasonClaimsErrored = "claims_errored"
	hookClaimReasonStaleSession  = "stale_session"
)

var hookClaimMutationTimeout = 10 * time.Second

var hookClaimCommandRunnerWithEnvContext = beads.ExecCommandRunnerWithEnvContext

type hookClaimOptions struct {
	Assignee           string
	IdentityCandidates []string
	RouteTargets       []string
	Env                []string
	DrainAck           bool
	JSON               bool
}

type hookClaimOps struct {
	Runner             WorkQueryRunner
	Claim              hookClaimFunc
	ListContinuation   hookListContinuationFunc
	AssignContinuation hookAssignContinuationFunc
	DrainAck           hookDrainAckFunc
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
	// RecordSessionPointers writes the session bead's current-pointers — gc.current_run_id
	// AND gc.active_work_bead (the claimed work bead's gc.step_id) — in ONE update, so
	// the (run, step) tuple stays atomically consistent. Best-effort.
	RecordSessionPointers hookRecordSessionPointersFunc
	Now                   func() time.Time
}

type (
	hookClaimFunc                 func(context.Context, string, []string, string, string) (beads.Bead, bool, error)
	hookListContinuationFunc      func(context.Context, string, []string, string, string) ([]beads.Bead, error)
	hookAssignContinuationFunc    func(context.Context, string, []string, string, string) error
	hookDrainAckFunc              func(io.Writer) error
	hookEmitClaimRejectedFunc     func(beadID, existingClaimant, attemptedClaimant string)
	hookResolveWorkBranchFunc     func(dir string) string
	hookStampWorkMetaFunc         func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error
	hookRecordSessionPointersFunc func(ctx context.Context, dir string, env []string, assignee, sessionBeadID, runID, stepID string) error
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
	// contention or a controller-socket flap in the read→write window) is not
	// laundered into an idle signal. Meaningless on a terminal result.
	claimsErrored bool
}

func doHookClaim(workQuery, dir string, opts hookClaimOptions, ops hookClaimOps, stdout, stderr io.Writer) int {
	res := tryHookClaim(workQuery, dir, &opts, &ops, stdout, stderr)
	if res.terminal {
		return res.code
	}
	return writeHookClaimNoWork(opts, ops, res.claimsErrored, stdout, stderr)
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
	now := time.Now
	if ops.Now != nil {
		now = ops.Now
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
	candidates, err := decodeHookClaimBeads(normalized)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: requires JSON work_query output to identify claim candidates: %v\n", err) //nolint:errcheck
		return hookClaimResult{terminal: true, code: 1}
	}
	if len(candidates) == 0 {
		return hookClaimResult{}
	}

	if result, bead, ok := hookClaimExistingOrAssigned(candidates, *opts); ok {
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, bead, *opts, *ops, dir, stdout, stderr)}
	}

	return claimFirstEligibleHookCandidate(candidates, *opts, *ops, dir, stdout, stderr)
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
	if ops.EmitClaimRejected == nil {
		ops.EmitClaimRejected = hookEmitClaimRejected
	}
	if ops.ResolveWorkBranch == nil {
		ops.ResolveWorkBranch = hookResolveWorkBranch
	}
	if ops.StampWorkMeta == nil {
		ops.StampWorkMeta = hookStampWorkMetaWithBdStore
	}
	if ops.RecordSessionPointers == nil {
		ops.RecordSessionPointers = hookRecordSessionPointersWithBdStore
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	claimsErrored := false
	for _, candidate := range candidates {
		if !hookCandidateClaimable(candidate, opts.RouteTargets) {
			continue
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
		if claimed.Metadata == nil {
			claimed.Metadata = candidate.Metadata
		}
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
		return hookClaimResult{terminal: true, code: writeHookClaimWorkResultForBead(result, claimed, opts, ops, dir, stdout, stderr)}
	}

	return hookClaimResult{claimsErrored: claimsErrored}
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

func hookClaimExistingOrAssigned(candidates []beads.Bead, opts hookClaimOptions) (hookClaimJSONResult, beads.Bead, bool) {
	for _, candidate := range candidates {
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
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.Status), "open") &&
			hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) {
			result := hookClaimJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Command:       hookClaimCommandName,
				Action:        "work",
				Reason:        "ready_assignment",
				BeadID:        candidate.ID,
				Assignee:      candidate.Assignee,
				Route:         hookClaimRoute(candidate),
			}
			return result, candidate, true
		}
	}
	return hookClaimJSONResult{}, beads.Bead{}, false
}

func writeHookClaimWorkResultForBead(result hookClaimJSONResult, bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stdout, stderr io.Writer) int {
	stampHookClaimIdentity(bead, opts, ops, dir, stderr)
	recordHookClaimSessionPointers(bead, opts, ops, dir, stderr)
	assigned, err := preassignHookContinuationGroup(bead, opts, ops, dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: preassigning continuation group for %s: %v\n", bead.ID, err) //nolint:errcheck
		return 1
	}
	result.ContinuationAssigned = assigned
	if opts.JSON {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, result.BeadID) //nolint:errcheck
	return 0
}

// writeHookClaimNoWork writes the single drain result for a hook that claimed
// nothing. The reason is "no_work" for a genuinely idle store; it is
// "claims_errored" when claimsErrored is set — ready work existed but every
// eligible claim mutation errored — so an operational write failure stays
// distinguishable from idle even though both still drain and reclaim next tick.
func writeHookClaimNoWork(opts hookClaimOptions, ops hookClaimOps, claimsErrored bool, stdout, stderr io.Writer) int {
	reason := hookClaimReasonNoWork
	if claimsErrored {
		reason = hookClaimReasonClaimsErrored
	}
	return writeHookClaimDrain(reason, opts.JSON, opts.DrainAck, ops.DrainAck, stdout, stderr)
}

// writeHookClaimStaleSessionDrain emits the terminal result for a refused stale
// session (closed, superseded instance token, or a dormant/terminal state) that
// must stop instead of claiming. It preserves the gc hook --claim result
// contract: a --json caller gets a schema-backed drain record (action "drain",
// reason "stale_session"), and --drain-ack is honored, so a startup wrapper
// acknowledges drain and exits cleanly rather than seeing a bare exit 1 and
// retrying the refusal forever.
func writeHookClaimStaleSessionDrain(opts hookCommandOptions, stdout, stderr io.Writer) int {
	return writeHookClaimDrain(hookClaimReasonStaleSession, opts.JSON, opts.DrainAck, hookRuntimeDrainAck, stdout, stderr)
}

// writeHookClaimDrain writes the single structured drain result shared by every
// terminal no-claim outcome: an idle no-work store, a claims-errored store, and a
// refused stale session. For a --json caller it emits the schema-backed drain
// line; when drainAck is set it first runs drainAckFn and marks the result
// acknowledged. The exit code mirrors the historical contract — 0 once drain is
// acknowledged, else 1 — so a non-drain-ack caller still reports action=drain
// (a completed drain) rather than a bare failure.
func writeHookClaimDrain(reason string, jsonOut, drainAck bool, drainAckFn hookDrainAckFunc, stdout, stderr io.Writer) int {
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "drain",
		Reason:        reason,
	}
	if drainAck {
		if err := drainAckFn(stderr); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: drain-ack failed: %v\n", err) //nolint:errcheck
			return 1
		}
		result.DrainAcknowledged = true
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	if drainAck {
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
	assigned := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		if strings.TrimSpace(sibling.ID) == "" ||
			sibling.ID == bead.ID ||
			strings.TrimSpace(sibling.Assignee) != "" ||
			!strings.EqualFold(strings.TrimSpace(sibling.Status), "open") ||
			!hookClaimMatchesRoute(sibling, opts.RouteTargets) {
			continue
		}
		if err := ops.AssignContinuation(ctx, dir, opts.Env, sibling.ID, opts.Assignee); err != nil {
			return assigned, fmt.Errorf("assigning %s: %w", sibling.ID, err)
		}
		assigned = append(assigned, sibling.ID)
	}
	return assigned, nil
}

func hookClaimWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
	store := hookClaimBdStore(dir, env, assignee)
	claimed, ok, err := store.Claim(beadID)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if !ok {
		// Claim conflict: re-read the bead so the caller can surface who won
		// the race in the bead.claim_rejected event (ADR-0009). Best-effort —
		// a read error degrades to a silent no-op (empty bead, no event).
		current, getErr := store.Get(beadID)
		if getErr != nil {
			return beads.Bead{}, false, nil
		}
		return current, false, nil
	}
	if !hookClaimHasIdentity(claimed.Assignee, []string{assignee}) {
		// bd reported a successful mutation but the bead is owned by another
		// claimant (stale projection / lost race). Return it as a non-claim so
		// the caller can report the rejection rather than treat it as ours.
		return claimed, false, nil
	}
	return claimed, true, nil
}

// stampHookClaimIdentity records the claiming worker's execution identity on the
// claimed bead in ONE metadata write: gc.work_branch (the durable handle from the
// bead to its work that the close gate later reads, ADR-0009) plus the durable
// session back-reference gc.session_id / gc.session_name (#2843) so the dashboard
// run-detail can resolve which session executed a pool step after the transient
// Assignee is cleared on close. graphroute leaves pool steps unbound at route time,
// deferring the session binding to this claim (graphroute.go:200-203).
//
// The patch is compare-and-skipped against the bead's current metadata and the
// write is issued only when at least one key actually changes: this runs again on
// every hook tick via the existing_assignment / ready_assignment adoption paths, so
// an unconditional write would emit a bead.updated per tick per in-progress bead
// (the cache-reconcile flood class). Best-effort: a missing repo, detached HEAD,
// absent session, or write error never blocks the claim.
func stampHookClaimIdentity(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) {
	patch := hookClaimIdentityPatch(bead, opts, ops, dir)
	if len(patch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.StampWorkMeta(ctx, dir, opts.Env, bead.ID, opts.Assignee, patch); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: stamping execution identity on %s: %v\n", bead.ID, err) //nolint:errcheck
	}
}

// hookClaimIdentityPatch builds the compare-and-skipped claim-time metadata patch.
// It carries gc.work_branch when the worktree resolves a branch that differs from
// the bead's, and the session back-reference gc.session_id / gc.session_name when
// this is a session-run claim (GC_SESSION_ID present) of a non-control bead and the
// values differ. Session identity is stamped even when the branch is empty — a
// session with no worktree still needs its back-reference — but never on control
// beads, which stay session-free by graphroute's design
// (ApplyGraphControlRouteBinding), even when a control-dispatcher session claims one
// through this same hook path. An empty result means every key is already current,
// so the caller issues no write.
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
	return patch
}

func hookStampWorkMetaWithBdStore(_ context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
	store := hookClaimBdStore(dir, env, assignee)
	return store.Update(beadID, beads.UpdateOpts{Metadata: patch})
}

// recordHookClaimRunID records, on the session bead named by GC_SESSION_ID, the
// run this session is now working: beadmeta.ResolveRunID of the just-claimed
// bead, the same resolver the usage-fact emitters use (internal/worker). Those
// emitters still resolve the run id from the session bead's own chain today;
// once the deferred reader (ga-2m8abf) consumes gc.current_run_id, a per-request
// reader of the session bead will yield the same run id the model and compute
// facts carry. A bead with no run chain resolves to its own id, so a
// standalone unit is its own run and is never misattributed to a previous run on
// this reused session bead. The write is unconditional on every claim by design:
// the run id is a current-pointer that must follow a reused pool session onto its
// new run, and the prior value isn't in hand here to guard against (only the work
// bead and session id are). The only in-process idempotence guard available to
// this subprocess is a pre-write read of the session bead — the controller's
// CachingStore value-match guard is unreachable from here — so on a reused
// session that re-stamps the same run id the cost is one redundant bd update and
// its bead.updated event per claim. That is an accepted cost: claims are far less
// frequent than the per-second no-op writes the CachingStore guard targets, and a
// guard here would only trade the write for an equally unconditional read. The
// write reuses the claiming assignee as the bd actor for parity with the
// work_branch stamp, so both claim-time stamps attribute identically. Best-effort:
// the bd write is bound to ctx, so a slow or stuck update cannot outlast
// hookClaimMutationTimeout, and a non-session run (no GC_SESSION_ID), a timeout,
// or a write error never blocks the claim.
func recordHookClaimSessionPointers(bead beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) {
	sessionBeadID := hookClaimSessionID(opts.Env)
	if sessionBeadID == "" {
		return
	}
	// Both pointers are derived from the SAME just-claimed work bead so the (run, step)
	// tuple is consistent: run_id is the bead's resolved run root; step_id is its bare
	// gc.step_id (the cross-plane join key the events plane also uses), empty when the
	// work has no formula step (ad-hoc/manual) — which clears any prior step.
	runID := beadmeta.ResolveRunID(bead.Metadata, bead.ID, sessionBeadID)
	stepID := strings.TrimSpace(bead.Metadata[beadmeta.StepIDMetadataKey])
	// Publish a session→run-id map file so external tools can correlate this
	// session's activity to its run. Independent of and best-effort like the
	// pointer write below. The session may be addressed by any of these keys, so
	// the map is written under each.
	if err := writeRunMap(runID, bead.ID,
		hookClaimEnvValue(opts.Env, "GC_SESSION_NAME"),
		sessionBeadID,
		hookClaimEnvValue(opts.Env, "BEADS_ACTOR")); err != nil {
		// Best-effort correlation aid: a failed publish never blocks the claim,
		// but a persistent, systemic failure (an unwritable or unsafe run-map
		// dir) is surfaced here rather than silently dropped, so the "map never
		// appears" symptom is diagnosable instead of invisible.
		fmt.Fprintf(stderr, "gc hook --claim: publishing run-map for session %s: %v\n", sessionBeadID, err) //nolint:errcheck
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	if err := ops.RecordSessionPointers(ctx, dir, opts.Env, opts.Assignee, sessionBeadID, runID, stepID); err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: recording session pointers on session bead %s: %v\n", sessionBeadID, err) //nolint:errcheck
	}
}

func hookRecordSessionPointersWithBdStore(ctx context.Context, _ string, env []string, assignee, sessionBeadID, runID, stepID string) error {
	cityDir, cityEnv, err := hookClaimSessionStoreContext(ctx, env)
	if err != nil {
		return err
	}
	store := hookClaimBdStoreContext(ctx, cityDir, cityEnv, assignee)
	return store.Update(sessionBeadID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.CurrentRunIDMetadataKey:   runID,
		beadmeta.ActiveWorkBeadMetadataKey: stepID,
	}})
}

// hookClaimSessionStoreContext rebuilds the store environment for the city
// scope. Claim and continuation mutations use the selected work store, but
// session beads always live in the city store, including when work was claimed
// through cross-store federation from a rig.
func hookClaimSessionStoreContext(ctx context.Context, env []string) (string, []string, error) {
	cityPath := ""
	for _, key := range []string{"GC_CITY_PATH", "GC_CITY"} {
		for _, entry := range env {
			k, value, ok := strings.Cut(entry, "=")
			if !ok || k != key {
				continue
			}
			value = strings.TrimSpace(value)
			if value != "" && filepath.IsAbs(value) {
				cityPath = filepath.Clean(value)
				break
			}
		}
		if cityPath != "" {
			break
		}
	}
	if cityPath == "" {
		return "", nil, errors.New("resolving city store for session pointers: missing absolute GC_CITY_PATH or GC_CITY")
	}

	overrides, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, true)
	if err != nil {
		return "", nil, fmt.Errorf("resolving city store for session pointers: %w", err)
	}
	overrides["GC_STORE_ROOT"] = cityPath
	overrides["GC_STORE_SCOPE"] = "city"
	overrides["GC_RIG"] = ""
	overrides["GC_RIG_ROOT"] = ""
	return cityPath, mergeRuntimeEnv(env, overrides), nil
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
// <session>.json files. recordHookClaimSessionPointers always publishes both
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

func decodeHookClaimBeads(output string) ([]beads.Bead, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	if !json.Valid([]byte(output)) {
		extracted, ok := firstHookJSONValue(output)
		if !ok {
			return nil, errors.New("output is not JSON")
		}
		output = extracted
	}
	output = normalizeWorkQueryOutput(output)
	var candidates []beads.Bead
	if err := json.Unmarshal([]byte(output), &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
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
		if routedTo == target {
			return true
		}
		if routedTo == "" && kind == beadmeta.KindWorkflow && runTarget == target {
			return true
		}
	}
	return false
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
