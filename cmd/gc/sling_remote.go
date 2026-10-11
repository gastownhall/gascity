package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
)

// cmdSlingRemote routes a sling mutation to a REMOTE city over the control
// plane. The remote server does all config and store resolution, so this
// forwards the raw sling parameters (target, bead-or-formula, --on, vars,
// scope, force, title) and renders the result. Modes that require local state
// are refused with a clear message: inline text (needs a locally-created bead),
// the 1-arg form (infers the target from local rig config), and the
// --dry-run/--nudge flags the server API does not model.
func cmdSlingRemote(c *api.Client, target *remoteTarget, args []string, isFormula, doNudge, force bool, title string, vars []string, merge string, noConvoy, owned, reassign bool, onFormula string, noFormula, fromStdin, dryRun bool, scopeKind, scopeRef string, jsonOutput bool, stdout, stderr io.Writer) int {
	fail := func(code, message string) int {
		if jsonOutput {
			return writeJSONError(stdout, stderr, code, message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}

	if fromStdin {
		return fail("unsupported_remote", "gc sling: --stdin (inline text) is not supported for a remote city; sling an existing bead")
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

	// A remote city cannot infer the default target from local rig config, so an
	// explicit target is required (the 2-arg form).
	if len(args) != 2 {
		return fail("invalid_arguments", "gc sling: a remote city requires an explicit target and an existing bead/formula: gc sling <target> <bead-or-formula>")
	}
	// Inline text (a bead-or-formula argument with whitespace) auto-creates a
	// task bead locally, but a remote city has no such path — the sling API takes
	// only an existing bead ID or a formula name. Refuse it with a clear message
	// (a bead ID / formula name never contains whitespace) rather than forwarding
	// prose as a bogus bead ID.
	if !isFormula && strings.ContainsAny(args[1], " \t\n") {
		return fail("unsupported_remote", "gc sling: inline text is not supported for a remote city; sling an existing bead by ID")
	}

	vmap, err := parseSlingVars(vars)
	if err != nil {
		return fail("invalid_arguments", "gc sling: "+err.Error())
	}
	req := api.SlingRequest{
		Target:    args[0],
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
		req.Formula = args[1]
	case onFormula != "":
		// The API spells `--on <formula> <bead>` as formula + attached_bead_id.
		req.Formula = onFormula
		req.AttachedBeadID = args[1]
	default:
		req.Bead = args[1]
	}

	// Echo the resolved target (human mode only) so the operator can see which
	// control plane this mutation is about to hit — matching `gc rig add` and
	// guarding against a silent write to a remote city selected by a stale env or
	// sticky-default context.
	if !jsonOutput {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}

	res, err := c.Sling(req)
	if err != nil {
		return fail("sling_failed", "gc sling: "+err.Error())
	}
	if onFormula != "" {
		if msg := checkRemoteOnAttach(c, &res, args[0], args[1], onFormula); msg != "" {
			return fail("attached_to_container", msg)
		}
	}
	return renderRemoteSlingResult(res, jsonOutput, stdout, stderr)
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
// one-line summary).
func renderRemoteSlingResult(res api.SlingResult, jsonOutput bool, stdout, stderr io.Writer) int {
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
			"success":        true,
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
		if len(res.Warnings) > 0 {
			payload["warnings"] = res.Warnings
		}
		enc, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintln(stderr, "gc sling: encoding result:", err) //nolint:errcheck
			return 1
		}
		fmt.Fprintln(stdout, string(enc)) //nolint:errcheck // best-effort stdout
		return 0
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

func putIfSet(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
