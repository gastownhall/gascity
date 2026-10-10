package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The runtime lease's provider-verb lint (ARCH-RESTRUCTURE §3.2, I-LEASE).
// Outside the v2 effect and step files (which reconcile_effects_lint_test.go
// covers), every runtime start or stop goes either through a worker-boundary
// helper given the city's real path, so the Manager takes the lease, or
// through a direct provider call in a function listed here with why it may.

// runtimeLeaseCityHelpers are the worker-boundary helpers that may start or
// stop a runtime, by the index of their city path argument.
var runtimeLeaseCityHelpers = map[string]int{
	"workerKillSessionTargetWithConfig":                 0,
	"controllerKillSessionRow":                          0,
	"workerKillSessionTargetCtx":                        1,
	"workerHandleForSessionWithConfig":                  0,
	"workerHandleForSessionWithStaleKeyDetectionWaiter": 0,
	// The controller's stop paths, which hand their city to the helpers.
	"verifiedStop":                                  0,
	"advanceSessionDrainsWithSessionsTraced":        0,
	"stopRuntimeBeforeSessionBeadMutation":          0,
	"stopRuntimeBeforeSessionBeadMutationInfo":      0,
	"cycleAliveSessionForFreshReassign":             0,
	"resetConfiguredNamedSessionForConfigDriftInfo": 0,
	"queueDrainAckAsyncStop":                        0,
	"queueDrainAckForcedTermination":                0,
	"confirmDrainAckRuntimeDead":                    1,
	"controllerStopLease":                           1,
	"cleanupDeadRuntimeSessionCorpses":              0,
	"releaseBeadScopedPoolRuntimeLeased":            0,
	"reapStaleSessionBeads":                         0,
	"autoSuspendChatSessions":                       0,
}

// runtimeLeaseProviderAllowed are the functions whose body may call a
// provider's Start or Stop directly, and why.
var runtimeLeaseProviderAllowed = map[string]string{
	"gracefulStopAllWithForceSignal":         "city stop: stops every runtime (the allowlist)",
	"startPreparedStartCandidate":            "the legacy start: runs under the candidate's lease",
	"stopStaleAsyncStartRuntime":             "a start's stale-commit cleanup: runs under the start's lease, identity-checked",
	"pendingCreateRuntimeClearedForRollback": "an async start's rollback: runs under the start's lease",
	"releaseBeadScopedPoolRuntime":           "a pool runtime's teardown, under the start's lease, or the tick's flock (releaseBeadScopedPoolRuntimeLeased)",
	"stopStillBoundClosedRuntimeLeased":      "the closed-row reaper: takes the lease itself",
	"cleanDeadRuntimeCorpse":                 "a dead runtime's cleanup, under the name's flock (cleanupDeadRuntimeSessionCorpses)",
	"statusProvider.Start":                   "the status wrapper forwarding to its provider",
	"statusProvider.Stop":                    "the status wrapper forwarding to its provider",
}

// runtimeLeaseProviderReceivers are the receiver spellings of a runtime
// provider in cmd/gc.
var runtimeLeaseProviderReceivers = []string{"sp", "provider", "p.sp", "cr.sp", "p.base", "p.Provider"}

// Kills a starter or stopper that slips past the lease: a worker-boundary
// kill or start with no city path (outside a stop sweep), and a direct
// provider Start or Stop outside the allowlist.
func TestRuntimeLeaseCoversEveryStartAndStop(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	used := map[string]bool{}
	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "reconcile_effect") || strings.HasPrefix(base, "reconcile_steps") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		found, hit := lintRuntimeLeaseSource(t, path, src)
		findings = append(findings, found...)
		for fn := range hit {
			used[fn] = true
		}
	}
	for _, f := range findings {
		t.Errorf("a runtime start or stop outside the lease: %s", f)
	}
	for fn := range runtimeLeaseProviderAllowed {
		if !used[fn] {
			t.Errorf("allowlisted %s calls no provider Start or Stop: drop it", fn)
		}
	}
}

func lintRuntimeLeaseSource(t *testing.T, path string, src []byte) (out []string, used map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	text := func(e ast.Expr) string {
		return string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
	}
	used = map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		_, allowed := runtimeLeaseProviderAllowed[funcDeclName(fn)]
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if i, ok := runtimeLeaseCityHelpers[fun.Name]; ok && i < len(call.Args) && text(call.Args[i]) == `""` &&
					(i == 0 || !strings.Contains(text(call.Args[0]), "CitySweepContext")) {
					out = append(out, fmt.Sprintf("%s: %s with no city path", fset.Position(call.Pos()), fun.Name))
				}
			case *ast.SelectorExpr:
				verb := fun.Sel.Name
				provider := slices.Contains(runtimeLeaseProviderReceivers, text(fun.X))
				switch {
				case text(fun) == "runtime.StopForCleanup", provider && verb == "Stop" && len(call.Args) == 1, provider && verb == "Start" && len(call.Args) == 3:
					if !allowed {
						out = append(out, fmt.Sprintf("%s: %s in %s", fset.Position(call.Pos()), text(fun), funcDeclName(fn)))
					}
					used[funcDeclName(fn)] = true
				}
			}
			return true
		})
	}
	return out, used
}

// Kills a lint blind to either rule: a seeded file with each forbidden shape
// is reported, and the same calls in an allowlisted function or a stop sweep
// are not.
func TestRuntimeLeaseLintSeeded(t *testing.T) {
	src := []byte(`package main
func seeded() {
	_ = workerKillSessionTargetWithConfig("", store, sp, cfg, name)
	_ = controllerKillSessionRow("", store, sp, cfg, info)
	_ = workerKillSessionTargetCtx(ctx, "", store, sp, cfg, name)
	_, _ = workerHandleForSessionWithConfig("", store, sp, cfg, id)
	_ = sp.Stop(name)
	_ = sp.Start(ctx, name, cfg)
	_ = runtime.StopForCleanup(sp, name)
}
func gracefulStopAllWithForceSignal() {
	_ = sp.Stop(name)
	_ = workerKillSessionTargetCtx(session.CitySweepContext(ctx), "", store, sp, cfg, name)
	_ = workerKillSessionTargetWithConfig(cityPath, store, sp, cfg, name)
	timer.Stop()
}
`)
	if got, _ := lintRuntimeLeaseSource(t, "seeded.go", src); len(got) != 7 {
		t.Fatalf("seeded findings = %d, want 7:\n%s", len(got), strings.Join(got, "\n"))
	}
}
