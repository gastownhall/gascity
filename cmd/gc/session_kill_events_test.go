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
	"time"

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

// liveNamedSessionBead creates a named-session bead whose runtime is running in sp.
func liveNamedSessionBead(t *testing.T, store beads.Store, sp *runtime.Fake, sessionName string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   "session",
		Status: "open",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "worker",
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "reviewer",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	return b
}

// Stopping a live runtime before a session-bead mutation (duplicate retire,
// removed-named retire) records the stop; both the raw and the Info form.
func TestStopRuntimeBeforeSessionBeadMutationRecordsStoppedEvent(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := events.NewFake()
	b := liveNamedSessionBead(t, store, sp, "s-live")

	if !stopRuntimeBeforeSessionBeadMutation(t.TempDir(), store, sp, nil, b, "removed named session", rec, nil) {
		t.Fatal("stop returned false")
	}
	got := stoppedEventsFor(rec, b.ID)
	if len(got) != 1 || got[0].Subject != "s-live" || !strings.Contains(got[0].Message, "removed named session") {
		t.Fatalf("raw form events = %#v, want one session.stopped naming s-live and the reason", rec.Events)
	}

	infoRec := events.NewFake()
	b2 := liveNamedSessionBead(t, store, sp, "s-live-info")
	if !stopRuntimeBeforeSessionBeadMutationInfo(t.TempDir(), store, sp, nil, sessionInfoFromBead(b2), "duplicate named session", infoRec, nil) {
		t.Fatal("info stop returned false")
	}
	if got := stoppedEventsFor(infoRec, b2.ID); len(got) != 1 || got[0].Subject != "s-live-info" {
		t.Fatalf("info form events = %#v, want one session.stopped naming s-live-info", infoRec.Events)
	}
}

// A runtime that is not running is not killed, so the helper records nothing.
func TestStopRuntimeBeforeSessionBeadMutationNotRunningRecordsNothing(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := events.NewFake()
	b := liveNamedSessionBead(t, store, sp, "s-dead")
	if err := sp.Stop("s-dead"); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}

	if !stopRuntimeBeforeSessionBeadMutation(t.TempDir(), store, sp, nil, b, "removed named session", rec, nil) {
		t.Fatal("stop returned false")
	}
	if len(rec.Events) != 0 {
		t.Fatalf("events = %#v, want none for a runtime that was not running", rec.Events)
	}
}

// The removed-configured-named retire path (the sync path) names the stop.
func TestRetireRemovedConfiguredNamedSessionBeadRecordsStoppedEvent(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := events.NewFake()
	city := t.TempDir()
	b := liveNamedSessionBead(t, store, sp, "s-removed")

	if !retireRemovedConfiguredNamedSessionBead(city, store, testSeatWork(city, nil, store, nil), sp, b, time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC), rec, nil) {
		t.Fatal("retire returned false")
	}
	if sp.IsRunning("s-removed") {
		t.Fatal("runtime still running after retire")
	}
	if got := stoppedEventsFor(rec, b.ID); len(got) != 1 {
		t.Fatalf("session.stopped events = %d, want 1: %#v", len(got), rec.Events)
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

// nodeCallsRecord reports whether n contains a call to a method named Record or
// to recordSessionBeadStop, the session-bead stop helpers' recorder.
func nodeCallsRecord(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				found = found || fun.Sel.Name == "Record"
			case *ast.Ident:
				found = found || fun.Name == "recordSessionBeadStop"
			}
		}
		return !found
	})
	return found
}
