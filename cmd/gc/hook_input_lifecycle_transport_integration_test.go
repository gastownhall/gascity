//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestInputLifecycleServer(t *testing.T) {
	t.Setenv("GC_FAST_UNIT", "0")
	currentGCBinaryForTests(t) // Build before the managed fixture isolates HOME.
	city, scope := setupManagedBdWaitTestCity(t)
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	values, err := nativeDoltOneShotOpenEnvForScope(city, cfg, scope)
	if err != nil {
		t.Fatal(err)
	}
	values["BEADS_DIR"] = filepath.Join(scope, ".beads")
	var env []string
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	measureInputLifecycle(t, city, scope, env, "server")
}

func TestInputLifecycleProxyAndCLI(t *testing.T) {
	t.Setenv("GC_FAST_UNIT", "0")
	configureIsolatedRuntimeEnv(t)
	configureTestDoltIdentityEnv(t)
	bd := waitTestRealBDPath(t)
	t.Setenv("GC_DOLT", "")
	t.Setenv("GC_BIN", currentGCBinaryForTests(t))
	dir := shortSocketTempDir(t, "gc-il-")
	toml := "[workspace]\nname = 'fixture'\nprefix = 'il'\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"BD_BIN": bd, "BEADS_DIR": filepath.Join(dir, ".beads"), "GC_CITY_PATH": dir, "GC_BEADS": "bd", "GC_BEADS_PROXIED_NATIVE": "1", "GC_BEADS_PROVIDER_OWNED": "1", "GC_BEADS_TRANSPORT": "proxied", "GC_BEADS_TARGET": "local", "GC_BEADS_PROXIED_IDLE_TIMEOUT": "0", "BEADS_ACTOR": "worker-1"} {
		t.Setenv(k, v)
	}
	env := os.Environ()
	processEnv := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			processEnv[key] = value
		}
	}
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		return beads.ExecCommandRunnerWithExactEnvContext(ctx, processEnv)(dir, bin, args...)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if out, err := run(ctx, bd, "dolt", "stop"); err != nil {
			t.Errorf("private proxy stop: %v %s", err, out)
		}
	})
	if out, err := run(t.Context(), "git", "init", "-q"); err != nil {
		t.Fatalf("private git: %v %s", err, out)
	}
	script := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	if out, err := run(t.Context(), script, "init", dir, "il", "il"); err != nil {
		t.Fatalf("private init: %v %s", err, out)
	}
	registerProviderOwnedScopeCustomTypes(dir, dir)
	measureInputLifecycle(t, dir, dir, env, "proxied-server")
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml+"\n[beads]\nnative_transport = 'off'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	measureInputLifecycle(t, dir, dir, env, "cli")
}

// Queries, classification, claim and retirement use the production hook. Only
// drain acknowledgement and unrelated session notifications are test-local.
func measureInputLifecycle(t *testing.T, city, scope string, env []string, profile string) {
	t.Helper()
	result, err := openStoreResultAtForCity(scope, city)
	if err != nil {
		t.Fatal(err)
	}
	store := result.Store
	t.Cleanup(func() {
		if err := closeBeadStoreHandle(store); err != nil {
			t.Error(err)
		}
	})
	t.Logf("profile=%s factory=%T diagnostic=%+v", profile, storeEngine(store), result.Diagnostic)
	if profile == "cli" {
		if _, ok := storeEngine(store).(*beads.BdStore); !ok {
			t.Fatalf("native_transport=off selected %T", storeEngine(store))
		}
	}
	if profile == "proxied-server" {
		if _, ok := storeEngine(store).(*beads.ProxiedStore); !ok {
			t.Fatalf("proxy selected %T", storeEngine(store))
		}
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"", "external", "graph", "cancel"} {
		t.Run(profile+"/policy="+scenario, func(t *testing.T) {
			policy := scenario
			if scenario == "cancel" {
				policy = "graph"
			}
			create := func(b beads.Bead) beads.Bead {
				t.Helper()
				row, err := store.Create(b)
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			get := func(id string) beads.Bead {
				t.Helper()
				row, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			source := create(beads.Bead{Title: "source", Type: "task"})
			input := create(beads.Bead{Title: "input", Type: "convoy"})
			if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(store)}, input.ID, source.ID); err != nil {
				t.Fatal(err)
			}
			meta := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.input_convoy_id": input.ID}
			if policy != "" {
				meta["gc.input_lifecycle"] = policy
			}
			if scenario == "cancel" {
				meta["gc.routed_to"], meta["gc.cancel_requested"] = "worker", "true"
			}
			root := create(beads.Bead{Title: "workflow", Type: "task", Metadata: meta})
			bodyRoute := "worker"
			if scenario == "cancel" {
				bodyRoute = "ordinary"
			}
			body := create(beads.Bead{Title: "body", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": bodyRoute}})
			if err := store.Close(source.ID); err != nil {
				t.Fatal(err)
			}
			var tail beads.Bead
			if scenario == "cancel" {
				tail = create(beads.Bead{Title: "teardown", Type: "task", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.routed_to": "cleanup", "gc.scope_role": "teardown"}})
				exclude, err := molecule.TeardownTailExclusion(beadstest.GraphMolecule(store), root.ID)
				if err != nil {
					t.Fatal(err)
				}
				failed := &inputLifecycleCancelFailure{Store: store, failures: 1}
				if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(beadstest.GraphMolecule(failed), root.ID, "canceled", sourceworkflow.RunCanceledCloseReason, map[string]string{"gc.cancel_requested": "true"}, exclude); err == nil {
					t.Fatal("winddown failure not exercised")
				}
			}
			root, source = get(root.ID), get(source.ID)
			ops := newHookCity(city, cfg).claimOps()
			ops.applyDefaults()
			ops.Runner = func(command, dir string) (string, error) { return shellWorkQueryWithEnv(command, dir, env) }
			claims := 0
			claim := ops.Claim
			ops.Claim = func(ctx context.Context, dir string, e []string, id, actor string) (beads.Bead, bool, error) {
				claims++
				return claim(ctx, dir, e, id, actor)
			}
			ops.DrainAck = func(io.Writer) error { return nil }
			if scenario == "cancel" {
				failed := &inputLifecycleCancelFailure{Store: ops.WorkflowStore(t.Context(), scope, env, root.ID, "worker-1"), failures: 1, batchOnly: true}
				ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store { return failed }
				drain := create(beads.Bead{Title: "drain", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.kind": "drain"}})
				item := create(beads.Bead{Title: "item", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.drain_control_id": drain.ID}})
				nested := create(beads.Bead{Title: "adopted", Metadata: map[string]string{"gc.root_bead_id": item.ID}})
				foreign := create(beads.Bead{Title: "foreign", Metadata: map[string]string{"gc.root_bead_id": root.ID}})
				for id, actor := range map[string]string{nested.ID: "worker-1", foreign.ID: "foreign-session"} {
					status := "in_progress"
					if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &actor}); err != nil {
						t.Fatal(err)
					}
				}
				var diagnostic bytes.Buffer
				if !hookClaimWorkflowStop(t.Context(), get(nested.ID), "worker-1", false, hookClaimOptions{Env: env, IdentityCandidates: []string{"worker-1"}}, ops, scope, &diagnostic) || get(nested.ID).Status != "closed" {
					t.Fatalf("real-bd adopted member not closed: %s", &diagnostic)
				}
				if get(root.ID).Status == "closed" || get(foreign.ID).Status != "in_progress" || failed.failures != 1 {
					t.Fatal("linked observation performed an unnecessary subtree close")
				}
				t.Logf("real-bd nested adoption and foreign holder: %s", &diagnostic)
			}
			query := fmt.Sprintf("%q ready --json --include-ephemeral --metadata-field gc.routed_to=worker --limit 0", waitTestRealBDPath(t))
			var out, stderr bytes.Buffer
			code := doHookClaim(query, scope, hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, Env: env, JSON: true, DrainAck: true}, ops, &out, &stderr)
			t.Logf("code=%d result=%s stderr=%s claim_calls=%d hook_store=%T", code, &out, &stderr, claims, ops.WorkflowStore(t.Context(), scope, env, root.ID, "worker-1"))
			var got hookClaimJSONResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				t.Fatal("hook failed")
			}
			if scenario == "cancel" {
				if claims != 0 || got.BeadID != "" || get(root.ID).Status == "closed" {
					t.Fatalf("failed retry delivered work or closed root: %s", &out)
				}
				out.Reset()
				stderr.Reset()
				code = doHookClaim(query, scope, hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, Env: env, JSON: true, DrainAck: true}, ops, &out, &stderr)
				t.Logf("automatic next hook retry code=%d result=%s stderr=%s", code, &out, &stderr)
				if code != 0 || claims != 0 {
					t.Fatal("cancel retry failed or made ordinary CAS")
				}
				for _, id := range []string{root.ID, body.ID} {
					row := get(id)
					if row.Status != "closed" || row.Metadata["gc.outcome"] != "canceled" || row.CloseReason != sourceworkflow.RunCanceledCloseReason {
						t.Fatalf("cancel convergence=%+v", row)
					}
				}
				if get(root.ID).Metadata["gc.cancel_requested"] != "true" {
					t.Fatal("cancel intent lost")
				}
				cleanupQuery := fmt.Sprintf("%q ready --json --include-ephemeral --metadata-field gc.routed_to=cleanup --limit 0", waitTestRealBDPath(t))
				out.Reset()
				stderr.Reset()
				code = doHookClaim(cleanupQuery, scope, hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"cleanup"}, Env: env, JSON: true, DrainAck: true}, ops, &out, &stderr)
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if code != 0 || got.BeadID != tail.ID || claims != 1 {
					t.Fatalf("teardown not runnable: %s %s", &out, &stderr)
				}
				if _, err := store.CloseAll([]string{tail.ID}, map[string]string{"gc.outcome": "pass"}); err != nil {
					t.Fatal(err)
				}
			} else if policy == "graph" {
				if got.BeadID != body.ID || claims != 1 {
					t.Fatalf("graph claim=%+v calls=%d", got, claims)
				}
				if !reflect.DeepEqual(root, get(root.ID)) {
					t.Fatal("graph policy mutated root")
				}
				if err := store.Close(body.ID); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(root.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if got.BeadID != "" || claims != 0 {
					t.Fatalf("obsolete claim=%+v calls=%d", got, claims)
				}
				for _, id := range []string{root.ID, body.ID} {
					row := get(id)
					if row.Status != "closed" || row.Metadata["gc.outcome"] != "skipped" || row.CloseReason != hookSkipDoneWorkflowCloseReason {
						t.Fatalf("retirement=%+v", row)
					}
				}
			}
			if !reflect.DeepEqual(source, get(source.ID)) {
				t.Fatal("policy mutated source")
			}
			for poll := 0; poll < 3; poll++ {
				data, err := ops.Runner(query, scope)
				if err != nil {
					t.Fatal(err)
				}
				var rows []beads.Bead
				if err := json.Unmarshal([]byte(data), &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 {
					t.Fatalf("poll %d demand=%s", poll, data)
				}
			}
			t.Log("three real eligible-demand polls returned zero")
		})
	}
}
