package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// setAlphaSuspended writes rig alpha's runtime suspension state as gc rig
// suspend and gc rig resume do.
func setAlphaSuspended(t *testing.T, h *charHarness, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, h.cityPath, "alpha", &suspended); err != nil {
		t.Fatalf("set rig alpha suspended=%v: %v", suspended, err)
	}
}

// alphaSuspendedIn decodes `gc status --json` and reports rig alpha's
// suspended flag.
func alphaSuspendedIn(t *testing.T, raw []byte) bool {
	t.Helper()
	var out StatusJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode gc status --json: %v\n%s", err, raw)
	}
	for _, rig := range out.Rigs {
		if rig.Name == "alpha" {
			return rig.Suspended
		}
	}
	t.Fatalf("gc status --json has no rig alpha:\n%s", raw)
	return false
}

// gc status run right after a change shows the change: the server builds the
// body for each read rather than answering with the one the previous read
// left saved, and that read names no rebuild (ga-v1vyh8.17).
func TestCityStatusShowsAChangeMadeSinceThePreviousRead(t *testing.T) {
	h := newCityStatusRouteHarness(t)
	for _, lane := range apiLanes(t, h) {
		setAlphaSuspended(t, h, false)
		first, _ := cityStatusVia(t, h, lane, true)
		if alphaSuspendedIn(t, first) {
			t.Fatalf("%s: first read shows alpha suspended, want not", lane.name)
		}

		setAlphaSuspended(t, h, true)
		second, _ := cityStatusVia(t, h, lane, true)
		if !alphaSuspendedIn(t, second) {
			t.Errorf("%s: read right after suspending alpha shows it not suspended: the CLI got the previous read's saved body", lane.name)
		}
		if _, stderr := cityStatusVia(t, h, lane, false); strings.Contains(string(stderr), cacheAgeFooterMark) {
			t.Errorf("%s: text-mode stderr has a cache-age footer on a body built for the read:\n%s", lane.name, stderr)
		}
	}
}

// gc rig status run right after an agent starts shows it running.
func TestRigStatusShowsAnAgentStartedSinceThePreviousRead(t *testing.T) {
	h := newCityStatusRouteHarness(t)
	sp, ok := h.cs.SessionProvider().(*runtime.Fake)
	if !ok {
		t.Fatalf("controller session provider = %T, want *runtime.Fake", h.cs.SessionProvider())
	}
	sessionName := agent.SessionNameFor(charCityName, "alpha/rigbot", h.cs.Config().Workspace.SessionTemplate)
	rigbotRunning := func(t *testing.T, raw []byte) bool {
		t.Helper()
		row, found := rigStatusRowsByQualifiedName(t, decodeRigStatusPayload(t, raw))["alpha/rigbot"]
		if !found {
			t.Fatalf("gc rig status --json has no alpha/rigbot row:\n%s", raw)
		}
		return row.Running
	}
	for _, lane := range apiLanes(t, h) {
		_ = sp.Stop(sessionName)
		code, out, errOut := runRigStatusRoute(t, h.cityPath, sp, newFakeDrainOps(), "alpha", lane.client, "", true)
		if code != 0 {
			t.Fatalf("%s: first gc rig status exit %d: %s", lane.name, code, errOut)
		}
		if rigbotRunning(t, out) {
			t.Fatalf("%s: first read shows rigbot running, want not", lane.name)
		}

		if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("start rigbot: %v", err)
		}
		code, out, errOut = runRigStatusRoute(t, h.cityPath, sp, newFakeDrainOps(), "alpha", lane.client, "", true)
		if code != 0 {
			t.Fatalf("%s: second gc rig status exit %d: %s", lane.name, code, errOut)
		}
		if !rigbotRunning(t, out) {
			t.Errorf("%s: read right after starting rigbot shows it not running: the CLI got the previous read's saved body", lane.name)
		}
		if _, _, errOut := runRigStatusRoute(t, h.cityPath, sp, newFakeDrainOps(), "alpha", lane.client, "", false); bytes.Contains(errOut, []byte(cacheAgeFooterMark)) {
			t.Errorf("%s: text-mode stderr has a cache-age footer on a body built for the read:\n%s", lane.name, errOut)
		}
	}
}

// freshIgnoringSupervisor is a supervisor that predates ?fresh: it records the
// query each /status read sends, ignores it, and answers with a saved body
// 300 s old. With rebuilding set it reports the refresh headers a #8756-era
// supervisor sends; without, only the age an older one sends.
type freshIgnoringSupervisor struct {
	mu      sync.Mutex
	queries []url.Values
}

func (s *freshIgnoringSupervisor) handler(rebuilding bool, body map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Query())
		s.mu.Unlock()
		w.Header().Set("X-GC-Cache-Age-S", "300")
		if rebuilding {
			w.Header().Set("X-GC-Cache-Refresh", "rebuilding")
			w.Header().Set("X-GC-Cache-Refresh-Interval-S", "30")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// requireFreshAsked fails unless every recorded read asked for ?fresh=true.
func (s *freshIgnoringSupervisor) requireFreshAsked(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		t.Fatal("the CLI made no /status read")
	}
	for _, q := range s.queries {
		if q.Get("fresh") != "true" {
			t.Errorf("/status query = %v, want fresh=true", q)
		}
	}
}

// olderSupervisorFooters are the footers a supervisor that ignores ?fresh
// provokes: the rebuild it names, or the bare age an older one reports.
var olderSupervisorFooters = map[bool]string{
	true:  "(cache age: 300s — the server answered with a response it saved that long ago and is building a fresh one",
	false: "(cache age: 300s — this read is over 240s old, and the server does not report why",
}

// A supervisor that ignores ?fresh still serves gc status, and the CLI reports
// exactly the age and cause that supervisor sends, claiming no freshness it
// did not get.
func TestRouteCityStatus_ServerIgnoringFreshKeepsItsReport(t *testing.T) {
	cityPath := writeCityStatusTestCity(t)
	cfg, err := loadCityConfig(cityPath, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	body := map[string]any{
		"name": "test-city", "path": "/tmp/test-city",
		"uptime_sec": 1, "suspended": false,
		"agent_count": 0, "rig_count": 0, "running": 0,
		"agents": map[string]any{"total": 0},
		"rigs":   map[string]any{"total": 0},
		"work":   map[string]any{}, "mail": map[string]any{},
	}
	for _, rebuilding := range []bool{true, false} {
		t.Run(map[bool]string{true: "reports rebuilding", false: "reports age only"}[rebuilding], func(t *testing.T) {
			sup := &freshIgnoringSupervisor{}
			srv := httptest.NewServer(sup.handler(rebuilding, body))
			defer srv.Close()
			c := api.NewCityScopedClient(srv.URL, "test-city")

			for _, jsonOut := range []bool{false, true} {
				var stdout, stderr bytes.Buffer
				if code := routeCityStatus(cityPath, cfg, newFakeDrainOps(), c, "", jsonOut, &stdout, &stderr, func() int {
					t.Errorf("gc status (json=%v) fell back to the local route: %s", jsonOut, stderr.String())
					return 1
				}); code != 0 {
					t.Fatalf("gc status (json=%v) exit = %d; stderr=%s", jsonOut, code, stderr.String())
				}
				if strings.Contains(stdout.String(), cacheAgeFooterMark) {
					t.Errorf("gc status (json=%v) stdout carries a cache-age footer:\n%s", jsonOut, stdout.String())
				}
				if !jsonOut {
					if want := olderSupervisorFooters[rebuilding]; !strings.Contains(stderr.String(), want) {
						t.Errorf("gc status stderr = %q, want it to contain %q", stderr.String(), want)
					}
					continue
				}
				var out StatusJSON
				if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
					t.Fatalf("decode gc status --json: %v\n%s", err, stdout.String())
				}
				if out.CacheAgeS == nil || *out.CacheAgeS != 300 {
					t.Errorf("gc status --json _cache_age_s = %v, want 300, the age the supervisor reported", out.CacheAgeS)
				}
			}
			sup.requireFreshAsked(t)
		})
	}
}

// A supervisor that ignores ?fresh still serves gc rig status, with the age
// and cause it reports.
func TestRouteRigStatus_ServerIgnoringFreshKeepsItsReport(t *testing.T) {
	cityPath := writeRigStatusTestCity(t)
	body := map[string]any{
		"name": "test-city", "path": "/tmp",
		"uptime_sec": 1, "suspended": false,
		"agent_count": 0, "rig_count": 0, "running": 0,
		"agents": map[string]any{}, "rigs": map[string]any{},
		"work": map[string]any{}, "mail": map[string]any{},
		"rig_details": []map[string]any{
			{"name": "frontend", "path": "/tmp/frontend", "suspended": false},
		},
	}
	for _, rebuilding := range []bool{true, false} {
		t.Run(map[bool]string{true: "reports rebuilding", false: "reports age only"}[rebuilding], func(t *testing.T) {
			sup := &freshIgnoringSupervisor{}
			srv := httptest.NewServer(sup.handler(rebuilding, body))
			defer srv.Close()
			c := api.NewCityScopedClient(srv.URL, "test-city")

			rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
			var stdout, stderr bytes.Buffer
			if code := routeRigStatus(cityPath, "test-city", rig, nil, "", nil, nil, nil, runtime.NewFake(), newFakeDrainOps(), c, "", false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc rig status exit = %d; stderr=%s", code, stderr.String())
			}
			if strings.Contains(stdout.String(), cacheAgeFooterMark) {
				t.Errorf("gc rig status stdout carries a cache-age footer:\n%s", stdout.String())
			}
			if want := olderSupervisorFooters[rebuilding]; !strings.Contains(stderr.String(), want) {
				t.Errorf("gc rig status stderr = %q, want it to contain %q", stderr.String(), want)
			}
			sup.requireFreshAsked(t)
		})
	}
}
