package api_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Client half of the keyset pagination contract. pagination_dialect_guard_test.go
// pins the SERVER side (every list handler speaks cursor + limit); this file pins
// the CLIENT side: an internal/api function that calls a paged list endpoint must
// follow next_cursor, or be named in pagedCallerAllowlist with a reason.
//
// The defect class is a Client method that calls a paged endpoint once, keeps only
// Items and drops next_cursor, so the CLI prints the first page at rc 0 with no
// notice (ListBeads, ListSessions and ListConvoys each shipped that way once).
//
// A "paged endpoint" is a generated GetV0City...Params struct in
// internal/api/genclient with a Cursor field. Its callers are the functions in
// internal/api that call the generated GetV0City... / ...WithResponse method.
// A caller is compliant when it, or a package function it calls (by name,
// transitively), reads a NextCursor field.

// pagedCallerAllowlist names the functions that call a paged endpoint without
// following next_cursor, with the one-line reason that makes that safe. Adding an
// entry is a conscious contract decision: a list method that drops a page
// silently is the defect this guard exists to stop. Keys are "Client.Method" for
// methods and the bare name for functions.
var pagedCallerAllowlist = map[string]string{}

// pagedParamsWithoutClientCaller names the paged endpoints no internal/api
// function calls, with where the paging is handled instead. A paged endpoint in
// neither set is unclassified, so a new one cannot ship unnoticed.
var pagedParamsWithoutClientCaller = map[string]string{}

// pagedFuncInfo is what the guard records about one function declaration.
type pagedFuncInfo struct {
	calls       map[string]bool // bare names called, closures included
	readsCursor bool            // the body selects a field named NextCursor
}

// pagedParamsTypes returns the generated *Params struct names that carry a
// Cursor query field, sorted.
func pagedParamsTypes(t *testing.T, genSrc string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "client_gen.go", genSrc, 0)
	if err != nil {
		t.Fatalf("parse generated client: %v", err)
	}
	var out []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			st, isStruct := ts.Type.(*ast.StructType)
			if !ok || !isStruct || !strings.HasSuffix(ts.Name.Name, "Params") {
				continue
			}
			for _, f := range st.Fields.List {
				if f.Tag != nil && strings.Contains(f.Tag.Value, `form:"cursor`) {
					out = append(out, ts.Name.Name)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// pagedFuncIndex parses the package sources and records every function.
func pagedFuncIndex(t *testing.T, srcs map[string]string) map[string]*pagedFuncInfo {
	t.Helper()
	index := map[string]*pagedFuncInfo{}
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := parser.ParseFile(token.NewFileSet(), name, srcs[name], 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			info := &pagedFuncInfo{calls: map[string]bool{}}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch f := x.Fun.(type) {
					case *ast.Ident:
						info.calls[f.Name] = true
					case *ast.SelectorExpr:
						info.calls[f.Sel.Name] = true
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "NextCursor" {
						info.readsCursor = true
					}
				}
				return true
			})
			index[pagedFuncKey(fn)] = info
		}
	}
	return index
}

// pagedFuncKey is "Recv.Name" for a method and the bare name for a function.
func pagedFuncKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch x := typ.(type) {
	case *ast.Ident:
		return x.Name + "." + fn.Name.Name
	case *ast.IndexExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// pagedBareName is the part of a key a call expression can name.
func pagedBareName(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// reachesCursor reports whether fn, or a function it calls by name,
// transitively, reads a NextCursor field.
func reachesCursor(index map[string]*pagedFuncInfo, byName map[string][]string, key string, seen map[string]bool) bool {
	if seen[key] {
		return false
	}
	seen[key] = true
	info := index[key]
	if info.readsCursor {
		return true
	}
	for callee := range info.calls {
		for _, next := range byName[callee] {
			if reachesCursor(index, byName, next, seen) {
				return true
			}
		}
	}
	return false
}

// checkClientPagedDrains returns one violation per breach of the client half of
// the pagination contract. genSrc is the generated client source and srcs the
// non-test source of the package under guard (file name to content).
func checkClientPagedDrains(t *testing.T, genSrc string, srcs map[string]string, allow, noCaller map[string]string) []string {
	t.Helper()
	paged := pagedParamsTypes(t, genSrc)
	index := pagedFuncIndex(t, srcs)
	byName := map[string][]string{}
	for key := range index {
		byName[pagedBareName(key)] = append(byName[pagedBareName(key)], key)
	}

	// endpoint call name -> its Params type
	endpoint := map[string]string{}
	for _, p := range paged {
		base := strings.TrimSuffix(p, "Params")
		endpoint[base] = p
		endpoint[base+"WithResponse"] = p
	}

	var violations []string
	callerOf := map[string]map[string]bool{} // Params type -> caller keys
	callerParams := map[string]string{}      // caller key -> Params type
	keys := make([]string, 0, len(index))
	for key := range index {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for call := range index[key].calls {
			p, ok := endpoint[call]
			if !ok {
				continue
			}
			if callerOf[p] == nil {
				callerOf[p] = map[string]bool{}
			}
			callerOf[p][key] = true
			callerParams[key] = p
		}
	}

	for _, key := range keys {
		p, isCaller := callerParams[key]
		if !isCaller {
			continue
		}
		drains := reachesCursor(index, byName, key, map[string]bool{})
		reason, allowed := allow[key]
		switch {
		case !drains && !allowed:
			violations = append(violations, fmt.Sprintf(
				"%s calls a paged endpoint (%s has a Cursor field) and never reads NextCursor: it returns the first page and drops the rest silently. Drain every page (see drainBeadPages) or, if one page is the contract, add it to pagedCallerAllowlist with a one-line reason", key, p))
		case drains && allowed:
			violations = append(violations, fmt.Sprintf(
				"%s is allowlisted in pagedCallerAllowlist but now follows NextCursor: remove its entry", key))
		case allowed && strings.TrimSpace(reason) == "":
			violations = append(violations, fmt.Sprintf(
				"%s is allowlisted in pagedCallerAllowlist without a reason: say why one page is enough", key))
		}
	}
	for key := range allow {
		if _, isCaller := callerParams[key]; !isCaller {
			violations = append(violations, fmt.Sprintf(
				"%s is in pagedCallerAllowlist but no longer calls a paged endpoint: remove its entry", key))
		}
	}

	for _, p := range paged {
		reason, classified := noCaller[p]
		switch {
		case len(callerOf[p]) == 0 && !classified:
			violations = append(violations, fmt.Sprintf(
				"%s has a Cursor field but no function in the package calls its endpoint: add it to pagedParamsWithoutClientCaller and say where its paging is handled", p))
		case len(callerOf[p]) > 0 && classified:
			violations = append(violations, fmt.Sprintf(
				"%s is in pagedParamsWithoutClientCaller but a Client function now calls it: remove its entry so the caller is held to the drain rule", p))
		case classified && strings.TrimSpace(reason) == "":
			violations = append(violations, fmt.Sprintf(
				"%s is in pagedParamsWithoutClientCaller without a reason", p))
		}
	}
	pagedSet := map[string]bool{}
	for _, p := range paged {
		pagedSet[p] = true
	}
	for p := range noCaller {
		if !pagedSet[p] {
			violations = append(violations, fmt.Sprintf(
				"%s is in pagedParamsWithoutClientCaller but is not a paged Params type in the generated client: remove its entry", p))
		}
	}
	sort.Strings(violations)
	return violations
}

// TestClientPagedListMethodsDrainOrAreAllowlisted holds the real package to the
// rule: a function that calls a paged endpoint follows next_cursor or is
// allowlisted with a reason.
func TestClientPagedListMethodsDrainOrAreAllowlisted(t *testing.T) {
	gen, err := os.ReadFile(filepath.Join("genclient", "client_gen.go"))
	if err != nil {
		t.Fatalf("read generated client: %v", err)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs[f] = string(b)
	}
	if len(srcs) == 0 {
		t.Fatal("no non-test sources found in the package directory")
	}
	for _, v := range checkClientPagedDrains(t, string(gen), srcs, pagedCallerAllowlist, pagedParamsWithoutClientCaller) {
		t.Error(v)
	}
}

// pagedFixtureGen is a generated client with one paged and one unpaged endpoint.
const pagedFixtureGen = `package genclient

type GetV0CityByCityNameWidgetsParams struct {
	Cursor *string ` + "`form:\"cursor,omitempty\" json:\"cursor,omitempty\"`" + `
	Limit  *int64  ` + "`form:\"limit,omitempty\" json:\"limit,omitempty\"`" + `
}

type GetV0CityByCityNameGadgetsParams struct {
	Limit *int64 ` + "`form:\"limit,omitempty\" json:\"limit,omitempty\"`" + `
}
`

// TestCheckClientPagedDrainsFlagsADroppedCursor proves the checker itself: it
// fails the lossy shape and passes each compliant shape, so a green run of the
// real-package test above means something.
func TestCheckClientPagedDrainsFlagsADroppedCursor(t *testing.T) {
	const lossy = `package api
func (c *Client) ListWidgets() { c.cw.GetV0CityByCityNameWidgetsWithResponse(nil, "", nil) }
`
	const inlineDrain = `package api
func (c *Client) ListWidgets() {
	resp, _ := c.cw.GetV0CityByCityNameWidgetsWithResponse(nil, "", nil)
	_ = resp.JSON200.NextCursor
}
`
	const helperDrain = `package api
func (c *Client) ListWidgets() {
	resp, _ := c.cw.GetV0CityByCityNameWidgetsWithResponse(nil, "", nil)
	_ = nextOf(resp.JSON200)
}
func nextOf(b *Body) string { return *b.NextCursor }
`
	const delegatedLossy = `package api
func (c *Client) widgetsPage() { c.cw.GetV0CityByCityNameWidgetsWithResponse(nil, "", nil); itemsOf(nil) }
func itemsOf(b *Body) []int { return b.Items }
`
	const unpaged = `package api
func (c *Client) ListGadgets() { c.cw.GetV0CityByCityNameGadgetsWithResponse(nil, "", nil) }
`
	noCaller := map[string]string{"GetV0CityByCityNameWidgetsParams": "paged by the CLI"}
	widgets := "GetV0CityByCityNameWidgetsParams"

	cases := []struct {
		name     string
		src      string
		allow    map[string]string
		noCaller map[string]string
		want     []string // substrings, one per expected violation; empty means clean
	}{
		{name: "dropped cursor is flagged", src: lossy, want: []string{"Client.ListWidgets calls a paged endpoint"}},
		{name: "inline drain passes", src: inlineDrain},
		{name: "drain through a helper passes", src: helperDrain},
		{name: "delegated lossy page is flagged", src: delegatedLossy, want: []string{"Client.widgetsPage calls a paged endpoint"}},
		{name: "unpaged endpoint is ignored", src: unpaged, noCaller: noCaller},
		{name: "allowlisted lossy caller passes", src: lossy, allow: map[string]string{"Client.ListWidgets": "one page is the contract"}},
		{name: "allowlist entry needs a reason", src: lossy, allow: map[string]string{"Client.ListWidgets": " "}, want: []string{"without a reason"}},
		{name: "allowlist entry for a draining caller is stale", src: inlineDrain, allow: map[string]string{"Client.ListWidgets": "old"}, want: []string{"now follows NextCursor"}},
		{name: "allowlist entry for a missing caller is stale", src: unpaged, allow: map[string]string{"Client.ListWidgets": "old"}, noCaller: noCaller, want: []string{"no longer calls a paged endpoint"}},
		{name: "paged endpoint nobody calls is unclassified", src: unpaged, want: []string{widgets + " has a Cursor field but no function"}},
		{name: "classified no-caller endpoint passes", src: unpaged, noCaller: noCaller},
		{name: "no-caller entry for a called endpoint is stale", src: inlineDrain, noCaller: noCaller, want: []string{"a Client function now calls it"}},
		{name: "no-caller entry for an unknown type is stale", src: unpaged, noCaller: map[string]string{widgets: "x", "NopeParams": "x"}, want: []string{"not a paged Params type"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkClientPagedDrains(t, pagedFixtureGen, map[string]string{"client.go": tc.src}, tc.allow, tc.noCaller)
			if len(got) != len(tc.want) {
				t.Fatalf("violations = %q, want %d matching %q", got, len(tc.want), tc.want)
			}
			sort.Strings(tc.want)
			for i := range got {
				if !strings.Contains(got[i], tc.want[i]) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
