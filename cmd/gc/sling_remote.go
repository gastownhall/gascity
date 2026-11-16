package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
)

// cmdSlingRemote routes a sling mutation to a REMOTE city over the control
// plane. The remote server does all config and store resolution, so this
// forwards the raw sling parameters (target, bead-or-formula, --on, vars,
// scope, force, title) and renders the result. Inline text and --stdin create
// the task bead remotely first (see createRemoteInlineBead), then sling it by
// ID. Modes that require local state are refused with a clear message: the
// 1-arg form (infers the target from local rig config), and the
// --dry-run/--nudge flags the server API does not model.
func cmdSlingRemote(c *api.Client, target *remoteTarget, args []string, isFormula, doNudge, force bool, title string, vars []string, merge string, noConvoy, owned, reassign bool, onFormula string, noFormula, fromStdin, dryRun bool, scopeKind, scopeRef string, jsonOutput bool, stdout, stderr io.Writer) int {
	fail := func(code, message string) int {
		if jsonOutput {
			return writeJSONError(stdout, stderr, code, message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}

	if dryRun {
		return fail("unsupported_remote", "gc sling: --dry-run is not supported for a remote city")
	}
	// --nudge stays refused for a remote city: it needs server-side delivery
	// wiring. --on and the metadata flags (--merge/--no-convoy/--owned/
	// --no-formula) are server-expressible and forwarded below. A current server
	// runs the same sling.(*Sling).Dispatch as the local path, so --on on a
	// convoy is attached per child; checkRemoteOnAttach catches an older server
	// that attached it to the container instead.
	if doNudge {
		return fail("unsupported_remote", "gc sling: --nudge delivery for a remote city lands separately; sling without --nudge")
	}

	work, errCode, errMsg := resolveRemoteSlingWork(args, isFormula, fromStdin)
	if errCode != "" {
		return fail(errCode, errMsg)
	}

	vmap, err := parseSlingVars(vars)
	if err != nil {
		return fail("invalid_arguments", "gc sling: "+err.Error())
	}

	// Echo the resolved target (human mode only) so the operator can see which
	// control plane this mutation is about to hit — matching `gc rig add` and
	// guarding against a silent write to a remote city selected by a stale env or
	// sticky-default context.
	if !jsonOutput {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}

	if work.inline {
		if work, errCode, errMsg = createRemoteInlineBead(c, work, scopeKind, scopeRef, jsonOutput, stdout); errCode != "" {
			return fail(errCode, errMsg)
		}
	}

	req := api.SlingRequest{
		Target:    work.target,
		Title:     title,
		Vars:      vmap,
		ScopeKind: scopeKind,
		ScopeRef:  scopeRef,
		Force:     force,
		Reassign:  reassign,
		Merge:     merge,
		NoConvoy:  noConvoy,
		Owned:     owned,
		NoFormula: noFormula,
	}
	switch {
	case isFormula:
		req.Formula = work.beadOrFormula
	case onFormula != "":
		// The API spells `--on <formula> <bead>` as formula + attached_bead_id.
		req.Formula = onFormula
		req.AttachedBeadID = work.beadOrFormula
	default:
		req.Bead = work.beadOrFormula
	}

	res, err := c.Sling(req)
	if err != nil {
		return fail("sling_failed", remoteSlingFailureMessage(err, target, work, remoteSlingResumeFlags{
			onFormula: onFormula, title: title, vars: vars, merge: merge,
			noConvoy: noConvoy, owned: owned, reassign: reassign, force: force,
			noFormula: noFormula, scopeKind: scopeKind, scopeRef: scopeRef, jsonOutput: jsonOutput,
		}))
	}
	if onFormula != "" {
		if msg := checkRemoteOnAttach(c, &res, work.target, work.beadOrFormula, onFormula); msg != "" {
			return fail("attached_to_container", msg)
		}
	}
	return renderRemoteSlingResult(res, onFormula != "", jsonOutput, stdout, stderr)
}

// remoteSlingWork is what a remote sling routes: the target, and the bead ID,
// formula name, or inline text to route to it.
type remoteSlingWork struct {
	target        string
	beadOrFormula string
	// inline marks beadOrFormula as the title of a task bead to create first,
	// with description as its body.
	inline      bool
	description string
	// createdID is the bead created from the inline text; beadOrFormula then
	// names it.
	createdID string
}

// resolveRemoteSlingWork reads what a remote sling routes from its arguments.
// With --stdin the one argument is the target and stdin holds the bead text
// (first line = title, rest = description), exactly as the local path reads
// it; otherwise the explicit 2-arg form is required, and an argument that
// contains whitespace is inline text. Either text becomes a task bead unless
// it names a formula. On failure it returns a non-empty (errCode, errMsg) pair.
func resolveRemoteSlingWork(args []string, isFormula, fromStdin bool) (work remoteSlingWork, errCode, errMsg string) {
	if fromStdin {
		if len(args) != 1 {
			return work, "invalid_arguments", "gc sling: --stdin requires exactly 1 argument (target)"
		}
		var title, description string
		if title, description, errCode, errMsg = readSlingStdinBead(); errCode != "" {
			return work, errCode, errMsg
		}
		return remoteSlingWork{target: args[0], beadOrFormula: title, inline: !isFormula, description: description}, "", ""
	}
	// A remote city cannot infer the default target from local rig config, so an
	// explicit target is required (the 2-arg form).
	if len(args) != 2 {
		return work, "invalid_arguments", "gc sling: a remote city requires an explicit target and a bead, formula, or inline text: gc sling <target> <bead-or-formula>"
	}
	work = remoteSlingWork{target: args[0], beadOrFormula: args[1]}
	// Inline text: a bead ID or formula name never contains whitespace, so an
	// argument that does is prose to create a task bead from, as locally.
	if !isFormula && strings.ContainsAny(args[1], " \t\n") {
		if err := refuseMultiLineInlineText(args[1]); err != nil {
			return work, "inline_bead_resolve_failed", "gc sling: " + err.Error()
		}
		work.inline = true
	}
	return work, "", ""
}

// createRemoteInlineBead creates the task bead for inline text in the remote
// city and returns the work to sling: that bead, by ID, to the resolved target.
// The target is resolved first, as the local path resolves the agent before it
// creates anything, so a target the sling cannot reach fails with nothing
// written. The bead is created in the resolved agent's rig store, or the city
// store for a city-scoped agent: the store the local path opens for the target,
// and the one the sling routes the bead from. On failure it returns a non-empty
// (errCode, errMsg) pair.
func createRemoteInlineBead(c *api.Client, work remoteSlingWork, scopeKind, scopeRef string, jsonOutput bool, stdout io.Writer) (remoteSlingWork, string, string) {
	slingTarget, rig, err := resolveRemoteSlingTarget(c, work.target, scopeKind, scopeRef)
	if err != nil {
		return work, "target_resolve_failed", fmt.Sprintf("gc sling: %v; nothing was created", err)
	}
	created, err := c.CreateBead(api.CreateBeadRequest{
		Rig:         rig,
		Title:       work.beadOrFormula,
		Description: work.description,
		Type:        slingInlineBeadType,
	})
	if err != nil {
		return work, "bead_create_failed", "gc sling: creating bead: " + err.Error()
	}
	if !jsonOutput {
		fmt.Fprintf(stdout, "Created %s — %q\n", created.ID, work.beadOrFormula) //nolint:errcheck // best-effort stdout
	}
	return remoteSlingWork{target: slingTarget, beadOrFormula: created.ID, createdID: created.ID}, "", ""
}

// resolveRemoteSlingTarget resolves the agent a remote sling to slingTarget
// reaches, as the server's sling does (qualifySlingTarget, then the agent
// lookup), and returns the name to sling to and the agent's rig. Under
// --scope-kind rig a bare target names the agent in the scope rig, and the
// agent must belong to that rig: the server refuses any other, and a bare name
// it does not qualify resolves only to a city-scoped agent.
func resolveRemoteSlingTarget(c *api.Client, slingTarget, scopeKind, scopeRef string) (name, rig string, err error) {
	rigScope := scopeKind == "rig"
	scopeRef = strings.TrimSpace(scopeRef)
	name = slingTarget
	if rigScope && !strings.Contains(slingTarget, "/") {
		name = scopeRef + "/" + slingTarget
	}
	agent, err := c.GetAgent(name)
	if err != nil {
		return "", "", fmt.Errorf("resolving target %q: %w", name, err)
	}
	if rigScope && agent.Rig != scopeRef {
		return "", "", fmt.Errorf("--scope-ref %s conflicts with target %s in rig %q", scopeRef, name, agent.Rig)
	}
	return name, agent.Rig, nil
}

// remoteSlingFailureMessage is the error a failed remote sling reports. The
// create and the sling are two requests, not one transaction, so a failed sling
// of a bead created from inline text leaves the bead behind: the message names
// it and the command that routes it, which repeats the original sling flags
// with the inline text replaced by the bead, instead of a re-run that would
// create another. A refusal from the server routed nothing. A request that
// failed in transport, or that the server failed while handling it, may have
// routed the bead and launched its workflow first, so the message says to check
// the bead before resuming: with --force, the resume would replace that
// workflow.
func remoteSlingFailureMessage(err error, target *remoteTarget, work remoteSlingWork, flags remoteSlingResumeFlags) string {
	if work.createdID == "" {
		return "gc sling: " + err.Error()
	}
	resume := remoteSlingResumeCommand(target, work.target, work.createdID, flags)
	if api.IsConnError(err) || api.IsServerError(err) {
		return fmt.Sprintf("gc sling: %v\ngc sling: routing created bead %s may have succeeded; check it with: gc %s beads show %s\ngc sling: if it was not routed, resume with: %s",
			err, work.createdID, remoteInvocationFlags(target), shellSingleQuote(work.createdID), resume)
	}
	return fmt.Sprintf("gc sling: %v\ngc sling: created bead %s was not routed; resume with: %s", err, work.createdID, resume)
}

// remoteSlingResumeFlags are the sling flags of an inline invocation,
// reproduced in the command that resumes its failed sling.
type remoteSlingResumeFlags struct {
	onFormula, title, merge, scopeKind, scopeRef            string
	vars                                                    []string
	noConvoy, owned, reassign, force, noFormula, jsonOutput bool
}

// remoteSlingResumeCommand is the command that slings an already-created bead
// to slingTarget on the same remote city with the original invocation's sling
// flags, so re-running it does the same sling. Every interpolated value is
// shell-quoted.
func remoteSlingResumeCommand(target *remoteTarget, slingTarget, beadID string, f remoteSlingResumeFlags) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gc %s sling %s %s", remoteInvocationFlags(target), shellSingleQuote(slingTarget), shellSingleQuote(beadID))
	value := func(flag, v string) {
		if v != "" {
			fmt.Fprintf(&b, " --%s %s", flag, shellSingleQuote(v))
		}
	}
	toggle := func(flag string, set bool) {
		if set {
			b.WriteString(" --" + flag)
		}
	}
	value("on", f.onFormula)
	value("title", f.title)
	for _, kv := range f.vars {
		value("var", kv)
	}
	value("merge", f.merge)
	toggle("no-convoy", f.noConvoy)
	toggle("owned", f.owned)
	toggle("reassign", f.reassign)
	toggle("force", f.force)
	toggle("no-formula", f.noFormula)
	value("scope-kind", f.scopeKind)
	value("scope-ref", f.scopeRef)
	toggle("json", f.jsonOutput)
	return b.String()
}

// checkRemoteOnAttach catches a remote server older than this client
// attaching an --on formula to a convoy container rather than to each open
// child, as `gc sling` does locally. A current server always reports where the
// formula went: a per-child batch for a convoy, a molecule_id for a v1 attach
// to a single bead, a workflow_id for a graph launch. An older server has no
// batch or molecule_id field, so its v1 attach reports none of the three, and
// only then is the bead looked up. A workflow_id skips the lookup, which is
// safe for a convoy: a graph formula takes a convoy as its one input on an
// older server as on a current one. It returns the failure message when the
// bead is a convoy; a failed lookup only adds a warning to res, because the
// sling itself succeeded. An older server also attaches a formula to an epic,
// which a current one refuses. This check misses that, since an epic is not a
// container type and a graph launch skips the lookup (ga-nabyph).
func checkRemoteOnAttach(c *api.Client, res *api.SlingResult, target, beadID, formula string) string {
	if res.Batch != nil || res.MoleculeID != "" || res.WorkflowID != "" {
		return ""
	}
	got, err := c.GetBead(beadID)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"could not check that %s is not a convoy (%v); a server older than this gc attaches --on %s to a convoy itself, not to each open child",
			beadID, err, formula))
		return ""
	}
	if !beads.IsContainerType(got.Body.Type) {
		return ""
	}
	return fmt.Sprintf(
		"gc sling: the remote city attached formula %q to %s %s itself, not to each open child: its server is older than this gc. Upgrade the remote city, or attach the formula child by child: gc sling %s <child> --on %s",
		formula, got.Body.Type, beadID, target, formula)
}

// parseSlingVars splits repeatable key=value strings into a map.
func parseSlingVars(vars []string) (map[string]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(vars))
	for _, kv := range vars {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --var %q (want key=value)", kv)
		}
		out[k] = v
	}
	return out, nil
}

// renderRemoteSlingResult prints a remote sling outcome. Warnings go to stderr;
// the result goes to stdout (a compact JSON object with --json, otherwise a
// one-line summary, or for a convoy the same per-child lines the local path
// prints). A convoy that routed some children and failed others (status
// partial) exits 1, as the local path does, or 3 when a child was refused
// because its source bead already has a live workflow: then, in either mode,
// each such child gets the two conflict lines the local path prints, with the
// server's cleanup command. onFormula says the sling attached an explicit --on
// formula, which only labels attached wisps.
func renderRemoteSlingResult(res api.SlingResult, onFormula, jsonOutput bool, stdout, stderr io.Writer) int {
	exit := 0
	if res.Partial() {
		exit = 1
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w) //nolint:errcheck // best-effort stderr
	}
	if jsonOutput {
		// Keep the automation-critical fields aligned with the local `sling --json`
		// shape (schema_version, success, target, bead_id, formula, molecule_id,
		// workflow_id, convoy_id, batch, warnings) so a script repointed at a
		// remote city keeps working. Fields with no server-side analog (routed/
		// queued/dry_run) are omitted; server-only detail (status, root_bead_id,
		// mode) is added.
		payload := map[string]any{
			"schema_version": "1",
			"success":        !res.Partial(),
			"status":         res.Status,
			"target":         res.Target,
		}
		putIfSet(payload, "formula", res.Formula)
		putIfSet(payload, "bead_id", res.Bead)
		putIfSet(payload, "workflow_id", res.WorkflowID)
		putIfSet(payload, "root_bead_id", res.RootBeadID)
		putIfSet(payload, "attached_bead_id", res.AttachedBeadID)
		putIfSet(payload, "mode", res.Mode)
		putIfSet(payload, "molecule_id", res.MoleculeID)
		putIfSet(payload, "convoy_id", res.ConvoyID)
		if b := res.Batch; b != nil {
			batch := map[string]any{
				"total":      b.Total,
				"routed":     b.Routed,
				"failed":     b.Failed,
				"skipped":    b.Skipped,
				"idempotent": b.Idempotent,
			}
			putIfSet(batch, "container_type", b.ContainerType)
			payload["batch"] = batch
		}
		if len(res.Children) > 0 {
			payload["children"] = remoteSlingChildrenJSON(res.Children)
		}
		if len(res.Warnings) > 0 {
			payload["warnings"] = res.Warnings
		}
		enc, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintln(stderr, "gc sling: encoding result:", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stdout, string(enc)) //nolint:errcheck // best-effort stdout
		if printRemoteChildConflicts(stderr, res.Children) > 0 {
			return 3
		}
		return exit
	}
	if res.Batch != nil && len(res.Children) > 0 {
		printBatchSlingResult(remoteBatchSlingResult(res, onFormula), stdout, stderr)
		if printRemoteChildConflicts(stderr, res.Children) > 0 {
			return 3
		}
		if res.Partial() {
			b := res.Batch
			fmt.Fprintf(stderr, "%d/%d children failed\n", b.Failed, b.Failed+b.Routed+b.Idempotent) //nolint:errcheck
		}
		return exit
	}
	line := res.Status + " → " + res.Target
	switch {
	case res.WorkflowID != "":
		line += " (workflow " + res.WorkflowID + ")"
	case res.Bead != "":
		line += " (" + res.Bead + ")"
	}
	fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
	return 0
}

// remoteSlingChildrenJSON renders a convoy's per-child outcomes for --json; a
// child refused on a source-workflow conflict also carries that conflict.
func remoteSlingChildrenJSON(children []api.SlingChildOutcome) []map[string]any {
	out := make([]map[string]any, 0, len(children))
	for _, c := range children {
		child := map[string]any{"bead_id": c.BeadID, "outcome": c.Outcome}
		putIfSet(child, "status", c.Status)
		putIfSet(child, "reason", c.Reason)
		putIfSet(child, "formula", c.Formula)
		putIfSet(child, "workflow_id", c.WorkflowID)
		putIfSet(child, "molecule_id", c.MoleculeID)
		if conflict := c.Conflict; conflict != nil {
			child["conflict"] = map[string]any{
				"source_bead_id":        conflict.SourceBeadID,
				"blocking_workflow_ids": conflict.BlockingWorkflowIDs,
				"hint":                  conflict.Hint,
			}
		}
		out = append(out, child)
	}
	return out
}

// printRemoteChildConflicts prints the conflict lines for each convoy child the
// server refused because its source bead already has a live workflow, and
// returns how many it printed.
func printRemoteChildConflicts(stderr io.Writer, children []api.SlingChildOutcome) (printed int) {
	for _, c := range children {
		if c.Conflict == nil {
			continue
		}
		printSourceWorkflowConflictLines(stderr, c.Conflict.SourceBeadID, c.Conflict.BlockingWorkflowIDs, c.Conflict.Hint)
		printed++
	}
	return printed
}

// remoteBatchSlingResult rebuilds the domain batch result from a remote convoy
// sling's wire result, so the remote path renders it with the local path's
// printBatchSlingResult. An --on sling sends the convoy as attached_bead_id,
// so the server echoes it there rather than as bead.
func remoteBatchSlingResult(res api.SlingResult, onFormula bool) sling.SlingResult {
	b := res.Batch
	out := sling.SlingResult{
		BeadID:        cmp.Or(res.Bead, res.AttachedBeadID),
		Target:        res.Target,
		Method:        "batch-default-on",
		ContainerType: b.ContainerType,
		Total:         b.Total,
		Routed:        b.Routed,
		Failed:        b.Failed,
		Skipped:       b.Skipped,
		IdempotentCt:  b.Idempotent,
	}
	if onFormula {
		out.Method = "batch-on"
	}
	for _, c := range res.Children {
		out.Children = append(out.Children, sling.SlingChildResult{
			BeadID:      c.BeadID,
			Status:      c.Status,
			Routed:      c.Outcome == sling.ChildOutcomeRouted,
			Skipped:     c.Outcome == sling.ChildOutcomeSkipped,
			Failed:      c.Outcome == sling.ChildOutcomeFailed,
			FailReason:  c.Reason,
			WorkflowID:  c.WorkflowID,
			WispRootID:  c.MoleculeID,
			FormulaName: c.Formula,
		})
	}
	return out
}

func putIfSet(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
