package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// hookRootJSONResult is the explicit continuation protocol. It deliberately
// has no drain action or acknowledgement: releasing a continuation and acquiring
// unrelated work are separate caller decisions.
type hookRootJSONResult struct {
	SchemaVersion        string   `json:"schema_version"`
	OK                   bool     `json:"ok"`
	Command              string   `json:"command"`
	Action               string   `json:"action"`
	Reason               string   `json:"reason"`
	RootBeadID           string   `json:"root_bead_id"`
	BeadID               string   `json:"bead_id,omitempty"`
	Assignee             string   `json:"assignee,omitempty"`
	Route                string   `json:"route,omitempty"`
	ContinuationGroup    string   `json:"continuation_group,omitempty"`
	ContinuationAssigned []string `json:"continuation_assigned,omitempty"`
}

func hookRootContains(root string, b beads.Bead) bool {
	return root == "" || ((strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]) == root ||
		(b.ID == root && b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow)) &&
		b.Metadata[beadmeta.KindMetadataKey] != "workflow-finalize")
}

func writeHookRootResult(result hookRootJSONResult, jsonOut bool, stdout, stderr io.Writer) int {
	var err error
	switch {
	case jsonOut:
		err = writeCLIJSONLine(stdout, result)
	case result.Action == "work":
		_, err = fmt.Fprintf(stdout, "work %s %s\n", result.RootBeadID, result.BeadID)
	default:
		_, err = fmt.Fprintf(stdout, "%s %s %s\n", result.Action, result.RootBeadID, result.Reason)
	}
	if err != nil {
		return reportClaimWorkQueryFailure(fmt.Errorf("writing scoped hook result: %w", err), stderr)
	}
	return 0
}

func hookRootOutcome(opts hookClaimOptions, action, reason string, stdout, stderr io.Writer) int {
	return writeHookRootResult(hookRootJSONResult{
		SchemaVersion: "2", OK: true, Command: hookClaimCommandName,
		RootBeadID: opts.RootBeadID, Action: action, Reason: reason,
	}, opts.JSON, stdout, stderr)
}

// claimRootHookWorkWithRunner uses the canonical frontier with the explicit
// root predicate applied before readiness and candidate selection. A generic
// work_query can short-circuit on another workflow at any of its tiers, so it
// cannot answer a strict continuation request. The existing claim tiers still
// own routing, assignment, worktree, workflow-stop and claim-delivery checks.
func claimRootHookWorkWithRunner(workDir string, env []string, stores []hookStore, opts hookClaimOptions, ops hookClaimOps, run hookStoreRunner, stdout, stderr io.Writer) int {
	if run == nil {
		return reportClaimWorkQueryFailure(errors.New("missing scoped ready query runner"), stderr)
	}
	if opts.DrainAck {
		return reportClaimWorkQueryFailure(errors.New("--root cannot be combined with --drain-ack"), stderr)
	}
	opts.RootBeadID = strings.TrimSpace(opts.RootBeadID)
	opts.IdentityCandidates = hookClaimIdentityCandidates(append([]string{opts.Assignee}, opts.IdentityCandidates...)...)
	if opts.RootBeadID == "" || strings.TrimSpace(opts.Assignee) == "" {
		return reportClaimWorkQueryFailure(errors.New("--root requires a root bead and a claiming identity"), stderr)
	}
	if len(stores) == 0 {
		stores = []hookStore{{dir: workDir, env: env}}
	}
	ops.applyDefaults()
	ops.ClassRoute.observeWorkLegs(stores)
	root, rootStore, err := readHookRoot(stores, opts, ops)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	if hookRootTerminal(root) {
		return hookRootOutcome(opts, "complete", "workflow_complete", stdout, stderr)
	}
	// Existing assignments can refuse acquisition in this root; they never
	// supply the root selector or transfer an obligation to this workflow.
	otherAssignment, err := hookRootOtherAssignment(stores, opts, ops)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	if otherAssignment {
		return hookRootOutcome(opts, "wait", "different_workflow_assignment", stdout, stderr)
	}
	primary := stores[0]
	read := func(command string) (string, error) { return run(command, primary.dir, primary.env) }
	predicate := " --metadata-field " + shellquote.Quote(beadmeta.RootBeadIDMetadataKey+"="+opts.RootBeadID)
	assigned, err := read("gc ready --json --status in_progress --limit 0" + predicate)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	assignments, err := decodeScopedHookRows(assigned)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	for _, b := range assignments {
		if b.Status == "in_progress" && !hookClaimCandidateIsMessage(b) && hookClaimHasIdentity(b.Assignee, opts.IdentityCandidates) && !hookRootContains(opts.RootBeadID, b) {
			return hookRootOutcome(opts, "wait", "different_workflow_assignment", stdout, stderr)
		}
	}
	if root.Status == "blocked" || beadmeta.HasDispatchHold(root.Labels) {
		return hookRootOutcome(opts, "wait", "workflow_blocked", stdout, stderr)
	}
	query := "gc ready --json --limit 0" + predicate
	ready, err := read(query)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	// Keep the frontier's blocked_by fields intact. They are part of the
	// readiness proof, and Bead intentionally does not project them.
	output, err := mergeScopedHookRows(assigned, ready, opts.RootBeadID)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	claimsErrored := false
	for _, st := range stores {
		storeOpts := opts
		storeOpts.Env = st.env
		storeOps := ops
		storeOps.Runner = func(string, string) (string, error) { return output, nil }
		res := tryHookClaim(query, st.dir, &storeOpts, &storeOps, stdout, stderr)
		if res.terminal {
			return res.code
		}
		claimsErrored = claimsErrored || res.claimsErrored
	}
	if claimsErrored {
		return reportClaimWorkQueryFailure(errors.New("scoped claim writes failed"), stderr)
	}
	// Re-read after lost claims and predecessor-close races. Only the root's
	// terminal state proves completion; an empty frontier never does.
	root, _, err = readHookRoot([]hookStore{rootStore}, opts, ops)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	if hookRootTerminal(root) {
		return hookRootOutcome(opts, "complete", "workflow_complete", stdout, stderr)
	}
	blocked, err := hookRootBlocked(root, rootStore, opts, ops)
	if err != nil {
		return reportClaimWorkQueryFailure(err, stderr)
	}
	reason := "workflow_no_ready"
	if blocked {
		reason = "workflow_blocked"
	}
	return hookRootOutcome(opts, "wait", reason, stdout, stderr)
}

// hookRootOtherAssignment batches the session's aliases in one live read per
// store, including open reservations. It never infers continuation intent from
// those assignments; they can only refuse acquisition in the requested root.
func hookRootOtherAssignment(stores []hookStore, opts hookClaimOptions, ops hookClaimOps) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	read := func(list func(beads.ListQuery) ([]beads.Bead, error)) (bool, error) {
		rows, err := list(beads.ListQuery{Assignees: opts.IdentityCandidates, TierMode: beads.TierBoth, Live: true})
		if err != nil {
			return false, fmt.Errorf("reading existing hook assignments: %w", err)
		}
		for _, b := range rows {
			if beads.IsReadyExcludedType(b.Type) && b.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
				continue
			}
			if !hookRootTerminal(b) && !hookClaimCandidateIsMessage(b) && hookClaimHasIdentity(b.Assignee, opts.IdentityCandidates) && !hookRootContains(opts.RootBeadID, b) {
				return true, nil
			}
		}
		return false, nil
	}
	for _, st := range stores {
		found, err := read(ops.WorkflowStore(ctx, st.dir, st.env, "", opts.Assignee).List)
		if err != nil || found {
			return found, err
		}
	}
	if ops.ClassRoute != nil {
		return read(ops.ClassRoute.class.List)
	}
	return false, nil
}

func readHookRoot(stores []hookStore, opts hookClaimOptions, ops hookClaimOps) (beads.Bead, hookStore, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	for _, st := range stores {
		root, err := ops.ReadWorkMeta(ctx, st.dir, st.env, opts.RootBeadID, opts.Assignee)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return beads.Bead{}, hookStore{}, fmt.Errorf("reading workflow root %q: %w", opts.RootBeadID, err)
		}
		if root.ID != opts.RootBeadID || root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
			return beads.Bead{}, hookStore{}, fmt.Errorf("bead %q is not a workflow root", opts.RootBeadID)
		}
		return root, st, nil
	}
	return beads.Bead{}, hookStore{}, fmt.Errorf("workflow root %q: %w", opts.RootBeadID, beads.ErrNotFound)
}

func hookRootTerminal(root beads.Bead) bool {
	return root.Status == "closed" || root.Status == "tombstone"
}

func decodeScopedHookRows(output string) ([]beads.Bead, error) {
	normalized := normalizeWorkQueryOutput(strings.TrimSpace(output))
	if !strings.HasPrefix(normalized, "[") {
		return nil, errors.New("scoped ready query did not return a JSON array")
	}
	rows, skipped, err := decodeHookClaimBeads(normalized)
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("decoding scoped hook bead %s: %w", skipped[0].ID, skipped[0].Err)
	}
	for _, b := range rows {
		if b.ID == "" || b.Status == "" {
			return nil, errors.New("scoped ready query returned a bead without identity or status")
		}
	}
	return rows, nil
}

func mergeScopedHookRows(assigned, ready, root string) (string, error) {
	rows := []json.RawMessage{}
	seen := map[string]bool{}
	for _, output := range []string{assigned, ready} {
		if _, err := decodeScopedHookRows(output); err != nil {
			return "", err
		}
		var raw []json.RawMessage
		if err := json.Unmarshal([]byte(normalizeWorkQueryOutput(strings.TrimSpace(output))), &raw); err != nil {
			return "", err
		}
		for _, item := range raw {
			b, _, err := decodeHookClaimBeads("[" + string(item) + "]")
			if err != nil {
				return "", err
			}
			if len(b) == 1 && hookRootContains(root, b[0]) && !seen[b[0].ID] {
				seen[b[0].ID] = true
				rows = append(rows, item)
			}
		}
	}
	result, err := json.Marshal(rows)
	return string(result), err
}

func hookRootBlocked(root beads.Bead, st hookStore, opts hookClaimOptions, ops hookClaimOps) (bool, error) {
	if root.Status == "blocked" || beadmeta.HasDispatchHold(root.Labels) {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookWorkQueryTimeout)
	defer cancel()
	store := ops.WorkflowStore(ctx, st.dir, st.env, root.ID, opts.Assignee)
	rows, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, TierMode: beads.TierBoth})
	if err != nil {
		return false, fmt.Errorf("reading workflow %s members: %w", root.ID, err)
	}
	for _, b := range rows {
		if !hookRootContains(root.ID, b) || hookRootTerminal(b) {
			continue
		}
		if b.Status == "blocked" || b.Status == "deferred" || beadmeta.HasDispatchHold(b.Labels) {
			return true, nil
		}
		blocked, err := beadHasUnmetPlainBlocksDep(store, b.ID)
		if err != nil {
			return false, fmt.Errorf("reading workflow %s readiness: %w", root.ID, err)
		}
		if blocked {
			return true, nil
		}
	}
	return false, nil
}
