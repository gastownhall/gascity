package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// The core reaper order (internal/bootstrap/packs/core/assets/scripts/reaper.sh)
// runs `gc bd purge --wisps-plane` and `gc bd prune --pattern` on every bd
// scope. Both select closed no-history rows, which is where op-journal rows
// (issue type op) live, and bd deletes them with no tombstone unless it
// honors types.retained and the scope's database names op there. So the
// reaper holds both back on a scope that holds op rows until then
// (op_rows_guard). reaperOpRowsHeldBack is that rule for gc doctor; the two
// must agree.

const (
	// reaperOpRowType is the issue type of every op-journal row kind.
	reaperOpRowType = "op"
	// reaperRetainedTypesKey is the workspace setting naming the types a sweep
	// without --retain-ids keeps. A bd that honors it names it in its purge
	// and prune help.
	reaperRetainedTypesKey = "types.retained"
	// reaperOpRowCensusSQL counts at most one op row in each table of the
	// scope's database, as the reaper's census does.
	reaperOpRowCensusSQL = "SELECT COUNT(*) FROM (" +
		"SELECT id FROM (SELECT id FROM wisps WHERE issue_type = '" + reaperOpRowType + "' LIMIT 1) AS op_wisp " +
		"UNION ALL " +
		"SELECT id FROM (SELECT id FROM issues WHERE issue_type = '" + reaperOpRowType + "' LIMIT 1) AS op_issue" +
		") AS op_row_census"
)

// reaperGuardedVerbs are the bd verbs the reaper holds back on a scope whose
// op rows bd would not keep.
var reaperGuardedVerbs = []string{"purge", "prune"}

// reaperOpRowsScope is one bd scope the core reaper visits.
type reaperOpRowsScope struct {
	label string
	dir   string
	run   beads.CommandRunner
}

// reaperOpRowsHeldBack returns why the reaper holds its purge and prune back
// on scope, or "" when it runs them. Like the reaper it fails closed once the
// scope is known to hold op rows: help or a setting it cannot read holds them
// back. A census it cannot read is the error: the reaper holds back there
// too, but such a scope is one bd cannot serve, which the bead-store checks
// report.
func reaperOpRowsHeldBack(scope reaperOpRowsScope) (string, error) {
	out, err := scope.run(scope.dir, "bd", "sql", "--csv", reaperOpRowCensusSQL)
	if err != nil {
		return "", fmt.Errorf("counting op rows: %w", err)
	}
	count, err := reaperCensusCount(out)
	if err != nil {
		return "", fmt.Errorf("counting op rows: %w", err)
	}
	if count == 0 {
		return "", nil
	}
	for _, verb := range reaperGuardedVerbs {
		help, err := scope.run(scope.dir, "bd", verb, "--help")
		if err != nil || !strings.Contains(string(help), reaperRetainedTypesKey) {
			return "holds op rows and its bd does not honor " + reaperRetainedTypesKey, nil
		}
	}
	out, err = scope.run(scope.dir, "bd", "config", "get", "--json", reaperRetainedTypesKey)
	if err != nil {
		return fmt.Sprintf("holds op rows and %s could not be read: %v", reaperRetainedTypesKey, err), nil
	}
	namesOp, err := retainedTypesNameOp(out)
	if err != nil {
		return fmt.Sprintf("holds op rows and %s could not be read: %v", reaperRetainedTypesKey, err), nil
	}
	if !namesOp {
		return fmt.Sprintf("holds op rows and %s does not name %s", reaperRetainedTypesKey, reaperOpRowType), nil
	}
	return "", nil
}

// reaperCensusCount reads the count from `bd sql --csv` output: a header,
// then the value.
func reaperCensusCount(out []byte) (int, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	n, err := strconv.Atoi(last)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("the census returned %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

// retainedTypesNameOp reports whether the types.retained value in `bd config
// get --json` output names the op row type. bd's sweep parses that value as it
// parses types.custom, with one function (a JSON array of type names, else a
// comma list, each name trimmed), so this reads it with doctor's reader of
// types.custom. A mis-spelled list such as [op] or "op" then names no op, as
// for bd. reaper.sh's retained_types_name_op reads the setting the same way.
func retainedTypesNameOp(out []byte) (bool, error) {
	names, err := doctor.ParseCustomTypesConfigJSON(out)
	if err != nil {
		return false, err
	}
	return slices.Contains(names, reaperOpRowType), nil
}

// reaperOpRowsScopes lists the bd scopes the core reaper visits that doctor
// may read: the city and every rig that is not suspended, each scope
// directory once. A scope served over http is left out, as the reaper order
// is not dispatched there. So is a scope skipped says doctor must not read
// (the doctor store gate; nil skips none): on the proxied path any bd read of
// a stopped store starts its proxy and Dolt child, and the gate's stand-ins
// already report that scope.
func reaperOpRowsScopes(cityPath string, cfg *config.City, skipped func(scopeRoot string) bool) []reaperOpRowsScope {
	if cfg == nil {
		return nil
	}
	var scopes []reaperOpRowsScope
	seen := map[string]bool{}
	add := func(label, dir string, run func() beads.CommandRunner) {
		key := filepath.Clean(dir)
		if seen[key] || !providerUsesBdStoreContract(rawBeadsProviderForScope(dir, cityPath)) ||
			orderScopeBackend(cityPath, dir) == "http" || (skipped != nil && skipped(dir)) {
			return
		}
		seen[key] = true
		scopes = append(scopes, reaperOpRowsScope{label: label, dir: dir, run: run()})
	}
	add("city", bdCityScopeTarget(cityPath, cfg).ScopeRoot, func() beads.CommandRunner { return bdCommandRunnerForCity(cityPath) })
	suspended := buildSuspendedRigPathsForCity(cfg, cityPath)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || suspended[filepath.Clean(rig.Path)] {
			continue
		}
		rigPath := rig.Path
		add("rig:"+rig.Name, bdRigScopeTarget(cityPath, rig).ScopeRoot, func() beads.CommandRunner { return bdCommandRunnerForRig(cityPath, cfg, rigPath) })
	}
	return scopes
}

// reaperOpRowsHeldScopes lists, as "<scope>: <why>", every scope known to
// hold op rows on which the core reaper holds its purge and prune back.
func reaperOpRowsHeldScopes(scopes []reaperOpRowsScope) []string {
	var held []string
	for _, scope := range scopes {
		if why, err := reaperOpRowsHeldBack(scope); err == nil && why != "" {
			held = append(held, scope.label+": "+why)
		}
	}
	return held
}

// addReaperOpRowsWarning turns r into a Warning (an Error stays one) naming
// the scopes the core reaper does not purge or prune because of their op
// rows: closed rows pile up there until bd keeps the op rows itself.
func addReaperOpRowsWarning(r *doctor.CheckResult, held []string) {
	if len(held) == 0 {
		return
	}
	if r.Status < doctor.StatusWarning {
		r.Status = doctor.StatusWarning
	}
	warning := fmt.Sprintf("the core reaper does not purge or prune %d scope(s) holding op rows, which bd would delete with no tombstone", len(held))
	if r.Message == "" {
		r.Message = warning
	} else {
		r.Message += "; " + warning
	}
	r.Details = append(r.Details, held...)
	hint := "once every writer of the scope runs a bd that honors " + reaperRetainedTypesKey +
		" (its purge and prune help name it), run `gc bd config set " + reaperRetainedTypesKey + " " + reaperOpRowType + "` there (`--rig <name>` for a rig scope)"
	if r.FixHint == "" {
		r.FixHint = hint
	} else {
		r.FixHint += "; " + hint
	}
}
