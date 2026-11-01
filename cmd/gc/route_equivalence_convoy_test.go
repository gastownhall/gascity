package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Route equivalence for gc convoy list and gc convoy status (#8453): the
// supervisor API route must print the local route's schema with the local
// route's content. Each test seeds one city and reads it on the serverless
// lane (the local reference) and on the two API lanes (a local-target client
// and a remote TLS client) of the three-lane harness, then demands identical
// exit codes and byte-identical stdout. There are no declared additive keys:
// the only API-only output, the stale-cache banner, does not fire on a fresh
// in-process server.

// convoyRouteRun is one captured invocation.
type convoyRouteRun struct {
	exit           int
	stdout, stderr string
}

// runConvoyRoute drives one route invocation on lane.
func runConvoyRoute(lane charLane, route func(c *api.Client, nilReason string, stdout, stderr io.Writer) int) convoyRouteRun {
	var so, se bytes.Buffer
	clearBuiltinImportWarningCache()
	code := route(lane.client, lane.nilReason, &so, &se)
	return convoyRouteRun{exit: code, stdout: so.String(), stderr: se.String()}
}

// seedConvoyRouteCity seeds the gaps the API route used to drop: a convoy
// with a closed legacy child, an open tracked child and a dangling track; an
// empty convoy; bulk convoys past the 100-row page size; one synthetic
// convoy. It returns the IDs of the first two convoys.
func seedConvoyRouteCity(bulk int) (seed func(t *testing.T, store beads.Store), alphaID, betaID *string) {
	alphaID, betaID = new(string), new(string)
	seed = func(t *testing.T, store beads.Store) {
		t.Helper()
		alpha, err := store.Create(beads.Bead{
			Title:    "Alpha convoy",
			Type:     "convoy",
			Labels:   []string{"owned"},
			Metadata: map[string]string{"target": "main", "convoy.owner": "owner-a"},
		})
		if err != nil {
			t.Fatalf("seed alpha: %v", err)
		}
		*alphaID = alpha.ID
		legacy, err := store.Create(beads.Bead{Title: "legacy child", ParentID: alpha.ID})
		if err != nil {
			t.Fatalf("seed legacy child: %v", err)
		}
		if err := store.Close(legacy.ID); err != nil {
			t.Fatalf("close legacy child: %v", err)
		}
		tracked, err := store.Create(beads.Bead{Title: "tracked child", Assignee: "assignee-a"})
		if err != nil {
			t.Fatalf("seed tracked child: %v", err)
		}
		if err := store.DepAdd(alpha.ID, tracked.ID, "tracks"); err != nil {
			t.Fatalf("track child: %v", err)
		}
		if err := store.DepAdd(alpha.ID, "gc-missing", "tracks"); err != nil {
			t.Fatalf("dangling track: %v", err)
		}
		beta, err := store.Create(beads.Bead{Title: "Beta convoy", Type: "convoy"})
		if err != nil {
			t.Fatalf("seed beta: %v", err)
		}
		*betaID = beta.ID
		for i := 0; i < bulk; i++ {
			if _, err := store.Create(beads.Bead{Title: fmt.Sprintf("bulk %03d", i), Type: "convoy"}); err != nil {
				t.Fatalf("seed bulk convoy %d: %v", i, err)
			}
		}
		if _, err := store.Create(beads.Bead{Title: "launch input", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true"}}); err != nil {
			t.Fatalf("seed synthetic convoy: %v", err)
		}
	}
	return seed, alphaID, betaID
}

// splitConvoyLanes returns the serverless reference lane and the API lanes.
func splitConvoyLanes(t *testing.T, h *charHarness) (local charLane, apiLanes []charLane) {
	t.Helper()
	for _, lane := range h.lanes(t) {
		if lane.client == nil {
			local = lane
			continue
		}
		apiLanes = append(apiLanes, lane)
	}
	return local, apiLanes
}

// requireConvoyRouteEquivalent asserts got (an API lane) prints exactly what
// want (the local lane) printed.
func requireConvoyRouteEquivalent(t *testing.T, label string, want, got convoyRouteRun, jsonOut bool) {
	t.Helper()
	if got.exit != want.exit {
		t.Errorf("%s: exit = %d, local exit = %d (stderr %q)", label, got.exit, want.exit, got.stderr)
	}
	if !strings.Contains(got.stderr, "route=api") {
		t.Errorf("%s: stderr = %q, want route=api", label, got.stderr)
	}
	if jsonOut {
		if d := chartest.JSONShapeDiff([]byte(want.stdout), []byte(got.stdout), nil); d != "" {
			t.Errorf("%s: JSON differs from the local route: %s", label, d)
		}
	}
	if got.stdout != want.stdout {
		t.Errorf("%s: stdout differs from the local route\n--- local ---\n%s\n--- api ---\n%s", label, firstBytes(want.stdout, 2000), firstBytes(got.stdout, 2000))
	}
}

func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestConvoyListRouteEquivalence pins that gc convoy list prints the same rows
// in the same order, with child_ids, dangling-track counts and the full total,
// on the local route and on both API lanes.
func TestConvoyListRouteEquivalence(t *testing.T) {
	const bulk = 105
	seed, _, _ := seedConvoyRouteCity(bulk)
	h := newCharCity(t, charCityBasic, seed)
	local, apiLanes := splitConvoyLanes(t, h)

	for _, jsonOut := range []bool{true, false} {
		for _, includeSynthetic := range []bool{false, true} {
			opts := convoyListOptions{jsonOut: jsonOut, includeSynthetic: includeSynthetic}
			route := func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeConvoyList(h.cityPath, c, nilReason, opts, stdout, stderr)
			}
			want := runConvoyRoute(local, route)
			if want.exit != 0 {
				t.Fatalf("json=%v synthetic=%v: local exit %d, stderr %q", jsonOut, includeSynthetic, want.exit, want.stderr)
			}
			if !strings.Contains(want.stderr, "route=fallback reason=controller-down") {
				t.Errorf("local stderr = %q, want route=fallback reason=controller-down", want.stderr)
			}
			if jsonOut {
				requireConvoyListJSON(t, want.stdout, includeSynthetic, bulk)
			} else if !strings.Contains(want.stdout, "1/3 closed (1 dangling track)") {
				t.Errorf("local text = %q, want Alpha's dangling-track suffix", firstBytes(want.stdout, 400))
			}
			for _, lane := range apiLanes {
				label := fmt.Sprintf("json=%v synthetic=%v lane=%s", jsonOut, includeSynthetic, lane.name)
				got := runConvoyRoute(lane, route)
				requireConvoyRouteEquivalent(t, label, want, got, jsonOut)
			}
		}
	}
}

// requireConvoyListJSON checks the local reference document itself: schema
// valid, every convoy counted, rows in ID order, Alpha's members reported.
func requireConvoyListJSON(t *testing.T, out string, includeSynthetic bool, bulk int) {
	t.Helper()
	validateJSONAgainstResultSchema(t, []string{"convoy", "list"}, []byte(out))
	var doc struct {
		Convoys []struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			ChildIDs []string `json:"child_ids"`
			Progress struct {
				DanglingTracks int `json:"dangling_tracks"`
			} `json:"progress"`
		} `json:"convoys"`
		Summary struct {
			Total           int `json:"total"`
			HiddenSynthetic int `json:"hidden_synthetic"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode list JSON: %v", err)
	}
	wantTotal, wantHidden := bulk+2, 1
	if includeSynthetic {
		wantTotal, wantHidden = bulk+3, 0
	}
	if doc.Summary.Total != wantTotal || len(doc.Convoys) != wantTotal {
		t.Errorf("summary.total = %d, rows = %d, want %d", doc.Summary.Total, len(doc.Convoys), wantTotal)
	}
	if doc.Summary.HiddenSynthetic != wantHidden {
		t.Errorf("summary.hidden_synthetic = %d, want %d", doc.Summary.HiddenSynthetic, wantHidden)
	}
	ids := make([]string, 0, len(doc.Convoys))
	for _, c := range doc.Convoys {
		ids = append(ids, c.ID)
		if c.Title == "Alpha convoy" {
			if len(c.ChildIDs) != 3 || !slices.Contains(c.ChildIDs, "gc-missing") || c.Progress.DanglingTracks != 1 {
				t.Errorf("Alpha row child_ids = %v, dangling_tracks = %d; want 3 ids incl. gc-missing and 1", c.ChildIDs, c.Progress.DanglingTracks)
			}
		}
	}
	if !slices.IsSorted(ids) {
		t.Errorf("convoy IDs are not in ascending order: %v", firstBytes(strings.Join(ids, ","), 200))
	}
}

// TestConvoyStatusRouteEquivalence pins that gc convoy status prints the same
// detail, including progress.dangling_tracks and the human Progress suffix, on
// the local route and on both API lanes.
func TestConvoyStatusRouteEquivalence(t *testing.T) {
	seed, alphaID, betaID := seedConvoyRouteCity(0)
	h := newCharCity(t, charCityBasic, seed)
	local, apiLanes := splitConvoyLanes(t, h)

	for _, id := range []string{*alphaID, *betaID} {
		for _, jsonOut := range []bool{true, false} {
			route := func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeConvoyStatus(h.cityPath, id, c, nilReason, jsonOut, stdout, stderr)
			}
			want := runConvoyRoute(local, route)
			if want.exit != 0 {
				t.Fatalf("%s json=%v: local exit %d, stderr %q", id, jsonOut, want.exit, want.stderr)
			}
			if jsonOut {
				validateJSONAgainstResultSchema(t, []string{"convoy", "status"}, []byte(want.stdout))
				if id == *alphaID {
					requireAlphaStatusJSON(t, want.stdout)
				}
			} else if id == *alphaID && !strings.Contains(want.stdout, "Progress: 1/3 closed (1 dangling track)") {
				t.Errorf("local status text = %q, want the dangling-track suffix", want.stdout)
			}
			for _, lane := range apiLanes {
				label := fmt.Sprintf("%s json=%v lane=%s", id, jsonOut, lane.name)
				got := runConvoyRoute(lane, route)
				requireConvoyRouteEquivalent(t, label, want, got, jsonOut)
			}
		}
	}
}

func requireAlphaStatusJSON(t *testing.T, out string) {
	t.Helper()
	var doc struct {
		Progress struct {
			DanglingTracks int `json:"dangling_tracks"`
		} `json:"progress"`
		Children []struct {
			ID            string `json:"id"`
			DanglingTrack bool   `json:"dangling_track"`
		} `json:"children"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	if doc.Progress.DanglingTracks != 1 {
		t.Errorf("progress.dangling_tracks = %d, want 1", doc.Progress.DanglingTracks)
	}
	found := false
	for _, c := range doc.Children {
		if c.ID == "gc-missing" {
			found = c.DanglingTrack
		}
	}
	if !found {
		t.Errorf("children = %+v, want gc-missing with dangling_track:true", doc.Children)
	}
}

// supervisorHandler serves state through the production supervisor mux.
func supervisorHandler(state api.State) http.Handler {
	return api.NewSupervisorMux(&singleCityStateResolver{state: state}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
}

// convoyAPILanes serves handler on a plain and a TLS server and returns a
// local-target client and a remote client for them.
func convoyAPILanes(t *testing.T, handler http.Handler) (local, remote *api.Client) {
	t.Helper()
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(handler)
	t.Cleanup(tlsSrv.Close)
	remote, err := api.NewRemoteCityScopedClient(tlsSrv.URL, charCityName, api.RemoteOptions{CAFile: writeCapstoneServerCA(t, tlsSrv)})
	if err != nil {
		t.Fatalf("remote client: %v", err)
	}
	return api.NewCityScopedClient(plain.URL, charCityName), remote
}

// failingConvoyRigState serves the controller's stores plus one rig store
// whose convoy list fails, as a rig the supervisor cannot read would.
type failingConvoyRigState struct {
	*controllerState
	rig string
}

// BeadStores adds the failing rig store to the controller's stores.
func (s failingConvoyRigState) BeadStores() map[string]beads.Store {
	m := s.controllerState.BeadStores()
	m[s.rig] = convoyCheckFailStore{Store: beads.NewMemStore(), failConvoyList: true}
	return m
}

// TestConvoyListRouteEquivalenceFailsOnAFailedRig pins that a store whose
// convoy list fails fails gc convoy list (exit 1, nothing on stdout) on every
// route, instead of the API route printing the healthy stores' convoys as the
// whole list.
func TestConvoyListRouteEquivalenceFailsOnAFailedRig(t *testing.T) {
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		if _, err := store.Create(beads.Bead{Title: "healthy", Type: "convoy"}); err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
	})
	const rig = "rig-broken"
	localClient, remoteClient := convoyAPILanes(t, supervisorHandler(failingConvoyRigState{controllerState: h.cs, rig: rig}))

	cityStore, err := openCityStoreAt(h.cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	views := []convoyStoreView{
		{path: h.cityPath, store: cityStore},
		{path: "/rigs/" + rig, store: convoyCheckFailStore{Store: beads.NewMemStore(), failConvoyList: true}},
	}

	for _, jsonOut := range []bool{true, false} {
		opts := convoyListOptions{jsonOut: jsonOut}
		var lo, le bytes.Buffer
		lcode := doConvoyListAcrossStores(views, opts, &lo, &le)
		requireFailedConvoyList(t, fmt.Sprintf("json=%v local", jsonOut), convoyRouteRun{lcode, lo.String(), le.String()})

		for name, c := range map[string]*api.Client{"alive": localClient, "remote": remoteClient} {
			label := fmt.Sprintf("json=%v lane=%s", jsonOut, name)
			got := runConvoyRoute(charLane{name: name, client: c}, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeConvoyList(h.cityPath, c, nilReason, opts, stdout, stderr)
			})
			requireFailedConvoyList(t, label, got)
			for _, want := range []string{"partial result", rig, "route=api reason=error"} {
				if !strings.Contains(got.stderr, want) {
					t.Errorf("%s: stderr = %q, want it to contain %q", label, got.stderr, want)
				}
			}
		}
	}
}

func requireFailedConvoyList(t *testing.T, label string, run convoyRouteRun) {
	t.Helper()
	if run.exit != 1 {
		t.Errorf("%s: exit = %d, want 1 (stdout %q)", label, run.exit, firstBytes(run.stdout, 300))
	}
	if run.stdout != "" {
		t.Errorf("%s: stdout = %q, want empty", label, firstBytes(run.stdout, 300))
	}
	for _, want := range []string{"convoy list unavailable", "rig-broken"} {
		if !strings.Contains(run.stderr, want) {
			t.Errorf("%s: stderr = %q, want the failing store named (%q)", label, run.stderr, want)
		}
	}
}

// newConvoyRigCity builds a file-backed city with rigs alpha and omega, one
// convoy in each of the three stores, and then overwrites omega's ledger with
// corrupt JSON so neither the supervisor nor the CLI can open it. It returns
// the harness and omega's path.
func newConvoyRigCity(t *testing.T) (*charHarness, string) {
	t.Helper()
	clearGCEnv(t)
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "1")
	t.Setenv("GC_NO_API", "")

	cityPath := t.TempDir()
	alphaPath := filepath.Join(t.TempDir(), "alpha")
	omegaPath := filepath.Join(t.TempDir(), "omega")
	if err := bootstrapScopedFileProviderCityFS(fsys.OSFS{}, cityPath); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{alphaPath, omegaPath} {
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gc", "beads.json"), []byte("{\"seq\":0,\"beads\":[]}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[[rigs]]\nname = \"alpha\"\nprefix = \"al\"\n\n[[rigs]]\nname = \"omega\"\nprefix = \"om\"\n"
	siteToml := fmt.Sprintf("workspace_name = %q\nworkspace_prefix = \"hq\"\n\n[[rig]]\nname = \"alpha\"\npath = %q\n\n[[rig]]\nname = \"omega\"\npath = %q\n",
		charCityName, alphaPath, omegaPath)
	writeSchema2RigCity(t, cityPath, charCityName, cityToml, siteToml)

	for _, scope := range []string{cityPath, alphaPath, omegaPath} {
		store, err := openScopeLocalFileStoreForCity(scope, cityPath)
		if err != nil {
			t.Fatalf("open seed store %s: %v", scope, err)
		}
		if _, err := store.Create(beads.Bead{Title: "convoy in " + filepath.Base(scope), Type: "convoy"}); err != nil {
			t.Fatalf("seed convoy in %s: %v", scope, err)
		}
	}
	if err := os.WriteFile(filepath.Join(omegaPath, ".gc", "beads.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), charCityName, cityPath)
	return &charHarness{cityPath: cityPath, cs: cs}, omegaPath
}

// TestConvoyListRouteEquivalenceWhenARigStoreCannotOpen pins that a rig store
// neither the supervisor nor the CLI can open fails gc convoy list on every
// lane: the supervisor reports it as a partial answer the API route refuses,
// and the direct read refuses it too, naming the store, instead of printing
// the other stores' convoys as the whole list.
func TestConvoyListRouteEquivalenceWhenARigStoreCannotOpen(t *testing.T) {
	h, omegaPath := newConvoyRigCity(t)
	for _, jsonOut := range []bool{true, false} {
		opts := convoyListOptions{jsonOut: jsonOut}
		for _, lane := range h.lanes(t) {
			label := fmt.Sprintf("json=%v lane=%s", jsonOut, lane.name)
			got := runConvoyRoute(lane, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
				return routeConvoyList(h.cityPath, c, nilReason, opts, stdout, stderr)
			})
			if got.exit != 1 {
				t.Errorf("%s: exit = %d, want 1 (stdout %q, stderr %q)", label, got.exit, firstBytes(got.stdout, 300), got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("%s: stdout = %q, want empty", label, firstBytes(got.stdout, 300))
			}
			wantNamed := "rig omega"
			if lane.client == nil {
				wantNamed = omegaPath
			}
			if !strings.Contains(got.stderr, wantNamed) {
				t.Errorf("%s: stderr = %q, want the unopenable store named (%q)", label, got.stderr, wantNamed)
			}
		}
	}
}

// oldConvoyCheckServer wraps base as a supervisor that predates child_ids:
// /convoy/{id}/check answers carry neither child_ids nor dangling_tracks.
func oldConvoyCheckServer(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/check") {
			base.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		base.ServeHTTP(rec, r)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		delete(body, "child_ids")
		delete(body, "dangling_tracks")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// TestConvoyListAPIRouteOnAServerWithoutChildIDs pins the CLI's handling of a
// supervisor that predates child_ids: a local target reads the stores
// directly, so its output still matches the local route; a remote target,
// which has no local route, prints the old shape and says so on stderr.
func TestConvoyListAPIRouteOnAServerWithoutChildIDs(t *testing.T) {
	h := newCharCity(t, charCityBasic, func(t *testing.T, store beads.Store) {
		cv, err := store.Create(beads.Bead{Title: "alpha", Type: "convoy"})
		if err != nil {
			t.Fatalf("seed convoy: %v", err)
		}
		if _, err := store.Create(beads.Bead{Title: "child", ParentID: cv.ID}); err != nil {
			t.Fatalf("seed child: %v", err)
		}
	})
	localClient, remoteClient := convoyAPILanes(t, oldConvoyCheckServer(supervisorHandler(h.cs)))
	opts := convoyListOptions{jsonOut: true}
	route := func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
		return routeConvoyList(h.cityPath, c, nilReason, opts, stdout, stderr)
	}

	want := runConvoyRoute(charLane{name: "serverless", nilReason: "controller-down"}, route)
	if want.exit != 0 || !strings.Contains(want.stdout, `"child_ids"`) {
		t.Fatalf("local route: exit %d stdout %q, want exit 0 with child_ids", want.exit, want.stdout)
	}

	got := runConvoyRoute(charLane{name: "alive", client: localClient}, route)
	if got.exit != 0 {
		t.Errorf("old server, local target: exit = %d, stderr %q", got.exit, got.stderr)
	}
	if !strings.Contains(got.stderr, "route=fallback reason=convoy-check-predates-child-ids") {
		t.Errorf("old server, local target: stderr = %q, want the predates-child-ids fallback", got.stderr)
	}
	if got.stdout != want.stdout {
		t.Errorf("old server, local target: stdout = %q, want the local route's %q", got.stdout, want.stdout)
	}

	got = runConvoyRoute(charLane{name: "remote", client: remoteClient}, route)
	if got.exit != 0 {
		t.Errorf("old server, remote target: exit = %d, stderr %q", got.exit, got.stderr)
	}
	if !strings.Contains(got.stderr, "route=api") || !strings.Contains(got.stderr, "predates child_ids") {
		t.Errorf("old server, remote target: stderr = %q, want route=api and a predates-child_ids notice", got.stderr)
	}
	if strings.Contains(got.stdout, `"child_ids"`) {
		t.Errorf("old server, remote target: stdout = %q, want no child_ids", got.stdout)
	}
	validateJSONAgainstResultSchema(t, []string{"convoy", "list"}, []byte(got.stdout))
}
