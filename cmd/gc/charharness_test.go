package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// charharness is the cmd/gc glue of the three-lane characterization harness. It
// drives a read command's route<X> seam across the remote / local-controller-
// alive / serverless lanes and hands the captured surface to internal/chartest
// for canonicalization + golden comparison. See engdocs/plans/cli-unification/
// HARNESS-DESIGN.md. The driver is command-agnostic: any read command with the
// standard route<X>(cityPath, *api.Client, nilReason, jsonOut, stdout, stderr)
// signature plugs in via charCommand.

const charCityName = "chartest-city"

// charCityBasic is a minimal city.toml (workspace only) named for the harness.
const charCityBasic = "[workspace]\nname = \"" + charCityName + "\"\nprefix = \"gc\"\n"

// charLane is one of the three routing lanes.
type charLane struct {
	name      string
	client    *api.Client   // nil for the serverless lane
	nilReason string        // consulted only when client == nil
	reqs      *atomic.Int64 // server-side request counter; nil for serverless
}

// charCommand plugs a specific read command into the driver.
type charCommand struct {
	name     string // golden filename stem, e.g. "convoy-list"
	route    func(cityPath string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int
	readback func(cityPath string) ([]string, error) // optional post-run state read-back; nil = none
}

type charHarness struct {
	cityPath string
	cs       *controllerState
}

// newCharCity builds a throwaway file-store city from the given city.toml (which
// must name the workspace charCityName) and optional bead seed (run on disk
// before the server exists, so all three lanes read one set).
func newCharCity(t *testing.T, cityToml string, seed func(t *testing.T, store beads.Store)) *charHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "1") // the route=/reason= stderr line is gated on this

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	if seed != nil {
		store, err := openCityStoreAt(cityPath)
		if err != nil {
			t.Fatalf("open seed store: %v", err)
		}
		seed(t, store)
	}

	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), charCityName, cityPath)
	return &charHarness{cityPath: cityPath, cs: cs}
}

// lanes stands up one in-process server (plain + TLS fronts) shared by the two
// client lanes and returns all three lanes. Both fronts wrap the identical mux
// over the same controllerState, so every lane reads one store.
func (h *charHarness) lanes(t *testing.T) []charLane {
	t.Helper()
	base := api.NewSupervisorMux(&singleCityStateResolver{state: h.cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()

	var aliveReqs, tlsReqs atomic.Int64
	aliveSrv := httptest.NewServer(countingHandler(&aliveReqs, base))
	t.Cleanup(aliveSrv.Close)
	tlsSrv := httptest.NewTLSServer(countingHandler(&tlsReqs, base))
	t.Cleanup(tlsSrv.Close)

	caPath := writeCapstoneServerCA(t, tlsSrv)
	remoteClient, err := api.NewRemoteCityScopedClient(tlsSrv.URL, charCityName, api.RemoteOptions{CAFile: caPath})
	if err != nil {
		t.Fatalf("remote client: %v", err)
	}
	return []charLane{
		{name: "remote", client: remoteClient, reqs: &tlsReqs},
		{name: "alive", client: api.NewCityScopedClient(aliveSrv.URL, charCityName), reqs: &aliveReqs},
		{name: "serverless", client: nil, nilReason: "controller-down"},
	}
}

func countingHandler(counter *atomic.Int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		next.ServeHTTP(w, r)
	})
}

// clearBuiltinImportWarningCache resets the process-global sync.Map that dedups
// the "missing required builtin pack" warning to once per cityPath. Each harness
// run models a separate CLI process (a fresh cache), so clearing it before every
// invocation keeps that warning from being emitted only by the first lane —
// which would otherwise make A==B spuriously fail for config-reading commands.
func clearBuiltinImportWarningCache() {
	builtinImportWarningCache.Range(func(k, _ any) bool {
		builtinImportWarningCache.Delete(k)
		return true
	})
}

// run drives one command invocation and returns its exit code and the number of
// API requests it made (0 for the serverless lane).
func (h *charHarness) run(lane charLane, cmd charCommand, jsonOut bool, stdout, stderr *bytes.Buffer) (exit int, reqDelta int64) {
	clearBuiltinImportWarningCache()
	var before int64
	if lane.reqs != nil {
		before = lane.reqs.Load()
	}
	exit = cmd.route(h.cityPath, lane.client, lane.nilReason, jsonOut, stdout, stderr)
	if lane.reqs != nil {
		reqDelta = lane.reqs.Load() - before
	}
	return exit, reqDelta
}

// captureLane drives cmd for one lane in both the human and --json modes,
// capturing EACH run's full surface (exit, stderr, request count), reads state
// back (cmd.readback), records only THIS lane's new events (delta against the
// shared provider), and canonicalizes every surface with one Canonicalizer so
// ids stay identical across stdout/json/readback within the lane.
func (h *charHarness) captureLane(t *testing.T, lane charLane, cmd charCommand) chartest.Capture {
	t.Helper()

	var evSeqBefore uint64
	if fake, ok := h.cs.EventProvider().(*events.Fake); ok {
		evSeqBefore, _ = fake.LatestSeq()
	}

	var ho, he bytes.Buffer
	humanExit, humanReqs := h.run(lane, cmd, false, &ho, &he)

	var jo, je bytes.Buffer
	jsonExit, jsonReqs := h.run(lane, cmd, true, &jo, &je)

	var storeLines []string
	if cmd.readback != nil {
		lines, err := cmd.readback(h.cityPath)
		if err != nil {
			t.Fatalf("readback: %v", err)
		}
		storeLines = lines
	}

	// Every lane's event surface is measured (empty is a fact worth freezing);
	// only events emitted DURING this lane's runs count (delta vs the snapshot).
	var eventLines []string
	if fake, ok := h.cs.EventProvider().(*events.Fake); ok {
		evs, _ := fake.List(events.Filter{})
		for _, e := range evs {
			if e.Seq > evSeqBefore {
				eventLines = append(eventLines, fmt.Sprintf("type=%s subject=%s", e.Type, e.Subject))
			}
		}
		sort.Strings(eventLines)
	}

	// Redact the throwaway city path (a t.TempDir) to a stable token BEFORE
	// canonicalizing — DefaultRules deliberately does not touch temp paths, so a
	// path-emitting command (rig list, status) would otherwise flake per run.
	c := chartest.NewCanonicalizer(chartest.DefaultRules()...)
	redactCanon := func(b []byte) []byte {
		return c.Canonicalize(bytes.ReplaceAll(b, []byte(h.cityPath), []byte("<CITY>")))
	}
	return chartest.Capture{
		Exit:          humanExit,
		Stdout:        redactCanon(ho.Bytes()),
		Stderr:        redactCanon(he.Bytes()),
		JSONExit:      jsonExit,
		JSON:          redactCanon(jo.Bytes()),
		JSONStderr:    redactCanon(je.Bytes()),
		StoreReadback: canonLines(c, storeLines),
		Events:        canonLines(c, eventLines),
		Counts: []chartest.Count{
			{Name: "api_requests_human", N: int(humanReqs)},
			{Name: "api_requests_json", N: int(jsonReqs)},
		},
	}
}

// runCharGolden drives cmd across all three lanes and compares/updates the
// per-lane goldens under testdata/chargolden/<cmd>-<lane>.golden.
func (h *charHarness) runCharGolden(t *testing.T, cmd charCommand) {
	t.Helper()
	for _, lane := range h.lanes(t) {
		t.Run(lane.name, func(t *testing.T) {
			got := h.captureLane(t, lane, cmd).Golden()
			path := filepath.Join("testdata", "chargolden", cmd.name+"-"+lane.name+".golden")
			chartest.CompareGolden(t, path, got)
		})
	}
}

func canonLines(c *chartest.Canonicalizer, lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(c.Canonicalize([]byte(l)))
	}
	return out
}

// convoyReadback lists the convoy beads on disk after a run (reads should not
// mutate them), formatted deterministically for the golden.
func convoyReadback(cityPath string) ([]string, error) {
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	convoys, err := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true, Live: true})
	if err != nil {
		return nil, err
	}
	sort.Slice(convoys, func(i, j int) bool { return convoys[i].ID < convoys[j].ID })
	lines := make([]string, len(convoys))
	for i, b := range convoys {
		lines[i] = fmt.Sprintf("%s type=%s status=%s title=%q", b.ID, b.Type, b.Status, b.Title)
	}
	return lines, nil
}
