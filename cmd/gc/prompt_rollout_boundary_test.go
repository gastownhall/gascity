package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
)

// This file is PR-1a: the structural guarantee that feature-flag values (from
// internal/rollout) can NEVER flow into agent-visible prompt content. Rollout
// gates select MECHANICAL Go transport paths; a smarter model obviates
// agent-behavior toggles, so a flag VALUE reaching a prompt template would
// violate gascity's "no capability flags — a sentence in the prompt is
// sufficient" principle. It is the sanctioned lighter form of execution-plan
// task S1-T14 (DESIGN open-question-5's "defer the internal/prompt extraction,
// rely on the AST lint" fallback); the full extraction is deferred to ga-b1ii8y.
//
// SCOPE + HONEST LIMITS. All prompt construction lives in cmd/gc package main
// (PromptContext, renderPrompt, buildTemplateData, promptFuncMap are unexported),
// so the guard scopes to the AST-DERIVED set of cmd/gc render files. This is a
// DEFENSE-IN-DEPTH tripwire for DIRECT flows, NOT an absolute proof: a purely
// syntactic lint cannot chase every value-laundering path (DESIGN §2.4 says so
// and makes the value-flow half review-governed). It reliably catches the
// realistic, accidental leak — `ctx.Field = flags.Mode()` / a config accessor's
// value assigned or returned into template data — and its self-protection is
// pinned by TestPromptBoundaryCheckerHasTeeth. The STRUCTURAL fix that closes the
// residue classes below is ga-b1ii8y (extract internal/prompt so the import edge
// is compiler-enforced).
//
// THE RULE (learned from an adversarial red-team that defeated an earlier
// seam-by-seam matcher). Seam matching — "no flag ident inside a PromptContext
// literal / .Env write / named FuncMap" — leaks: a value reaches template data
// through a non-Env field assignment (ctx.WorkQuery = ...), an unnamed
// map[string]any passed to .Funcs, a buildTemplateData-result write (td[k] =
// ...), or an intermediate variable, and it also FALSE-flags mechanical flag
// transport elsewhere (a systemd-unit FuncMap, a subprocess Env). Instead:
// within a render file, a flag-value identifier is forbidden EVERYWHERE EXCEPT in
// a control-flow condition (if/for/switch). Reading a gate to BRANCH is
// legitimate (gc prime does); assigning or returning its VALUE — the only way it
// reaches template data — is not. One rule, every seam.
//
// KNOWN RESIDUE — review-governed (registry SelectsBetween litmus + CODEOWNERS on
// registry.go), and all closed by the ga-b1ii8y extraction:
//   - a value laundered through an intermediate variable OR a helper that lives in
//     a NON-render file (the file references no anchor, so it is not scanned);
//   - a value written to prompt data through a package-internal type ALIAS or an
//     embedding of PromptContext in a file that names no anchor ident (file-level
//     derivation is not closed under aliasing);
//   - a value carried by a side-effecting helper whose call sits in a condition
//     but whose body writes prompt data (only the argument laundering — a
//     forbidden ident passed as a call arg inside a condition — is caught here).
// The condition allowance is narrow: a gate read is legitimate ONLY in the
// condition expression itself (`if gate() {}`), NOT in an `if x := gate(); ...`
// init clause (which could leak x into the body) and NOT as an argument to
// another call in the condition.

const rolloutPkgPath = "github.com/gastownhall/gascity/internal/rollout"

// promptRenderAnchors are the identifiers whose presence marks a file as part of
// the prompt-render path. The render set is derived from these (not hardcoded),
// so a new render call site auto-joins the guarded set.
var promptRenderAnchors = []string{
	"PromptContext", "renderPrompt", "renderPromptWithMeta", "buildTemplateData", "promptFuncMap",
}

// pinnedRenderFiles must always appear in the derived render set; their absence
// means the derivation broke (anti-vacuity).
var pinnedRenderFiles = []string{"prompt.go", "template_resolve.go", "cmd_prime.go", "cmd_lint.go"}

// flagValuePin names an accessor the derivation MUST rediscover per gate, so a
// config-side rename fails loudly instead of silently shrinking the guard.
var flagValuePins = map[string]string{
	"beads.conditional_writes": "NormalizedConditionalWrites",
	"daemon.formula_v2":        "FormulaV2Enabled",
}

// TestPromptRenderFilesGateRolloutFlags is the guarantee: no cmd/gc render file
// imports internal/rollout, and no flag-value identifier appears in a render file
// outside a control-flow condition.
func TestPromptRenderFilesGateRolloutFlags(t *testing.T) {
	fset := token.NewFileSet()
	files := parseNonTestGoFiles(t, fset, cmdGCDir(t))

	render := map[string]*ast.File{}
	for name, f := range files {
		if fileReferencesAnyIdent(f, promptRenderAnchors...) {
			render[name] = f
		}
	}
	for _, want := range pinnedRenderFiles {
		if _, ok := render[want]; !ok {
			t.Fatalf("render-file derivation missed %s — the anchor set or scan is broken (anti-vacuity)", want)
		}
	}
	// Each anchor must be referenced by at least one file, else a dropped or
	// renamed anchor silently shrinks the derivation.
	for _, anchor := range promptRenderAnchors {
		live := false
		for _, f := range files {
			if fileReferencesAnyIdent(f, anchor) {
				live = true
				break
			}
		}
		if !live {
			t.Fatalf("render anchor %q matches no cmd/gc non-test file — dead anchor or renamed helper (anti-vacuity)", anchor)
		}
	}

	forbidden := promptFlagValueIdents(t)
	for name, f := range render {
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == rolloutPkgPath {
				t.Errorf("%s renders prompts and imports %s; a render file must never reach the rollout subsystem", name, rolloutPkgPath)
			}
		}
		for _, v := range flagIdentsOutsideConditions(f, forbidden) {
			t.Errorf("%s: %q reaches prompt rendering outside a control-flow condition — %s; a rollout gate's VALUE must never flow into prompt content (reading it to branch is fine, assigning/returning it is not)", name, v.name, v.why)
		}
	}
}

// TestPromptBoundaryCheckerHasTeeth pins the core rule against neutering,
// independent of the production tree. A pure condition read is allowed; a value
// use (assignment, map write, closure return, or a value laundered as a call
// argument inside a condition, or a write inside an if-body) is flagged. Mutations
// that neuter the walk (mark(x.Cond)->mark(x), dropping the call-arg distinction,
// or making it vacuous) change this count and fail.
func TestPromptBoundaryCheckerHasTeeth(t *testing.T) {
	const src = `package p
type ctxT struct{ WorkQuery string }
func f(c *ctxT, b beadsT, td map[string]string) {
	if b.NormalizedConditionalWrites() != "" { // ALLOWED: pure condition read
		c.WorkQuery = b.NormalizedConditionalWrites() // banned: value in an if-body
	}
	c.WorkQuery = b.NormalizedConditionalWrites()  // banned: value into a prompt field
	td["cas"] = b.NormalizedConditionalWrites()    // banned: value into template-data map
	_ = func() string { return b.NormalizedConditionalWrites() } // banned: value out of a funcmap-shaped closure
	if stash(c, b.NormalizedConditionalWrites()) { // banned: value laundered as a call argument in a condition
	}
}
func stash(c *ctxT, v string) bool { c.WorkQuery = v; return true }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "teeth.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]string{"NormalizedConditionalWrites": "test accessor"}
	got := flagIdentsOutsideConditions(f, forbidden)
	if len(got) != 5 {
		t.Fatalf("checker teeth: want exactly 5 value-position violations (if-body, assignment, map write, closure return, call-arg-in-condition) and 0 for the pure condition read, got %d: %+v", len(got), got)
	}
}

// promptFlagValueIdents returns identifier name -> reason for every symbol that
// carries a rollout gate's value: the controller's latch field/accessor, each
// registered gate's backing config field, and — DERIVED BY PARSING METHOD BODIES,
// not by name convention — every config accessor that reads that field. Any
// accessor reading the gate field is caught regardless of its name; a pin per
// known gate turns a config refactor that loses the accessor into a loud failure.
func promptFlagValueIdents(t *testing.T) map[string]string {
	t.Helper()
	forbidden := map[string]string{
		"rolloutFlags": "the controller's boot-latched rollout.Flags field",
		"RolloutFlags": "the State rollout-flags accessor",
	}
	configDir := filepath.Join(repoRoot(t), "internal", "config")
	for _, s := range rollout.Specs() {
		leaf, _, ok := configLeafField(s.ConfigPath)
		if !ok {
			t.Fatalf("spec %s: ConfigPath %q did not resolve against config.City", s.Key, s.ConfigPath)
		}
		forbidden[leaf] = "the config field backing gate " + s.Key
		for _, name := range configFlagAccessors(t, configDir, leaf) {
			forbidden[name] = "a config accessor for gate " + s.Key
		}
		if want, has := flagValuePins[s.Key]; has {
			if _, ok := forbidden[want]; !ok {
				t.Fatalf("gate %s: accessor derivation lost %q — a config refactor must update flagValuePins so the guard is re-reviewed (anti-vacuity)", s.Key, want)
			}
		}
	}
	// Pin the hardcoded latch idents so deleting them fails loudly (they are not
	// registry-derived, so nothing else covers them).
	for _, want := range []string{"rolloutFlags", "RolloutFlags"} {
		if _, ok := forbidden[want]; !ok {
			t.Fatalf("forbidden set lost the latch ident %q (anti-vacuity)", want)
		}
	}
	return forbidden
}

// configLeafField walks config.City by dotted toml path and returns the leaf
// struct field's Go name and its owning struct type.
func configLeafField(path string) (leaf string, owner reflect.Type, ok bool) {
	t := reflect.TypeOf(config.City{})
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return "", nil, false
		}
		f, found := fieldByTOMLTag(t, seg)
		if !found {
			return "", nil, false
		}
		if i == len(segs)-1 {
			return f.Name, t, true
		}
		t = f.Type
	}
	return "", nil, false
}

func fieldByTOMLTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" {
			continue
		}
		if strings.Split(tag, ",")[0] == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// configFlagAccessors parses internal/config and returns the EXPORTED functions
// whose body reads the gate field `leaf` — directly (a `.leaf` selector, on ANY
// receiver or a plain function) or TRANSITIVELY (calls another accessor already
// in the set). Fixpoint over all functions (including unexported links in a
// chain), so an accessor named anything (CASWriteMode), one on a wrapping type
// (City reading .Beads.ConditionalWrites), a plain function, or a wrapper that
// only calls another accessor are all caught. Only exported names are returned,
// since a render file (a different package) can reference only those.
func configFlagAccessors(t *testing.T, configDir, leaf string) []string {
	t.Helper()
	type fn struct {
		name     string
		body     *ast.BlockStmt
		exported bool
	}
	var fns []fn
	entries, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", configDir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(configDir, name), nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				fns = append(fns, fn{fd.Name.Name, fd.Body, fd.Name.IsExported()})
			}
		}
	}

	inSet := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range fns {
			if inSet[f.name] {
				continue
			}
			if returnReadsFieldOrAccessor(f.body, leaf, inSet) {
				inSet[f.name] = true
				changed = true
			}
		}
	}

	var out []string
	for _, f := range fns {
		if inSet[f.name] && f.exported {
			out = append(out, f.name)
		}
	}
	return out
}

// returnReadsFieldOrAccessor reports whether any RETURN statement in body yields
// the gate field `leaf` (a `.leaf` selector) or a name already in `known` (a
// discovered accessor). Return-scoped, not whole-body: a true accessor RETURNS
// the field value, whereas a decoder/validator (Parse, LoadPack) merely touches
// the field while returning something else — the latter must not be forbidden,
// since render files legitimately call them.
func returnReadsFieldOrAccessor(body *ast.BlockStmt, leaf string, known map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range ret.Results {
			ast.Inspect(r, func(m ast.Node) bool {
				switch x := m.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == leaf || known[x.Sel.Name] {
						found = true
						return false
					}
				case *ast.Ident:
					if known[x.Name] {
						found = true
						return false
					}
				}
				return true
			})
		}
		return true
	})
	return found
}

type flagViolation struct {
	name string
	why  string
}

// flagIdentsOutsideConditions returns every forbidden identifier in f that is NOT
// inside a control-flow condition (an if/for condition, a switch tag, or a case
// expression). Idents in those positions are legitimate control-flow reads; every
// other position — assignments, returns, call arguments, composite literals — is a
// value use that could carry the flag into prompt data.
func flagIdentsOutsideConditions(f *ast.File, forbidden map[string]string) []flagViolation {
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.IfStmt:
			markConditionReads(x.Cond, allowed)
		case *ast.ForStmt:
			markConditionReads(x.Cond, allowed)
		case *ast.SwitchStmt:
			markConditionReads(x.Tag, allowed)
		case *ast.CaseClause:
			for _, e := range x.List {
				markConditionReads(e, allowed)
			}
		}
		return true
	})

	var out []flagViolation
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && !allowed[id] {
			if why, bad := forbidden[id.Name]; bad {
				out = append(out, flagViolation{name: id.Name, why: why})
			}
		}
		return true
	})
	return out
}

// markConditionReads marks identifiers in a control-flow condition as allowed
// reads — but ONLY those that feed the condition's own boolean/comparison logic,
// NOT those passed as ARGUMENTS to a call (`if stash(ctx, gate())` launders
// gate()'s value into stash's side effects, so gate() there is a value use, not a
// branch read). It threads an inArg flag through the expression tree; unhandled
// shapes mark nothing (conservative — a forbidden ident there is flagged).
func markConditionReads(cond ast.Node, allowed map[*ast.Ident]bool) {
	var walk func(n ast.Node, inArg bool)
	walk = func(n ast.Node, inArg bool) {
		switch x := n.(type) {
		case nil:
			return
		case *ast.Ident:
			if !inArg {
				allowed[x] = true
			}
		case *ast.SelectorExpr:
			walk(x.X, inArg)
			walk(x.Sel, inArg)
		case *ast.CallExpr:
			walk(x.Fun, inArg)
			for _, a := range x.Args {
				walk(a, true)
			}
		case *ast.BinaryExpr:
			walk(x.X, inArg)
			walk(x.Y, inArg)
		case *ast.UnaryExpr:
			walk(x.X, inArg)
		case *ast.ParenExpr:
			walk(x.X, inArg)
		case *ast.StarExpr:
			walk(x.X, inArg)
		case *ast.IndexExpr:
			walk(x.X, inArg)
			walk(x.Index, true)
		case *ast.IndexListExpr:
			walk(x.X, inArg)
			for _, i := range x.Indices {
				walk(i, true)
			}
		case *ast.BasicLit:
			// literal — nothing to mark
		default:
			// Unhandled expression shape: mark nothing, so a forbidden ident here
			// is reported rather than silently allowed.
		}
	}
	walk(cond, false)
}

func cmdGCDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir := cmdGCDir(t)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cmd/gc")
		}
		dir = parent
	}
}

func parseNonTestGoFiles(t *testing.T, fset *token.FileSet, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	return out
}

func fileReferencesAnyIdent(f *ast.File, names ...string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && set[id.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}
