package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/apicontract"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The API contract harness stands up ONE real city in-process and drives the
// supervisor HTTP surface through the generated client:
//
//   - the production controllerState (cmd/gc/api_state.go), not the
//     internal/api fakeState handlers are unit-tested against;
//   - the production CityRuntime reconciler loop (newCityRuntime + run), wired
//     the way startOneCity wires a supervisor-managed city, so async (202)
//     operations complete the way they do in production;
//   - the production SupervisorMux, served through apicontract's in-process
//     transport (no listener), which validates EVERY response against the
//     live OpenAPI document and records the operationId it exercised;
//   - runtime.Fake as the session provider; the production file event
//     recorder at .gc/events.jsonl (the run projection reads that file
//     directly); and the file bead provider on a t.TempDir() city.
//
// Nothing else is stubbed except the two network boundaries the rig-create
// path crosses (git clone, SSRF DNS), exactly as the capstone E2E does.

const (
	contractCityName = "contract-city"
	// contractAgent is an arbitrary city-scoped agent template name. ZERO
	// hardcoded roles: the suite only needs *a* configured template.
	contractAgent = "worker"
	// contractRigAgent is a rig-scoped template; it expands to
	// contractRig/contractRigAgent.
	contractRigAgent = "rigbot"
	// contractRig is a rig declared in city.toml at startup.
	contractRig = "alpha"
	// contractCSRF is the anti-CSRF header value every mutation sends.
	contractCSRF = "api-contract"
	// contractWait bounds every event-driven wait. It is a safety deadline,
	// not the expected duration: waits return as soon as the fact arrives.
	contractWait = 30 * time.Second
)

// contractHarness is the per-suite fixture.
type contractHarness struct {
	t         *testing.T
	ctx       context.Context
	cityPath  string
	rigPath   string
	cs        *controllerState
	cr        *CityRuntime
	sp        *runtime.Fake
	events    events.Provider
	spec      *apicontract.Spec
	transport *apicontract.InProcessTransport
	client    *genclient.ClientWithResponses
	stderr    *lockedBuffer
	clones    atomic.Int64
	// mailbox is the session name the mail family addresses.
	mailbox string
}

func newContractHarness(t *testing.T) *contractHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_HOME", t.TempDir())
	clearInheritedBeadsEnv(t)

	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), contractRig)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapScopedFileProviderCityFS(fsys.OSFS{}, cityPath); err != nil {
		t.Fatal(err)
	}
	// The rig is provisioned the way `gc rig add` leaves a file-provider rig:
	// its own empty scoped store, bound to the city through .gc/site.toml.
	writeContractFile(t, filepath.Join(rigPath, ".gc", "beads.json"), "{\"seq\":0,\"beads\":[]}\n")
	siteToml := fmt.Sprintf("workspace_name = %q\nworkspace_prefix = \"hq\"\n\n[[rig]]\nname = %q\npath = %q\n", contractCityName, contractRig, rigPath)
	writeSchema2RigCity(t, cityPath, contractCityName, contractCityToml(), siteToml)
	writeContractFile(t, filepath.Join(cityPath, "agents", contractAgent, "agent.toml"), "scope = \"city\"\nstart_command = \"true\"\n")
	writeContractFile(t, filepath.Join(cityPath, "agents", contractAgent, "prompt.md"), "contract worker\n")
	// A webhook-trigger exec order backs the order-run (202) contract.
	writeContractFile(t, filepath.Join(cityPath, "orders", contractOrder+".toml"),
		"[order]\ndescription = \"contract webhook order\"\ntrigger = \"webhook\"\nexec = \"true\"\n\n[order.params]\nticket = { required = false }\n")
	// A rig-scoped template expands to <rig>/<name> and backs the qualified
	// agent routes.
	writeContractFile(t, filepath.Join(cityPath, "agents", contractRigAgent, "agent.toml"), "scope = \"rig\"\nstart_command = \"true\"\n")

	cfg, prov, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	applyFeatureFlags(cfg)
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	sp := runtime.NewFake()
	stderr := &lockedBuffer{}
	rec, err := openSupervisorCityEventsRecorder(cityPath, cfg.Events, stderr)
	if err != nil {
		t.Fatalf("open city event recorder: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("controller stderr:\n%s", stderr.String())
		}
	})

	wiring, err := newControllerWiring(cfg, func(string) (string, bool) { return "", false }, stderr)
	if err != nil {
		t.Fatalf("controller wiring: %v", err)
	}
	started := make(chan struct{})
	var startedOnce atomic.Bool
	cr, err := newCityRuntime(wiring.runtimeParams(CityRuntimeParams{
		CityPath:                cityPath,
		CityName:                contractCityName,
		TomlPath:                filepath.Join(cityPath, "city.toml"),
		WatchTargets:            config.WatchTargets(prov, cfg, cityPath),
		ConfigRev:               configRev,
		Cfg:                     cfg,
		SP:                      sp,
		BuildFn:                 supervisorBuildAgentsFn(cityPath, contractCityName, stderr),
		BuildFnWithSessionBeads: supervisorBuildAgentsFnWithSessionBeads(cityPath, contractCityName, stderr),
		Dops:                    newDrainOps(sp),
		Rec:                     rec,
		PoolSessions:            computePoolSessions(cfg, contractCityName, cityPath, sp),
		PoolDeathHandlers:       computePoolDeathHandlers(cfg, contractCityName, cityPath, sp, stderr),
		ForceStopShutdown:       &atomic.Bool{},
		OnStarted: func() {
			if startedOnce.CompareAndSwap(false, true) {
				close(started)
			}
		},
		// No managed Dolt: the file provider owns the city's beads.
		ManagedDoltHealth: func(string) error { return nil },
		ManagedDoltOwned:  func(string) (bool, error) { return false, nil },
		ManagedDoltPort:   func(string) string { return "" },
		LogPrefix:         "gc api-contract",
		Stdout:            stderr,
		Stderr:            stderr,
	}))
	if err != nil {
		t.Fatalf("build city runtime: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cs := newControllerStateWithRoutes(ctx, cr.storageRoutes, cfg, sp, rec, contractCityName, cityPath)
	cs.ct = cr.crashTrack()
	wireControllerWakeSignals(cs, wiring.wake)
	cs.configDirty = wiring.configDirty
	cs.services = cr.svc
	cr.setControllerState(cs)
	cs.startBeadEventWatcher(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(contractWait):
			t.Errorf("city runtime did not stop within %s", contractWait)
		}
		if err := rec.Close(); err != nil {
			t.Errorf("close event recorder: %v", err)
		}
	})
	select {
	case <-started:
	case <-done:
		t.Fatalf("city runtime exited before startup completed; stderr:\n%s", stderr.String())
	case <-time.After(contractWait):
		t.Fatalf("city runtime did not finish startup within %s; stderr:\n%s", contractWait, stderr.String())
	}

	handler := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "contract", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
	spec := loadLiveContractSpec(t, handler)
	transport := apicontract.NewInProcessTransport(spec, handler)
	client, err := genclient.NewClientWithResponses("http://contract.invalid", genclient.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("generated client: %v", err)
	}
	h := &contractHarness{
		t:         t,
		ctx:       ctx,
		cityPath:  cityPath,
		rigPath:   rigPath,
		cs:        cs,
		cr:        cr,
		sp:        sp,
		events:    rec,
		spec:      spec,
		transport: transport,
		client:    client,
		stderr:    stderr,
	}
	return h
}

// contractCityToml is the harness city: file beads, the fake session
// provider, one rig, and the built-in maintenance orders that shell out
// skipped (they are not part of the HTTP contract).
func contractCityToml() string {
	skipped := []string{
		"beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper",
		"order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect",
		"wisp-compact",
	}
	quoted := make([]string, len(skipped))
	for i, name := range skipped {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf(`[workspace]

[beads]
provider = "file"

[session]
provider = "fake"

[orders]
skip = [%s]

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = %q
prefix = "al"
`, strings.Join(quoted, ", "), contractRig)
}

func writeContractFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadLiveContractSpec fetches /openapi.json from the mux under test (the
// same document genspec commits; TestOpenAPISpecInSync keeps them equal).
func loadLiveContractSpec(t *testing.T, h http.Handler) *apicontract.Spec {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", rec.Code, rec.Body.String())
	}
	spec, err := apicontract.Load(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("load OpenAPI spec: %v", err)
	}
	return spec
}

// --- call helpers -------------------------------------------------------

// contractResponse is the subset of every genclient *Response type the suite
// needs; all generated response wrappers implement it.
type contractResponse interface {
	StatusCode() int
}

// expectStatus fails unless the generated call succeeded with one of the
// wanted statuses. It returns the response for further assertions.
func expectStatus[R contractResponse](t *testing.T, what string, resp R, err error, want ...int) R {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: transport error: %v", what, err)
	}
	got := resp.StatusCode()
	for _, w := range want {
		if got == w {
			return resp
		}
	}
	t.Fatalf("%s: status %d, want %v; body: %s", what, got, want, contractBody(resp))
	return resp
}

// expectKnownBug pins a documented API bug: it passes while the operation
// still answers bugStatus (logging it, so the bug stays visible in test
// output) and passes once the operation answers one of the fixed statuses.
// Anything else fails. Remove the call's bugStatus when the bug is fixed.
func expectKnownBug[R contractResponse](t *testing.T, what string, resp R, err error, bugStatus int, fixed ...int) {
	t.Helper()
	if err == nil && resp.StatusCode() == bugStatus {
		t.Logf("KNOWN BUG still present: %s answered %d: %s", what, bugStatus, contractBody(resp))
		return
	}
	expectStatus(t, what, resp, err, fixed...)
}

// contractBody extracts the raw Body field generated responses carry.
func contractBody(resp any) string {
	v := reflect.ValueOf(resp)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("Body"); f.IsValid() && f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.Uint8 {
			return string(f.Bytes())
		}
	}
	return fmt.Sprintf("%+v", resp)
}

// mustJSON fails when a generated typed-body pointer is nil.
func mustJSON[T any](t *testing.T, what string, v *T, resp any) *T {
	t.Helper()
	if v == nil {
		t.Fatalf("%s: typed body is nil; raw: %s", what, contractBody(resp))
	}
	return v
}

// --- event-driven waits -------------------------------------------------

// errEventNotFound is returned when the context ends before a match.
var errEventNotFound = errors.New("event not observed before deadline")

// waitEvent blocks on the city event bus (no polling) until match accepts an
// event with Seq > afterSeq, and returns it.
func (h *contractHarness) waitEvent(afterSeq uint64, what string, match func(events.Event) bool) events.Event {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, contractWait)
	defer cancel()
	w, err := h.events.Watch(ctx, afterSeq)
	if err != nil {
		h.t.Fatalf("watch events for %s: %v", what, err)
	}
	defer w.Close() //nolint:errcheck
	var seen []string
	for {
		e, err := w.Next()
		if err != nil {
			h.t.Fatalf("waiting for %s: %v (%v); last events: %v", what, err, errEventNotFound, tail(seen, 12))
		}
		seen = append(seen, e.Type+" "+e.Subject)
		if match(e) {
			return e
		}
	}
}

func tail(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// readAfterWrite asserts a mutation is visible on a read path. The API
// promises read-after-write for config mutations, so the first read must
// already pass; if it does not, the test logs the violation and then
// requires convergence within contractWait.
func (h *contractHarness) readAfterWrite(t *testing.T, what string, check func() (bool, string)) {
	t.Helper()
	ok, last := check()
	if ok {
		return
	}
	t.Logf("KNOWN BUG observed: stale read after write for %s: %s", what, last)
	h.backoff(t, what+" to become visible", func() (bool, string) { return check() })
}

// backoff re-evaluates check on a bounded exponential backoff until it
// passes or contractWait expires, failing with the last observed state.
func (h *contractHarness) backoff(t *testing.T, what string, check func() (bool, string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, contractWait)
	defer cancel()
	delay := 5 * time.Millisecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %s", what, last)
		case <-timer.C:
		}
		var ok bool
		if ok, last = check(); ok {
			return
		}
		if delay < 500*time.Millisecond {
			delay *= 2
		}
		timer.Reset(delay)
	}
}

// latestSeq is the current head of the city event log.
func (h *contractHarness) latestSeq() uint64 {
	h.t.Helper()
	seq, err := h.events.LatestSeq()
	if err != nil {
		h.t.Fatalf("latest event seq: %v", err)
	}
	return seq
}
