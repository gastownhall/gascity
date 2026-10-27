package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// laneInjectMethods are the calls that type input into a runtime or act on
// its turn, matched by name on any receiver or package: content, interrupt
// and respond, with their fenced forms (NudgeFenced, StopTurnFenced, ...)
// and package-level helpers (runtime.NudgeWithContext,
// session.InterruptTurnOn, ...). They leave out session.Manager.Send,
// whose name mail.Provider.Send shares (laneInjectSites).
var laneInjectMethods = map[string]bool{
	// Content.
	"Nudge": true, "NudgeNow": true, "NudgeFenced": true, "Message": true,
	"SendKeys": true, "SendKeysFenced": true, "FencedKeys": true,
	"Submit": true, "SendImmediate": true, "SendLiveOnly": true, "SendImmediateLiveOnly": true,
	"TryWaitIdleNudge": true, "TryWaitIdleNudgeLiveOnly": true,
	"NudgeWithContext": true, "NudgeWithReceipt": true, "NudgeWithDeliveryKey": true, "SendKeysWithContext": true,
	// Interrupt.
	"Interrupt": true, "InterruptAs": true, "InterruptFenced": true, "InterruptWithContext": true,
	"InterruptTurn": true, "InterruptTurnOn": true,
	"StopTurn": true, "StopTurnFenced": true, "StopTurnIf": true, "StopTurnWith": true,
	// Respond.
	"Respond": true, "RespondFenced": true, "RespondFencedVia": true,
}

// laneInjectSessionPath is the session package's import path: its *Manager
// types input (Submit, StopTurn, Respond, ...).
const laneInjectSessionPath = "github.com/gastownhall/gascity/internal/session"

// laneInjectDirs are the directories the lint scans, relative to the
// repository root, and the prefix their sites carry: cmd/gc's sites are
// bare <file>:<func>, internal/api's internal/api/<file>:<func>.
var laneInjectDirs = []struct{ dir, prefix string }{
	{"cmd/gc", ""},
	{"internal/api", "internal/api/"},
}

// laneInjectClass says when an allowlisted site may run (DES-O1 §9).
type laneInjectClass string

const (
	// laneInjectExecutor: a conversation lane executor. Permanent.
	laneInjectExecutor laneInjectClass = "executor"
	// laneInjectWrapper: a provider forwarder that produces nothing.
	// Permanent.
	laneInjectWrapper laneInjectClass = "wrapper"
	// laneInjectUnlisted: runs only while its kind is not listed in [ops]
	// authoritative; OJ-21 deletes it.
	laneInjectUnlisted laneInjectClass = "unlisted"
	// laneInjectFallback: may run while its kind is listed, for the reason
	// its entry states. The count is pinned (laneInjectFallbackPin) and
	// only shrinks.
	laneInjectFallback laneInjectClass = "fallback"
	// laneInjectLifecycle: a lifecycle operation (O2's), not a lane
	// producer: gc stop's graceful interrupt wave.
	laneInjectLifecycle laneInjectClass = "lifecycle"
	// laneInjectTestSurface: reachable only from tests (legacy handlers
	// served only through Server.ServeHTTP in tests, test-only helpers).
	laneInjectTestSurface laneInjectClass = "test-surface"
)

// laneInjectEntry is one allowlisted site: its class and why.
type laneInjectEntry struct {
	class laneInjectClass
	why   string
}

// laneInjectAllowlist names every non-test site in laneInjectDirs, as
// <prefix><file>:<func> (or <prefix><file>:var <name> for a package-level
// declaration), that may type input into a runtime, interrupt its turn or
// answer its prompt, with its class and why (DES-O1 §9). The executors and
// wrappers are permanent; OJ-21 deletes every unlisted entry, and every
// fallback entry but the excused ones (a session with no row or a
// protocol-mode session, which no lane serves; the drain reminder the gate
// holds, which runs only under the legacy reconciler).
var laneInjectAllowlist = map[string]laneInjectEntry{
	// The conversation lane's live executor.
	"conversation_lane_exec.go:conversationLaneExec.Deliver":          {laneInjectExecutor, "lane executor: content"},
	"conversation_lane_interrupt.go:conversationLaneExec.Interrupt":   {laneInjectExecutor, "lane executor: interrupt"},
	"conversation_lane_respond.go:conversationLaneExec.respondLocked": {laneInjectExecutor, "lane executor: respond"},
	// Provider wrappers forward their base's calls; they produce nothing.
	"status_provider.go:statusProvider.Nudge":          {laneInjectWrapper, "provider wrapper"},
	"status_provider.go:statusProvider.SendKeys":       {laneInjectWrapper, "provider wrapper"},
	"status_provider.go:statusProvider.Interrupt":      {laneInjectWrapper, "provider wrapper"},
	"status_provider.go:statusProvider.Respond":        {laneInjectWrapper, "provider wrapper"},
	"cmd_session.go:attachmentCachingProvider.Respond": {laneInjectWrapper, "provider wrapper"},
	// Lifecycle (O2), not a lane producer.
	"worker_handle.go:workerInterruptSessionTargetWithConfig": {laneInjectLifecycle, "gc stop's graceful interrupt wave (I-g)"},
	// Reachable only from tests.
	"worker_handle.go:workerRespondSessionTargetWithConfig":                   {laneInjectTestSurface, "a test-only helper; no production caller"},
	"internal/api/handler_session_interaction.go:Server.handleSessionRespond": {laneInjectTestSurface, "legacy handler served only through Server.ServeHTTP in tests"},
	"internal/api/handler_session_interaction.go:Server.handleSessionStop":    {laneInjectTestSurface, "legacy handler served only through Server.ServeHTTP in tests"},
	// Unlisted: they run only while their kind is not listed.
	"internal/api/huma_handlers_runs.go:Server.interruptRunHolders":            {laneInjectUnlisted, "run cancel interrupts its holders through the session layer (I-e); with interrupt listed it proposes lane ops"},
	"internal/api/session_control_ops.go:Server.executeStop":                   {laneInjectUnlisted, "POST /stop runs its interrupt inline (I-d); with interrupt listed it proposes a lane op"},
	"internal/api/session_control_ops.go:Server.respondSession":                {laneInjectUnlisted, "POST /respond answers through the session layer (R-a); with respond listed it proposes a lane op"},
	"internal/api/session_resolution.go:Server.sendBackgroundMessageToSession": {laneInjectUnlisted, "extmsg member notices (M11); with message listed they are lane notices"},
	"internal/api/session_resolution.go:Server.submitMessageToSessionFor":      {laneInjectUnlisted, "POST /submit, /messages and a create's initial message (POST /sessions) deliver through handle.Message (M5, I-f); with message listed they propose lane ops"},
	// Fallbacks: they run while their kind is listed.
	"cmd_nudge.go:deliverSessionNudgeLegacy":      {laneInjectFallback, "gc session nudge to a target with no session row, or a protocol-mode session no lane serves (excused at OJ-21)"},
	"cmd_nudge.go:deliverMailNotifyLegacy":        {laneInjectFallback, "mail --notify keeps its legacy delivery (M7)"},
	"cmd_nudge.go:tryDeliverQueuedNudgesByPoller": {laneInjectFallback, "the legacy queue's poller delivers what producers still queue (M9, M10, M12)"},
	"cmd_session.go:cmdSessionSubmit":             {laneInjectFallback, "gc session submit's local fallback delivers through handle.Message (M6)"},
	"cmd_sling.go:deliverSlingNudge":              {laneInjectFallback, "sling --nudge keeps its legacy delivery (M8)"},
	"drain_reminder.go:nudgeDrainReminder":        {laneInjectFallback, "the drain reminder the gate holds, or for a session no lane serves; legacy reconciler only (excused at OJ-21)"},
	"nudge_backstop.go:runNudgeBackstop":          {laneInjectFallback, "planner notice for a session no lane serves (protocol mode; excused at OJ-21)"},
	"warm_bind_nudge.go:nudgeWarmBindSlot":        {laneInjectFallback, "planner notice for a session no lane serves (protocol mode; excused at OJ-21)"},
	"work_ladder_lane.go:ladderDeliver":           {laneInjectFallback, "the work ladder's delivery seam (M4); W5 swaps it for a planner notice"},
}

// laneInjectFallbackPin is how many fallback entries laneInjectAllowlist
// holds. Each producer slice that moves a surface onto the lane moves its
// entries to unlisted and lowers the pin; it never rises.
const laneInjectFallbackPin = 9

// laneInjectSite is one place a scanned file types input: its site name and
// position, and what it does.
type laneInjectSite struct {
	site string
	pos  token.Position
	what string
}

// TestOnlyLanesInjectInput fails when a non-test site in laneInjectDirs
// outside laneInjectAllowlist types input into a runtime, interrupts its
// turn or answers its prompt (laneInjectSites), and when an allowlisted
// site no longer does, so the list only shrinks.
func TestOnlyLanesInjectInput(t *testing.T) {
	root := gcRepoRootFromEnv()
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, d := range laneInjectDirs {
		dir := filepath.Join(root, filepath.FromSlash(d.dir))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dir, err)
		}
		scanned := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			scanned++
			for _, s := range laneInjectSites(fset, d.prefix+name, file) {
				found[s.site] = true
				if _, allowed := laneInjectAllowlist[s.site]; !allowed {
					t.Errorf("%s %s outside the conversation lane: propose an op instead (DES-O1 §9), or allowlist %s with a class and a reason", s.pos, s.what, s.site)
				}
			}
		}
		if scanned == 0 {
			t.Fatalf("no Go sources under %s", dir)
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

// TestLaneInjectFallbacksOnlyShrink pins the count of fallback entries
// (laneInjectFallbackPin) and checks every entry names a known class and a
// reason.
func TestLaneInjectFallbacksOnlyShrink(t *testing.T) {
	known := []laneInjectClass{laneInjectExecutor, laneInjectWrapper, laneInjectUnlisted, laneInjectFallback, laneInjectLifecycle, laneInjectTestSurface}
	fallbacks := 0
	for site, e := range laneInjectAllowlist {
		if !slices.Contains(known, e.class) {
			t.Errorf("%s: unknown class %q", site, e.class)
		}
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s: no reason", site)
		}
		if e.class == laneInjectFallback {
			fallbacks++
		}
	}
	if fallbacks != laneInjectFallbackPin {
		t.Errorf("%d fallback entries, pinned at %d: a site that moved onto the lane lowers the pin; a new fallback is refused (propose an op instead)", fallbacks, laneInjectFallbackPin)
	}
}

// TestLaneInjectSitesSeesEveryShape pins what the lint catches, over the
// shapes a producer could type input through, and what it leaves alone.
func TestLaneInjectSitesSeesEveryShape(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"a function's call", `func f(sp runtime.Provider) error { return sp.Nudge("x", nil) }`, []string{"zz.go:f"}},
		{"a method's call", `func (h *host) f(sp runtime.Provider) { _ = sp.SendKeys("x") }`, []string{"zz.go:host.f"}},
		{"a package-level func literal", `var f = func(sp runtime.Provider) error { return sp.Nudge("x", nil) }`, []string{"zz.go:var f"}},
		{"a method value", `func f(sp runtime.Provider) error { n := sp.Nudge; return n("x", nil) }`, []string{"zz.go:f"}},
		{"a method value of a handle passed on", `func f(h worker.Handle) { run(h.Nudge) }`, []string{"zz.go:f"}},
		{"a package-level method value", `var sp runtime.Provider
var n = sp.Nudge`, []string{"zz.go:var n"}},
		{"a method expression", `var n = runtime.Provider.Nudge`, []string{"zz.go:var n"}},
		{"a message call", `func f(h worker.Handle) { _, _ = h.Message(ctx, by, req) }`, []string{"zz.go:f"}},
		{"a fenced nudge", `func f(fp runtime.FencedInputProvider) error { return fp.NudgeFenced(ctx, "x", content, n) }`, []string{"zz.go:f"}},
		{"fenced keys", `func f(fp runtime.FencedInputProvider) error { return fp.SendKeysFenced(ctx, "x", fence, "Enter") }`, []string{"zz.go:f"}},
		{"a fenced key sequence", `func f(c runtime.Carrier) error { return c.FencedKeys(ctx, "x", fence, body) }`, []string{"zz.go:f"}},
		{"a submit", `func f(m *session.Manager) { _, _ = m.Submit(ctx, by, id, msg, resume, hints, intent) }`, []string{"zz.go:f"}},
		{"an immediate send", `func f(m *session.Manager) { _, _ = m.SendImmediate(ctx, by, id, msg, resume, hints) }`, []string{"zz.go:f"}},
		{"a live-only send", `func f(m *session.Manager) { _, _ = m.SendLiveOnly(ctx, id, msg, procs) }`, []string{"zz.go:f"}},
		{"an immediate live-only send", `func f(m *session.Manager) { _, _ = m.SendImmediateLiveOnly(ctx, id, msg, procs) }`, []string{"zz.go:f"}},
		{"a wait-idle nudge", `func f(m *session.Manager) { _, _ = m.TryWaitIdleNudge(ctx, by, id, src, msg, resume, hints) }`, []string{"zz.go:f"}},
		{"a live-only wait-idle nudge", `func f(m *session.Manager) { _, _ = m.TryWaitIdleNudgeLiveOnly(ctx, id, src, msg, procs) }`, []string{"zz.go:f"}},
		{"a nudge helper", `func f(sp runtime.Provider) error { return runtime.NudgeWithContext(ctx, sp, "x", content, false) }`, []string{"zz.go:f"}},
		{"a receipted nudge helper", `func f(sp runtime.Provider) { _, _ = runtime.NudgeWithReceipt(ctx, sp, "x", content, false) }`, []string{"zz.go:f"}},
		{"a keyed nudge helper", `func f(sp runtime.Provider) error { return runtime.NudgeWithDeliveryKey(ctx, sp, "x", content) }`, []string{"zz.go:f"}},
		{"a keys helper", `func f(sp runtime.Provider) error { return runtime.SendKeysWithContext(ctx, sp, "x", "Enter") }`, []string{"zz.go:f"}},
		{"an interrupt call", `func f(h worker.Handle) error { return h.Interrupt(ctx, req) }`, []string{"zz.go:f"}},
		{"an actor's interrupt call", `func f(h worker.Handle) { _, _ = h.InterruptAs(ctx, by, req) }`, []string{"zz.go:f"}},
		{"a fenced interrupt", `func f(fp runtime.FencedInputProvider) error { return fp.InterruptFenced(ctx, "x", fence) }`, []string{"zz.go:f"}},
		{"an interrupt helper", `func f(sp runtime.Provider) error { return runtime.InterruptWithContext(ctx, sp, "x") }`, []string{"zz.go:f"}},
		{"a protocol turn interrupt", `func (s *srv) f(m *session.Manager) { _, _, _ = m.InterruptTurn(ctx, id, turnID, restart) }`, []string{"zz.go:srv.f"}},
		{"a turn interrupt helper", `func f(tf runtime.TurnFenceProvider) { _, _ = session.InterruptTurnOn(ctx, tf, "x", turnID) }`, []string{"zz.go:f"}},
		{"a stop of a turn", `func f(m *session.Manager) error { return m.StopTurn(id) }`, []string{"zz.go:f"}},
		{"a fenced stop", `func f(m *session.Manager) error { return m.StopTurnFenced(ctx, id, fence) }`, []string{"zz.go:f"}},
		{"a checked stop", `func f(m *session.Manager) error { return m.StopTurnIf(ctx, id, still) }`, []string{"zz.go:f"}},
		{"a shaped stop", `func f(m *session.Manager) { _, _ = m.StopTurnWith(ctx, id, opts) }`, []string{"zz.go:f"}},
		{"a respond", `func f(h worker.Handle) error { return h.Respond(ctx, r) }`, []string{"zz.go:f"}},
		{"a fenced respond", `func f(fp runtime.FencedInteractionProvider) error { return fp.RespondFenced(ctx, "x", fence, r) }`, []string{"zz.go:f"}},
		{"a fenced respond helper", `func f(sp runtime.Provider) error { return runtime.RespondFencedVia(ctx, sp, "x", fence, r) }`, []string{"zz.go:f"}},
		{"a message method value", `func f(h worker.Handle) { run(h.Message) }`, []string{"zz.go:f"}},
		{"an interrupt method value", `func f(sp runtime.Provider) { run(sp.Interrupt) }`, []string{"zz.go:f"}},
		{"a respond method value", `func f(h worker.Handle) { run(h.Respond) }`, []string{"zz.go:f"}},
		{"a manager's stop method value", `import "github.com/gastownhall/gascity/internal/session"
func f(m *session.Manager) { run(m.StopTurn) }`, []string{"zz.go:f"}},
		{"a stop method expression under a renamed import", `import sessionpkg "github.com/gastownhall/gascity/internal/session"
var stop = (*sessionpkg.Manager).StopTurn`, []string{"zz.go:var stop"}},
		{"another package's Manager", `import "example.com/other/session"
func f(m *session.Manager) { run(m.StopTurn) }`, nil},
		{"a message field read", `func f(e *event) string { return e.Message }`, nil},
		{"a config field read", `func f(a *config.Agent) string { return strings.TrimSpace(a.Nudge) }`, nil},
		{"a field write", `func f(cfg *runtime.Config, s string) { cfg.Nudge = s }`, nil},
		{"a composite literal's field", `var a = config.Agent{Nudge: "x"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "zz.go", "package main\n"+tc.src+"\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			var got []string
			for _, s := range laneInjectSites(fset, "zz.go", file) {
				if !slices.Contains(got, s.site) {
					got = append(got, s.site)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("sites = %v, want %v", got, tc.want)
			}
		})
	}
}

// laneInjectSites are the places in file (named name) that type input into
// a runtime, across every declaration: function and method bodies, and
// package-level var and const values (a func literal, a method value). A
// site types input when it calls one of laneInjectMethods on anything, or
// refers to one of those methods without calling it (a method
// value or method expression, which can be called later) on an operand the
// declaration types as a runtime.Provider, a worker.Handle or a
// *session.Manager (laneInjectTyped): the scan has no type checker, so an
// uncalled .Nudge on an operand of another type reads as a field, as
// config.Agent.Nudge is.
//
// Without a type checker the scan cannot type every operand, so a method
// value taken without a call is missed when its operand is a field chain
// (e.deps.sp.Nudge) or a variable declared from a call's result
// (sp := h.SessionProvider(); n := sp.Nudge), and so is a package-level
// helper taken as a value (n := runtime.NudgeWithContext). Calling such a
// value later (n(...)) names no injecting method either. A call is matched
// by name alone, so laneInjectMethods leaves out session.Manager.Send,
// whose name mail.Provider.Send shares. Review catches those; a go/types or
// nogo analyzer would close the gap.
func laneInjectSites(fset *token.FileSet, name string, file *ast.File) []laneInjectSite {
	sess := laneInjectSessionName(file)
	var out []laneInjectSite
	for _, decl := range file.Decls {
		var site string
		var body ast.Node
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			site, body = name+":"+laneInjectFuncName(d), d
		case *ast.GenDecl:
			if d.Tok != token.VAR && d.Tok != token.CONST {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				names := make([]string, len(vs.Names))
				for i, n := range vs.Names {
					names[i] = n.Name
				}
				out = append(out, laneInjectScan(fset, name+":var "+strings.Join(names, ","), vs, laneInjectTypedPkg(file, sess), sess)...)
			}
			continue
		default:
			continue
		}
		typed := laneInjectTypedPkg(file, sess)
		for k := range laneInjectTypedIn(body, sess) {
			typed[k] = true
		}
		out = append(out, laneInjectScan(fset, site, body, typed, sess)...)
	}
	return out
}

// laneInjectSessionName is the name file refers to the session package by:
// its import's name (the scanned files use session, sessionpkg and
// sessions), session for an unnamed import, "" when file does not import
// it.
func laneInjectSessionName(file *ast.File) string {
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err != nil || path != laneInjectSessionPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "session"
	}
	return ""
}

// laneInjectScan is laneInjectSites over one declaration's node, its sites
// named site, with typed the identifiers it types as a provider, handle or
// manager and sess the file's name for the session package.
func laneInjectScan(fset *token.FileSet, site string, node ast.Node, typed map[string]bool, sess string) []laneInjectSite {
	var out []laneInjectSite
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && laneInjectMethods[sel.Sel.Name] {
				called[sel] = true
				out = append(out, laneInjectSite{site: site, pos: fset.Position(x.Pos()), what: "calls ." + sel.Sel.Name + "("})
			}
		case *ast.SelectorExpr:
			if called[x] || !laneInjectMethods[x.Sel.Name] || !laneInjectTyped(x.X, typed, sess) {
				return true
			}
			out = append(out, laneInjectSite{site: site, pos: fset.Position(x.Pos()), what: "takes ." + x.Sel.Name + " as a value"})
		}
		return true
	})
	return out
}

// laneInjectTyped reports whether operand is, by its declaration, a
// runtime.Provider or worker.Handle (or a type of either package whose name
// ends in Provider or Handle) or a *session.Manager: an identifier typed so
// in typed, or the type itself, as in a method expression. sess is the
// file's name for the session package.
func laneInjectTyped(operand ast.Expr, typed map[string]bool, sess string) bool {
	switch x := operand.(type) {
	case *ast.Ident:
		return typed[x.Name]
	case *ast.ParenExpr:
		return laneInjectTyped(x.X, typed, sess)
	case *ast.StarExpr:
		return laneInjectTyped(x.X, typed, sess)
	case *ast.SelectorExpr:
		return laneInjectTypeExpr(x, sess)
	}
	return false
}

// laneInjectTypeExpr reports whether typ names a provider or handle type of
// the runtime or worker package, which every scanned file imports under its
// own name, or the Manager of the session package, which a file may rename:
// sess is the file's name for it (laneInjectSessionName).
func laneInjectTypeExpr(typ ast.Expr, sess string) bool {
	switch x := typ.(type) {
	case *ast.StarExpr:
		return laneInjectTypeExpr(x.X, sess)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return false
		}
		if pkg.Name == sess {
			return x.Sel.Name == "Manager"
		}
		return (pkg.Name == "runtime" || pkg.Name == "worker") &&
			(strings.HasSuffix(x.Sel.Name, "Provider") || strings.HasSuffix(x.Sel.Name, "Handle"))
	}
	return false
}

// laneInjectTypedIn are the identifiers node declares with a provider,
// handle or manager type (sess as in laneInjectTypeExpr): receivers,
// parameters, results and var declarations, including those of nested func
// literals.
func laneInjectTypedIn(node ast.Node, sess string) map[string]bool {
	typed := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			if laneInjectTypeExpr(f.Type, sess) {
				for _, n := range f.Names {
					typed[n.Name] = true
				}
			}
		}
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			addFields(x.Recv)
		case *ast.FuncType:
			addFields(x.Params)
			addFields(x.Results)
		case *ast.ValueSpec:
			if x.Type != nil && laneInjectTypeExpr(x.Type, sess) {
				for _, n := range x.Names {
					typed[n.Name] = true
				}
			}
		}
		return true
	})
	return typed
}

// laneInjectTypedPkg are the package-level identifiers file declares with a
// provider, handle or manager type (sess as in laneInjectTypeExpr), and
// those its package-level func literals' signatures declare.
func laneInjectTypedPkg(file *ast.File, sess string) map[string]bool {
	typed := map[string]bool{}
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
			for k := range laneInjectTypedIn(d, sess) {
				typed[k] = true
			}
		}
	}
	return typed
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
