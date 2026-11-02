package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// These tests pin #8453 for `gc beads list` and `gc beads show`: a local city
// prints one JSON shape and one set of bead fields whichever route answers (the
// supervisor API or the direct store read), and only a remote target gets the
// {beads|bead, _cache_age_s} envelope. Every lane reads one seeded city: the
// API lanes through an in-process SupervisorMux over a controllerState built on
// the same on-disk stores the serverless lane opens.

// beadsRouteFault selects how the fixture breaks the omega rig's store.
type beadsRouteFault int

const (
	// beadsRouteHealthy leaves every store readable.
	beadsRouteHealthy beadsRouteFault = iota
	// beadsRouteListFails makes omega's store fail every read on both planes:
	// the controller's open of it fails (so the server installs its production
	// unavailableStore) and the CLI's open is wrapped to fail List and Get.
	beadsRouteListFails
	// beadsRouteOpenFails corrupts omega's ledger on disk, so neither plane can
	// open it. No seams.
	beadsRouteOpenFails
)

// beadsRouteCity is the seeded fixture: a city store and two rig stores.
type beadsRouteCity struct {
	h         *charHarness
	cityPath  string
	omegaPath string
	lanes     []charLane

	plainID, blockedID, noHistoryID, closedID, alphaID, omegaID string
}

// beadsRouteBrokenStore is a store whose reads fail, standing in for a rig
// ledger the CLI opened but cannot read.
type beadsRouteBrokenStore struct {
	beads.Store
	err error
}

func (s beadsRouteBrokenStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }

func (s beadsRouteBrokenStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// beadsRouteReadinessStore models the server's CachingStore readiness
// projection: over the default bd and native-Dolt backends, a primed cache
// stamps is_blocked (true or false) on every open row, while a direct store
// read never carries it. Wrapping only the controller's stores makes that
// cache-only value visible to the equivalence checks.
type beadsRouteReadinessStore struct {
	beads.Store
}

func (s beadsRouteReadinessStore) project(b beads.Bead) beads.Bead {
	if b.Status == "closed" {
		return b
	}
	blocked := len(b.Needs) > 0
	b.IsBlocked = &blocked
	return b
}

func (s beadsRouteReadinessStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	for i := range rows {
		rows[i] = s.project(rows[i])
	}
	return rows, err
}

func (s beadsRouteReadinessStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err != nil {
		return b, err
	}
	return s.project(b), nil
}

func newBeadsRouteCity(t *testing.T, fault beadsRouteFault) *beadsRouteCity {
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

	c := &beadsRouteCity{cityPath: cityPath, omegaPath: omegaPath}
	create := func(scope string, b beads.Bead) string {
		t.Helper()
		store, err := openScopeLocalFileStoreForCity(scope, cityPath)
		if err != nil {
			t.Fatalf("open seed store %s: %v", scope, err)
		}
		got, err := store.Create(b)
		if err != nil {
			t.Fatalf("seed %s: %v", scope, err)
		}
		return got.ID
	}
	priority := 1
	c.plainID = create(cityPath, beads.Bead{
		Title: "plain city bead", Priority: &priority, Labels: []string{"route-eq"},
		Metadata: beads.StringMap{"k": "v"}, Assignee: "worker-1", Description: "described",
	})
	blockerID := create(cityPath, beads.Bead{Title: "blocker"})
	c.blockedID = create(cityPath, beads.Bead{Title: "blocked bead", Needs: []string{blockerID}})
	c.noHistoryID = create(cityPath, beads.Bead{Title: "no-history bead", NoHistory: true})
	c.closedID = create(cityPath, beads.Bead{Title: "closed bead"})
	cityStore, err := openScopeLocalFileStoreForCity(cityPath, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cityStore.Close(c.closedID); err != nil {
		t.Fatalf("close seed bead: %v", err)
	}
	c.alphaID = create(alphaPath, beads.Bead{Title: "alpha bead", Labels: []string{"route-eq"}})
	c.omegaID = create(omegaPath, beads.Bead{Title: "omega bead"})

	switch fault {
	case beadsRouteOpenFails:
		if err := os.WriteFile(filepath.Join(omegaPath, ".gc", "beads.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	case beadsRouteListFails:
		readErr := errors.New("omega ledger unreadable")
		prevOpen := controllerStateOpenRigStoreAtForCity
		controllerStateOpenRigStoreAtForCity = func(ctx context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
			if samePath(opts.ScopeRoot, omegaPath) {
				return beads.StoreOpenResult{}, readErr
			}
			return prevOpen(ctx, opts)
		}
		prevObserve := observeOpenedStore
		observeOpenedStore = func(scopeRoot string, store beads.Store) beads.Store {
			if prevObserve != nil {
				store = prevObserve(scopeRoot, store)
			}
			if samePath(scopeRoot, omegaPath) {
				return beadsRouteBrokenStore{Store: store, err: readErr}
			}
			return store
		}
		t.Cleanup(func() {
			controllerStateOpenRigStoreAtForCity = prevOpen
			observeOpenedStore = prevObserve
		})
	}

	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), charCityName, cityPath)
	cs.mu.Lock()
	cs.cityBeadStore = beadsRouteReadinessStore{Store: cs.cityBeadStore}
	for name, store := range cs.beadStores {
		cs.beadStores[name] = beadsRouteReadinessStore{Store: store}
	}
	cs.mu.Unlock()
	c.h = &charHarness{cityPath: cityPath, cs: cs}
	c.lanes = c.h.lanes(t)
	return c
}

// lane returns the named lane.
func (c *beadsRouteCity) lane(t *testing.T, name string) charLane {
	t.Helper()
	for _, l := range c.lanes {
		if l.name == name {
			return l
		}
	}
	t.Fatalf("no lane %q", name)
	return charLane{}
}

// beadsRouteResult is one command run's surface.
type beadsRouteResult struct {
	exit           int
	stdout, stderr string
}

// laneCityPath is the cityPath production hands a lane's route: a remote
// target carries none.
func (c *beadsRouteCity) laneCityPath(l charLane) string {
	if l.name == "remote" {
		return ""
	}
	return c.cityPath
}

func (c *beadsRouteCity) list(l charLane, format string, filters beadFilters) beadsRouteResult {
	clearBuiltinImportWarningCache()
	var stdout, stderr bytes.Buffer
	exit := routeBeadsList(c.laneCityPath(l), l.client, l.nilReason, format, filters, &stdout, &stderr)
	return beadsRouteResult{exit, stdout.String(), stderr.String()}
}

func (c *beadsRouteCity) show(l charLane, id, format string) beadsRouteResult {
	clearBuiltinImportWarningCache()
	var stdout, stderr bytes.Buffer
	exit := routeBeadsShow(c.laneCityPath(l), l.client, l.nilReason, id, format, &stdout, &stderr)
	return beadsRouteResult{exit, stdout.String(), stderr.String()}
}

// beadsRouteRequireAPI fails unless the run was answered by the API: without it
// a lane that silently fell back would compare the local read with itself.
func beadsRouteRequireAPI(t *testing.T, cmd string, r beadsRouteResult) {
	t.Helper()
	if !strings.Contains(r.stderr, "cmd="+cmd+" route=api") {
		t.Fatalf("API lane did not take the API route for %s; stderr:\n%s", cmd, r.stderr)
	}
}

// beadsRouteRequireSame asserts the local and API-served output are identical.
func beadsRouteRequireSame(t *testing.T, isJSON bool, local, served beadsRouteResult) {
	t.Helper()
	if local.exit != 0 || served.exit != 0 {
		t.Fatalf("exit local=%d api=%d, want 0; stderr local=%q api=%q", local.exit, served.exit, local.stderr, served.stderr)
	}
	if isJSON {
		if d := chartest.JSONShapeDiff([]byte(local.stdout), []byte(served.stdout), nil); d != "" {
			t.Fatalf("API JSON differs from local JSON: %s\nlocal:\n%s\napi:\n%s", d, local.stdout, served.stdout)
		}
	}
	if local.stdout != served.stdout {
		t.Fatalf("API output differs from local output:\nlocal:\n%s\napi:\n%s", local.stdout, served.stdout)
	}
}

// beadsRouteUnwrapRemote checks a remote JSON run printed exactly the
// {<key>, _cache_age_s} envelope and returns the inner document.
func beadsRouteUnwrapRemote(t *testing.T, cmd, key string, r beadsRouteResult) []byte {
	t.Helper()
	if r.exit != 0 {
		t.Fatalf("remote exit = %d, stderr=%q", r.exit, r.stderr)
	}
	beadsRouteRequireAPI(t, cmd, r)
	var env map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("remote JSON is not an object: %v\n%s", err, r.stdout)
	}
	if len(env) != 2 || env[key] == nil || env["_cache_age_s"] == nil {
		t.Fatalf("remote JSON keys = %v, want exactly {%s, _cache_age_s}:\n%s", beadsRouteKeys(env), key, r.stdout)
	}
	var age float64
	if err := json.Unmarshal(env["_cache_age_s"], &age); err != nil {
		t.Fatalf("_cache_age_s is not numeric: %s", env["_cache_age_s"])
	}
	return env[key]
}

func beadsRouteKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestBeadsListRouteEquivalence runs gc beads list on the local route, the API
// route for the same local city, and a remote target, against one seeded city.
func TestBeadsListRouteEquivalence(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteHealthy)
	local, alive, remote := c.lane(t, "serverless"), c.lane(t, "alive"), c.lane(t, "remote")

	rows := []struct {
		name    string
		format  string
		filters beadFilters
		check   func(t *testing.T, localOut string)
	}{
		{"json", "json", beadFilters{}, func(t *testing.T, out string) {
			for _, want := range []string{`"no_history": true`, `"hq-`, `"al-`, `"om-`} {
				if !strings.Contains(out, want) {
					t.Fatalf("local JSON lacks %s (fixture did not seed what it claims):\n%s", want, out)
				}
			}
			if strings.Contains(out, c.closedID) {
				t.Fatalf("closed bead %s listed without --all:\n%s", c.closedID, out)
			}
		}},
		{"json-all", "json", beadFilters{all: true}, func(t *testing.T, out string) {
			if !strings.Contains(out, `"`+c.closedID+`"`) {
				t.Fatalf("--all omits closed bead %s:\n%s", c.closedID, out)
			}
		}},
		{"json-label", "json", beadFilters{label: "route-eq"}, func(t *testing.T, out string) {
			if !strings.Contains(out, c.plainID) || !strings.Contains(out, c.alphaID) || strings.Contains(out, c.omegaID) {
				t.Fatalf("--label route-eq selected the wrong beads:\n%s", out)
			}
		}},
		{"json-no-match", "json", beadFilters{label: "no-such-label"}, func(t *testing.T, out string) {
			if strings.TrimSpace(out) != "[]" {
				t.Fatalf("no-match list = %q, want []", out)
			}
		}},
		{"text", "text", beadFilters{}, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			l := c.list(local, row.format, row.filters)
			a := c.list(alive, row.format, row.filters)
			beadsRouteRequireAPI(t, "beads list", a)
			beadsRouteRequireSame(t, row.format == "json", l, a)
			if row.check != nil {
				row.check(t, l.stdout)
			}
			r := c.list(remote, row.format, row.filters)
			beadsRouteRequireAPI(t, "beads list", r)
			if row.format != "json" {
				if r.exit != 0 || r.stdout != l.stdout {
					t.Fatalf("remote text differs (exit %d):\nlocal:\n%s\nremote:\n%s", r.exit, l.stdout, r.stdout)
				}
				return
			}
			inner := beadsRouteUnwrapRemote(t, "beads list", "beads", r)
			if d := chartest.JSONShapeDiff([]byte(l.stdout), inner, nil); d != "" {
				t.Fatalf("remote beads differ from local: %s\nlocal:\n%s\nremote:\n%s", d, l.stdout, r.stdout)
			}
		})
	}
}

// TestBeadsShowRouteEquivalence is the show twin: the same bead prints the
// same document on the local and API routes; a remote target wraps it.
func TestBeadsShowRouteEquivalence(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteHealthy)
	local, alive, remote := c.lane(t, "serverless"), c.lane(t, "alive"), c.lane(t, "remote")

	for _, id := range []string{c.blockedID, c.noHistoryID, c.alphaID} {
		for _, format := range []string{"json", "text"} {
			t.Run(id+"/"+format, func(t *testing.T) {
				l := c.show(local, id, format)
				a := c.show(alive, id, format)
				beadsRouteRequireAPI(t, "beads show", a)
				beadsRouteRequireSame(t, format == "json", l, a)
				r := c.show(remote, id, format)
				beadsRouteRequireAPI(t, "beads show", r)
				if format != "json" {
					if r.exit != 0 || r.stdout != l.stdout {
						t.Fatalf("remote text differs (exit %d):\nlocal:\n%s\nremote:\n%s", r.exit, l.stdout, r.stdout)
					}
					return
				}
				inner := beadsRouteUnwrapRemote(t, "beads show", "bead", r)
				if d := chartest.JSONShapeDiff([]byte(l.stdout), inner, nil); d != "" {
					t.Fatalf("remote bead differs from local: %s\nlocal:\n%s\nremote:\n%s", d, l.stdout, r.stdout)
				}
			})
		}
	}
	if out := c.show(local, c.noHistoryID, "json").stdout; !strings.Contains(out, `"no_history": true`) {
		t.Fatalf("no-history bead lacks no_history:\n%s", out)
	}

	t.Run("missing-id", func(t *testing.T) {
		for _, l := range c.lanes {
			r := c.show(l, "hq-does-not-exist", "json")
			if r.exit != 1 || r.stdout != "" {
				t.Fatalf("%s lane: exit=%d stdout=%q, want 1 and empty; stderr=%q", l.name, r.exit, r.stdout, r.stderr)
			}
		}
	})
}

// beadsRouteRequireListFailure asserts every lane refuses the listing, naming
// the omega store, instead of printing the other stores' beads.
func beadsRouteRequireListFailure(t *testing.T, c *beadsRouteCity) {
	t.Helper()
	for _, format := range []string{"json", "text"} {
		for _, l := range c.lanes {
			r := c.list(l, format, beadFilters{})
			if r.exit != 1 || r.stdout != "" {
				t.Fatalf("%s lane %s: exit=%d stdout=%q, want exit 1 and empty stdout; stderr=%q", l.name, format, r.exit, r.stdout, r.stderr)
			}
			if !strings.Contains(r.stderr, "omega") {
				t.Fatalf("%s lane %s: stderr does not name the omega store: %q", l.name, format, r.stderr)
			}
			if l.client != nil && !strings.Contains(r.stderr, "cmd=beads list route=api reason=error") {
				t.Fatalf("%s lane %s: stderr lacks route=api reason=error: %q", l.name, format, r.stderr)
			}
		}
	}
}

// TestBeadsListRouteEquivalenceWhenARigStoreFailsToList proves an unreadable
// rig store fails gc beads list on every lane rather than shortening it on the
// API lanes, while show of a bead in a healthy store still answers.
func TestBeadsListRouteEquivalenceWhenARigStoreFailsToList(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteListFails)
	beadsRouteRequireListFailure(t, c)

	l := c.show(c.lane(t, "serverless"), c.plainID, "json")
	for _, lane := range c.lanes {
		r := c.show(lane, c.plainID, "json")
		if lane.name == "remote" {
			if r.exit != 0 {
				// A remote target never falls back; the healthy bead must still
				// answer from the API.
				t.Fatalf("remote show of a healthy-store bead: exit=%d stderr=%q", r.exit, r.stderr)
			}
			inner := beadsRouteUnwrapRemote(t, "beads show", "bead", r)
			if d := chartest.JSONShapeDiff([]byte(l.stdout), inner, nil); d != "" {
				t.Fatalf("remote bead differs from local: %s", d)
			}
			continue
		}
		if r.exit != 0 || r.stdout != l.stdout {
			t.Fatalf("%s lane show of a healthy-store bead: exit=%d stdout=%q, want local %q; stderr=%q", lane.name, r.exit, r.stdout, l.stdout, r.stderr)
		}
	}
	for _, lane := range c.lanes {
		r := c.show(lane, c.omegaID, "json")
		if r.exit != 1 || r.stdout != "" {
			t.Fatalf("%s lane show of the unreadable bead: exit=%d stdout=%q, want 1 and empty", lane.name, r.exit, r.stdout)
		}
	}
}

// TestBeadsListRouteEquivalenceWhenARigStoreCannotOpen proves a rig ledger
// neither plane can open fails gc beads list on every lane: the server reports
// it as a partial answer, the direct read refuses it, and no lane silently
// leaves its beads out.
func TestBeadsListRouteEquivalenceWhenARigStoreCannotOpen(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteOpenFails)
	beadsRouteRequireListFailure(t, c)
}

// TestDoBeadsListFallback_UnopenableStoreFailsTheListing pins the direct read:
// a store that cannot open fails the listing and is named, with the doctor hint.
func TestDoBeadsListFallback_UnopenableStoreFailsTheListing(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteOpenFails)
	var stdout, stderr bytes.Buffer
	if code := doBeadsListFallback(c.cityPath, "json", beadFilters{}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	for _, want := range []string{c.omegaPath, `hint: run "gc doctor" for diagnostics`} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

// TestDoBeadsListFallback_ListErrorNamesTheStore pins the direct read's error
// context: a store whose List fails is named in the message.
func TestDoBeadsListFallback_ListErrorNamesTheStore(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteListFails)
	var stdout, stderr bytes.Buffer
	if code := doBeadsListFallback(c.cityPath, "json", beadFilters{}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	want := "gc beads list: listing beads in " + c.omegaPath + ": omega ledger unreadable"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// TestBeadsReadsOnASupervisorHostedCityPrintTheLocalShape drives the real CLI
// entry point on the route #8331 introduced (a supervisor-hosted city served by
// the supervisor API) and proves it prints the same bare JSON as the direct
// read, while a --city-url target keeps the envelope.
func TestBeadsReadsOnASupervisorHostedCityPrintTheLocalShape(t *testing.T) {
	c := newBeadsRouteCity(t, beadsRouteHealthy)
	alive := c.lane(t, "alive")

	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client { return alive.client }
	apiRouteSupervisorGrantsHook = func() bool { return false }

	runGC := func(t *testing.T, args ...string) beadsRouteResult {
		t.Helper()
		clearBuiltinImportWarningCache()
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		return beadsRouteResult{code, stdout.String(), stderr.String()}
	}
	for _, cmd := range [][]string{
		{"beads", "list", "--format=json"},
		{"beads", "show", c.blockedID, "--format=json"},
	} {
		name := strings.Join(cmd[:2], " ")
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--city", c.cityPath}, cmd...)
			t.Setenv("GC_NO_API", "")
			served := runGC(t, args...)
			beadsRouteRequireAPI(t, name, served)
			t.Setenv("GC_NO_API", "1")
			local := runGC(t, args...)
			if !strings.Contains(local.stderr, "cmd="+name+" route=fallback reason=escape-hatch") {
				t.Fatalf("GC_NO_API=1 run did not take the escape hatch; stderr:\n%s", local.stderr)
			}
			beadsRouteRequireSame(t, true, local, served)
		})
	}

	t.Run("city-url keeps the envelope", func(t *testing.T) {
		t.Setenv("GC_NO_API", "")
		base := api.NewSupervisorMux(&singleCityStateResolver{state: c.h.cs}, nil, false, "controller", "test", time.Now()).
			WithAnyHostAllowed().
			Handler()
		srv := httptest.NewServer(base)
		t.Cleanup(srv.Close)
		r := runGC(t, "--city-url", srv.URL, "--city-name", charCityName, "beads", "list", "--format=json")
		inner := beadsRouteUnwrapRemote(t, "beads list", "beads", r)
		if !strings.Contains(string(inner), c.blockedID) {
			t.Fatalf("remote envelope lacks the seeded beads:\n%s", r.stdout)
		}
	})
}
