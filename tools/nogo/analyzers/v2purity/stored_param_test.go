package v2purity_test

import (
	"testing"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/v2purity"
)

// Kills a //gc:stored-param that defers an argument evaluated where it is
// passed (a call result, a method value's receiver), and the written-field
// rule hiding a computed base or an impure right-hand side (the C5a1-1 max
// review's probes): each is reported.
func TestStoredParamDefersOnlyStorableArguments(t *testing.T) {
	impure := "\n\nfunc impure() int { return Now() }\n\nfunc clean() int { return 1 }\n"
	secDecl := "type sec struct {\n\tclass int\n\tp     func() int\n}\n\n//gc:stored-param p\nfunc probed(p func() int) sec { return sec{class: 1, p: func() int { return p() }} }\n\n"
	plainDecl := "type sec struct {\n\tclass int\n\tp     func() int\n}\n\nfunc probed(p func() int) sec { return sec{class: 1, p: func() int { return p() }} }\n\n"
	for _, c := range []struct {
		name, src string
		want      string
	}{
		// An argument evaluated eagerly at the call site, inside a pure root:
		// mk() calls Now() when decide runs, whatever probed does with its result.
		{"eager call arg to a stored param in a pure root", secDecl + "func mk() func() int { Now(); return clean }\n\n//gc:pure\nfunc decide() int { s := probed(mk()); return s.class }" + impure, "Now"},
		{"control: eager call arg to a plain param in a pure root", plainDecl + "func mk() func() int { Now(); return clean }\n\n//gc:pure\nfunc decide() int { s := probed(mk()); return s.class }" + impure, "Now"},
		// A method value whose receiver expression does I/O when evaluated.
		{"eager method value receiver to a stored param", secDecl + "type pr struct{}\n\nfunc (pr) run() int { return 1 }\n\nfunc newPr() pr { Now(); return pr{} }\n\n//gc:pure\nfunc decide() int { s := probed(newPr().run); return s.class }" + impure, "Now"},
		// The written-field rule: the selector's X is still a read.
		{"written field whose base is computed with I/O", "type box struct{ f func() int }\n\nvar b box\n\nfunc mkp() *box { Now(); return &b }\n\n//gc:pure\nfunc decide() int { mkp().f = clean; return 1 }" + impure, "Now"},
		{"compound assignment to a field is a read", "type box struct{ n int }\n\nvar b box\n\n//gc:pure\nfunc decide() int { b.n += impure(); return 1 }" + impure, "(via decide -> impure)"},
		{"written field, RHS impure call", "type box struct{ n int }\n\nvar b box\n\n//gc:pure\nfunc decide() int { b.n = impure(); return 1 }" + impure, "(via decide -> impure)"},
		{"written func field, RHS call returning func", "type box struct{ f func() int }\n\nvar b box\n\nfunc mk() func() int { Now(); return clean }\n\n//gc:pure\nfunc decide() int { b.f = mk(); return 1 }" + impure, "Now"},
		// A stored param laundered through a local before the store.
		{"stored param laundered through a local, field read", "type sec struct {\n\tp func() int\n}\n\n//gc:stored-param p\nfunc probed(p func() int) sec { q := p; return sec{p: func() int { return q() }} }\n\nvar secs = []sec{probed(impure)}\n\n//gc:pure\nfunc decide() int { return secs[0].p() }" + impure, "impure"},
	} {
		check(t, c.name, messages(t, cfg, "", c.src), c.want)
	}
}

// Kills a two-type-argument instantiation losing the deferral (a false
// positive), and a stored param on an exempt callee hiding an impure
// argument: an exempt body is never entered, so its argument is checked
// where it is passed.
func TestStoredParamEdges(t *testing.T) {
	impure := "\n\nfunc impure() int { return Now() }\n\nfunc clean() int { return 1 }\n"
	// Explicit multi-type-argument instantiation: is the stored param still deferred?
	src2 := "type sec struct {\n\tclass int\n\tp     func() int\n}\n\n//gc:stored-param p\nfunc called[A, B any](p func() int) sec { return sec{class: 1, p: func() int { return p() }} }\n\nvar secs = []sec{called[int, int](impure)}\n\n//gc:pure\nfunc decide() int { return secs[0].class }" + impure
	check(t, "explicit two-type-arg instantiation, data field read (want none)", messages(t, cfg, "", src2), "")
	// A stored param on an exempt function: is an impure argument hidden?
	src3 := "type sec struct {\n\tp func() int\n}\n\n//gc:stored-param p\nfunc run(p func() int) int { return p() }\n\n//gc:pure\nfunc decide() int { return run(impure) }" + impure
	c := cfg
	c.Exempt = map[string]v2purity.Exemption{pkg + ".run": {Class: "live", Bead: "b", Reason: "r"}}
	check(t, "an exempt stored-param callee does not hide an impure argument", messages(t, c, "", src3), "Now")
	check(t, "a stored-param callee reached with an impure argument", messages(t, cfg, "", src3), "Now")
}
