package genshell

import (
	"fmt"
	"runtime"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

type scriptEnviron struct {
	pairs []string
}

func newScriptEnviron(base, assigns []string) *scriptEnviron {
	env := &scriptEnviron{}
	for _, kv := range base {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		env.set(name, value)
	}
	for _, kv := range assigns {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		env.set(name, value)
	}
	return env
}

func (e *scriptEnviron) set(name, value string) {
	entry := name + "=" + value
	for i, kv := range e.pairs {
		if strings.HasPrefix(kv, name+"=") {
			e.pairs[i] = entry
			return
		}
	}
	e.pairs = append(e.pairs, entry)
}

func (e *scriptEnviron) Get(name string) expand.Variable {
	for _, kv := range e.pairs {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == name {
			return expand.Variable{Set: true, Exported: true, Kind: expand.String, Str: v}
		}
	}
	return expand.Variable{}
}

func (e *scriptEnviron) Each(fn func(name string, vr expand.Variable) bool) {
	for _, kv := range e.pairs {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if !fn(name, expand.Variable{Set: true, Exported: true, Kind: expand.String, Str: value}) {
			return
		}
	}
}

func (e *scriptEnviron) hasCaseConflict(name string) bool {
	foundFolded := false
	for _, kv := range e.pairs {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k == name {
			return false
		}
		if strings.EqualFold(k, name) {
			foundFolded = true
		}
	}
	return foundFolded
}

func (e *scriptEnviron) hasNameFold(name string) bool {
	for _, kv := range e.pairs {
		k, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func rewriteCaseConflictingBindings(file *syntax.File, env *scriptEnviron) map[string]string {
	renames := map[string]string{}
	scriptNames := scriptVariableNames(file)
	addBinding := func(name string) {
		if !isShellName(name) || !env.hasCaseConflict(name) {
			return
		}
		if _, ok := renames[name]; ok {
			return
		}
		renames[name] = replacementName(name, env, renames, scriptNames)
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil {
				addBinding(n.Name.Value)
			}
		case *syntax.WordIter:
			if n.Name != nil {
				addBinding(n.Name.Value)
			}
		case *syntax.CallExpr:
			for _, lit := range readBindingLits(n) {
				addBinding(lit.Value)
			}
		}
		return true
	})
	if len(renames) == 0 {
		return nil
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			renameLit(n.Name, renames)
		case *syntax.WordIter:
			renameLit(n.Name, renames)
		case *syntax.ParamExp:
			renameLit(n.Param, renames)
		case *syntax.CallExpr:
			for _, lit := range readBindingLits(n) {
				renameLit(lit, renames)
			}
		}
		return true
	})
	aliases := make(map[string]string, len(renames))
	for original, replacement := range renames {
		aliases[replacement] = original
	}
	return aliases
}

func scriptVariableNames(file *syntax.File) map[string]struct{} {
	names := map[string]struct{}{}
	add := func(name string) {
		if isShellName(name) {
			names[name] = struct{}{}
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil {
				add(n.Name.Value)
			}
		case *syntax.WordIter:
			if n.Name != nil {
				add(n.Name.Value)
			}
		case *syntax.ParamExp:
			if n.Param != nil {
				add(n.Param.Value)
			}
		case *syntax.CallExpr:
			for _, lit := range readBindingLits(n) {
				add(lit.Value)
			}
		}
		return true
	})
	return names
}

func replacementName(name string, env *scriptEnviron, renames map[string]string, scriptNames map[string]struct{}) string {
	base := "__gascity_genshell_" + name
	for i := 0; ; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s_%d", base, i)
		}
		if env.hasNameFold(candidate) {
			continue
		}
		if scriptHasNameFold(scriptNames, candidate) {
			continue
		}
		used := false
		for old, replacement := range renames {
			if strings.EqualFold(old, candidate) || strings.EqualFold(replacement, candidate) {
				used = true
				break
			}
		}
		if !used {
			return candidate
		}
	}
}

func scriptHasNameFold(names map[string]struct{}, candidate string) bool {
	for name := range names {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

func renameLit(lit *syntax.Lit, renames map[string]string) {
	if lit == nil {
		return
	}
	if replacement, ok := renames[lit.Value]; ok {
		lit.Value = replacement
	}
}

func readBindingLits(call *syntax.CallExpr) []*syntax.Lit {
	if len(call.Args) == 0 {
		return nil
	}
	name, ok := literalWord(call.Args[0])
	if !ok || name != "read" {
		return nil
	}
	var lits []*syntax.Lit
	for _, word := range call.Args[1:] {
		lit, text, ok := literalWordLit(word)
		if !ok {
			continue
		}
		if strings.HasPrefix(text, "-") {
			continue
		}
		if isShellName(text) {
			lits = append(lits, lit)
		}
	}
	return lits
}

func literalWord(word *syntax.Word) (string, bool) {
	_, text, ok := literalWordLit(word)
	return text, ok
}

func literalWordLit(word *syntax.Word) (*syntax.Lit, string, bool) {
	if word == nil || len(word.Parts) != 1 {
		return nil, "", false
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return nil, "", false
	}
	return lit, lit.Value, true
}

func isShellName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || (i > 0 && '0' <= r && r <= '9') {
			continue
		}
		return false
	}
	return true
}
