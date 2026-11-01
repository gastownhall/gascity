package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Route-equivalence tests for `gc rig status --json`, `gc mail count` and
// `gc mail check` (#8453). Each command runs once on the local route (nil
// client) and once through an in-process SupervisorMux over the same
// controllerState, store and runtime.Fake, and the two outputs must match
// exactly: no API-only additive keys are declared for these commands.

// serveStateClient serves cs through an in-process supervisor mux and returns
// a city-scoped client for it.
func serveStateClient(t *testing.T, cs *controllerState) *api.Client {
	t.Helper()
	handler := api.NewSupervisorMux(&singleCityStateResolver{state: cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return api.NewCityScopedClient(srv.URL, cs.CityName())
}

// runRigStatusRoute mirrors cmdRigStatus for an already-built provider so
// both routes share one runtime.Fake and one set of drain ops.
func runRigStatusRoute(t *testing.T, cityPath string, sp runtime.Provider, dops drainOps, rigName string, c *api.Client, nilReason string, jsonOut bool) (int, []byte, []byte) {
	t.Helper()
	clearBuiltinImportWarningCache()
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	var rig config.Rig
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			rig, found = r, true
		}
	}
	if !found {
		t.Fatalf("rig %q not in config", rigName)
	}
	var rigAgents []config.Agent
	for _, a := range cfg.Agents {
		if a.Dir == rigName {
			rigAgents = append(rigAgents, a)
		}
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	cityName := loadedCityName(cfg, cityPath)
	snapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), io.Discard)
	var stdout, stderr bytes.Buffer
	exit := routeRigStatus(cityPath, cityName, rig, rigAgents, cfg.Workspace.SessionTemplate, cfg, store, snapshot, sp, dops, c, nilReason, jsonOut, &stdout, &stderr)
	return exit, stdout.Bytes(), stderr.Bytes()
}

// assertRouteEquivalentJSON requires both payloads to validate against the
// command's result schema and to match exactly (no additive keys).
func assertRouteEquivalentJSON(t *testing.T, command []string, local, viaAPI []byte) {
	t.Helper()
	validateJSONAgainstResultSchema(t, command, local)
	validateJSONAgainstResultSchema(t, command, viaAPI)
	if diff := chartest.JSONShapeDiff(local, viaAPI, nil); diff != "" {
		t.Fatalf("API route differs from local route for %v: %s\nlocal: %s\napi:   %s", command, diff, local, viaAPI)
	}
	if diff := chartest.JSONShapeDiff(viaAPI, local, nil); diff != "" {
		t.Fatalf("local route differs from API route for %v: %s\nlocal: %s\napi:   %s", command, diff, local, viaAPI)
	}
}

func requireRoute(t *testing.T, stderr []byte, cmdName, route string) {
	t.Helper()
	want := "cmd=" + cmdName + " route=" + route
	if !strings.Contains(string(stderr), want) {
		t.Fatalf("stderr missing %q:\n%s", want, stderr)
	}
}

// stripRouteLog drops the GC_DEBUG route log lines so the user-facing
// stderr of both routes can be compared.
func stripRouteLog(stderr []byte) string {
	var kept []string
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "cmd=") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func resetRouteEquivalenceIdentityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GC_TMUX_SESSION", "GC_SESSION_ID", "GC_ALIAS", "GC_AGENT", "GC_MAIL"} {
		t.Setenv(key, "")
	}
}

type rigStatusPayload struct {
	CityName string `json:"city_name"`
	Rig      struct {
		Suspended bool `json:"suspended"`
	} `json:"rig"`
	Agents []RigStatusAgent `json:"agents"`
}

func decodeRigStatusPayload(t *testing.T, data []byte) rigStatusPayload {
	t.Helper()
	var p rigStatusPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("decode rig status: %v\n%s", err, data)
	}
	return p
}

func rigStatusRowsByQualifiedName(t *testing.T, p rigStatusPayload) map[string]RigStatusAgent {
	t.Helper()
	out := make(map[string]RigStatusAgent, len(p.Agents))
	for _, a := range p.Agents {
		out[a.QualifiedName] = a
	}
	return out
}

func routeEquivalenceSessionBead(meta map[string]string) beads.Bead {
	if meta["state"] == "" {
		meta["state"] = string(session.StateActive)
	}
	return beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

func TestRigStatusJSONRouteEquivalence(t *testing.T) {
	type rigCase struct {
		name   string
		agents string
		// seed runs before the server exists; it returns the IDs it created.
		seed func(t *testing.T, store beads.Store) map[string]string
		// prepare runs after the city exists and before the first request.
		prepare func(t *testing.T, h *charHarness, sp *runtime.Fake, dops *fakeDrainOps)
		// apiState returns an alternate state to serve; nil serves h.cs.
		apiState func(h *charHarness, sp *runtime.Fake) *controllerState
		check    func(t *testing.T, ids map[string]string, local, viaAPI rigStatusPayload)
	}
	singleton := func(name string, extra string) string {
		return fmt.Sprintf("[[agent]]\nname = %q\ndir = \"frontend\"\nmax_active_sessions = 1\n%s\n", name, extra)
	}
	cases := []rigCase{
		{
			name:   "session id",
			agents: singleton("worker", ""),
			seed: func(t *testing.T, store beads.Store) map[string]string {
				b, err := store.Create(routeEquivalenceSessionBead(map[string]string{
					"template": "frontend/worker", "agent_name": "frontend/worker", "session_name": "frontend--worker",
				}))
				if err != nil {
					t.Fatalf("seed session bead: %v", err)
				}
				return map[string]string{"worker": b.ID}
			},
			prepare: func(t *testing.T, _ *charHarness, sp *runtime.Fake, _ *fakeDrainOps) {
				if err := sp.Start(context.Background(), "frontend--worker", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, ids map[string]string, _, viaAPI rigStatusPayload) {
				row := rigStatusRowsByQualifiedName(t, viaAPI)["frontend/worker"]
				if row.SessionID != ids["worker"] || !row.Running {
					t.Fatalf("API worker row = %+v, want session_id %q and running", row, ids["worker"])
				}
			},
		},
		{
			name:   "pool names",
			agents: "[[agent]]\nname = \"builder\"\ndir = \"frontend\"\nmin_active_sessions = 0\nmax_active_sessions = 2\n",
			prepare: func(t *testing.T, _ *charHarness, sp *runtime.Fake, dops *fakeDrainOps) {
				if err := sp.Start(context.Background(), "frontend--builder-1", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				dops.draining["frontend--builder-1"] = true
			},
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					rows := rigStatusRowsByQualifiedName(t, p)
					for _, qn := range []string{"frontend/builder-1", "frontend/builder-2"} {
						if rows[qn].Name != "builder" {
							t.Fatalf("row %s name = %q, want builder (rows=%+v)", qn, rows[qn].Name, p.Agents)
						}
					}
					if rows["frontend/builder-1"].Status != "draining" {
						t.Fatalf("builder-1 status = %q, want draining", rows["frontend/builder-1"].Status)
					}
				}
			},
		},
		{
			name:   "suspended rig",
			agents: singleton("worker", "") + "[[agent]]\nname = \"builder\"\ndir = \"frontend\"\nmax_active_sessions = 2\n",
			prepare: func(t *testing.T, h *charHarness, sp *runtime.Fake, _ *fakeDrainOps) {
				suspended := true
				if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, h.cityPath, "frontend", &suspended); err != nil {
					t.Fatalf("suspend rig: %v", err)
				}
				if err := sp.Start(context.Background(), "frontend--builder-1", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					if !p.Rig.Suspended {
						t.Fatalf("rig.suspended = false, want true")
					}
					for _, row := range p.Agents {
						if !row.Suspended {
							t.Fatalf("row %s suspended = false in a suspended rig", row.QualifiedName)
						}
						if !row.Running && row.Status != "suspended" {
							t.Fatalf("row %s status = %q, want suspended", row.QualifiedName, row.Status)
						}
					}
				}
			},
		},
		{
			name:   "every agent config-suspended",
			agents: singleton("idler", "suspended = true"),
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					if p.Rig.Suspended {
						t.Fatalf("rig.suspended = true, want false: the rig itself is not suspended")
					}
					row := rigStatusRowsByQualifiedName(t, p)["frontend/idler"]
					if !row.Suspended || row.Status != "suspended" {
						t.Fatalf("idler row = %+v, want suspended", row)
					}
				}
			},
		},
		{
			name:   "registered under another name",
			agents: singleton("worker", ""),
			apiState: func(h *charHarness, sp *runtime.Fake) *controllerState {
				return newControllerState(context.Background(), h.cs.Config(), sp, events.NewFake(), "alias-city", h.cityPath)
			},
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					if p.CityName != charCityName {
						t.Fatalf("city_name = %q, want %q", p.CityName, charCityName)
					}
				}
			},
		},
		{
			name:   "rig with no agents",
			agents: "",
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					if p.Agents == nil || len(p.Agents) != 0 {
						t.Fatalf("agents = %#v, want []", p.Agents)
					}
				}
			},
		},
		{
			name:   "config-suspended agent in active rig",
			agents: singleton("sleeper", "suspended = true") + singleton("worker", ""),
			check: func(t *testing.T, _ map[string]string, local, viaAPI rigStatusPayload) {
				for _, p := range []rigStatusPayload{local, viaAPI} {
					rows := rigStatusRowsByQualifiedName(t, p)
					if !rows["frontend/sleeper"].Suspended || rows["frontend/worker"].Suspended {
						t.Fatalf("rows = %+v, want only sleeper suspended", p.Agents)
					}
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearInheritedBeadsEnv(t)
			resetRouteEquivalenceIdentityEnv(t)
			rigPath := t.TempDir()
			cityToml := charCityBasic + fmt.Sprintf("\n[[rigs]]\nname = \"frontend\"\npath = %q\n\n", rigPath) + tc.agents
			ids := map[string]string{}
			h := newCharCity(t, cityToml, func(t *testing.T, store beads.Store) {
				if tc.seed != nil {
					ids = tc.seed(t, store)
				}
			})
			sp, ok := h.cs.SessionProvider().(*runtime.Fake)
			if !ok {
				t.Fatalf("session provider is %T, want *runtime.Fake", h.cs.SessionProvider())
			}
			dops := newFakeDrainOps()
			if tc.prepare != nil {
				tc.prepare(t, h, sp, dops)
			}
			served := h.cs
			if tc.apiState != nil {
				served = tc.apiState(h, sp)
			}
			client := serveStateClient(t, served)

			apiExit, apiOut, apiErr := runRigStatusRoute(t, h.cityPath, sp, dops, "frontend", client, "", true)
			requireRoute(t, apiErr, "rig status", "api")
			localExit, localOut, localErr := runRigStatusRoute(t, h.cityPath, sp, dops, "frontend", nil, "controller-down", true)
			requireRoute(t, localErr, "rig status", "fallback")
			if apiExit != 0 || localExit != 0 {
				t.Fatalf("exit api=%d local=%d, want 0/0\napi stderr: %s\nlocal stderr: %s", apiExit, localExit, apiErr, localErr)
			}

			assertRouteEquivalentJSON(t, []string{"rig", "status"}, localOut, apiOut)
			local, viaAPI := decodeRigStatusPayload(t, localOut), decodeRigStatusPayload(t, apiOut)
			if len(local.Agents) != len(viaAPI.Agents) {
				t.Fatalf("row counts differ: local=%d api=%d", len(local.Agents), len(viaAPI.Agents))
			}
			for i := range local.Agents {
				if local.Agents[i].QualifiedName != viaAPI.Agents[i].QualifiedName {
					t.Fatalf("row %d order differs: local=%q api=%q", i, local.Agents[i].QualifiedName, viaAPI.Agents[i].QualifiedName)
				}
			}
			tc.check(t, ids, local, viaAPI)
		})
	}
}

// mailRouteCity builds a city with one "lead" session and mail for it. It
// returns the harness and the IDs it seeded ("lead", and "gone" for a closed
// session whose mailbox still holds one message).
func mailRouteCity(t *testing.T) (*charHarness, map[string]string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	resetRouteEquivalenceIdentityEnv(t)
	cityToml := charCityBasic + "\n[[agent]]\nname = \"lead\"\nmax_active_sessions = 1\n"
	ids := map[string]string{}
	h := newCharCity(t, cityToml, func(t *testing.T, store beads.Store) {
		lead, err := store.Create(routeEquivalenceSessionBead(map[string]string{
			"alias": "lead", "session_name": "lead", "template": "lead",
		}))
		if err != nil {
			t.Fatalf("seed lead session: %v", err)
		}
		ids["lead"] = lead.ID
		gone, err := store.Create(routeEquivalenceSessionBead(map[string]string{
			"alias": "gone", "session_name": "gone", "template": "lead",
		}))
		if err != nil {
			t.Fatalf("seed gone session: %v", err)
		}
		ids["gone"] = gone.ID
		mp := beadmail.New(store)
		first, err := mp.Send("human", "lead", "one", "body")
		if err != nil {
			t.Fatalf("seed mail: %v", err)
		}
		for _, msg := range []struct{ to, subject string }{{"lead", "two"}, {"lead", "three"}, {"human", "four"}, {gone.ID, "five"}} {
			if _, err := mp.Send("human", msg.to, msg.subject, "body"); err != nil {
				t.Fatalf("seed mail: %v", err)
			}
		}
		if err := mp.MarkRead(first.ID); err != nil {
			t.Fatalf("mark read: %v", err)
		}
		if err := store.Close(gone.ID); err != nil {
			t.Fatalf("close gone session: %v", err)
		}
	})
	t.Setenv("GC_CITY_PATH", h.cityPath)
	return h, ids
}

func TestMailCountRouteEquivalence(t *testing.T) {
	type mailCountCase struct {
		name          string
		args          func(ids map[string]string) []string
		env           func(ids map[string]string) map[string]string
		jsonOut       bool
		wantRecipient string
		wantFail      bool
	}
	cases := []mailCountCase{
		{name: "by session id", args: func(ids map[string]string) []string { return []string{ids["lead"]} }, jsonOut: true, wantRecipient: "lead"},
		{name: "by alias", args: func(map[string]string) []string { return []string{"lead"} }, jsonOut: true, wantRecipient: "lead"},
		{name: "default GC_SESSION_ID", env: func(ids map[string]string) map[string]string {
			return map[string]string{"GC_SESSION_ID": ids["lead"]}
		}, jsonOut: true, wantRecipient: "lead"},
		{name: "stale GC_SESSION_ID falls back to GC_ALIAS", env: func(map[string]string) map[string]string {
			return map[string]string{"GC_SESSION_ID": "gc-999", "GC_ALIAS": "lead"}
		}, jsonOut: true, wantRecipient: "lead"},
		{name: "text by session id", args: func(ids map[string]string) []string { return []string{ids["lead"]} }},
		{name: "default human", jsonOut: true, wantRecipient: "human"},
		{name: "unresolvable recipient", args: func(map[string]string) []string { return []string{"nosuch"} }, jsonOut: true, wantFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ids := mailRouteCity(t)
			if tc.env != nil {
				for k, v := range tc.env(ids) {
					t.Setenv(k, v)
				}
			}
			var args []string
			if tc.args != nil {
				args = tc.args(ids)
			}
			client := serveStateClient(t, h.cs)
			run := func(c *api.Client, nilReason string) (int, []byte, []byte) {
				clearBuiltinImportWarningCache()
				var stdout, stderr bytes.Buffer
				exit := routeMailCount(h.cityPath, args, c, nilReason, tc.jsonOut, &stdout, &stderr)
				return exit, stdout.Bytes(), stderr.Bytes()
			}
			apiExit, apiOut, apiErr := run(client, "")
			localExit, localOut, localErr := run(nil, "controller-down")
			requireRoute(t, localErr, "mail count", "fallback")

			if tc.wantFail {
				requireRoute(t, apiErr, "mail count", "fallback reason=recipient-unresolved")
				if apiExit != 1 || localExit != 1 || len(apiOut) != 0 || len(localOut) != 0 {
					t.Fatalf("exit api=%d local=%d stdout api=%q local=%q, want 1/1 and empty", apiExit, localExit, apiOut, localOut)
				}
				if stripRouteLog(apiErr) != stripRouteLog(localErr) {
					t.Fatalf("stderr differs:\napi:   %s\nlocal: %s", apiErr, localErr)
				}
				return
			}
			requireRoute(t, apiErr, "mail count", "api")
			if apiExit != 0 || localExit != 0 {
				t.Fatalf("exit api=%d local=%d\napi stderr: %s\nlocal stderr: %s", apiExit, localExit, apiErr, localErr)
			}
			if !tc.jsonOut {
				if string(apiOut) != string(localOut) {
					t.Fatalf("text differs:\napi:   %q\nlocal: %q", apiOut, localOut)
				}
				if want := "3 total, 2 unread for lead\n"; string(localOut) != want {
					t.Fatalf("text = %q, want %q", localOut, want)
				}
				return
			}
			assertRouteEquivalentJSON(t, []string{"mail", "count"}, localOut, apiOut)
			var got mailCountJSONResult
			if err := json.Unmarshal(apiOut, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Recipient != tc.wantRecipient {
				t.Fatalf("recipient = %q, want %q (%s)", got.Recipient, tc.wantRecipient, apiOut)
			}
			if tc.wantRecipient == "lead" {
				if got.Total != 3 || got.Unread != 2 {
					t.Fatalf("counts = %d/%d, want 3/2 (%s)", got.Total, got.Unread, apiOut)
				}
				if !containsString(got.Recipients, ids["lead"]) {
					t.Fatalf("recipients = %v, want the session bead id %q", got.Recipients, ids["lead"])
				}
			}
		})
	}
}

func TestMailCheckRouteEquivalence(t *testing.T) {
	type mailCheckCase struct {
		name       string
		args       func(ids map[string]string) []string
		env        func(ids map[string]string) map[string]string
		wantExit   int
		wantStdout string
		wantReason string
		wantStderr string
	}
	cases := []mailCheckCase{
		{
			name:       "by session id",
			args:       func(ids map[string]string) []string { return []string{ids["lead"]} },
			wantStdout: "2 unread message(s) for lead\n",
		},
		{
			name: "closed own session",
			env: func(ids map[string]string) map[string]string {
				return map[string]string{"GC_SESSION_ID": ids["gone"]}
			},
			wantExit:   1,
			wantReason: "closed-session",
			wantStderr: "not reading its mailbox",
		},
		{
			name:     "no unread mail",
			args:     func(map[string]string) []string { return []string{"human"} },
			wantExit: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, ids := mailRouteCity(t)
			if tc.name == "no unread mail" {
				// Drain the human mailbox so the guard sees an empty inbox.
				store, err := openCityStoreAt(h.cityPath)
				if err != nil {
					t.Fatal(err)
				}
				mp := beadmail.New(store)
				msgs, err := mp.Check("human")
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if err := mp.MarkRead(m.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.env != nil {
				for k, v := range tc.env(ids) {
					t.Setenv(k, v)
				}
			}
			var args []string
			if tc.args != nil {
				args = tc.args(ids)
			}
			client := serveStateClient(t, h.cs)
			run := func(c *api.Client, nilReason string) (int, []byte, []byte) {
				clearBuiltinImportWarningCache()
				var stdout, stderr bytes.Buffer
				exit := routeMailCheck(h.cityPath, args, false, "", c, nilReason, &stdout, &stderr)
				return exit, stdout.Bytes(), stderr.Bytes()
			}
			apiExit, apiOut, apiErr := run(client, "")
			localExit, localOut, localErr := run(nil, "controller-down")
			requireRoute(t, localErr, "mail check", "fallback")
			if tc.wantReason != "" {
				requireRoute(t, apiErr, "mail check", "fallback reason="+tc.wantReason)
			} else {
				requireRoute(t, apiErr, "mail check", "api")
			}
			if apiExit != tc.wantExit || localExit != tc.wantExit {
				t.Fatalf("exit api=%d local=%d, want %d\napi stderr: %s\nlocal stderr: %s", apiExit, localExit, tc.wantExit, apiErr, localErr)
			}
			if string(apiOut) != tc.wantStdout || string(localOut) != tc.wantStdout {
				t.Fatalf("stdout api=%q local=%q, want %q", apiOut, localOut, tc.wantStdout)
			}
			if stripRouteLog(apiErr) != stripRouteLog(localErr) {
				t.Fatalf("stderr differs:\napi:   %s\nlocal: %s", apiErr, localErr)
			}
			if tc.wantStderr != "" && !strings.Contains(string(localErr), tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", localErr, tc.wantStderr)
			}
		})
	}
}

// TestRenderMailCheckFromAPICountsAllUnread pins that the API route reports
// the server's pre-paging total, not just the first page it received.
func TestRenderMailCheckFromAPICountsAllUnread(t *testing.T) {
	var stdout bytes.Buffer
	cr := api.CachedRead[api.MailListView]{Body: api.MailListView{
		Items: []mail.Message{{ID: "gc-1"}, {ID: "gc-2"}},
		Total: 150,
	}}
	if code := renderMailCheckFromAPI(cr, "lead", false, "", &stdout); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got, want := stdout.String(), "150 unread message(s) for lead\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}

	stdout.Reset()
	empty := api.CachedRead[api.MailListView]{}
	if code := renderMailCheckFromAPI(empty, "lead", false, "", &stdout); code != 1 {
		t.Fatalf("empty exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("empty stdout = %q, want empty", stdout.String())
	}
}
