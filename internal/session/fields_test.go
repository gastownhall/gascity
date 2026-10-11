package session

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The session field registry's table tests.

var beadID = regexp.MustCompile(`^mc-[a-z0-9]+(\.[0-9]+)*$`)

// liveModes are the controller modes that see f written: an operator write
// lands under either, except a new key's, which only a v2 city writes.
func liveModes(f Field) []Mode {
	live := map[Mode]bool{}
	for _, s := range f.Writers {
		switch {
		case s.Mode != ModeOperator:
			live[s.Mode] = true
		case f.NewKey:
			live[ModeV2] = true
		default:
			live[ModeLegacy], live[ModeV2] = true, true
		}
	}
	var out []Mode
	for _, m := range []Mode{ModeLegacy, ModeV2} {
		if live[m] {
			out = append(out, m)
		}
	}
	return out
}

func hasMode(ss []Site, m Mode) bool {
	for _, s := range ss {
		if s.Mode == m {
			return true
		}
	}
	return false
}

// TestSessionFieldRegistry checks the table's rules (registryFindings) and
// that every named site is a function of its file (missingSites).
func TestSessionFieldRegistry(t *testing.T) {
	for _, f := range append(registryFindings(Fields()), missingSites(t, Fields())...) {
		t.Error(f)
	}
}

// registryFindings is the table's rules: a class and a writer (or a ruling)
// per key; a mode, a file, and a bead and note when pending, per site; no
// legacy writer of a new key; no per-tick key in the premise; and a consumer
// and a clear point for every request in every mode that writes it.
func registryFindings(fields []Field) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Key == "" || seen[f.Key] {
			out = append(out, fmt.Sprintf("%q: empty or duplicate key", f.Key))
		}
		seen[f.Key] = true
		if _, ok := classInPremise[f.Class]; !ok {
			out = append(out, fmt.Sprintf("%s: no class", f.Key))
		}
		if (len(f.Writers) == 0) == (f.NoWriter == "") {
			out = append(out, fmt.Sprintf("%s: needs writers or a NoWriter ruling, not both", f.Key))
		}
		if f.PerTick && f.Premise {
			out = append(out, fmt.Sprintf("%s: a per-tick key cannot be in the premise", f.Key))
		}
		for _, s := range sitesOf(f) {
			switch {
			case s.Mode < ModeLegacy || s.Mode > ModeOperator || s.File == "":
				out = append(out, fmt.Sprintf("%s: site %+v has no mode or file", f.Key, s))
			case s.Func == "" && s.Pending == "":
				out = append(out, fmt.Sprintf("%s: unbuilt site in %s names no pending bead", f.Key, s.File))
			case s.Pending != "" && (!beadID.MatchString(s.Pending) || s.Note == ""):
				out = append(out, fmt.Sprintf("%s: site in %s: pending %q needs a bead id and a note", f.Key, s.File, s.Pending))
			}
		}
		if f.NewKey && hasMode(f.Writers, ModeLegacy) {
			out = append(out, fmt.Sprintf("%s: new key with a legacy writer (CONTRACT R1 rollback rule)", f.Key))
		}
		if f.Class != ClassRequest && f.Class != ClassStopRequest {
			continue
		}
		for _, m := range liveModes(f) {
			if !hasMode(f.Readers, m) || !hasMode(f.Clears, m) {
				out = append(out, fmt.Sprintf("%s: written under mode %d with no consumer or clear point there; build one or declare it pending on a bead", f.Key, m))
			}
		}
	}
	return out
}

func sitesOf(f Field) []Site {
	return append(append(append([]Site{}, f.Writers...), f.Readers...), f.Clears...)
}

// missingSites is every named site that is not a function of its file, so
// the table cannot keep naming code that is gone.
func missingSites(t *testing.T, fields []Field) []string {
	root := repoRoot(t)
	var out []string
	funcs := map[string]map[string]bool{}
	for _, f := range fields {
		for _, s := range sitesOf(f) {
			if s.Func == "" {
				continue
			}
			if funcs[s.File] == nil {
				funcs[s.File] = map[string]bool{}
				parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, s.File), nil, 0)
				if err != nil {
					out = append(out, fmt.Sprintf("%s: %s: %v", f.Key, s.File, err))
					continue
				}
				for _, d := range parsed.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok {
						funcs[s.File][funcName(fd)] = true
					}
				}
			}
			if !funcs[s.File][s.Func] {
				out = append(out, fmt.Sprintf("%s: site %s:%s no longer exists", f.Key, s.File, s.Func))
			}
		}
	}
	return out
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// TestSessionFieldRegistryCatches mutates the table the ways its rules exist
// to catch and checks each mutant fails with its reason.
func TestSessionFieldRegistryCatches(t *testing.T) {
	edit := func(key string, fn func(*Field)) []Field {
		fields := Fields()
		for i := range fields {
			if fields[i].Key == key {
				fn(&fields[i])
			}
		}
		return fields
	}
	for _, tc := range []struct {
		name   string
		fields []Field
		want   string
	}{
		{"duplicate key", append(Fields(), Fields()[0]), "empty or duplicate key"},
		{"no class", edit("state", func(f *Field) { f.Class = 0 }), "state: no class"},
		{"no writer, no ruling", edit("slept_at", func(f *Field) { f.Writers = nil }), "slept_at: needs writers or a NoWriter ruling"},
		{"per-tick premise key", edit("execution_claim_hold", func(f *Field) { f.Premise = true }), "per-tick key cannot be in the premise"},
		{"pending with no bead", edit("sleep_policy_fingerprint", func(f *Field) { f.Writers[1].Pending = "" }), "names no pending bead"},
		{"legacy writer of a new key", edit(DrainAckAtKey, func(f *Field) { f.Writers[0].Mode = ModeLegacy }), "new key with a legacy writer"},
		{"request with no v2 clear", edit("wake_request", func(f *Field) { f.Clears = f.Clears[:1] }), "wake_request: written under mode 2 with no consumer or clear point"},
		{"request with no legacy consumer", edit("restart_requested", func(f *Field) { f.Readers = f.Readers[1:] }), "restart_requested: written under mode 1"},
		{"site gone", edit("slept_at", func(f *Field) { f.Writers[0].Func = "SleepPatchGone" }), "slept_at: site internal/session/lifecycle_transition.go:SleepPatchGone no longer exists"},
	} {
		got := strings.Join(append(registryFindings(tc.fields), missingSites(t, tc.fields)...), "\n")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: mutant passed or failed otherwise; want %q in:\n%s", tc.name, tc.want, got)
		}
	}
}

// premiseGolden is every key F2's premise compares. A change here is a
// change to what every v2 fence refuses on.
var premiseGolden = strings.Fields(`
	archived_at churn_count close_reason closed_at configured_named_identity continuation_reset_pending
	continuity_eligible crash_count creation_complete_at current_claim_bead_id currently_processing_bead_id
	detached_at drain_ack_at drain_ack_incarnation drain_at drain_finalize drain_intent_at drain_intent_incarnation
	drain_intent_reason generation held_until idle_respawn_attempts idle_respawn_bead_id instance_token last_woke_at
	pending_create_claim pending_create_started_at pin_awake provider_terminal_error provider_terminal_error_at
	quarantine_cycle quarantined_until reset_committed_at restart_requested session_circuit_open_restart_count
	session_circuit_opened_at session_circuit_reset_generation session_circuit_restarts session_circuit_state
	session_drainable session_health session_health_reason session_key sleep_intent sleep_policy_fingerprint
	sleep_reason slept_at started_config_hash startup_dialog_verified state state_reason stranded_event_emitted_at
	suspended_at template_overrides unknown_state_escalated_at unknown_state_first_seen unknown_state_value
	wait_hold wake_attempts wake_refused_event_at wake_request wake_requested_at`)

// TestSessionFieldPremise pins F2's premise to the golden set and checks it
// covers today's fence (LifecycleInputFromInfo's keys) and the keys K2 found
// missing from it: the kill fence's state_reason and slept_at, the operator
// intents, the wake request's stamp and detached_at.
func TestSessionFieldPremise(t *testing.T) {
	var got []string
	for _, f := range Fields() {
		if f.InPremise() {
			got = append(got, f.Key)
		}
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(premiseGolden, " ") {
		t.Errorf("premise keys changed:\n got %v\nwant %v", got, premiseGolden)
	}
	src := readSources(t, "internal/session")
	fence := src.reads("internal/session", "LifecycleInputFromInfo")
	if len(fence) < 13 {
		t.Fatalf("LifecycleInputFromInfo reads %d keys, want its 13", len(fence))
	}
	for _, k := range append(fence, "state_reason", "slept_at", "sleep_intent", "wait_hold", "suspended_at", "wake_requested_at", "detached_at") {
		if f, ok := LookupField(k); !ok || !f.InPremise() {
			t.Errorf("%s: a fence needs it but the premise leaves it out", k)
		}
	}
}

// TestSessionFieldAssignmentIdentity pins ClassAssignmentIdentity to exactly
// the keys both forms of sessionAssignmentIdentifiersForConfig read, through
// their own package's callees: the identities a claim can be held under.
func TestSessionFieldAssignmentIdentity(t *testing.T) {
	src := readSources(t, "internal/session", "cmd/gc")
	var want []string
	for _, f := range Fields() {
		if f.Class == ClassAssignmentIdentity {
			want = append(want, f.Key)
		}
	}
	sort.Strings(want)
	for _, fn := range []string{"sessionAssignmentIdentifiersForConfig", "sessionAssignmentIdentifiersForConfigInfo"} {
		var got []string
		for _, k := range src.reads("cmd/gc", fn) {
			if f, ok := LookupField(k); ok && (f.Class == ClassAssignmentIdentity || f.Class == ClassLaunchConfig) {
				got = append(got, k)
			}
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s reads identity keys %v; ClassAssignmentIdentity is %v", fn, got, want)
		}
	}
}

// TestSessionFieldLiteralKeys checks the keys spelled in code without running
// it: every key in a MetadataPatch (a session row's patch), and every
// computed family (a key-shaped prefix plus a name, written as a map key),
// must be registered, a family as a Family entry.
func TestSessionFieldLiteralKeys(t *testing.T) {
	src := readSources(t, "internal/session", "cmd/gc", "internal/api", "internal/worker")
	got, patches, families := literalFindings(src, LookupField)
	for _, f := range got {
		t.Error(f)
	}
	if patches < 100 || families < 1 {
		t.Fatalf("found %d MetadataPatch keys and %d computed families; was a source dir moved?", patches, families)
	}
	// Mutants: a table without the opt_ family, or without detached_at.
	without := func(key string) func(string) (Field, bool) {
		return func(k string) (Field, bool) {
			if f, ok := LookupField(k); ok && f.Key != key {
				return f, true
			}
			return Field{}, false
		}
	}
	for key, want := range map[string]string{"opt_": "computed key opt_* is not a registered family", "detached_at": `MetadataPatch key "detached_at" is not registered`} {
		if got, _, _ := literalFindings(src, without(key)); !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("without %s: want %q in %v", key, want, got)
		}
	}
}

var familyPrefix = regexp.MustCompile(`^[a-z][a-z0-9_.]*_$`)

func literalFindings(src *sources, lookup func(string) (Field, bool)) (out []string, patches, families int) {
	for _, p := range src.files {
		if p.dir == "internal/beadmeta" {
			continue
		}
		ast.Inspect(p.f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if typeName(x.Type) != "MetadataPatch" {
					return true
				}
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := src.key(p, kv.Key); ok {
							patches++
							if _, reg := lookup(k); !reg {
								out = append(out, fmt.Sprintf("%s: MetadataPatch key %q is not registered", p.rel, k))
							}
						}
					}
				}
			case *ast.IndexExpr:
				if b, ok := x.Index.(*ast.BinaryExpr); ok && b.Op == token.ADD {
					if prefix, ok := src.key(p, b.X); ok && familyPrefix.MatchString(prefix) {
						families++
						if f, reg := lookup(prefix + "x"); !reg || !f.Family {
							out = append(out, fmt.Sprintf("%s: computed key %s* is not a registered family", p.rel, prefix))
						}
					}
				}
			}
			return true
		})
	}
	return out, patches, families
}

// sources is the parsed production Go of some package dirs, with their
// string constants, for the few checks that read code.
type sources struct {
	files  []srcFile
	consts map[string]map[string]string // dir -> name -> value
	funcs  map[string]srcFunc           // "dir:Func"
	fields map[string][]string          // Info field -> keys (info_codec.go)
}

type srcFunc struct {
	fd *ast.FuncDecl
	p  srcFile
}

type srcFile struct {
	rel, dir string
	f        *ast.File
	imports  map[string]string // import name -> repo dir
}

func readSources(t *testing.T, dirs ...string) *sources {
	t.Helper()
	root := repoRoot(t)
	src := &sources{consts: map[string]map[string]string{}, funcs: map[string]srcFunc{}, fields: map[string][]string{}}
	for _, dir := range append(dirs, "internal/session", "internal/beadmeta") {
		if src.consts[dir] != nil {
			continue
		}
		src.consts[dir] = map[string]string{}
		ents, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, dir, e.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			p := srcFile{rel: dir + "/" + e.Name(), dir: dir, f: f, imports: map[string]string{}}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				name := filepath.Base(path)
				if imp.Name != nil {
					name = imp.Name.Name
				}
				p.imports[name] = strings.TrimPrefix(path, "github.com/gastownhall/gascity/")
			}
			src.files = append(src.files, p)
		}
	}
	for range 2 { // constants declared as other constants resolve on the second round
		for _, p := range src.files {
			for _, d := range p.f.Decls {
				if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
					for _, spec := range gd.Specs {
						vs := spec.(*ast.ValueSpec)
						for i := 0; i < len(vs.Names) && i < len(vs.Values); i++ {
							if v, ok := src.key(p, vs.Values[i]); ok {
								src.consts[p.dir][vs.Names[i].Name] = v
							}
						}
					}
				}
			}
		}
	}
	for _, p := range src.files {
		for _, d := range p.f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				src.funcs[p.dir+":"+fd.Name.Name] = srcFunc{fd, p}
			}
		}
		if p.rel == "internal/session/info_codec.go" {
			ast.Inspect(p.f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok || len(cl.Elts) != 2 {
					return true
				}
				k, ok := src.key(p, cl.Elts[0])
				if fl, isFn := cl.Elts[1].(*ast.FuncLit); ok && isFn {
					ast.Inspect(fl.Body, func(n ast.Node) bool {
						if sel, ok := n.(*ast.SelectorExpr); ok {
							src.fields[sel.Sel.Name] = append(src.fields[sel.Sel.Name], k)
						}
						return true
					})
				}
				return false
			})
		}
	}
	return src
}

// key resolves e to a string: a literal, or a constant of p's package or an
// imported one.
func (src *sources) key(p srcFile, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			v, err := strconv.Unquote(x.Value)
			return v, err == nil
		}
	case *ast.Ident:
		v, ok := src.consts[p.dir][x.Name]
		return v, ok
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok {
			v, ok := src.consts[p.imports[pkg.Name]][x.Sel.Name]
			return v, ok
		}
	}
	return "", false
}

// reads is the registered keys a function of dir reads, by index or through
// its Info field, and through the loaded functions it calls, sorted.
func (src *sources) reads(dir, fn string) []string {
	keys, seen := map[string]bool{}, map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		sf, ok := src.funcs[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		ast.Inspect(sf.fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.IndexExpr:
				if k, ok := src.key(sf.p, x.Index); ok {
					keys[k] = true
				}
			case *ast.SelectorExpr:
				for _, k := range src.fields[x.Sel.Name] {
					keys[k] = true
				}
			case *ast.CallExpr:
				switch f := x.Fun.(type) {
				case *ast.Ident:
					visit(sf.p.dir + ":" + f.Name)
				case *ast.SelectorExpr:
					if pkg, ok := f.X.(*ast.Ident); ok && sf.p.imports[pkg.Name] != "" {
						visit(sf.p.imports[pkg.Name] + ":" + f.Sel.Name)
					}
				}
			}
			return true
		})
	}
	visit(dir + ":" + fn)
	var out []string
	for k := range keys {
		if _, ok := LookupField(k); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return fmt.Sprint(e)
}
