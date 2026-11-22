package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The work-leg claim routing unit rows (claim_work_leg_route.go). The
// split-topology conformance suite (I15) and the #9159 repro
// (TestHookClaimLandsRigRoutedCandidateInTheRigStore) own the end-to-end
// statement through the production claim path; these rows own the routing
// RULE itself — which answers open the probe, which leg a homed bead's later
// ops follow, what never probes, and what an unreadable leg costs.

// workLegTestCity builds a hand-held config over real directories: the city
// dir plus one dir per rig, rigs named for their base. The route builder only
// needs the rig paths to resolve (agentCommandDir) and the suspension state to
// be readable (absent on a fresh temp dir).
func workLegTestCity(t *testing.T, rigNames ...string) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{}
	for _, name := range rigNames {
		dir := filepath.Join(cityPath, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: dir})
	}
	return cityPath, cfg
}

// stubWorkLegEnv replaces the leg env builder with one that always succeeds,
// counting the legs whose env was built. Building an env is the cost AC-4 pins
// as paid only when a probe reads that leg.
func stubWorkLegEnv(t *testing.T) *int32 {
	t.Helper()
	var builds int32
	original := hookStoreQueryEnv
	t.Cleanup(func() { hookStoreQueryEnv = original })
	hookStoreQueryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) {
		builds++
		return map[string]string{"GC_TEST_WORK_LEG": "1"}, nil
	}
	return &builds
}

// workLegOpsRecorder is a base ops whose seams record the dir each call ran
// against and answer from a per-dir script: reads resolve id from holds (dir ->
// bead), claims succeed only in holds, and everything else fails closed.
type workLegOpsRecorder struct {
	mu       sync.Mutex
	claims   []string
	reads    []string
	stamps   []string
	releases []string
	restamps []string
	emits    []string
	holdErr  map[string]error // read dir -> read failure (non-not-found)
}

func (w *workLegOpsRecorder) ops(cityPath string, holds map[string]beads.Bead) hookClaimOps {
	notFound := func(dir string) error {
		if err, ok := w.holdErr[dir]; ok {
			return err
		}
		return beads.ErrNotFound
	}
	return hookClaimOps{
		Claim: func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			w.mu.Lock()
			w.claims = append(w.claims, dir)
			w.mu.Unlock()
			if err := notFound(dir); !errors.Is(err, beads.ErrNotFound) {
				return beads.Bead{}, false, err
			}
			if held, ok := holds[dir]; ok && held.ID == beadID {
				claimed := held
				claimed.Status = "in_progress"
				claimed.Assignee = assignee
				return claimed, true, nil
			}
			return beads.Bead{}, false, beads.ErrNotFound
		},
		ReadWorkMeta: func(_ context.Context, dir string, _ []string, beadID, _ string) (beads.Bead, error) {
			w.mu.Lock()
			w.reads = append(w.reads, dir)
			w.mu.Unlock()
			if err := notFound(dir); !errors.Is(err, beads.ErrNotFound) {
				return beads.Bead{}, err
			}
			if held, ok := holds[dir]; ok && held.ID == beadID {
				return held, nil
			}
			return beads.Bead{}, beads.ErrNotFound
		},
		StampWorkMeta: func(_ context.Context, dir string, _ []string, beadID, _ string, _ map[string]string) error {
			w.mu.Lock()
			w.stamps = append(w.stamps, dir)
			w.mu.Unlock()
			if err := notFound(dir); !errors.Is(err, beads.ErrNotFound) {
				return err
			}
			if held, ok := holds[dir]; ok && held.ID == beadID {
				return nil
			}
			return beads.ErrNotFound
		},
		Release: func(_ context.Context, dir string, _ []string, _, _ string) (bool, error) {
			w.mu.Lock()
			w.releases = append(w.releases, dir)
			w.mu.Unlock()
			return true, nil
		},
		RestampAdopted: func(_ context.Context, dir string, _ []string, _, _, _ string) (bool, error) {
			w.mu.Lock()
			w.restamps = append(w.restamps, dir)
			w.mu.Unlock()
			return true, nil
		},
		EmitExecutionStepStarted: func(_ beads.Bead, dir string, _ []string, _ string) {
			w.mu.Lock()
			w.emits = append(w.emits, dir)
			w.mu.Unlock()
		},
		StoreRef: func(dir string) string {
			switch dir {
			case cityPath:
				return "city"
			default:
				return "rig:" + filepath.Base(dir)
			}
		},
	}
}

func (w *workLegOpsRecorder) snapshot() (claims, reads []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.claims...), append([]string(nil), w.reads...)
}

// TestWorkLegClaimLandsInTheLegThatHoldsTheBead is AC-2: a rig-scoped agent's
// primary is its own rig store, and a bead held only by the city work store or
// another serving rig's store is claimed THERE, not in the primary.
func TestWorkLegClaimLandsInTheLegThatHoldsTheBead(t *testing.T) {
	for _, tt := range []struct {
		name     string
		holdsAt  string // the dir whose store holds the bead
		wantLegs []string
	}{
		{name: "held by the city work store", holdsAt: "city", wantLegs: []string{"city"}},
		{name: "held by another serving rig", holdsAt: "rig-b", wantLegs: []string{"rig-b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubWorkLegEnv(t)
			cityPath, cfg := workLegTestCity(t, "rig-a", "rig-b")
			rigA := filepath.Join(cityPath, "rig-a")
			rigB := filepath.Join(cityPath, "rig-b")
			holds := map[string]beads.Bead{}
			var holdDir string
			switch tt.holdsAt {
			case "city":
				holdDir = cityPath
			case "rig-b":
				holdDir = rigB
			}
			holds[holdDir] = beads.Bead{ID: "zz-1", Status: "open"}
			rec := &workLegOpsRecorder{}
			base := rec.ops(cityPath, holds)
			// A rig-scoped agent: GC_AGENT resolves to the configured rig
			// agent, whose own rig store is the primary.
			cfg.Agents = []config.Agent{{Name: "worker", Dir: "rig-a"}}
			queryEnv := []string{"GC_AGENT=rig-a/worker"}
			route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, queryEnv, []hookStore{{dir: rigA, env: queryEnv}}, io.Discard)
			if route == nil {
				t.Fatal("rig-scoped claim with other work legs must build a route")
			}
			ops := workLegRoutedHookClaimOps(base, route)
			claimed, ok, err := ops.Claim(context.Background(), rigA, queryEnv, "zz-1", "worker-1")
			if err != nil || !ok {
				t.Fatalf("routed claim of zz-1 = (ok=%v err=%v), want a successful claim", ok, err)
			}
			if claimed.Assignee != "worker-1" {
				t.Fatalf("claim returned assignee %q, want worker-1", claimed.Assignee)
			}
			claims, _ := rec.snapshot()
			// The primary attempt, then the write through the holding leg.
			want := append([]string{rigA}, tt.wantLegs...)
			gotDirs := make([]string, len(claims))
			for i, d := range claims {
				gotDirs[i] = relativeWorkLegDir(cityPath, d)
			}
			wantDirs := make([]string, len(want))
			for i, d := range want {
				wantDirs[i] = relativeWorkLegDir(cityPath, d)
			}
			if strings.Join(gotDirs, ",") != strings.Join(wantDirs, ",") {
				t.Fatalf("claims ran in %v, want %v", gotDirs, wantDirs)
			}
		})
	}
}

func relativeWorkLegDir(cityPath, dir string) string {
	rel, err := filepath.Rel(cityPath, dir)
	if err != nil {
		return dir
	}
	if rel == "." {
		return "city"
	}
	return rel
}

// TestWorkLegPostClaimOpsFollowTheHomeLeg is AC-3: once a claim is routed to
// a holding leg, the meta stamp, the canonical readback, an undelivered
// claim's release, an adoption re-stamp and the lifecycle emission all run
// with that leg's dir and env, and the work_started key names the home leg as
// a proven residence. The primary leg's store is not written.
func TestWorkLegPostClaimOpsFollowTheHomeLeg(t *testing.T) {
	stubWorkLegEnv(t)
	cityPath, cfg := workLegTestCity(t, "rig-a", "rig-b")
	rigA := filepath.Join(cityPath, "rig-a")
	rigB := filepath.Join(cityPath, "rig-b")
	holds := map[string]beads.Bead{rigB: {ID: "zz-2", Status: "open"}}
	rec := &workLegOpsRecorder{}
	base := rec.ops(cityPath, holds)
	queryEnv := []string{"GC_AGENT=rig-a/worker"}
	cfg.Agents = []config.Agent{{Name: "worker", Dir: "rig-a"}}
	route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, queryEnv, []hookStore{{dir: rigA, env: queryEnv}}, io.Discard)
	ops := workLegRoutedHookClaimOps(base, route)
	if _, ok, err := ops.Claim(context.Background(), rigA, queryEnv, "zz-2", "worker-1"); err != nil || !ok {
		t.Fatalf("routed claim = (ok=%v err=%v)", ok, err)
	}
	ctx := context.Background()
	if err := ops.StampWorkMeta(ctx, rigA, queryEnv, "zz-2", "worker-1", map[string]string{"gc.session": "s"}); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := ops.ReadWorkMeta(ctx, rigA, queryEnv, "zz-2", "worker-1"); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if _, err := ops.Release(ctx, rigA, queryEnv, "zz-2", "worker-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := ops.RestampAdopted(ctx, rigA, queryEnv, "zz-2", "old", "worker-1"); err != nil {
		t.Fatalf("restamp: %v", err)
	}
	ops.EmitExecutionStepStarted(beads.Bead{ID: "zz-2"}, rigA, queryEnv, "worker-1")
	for name, dirs := range map[string][]string{
		"stamps":   rec.stamps,
		"reads":    dropFirst(rec.reads), // the probe's read of rig-b is the first
		"releases": rec.releases,
		"restamps": rec.restamps,
		"emits":    rec.emits,
	} {
		for _, d := range dirs {
			if d != rigB {
				t.Fatalf("%s ran in %s, want the home leg %s", name, d, rigB)
			}
		}
		if len(dirs) == 0 {
			t.Fatalf("%s never ran", name)
		}
	}
	key := hookStartedKeyFor(ops, rigA, "zz-2", hookClaimMinted)
	if key.Store != "rig:rig-b" || key.Bead != "zz-2" || !key.Resident {
		t.Fatalf("started key = %+v, want Store=rig:rig-b Bead=zz-2 Resident=true", key)
	}
	// A bead the route never homed keeps the leg's own spelling: the primary
	// answered for it.
	other := hookStartedKeyFor(ops, rigA, "zz-3", hookClaimMinted)
	if other.Store != "rig:rig-a" {
		t.Fatalf("unrouted started key = %+v, want Store=rig:rig-a", other)
	}
}

func dropFirst(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	return xs[1:]
}

// TestWorkLegRouteProbesNothingItNeedNotRead is AC-4: a candidate the primary
// holds, or one whose id is inside a relocated class's reserved namespace,
// builds no other leg's environment and reads no other leg.
func TestWorkLegRouteProbesNothingItNeedNotRead(t *testing.T) {
	for _, tt := range []struct {
		name   string
		beadID string
	}{
		{name: "primary holds the candidate", beadID: "zz-10"},
		{name: "reserved class namespace", beadID: "gcg-11"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			builds := stubWorkLegEnv(t)
			cityPath, cfg := workLegTestCity(t, "rig-a", "rig-b")
			rigA := filepath.Join(cityPath, "rig-a")
			if tt.name == "reserved class namespace" {
				// The primary does NOT hold it and the write proves absence;
				// only the reserved namespace keeps the probe closed.
				rec := &workLegOpsRecorder{}
				base := rec.ops(cityPath, nil)
				route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, nil, []hookStore{{dir: rigA}}, io.Discard)
				ops := workLegRoutedHookClaimOps(base, route)
				if _, _, err := ops.Claim(context.Background(), rigA, nil, tt.beadID, "w"); !errors.Is(err, beads.ErrNotFound) {
					t.Fatalf("reserved-id claim err = %v, want the primary's not-found as-is", err)
				}
				if *builds != 0 {
					t.Fatalf("env built %d times, want 0", *builds)
				}
				if _, reads := rec.snapshot(); len(reads) != 0 {
					t.Fatalf("read legs %v, want none", reads)
				}
				return
			}
			holds := map[string]beads.Bead{rigA: {ID: tt.beadID, Status: "open"}}
			rec := &workLegOpsRecorder{}
			base := rec.ops(cityPath, holds)
			route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, nil, []hookStore{{dir: rigA}}, io.Discard)
			ops := workLegRoutedHookClaimOps(base, route)
			if _, ok, err := ops.Claim(context.Background(), rigA, nil, tt.beadID, "w"); err != nil || !ok {
				t.Fatalf("claim = (ok=%v err=%v)", ok, err)
			}
			if *builds != 0 {
				t.Fatalf("env built %d times, want 0", *builds)
			}
			if _, reads := rec.snapshot(); len(reads) != 0 {
				t.Fatalf("read legs %v, want none", reads)
			}
		})
	}
}

// TestWorkLegRouteProvesAbsenceInEveryLegBeforeTheBindingEscalates is AC-5:
// a work-shaped id only the class binding holds is read for in every other
// serving work leg first, each proving absence, and the claim then escalates
// to the binding exactly as it did before this route existed.
func TestWorkLegRouteProvesAbsenceInEveryLegBeforeTheBindingEscalates(t *testing.T) {
	stubWorkLegEnv(t)
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "zz-500", nil)
	classRoute := newClaimRouteFor(t, class)
	cityPath, cfg := workLegTestCity(t, "rig-a", "rig-b")
	rigA := filepath.Join(cityPath, "rig-a")
	rec := &workLegOpsRecorder{}
	base := rec.ops(cityPath, nil) // no leg holds zz-500
	queryEnv := []string{"GC_AGENT=rig-a/worker"}
	cfg.Agents = []config.Agent{{Name: "worker", Dir: "rig-a"}}
	route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, queryEnv, []hookStore{{dir: rigA, env: queryEnv}}, io.Discard)
	ops := classRoutedHookClaimOps(workLegRoutedHookClaimOps(base, route), classRoute)
	claimed, ok, err := ops.Claim(context.Background(), rigA, queryEnv, "zz-500", "worker-1")
	if err != nil || !ok {
		t.Fatalf("escalated claim = (ok=%v err=%v), want the binding's claim", ok, err)
	}
	if claimed.ID != "zz-500" {
		t.Fatalf("claimed %q, want zz-500", claimed.ID)
	}
	_, reads := rec.snapshot()
	got := make([]string, len(reads))
	for i, d := range reads {
		got[i] = relativeWorkLegDir(cityPath, d)
	}
	// Reader order: the city work store, then rigs by name ascending — each
	// read exactly once, before the escalation.
	if strings.Join(got, ",") != "city,rig-b" {
		t.Fatalf("probe read %v, want [city rig-b] once each in order", got)
	}
}

// TestWorkLegClaimTakesTheFirstHolderInReaderOrder is AC-6: an id held by more
// than one non-primary work leg claims the copy in the FIRST leg in gc ready's
// leg order — the city work store before rigs, rigs by name ascending — which
// is the copy the federated reader serves.
func TestWorkLegClaimTakesTheFirstHolderInReaderOrder(t *testing.T) {
	for _, tt := range []struct {
		name     string
		agent    *config.Agent
		rigOrder []string
		holds    []string
		want     string
	}{
		{
			name:     "the city work store wins over a later rig",
			agent:    &config.Agent{Name: "worker", Dir: "rig-a"},
			rigOrder: []string{"rig-a", "rig-b"},
			holds:    []string{"city", "rig-b"},
			want:     "city",
		},
		{
			name:     "rigs resolve by name ascending, not config order",
			agent:    &config.Agent{Name: "worker", Scope: "city"},
			rigOrder: []string{"rig-c", "rig-b"},
			holds:    []string{"rig-b", "rig-c"},
			want:     "rig-b",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubWorkLegEnv(t)
			cityPath, cfg := workLegTestCity(t, tt.rigOrder...)
			var primary hookStore
			queryEnv := []string{}
			holds := map[string]beads.Bead{}
			for _, leg := range tt.holds {
				dir := cityPath
				if leg != "city" {
					dir = filepath.Join(cityPath, leg)
				}
				holds[dir] = beads.Bead{ID: "zz-6", Status: "open"}
			}
			if tt.agent.Dir != "" {
				primary = hookStore{dir: filepath.Join(cityPath, tt.agent.Dir), env: queryEnv}
				queryEnv = []string{"GC_AGENT=" + tt.agent.Dir + "/worker"}
				cfg.Agents = []config.Agent{*tt.agent}
				primary.env = queryEnv
			} else {
				primary = hookStore{dir: cityPath, env: queryEnv}
				cfg.Agents = []config.Agent{*tt.agent}
				queryEnv = []string{"GC_AGENT=worker"}
				primary.env = queryEnv
			}
			rec := &workLegOpsRecorder{}
			base := rec.ops(cityPath, holds)
			route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, queryEnv, []hookStore{primary}, io.Discard)
			if route == nil {
				t.Fatal("route must exist with other work legs in the frame")
			}
			ops := workLegRoutedHookClaimOps(base, route)
			if _, ok, err := ops.Claim(context.Background(), primary.dir, primary.env, "zz-6", "worker-1"); err != nil || !ok {
				t.Fatalf("claim = (ok=%v err=%v)", ok, err)
			}
			claims, _ := rec.snapshot()
			if len(claims) != 2 {
				t.Fatalf("claims ran in %v, want the primary then the first holder", claims)
			}
			if got := relativeWorkLegDir(cityPath, claims[1]); got != tt.want {
				t.Fatalf("claim landed in %s, want %s", got, tt.want)
			}
		})
	}
}

// TestWorkLegUnreadableLegStopsTheProbeAndSkipsTheCandidate is AC-7: a work
// leg whose read fails mid-probe writes nothing anywhere, names the leg and
// the read error in a sentinel, and the assigned tier goes on to the next
// candidate — the invocation still exits 0 with the work it could claim.
func TestWorkLegUnreadableLegStopsTheProbeAndSkipsTheCandidate(t *testing.T) {
	stubWorkLegEnv(t)
	cityPath, cfg := workLegTestCity(t, "rig-a", "rig-b")
	rigA := filepath.Join(cityPath, "rig-a")
	readErr := errors.New("bd show: database is locked")
	rec := &workLegOpsRecorder{holdErr: map[string]error{cityPath: readErr}}
	// rig-b holds the bead, but the city leg — ahead of it in reader order —
	// is unreadable, so the probe must stop before writing anywhere.
	holds := map[string]beads.Bead{filepath.Join(cityPath, "rig-b"): {ID: "zz-7", Status: "open"}}
	base := rec.ops(cityPath, holds)
	queryEnv := []string{"GC_AGENT=rig-a/worker"}
	cfg.Agents = []config.Agent{{Name: "worker", Dir: "rig-a"}}
	route := hookClaimWorkLegRouteForInvocation(cityPath, cfg, queryEnv, []hookStore{{dir: rigA, env: queryEnv}}, io.Discard)
	ops := workLegRoutedHookClaimOps(base, route)
	_, ok, err := ops.Claim(context.Background(), rigA, queryEnv, "zz-7", "worker-1")
	if ok || err == nil {
		t.Fatalf("unreadable-leg claim = (ok=%v err=%v), want the sentinel", ok, err)
	}
	if !errors.Is(err, errHookClaimWorkLegUnreadable) {
		t.Fatalf("claim err = %v, want errHookClaimWorkLegUnreadable", err)
	}
	for _, want := range []string{"city", "database is locked"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("claim err %q must name %q", err.Error(), want)
		}
	}
	claims, reads := rec.snapshot()
	if len(claims) != 1 || claims[0] != rigA {
		t.Fatalf("claims ran in %v, want only the primary attempt — nothing written elsewhere", claims)
	}
	if len(reads) != 1 || reads[0] != cityPath {
		t.Fatalf("probe read %v, want it to stop at the unreadable city leg", reads)
	}

	// The tier above: the sentinel skips the candidate, the next one is
	// claimed, and the invocation reports work (exit 0), not a failure.
	runner := func(_, _ string, _ []string) (string, error) {
		out, err := json.Marshal([]beads.Bead{
			{ID: "zz-7", Status: "open", Assignee: "worker-1"},
			{ID: "zz-8", Status: "open", Assignee: "worker-1"},
		})
		return string(out), err
	}
	claimable := &workLegOpsRecorder{holdErr: map[string]error{cityPath: readErr}}
	claimableHolds := map[string]beads.Bead{
		rigA: {ID: "zz-8", Status: "open"},
	}
	claimOps := workLegRoutedHookClaimOps(claimable.ops(cityPath, claimableHolds), route)
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("true", rigA, queryEnv, []hookStore{{dir: rigA, env: queryEnv}},
		hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, Env: queryEnv, JSON: true},
		claimOps, runner, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), `"drain"`) {
		t.Fatalf("stdout=%s, want claimed work not a drain", stdout.String())
	}
	if !strings.Contains(stdout.String(), "zz-8") {
		t.Fatalf("stdout=%s, want the second candidate claimed", stdout.String())
	}
	// And only the primary attempt plus the zz-8 claim ran: nothing was
	// written for the candidate whose probe stopped.
	claimLog, _ := claimable.snapshot()
	wantClaims := []string{rigA, rigA}
	if !slices.Equal(claimLog, wantClaims) {
		t.Fatalf("claims ran in %v, want the zz-7 primary attempt and the zz-8 claim only", claimLog)
	}
}

// TestWorkLegRouteIsNotInstalledWhereItCannotRoute is AC-8: a full fan-out, a
// custom work query (both visible as a store set that is not the single
// primary), and a single-store city (a frame with no other leg) install no
// route, and a nil route leaves the ops' function values untouched.
func TestWorkLegRouteIsNotInstalledWhereItCannotRoute(t *testing.T) {
	stubWorkLegEnv(t)
	cityPath, cfg := workLegTestCity(t, "rig-a")
	rigA := filepath.Join(cityPath, "rig-a")
	if got := hookClaimWorkLegRouteForInvocation(cityPath, cfg, nil, []hookStore{{dir: cityPath}, {dir: rigA}}, io.Discard); got != nil {
		t.Fatal("a multi-store fan-out must not install a route")
	}
	// A single-store city: the primary is the city leg, no rigs, so the frame
	// has no other leg and the route is nil.
	if got := hookClaimWorkLegRouteForInvocation(cityPath, &config.City{}, nil, []hookStore{{dir: cityPath}}, io.Discard); got != nil {
		t.Fatal("a single-store city must not install a route")
	}
	// A nil route returns the ops unchanged, so the same function values run.
	assertHookClaimOpsUnwrapped(t, workLegRoutedHookClaimOps(defaultedHookClaimOps(), nil), defaultedHookClaimOps())
}
