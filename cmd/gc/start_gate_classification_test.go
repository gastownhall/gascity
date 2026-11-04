package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The start-deferral memo (start_deferral_memo.go) records the candidates a
// start GATE declines, so the pending-create rollback keeps them past the
// lease. Which start-op outcomes are gates was a judgment kept in a comment
// that listed two of them; the prepare-budget gate (#8025) landed without
// being listed. This file replaces the list with a criterion and a closed
// table, and fails when a start-op outcome has no row.
//
// A start-op outcome RECORDS when its site meets all three clauses:
//
//	(a) it declines a ready candidate this pass for a reason independent of
//	    the candidate's own state: shared throughput, not anything about it;
//	(b) it refuses nothing: the candidate is re-offered next pass, and at
//	    least one candidate is admitted per pass, so the queue drains;
//	(c) it writes nothing to any bead.
//
// A gate with a hold of its own (endpoint capacity) does not record: its hold
// already keeps the row.

// startOutcomeClass is one row of the closed classification table.
type startOutcomeClass struct {
	// records reports that the outcome's site calls noteGateDeferred.
	records bool
	// reason, for a row that does not record, is the one-line reason: the
	// failed clause, an own hold, a post-attempt outcome, or the cancel rule.
	reason string
}

// startOutcomeClassification maps every outcome value a start-op
// logLifecycleOutcome call can report to whether it records. The scan in
// scanStartOutcomes is the authority on which values exist.
var startOutcomeClassification = map[string]startOutcomeClass{
	"deferred_by_wake_budget":       {records: true},
	"deferred_by_async_start_limit": {records: true},
	"deferred_by_prepare_budget":    {records: true},

	"blocked_on_dependencies":            {reason: "clause (a): the candidate's own dependencies are not alive"},
	"deferred_by_on_death_hook":          {reason: "clause (a): its own runtime name's on_death hook is pending"},
	"deferred_by_runtime_lease":          {reason: "clause (a): its own runtime name is leased by another holder"},
	"deferred_by_endpoint_capacity":      {reason: "own hold: endpointCapacityGuard.HoldsPendingCreate already keeps the row"},
	"user_held":                          {reason: "clause (a): the session's own state, held by the user"},
	"context_canceled":                   {reason: "the cancel rule: a canceled run commits nothing"},
	"abandoned_at_wave":                  {reason: "lifecycle result after gate admission: wave abandonment is not an admission gate and may commit lifecycle cleanup"},
	"start_boundary_liveness_unanswered": {reason: "lifecycle result after gate admission: an unanswered runtime observation may commit a store-only deferral"},
	"start_store_only":                   {reason: "lifecycle result after gate admission: an unverifiable pre-start observation may commit store-only lifecycle state"},

	// Post-attempt: the candidate was offered a start and the attempt ran.
	"async_start_drift_rolled_back": {reason: "post-attempt: an async start that ran and was rolled back on drift"},
	"async_start_refresh_failed":    {reason: "post-attempt: an async start whose result refresh failed"},
	"stale_async_start":             {reason: "post-attempt: an async start whose prepared state went stale"},
	"capacity_refused":              {reason: "post-attempt: the endpoint refused a start that was attempted"},
	"circuit_metadata_failed":       {reason: "post-attempt: the breaker's restart accounting failed (clause c: it writes)"},
	"failed":                        {reason: "post-attempt: the start or its runtime record failed"},
	"metadata_batch_failed":         {reason: "post-attempt: the start commit's metadata write failed"},
	"metadata_encode_failed":        {reason: "post-attempt: the start commit's metadata encode failed"},
	"runtime_mcp_snapshot_failed":   {reason: "post-attempt: the runtime MCP snapshot failed after the start"},
	"panic_recovered":               {reason: "post-attempt: the start panicked"},
	"canceled":                      {reason: "post-attempt: the start's context was canceled mid-attempt"},
	"deadline_exceeded":             {reason: "post-attempt: the start's deadline passed mid-attempt"},
	"deferred":                      {reason: "post-attempt: the provider start deferred an unattributable runtime"},
	"provider_error":                {reason: "post-attempt: the provider start failed"},
	"session_exists":                {reason: "post-attempt: the runtime session already exists"},
	"session_exists_converged":      {reason: "post-attempt: the runtime session was ours and converged"},
	"session_initializing":          {reason: "post-attempt: the runtime session is still initializing"},
	"start_enqueued":                {reason: "post-attempt: the async start was enqueued, not deferred"},
	"start_error_converged":         {reason: "post-attempt: a start error converged with an existing runtime"},
	"success":                       {reason: "post-attempt: the start succeeded"},
}

// startGateOutcomeClauses is quoted in every scan failure.
const startGateOutcomeClauses = `a start-op outcome records only when its site (a) declines a ready candidate for a reason independent of the candidate's own state, (b) refuses nothing, so the candidate is re-offered next pass, and (c) writes nothing to any bead; add a row to startOutcomeClassification in start_gate_classification_test.go saying which, and wire noteGateDeferred at the site when it records`

// startOutcomeScan is the result of scanning the start-op logLifecycleOutcome
// calls of a set of Go sources.
type startOutcomeScan struct {
	fset   *token.FileSet
	decls  map[string][]*ast.FuncDecl
	owner  map[*ast.CallExpr]*ast.FuncDecl
	calls  map[string][]*ast.CallExpr
	consts map[string]string
	files  []*ast.File
	// values maps each reachable start-op outcome value to its sites.
	values map[string][]string
	// errs are outcome expressions the scan could not resolve.
	errs []string
}

// scanStartOutcomes parses sources (file name -> text) and resolves the
// outcome argument of every logLifecycleOutcome call whose op is "start".
// An argument it cannot resolve is an error, never a silent skip.
func scanStartOutcomes(sources map[string]string) (*startOutcomeScan, error) {
	s := &startOutcomeScan{
		fset:   token.NewFileSet(),
		decls:  map[string][]*ast.FuncDecl{},
		owner:  map[*ast.CallExpr]*ast.FuncDecl{},
		calls:  map[string][]*ast.CallExpr{},
		consts: map[string]string{},
		values: map[string][]string{},
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(s.fset, name, sources[name], 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		s.files = append(s.files, f)
	}
	for _, f := range s.files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								s.consts[n.Name] = v
							}
						}
					}
				}
			case *ast.FuncDecl:
				s.decls[d.Name.Name] = append(s.decls[d.Name.Name], d)
				if d.Body == nil {
					continue
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						s.owner[call] = d
						s.calls[scanCalleeName(call)] = append(s.calls[scanCalleeName(call)], call)
					}
					return true
				})
			}
		}
	}
	for _, f := range s.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "logLifecycleOutcome" || len(call.Args) < 6 {
					return true
				}
				if s.src(call.Args[1]) != `"start"` {
					return true
				}
				site := s.fset.Position(call.Pos()).String()
				vals, err := s.resolve(call.Args[5], 0, fn, map[string]bool{})
				if err != nil {
					s.errs = append(s.errs, fmt.Sprintf("%s: cannot resolve the outcome argument %s: %v; %s", site, s.src(call.Args[5]), err, startGateOutcomeClauses))
					return true
				}
				for _, v := range vals {
					if v != "" { // an empty outcome logs nothing
						s.values[v] = append(s.values[v], site)
					}
				}
				return true
			})
		}
	}
	return s, nil
}

func scanCalleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func (s *startOutcomeScan) src(n ast.Node) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, s.fset, n)
	return b.String()
}

// resolve returns every string value expression e (the idx-th value of a
// tuple-valued expression) can take inside fn. The empty string is a value.
func (s *startOutcomeScan) resolve(e ast.Expr, idx int, fn *ast.FuncDecl, seen map[string]bool) ([]string, error) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return s.resolve(x.X, idx, fn, seen)
	case *ast.BasicLit:
		if x.Kind == token.STRING && idx == 0 {
			v, err := strconv.Unquote(x.Value)
			return []string{v}, err
		}
	case *ast.CallExpr:
		name := scanCalleeName(x)
		if (name == "string" || name == "TraceOutcomeCode") && len(x.Args) == 1 && idx == 0 {
			return s.resolve(x.Args[0], 0, fn, seen)
		}
		decls := s.decls[name]
		if len(decls) == 0 {
			return nil, fmt.Errorf("call to %s, which is not declared in the scanned files", s.src(x.Fun))
		}
		var out []string
		for _, d := range decls {
			vals, err := s.returns(d, idx, seen)
			if err != nil {
				return nil, err
			}
			out = append(out, vals...)
		}
		return out, nil
	case *ast.Ident:
		if v, ok := s.consts[x.Name]; ok && idx == 0 {
			return []string{v}, nil
		}
		if idx == 0 {
			return s.variable(x.Name, fn, seen)
		}
	case *ast.SelectorExpr:
		if x.Sel.Name == "outcome" && idx == 0 {
			return s.outcomeField(seen)
		}
	}
	return nil, fmt.Errorf("an expression of a shape the scan does not follow")
}

// returns resolves the idx-th result of every return statement of d.
func (s *startOutcomeScan) returns(d *ast.FuncDecl, idx int, seen map[string]bool) ([]string, error) {
	key := fmt.Sprintf("return|%s|%d", d.Name.Name, idx)
	if seen[key] {
		return nil, nil
	}
	seen[key] = true
	if d.Body == nil {
		return nil, fmt.Errorf("function %s has no body", d.Name.Name)
	}
	results := 0
	if d.Type.Results != nil {
		for _, f := range d.Type.Results.List {
			results += max(1, len(f.Names))
		}
	}
	var out []string
	var failure error
	ast.Inspect(d.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || failure != nil {
			return true
		}
		var vals []string
		var err error
		switch {
		case len(ret.Results) == results && idx < results:
			vals, err = s.resolve(ret.Results[idx], 0, d, seen)
		case len(ret.Results) == 1 && results > 1:
			vals, err = s.resolve(ret.Results[0], idx, d, seen)
		default:
			err = fmt.Errorf("a return in %s the scan does not follow", d.Name.Name)
		}
		if err != nil {
			failure = err
			return true
		}
		out = append(out, vals...)
		return true
	})
	return out, failure
}

// variable resolves a local variable or parameter of fn by every assignment to
// its name in fn, and, for a parameter, every call site's argument.
func (s *startOutcomeScan) variable(name string, fn *ast.FuncDecl, seen map[string]bool) ([]string, error) {
	key := fmt.Sprintf("var|%s|%s", fn.Name.Name, name)
	if seen[key] {
		return nil, nil
	}
	seen[key] = true
	var out []string
	found := false
	var failure error
	add := func(vals []string, err error) {
		found = true
		if err != nil && failure == nil {
			failure = err
		}
		out = append(out, vals...)
	}
	assigned := func(lhs []ast.Expr, rhs []ast.Expr) {
		for i, l := range lhs {
			if id, ok := l.(*ast.Ident); !ok || id.Name != name {
				continue
			}
			switch {
			case len(lhs) == len(rhs):
				add(s.resolve(rhs[i], 0, fn, seen))
			case len(rhs) == 1:
				add(s.resolve(rhs[0], i, fn, seen))
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			assigned(x.Lhs, x.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(x.Names))
			for i, id := range x.Names {
				lhs[i] = id
			}
			if len(x.Values) > 0 {
				assigned(lhs, x.Values)
			} else {
				for _, id := range x.Names {
					if id.Name == name {
						found = true // a zero value that later assignments overwrite
					}
				}
			}
		}
		return true
	})
	// A parameter of fn itself: the union of its call sites' arguments.
	if i := scanParamIndex(fn.Type, name); i >= 0 {
		found = true
		for _, call := range s.calls[fn.Name.Name] {
			if i >= len(call.Args) {
				failure = fmt.Errorf("a call to %s passes no argument for %s", fn.Name.Name, name)
				continue
			}
			caller := s.owner[call]
			if caller == nil {
				failure = fmt.Errorf("a call to %s outside any function", fn.Name.Name)
				continue
			}
			vals, err := s.resolve(call.Args[i], 0, caller, seen)
			if err != nil && failure == nil {
				failure = err
			}
			out = append(out, vals...)
		}
	}
	// A parameter of a closure bound to a local: the union of that local's
	// call arguments.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		i := scanParamIndex(lit.Type, name)
		bound, ok := as.Lhs[0].(*ast.Ident)
		if i < 0 || !ok {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == bound.Name && i < len(call.Args) {
				vals, err := s.resolve(call.Args[i], 0, fn, seen)
				if err != nil && failure == nil {
					failure = err
				}
				out = append(out, vals...)
			}
			return true
		})
		return true
	})
	if failure != nil {
		return nil, failure
	}
	if !found {
		return nil, fmt.Errorf("variable %s is never assigned in %s", name, fn.Name.Name)
	}
	return out, nil
}

func scanParamIndex(ft *ast.FuncType, name string) int {
	i := 0
	for _, f := range ft.Params.List {
		if len(f.Names) == 0 {
			i++
			continue
		}
		for _, n := range f.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// outcomeField resolves startResult.outcome: every value a startResult literal
// or a selector assignment gives the field.
func (s *startOutcomeScan) outcomeField(seen map[string]bool) ([]string, error) {
	if seen["field|outcome"] {
		return nil, nil
	}
	seen["field|outcome"] = true
	var out []string
	var failure error
	for _, f := range s.files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			add := func(e ast.Expr) {
				vals, err := s.resolve(e, 0, fn, seen)
				if err != nil && failure == nil {
					failure = err
				}
				out = append(out, vals...)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); !ok || id.Name != "startResult" {
						return true
					}
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "outcome" {
								add(kv.Value)
							}
						}
					}
				case *ast.AssignStmt:
					for i, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "outcome" && len(x.Lhs) == len(x.Rhs) {
							add(x.Rhs[i])
						}
					}
				}
				return true
			})
		}
	}
	return out, failure
}

// checkStartOutcomeTable returns every way table fails to match scan: a
// reachable value with no row, a row naming no reachable value, an outcome
// expression the scan could not resolve, a non-recording row with no reason.
func checkStartOutcomeTable(scan *startOutcomeScan, table map[string]startOutcomeClass) []string {
	problems := slices.Clone(scan.errs)
	for _, v := range slices.Sorted(scanMapKeys(scan.values)) {
		if _, ok := table[v]; !ok {
			problems = append(problems, fmt.Sprintf("start-op outcome %q (reported at %s) has no row in startOutcomeClassification; %s", v, strings.Join(scan.values[v], ", "), startGateOutcomeClauses))
		}
	}
	for _, v := range slices.Sorted(scanMapKeys(table)) {
		row := table[v]
		if _, ok := scan.values[v]; !ok {
			problems = append(problems, fmt.Sprintf("startOutcomeClassification has a row for %q, but no start-op logLifecycleOutcome call can report it; remove the row", v))
		}
		if row.records {
			continue
		}
		reason := strings.TrimSpace(row.reason)
		switch strings.ToLower(reason) {
		case "", "pending", "tbd", "todo":
			problems = append(problems, fmt.Sprintf("startOutcomeClassification row %q does not record and has no reason (got %q); name the failed clause, the own hold, the post-attempt outcome or the cancel rule", v, row.reason))
		}
	}
	return problems
}

func scanMapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// startOutcomeSources reads the files the scan covers from the package
// directory (bazel runfiles-aware, as the worker-boundary guard does).
func startOutcomeSources(t *testing.T) map[string]string {
	t.Helper()
	_, currentFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	sources := map[string]string{}
	for _, name := range []string{"session_lifecycle_parallel.go", "session_reconciler_trace_types.go"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		sources[name] = string(data)
	}
	return sources
}

func TestStartOutcomeClassificationCoversEveryStartOpOutcome(t *testing.T) {
	scan, err := scanStartOutcomes(startOutcomeSources(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.values) == 0 {
		t.Fatal("the scan found no start-op outcome values: it is not reading the executor")
	}
	if problems := checkStartOutcomeTable(scan, startOutcomeClassification); len(problems) > 0 {
		t.Fatal("\n" + strings.Join(problems, "\n"))
	}
}

// The scan is the authority, so its failure modes are pinned on small sources
// rather than only on the real executor: a new literal, a new constant and an
// expression it cannot follow each fail it.
func TestStartOutcomeScanFailsClosed(t *testing.T) {
	const types = "package main\nconst TraceOutcomeKnown TraceOutcomeCode = \"known\"\nconst TraceOutcomeNovel TraceOutcomeCode = \"novel\"\n"
	table := map[string]startOutcomeClass{
		"known": {records: true},
		"lit":   {reason: "post-attempt"},
	}
	run := func(body string, table map[string]startOutcomeClass) []string {
		t.Helper()
		scan, err := scanStartOutcomes(map[string]string{
			"exec.go":  "package main\nfunc f(w io.Writer, other string) {\n" + body + "\n}\n",
			"types.go": types,
		})
		if err != nil {
			t.Fatal(err)
		}
		return checkStartOutcomeTable(scan, table)
	}
	const known = `logLifecycleOutcome(w, "start", 0, "n", "t", string(TraceOutcomeKnown), time.Time{}, time.Time{}, nil)
logLifecycleOutcome(w, "start", 0, "n", "t", "lit", time.Time{}, time.Time{}, nil)
logLifecycleOutcome(w, "stop", 0, "n", "t", "stop_only", time.Time{}, time.Time{}, nil)`

	if got := run(known, table); len(got) != 0 {
		t.Fatalf("a table matching the source reported %v", got)
	}
	for _, tc := range []struct {
		name  string
		body  string
		table map[string]startOutcomeClass
		want  string
	}{
		{"M1 a new literal outcome", known + "\nlogLifecycleOutcome(w, \"start\", 0, \"n\", \"t\", \"brand_new\", time.Time{}, time.Time{}, nil)", table, `start-op outcome "brand_new"`},
		{"M2 a new constant outcome", known + "\nlogLifecycleOutcome(w, \"start\", 0, \"n\", \"t\", string(TraceOutcomeNovel), time.Time{}, time.Time{}, nil)", table, `start-op outcome "novel"`},
		{"M4 a deleted row", known, map[string]startOutcomeClass{"known": {records: true}}, `start-op outcome "lit"`},
		{"M5 a stale row", known, map[string]startOutcomeClass{"known": {records: true}, "lit": {reason: "post-attempt"}, "ghost": {reason: "post-attempt"}}, `row for "ghost"`},
		{"M6 an expression the scan cannot follow", known + "\nlogLifecycleOutcome(w, \"start\", 0, \"n\", \"t\", fmt.Sprint(other), time.Time{}, time.Time{}, nil)", table, "cannot resolve the outcome argument fmt.Sprint(other)"},
		{"M6 a variable never assigned", known + "\nlogLifecycleOutcome(w, \"start\", 0, \"n\", \"t\", ghost, time.Time{}, time.Time{}, nil)", table, "cannot resolve the outcome argument ghost"},
		{"an empty reason", known, map[string]startOutcomeClass{"known": {records: true}, "lit": {reason: " "}}, `row "lit" does not record and has no reason`},
		{"a placeholder reason", known, map[string]startOutcomeClass{"known": {records: true}, "lit": {reason: "TODO"}}, `row "lit" does not record and has no reason`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(run(tc.body, tc.table), "\n")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("problems = %q, want one containing %q", got, tc.want)
			}
			if !strings.Contains(got, "(a) declines") && !strings.Contains(got, "no reason") && !strings.Contains(got, "remove the row") {
				t.Fatalf("problems = %q: neither the three clauses nor a row fix is named", got)
			}
		})
	}
}

// The scan resolves each shape the executor reports an outcome in: a helper's
// tuple return, a closure parameter, a typed struct field and a conversion.
func TestStartOutcomeScanResolvesTheExecutorShapes(t *testing.T) {
	scan, err := scanStartOutcomes(map[string]string{
		"exec.go": `package main
type startResult struct{ outcome TraceOutcomeCode }
func reserve() (func(), bool, string) { return nil, false, "from_helper" }
func settle() string { return "from_settle" }
func build() startResult {
	var o TraceOutcomeCode
	o = TraceOutcomeA
	return startResult{outcome: o}
}
func run(w io.Writer) {
	var out string
	_, _, out = reserve()
	logLifecycleOutcome(w, "start", 0, "n", "t", out, time.Time{}, time.Time{}, nil)
	logLifecycleOutcome(w, "start", 0, "n", "t", settle(), time.Time{}, time.Time{}, nil)
	r := build()
	r.outcome = TraceOutcomeB
	logLifecycleOutcome(w, "start", 0, "n", "t", string(r.outcome), time.Time{}, time.Time{}, nil)
	drop := func(outcome string) {
		logLifecycleOutcome(w, "start", 0, "n", "t", outcome, time.Time{}, time.Time{}, nil)
	}
	drop("")
	drop("closure_arg")
}
`,
		"types.go": "package main\nconst (\nTraceOutcomeA TraceOutcomeCode = \"a\"\nTraceOutcomeB TraceOutcomeCode = \"b\"\n)\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.errs) != 0 {
		t.Fatalf("unresolved: %v", scan.errs)
	}
	got := slices.Sorted(scanMapKeys(scan.values))
	want := []string{"a", "b", "closure_arg", "from_helper", "from_settle"}
	if !slices.Equal(got, want) {
		t.Fatalf("values = %v, want %v (the empty outcome logs nothing and is dropped)", got, want)
	}
}

// startGateRecordingDrivers is a function, not a package-level map, so the
// drivers' test relaxations stay scoped to the tests that call it (testrelax).
// It drives one recording row each: after an uncanceled
// run with the memo installed, the candidate the gate deferred is in the
// committed set. The table is the iteration source, so a recording row with no
// driver fails.
func startGateRecordingDrivers() map[string]func(t *testing.T) {
	return map[string]func(t *testing.T){
		"deferred_by_wake_budget":       driveWakeBudgetRecords,
		"deferred_by_async_start_limit": driveAsyncLimitRecords,
		"deferred_by_prepare_budget":    drivePrepareBudgetRecords,
	}
}

// startGateNegativeDrivers drives a row that does not record: the gate defers
// a candidate and the memo commits nothing. startGateRequiredNegatives names
// the rows that must have one.
func startGateNegativeDrivers() map[string]func(t *testing.T) {
	return map[string]func(t *testing.T){
		"blocked_on_dependencies":       driveDependencyBlocksRecordNothing,
		"deferred_by_on_death_hook":     driveOnDeathHookRecordsNothing,
		"deferred_by_runtime_lease":     driveRuntimeLeaseRecordsNothing,
		"deferred_by_endpoint_capacity": driveEndpointCapacityRecordsNothing,
	}
}

var startGateRequiredNegatives = []string{
	"blocked_on_dependencies",
	"deferred_by_on_death_hook",
	"deferred_by_runtime_lease",
	"deferred_by_endpoint_capacity",
}

func TestStartOutcomeClassificationDrivers(t *testing.T) {
	recording, negative := startGateRecordingDrivers(), startGateNegativeDrivers()
	for _, v := range slices.Sorted(scanMapKeys(startOutcomeClassification)) {
		row := startOutcomeClassification[v]
		if !row.records {
			continue
		}
		driver, ok := recording[v]
		if !ok {
			t.Errorf("recording row %q has no driver in startGateRecordingDrivers: a gate that records must be shown to record", v)
			continue
		}
		t.Run("records/"+v, driver)
	}
	for v := range recording {
		if row, ok := startOutcomeClassification[v]; !ok || !row.records {
			t.Errorf("startGateRecordingDrivers has a driver for %q, which is not a recording row", v)
		}
	}
	for _, v := range startGateRequiredNegatives {
		if _, ok := negative[v]; !ok {
			t.Errorf("non-recording row %q has no negative driver", v)
		}
	}
	for _, v := range slices.Sorted(scanMapKeys(negative)) {
		if row, ok := startOutcomeClassification[v]; !ok || row.records {
			t.Errorf("startGateNegativeDrivers has a driver for %q, which is not a non-recording row", v)
			continue
		}
		t.Run("records-nothing/"+v, negative[v])
	}
}

// requireCommitted fails unless the memo's committed set is exactly want.
func requireCommitted(t *testing.T, memo *startDeferralMemo, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := committedDeferrals(memo); !slices.Equal(got, want) {
		t.Fatalf("committed = %v, want %v", got, want)
	}
}

func driveWakeBudgetRecords(t *testing.T) {
	t.Run("wave level", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
		e.limitToOneWake()
		e.cfg.Agents[1].DependsOn = []string{"a"}
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		e.start(context.Background(), a, b)
		if !e.sp.IsRunning("a") || e.sp.IsRunning("b") {
			t.Fatalf("premise: a running=%v b running=%v, want only the first wave started", e.sp.IsRunning("a"), e.sp.IsRunning("b"))
		}
		requireCommitted(t, memo, b.info.ID)
	})
	t.Run("ready loop", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
		e.limitToOneWake()
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		e.start(context.Background(), a, b)
		deferred := a.info.ID
		if e.sp.IsRunning("a") {
			deferred = b.info.ID
		}
		requireCommitted(t, memo, deferred)
	})
}

func driveAsyncLimitRecords(t *testing.T) {
	e := newCapacityEnv(t, false, "a")
	memo := newStartDeferralMemo()
	limiter := newAsyncStartLimiter(1)
	release, reserved, outcome := limiter.reserve(context.Background())
	if !reserved {
		t.Fatalf("premise: reserve = %q, want the only slot", outcome)
	}
	defer release()
	var tracker asyncStartTracker
	opts := append(slices.Clone(e.startOptions), withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()), withAsyncStartExecution(), withAsyncStartLimiter(limiter), withAsyncStartTracker(&tracker))
	a := e.pendingCreate(t, "a", "", nil)

	executePlannedStartsTraced(context.Background(), []startCandidate{a}, e.cfg, e.desiredState, e.sp, e.store, "", e.city, e.clk, e.rec, 0, &e.log, &e.log, nil, opts...)
	if !tracker.wait(hangBudget) {
		t.Fatalf("async starts did not finish within the hang budget (%s)", hangBudget)
	}
	if e.sp.IsRunning("a") {
		t.Fatal("premise: a started although the limiter had no free slot")
	}
	requireCommitted(t, memo, a.info.ID)
}

// A spent prepare budget defers the rest of the planner call before any slot,
// lease, ticket or write: shared throughput, nothing refused, nothing written.
// The candidate that spent the budget was admitted and is not recorded.
func drivePrepareBudgetRecords(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e := newPrepareBudgetEnv(t, "a", "b", "c")
			memo := newStartDeferralMemo()
			a := e.candidate(t, "a", testPrepareBudget+time.Second)
			b := e.candidate(t, "b", time.Second)
			c := e.candidate(t, "c", time.Second)

			e.run(testPrepareBudget, async, []startExecutionOption{withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet())}, a, b, c)
			e.requireDeferred(t, async, "b", "c")
			requireCommitted(t, memo, b.info.ID, c.info.ID)
		})
	}
	t.Run("a budget that is not spent records nothing", func(t *testing.T) {
		e := newPrepareBudgetEnv(t, "a", "b")
		memo := newStartDeferralMemo()
		a := e.candidate(t, "a", time.Second)
		b := e.candidate(t, "b", time.Second)

		if woken := e.run(testPrepareBudget, false, []startExecutionOption{withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet())}, a, b); woken != 2 {
			t.Fatalf("woken = %d, want 2: both fit the budget", woken)
		}
		requireCommitted(t, memo)
	})
	t.Run("the hold keeps the deferred create past the lease and releases it at the ceiling", func(t *testing.T) {
		e := newPrepareBudgetEnv(t, "a", "b")
		memo := newStartDeferralMemo()
		a := e.candidate(t, "a", testPrepareBudget+time.Second)
		b := e.candidate(t, "b", time.Second)
		// The store stamps CreatedAt from wall time: begin the pass after
		// both rows exist, so only the memo's record can hold b.
		e.capacityEnv.clk.Time = time.Now().UTC().Add(time.Minute)

		e.run(testPrepareBudget, false, []startExecutionOption{withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet())}, a, b)
		e.requireDeferred(t, false, "b")
		info := e.sessionInfo(b.info.ID)
		anchor := pendingCreateLeaseAnchor(info)
		if anchor.IsZero() {
			t.Fatal("premise: the deferred create has no lease anchor")
		}
		held := func(now time.Time) bool {
			return pendingCreateHold(nil, memo, &clock.Fake{Time: now})(info)
		}
		if !held(anchor.Add(pendingCreateNeverStartedTimeout + time.Minute)) {
			t.Fatalf("a create the prepare budget deferred is not held at its anchor + %s", pendingCreateNeverStartedTimeout+time.Minute)
		}
		if held(anchor.Add(startDeferralHoldCeiling + time.Second)) {
			t.Fatalf("the hold did not release once now is past the anchor + %s", startDeferralHoldCeiling)
		}
		// With no memo the same create is not held: the rollback would close it.
		if pendingCreateHold(nil, nil, &clock.Fake{Time: anchor.Add(pendingCreateNeverStartedTimeout + time.Minute)})(info) {
			t.Fatal("premise: a nil memo must hold nothing")
		}
	})
}

// A dependency block is the candidate's own condition (clause a): both
// reporting sites record nothing, the wave-level check and the recheck between
// a wave's batches (R5b).
func driveDependencyBlocksRecordNothing(t *testing.T) {
	t.Run("wave level", func(t *testing.T) {
		e := newCapacityEnv(t, false, "a", "b")
		memo := newStartDeferralMemo()
		e.startOptions = append(e.startOptions, withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
		e.cfg.Agents[1].DependsOn = []string{"a"}
		e.sp.StartErrors["a"] = fmt.Errorf("provider start failure")
		a := e.pendingCreate(t, "a", "", nil)
		b := e.pendingCreate(t, "b", "", nil)

		e.start(context.Background(), a, b)
		if e.sp.IsRunning("b") || !strings.Contains(e.log.String(), "session=b template=b outcome=blocked_on_dependencies") {
			t.Fatalf("premise: b was not blocked on its dependency:\n%s", e.log.String())
		}
		requireCommitted(t, memo)
	})
	t.Run("recheck between batches", func(t *testing.T) {
		city := t.TempDir()
		maxWakes := 8
		sp := &dropDependencyAfterNStartsProvider{Fake: runtime.NewFake(), dropAfter: 3, depName: "db"}
		if err := sp.Start(context.Background(), "db", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{
			Daemon: config.DaemonConfig{MaxWakesPerTick: &maxWakes},
			Agents: []config.Agent{
				{Name: "app-1", DependsOn: []string{"db"}},
				{Name: "app-2", DependsOn: []string{"db"}},
				{Name: "app-3", DependsOn: []string{"db"}},
				{Name: "app-4", DependsOn: []string{"db"}},
				{Name: "db", MaxActiveSessions: intPtr(1)},
			},
		}
		store := beads.NewMemStore()
		clk := &clock.Fake{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
		desired := map[string]TemplateParams{}
		var candidates []startCandidate
		for _, name := range []string{"app-1", "app-2", "app-3", "app-4"} {
			tp := TemplateParams{Command: name, SessionName: name, TemplateName: name}
			desired[name] = tp
			created, err := store.Create(beads.Bead{
				ID: name + "-id", Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: creatingMeta(map[string]string{
					"session_name": name, "template": name, "generation": "1", "continuation_epoch": "1",
					"instance_token": "tok-" + name, "pending_create_claim": "true",
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, created), tp: tp})
		}
		memo := newStartDeferralMemo()
		var stderr bytes.Buffer

		woken := executePlannedStartsTraced(context.Background(), candidates, cfg, desired, sp, store, "test-city", city, clk, events.Discard, 5*time.Second, ioDiscard{}, &stderr, nil, withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
		if woken != 3 || sp.IsRunning("app-4") {
			t.Fatalf("premise: woken = %d, app-4 running = %v; want 3 started and app-4 held back", woken, sp.IsRunning("app-4"))
		}
		if !strings.Contains(stderr.String(), "session=app-4 template=app-4 outcome=blocked_on_dependencies") {
			t.Fatalf("premise: app-4 was not blocked by the recheck:\n%s", stderr.String())
		}
		requireCommitted(t, memo)
	})
}

// An on_death hook pending for the candidate's own runtime name defers it
// (clause a); the memo records nothing.
func driveOnDeathHookRecordsNothing(t *testing.T) {
	e := newCapacityEnv(t, false, "a")
	memo := newStartDeferralMemo()
	gate := newOnDeathGate()
	gate.enqueue([]deathEdge{{name: "a"}})
	e.startOptions = append(e.startOptions, withOnDeathGate(gate), withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
	a := e.pendingCreate(t, "a", "", nil)

	e.start(context.Background(), a)
	if e.sp.CountCalls("Start", "a") != 0 || !strings.Contains(e.log.String(), "outcome=deferred_by_on_death_hook") {
		t.Fatalf("premise: a was not deferred by its pending hook:\n%s", e.log.String())
	}
	requireCommitted(t, memo)
}

// A runtime name another holder leases defers the candidate (clause a, R7);
// the memo records nothing.
func driveRuntimeLeaseRecordsNothing(t *testing.T) {
	f := newLeaseStartFixture(t, beads.NewMemStore())
	memo := newStartDeferralMemo()
	held := f.hold(t)
	defer held.Release()

	if got := f.run(withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet())); got != 0 {
		t.Fatalf("woken under a held lease = %d, want 0", got)
	}
	if !strings.Contains(f.log.String(), "outcome=deferred_by_runtime_lease") {
		t.Fatalf("premise: the start was not deferred by the lease:\n%s", f.log.String())
	}
	requireCommitted(t, memo)
}

// An endpoint refusing capacity has its own hold on the row
// (endpointCapacityGuard.HoldsPendingCreate); the memo records nothing.
func driveEndpointCapacityRecordsNothing(t *testing.T) {
	e := newCapacityEnv(t, true, "a")
	memo := newStartDeferralMemo()
	e.startOptions = append(e.startOptions, withStartDeferralMemo(memo), withStartDeferralSeen(neverStartedSet()))
	a := e.pendingCreate(t, "a", "e", nil)
	e.openEndpoint(t)

	e.start(context.Background(), a)
	if e.sp.IsRunning("a") || !strings.Contains(e.log.String(), "outcome=deferred_by_endpoint_capacity") {
		t.Fatalf("premise: a was not deferred by its endpoint:\n%s", e.log.String())
	}
	requireCommitted(t, memo)
}
