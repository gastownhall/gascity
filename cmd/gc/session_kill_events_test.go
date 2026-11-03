package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// stoppedEventsFor returns the session.stopped events a recorder holds for the
// session bead id.
func stoppedEventsFor(rec *events.Fake, sessionID string) []events.Event {
	var out []events.Event
	for _, e := range rec.Events {
		if e.Type == events.SessionStopped && e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out
}

// A restart-requested kill of a LIVE session must leave a session event: the
// legacy reconciler stopped it, cleared the request and `continue`d with only a
// stdout line, so `gc events` showed nothing for a session that had just died.
func TestReconcileSessionBeads_RestartRequestKillOfLiveSessionEmitsStoppedEvent(t *testing.T) {
	env := newReconcilerTestEnv(t)
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("restart-requested live session should have been stopped")
	}
	got := stoppedEventsFor(rec, session.ID)
	if len(got) != 1 {
		t.Fatalf("session.stopped events for %s = %d, want 1: %#v", session.ID, len(got), rec.Events)
	}
	if got[0].Subject == "" {
		t.Errorf("session.stopped event has no Subject: %#v", got[0])
	}
	if !strings.Contains(got[0].Message, "restart") {
		t.Errorf("session.stopped Message = %q, want it to name the restart request", got[0].Message)
	}
}

// A restart request on a session whose runtime is already dead kills nothing,
// so it must not claim a stop.
func TestReconcileSessionBeads_RestartRequestOnDeadSessionEmitsNoStoppedEvent(t *testing.T) {
	env := newReconcilerTestEnv(t)
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
	})

	env.reconcile([]beads.Bead{session})

	if got := stoppedEventsFor(rec, session.ID); len(got) != 0 {
		t.Fatalf("session.stopped events for a dead session = %d, want 0: %#v", len(got), got)
	}
}

// killSiteAllowlist names the controllerKillSessionRow call sites that do not
// record a stop in their own block, with why each is still honest.
var killSiteAllowlist = map[string]string{
	"session_bead_cycle.go":          "out of scope here: ga-uci1jl adds the session.draining event to this fresh-reassign cycle; when it lands this entry goes stale and the test fails until it is deleted",
	"session_pool_drain_deadline.go": "the caller records the drain-deadline event before it calls the kill (session.draining at the deadline)",
	"session_reconciler.go:resetConfiguredNamedSessionForConfigDriftInfo": "the live-session caller records session.draining (config drift detected) right after the reset; the other caller passes alive=false and kills nothing",
}

// TestEveryLiveSessionKillSiteRecordsAnEvent is the guard for ga-2t51lm: every
// production call to controllerKillSessionRow (the kill that can stop a RUNNING
// session) must be followed, in the same block, by a Record call, or be
// allowlisted with the reason it is evented elsewhere. A new silent kill site,
// or a recorder dropped from an existing one, fails here.
func TestEveryLiveSessionKillSiteRecordsAnEvent(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)

	fset := token.NewFileSet()
	seenAllow := map[string]bool{}
	sites := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "controllerKillSessionRow" {
				continue
			}
			checkKillSites(t, fset, name, fn, killSiteAllowlist, seenAllow, &sites)
		}
	}
	for key := range killSiteAllowlist {
		if !seenAllow[key] {
			t.Errorf("stale killSiteAllowlist entry %q: no unevented kill site matches it", key)
		}
	}
	if sites == 0 {
		t.Fatal("found no controllerKillSessionRow call sites; the guard is not looking at the code")
	}
}

// checkKillSites walks fn for controllerKillSessionRow calls and reports each
// whose enclosing block holds no Record call after the kill.
func checkKillSites(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl, allow map[string]string, seenAllow map[string]bool, sites *int) {
	t.Helper()
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "controllerKillSessionRow" {
			return true
		}
		*sites++
		// The innermost statement list (block or switch-case body) holding the
		// statement that contains the kill: Record must follow it there.
		var list []ast.Stmt
		var stmt ast.Stmt
		for i := len(stack) - 2; i >= 0 && list == nil; i-- {
			switch p := stack[i].(type) {
			case *ast.BlockStmt:
				list = p.List
			case *ast.CaseClause:
				list = p.Body
			case *ast.CommClause:
				list = p.Body
			default:
				continue
			}
			stmt, _ = stack[i+1].(ast.Stmt)
		}
		if list == nil || stmt == nil || listRecordsAfter(list, stmt) {
			return true
		}
		for _, key := range []string{file + ":" + fn.Name.Name, file} {
			if _, ok := allow[key]; ok {
				seenAllow[key] = true
				return true
			}
		}
		t.Errorf("%s: %s calls controllerKillSessionRow at %s with no Record call after it in the same block; record a session event for the stop, or allowlist it with the reason it is evented elsewhere",
			file, fn.Name.Name, fset.Position(call.Pos()))
		return true
	})
}

// listRecordsAfter reports whether stmt's else branch (the success side of
// `if err := kill(); err != nil {...} else {...}`) or a statement after stmt in
// list calls a method named Record.
func listRecordsAfter(list []ast.Stmt, stmt ast.Stmt) bool {
	after := false
	found := false
	if ifs, ok := stmt.(*ast.IfStmt); ok && ifs.Else != nil {
		found = nodeCallsRecord(ifs.Else)
	}
	for _, s := range list {
		if s == stmt {
			after = true
			continue
		}
		if !after {
			continue
		}
		found = found || nodeCallsRecord(s)
	}
	return found
}

// nodeCallsRecord reports whether n contains a call to a method named Record.
func nodeCallsRecord(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Record" {
				found = true
			}
		}
		return !found
	})
	return found
}
