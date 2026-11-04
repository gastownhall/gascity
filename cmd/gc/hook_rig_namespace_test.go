package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hrgCity is city B: rig "beta" (prefix "bt") is guarded, rig "alpha"
// (prefix "al") is not, and rigs "gamma" and "delta" are both guarded with
// the prefix "zz", which makes that prefix ambiguous. Each rig has a store
// of its own on disk, as a guarded rig must.
func hrgCity(t *testing.T) *config.City {
	t.Helper()
	root := t.TempDir()
	dir := func(name string) string {
		d := filepath.Join(root, "rigs", name)
		if err := os.MkdirAll(filepath.Join(d, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	beta := rnsGuardedRig("beta")
	beta.Prefix = "bt"
	beta.Path = dir("beta")
	gamma := rnsGuardedRig("gamma")
	gamma.Prefix = "zz"
	gamma.Path = dir("gamma")
	delta := rnsGuardedRig("delta")
	delta.Prefix = "zz"
	delta.Path = dir("delta")
	return &config.City{
		Workspace: config.Workspace{Name: "b", Prefix: "hq"},
		Rigs: []config.Rig{
			beta,
			{Name: "alpha", Prefix: "al", Path: dir("alpha")},
			gamma,
			delta,
		},
	}
}

// hrgDir is the store root of the fixture's rig called name.
func hrgDir(cfg *config.City, name string) string {
	for _, r := range cfg.Rigs {
		if r.Name == name {
			return r.Path
		}
	}
	return ""
}

func hrgGuard(t *testing.T) *hookRigGuard {
	t.Helper()
	res, _ := rnsCityBResolver(t)
	g := newHookRigGuard("/cities/b", hrgCity(t), res)
	if g == nil {
		t.Fatal("newHookRigGuard returned nil for a city with a guarded rig")
	}
	return g
}

// hrgWrites records every claim-cycle write seam, so a test can assert that
// none ran (no bd process would have been spawned).
type hrgWrites struct {
	claims, reclaims, restamps, assigns, closes, stamps, retires []string
}

func (w *hrgWrites) total() int {
	return len(w.claims) + len(w.reclaims) + len(w.restamps) + len(w.assigns) + len(w.closes) + len(w.stamps) + len(w.retires)
}

func hrgOps(t *testing.T, w *hrgWrites, guard *hookRigGuard, rows []beads.Bead) hookClaimOps {
	t.Helper()
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(out), nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			w.claims = append(w.claims, id+"="+assignee)
			for _, r := range rows {
				if r.ID == id {
					r.Assignee = assignee
					r.Status = "in_progress"
					return r, true, nil
				}
			}
			return beads.Bead{}, false, errors.New("unknown bead")
		},
		ReclaimStale: func(_ context.Context, _ string, _ []string, id string) (bool, string, error) {
			w.reclaims = append(w.reclaims, id)
			return true, "someone", nil
		},
		RestampAdopted: func(_ context.Context, _ string, _ []string, id, from, to string) (bool, error) {
			w.restamps = append(w.restamps, id+":"+from+"->"+to)
			return true, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, id, assignee string) error {
			w.assigns = append(w.assigns, id+"="+assignee)
			return nil
		},
		StampWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string, _ map[string]string) error {
			w.stamps = append(w.stamps, id)
			return nil
		},
		WorkflowStore: func(context.Context, string, []string, string, string) beads.Store {
			w.closes = append(w.closes, "workflow-store")
			return beads.NewMemStore()
		},
		// No workflow's input is done unless a test says so; a retirement
		// is recorded as a write.
		InputDone: func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
			return "", false, nil
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string, _ func(beads.Bead) bool) error {
			w.retires = append(w.retires, rootID)
			return nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			for _, r := range rows {
				if r.ID == id {
					return r, nil
				}
			}
			return beads.Bead{}, beads.ErrNotFound
		},
		DrainAck:                 func(io.Writer) error { return nil },
		DrainPending:             func(string) (bool, error) { return false, nil },
		StampSessionClaim:        func(string, string) error { return nil },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EnqueueContinuationNudge: func(string, string, io.Writer) {},
		EmitClaimRejected:        func(string, string, string) {},
		EmitClaimFailed:          func(string, hookClaimOptions, error) {},
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		RecordWorkStarted: func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
			return hookStartedUnrecorded, nil
		},
		ForgetWorkStarted: func(string, hookStartedKey) error { return nil },
		ReadWorkStarted:   func(string) (string, error) { return "", nil },
		StoreRef:          func(string) string { return "" },
		RigGuard:          guard,
	}
}

func TestNewHookRigGuardNilWithoutGuardedRig(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Prefix: "al"}}}
	if g := newHookRigGuard("/cities/b", cfg, nil); g != nil {
		t.Fatal("a city with no guarded rig must get no guard (unchanged behavior)")
	}
	if newHookCity("/cities/b", cfg).rigGuard() != nil {
		t.Fatal("hookCity.rigGuard() for an unguarded city must be nil")
	}
	var nilGuard *hookRigGuard
	rows := []beads.Bead{{ID: "bt-1"}}
	if got := nilGuard.filterCandidates(rows, io.Discard); len(got) != 1 {
		t.Fatalf("nil guard filtered %v", got)
	}
	if nilGuard.refuseClaim(rows[0], io.Discard, "") {
		t.Fatal("nil guard refused a claim")
	}
}

func TestHookRigGuardFiltersForeignCandidates(t *testing.T) {
	g := hrgGuard(t)
	rows := []beads.Bead{
		// Assigned tier: B's own 128-bit pool session is kept.
		{ID: "bt-own", Status: "in_progress", Assignee: poolIDB},
		// A colliding short id (another city's session) is Foreign.
		{ID: "bt-short", Status: "in_progress", Assignee: shortIDB},
		// Another city's pool session is Foreign.
		{ID: "bt-other-pool", Status: "in_progress", Assignee: poolIDA},
		// A bare (city-scoped) route is Foreign; a rig route is Owned.
		{ID: "bt-bare", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.worker")},
		{ID: "bt-routed", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")},
		// Unattributed rows keep today's behavior.
		{ID: "bt-plain", Status: "open"},
		// Rows of an unguarded rig and of the city are never touched.
		{ID: "al-1", Status: "in_progress", Assignee: shortIDB},
		{ID: "hq-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.worker")},
		// An ambiguous guarded prefix admits nothing: even a routed row goes.
		{ID: "zz-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "gamma/crew.worker")},
	}
	var stderr bytes.Buffer
	got := g.filterCandidates(rows, &stderr)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := "bt-own,bt-routed,bt-plain,al-1,hq-1"
	if strings.Join(ids, ",") != want {
		t.Fatalf("kept = %v, want %s", ids, want)
	}
	for _, id := range []string{"bt-short", "bt-other-pool", "bt-bare", "zz-1"} {
		if !strings.Contains(stderr.String(), "skipping "+id+": rig ") {
			t.Errorf("stderr lacks the skip line for %s:\n%s", id, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), `skipping bt-short: rig "beta" serves only beta/ (federation = "rig"); its owner "gcx-17" is outside it`) {
		t.Errorf("stderr lacks the documented message:\n%s", stderr.String())
	}
}

func TestHookClaimDropsForeignAssignedRowAndKeepsOwnPoolRow(t *testing.T) {
	g := hrgGuard(t)
	rows := []beads.Bead{
		{ID: "bt-short", Status: "open", Assignee: shortIDB},
		{ID: "bt-own", Status: "open", Assignee: poolIDB},
	}
	var w hrgWrites
	ops := hrgOps(t, &w, g, rows)
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
		Assignee:           poolIDB,
		SessionID:          poolIDB,
		IdentityCandidates: []string{poolIDB, shortIDB},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Join(w.claims, ",") != "bt-own="+poolIDB {
		t.Fatalf("claims = %v, want only the own pool row", w.claims)
	}
	if !strings.Contains(stdout.String(), `"bead_id":"bt-own"`) {
		t.Fatalf("stdout = %s, want the own row", stdout.String())
	}
}

// Every refused claim-cycle write spawns no bd: the claim, the stale
// reclaim, the promotion, the adoption re-stamp and the workflow-stop close
// all stay unrun when the actor is outside the namespace.
func TestHookClaimRefusedActorSpawnsNoBd(t *testing.T) {
	g := hrgGuard(t)
	cases := []struct {
		name string
		rows []beads.Bead
		opts hookClaimOptions
	}{
		{
			name: "routed tier, city-scoped actor",
			rows: []beads.Bead{{ID: "bt-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}},
			opts: hookClaimOptions{Assignee: "mayor", RouteTargets: []string{"beta/crew.worker"}},
		},
		{
			name: "stale reclaim, short-id actor",
			rows: []beads.Bead{{ID: "bt-1", Status: "in_progress", Assignee: "beta/crew.worker", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}},
			opts: hookClaimOptions{Assignee: shortIDB, RouteTargets: []string{"beta/crew.worker"}, AutoReclaimStaleClaims: true},
		},
		{
			name: "ready assignment promotion, short-id actor",
			rows: []beads.Bead{{ID: "bt-1", Status: "open", Assignee: "beta/crew.lead"}},
			opts: hookClaimOptions{Assignee: shortIDB, IdentityCandidates: []string{"beta/crew.lead"}},
		},
		{
			name: "adoption re-stamp, short-id actor",
			rows: []beads.Bead{{ID: "bt-1", Status: "in_progress", Assignee: "beta/crew.lead"}},
			opts: hookClaimOptions{Assignee: shortIDB, RuntimeActor: shortIDB, IdentityCandidates: []string{"beta/crew.lead"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w hrgWrites
			rows := tc.rows
			for i := range rows {
				rows[i].Metadata = withWorkflowLink(rows[i].Metadata)
			}
			ops := hrgOps(t, &w, g, rows)
			// Every candidate's workflow is done, so an ungated retirement
			// would close it.
			ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
				return "bt-root", true, nil
			}
			var stdout, stderr bytes.Buffer
			tc.opts.JSON = true
			doHookClaim("query", hrgDir(g.cfg, "beta"), tc.opts, ops, &stdout, &stderr)
			if w.total() != 0 {
				t.Fatalf("writes ran despite the refusal: %+v; stderr=%s", w, stderr.String())
			}
			if !strings.Contains(stderr.String(), `gc hook --claim: not claiming bt-1: rig "beta" (federation = "rig") serves only beta/ and this city's pool sessions; claim actor`) {
				t.Fatalf("stderr lacks the refusal:\n%s", stderr.String())
			}
			if strings.Contains(stdout.String(), `"action":"work"`) {
				t.Fatalf("a refused claim was handed out: %s", stdout.String())
			}
		})
	}
}

// withWorkflowLink makes a row workflow-linked, so the stop gate (a write)
// would open the workflow store if the guard let the claim through.
func withWorkflowLink(md map[string]string) map[string]string {
	if md == nil {
		md = map[string]string{}
	}
	md[beadmeta.RootBeadIDMetadataKey] = "bt-root"
	return md
}

func TestHookClaimRefusalHintForShortID(t *testing.T) {
	g := hrgGuard(t)
	err := g.claimRefusal(beads.Bead{ID: "bt-1", Status: "open"}, shortIDB)
	if err == nil || !errors.Is(err, errRigNamespace) {
		t.Fatalf("claimRefusal = %v, want a guard refusal", err)
	}
	if !strings.Contains(err.Error(), `give the agent a rig alias (named session) or run it as a pool of rig "beta"`) {
		t.Fatalf("refusal lacks the short-id hint: %v", err)
	}
	if err := g.claimRefusal(beads.Bead{ID: "bt-1", Status: "open"}, ""); err == nil {
		t.Fatal("an empty actor must be refused on a guarded rig")
	}
	if err := g.claimRefusal(beads.Bead{ID: "bt-1", Status: "open"}, poolIDB); err != nil {
		t.Fatalf("own pool id refused: %v", err)
	}
	if err := g.claimRefusal(beads.Bead{ID: "al-1", Status: "open"}, "mayor"); err != nil {
		t.Fatalf("unguarded rig refused: %v", err)
	}
}

func TestHookClaimAdmittedPoolActorClaims(t *testing.T) {
	g := hrgGuard(t)
	rows := []beads.Bead{{ID: "bt-1", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}}
	var w hrgWrites
	ops := hrgOps(t, &w, g, rows)
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
		Assignee:     poolIDB,
		RouteTargets: []string{"beta/crew.worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)
	if code != 0 || len(w.claims) != 1 {
		t.Fatalf("code=%d claims=%v; stderr=%s", code, w.claims, stderr.String())
	}
}

func TestHookContinuationPinOnGuardedRig(t *testing.T) {
	g := hrgGuard(t)
	sibling := beads.Bead{ID: "bt-2", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")}
	cases := []struct {
		name string
		opts hookClaimOptions
		want string
	}{
		{"pool session pins its 128-bit id", hookClaimOptions{SessionID: poolIDB, Assignee: poolIDB}, poolIDB},
		{"named session pins its alias", hookClaimOptions{SessionID: shortIDB, Assignee: "beta/crew.lead"}, "beta/crew.lead"},
		{"city-scoped agent does not pin", hookClaimOptions{SessionID: shortIDB, Assignee: "mayor"}, ""},
		{"another city's pool id does not pin", hookClaimOptions{SessionID: poolIDA, Assignee: poolIDA}, ""},
	}
	for _, tc := range cases {
		if got := g.continuationPin(sibling, tc.opts); got != tc.want {
			t.Errorf("%s: pin = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A Foreign sibling is never pinned.
	foreign := beads.Bead{ID: "bt-3", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "crew.worker")}
	if got := g.continuationPin(foreign, hookClaimOptions{SessionID: poolIDB, Assignee: poolIDB}); got != "" {
		t.Errorf("foreign sibling pinned to %q", got)
	}
	// Off a guarded rig the pin is unchanged.
	if got := g.continuationPin(beads.Bead{ID: "al-2"}, hookClaimOptions{SessionID: shortIDB, Assignee: "mayor"}); got != shortIDB {
		t.Errorf("unguarded pin = %q, want %q", got, shortIDB)
	}
}

func TestPreassignContinuationSkipsUnadmittedPin(t *testing.T) {
	g := hrgGuard(t)
	bead := beads.Bead{ID: "bt-1", Metadata: rnsMD(beadmeta.RootBeadIDMetadataKey, "bt-root", beadmeta.ContinuationGroupMetadataKey, "grp")}
	siblings := []beads.Bead{
		{ID: "bt-2", Status: "open", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")},
	}
	var w hrgWrites
	ops := hrgOps(t, &w, g, nil)
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return siblings, nil
	}
	ops.WorkflowStore = nil
	assigned, err := preassignHookContinuationGroup(bead, hookClaimOptions{SessionID: shortIDB, Assignee: "mayor", RouteTargets: []string{"beta/crew.worker"}}, ops, hrgDir(g.cfg, "beta"))
	if err != nil || len(assigned) != 0 || len(w.assigns) != 0 {
		t.Fatalf("assigned=%v assigns=%v err=%v, want no pin", assigned, w.assigns, err)
	}
	assigned, err = preassignHookContinuationGroup(bead, hookClaimOptions{SessionID: shortIDB, Assignee: "beta/crew.lead", RouteTargets: []string{"beta/crew.worker"}}, ops, hrgDir(g.cfg, "beta"))
	if err != nil || strings.Join(w.assigns, ",") != "bt-2=beta/crew.lead" {
		t.Fatalf("assigned=%v assigns=%v err=%v, want the alias pin", assigned, w.assigns, err)
	}
}

func TestStripOwnerStamp(t *testing.T) {
	patch := map[string]string{"gc.session_id": "x", beadmeta.OwnerNamespaceMetadataKey: "beta/", " " + beadmeta.OwnerNamespaceMetadataKey + " ": "beta/"}
	stripOwnerStamp(patch)
	if len(patch) != 1 || patch["gc.session_id"] != "x" {
		t.Fatalf("patch = %v, want only gc.session_id", patch)
	}
}

func TestAppendRigHookStoresSkipsGuardedRigs(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "b"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: cityPath + "/alpha", Prefix: "al"},
			{Name: "beta", Path: cityPath + "/beta", Prefix: "bt", Federation: config.RigFederationRig},
		},
	}
	a := &config.Agent{Name: "mayor", Scope: "city"}
	stores := appendRigHookStores(nil, cityPath, cfg, a, nil, nil)
	for _, s := range stores {
		if s.rig == "beta" {
			t.Fatalf("a guarded rig got a fan-out leg: %+v", stores)
		}
	}
}

// The retirement of a done workflow closes rows like any other claim-cycle
// write: only for an actor the guarded rig admits, and never another city's
// rows that carry the workflow's root id.
func TestHookRigGuardRetiresOnlyForAnAdmittedActor(t *testing.T) {
	g := hrgGuard(t)
	row := beads.Bead{ID: "bt-1", Status: "open", Metadata: withWorkflowLink(rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker"))}
	done := func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
		return "bt-root", true, nil
	}
	for _, tc := range []struct {
		actor      string
		wantRetire bool
	}{
		{"beta/crew.worker", true},
		{"mayor", false},
		{shortIDB, false},
	} {
		var w hrgWrites
		ops := hrgOps(t, &w, g, []beads.Bead{row})
		ops.InputDone = done
		var stdout, stderr bytes.Buffer
		doHookClaim("query", hrgDir(g.cfg, "beta"), hookClaimOptions{
			Assignee: tc.actor, RouteTargets: []string{"beta/crew.worker"}, JSON: true,
		}, ops, &stdout, &stderr)
		if got := len(w.retires) == 1; got != tc.wantRetire {
			t.Errorf("actor %q: retired %v, want %v (writes %+v; stderr=%s)", tc.actor, w.retires, tc.wantRetire, w, stderr.String())
		}
		if !tc.wantRetire && w.total() != 0 {
			t.Errorf("actor %q: writes ran despite the refusal: %+v", tc.actor, w)
		}
	}
}

func TestSkipDoneWorkflowLeavesForeignRowsOpen(t *testing.T) {
	keep := hrgGuard(t).forLeg("beta", "").retireFilter()
	seed := func(t *testing.T, rootMD map[string]string) (*beads.MemStore, beads.Bead, map[string]beads.Bead) {
		t.Helper()
		store := beads.NewMemStore()
		root, err := store.Create(beads.Bead{Title: "mol", Metadata: rootMD})
		if err != nil {
			t.Fatal(err)
		}
		steps := map[string]beads.Bead{}
		for name, b := range map[string]beads.Bead{
			"own":     {Title: "own", Metadata: rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker")},
			"foreign": {Title: "foreign", Status: "in_progress", Assignee: "alpha/crew.worker"},
			"plain":   {Title: "plain"},
		} {
			if b.Metadata == nil {
				b.Metadata = map[string]string{}
			}
			b.Metadata[beadmeta.RootBeadIDMetadataKey] = root.ID
			created, err := store.Create(b)
			if err != nil {
				t.Fatal(err)
			}
			steps[name] = created
		}
		return store, root, steps
	}
	status := func(t *testing.T, store *beads.MemStore, id string) string {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return b.Status
	}

	store, root, steps := seed(t, rnsMD(beadmeta.RoutedToMetadataKey, "beta/crew.worker"))
	if err := skipDoneWorkflow(store, root.ID, keep); err != nil {
		t.Fatalf("skipDoneWorkflow: %v", err)
	}
	for _, id := range []string{root.ID, steps["own"].ID, steps["plain"].ID} {
		if got := status(t, store, id); got != "closed" {
			t.Errorf("%s status = %q, want closed", id, got)
		}
	}
	if got := status(t, store, steps["foreign"].ID); got == "closed" {
		t.Error("another city's step was closed by the retirement")
	}

	// Another city's root stays open; this city's own steps under it close.
	store, root, steps = seed(t, rnsMD(beadmeta.RoutedToMetadataKey, "alpha/crew.worker"))
	if err := skipDoneWorkflow(store, root.ID, keep); err != nil {
		t.Fatalf("skipDoneWorkflow: %v", err)
	}
	if got := status(t, store, root.ID); got == "closed" {
		t.Error("another city's root was closed by the retirement")
	}
	if got := status(t, store, steps["own"].ID); got != "closed" {
		t.Errorf("own step status = %q, want closed", got)
	}
}
