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
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// nudgeQueueShadowWriters write a queue item's shadow outside the queue's
// verbs. They are reached through the queue (Enqueue, Ack, RecordFailure,
// Rollback), never directly, so a queue selected at the seam owns its shadows.
var nudgeQueueShadowWriters = []string{"ensureQueuedNudgeBead", "markQueuedNudgeTerminal"}

// nudgeFileQueueFile declares the file queue's mechanics. Its top-level
// functions are the queue's internals: outside it and the seam, a call to one
// bypasses openNudgeQueue as surely as a raw state.json read does.
const nudgeFileQueueFile = "nudge_file_queue.go"

// nudgeQueueRawAccessAllowed is where cmd/gc may reach the queue directly,
// and how many times. A nil entry allows every access. Every count must match
// exactly, so a retired exception is removed here and a second call beside an
// excepted one fails rather than riding on the exception.
var nudgeQueueRawAccessAllowed = map[string]map[string]int{
	// The file queue itself.
	nudgeFileQueueFile: nil,
	// openNudgeQueue and nudgeShadows.
	"nudge_queue_seam.go": {"fileNudgeQueue{}": 1, "nudgeFrontDoor": 1},
	// The shadow front door's definition and its two leaf helpers.
	"nudge_beads.go": {"nudgequeue.NewStore": 1, "nudgeFrontDoor": 2},
	// The legacy shadow sweep reads shadows, not the queue (DES-NQ D11).
	"nudge_mail_sweep.go": {"nudgequeue.NewStore": 2},
	// The mail-sweep watchdog's snapshot in runNudgeMailSweepWatchdog;
	// city_runtime.go is owned by the session-reconciler work, which has not
	// yet taken the one-line change to openNudgeQueue (V2-14). Exactly one.
	"city_runtime.go": {"nudgequeue.LoadState": 1},
}

// nudgeQueueForbidden is every raw access the ratchet rejects: the raw
// primitives, the shadow writers, and the file queue's top-level functions.
func nudgeQueueForbidden(t *testing.T, dir string) map[string]bool {
	t.Helper()
	forbidden := make(map[string]bool, len(nudgeQueueRawAccess))
	for name := range nudgeQueueRawAccess {
		forbidden[name] = true
	}
	for _, name := range nudgeQueueShadowWriters {
		forbidden[name] = true
	}
	path := filepath.Join(dir, nudgeFileQueueFile)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	funcs := 0
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			forbidden[fn.Name.Name] = true
			funcs++
		}
	}
	if funcs == 0 {
		t.Fatalf("%s declares no top-level functions; the ratchet would guard nothing", path)
	}
	return forbidden
}

// nudgeQueueRawAccessIn returns the accesses in one Go file that forbidden
// names, as "name" entries, in source order.
func nudgeQueueRawAccessIn(t *testing.T, path string, forbidden map[string]bool) []string {
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
		if forbidden[name] {
			found = append(found, name)
		}
		return true
	})
	return found
}

// TestNudgeQueueIsReachedOnlyThroughTheSeam keeps openNudgeQueue the one
// constructor of the city's nudge queue in cmd/gc: no other non-test file
// reads or writes state.json, takes its lock, builds a queue value, calls the
// file queue's internals, or writes a shadow behind the queue's back, so a
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
	forbidden := nudgeQueueForbidden(t, dir)
	used := map[string]map[string]int{}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		allowed, listed := nudgeQueueRawAccessAllowed[name]
		for _, access := range nudgeQueueRawAccessIn(t, filepath.Join(dir, name), forbidden) {
			switch {
			case listed && allowed == nil:
			case allowed[access] > 0:
				if used[name] == nil {
					used[name] = map[string]int{}
				}
				used[name][access]++
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
		for access, want := range accesses {
			if got := used[file][access]; got != want {
				t.Errorf("%s calls %s %d times, want exactly %d: route a new call through openNudgeQueue, or remove a retired exception from nudgeQueueRawAccessAllowed", file, access, got, want)
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
	_, _ = claimDueQueuedNudgesMatching(cityPath, time.Time{}, nil)
	_ = runNudgeQueueMaintenanceSweep(cityPath, time.Time{})
	_, _, _ = ensureQueuedNudgeBead(beads.NudgesStore{}, queuedNudge{})
	_ = markQueuedNudgeTerminal(beads.NudgesStore{}, queuedNudge{}, "", "", "", time.Time{})
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("writing the sample: %v", err)
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	got := strings.Join(nudgeQueueRawAccessIn(t, path, nudgeQueueForbidden(t, gcCallerDir(currentFile))), ",")
	want := "nudgequeue.LoadState,withNudgeQueueState,fileNudgeQueue{},nudgequeue.StateFileQueue{}," +
		"claimDueQueuedNudgesMatching,runNudgeQueueMaintenanceSweep,ensureQueuedNudgeBead,markQueuedNudgeTerminal"
	if got != want {
		t.Fatalf("raw accesses = %s, want %s", got, want)
	}
}

// openNudgeQueue builds the file queue: its location is the state.json path,
// and its dispatch skip counters and repin capability are present.
func TestOpenNudgeQueueBuildsTheFileQueue(t *testing.T) {
	city := t.TempDir()
	h := openNudgeQueue(city, beads.NudgesStore{})
	if _, ok := h.Queue.(*fileNudgeQueue); !ok {
		t.Fatalf("Queue = %T, want the file queue", h.Queue)
	}
	if h.Location != nudgequeue.StatePath(city) {
		t.Fatalf("Location = %q, want %q", h.Location, nudgequeue.StatePath(city))
	}
	if h.Skips == nil || h.Repin == nil {
		t.Fatalf("handle = %+v, want skips and repin", h)
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

// The controller hands the API the city's file queue over its nudges store: a
// deferred submit lands in state.json, and a wait-nudge withdrawal closes the
// shadow in that store.
func TestControllerStateNudgeQueueIsTheCitysFileQueue(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	cs := &controllerState{cityPath: city, cfg: &config.City{}, cityBeadStore: store}
	queue := cs.NudgeQueue()
	if _, ok := queue.(*fileNudgeQueue); !ok {
		t.Fatalf("NudgeQueue = %T, want the file queue", queue)
	}

	now := time.Now().UTC()
	deferred := newQueuedNudgeWithOptions("alpha", "later", "session", now, queuedNudgeOptions{ID: "nudge-deferred01"})
	if err := queue.EnqueueDeferred(deferred); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	wait := newQueuedNudgeWithOptions("alpha", "wait done", "wait", now, queuedNudgeOptions{ID: "wait-w1-0-1"})
	beadID, _, err := nudgeShadows(beads.NudgesStore{Store: store}).Save(wait)
	if err != nil {
		t.Fatalf("saving the wait shadow: %v", err)
	}
	wait.BeadID = beadID
	if err := queue.EnqueueDeferred(wait); err != nil {
		t.Fatalf("EnqueueDeferred(wait): %v", err)
	}
	if err := queue.WithdrawQueuedWaitNudges([]string{wait.ID}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	state, err := nudgequeue.LoadState(city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != deferred.ID {
		t.Fatalf("pending = %+v, want only the deferred submit", state.Pending)
	}
	shadow, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("Get shadow: %v", err)
	}
	if shadow.Status != "closed" {
		t.Fatalf("wait shadow status = %q, want closed by the withdrawal", shadow.Status)
	}
}
