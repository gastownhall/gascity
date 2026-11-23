package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeCodeArg is what a variable close-code argument can carry.
type closeCodeArg struct {
	// codes are the literal codes the argument can carry.
	codes []string
	// poolSlot marks the pool-slot close, which also writes every pool-freeable
	// sleep reason verbatim as its close code.
	poolSlot bool
	// forwarded says where the codes the argument forwards are censused
	// instead; such an argument carries no codes of its own.
	forwarded string
}

// interruptedCloseCensusArgs is every variable close-code argument, keyed by
// "file#func#expr", with the values it can carry. A new variable argument
// fails the census until it is added here.
var interruptedCloseCensusArgs = map[string]closeCodeArg{
	`cmd/gc/reconcile_arms_close.go#armClose#cmp.Or(strings.TrimSpace(info.SleepReason), "drained")`: {
		codes: []string{"drained"}, poolSlot: true,
	},
	`cmd/gc/reconcile_arms_close.go#armClose#e.DrainReason`: {
		codes: []string{drainOrphaned, drainSuspended},
	},
	`cmd/gc/session_reconciler.go#reconcileSessionBeadsTracedWithNamedDemand#reason`: {
		codes: []string{"orphaned", "suspended"},
	},
	`cmd/gc/session_reconciler.go#reconcileSessionBeadsTracedWithNamedDemand#closeReason`: {
		codes: []string{"drained"}, poolSlot: true,
	},
	`cmd/gc/session_beads.go#closeBeadPreservingAssignees#reason`: {
		forwarded: "closeSeat's closeWhy reason; every closeWhy literal is censused",
	},
	`cmd/gc/session_reconciler_interrupted_close.go#finishInterruptedClose#info.MetadataState`: {
		forwarded: "the code interruptedCloseGate recognized, rewritten by its finisher",
	},
	`internal/session/store.go#Close#stateCode`: {
		forwarded: "Store.Close's caller; no production caller passes a code but the storebinding wrapper, which forwards its own",
	},
}

// closeGateIdents resolves a closeWhy literal's gate expression.
var closeGateIdents = map[string]closeGate{
	"closeGated":     closeGated,
	"closeFreeSlot":  closeFreeSlot,
	"closeReleasing": closeReleasing,
	"closeRollback":  closeRollback,
}

// closeCensusSource is the parsed non-test Go source the census reads.
type closeCensusSource struct {
	files       map[string]*ast.File // repo-relative path -> file
	consts      map[string]map[string]ast.Expr
	stringTypes map[string]bool
}

// closeCensusPkgKey names a file's package for constant lookup: its directory.
func closeCensusPkgKey(rel string) string { return filepath.ToSlash(filepath.Dir(rel)) }

func parseCloseCensusSource(t *testing.T, root string) *closeCensusSource {
	t.Helper()
	src := &closeCensusSource{
		files:       map[string]*ast.File{},
		consts:      map[string]map[string]ast.Expr{},
		stringTypes: map[string]bool{"string": true},
	}
	fset := token.NewFileSet()
	for _, dir := range []string{"cmd/gc", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			src.files[rel] = f
			pkg := closeCensusPkgKey(rel)
			if src.consts[pkg] == nil {
				src.consts[pkg] = map[string]ast.Expr{}
			}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						if gd.Tok != token.CONST {
							continue
						}
						for i, name := range s.Names {
							if i < len(s.Values) {
								src.consts[pkg][name.Name] = s.Values[i]
							}
						}
					case *ast.TypeSpec:
						if id, ok := s.Type.(*ast.Ident); ok && id.Name == "string" {
							src.stringTypes[s.Name.Name] = true
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
	}
	if len(src.files) == 0 {
		t.Fatalf("no Go source found under %s", root)
	}
	return src
}

// resolve returns the string value of a constant expression in pkg: a string
// literal, a constant (of pkg, or of internal/session through a selector), or
// a conversion of one to a string type.
func (s *closeCensusSource) resolve(pkg string, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		return v, err == nil
	case *ast.ParenExpr:
		return s.resolve(pkg, x.X)
	case *ast.Ident:
		if v, ok := s.consts[pkg][x.Name]; ok {
			return s.resolve(pkg, v)
		}
	case *ast.SelectorExpr:
		if q, ok := x.X.(*ast.Ident); ok && (q.Name == "session" || q.Name == "sessionpkg") {
			if v, ok := s.consts["internal/session"][x.Sel.Name]; ok {
				return s.resolve("internal/session", v)
			}
		}
	case *ast.CallExpr:
		if len(x.Args) != 1 {
			return "", false
		}
		var typ string
		switch fn := x.Fun.(type) {
		case *ast.Ident:
			typ = fn.Name
		case *ast.SelectorExpr:
			typ = fn.Sel.Name
		}
		if s.stringTypes[typ] {
			return s.resolve(pkg, x.Args[0])
		}
	}
	return "", false
}

// closeCodeSite is one close-code argument the census found.
type closeCodeSite struct {
	pos  string
	key  string
	arg  ast.Expr
	gate ast.Expr // closeWhy literals only
	pkg  string
}

func (s *closeCensusSource) closeCodeSites(t *testing.T) []closeCodeSite {
	t.Helper()
	var sites []closeCodeSite
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		f := s.files[rel]
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				site := closeCodeSite{pkg: closeCensusPkgKey(rel)}
				switch x := n.(type) {
				case *ast.CallExpr:
					name := ""
					switch f := x.Fun.(type) {
					case *ast.Ident:
						name = f.Name
					case *ast.SelectorExpr:
						name = f.Sel.Name
					}
					if name != "ClosePatch" {
						return true
					}
					if len(x.Args) != 2 {
						t.Errorf("%s#%s: ClosePatch call with %d arguments", rel, fn.Name.Name, len(x.Args))
						return true
					}
					site.arg = x.Args[1]
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); !ok || id.Name != "closeWhy" {
						return true
					}
					for _, elt := range x.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch k, _ := kv.Key.(*ast.Ident); {
						case k == nil:
						case k.Name == "reason":
							site.arg = kv.Value
						case k.Name == "gate":
							site.gate = kv.Value
						}
					}
					if site.arg == nil {
						t.Errorf("%s#%s: closeWhy literal without a reason", rel, fn.Name.Name)
						return true
					}
				default:
					return true
				}
				site.pos = rel + "#" + fn.Name.Name
				site.key = site.pos + "#" + types.ExprString(site.arg)
				sites = append(sites, site)
				return true
			})
		}
	}
	return sites
}

// poolFreeableSleepReasons returns every SleepReason constant of
// internal/session the pool-slot close admits on an asleep row.
func (s *closeCensusSource) poolFreeableSleepReasons(t *testing.T) []string {
	t.Helper()
	var out []string
	for rel, f := range s.files {
		if closeCensusPkgKey(rel) != "internal/session" {
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "SleepReason" {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						t.Errorf("SleepReason constant %s has no value", name.Name)
						continue
					}
					v, ok := s.resolve("internal/session", vs.Values[i])
					if !ok {
						t.Errorf("SleepReason constant %s = %s does not resolve", name.Name, types.ExprString(vs.Values[i]))
						continue
					}
					info := sessionpkg.Info{MetadataState: "asleep", SleepReason: v, PoolManaged: true}
					if isPoolSessionSlotFreeableInfo(info, time.Now()) {
						out = append(out, v)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// Every close code this binary can leave on an open row is either a known
// state or recognized by interruptedCloseGate with the gate of the close that
// writes it, so no interrupted close is left to the unknown-state skip.
func TestInterruptedCloseCensusCoversEveryCloseCode(t *testing.T) {
	src := parseCloseCensusSource(t, repoRootForLint(t))
	freeable := src.poolFreeableSleepReasons(t)
	if len(freeable) == 0 {
		t.Fatal("no pool-freeable SleepReason constants found")
	}
	for _, v := range freeable {
		if !knownSessionStates[v] && !interruptedClosePassthroughCodes[v] {
			t.Errorf("pool-freeable sleep reason %q is neither a known state nor on the passthrough list", v)
		}
	}

	sites := src.closeCodeSites(t)
	used := map[string]bool{}
	for _, site := range sites {
		var codes, poolCodes []string
		if v, ok := src.resolve(site.pkg, site.arg); ok {
			codes = []string{v}
		} else {
			arg, ok := interruptedCloseCensusArgs[site.key]
			if !ok {
				t.Errorf("%s: close code %s is not a constant and not in interruptedCloseCensusArgs", site.pos, types.ExprString(site.arg))
				continue
			}
			used[site.key] = true
			if arg.forwarded != "" {
				continue
			}
			codes = arg.codes
			if arg.poolSlot {
				poolCodes = freeable
			}
		}
		var gates []closeGate
		for _, code := range codes {
			if knownSessionStates[code] {
				continue
			}
			gate, ok := interruptedCloseGate(sessionpkg.Info{MetadataState: code})
			if !ok {
				t.Errorf("%s: close code %q is neither a known state nor recognized by interruptedCloseGate", site.pos, code)
				continue
			}
			gates = append(gates, gate)
		}
		for _, code := range poolCodes {
			if knownSessionStates[code] {
				continue
			}
			gate, ok := interruptedCloseGate(sessionpkg.Info{MetadataState: code, SleepReason: code, PoolManaged: true})
			if !ok {
				t.Errorf("%s: pool-slot close code %q is neither a known state nor recognized by interruptedCloseGate", site.pos, code)
				continue
			}
			gates = append(gates, gate)
		}
		if site.gate == nil || len(gates) == 0 {
			continue
		}
		gate, ok := closeGateIdents[types.ExprString(site.gate)]
		if !ok {
			t.Errorf("%s: closeWhy gate %s does not resolve", site.pos, types.ExprString(site.gate))
			continue
		}
		for _, want := range gates {
			if gate != want {
				t.Errorf("%s: closeWhy gate %s, want the recognizer's %s", site.pos, closeGateName(gate), closeGateName(want))
			}
		}
	}
	for key := range interruptedCloseCensusArgs {
		if !used[key] {
			t.Errorf("interruptedCloseCensusArgs entry %q matches no close-code argument", key)
		}
	}

	for _, state := range []string{"archived", "draining"} {
		if _, ok := interruptedCloseGate(sessionpkg.Info{MetadataState: state, SleepReason: state, PoolManaged: true}); ok {
			t.Errorf("state %q recognized as an interrupted close, want it left to the unknown-state skip", state)
		}
	}
}
