package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/apicontract"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gc session peek route equivalence (#8453): the local route and the API route
// (an in-process supervisor mux over the same city) peek the same sessions,
// refuse the same sessions with the same message, and print the same bytes.
// Both lanes share one fake runtime, so the only difference is the route. The
// declared API-only extra is the human-mode cache-age footer. ACP sessions are
// a declared improvement of the API route (the supervisor owns their
// connections) and are not exercised here.

const peekRouteCityName = "peek-city"

// peekRouteProbe is the runtime both lanes peek: a fake that records the
// lines each Peek asked for and fails the peek of chosen sessions.
type peekRouteProbe struct {
	*runtime.Fake
	mu    sync.Mutex
	lines map[string][]int
	fail  map[string]error
}

func (p *peekRouteProbe) Peek(name string, lines int) (string, error) {
	p.mu.Lock()
	p.lines[name] = append(p.lines[name], lines)
	err := p.fail[name]
	p.mu.Unlock()
	if err != nil {
		return "", err
	}
	return p.Fake.Peek(name, lines)
}

// takePeeks returns and clears the lines recorded for name.
func (p *peekRouteProbe) takePeeks(name string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	got := p.lines[name]
	delete(p.lines, name)
	return got
}

// peekRouteSession is one seeded session bead.
type peekRouteSession struct {
	key, id, sessionName, alias string
}

type peekRouteCity struct {
	path      string
	probe     *peekRouteProbe
	client    *api.Client
	transport *apicontract.InProcessTransport
	sessions  map[string]peekRouteSession
}

// peekRouteSeeds are the seeded sessions: state, whether the runtime runs,
// whether the bead is closed.
var peekRouteSeeds = []struct {
	key, state     string
	running, close bool
}{
	{key: "active", state: "active", running: true},
	{key: "draining", state: "draining", running: true},
	{key: "creating", state: "creating"},
	{key: "asleep", state: "asleep"},
	{key: "suspended", state: "suspended"},
	{key: "closed", state: "asleep", close: true},
	{key: "closed-alias", state: "asleep", close: true},
	{key: "broken", state: "active", running: true},
}

func newPeekRouteCity(t *testing.T) *peekRouteCity {
	t.Helper()
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DEBUG", "1")

	city := t.TempDir()
	t.Setenv("GC_CITY", city)
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"" + peekRouteCityName + "\"\nprefix = \"gc\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, city, "core")

	probe := &peekRouteProbe{Fake: runtime.NewFake(), lines: map[string][]int{}, fail: map[string]error{}}
	store, err := openCityStoreAt(city)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	sessions := map[string]peekRouteSession{}
	for _, seed := range peekRouteSeeds {
		s := peekRouteSession{key: seed.key, sessionName: "peek-" + seed.key + "-sess", alias: "peek-" + seed.key}
		if seed.key == "closed-alias" {
			s.alias = "peek-gone"
		}
		b, err := store.Create(beads.Bead{
			Type:   "session",
			Title:  "peek " + seed.key,
			Labels: []string{"gc:session"},
			Metadata: map[string]string{
				"session_name": s.sessionName,
				"alias":        s.alias,
				"template":     "peek-probe",
				"state":        seed.state,
			},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", seed.key, err)
		}
		s.id = b.ID
		if seed.close {
			if err := store.Close(b.ID); err != nil {
				t.Fatalf("close %s: %v", seed.key, err)
			}
		}
		if seed.running {
			if err := probe.Start(context.Background(), s.sessionName, runtime.Config{}); err != nil {
				t.Fatalf("start %s: %v", seed.key, err)
			}
		}
		probe.SetPeekOutput(s.sessionName, "output of "+seed.key+"\nsecond line\n")
		sessions[seed.key] = s
	}
	probe.fail[sessions["broken"].sessionName] = errors.New("pane capture failed")

	prevBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = prevBuild })
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return probe, nil
	}

	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(city, "city.toml"))
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := newControllerState(ctx, cfg, probe, events.NewFake(), peekRouteCityName, city)
	h := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
	tr := apicontract.NewInProcessTransport(loadLiveContractSpec(t, h), h)
	client := api.NewCityScopedClientWithHTTPClient("http://peek.invalid", peekRouteCityName, &http.Client{Transport: tr})
	c := &peekRouteCity{path: city, probe: probe, client: client, transport: tr, sessions: sessions}
	t.Cleanup(func() {
		for _, f := range tr.Failures() {
			t.Errorf("API contract failure: %v", f)
		}
	})
	return c
}

// peek runs routeSessionPeek on one lane: the API lane over the in-process
// client, the local lane with no client.
func (c *peekRouteCity) peek(target string, lines int, overAPI, jsonOut bool) (int, string, string) {
	clearBuiltinImportWarningCache()
	var stdout, stderr bytes.Buffer
	client, nilReason := (*api.Client)(nil), "controller-down"
	if overAPI {
		client, nilReason = c.client, ""
	}
	code := routeSessionPeek(c.path, target, lines, client, nilReason, jsonOut, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// peekRouteErrorLine is the gc session peek: line of stderr, or "".
func peekRouteErrorLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "gc session peek: ") {
			return line
		}
	}
	return ""
}

// peekRouteDropCacheFooter drops the API route's declared freshness footer.
func peekRouteDropCacheFooter(stdout string) string {
	lines := strings.SplitAfter(stdout, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, "(cache age: ") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "")
}

func TestSessionPeekRouteEquivalence(t *testing.T) {
	c := newPeekRouteCity(t)
	type row struct {
		key      string
		target   string
		lines    int
		wantExit int
		wantErr  string
		apiRoute string
	}
	byID := func(key string) string { return c.sessions[key].id }
	rows := []row{
		{key: "active", target: byID("active"), lines: 50, apiRoute: "route=api"},
		{key: "draining", target: byID("draining"), lines: 50, apiRoute: "route=api"},
		{key: "creating", target: byID("creating"), lines: 50, apiRoute: "route=api"},
		{key: "asleep", target: byID("asleep"), lines: 50, apiRoute: "route=api"},
		{key: "suspended", target: byID("suspended"), lines: 50, wantExit: 1, wantErr: "session is not active: " + byID("suspended"), apiRoute: "route=api reason=error"},
		{key: "closed", target: byID("closed"), lines: 50, wantExit: 1, wantErr: "session is not active: " + byID("closed"), apiRoute: "route=api reason=error"},
		{key: "closed-alias", target: "peek-gone", lines: 50, wantExit: 1, wantErr: `session not found: "peek-gone"`, apiRoute: "route=api reason=error"},
		{key: "broken", target: byID("broken"), lines: 50, wantExit: 1, wantErr: "pane capture failed", apiRoute: "route=fallback"},
		{key: "active", target: byID("active"), lines: 0, apiRoute: "route=api"},
		{key: "active", target: byID("active"), lines: -5, apiRoute: "route=api"},
		{key: "active", target: byID("active"), lines: 20000, apiRoute: "route=api"},
	}
	for _, r := range rows {
		for _, jsonOut := range []bool{false, true} {
			name := r.key + "/lines=" + strconv.Itoa(r.lines) + "/json=" + strconv.FormatBool(jsonOut)
			t.Run(name, func(t *testing.T) {
				sessName := c.sessions[r.key].sessionName

				localExit, localOut, localErr := c.peek(r.target, r.lines, false, jsonOut)
				localPeeks := c.probe.takePeeks(sessName)
				apiExit, apiOut, apiErr := c.peek(r.target, r.lines, true, jsonOut)
				apiPeeks := c.probe.takePeeks(sessName)

				if localExit != r.wantExit || apiExit != r.wantExit {
					t.Fatalf("exit local=%d api=%d, want %d\nlocal stderr:\n%s\napi stderr:\n%s", localExit, apiExit, r.wantExit, localErr, apiErr)
				}
				if got := peekRouteDropCacheFooter(apiOut); got != localOut {
					t.Fatalf("stdout differs:\nlocal: %q\napi:   %q", localOut, got)
				}
				if want := "cmd=session peek " + r.apiRoute; !strings.Contains(apiErr, want) {
					t.Fatalf("api stderr = %q, want %q", apiErr, want)
				}
				if r.wantExit != 0 {
					localLine, apiLine := peekRouteErrorLine(localErr), peekRouteErrorLine(apiErr)
					if localLine != apiLine || !strings.Contains(localLine, r.wantErr) {
						t.Fatalf("error lines local=%q api=%q, want both to carry %q", localLine, apiLine, r.wantErr)
					}
					return
				}
				if !strings.Contains(localOut, "output of "+r.key) {
					t.Fatalf("stdout = %q, want the seeded output", localOut)
				}
				if jsonOut {
					if d := chartest.JSONShapeDiff([]byte(localOut), []byte(apiOut), nil); d != "" {
						t.Fatalf("JSON shape local->api: %s", d)
					}
					if d := chartest.JSONShapeDiff([]byte(apiOut), []byte(localOut), nil); d != "" {
						t.Fatalf("JSON shape api->local: %s", d)
					}
					validateJSONAgainstBuiltinSchema(t, []string{"session", "peek"}, []byte(apiOut))
				}
				if len(localPeeks) != 1 || localPeeks[0] != r.lines {
					t.Fatalf("local Peek lines = %v, want [%d]", localPeeks, r.lines)
				}
				if want := min(max(r.lines, 0), 10000); len(apiPeeks) != 1 || apiPeeks[0] != want {
					t.Fatalf("api Peek lines = %v, want [%d]", apiPeeks, want)
				}
			})
		}
	}
}

func TestSessionPeekSupervisorHostedCityReadsThroughTheSupervisorAPI(t *testing.T) {
	c := newPeekRouteCity(t)
	t.Setenv("GC_NO_API", "")
	prevIdentity, prevClient, prevGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook = prevIdentity, prevClient, prevGrants
	})
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client { return c.client }
	apiRouteSupervisorGrantsHook = func() bool { return false }

	draining := c.sessions["draining"].id
	_, localOut, _ := c.peek(draining, 50, false, true)

	clearBuiltinImportWarningCache()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", c.path, "session", "peek", draining, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc session peek exit = %d; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cmd=session peek route=api") {
		t.Fatalf("stderr = %q, want the API route", stderr.String())
	}
	if stdout.String() != localOut {
		t.Fatalf("stdout = %q, want the local route's %q", stdout.String(), localOut)
	}
}
