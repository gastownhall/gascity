package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// laneInjectMethods are the calls that type input into a runtime.
var laneInjectMethods = map[string]bool{"Nudge": true, "NudgeNow": true, "SendKeys": true}

// laneInjectAllowlist names every non-test cmd/gc function, as
// <file>:<func>, that may type input into a runtime, and why (DES-O1 §9).
// The lane executors are permanent; every other entry is a legacy path that
// OJ-21 deletes or moves behind the lane, emptying the list but for them.
var laneInjectAllowlist = map[string]string{
	// The conversation lane's live executor.
	"conversation_lane_exec.go:conversationLaneExec.Deliver": "lane executor",
	// A provider wrapper forwards its base's calls; it produces nothing.
	"status_provider.go:statusProvider.Nudge":    "provider wrapper",
	"status_provider.go:statusProvider.SendKeys": "provider wrapper",
	// Legacy CLI paths.
	"cmd_nudge.go:deliverSessionNudgeWithWorker":  "legacy CLI: gc session nudge",
	"cmd_nudge.go:sendMailNotifyWithWorker":       "legacy CLI: mail --notify",
	"cmd_nudge.go:tryDeliverQueuedNudgesByPoller": "legacy poller",
	"cmd_sling.go:deliverSlingNudge":              "legacy CLI: sling --nudge",
	// Planner notices with [ops] authoritative empty, or without a lane
	// host for the sessions store.
	"nudge_backstop.go:runNudgeBackstop":   "legacy planner notice",
	"warm_bind_nudge.go:nudgeWarmBindSlot": "legacy planner notice",
	// The drain reminder's audience is a draining row, which the lane's
	// content gate holds.
	"drain_reminder.go:deliverDrainReminder": "drain reminder (content gate holds draining rows)",
}

// TestOnlyLanesInjectInput fails when a non-test cmd/gc function outside
// laneInjectAllowlist calls .Nudge(, .NudgeNow( or .SendKeys(, and when an
// allowlisted function no longer does, so the list only shrinks.
func TestOnlyLanesInjectInput(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			site := name + ":" + laneInjectFuncName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && laneInjectMethods[sel.Sel.Name] {
					found[site] = true
					if _, allowed := laneInjectAllowlist[site]; !allowed {
						t.Errorf("%s calls .%s( outside the conversation lane: propose a message op instead (DES-O1 §9), or allowlist it with a reason", fset.Position(call.Pos()), sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	var stale []string
	for site := range laneInjectAllowlist {
		if !found[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(stale)
	for _, site := range stale {
		t.Errorf("allowlisted %s no longer types input; remove it from laneInjectAllowlist", site)
	}
}

// laneInjectFuncName is fn's name, Type.method for a method.
func laneInjectFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	for {
		switch t := typ.(type) {
		case *ast.StarExpr:
			typ = t.X
			continue
		case *ast.IndexExpr:
			typ = t.X
			continue
		case *ast.IndexListExpr:
			typ = t.X
			continue
		case *ast.Ident:
			return t.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}
