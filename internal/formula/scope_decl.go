package formula

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ScopeOnFailAbort is the only gc.on_fail value a [[scope]] declaration may
// stamp on its members.
const ScopeOnFailAbort = "abort_scope"

// ScopeDecl is a formula-level [[scope]] declaration: it names a scope body and
// the steps that belong to it, by step id, in one place.
//
// mergeSteps replaces an overridden step wholesale, so scope membership written
// on a step's own metadata is lost whenever an extender overrides that step.
// A declaration is resolved against the final step list, after extends, so an
// overridden step or a step the extender adds still lands in the scope.
// Resolve stamps gc.scope_ref, gc.scope_role and the scope body's gc.kind and
// gc.scope_name from it, before graph controls read them.
//
// Ids name top-level steps. A child formula's declaration with the same Name as
// an inherited one replaces it whole; names the child does not redeclare are
// inherited unchanged. A formula that declares no [[scope]] is unaffected.
type ScopeDecl struct {
	// Name is the scope's name (gc.scope_name on the body) and the key child
	// formulas use to replace an inherited declaration.
	Name string `json:"name" toml:"name"`

	// Body is the id of the scope body step.
	Body string `json:"body" toml:"body"`

	// Members are the ids of the steps stamped gc.scope_role=member.
	Members []string `json:"members" toml:"members"`

	// Teardown are the ids of the steps stamped gc.scope_role=teardown.
	Teardown []string `json:"teardown,omitempty" toml:"teardown,omitempty"`

	// OnFail, when set, is stamped as gc.on_fail on each member. The only value
	// is "abort_scope". Teardown steps never receive it.
	OnFail string `json:"on_fail,omitempty" toml:"on_fail,omitempty"`
}

// cloneScopeDecls copies a declaration list so a merged formula never aliases a
// parent's.
func cloneScopeDecls(decls []*ScopeDecl) []*ScopeDecl {
	if len(decls) == 0 {
		return nil
	}
	out := make([]*ScopeDecl, 0, len(decls))
	for _, d := range decls {
		if d == nil {
			continue
		}
		c := *d
		c.Members = append([]string(nil), d.Members...)
		c.Teardown = append([]string(nil), d.Teardown...)
		out = append(out, &c)
	}
	return out
}

// inheritScopeDecls appends the parent declarations whose name the formula does
// not already declare. The formula's own declarations, and those of earlier
// parents, win, as for vars.
func inheritScopeDecls(have, parent []*ScopeDecl) []*ScopeDecl {
	names := make(map[string]bool, len(have))
	for _, d := range have {
		names[d.Name] = true
	}
	for _, d := range cloneScopeDecls(parent) {
		if !names[d.Name] {
			have = append(have, d)
		}
	}
	return have
}

// applyScopeDecls resolves f's [[scope]] declarations against its final steps.
// It returns f itself when f declares none. Otherwise it returns a copy whose
// stamped steps are copies too, because the parser shares *Step values between
// the formulas it resolves and an in-place stamp would leak into them.
func applyScopeDecls(f *Formula) (*Formula, error) {
	if len(f.Scopes) == 0 {
		return f, nil
	}
	plan, err := planScopeDecls(f)
	if err != nil {
		return nil, err
	}

	out := *f
	out.Steps = make([]*Step, len(f.Steps))
	for i, step := range f.Steps {
		stamps := plan[step.ID]
		if len(stamps) == 0 {
			out.Steps[i] = step
			continue
		}
		clone := cloneStep(step)
		// Always build a fresh map: cloneStep keeps the original map when it is
		// empty but non-nil, and stamping that would leak into shared steps.
		md := make(map[string]string, len(step.Metadata)+len(stamps))
		for k, v := range step.Metadata {
			md[k] = v
		}
		for k, v := range stamps {
			md[k] = v
		}
		clone.Metadata = md
		out.Steps[i] = clone
	}
	return &out, nil
}

// planScopeDecls validates every declaration and returns the metadata to stamp,
// keyed by step id. It reports every problem at once, sorted.
func planScopeDecls(f *Formula) (map[string]map[string]string, error) {
	sp := &scopePlanner{
		byID:  make(map[string]*Step, len(f.Steps)),
		plan:  make(map[string]map[string]string),
		owner: make(map[string]string),
	}
	for _, step := range f.Steps {
		sp.byID[step.ID] = step
	}

	seenName := make(map[string]bool)
	for _, d := range f.Scopes {
		switch {
		case d.Name == "":
			sp.problems = append(sp.problems, "a [[scope]] declaration has no name")
			continue
		case seenName[d.Name]:
			sp.problems = append(sp.problems, fmt.Sprintf("scope %q is declared twice in one formula", d.Name))
			continue
		}
		seenName[d.Name] = true
		sp.declare(d)
	}

	if len(sp.problems) > 0 {
		sort.Strings(sp.problems)
		return nil, fmt.Errorf("formula %q: scope declaration:\n  - %s", f.Formula, strings.Join(sp.problems, "\n  - "))
	}
	return sp.plan, nil
}

// scopePlanner accumulates the stamps and problems of planScopeDecls.
type scopePlanner struct {
	byID     map[string]*Step
	problems []string
	plan     map[string]map[string]string
	owner    map[string]string // step id -> scope that claimed it
}

// declare validates one named declaration and plans its stamps.
func (sp *scopePlanner) declare(d *ScopeDecl) {
	if d.Body == "" {
		sp.problems = append(sp.problems, fmt.Sprintf("scope %q: no body step id declared (body = \"...\")", d.Name))
	}
	if len(d.Members) == 0 {
		sp.problems = append(sp.problems, fmt.Sprintf("scope %q: no members declared (members = [...])", d.Name))
	}
	if d.OnFail != "" && d.OnFail != ScopeOnFailAbort {
		sp.problems = append(sp.problems, fmt.Sprintf(
			"scope %q: on_fail %q is not supported; the only value is %q", d.Name, d.OnFail, ScopeOnFailAbort))
	}

	if d.Body != "" && sp.exists(d.Name, "body", d.Body) {
		sp.claim(d.Name, d.Body)
		sp.stamp(d.Name, d.Body, map[string]string{
			beadmeta.KindMetadataKey:      beadmeta.KindScope,
			beadmeta.ScopeNameMetadataKey: d.Name,
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody,
		})
	}
	// role records the role each step id has taken within this declaration, so
	// a step listed twice, or as both member and teardown, is rejected.
	role := make(map[string]string)
	for _, id := range d.Members {
		if !sp.admit(d, role, beadmeta.ScopeRoleMember, id) {
			continue
		}
		kv := map[string]string{
			beadmeta.ScopeRefMetadataKey:  d.Body,
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
		}
		if d.OnFail != "" {
			kv[beadmeta.OnFailMetadataKey] = d.OnFail
		}
		sp.stamp(d.Name, id, kv)
	}
	for _, id := range d.Teardown {
		if !sp.admit(d, role, beadmeta.ScopeRoleTeardown, id) {
			continue
		}
		sp.stamp(d.Name, id, map[string]string{
			beadmeta.ScopeRefMetadataKey:  d.Body,
			beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		})
	}
}

// admit reports whether id may take kind (member or teardown) in d, and claims
// it for d's scope when it may. It records a problem when id is the body, names
// no step, or already took a role in d.
func (sp *scopePlanner) admit(d *ScopeDecl, role map[string]string, kind, id string) bool {
	if id == d.Body {
		article := "a "
		if kind == beadmeta.ScopeRoleTeardown {
			article = ""
		}
		sp.problems = append(sp.problems, fmt.Sprintf(
			"scope %q: step %q is the body and cannot also be %s%s", d.Name, id, article, kind))
		return false
	}
	if !sp.exists(d.Name, kind, id) {
		return false
	}
	if prev, ok := role[id]; ok {
		if prev == kind {
			sp.problems = append(sp.problems, fmt.Sprintf(
				"scope %q: step %q is listed twice as %s", d.Name, id, kind))
		} else {
			sp.problems = append(sp.problems, fmt.Sprintf(
				"scope %q: step %q is listed as both %s and %s", d.Name, id, prev, kind))
		}
		return false
	}
	role[id] = kind
	sp.claim(d.Name, id)
	return true
}

// stamp plans kv onto step id, recording a problem for each key the step
// already sets to a different value.
func (sp *scopePlanner) stamp(scope, id string, kv map[string]string) {
	step := sp.byID[id]
	if step == nil {
		return
	}
	for k, v := range kv {
		if cur, ok := step.Metadata[k]; ok && cur != v {
			sp.problems = append(sp.problems, fmt.Sprintf(
				"scope %q: step %q sets %s = %q, which conflicts with the declaration (%q); remove it from the step",
				scope, id, k, cur, v))
		}
	}
	if sp.plan[id] == nil {
		sp.plan[id] = make(map[string]string, len(kv))
	}
	for k, v := range kv {
		sp.plan[id][k] = v
	}
}

// claim records that scope owns id, or a problem when another scope does.
func (sp *scopePlanner) claim(scope, id string) {
	if prev, ok := sp.owner[id]; ok && prev != scope {
		sp.problems = append(sp.problems, fmt.Sprintf(
			"step %q is listed in both scope %q and scope %q; a step belongs to one scope", id, prev, scope))
		return
	}
	sp.owner[id] = scope
}

// exists reports whether id names a step, recording a problem when it does not.
func (sp *scopePlanner) exists(scope, role, id string) bool {
	if sp.byID[id] != nil {
		return true
	}
	sp.problems = append(sp.problems, fmt.Sprintf(
		"scope %q: %s %q: no step with that id in the resolved formula", scope, role, id))
	return false
}

// validateUnstampedScopeMembership checks a merge for dangling scope refs
// unless it carries [[scope]] declarations: Resolve checks those once stamped,
// because a declared body is not a scope body until then.
func validateUnstampedScopeMembership(f *Formula) error {
	if len(f.Scopes) > 0 {
		return nil
	}
	return validateScopeMembership(f)
}
