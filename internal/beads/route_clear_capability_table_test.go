package beads_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// routeClearCapabilityRow classifies one optional capability for the
// route-clearing seam (ga-l69b9m, architect ruling C-prime on ga-cn94949q).
//
// A capability is gated when it can write gc.routed_to on an existing bead and
// the resolver that hands it out returns the gated wrapper for a walk that
// crossed the route-clearing decorator. Every other capability carries a
// reason it cannot re-route a bead, so adding a capability to
// resolveCapability fails this table until someone classifies it.
type routeClearCapabilityRow struct {
	// capability is the interface name as it appears in resolveCapability's
	// type switch, or the bare name for a capability the switch does not list.
	capability string
	// inSwitch reports that resolveCapability has a case for it.
	inSwitch bool
	// gated marks a capability the seam gates.
	gated bool
	// pinnedBy names the test that proves a gated capability clears stamps.
	pinnedBy string
	// reason says why an ungated capability cannot re-route a bead.
	reason string
}

func routeClearCapabilityRows() []routeClearCapabilityRow {
	const seamTest = "TestRouteClearSeam_GenuineRerouteThroughCapabilityClearsStamps"
	return []routeClearCapabilityRow{
		{capability: "ConditionalWriter", inSwitch: true, gated: true, pinnedBy: seamTest},
		{capability: "MetadataCASWriter", inSwitch: true, gated: true, pinnedBy: seamTest},
		{capability: "AssignmentGuardedUpdater", inSwitch: true, gated: true, pinnedBy: seamTest},

		{
			capability: "GuardedBatcher", inSwitch: true,
			reason: "applies pinned creates and revision-guarded updates to conversation-lane rows; no gc.routed_to producer calls it today (open question to the architect: gcg-1862657)",
		},
		{
			capability: "GraphApplyStore", inSwitch: true,
			reason: "creates the beads of one plan; a new bead has no earlier gc.routed_to to supersede",
		},
		{
			capability: "AtomicConditionalCloser", inSwitch: true,
			reason: "terminal write of a closing bead; a closed bead is not re-routed",
		},
		{capability: "NamespaceCensus", inSwitch: true, reason: "read-only"},
		{capability: "DependencyBatchLister", inSwitch: true, reason: "read-only"},
		{capability: "GraphOnlyReadyStore", inSwitch: true, reason: "read-only"},
		{capability: "AbsentBlockedReader", inSwitch: true, reason: "read-only"},

		{
			capability: "ConditionalAssignmentReleaser",
			reason:     "writes the assignee only; it takes no metadata",
		},
		{
			capability: "ConditionalAssigneeTransferer",
			reason:     "writes the assignee only; it takes no metadata",
		},
	}
}

// resolveCapabilityCases parses class_store_as.go and returns the interface
// names resolveCapability's type switch lists.
func resolveCapabilityCases(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(findRepoRoot(t), "internal", "beads", "class_store_as.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var cases []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "resolveCapability" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if star, ok := expr.(*ast.StarExpr); ok {
					if ident, ok := star.X.(*ast.Ident); ok {
						cases = append(cases, ident.Name)
					}
				}
			}
			return true
		})
	}
	sort.Strings(cases)
	return cases
}

// Every case of resolveCapability has a row, and every row that claims a case
// has one. A new capability fails here until it is classified.
func TestRouteClearCapabilityTableCoversResolveCapability(t *testing.T) {
	rows := routeClearCapabilityRows()
	byName := map[string]routeClearCapabilityRow{}
	for _, row := range rows {
		if _, dup := byName[row.capability]; dup {
			t.Errorf("duplicate row for %s", row.capability)
		}
		byName[row.capability] = row
	}

	cases := resolveCapabilityCases(t)
	if len(cases) == 0 {
		t.Fatal("found no case in resolveCapability's type switch; the parser lost the switch")
	}
	inSwitch := map[string]bool{}
	for _, name := range cases {
		inSwitch[name] = true
		row, ok := byName[name]
		if !ok {
			t.Errorf("resolveCapability has a case for %s but the table has no row: classify it as gated or give the reason it cannot re-route a bead", name)
			continue
		}
		if !row.inSwitch {
			t.Errorf("%s is a resolveCapability case but its row says inSwitch=false", name)
		}
	}
	for _, row := range rows {
		if row.inSwitch && !inSwitch[row.capability] {
			t.Errorf("row %s claims a resolveCapability case that no longer exists", row.capability)
		}
		switch {
		case row.gated && row.pinnedBy == "":
			t.Errorf("gated row %s names no test that pins it", row.capability)
		case !row.gated && strings.TrimSpace(row.reason) == "":
			t.Errorf("ungated row %s gives no reason", row.capability)
		}
	}
}

// A gated row's pinning test exists in this package.
func TestRouteClearCapabilityTablePinningTestsExist(t *testing.T) {
	dir := filepath.Join(findRepoRoot(t), "internal", "beads")
	src, err := os.ReadFile(filepath.Join(dir, "route_clear_seam_test.go"))
	if err != nil {
		t.Fatalf("reading the seam tests: %v", err)
	}
	for _, row := range routeClearCapabilityRows() {
		if !row.gated {
			continue
		}
		if !strings.Contains(string(src), "func "+row.pinnedBy+"(") {
			t.Errorf("gated row %s is pinned by %s, which route_clear_seam_test.go does not define", row.capability, row.pinnedBy)
		}
	}
}
