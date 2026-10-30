package splittest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const beadsImportPath = "github.com/gastownhall/gascity/internal/beads"

// markerKinds maps each internal/beads marker function to the kind of debt
// it declares.
var markerKinds = map[string]string{
	"FederatedRead": "federated",
	"FederatedGet":  "federated",
	"RelicRead":     "relic",
	"RelicGet":      "relic",
}

// reviewedMarker is one reviewed use of a read marker: a production
// function that wraps reads in it, how many times, and why.
type reviewedMarker struct {
	kind   string // "federated" (a legitimate federation leg) or "relic" (S4 debt)
	count  int
	reason string
}

// reviewedMarkers is the reviewed set of production functions that wrap a
// store read in a read marker, keyed "file func". A marked read is invisible
// to the split-strict gate's ratchet, so each entry is a reviewed claim.
// FEDERATED entries are legitimate: another leg covers the miss. RELIC
// entries are S4 debt: a scope store holds the class only as pre-split rows
// (or rows a writer S4 moves still writes there), and the leg goes when they
// do. Adding, moving or removing a mark changes this table in the same
// change, where a reviewer sees it.
var reviewedMarkers = map[string]reviewedMarker{
	// Legitimate federation legs.
	"internal/storeref/resolve.go resolveByID": {"federated", 1, "the ModeFirstOwner leg walk: each leg is a planned residence probe"},
	"cmd/gc/claim_class_route.go classRoutedHookClaimOps": {
		"federated", 5,
		"the hook-claim class route's work-first escalation (#8137): a not-found (or an empty continuation list) on the work leg is what sends the call to the binding",
	},
	"cmd/gc/wisp_autoclose.go collectInputConvoyWorkflowRoots": {
		"federated", 1,
		"tracking convoys are probed on the work store and the graph store; a convoy minted single-store-era can sit on the graph side",
	},

	"internal/api/handler_convoys.go isGraphConvoyID": {
		"federated", 1,
		"graph-first probe of a convoy id; a plain (work) convoy misses there and the rig legs answer it",
	},
	"cmd/gc/cmd_order.go bdCursor": {
		"federated", 1,
		"one leg of bdCursorAcrossStores (S3a): order-run records live in the orders binding and, as v1 poured roots, on the work legs",
	},
	"cmd/gc/wisp_gc.go closedWispGCEntries": {
		"federated", 1,
		"the graph leg of the wisp GC root union (S3a): v1 poured molecule roots live on the work legs",
	},
	"cmd/gc/wisp_gc.go openWispGCRootCandidates": {
		"federated", 1,
		"the graph leg of the wisp GC root union (S3a): v1 poured molecule roots live on the work legs",
	},
	"cmd/gc/wisp_gc.go enumerateWorkClassMoleculeRoots": {
		"federated", 1,
		"the work leg of the wisp GC root union (S3a): graph molecule roots live in the graph store",
	},
	"cmd/gc/wisp_gc.go storeHoldsLiveAttachmentSource": {
		"federated", 1,
		"one leg of the stepless-root holder walk (S3a): the graph store, then every work leg; a holder lives in one",
	},
	"cmd/gc/cmd_convoy.go closeCompletedConvoyAcross": {
		"federated", 2,
		"the two legs of a work convoy's legacy-child union (S3c): a graph child poured under it lives in the binding, a work child on the convoy's ledger",
	},
	"cmd/gc/molecule_autoclose.go autocloseLocate": {
		"federated", 2,
		"a two-store residence probe for an autoclose parent or root (S3a, S3c): the second store answers when the first misses",
	},
	"internal/orders/store_reads.go (*Store).LastRun": {
		"federated", 1,
		"one leg of the last-run read (S3a, S3c): run evidence is a tracking bead (orders), a graph root, or a v1 poured root on the work leg",
	},
	"cmd/gc/api_state.go liveCloseOwner": {
		"federated", 1,
		"first-owner close probe (S3a R4): the prefix route and the binding the residence probe proved holds a relic id",
	},
	"cmd/gc/wisp_step_inject.go stepResolution.activeMoleculeIn": {
		"federated", 1,
		"one leg of the step reminder's molecule union (S3a): graph roots in the graph store, v1 poured molecules on the work legs",
	},
	"internal/sling/sling.go graphWorkflowAttachmentInGraphStore": {
		"federated", 1,
		"graph.v2 root test against the graph store: a v1 poured root lives in the work store, and the miss is the answer (not graph)",
	},
	"internal/sling/sling_attachment.go getFromStores": {
		"federated", 1,
		"first-owner attachment probe (S3a): graph store or, for a v1 poured molecule, the work store",
	},
	"internal/api/huma_handlers_beads.go (*Server).humaHandleBeadList": {
		"federated", 1,
		"/beads fan-out leg: work legs and the class binding each list only their own rows",
	},

	// Relic legs: graph rows read from city and rig scope stores (S4).
	"internal/api/huma_handlers_convoys.go (*Server).humaDeleteWorkflow":       {"relic", 3, "workflowStores root probe, metadata sweep and descendant sweep in workflow teardown"},
	"cmd/gc/cmd_convoy_dispatch.go findBeadScopeAcrossStores":                  {"relic", 2, "control-bead scope probe over the city and rig ledgers before the binding"},
	"cmd/gc/graph_placement.go graphResidenceForTree":                          {"relic", 1, "the S4 rule's relic leg: a pre-S4 rig tree read from the rig ledger until the rig is imported (S4b)"},
	"cmd/gc/convergence_store.go (*convergenceStoreAdapter).list":              {"relic", 1, "pre-S4a rig convergence loops in the rig's own store, read until the rig is imported (S4b)"},
	"cmd/gc/formula_cook_attach_class.go attachGraftClassRefusal":              {"relic", 1, "cook --attach onto a pre-S4 rig tree reads the attach bead where it lives, until the rig is imported (S4b)"},
	"internal/api/handler_convoys.go rigLegConvoyGet":                          {"relic", 1, "isGraphConvoyID's rig leg: a pre-S4 graph convoy in a rig ledger, until the rig is imported (S4b)"},
	"cmd/gc/cmd_convoy_dispatch.go cmdWorkflowDelete":                          {"relic", 1, "workflow delete sweeps the retained scope-store copies beside the binding"},
	"cmd/gc/cmd_convoy_dispatch.go (*sourceWorkflowMatchCollector).scanStore":  {"relic", 1, "the source-workflow walk's root probe over scope stores and the binding (child sources are ratcheted, not marked)"},
	"cmd/gc/cmd_convoy_dispatch.go assessZeroMatchSourceWorkflowScan":          {"relic", 1, "zero-match re-scan over the same views"},
	"internal/api/convoy_event_stream.go workflowEventBeadFromSubject":         {"relic", 1, "workflowStores residence probe (graph, city, rigs)"},
	"internal/api/convoy_event_stream.go workflowEventRoot":                    {"relic", 1, "workflowStores root scan (graph, city, rigs)"},
	"internal/api/handler_convoy_dispatch.go (*Server).buildWorkflowSnapshot":  {"relic", 2, "workflowStores root probe and metadata sweep"},
	"internal/api/huma_handlers_convoys.go (*Server).humaHandleWorkflowDelete": {"relic", 3, "workflowStores root probe, metadata sweep and descendant sweep"},
	"internal/api/huma_handlers_runs.go (*Server).cancelRun":                   {"relic", 1, "workflowStores root scan for a run"},
	"internal/api/orders_feed.go buildWorkflowRunProjections":                  {"relic", 1, "workflowStores closed-root list"},
	"internal/api/orders_feed.go buildWorkflowRunProjectionsRootOnly":          {"relic", 1, "workflowStores closed-root list (root-only projection)"},
	"internal/sling/sling_core.go (*sourceWorkflowRootCollector).scanStore":    {"relic", 1, "source-workflow root scan over SourceWorkflowStores"},

	// Relic legs: session beads read from a scope store unioned beside the
	// sessions store.
	"cmd/gc/pool_detached_orphan_sweep.go buildDetachedOrphanRouteIndex":               {"relic", 1, "detached-orphan route index over a work leg and the sessions store"},
	"cmd/gc/doctor_executor_identity_residue_check.go buildExecutorRouteIdentityIndex": {"relic", 1, "executor-identity doctor index unioning scope stores' session beads"},

	// Relic legs: order-tracking rows read from city and rig scope stores
	// (pre-cutover tracking history).
	"internal/api/orders_feed.go buildOrderRunFeedItems":                                {"relic", 1, "order feed ListTracking over graph, binding and scope legs"},
	"internal/api/huma_handlers_orders.go orderHistoryBeadsAcrossStoreInfosCachedFirst": {"relic", 1, "order history list per leg"},
	"internal/api/huma_handlers_orders.go orderHistoryBeadsAcrossStoreInfos":            {"relic", 1, "order history list per leg"},
	"internal/api/huma_handlers_orders.go orderHistoryBeadAcrossStoreInfos":             {"relic", 1, "order history by-id probe per leg"},
	"cmd/gc/order_dispatch.go sweepStaleOrderTrackingWithOptionsLimitMode":              {"relic", 1, "stale tracking sweep's list probe on a scope leg"},
	"cmd/gc/order_dispatch.go sweepClosedOrderTrackingRetention":                        {"relic", 1, "retention prune's list probe on a scope leg"},
	"cmd/gc/order_dispatch.go sweepClosedOrderTrackingRetentionBounded":                 {"relic", 1, "bounded retention prune's list probe on a scope leg"},
	"cmd/gc/order_dispatch.go countClosedOrderTrackingRetentionEligible":                {"relic", 1, "retention count's list probe on a scope leg"},
}

// markerUse is one reference to a read marker found in production code.
type markerUse struct {
	file, fn, kind string
}

// scanMarkerUses parses one Go file and returns every reference to an
// internal/beads read marker — a call or a function value, through any import
// alias or a dot import — keyed by its enclosing top-level function.
func scanMarkerUses(fset *token.FileSet, rel string, src []byte) ([]markerUse, error) {
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	local := ""
	dot := false
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != beadsImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			local = "beads"
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name != "_":
			local = imp.Name.Name
		}
	}
	// Inside internal/beads the markers are the package's own identifiers.
	if file.Name.Name == "beads" && strings.HasPrefix(rel, "internal/beads/") && !strings.Contains(strings.TrimPrefix(rel, "internal/beads/"), "/") {
		dot = true
	}
	if local == "" && !dot {
		return nil, nil
	}
	var uses []markerUse
	record := func(fn, name string) {
		if kind, ok := markerKinds[name]; ok {
			uses = append(uses, markerUse{file: rel, fn: fn, kind: kind})
		}
	}
	for _, decl := range file.Decls {
		fn := "<package>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = funcDeclName(fd)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local && id.Obj == nil {
					record(fn, x.Sel.Name)
				}
				return false
			case *ast.Ident:
				if dot && x.Obj == nil {
					record(fn, x.Name)
				}
			}
			return true
		})
	}
	return uses, nil
}

// funcDeclName renders a declaration as "name" or "(*Recv).name".
func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	star := ""
	if s, ok := recv.(*ast.StarExpr); ok {
		star = "*"
		recv = s.X
	}
	if idx, ok := recv.(*ast.IndexExpr); ok {
		recv = idx.X
	}
	if idx, ok := recv.(*ast.IndexListExpr); ok {
		recv = idx.X
	}
	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}
	if star != "" {
		return "(" + star + name + ")." + fd.Name.Name
	}
	return name + "." + fd.Name.Name
}

func TestScanMarkerUsesResolvesAliasesDotImportsAndFuncValues(t *testing.T) {
	src := `package p
import (
	b "github.com/gastownhall/gascity/internal/beads"
	. "github.com/gastownhall/gascity/internal/beads"
)
func viaAlias() { _ = b.FederatedRead(nil) }
func viaValue() { f := b.RelicRead; _ = f }
func viaDot() { _ = FederatedGet(nil, "") }
type T struct{}
func (t *T) method() { _ = b.RelicGet(nil, "") }
func shadowed(beads int) { _ = beads }
`
	uses, err := scanMarkerUses(token.NewFileSet(), "p/p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, u := range uses {
		got[u.fn] = u.kind
	}
	want := map[string]string{"viaAlias": "federated", "viaValue": "relic", "viaDot": "federated", "(*T).method": "relic"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("uses = %v, want %v", got, want)
	}
}

func TestScanMarkerUsesSeesBareMarkersInsideInternalBeads(t *testing.T) {
	src := `package beads
func probe(s Store) { _, _ = FederatedGet(s, "x") }
`
	uses, err := scanMarkerUses(token.NewFileSet(), "internal/beads/probe.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].fn != "probe" || uses[0].kind != "federated" {
		t.Fatalf("uses = %+v, want one federated use in probe", uses)
	}
}

func TestReadMarkersAreReviewed(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	fset := token.NewFileSet()
	got := map[string]map[string]int{}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipImportScanDir(path, root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/beads/federated_read.go" {
			return nil // the markers' own definitions
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		uses, err := scanMarkerUses(fset, rel, src)
		if err != nil {
			return err
		}
		for _, u := range uses {
			key := u.file + " " + u.fn
			if got[key] == nil {
				got[key] = map[string]int{}
			}
			got[key][u.kind]++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for read markers: %v", err)
	}
	if scanned == 0 {
		t.Fatal("the marker guard scanned zero production files; it is not evaluating anything")
	}
	var diffs []string
	for key, kinds := range got {
		want, ok := reviewedMarkers[key]
		for kind, n := range kinds {
			if !ok || want.kind != kind || want.count != n {
				diffs = append(diffs, fmt.Sprintf("%s: %d %s mark(s); the reviewed table says %d %s", key, n, kind, want.count, want.kind))
			}
		}
	}
	for key, want := range reviewedMarkers {
		if _, ok := got[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: no mark; the reviewed table says %d %s", key, want.count, want.kind))
		}
		if strings.TrimSpace(want.reason) == "" {
			diffs = append(diffs, key+": reviewed entry has no reason")
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Errorf("read-marker uses differ from reviewedMarkers:\n  %s\n\n"+
			"A marked read is invisible to the split-strict gate's ratchet. Use beads.FederatedRead only for a federation leg or residence probe whose miss "+
			"another leg covers, and beads.RelicRead for a leg that reads a class's rows from a scope store that should no longer hold them (S4 debt); "+
			"record either in reviewedMarkers with the reason.", strings.Join(diffs, "\n  "))
	}
}
