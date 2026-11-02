package v2purity

import (
	"go/types"
	"strconv"
	"strings"
)

// Methods is the package fact of a Module package: the method sets of its
// named types that have a method reaching a target. An interface call in
// another package finds its implementers here, whatever that package's view
// of this one: export data, which go vet and nogo read, hides unexported
// types.
type Methods struct {
	Types []TypeMethods
}

// TypeMethods is one named type's method set, its pointer's.
type TypeMethods struct {
	Name    string // "path.Name"
	Generic bool   // uninstantiated: an interface matches it by method names
	Methods []MethodFact
}

// MethodFact is one method: its key (the name, path-qualified when
// unexported), its signature (methodSig), and what it reaches, if anything.
type MethodFact struct {
	Key, Sig, Full string // Full: types.Func.FullName, for the baseline
	Reaches        *Reaches
}

// AFact marks Methods a fact.
func (*Methods) AFact() {}

func (m *Methods) String() string { return "methods of " + strconv.Itoa(len(m.Types)) + " types" }

// impl is one method an interface call may run: a function of the package
// to follow, a target it reaches, or another package's method the baseline
// exempts, by FullName.
type impl struct {
	fn             *types.Func
	target, exempt string
}

// implementers are the methods an interface method call may run: fn's on
// every named type of the package that implements fn's interface (a
// generic one by method names), and on every type of another Module package
// whose Methods fact matches the interface and whose method reaches a
// target. A package sees the types of its own import closure only, so an
// interface call in a lower package runs the implementers that package can
// see, and a type declared above it is a leaf there; the rules' package,
// which imports the rest, sees every Module type. A method promoted from an embedded interface is that interface's:
// a forbidden one's is a target, any other's runs its implementers. Other
// packages' types are leaves, as their functions are.
func (g *graph) implementers(fn *types.Func) []impl {
	if ims, ok := g.impls[fn]; ok {
		return ims
	}
	g.impls[fn] = nil // an embedded interface may lead back here
	iface, _ := fn.Type().(*types.Signature).Recv().Type().Underlying().(*types.Interface)
	if iface == nil {
		return nil
	}
	var out []impl
	scope := g.pass.Pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() || types.IsInterface(tn.Type()) {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok || !implements(named, iface) {
			continue
		}
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(named), true, fn.Pkg(), fn.Name())
		m, ok := obj.(*types.Func)
		if !ok {
			continue
		}
		out = append(out, g.methodImpls(m.Origin())...)
	}
	seen := map[string]bool{}
	for _, tm := range g.depTypes() {
		if seen[tm.Name] {
			continue
		}
		seen[tm.Name] = true
		mf, ok := tm.method(iface, methodKey(fn))
		switch _, exempt := g.c.Exempt[mf.Full]; {
		case !ok || mf.Reaches == nil:
		case exempt && g.rules:
			out = append(out, impl{exempt: mf.Full})
		default:
			out = append(out, impl{target: "(" + tm.Name + ")." + fn.Name() + ", which reaches " + mf.Reaches.Target + " (via " + mf.Reaches.Path + ")"})
		}
	}
	g.impls[fn] = out
	return out
}

// methodImpls is what calling m, a method of one of the package's types,
// runs: m itself, an imported method's fact, or, promoted from an embedded
// interface, that interface's.
func (g *graph) methodImpls(m *types.Func) []impl {
	var r Reaches
	switch {
	case abstract(m):
		if t := g.forbiddenMethod(m); t != "" {
			return []impl{{target: t}}
		}
		return g.implementers(m)
	case m.Pkg() == g.pass.Pkg:
		return []impl{{fn: m}}
	case g.pass.ImportObjectFact(m, &r):
		return []impl{{target: m.FullName() + ", which reaches " + r.Target + " (via " + r.Path + ")"}}
	}
	return nil
}

// implements reports named, or its pointer, implementing iface; a generic
// type does when it has every method of iface by name.
func implements(named *types.Named, iface *types.Interface) bool {
	if named.TypeParams().Len() == 0 {
		return types.Implements(named, iface) || types.Implements(types.NewPointer(named), iface)
	}
	for i := range iface.NumMethods() {
		im := iface.Method(i)
		if obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(named), true, im.Pkg(), im.Name()); obj == nil {
			return false
		}
	}
	return true
}

// method is tm's method key, when tm implements iface.
func (tm TypeMethods) method(iface *types.Interface, key string) (MethodFact, bool) {
	have := map[string]MethodFact{}
	for _, m := range tm.Methods {
		have[m.Key] = m
	}
	for i := range iface.NumMethods() {
		im := iface.Method(i)
		m, ok := have[methodKey(im)]
		if !ok || !tm.Generic && m.Sig != methodSig(im.Type().(*types.Signature)) {
			return MethodFact{}, false
		}
	}
	m, ok := have[key]
	return m, ok
}

// summarize exports the package's Methods fact from reach, the package's
// functions' Reaches.
func (g *graph) summarize(reach map[types.Object]*Reaches) {
	var out Methods
	scope := g.pass.Pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || tn.IsAlias() || types.IsInterface(tn.Type()) {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		tm := TypeMethods{Name: g.pass.Pkg.Path() + "." + name, Generic: named.TypeParams().Len() > 0}
		reaches := false
		ms := types.NewMethodSet(types.NewPointer(named))
		for i := range ms.Len() {
			m := ms.At(i).Obj().(*types.Func).Origin()
			mf := MethodFact{Key: methodKey(m), Sig: methodSig(m.Type().(*types.Signature)), Full: m.FullName()}
			for _, im := range g.methodImpls(m) {
				switch {
				case mf.Reaches != nil:
				case im.target != "":
					mf.Reaches = &Reaches{Target: im.target, Path: name + "." + m.Name()}
				case reach[im.fn] != nil:
					mf.Reaches = reach[im.fn]
				}
			}
			reaches = reaches || mf.Reaches != nil
			tm.Methods = append(tm.Methods, mf)
		}
		if reaches {
			out.Types = append(out.Types, tm)
		}
	}
	have := map[string]bool{}
	for _, tm := range out.Types {
		have[tm.Name] = true
	}
	for _, tm := range g.depTypes() { // the summaries travel on a direct import's fact
		if !have[tm.Name] {
			have[tm.Name] = true
			out.Types = append(out.Types, tm)
		}
	}
	if len(out.Types) > 0 {
		g.pass.ExportPackageFact(&out)
	}
}

// depTypes are the type summaries of every other package's Methods fact
// the driver shows: under go vet and nogo, a direct import's, which merges
// its own dependencies'.
func (g *graph) depTypes() []TypeMethods {
	var out []TypeMethods
	for _, pf := range g.pass.AllPackageFacts() {
		if ms, ok := pf.Fact.(*Methods); ok && pf.Package != g.pass.Pkg {
			out = append(out, ms.Types...)
		}
	}
	return out
}

// methodKey is fn's name, path-qualified when unexported.
func methodKey(fn *types.Func) string {
	if fn.Exported() || fn.Pkg() == nil {
		return fn.Name()
	}
	return fn.Pkg().Path() + "." + fn.Name()
}

// methodSig is sig's parameters and results by type, without names or
// receiver: the same under every driver.
func methodSig(sig *types.Signature) string {
	tuple := func(t *types.Tuple) string {
		var parts []string
		for i := range t.Len() {
			parts = append(parts, types.TypeString(t.At(i).Type(), nil))
		}
		return "(" + strings.Join(parts, ",") + ")"
	}
	v := ""
	if sig.Variadic() {
		v = "..."
	}
	return tuple(sig.Params()) + v + tuple(sig.Results())
}
