package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
)

// cmdSlingRemote routes a sling mutation to a REMOTE city over the control
// plane. The remote server does all config and store resolution, so this
// forwards the raw sling parameters (target, bead-or-formula, vars, scope,
// force, title) and renders the result. Modes that require local state are
// refused with a clear message: inline text (needs a locally-created bead), the
// 1-arg form (infers the target from local rig config), and the local
// batch/dry-run flags the server API does not model.
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
	var unsupported []string
	for _, u := range []struct {
		set  bool
		flag string
	}{
		{doNudge, "--nudge"},
		{merge != "", "--merge"},
		{noConvoy, "--no-convoy"},
		{owned, "--owned"},
		{reassign, "--reassign"},
		{onFormula != "", "--on"},
		{noFormula, "--no-formula"},
	} {
		if u.set {
			unsupported = append(unsupported, u.flag)
		}
	}
	if len(unsupported) > 0 {
		return fail("unsupported_remote", "gc sling: these flags are not supported for a remote city yet: "+strings.Join(unsupported, ", "))
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
	}
	if isFormula {
		req.Formula = args[1]
	} else {
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
	return renderRemoteSlingResult(res, jsonOutput, stdout, stderr)
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
		// shape (schema_version, success, target, bead_id, formula, workflow_id,
		// warnings) so a script repointed at a remote city keeps working. Fields
		// with no server-side analog (molecule_id, convoy_id, batch, routed/queued/
		// dry_run) are omitted; server-only detail (status, root_bead_id, mode) is
		// added.
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
