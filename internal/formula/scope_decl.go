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
		if clone.Metadata == nil {
			clone.Metadata = make(map[string]string, len(stamps))
		}
		for k, v := range stamps {
			clone.Metadata[k] = v
		}
		out.Steps[i] = clone
	}
	return &out, nil
}

// planScopeDecls validates every declaration and returns the metadata to stamp,
// keyed by step id. It reports every problem at once, sorted.
func planScopeDecls(f *Formula) (map[string]map[string]string, error) {
	byID := make(map[string]*Step, len(f.Steps))
	for _, step := range f.Steps {
		byID[step.ID] = step
	}

	var problems []string
	plan := make(map[string]map[string]string)
	owner := make(map[string]string) // step id -> scope that claimed it
	seenName := make(map[string]bool)

	stamp := func(scope, id string, kv map[string]string) {
		step := byID[id]
		if step == nil {
			return
		}
		for k, v := range kv {
			if cur, ok := step.Metadata[k]; ok && cur != v {
				problems = append(problems, fmt.Sprintf(
					"scope %q: step %q sets %s = %q, which conflicts with the declaration (%q); remove it from the step",
					scope, id, k, cur, v))
			}
		}
		if plan[id] == nil {
			plan[id] = make(map[string]string, len(kv))
		}
		for k, v := range kv {
			plan[id][k] = v
		}
	}
	claim := func(scope, id string) {
		if prev, ok := owner[id]; ok && prev != scope {
			problems = append(problems, fmt.Sprintf(
				"step %q is listed in both scope %q and scope %q; a step belongs to one scope", id, prev, scope))
			return
		}
		owner[id] = scope
	}
	exists := func(scope, role, id string) bool {
		if byID[id] != nil {
			return true
		}
		problems = append(problems, fmt.Sprintf(
			"scope %q: %s %q: no step with that id in the resolved formula", scope, role, id))
		return false
	}

	for _, d := range f.Scopes {
		switch {
		case d.Name == "":
			problems = append(problems, "a [[scope]] declaration has no name")
			continue
		case seenName[d.Name]:
			problems = append(problems, fmt.Sprintf("scope %q is declared twice in one formula", d.Name))
			continue
		}
		seenName[d.Name] = true

		if d.Body == "" {
			problems = append(problems, fmt.Sprintf("scope %q: no body step id declared (body = \"...\")", d.Name))
		}
		if len(d.Members) == 0 {
			problems = append(problems, fmt.Sprintf("scope %q: no members declared (members = [...])", d.Name))
		}
		if d.OnFail != "" && d.OnFail != ScopeOnFailAbort {
			problems = append(problems, fmt.Sprintf(
				"scope %q: on_fail %q is not supported; the only value is %q", d.Name, d.OnFail, ScopeOnFailAbort))
		}

		if d.Body != "" && exists(d.Name, "body", d.Body) {
			claim(d.Name, d.Body)
			stamp(d.Name, d.Body, map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindScope,
				beadmeta.ScopeNameMetadataKey: d.Name,
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleBody,
			})
		}
		for _, id := range d.Members {
			if id == d.Body {
				problems = append(problems, fmt.Sprintf(
					"scope %q: step %q is the body and cannot also be a member", d.Name, id))
				continue
			}
			if !exists(d.Name, "member", id) {
				continue
			}
			claim(d.Name, id)
			kv := map[string]string{
				beadmeta.ScopeRefMetadataKey:  d.Body,
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleMember,
			}
			if d.OnFail != "" {
				kv[beadmeta.OnFailMetadataKey] = d.OnFail
			}
			stamp(d.Name, id, kv)
		}
		for _, id := range d.Teardown {
			if id == d.Body {
				problems = append(problems, fmt.Sprintf(
					"scope %q: step %q is the body and cannot also be teardown", d.Name, id))
				continue
			}
			if !exists(d.Name, "teardown", id) {
				continue
			}
			claim(d.Name, id)
			stamp(d.Name, id, map[string]string{
				beadmeta.ScopeRefMetadataKey:  d.Body,
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
			})
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("formula %q: scope declaration:\n  - %s", f.Formula, strings.Join(problems, "\n  - "))
	}
	return plan, nil
}
