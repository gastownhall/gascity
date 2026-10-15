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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// nudgeQueueRawAccess names the ways to reach the city's nudge queue other
// than through openNudgeQueue: the state.json primitives, the lock wrapper,
// the shadow front-door constructors, and building a queue value directly.
var nudgeQueueRawAccess = map[string]bool{
	"nudgequeue.WithState":          true,
	"nudgequeue.LoadState":          true,
	"nudgequeue.WithdrawWaitNudges": true,
	"nudgequeue.StatePath":          true,
	"nudgequeue.LockPath":           true,
	"nudgequeue.NewStore":           true,
	"withNudgeQueueState":           true,
	"nudgeFrontDoor":                true,
	"fileNudgeQueue{}":              true,
	"nudgequeue.StateFileQueue{}":   true,
}

// nudgeQueueRawAccessAllowed is where cmd/gc may reach the queue directly.
// Every entry must still be in use, so a retired exception is removed here.
var nudgeQueueRawAccessAllowed = map[string]map[string]bool{
	// The file queue itself.
	"nudge_file_queue.go": nil,
	// openNudgeQueue and nudgeShadows.
	"nudge_queue_seam.go": {"fileNudgeQueue{}": true, "nudgeFrontDoor": true},
	// The shadow front door's definition and its two leaf helpers.
	"nudge_beads.go": {"nudgequeue.NewStore": true, "nudgeFrontDoor": true},
	// The legacy shadow sweep reads shadows, not the queue (DES-NQ D11).
	"nudge_mail_sweep.go": {"nudgequeue.NewStore": true},
	// The mail-sweep watchdog's snapshot; city_runtime.go is owned by the
	// session-reconciler work, which has not yet taken the one-line change
	// to openNudgeQueue (V2-14).
	"city_runtime.go": {"nudgequeue.LoadState": true},
}

// nudgeQueueRawAccessIn returns the raw queue accesses in one Go file, as
// "name" entries, in source order.
func nudgeQueueRawAccessIn(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		var name string
		switch node := n.(type) {
		case *ast.CallExpr:
			switch fun := node.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok {
					name = pkg.Name + "." + fun.Sel.Name
				}
			}
		case *ast.CompositeLit:
			switch typ := node.Type.(type) {
			case *ast.Ident:
				name = typ.Name + "{}"
			case *ast.SelectorExpr:
				if pkg, ok := typ.X.(*ast.Ident); ok {
					name = pkg.Name + "." + typ.Sel.Name + "{}"
				}
			}
		}
		if nudgeQueueRawAccess[name] {
			found = append(found, name)
		}
		return true
	})
	return found
}

// TestNudgeQueueIsReachedOnlyThroughTheSeam keeps openNudgeQueue the one
// constructor of the city's nudge queue in cmd/gc: no other non-test file
// reads or writes state.json, takes its lock, or builds a queue value, so a
// queue selected at the seam is the queue every caller uses.
func TestNudgeQueueIsReachedOnlyThroughTheSeam(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	used := map[string]map[string]bool{}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		allowed, listed := nudgeQueueRawAccessAllowed[name]
		for _, access := range nudgeQueueRawAccessIn(t, filepath.Join(dir, name)) {
			switch {
			case listed && allowed == nil:
			case allowed[access]:
				if used[name] == nil {
					used[name] = map[string]bool{}
				}
				used[name][access] = true
			default:
				violations = append(violations, name+": "+access)
			}
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("reach the nudge queue through openNudgeQueue (nudge_queue_seam.go), not directly:\n  %s", strings.Join(violations, "\n  "))
	}
	for file, accesses := range nudgeQueueRawAccessAllowed {
		for access := range accesses {
			if !used[file][access] {
				t.Errorf("%s no longer calls %s; remove the exception from nudgeQueueRawAccessAllowed", file, access)
			}
		}
	}
}

// The ratchet's matcher sees calls and composite literals, not mentions in
// comments or strings, and it sees a call through a package selector.
func TestNudgeQueueRawAccessMatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.go")
	src := `package main

// nudgequeue.LoadState(cityPath) in a comment is not a call.
var note = "withNudgeQueueState(cityPath, fn)"

func sample(cityPath string) {
	_, _ = nudgequeue.LoadState(cityPath)
	_ = withNudgeQueueState(cityPath, nil)
	_ = &fileNudgeQueue{cityPath: cityPath}
	_ = nudgequeue.StateFileQueue{CityPath: cityPath}
	_ = nudgeShadows(beads.NudgesStore{})
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing the sample: %v", err)
	}
	got := strings.Join(nudgeQueueRawAccessIn(t, path), ",")
	want := "nudgequeue.LoadState,withNudgeQueueState,fileNudgeQueue{},nudgequeue.StateFileQueue{}"
	if got != want {
		t.Fatalf("raw accesses = %s, want %s", got, want)
	}
}

// openNudgeQueue builds the file queue: its location is the state.json path,
// and its shadow view, dispatch skip counters and repin capability are present.
func TestOpenNudgeQueueBuildsTheFileQueue(t *testing.T) {
	city := t.TempDir()
	h := openNudgeQueue(city, beads.NudgesStore{})
	if _, ok := h.Queue.(*fileNudgeQueue); !ok {
		t.Fatalf("Queue = %T, want the file queue", h.Queue)
	}
	if h.Location != nudgequeue.StatePath(city) {
		t.Fatalf("Location = %q, want %q", h.Location, nudgequeue.StatePath(city))
	}
	if h.Shadows == nil || h.Skips == nil || h.Repin == nil {
		t.Fatalf("handle = %+v, want shadows, skips and repin", h)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(nudgequeue.StatePath(city)); !os.IsNotExist(err) {
		t.Fatalf("opening the queue touched state.json (stat err %v); it must open nothing up front", err)
	}
}

// A target built from a ClaimTarget answers to exactly its queue keys,
// normalized, and not to its session id: a target rebuilt through its alias
// history would add the session id as a key.
func TestFileClaimTargetAddressesExactlyItsQueueKeys(t *testing.T) {
	target := fileClaimTarget(nudgeTarget{sessionID: "s-1", continuationEpoch: "2"}.claimTarget(""))
	if got := target.queueKeys(); len(got) != 1 || got[0] != "s-1" {
		t.Fatalf("keys of a session-only target = %q, want [s-1]", got)
	}
	ct := nudgeTarget{alias: "alpha", sessionID: "s-1"}.claimTarget("")
	ct.QueueKeys = []string{" alpha ", "", "alpha", "beta"}
	target = fileClaimTarget(ct)
	if got := strings.Join(target.queueKeys(), ","); got != "alpha,beta" {
		t.Fatalf("keys = %s, want alpha,beta", got)
	}
	if target.matchesQueueAgent("s-1") {
		t.Fatal("the target answered to its session id, which is not one of its queue keys")
	}
	if target.sessionID != "s-1" {
		t.Fatalf("sessionID = %q, want the claim's fence", target.sessionID)
	}
}

// claimTarget asks the census of the city it is given, at most once.
func TestClaimTargetCensusIsTheCitysAndMemoized(t *testing.T) {
	var cities []string
	prev := liveNudgeFenceSessionIDs
	t.Cleanup(func() { liveNudgeFenceSessionIDs = prev })
	liveNudgeFenceSessionIDs = func(cityPath string) map[string]struct{} {
		cities = append(cities, cityPath)
		if cityPath == "/city-down" {
			return nil
		}
		return map[string]struct{}{"s-1": {}}
	}
	ct := nudgeTarget{cityPath: "/elsewhere", sessionID: "s-1"}.claimTarget("/city")
	for range 2 {
		if ids, ok := ct.LiveFenceSessions(); !ok || len(ids) != 1 {
			t.Fatalf("census = %v, %v; want the live session", ids, ok)
		}
	}
	if strings.Join(cities, ",") != "/city" {
		t.Fatalf("census asked of %q, want once of /city", cities)
	}
	if _, ok := (nudgeTarget{}).claimTarget("/city-down").LiveFenceSessions(); ok {
		t.Fatal("an unavailable census reported available")
	}
}
