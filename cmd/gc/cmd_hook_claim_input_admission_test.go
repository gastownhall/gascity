package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestHookClaimNativeInputAdmission(t *testing.T) {
	for _, state := range []string{"ready", "held", "park during claim", "read fails after claim", "adopt held", "foreign step owner"} {
		t.Run(state, func(t *testing.T) {
			f := newHookWorkflowStopFixture(t)
			input := f.create(beads.Bead{Title: "input", Assignee: "another-lane"})
			conv := f.create(beads.Bead{Title: "inputs", Type: "convoy"})
			if err := f.store.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
				t.Fatal(err)
			}
			root := f.root("root", "")
			if err := f.store.SetMetadata(root.ID, "gc.input_convoy_id", conv.ID); err != nil {
				t.Fatal(err)
			}
			step := f.step("step", root.ID, nil)
			f.served = []string{step.ID}
			park := func() {
				t.Helper()
				if err := f.store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "held" || state == "adopt held" {
				park()
			}
			if state == "adopt held" || state == "foreign step owner" {
				status, owner := "in_progress", "worker-1"
				if state == "foreign step owner" {
					owner = "other-worker"
				}
				if err := f.store.Update(step.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "park during claim" {
				f.afterClaim = func(string) { park() }
			}
			ops := f.ops()
			ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
				return "", false, nil
			}
			stores := sourceworkflow.InputStores{Graph: beadstest.Graph(f.store), Work: []beads.WorkStore{beadstest.Work(f.store)}}
			ops.InputAdmission = func(ctx context.Context, _ string, _ []string, candidate beads.Bead) (sourceworkflow.InputAdmission, error) {
				if state == "read fails after claim" && len(f.claims) > 0 {
					return sourceworkflow.InputAdmission{}, errors.New("fixture input read failed")
				}
				return stores.Admission(ctx, candidate)
			}
			started := 0
			ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) { started++ }
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", t.TempDir(), hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true}, ops, &stdout, &stderr)
			delivered := strings.Contains(stdout.String(), `"action":"work"`)
			if delivered != (state == "ready") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if state != "ready" && started != 0 {
				t.Fatalf("started %d executions for withheld input", started)
			}
			after := f.get(step.ID)
			if (state == "park during claim" || state == "read fails after claim") && (after.Status != "open" || after.Assignee != "") {
				t.Fatalf("undelivered claim not released: %+v", after)
			}
			if state == "held" && len(f.claims) != 0 {
				t.Fatal("claimed despite parked input")
			}
			if state == "adopt held" && (after.Assignee != "worker-1" || after.Status != "in_progress") {
				t.Fatal("changed existing obligation")
			}
			if got := f.get(input.ID).Assignee; got != "another-lane" {
				t.Fatalf("input reassigned to %q", got)
			}
		})
	}
}

func TestHookClaimNativeInputAdmissionSplitRoute(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, false)
	base := f.baseOps(t)
	base.InputAdmission = func(ctx context.Context, _ string, _ []string, candidate beads.Bead) (sourceworkflow.InputAdmission, error) {
		return (sourceworkflow.InputStores{Graph: beadstest.Graph(f.work), Work: []beads.WorkStore{beadstest.Work(f.work)}}).Admission(ctx, candidate)
	}
	ops := classRoutedHookClaimOps(base, f.route)
	for _, ready := range []bool{true, false, true} {
		patch := beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}
		if !ready {
			patch = beads.UpdateOpts{Labels: []string{"hold:external"}}
		}
		if err := f.work.Update(f.source.ID, patch); err != nil {
			t.Fatal(err)
		}
		got, err := ops.InputAdmission(context.Background(), "/work", nil, f.step)
		if err != nil || got.Ready != ready {
			t.Fatalf("split admission = %+v, %v; want ready=%v", got, err, ready)
		}
	}
}

// The claim/control work frame must include cross-rig prerequisites, while
// reusing the current scope's handle to avoid duplicate-residence false errors.
func TestInputAdmissionWorkFrame(t *testing.T) {
	cityPath, rigPaths := newReadySuspensionCity(t, readySuspensionRigs)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cityWork := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "input", Status: "open"},
		{ID: "convoy", Type: "convoy", Status: "open"},
		{ID: "root", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}},
	}, []beads.Dep{
		{IssueID: "convoy", DependsOnID: "input", Type: "tracks"},
		{IssueID: "input", DependsOnID: "remote", Type: "blocks"},
	})
	remote := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "remote", Status: "open"}}, nil)
	previous := readyRigStoreOpener
	fail := false
	readyRigStoreOpener = func(*config.City) rigStoreOpener {
		return func(rigPath, _ string) (beads.Store, error) {
			if fail {
				return nil, errors.New("work frame unavailable")
			}
			if rigPath == rigPaths["alpha"] {
				return remote, nil
			}
			return beads.NewMemStore(), nil
		}
	}
	t.Cleanup(func() { readyRigStoreOpener = previous })
	inputs, err := inputWorkStoresForScope(context.Background(), cityPath, cityPath, cfg, beadstest.Work(cityWork))
	if err != nil {
		t.Fatal(err)
	}
	stores := sourceworkflow.InputStores{Graph: beadstest.Graph(cityWork), Work: inputs}
	root, _ := cityWork.Get("root")
	got, err := stores.Admission(context.Background(), root)
	if err != nil || got.Ready {
		t.Fatalf("open remote prerequisite: %+v, %v", got, err)
	}
	if err := remote.Close("remote"); err != nil {
		t.Fatal(err)
	}
	got, err = stores.Admission(context.Background(), root)
	if err != nil || !got.Ready {
		t.Fatalf("closed remote prerequisite: %+v, %v", got, err)
	}
	fail = true
	if _, err := inputWorkStoresForScope(context.Background(), cityPath, cityPath, cfg, beadstest.Work(cityWork)); err == nil {
		t.Fatal("unread work frame admitted")
	}
}

// Exercise the production hookCity wrapper, including the window after CAS.
// A standalone native root has no foreign ancestor, but still has input gates.
func TestHookCityStandaloneInputAdmission(t *testing.T) {
	for _, selfRoot := range []bool{false, true} {
		for _, phase := range []string{"ready", "held", "hold during claim", "adopt held", "transferred during claim"} {
			t.Run(fmt.Sprintf("self=%v/%s", selfRoot, phase), func(t *testing.T) {
				f := newHookWorkflowStopFixture(t)
				input := f.create(beads.Bead{Title: "input"})
				conv := f.create(beads.Bead{Title: "inputs", Type: "convoy"})
				if err := f.store.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
					t.Fatal(err)
				}
				root := f.root("standalone", "")
				meta := map[string]string{"gc.input_convoy_id": conv.ID, "gc.routed_to": "worker"}
				if selfRoot {
					meta["gc.root_bead_id"] = root.ID
				}
				if err := f.store.SetMetadataBatch(root.ID, meta); err != nil {
					t.Fatal(err)
				}
				f.served = []string{root.ID}
				park := func() {
					t.Helper()
					if err := f.store.Update(input.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "held" || phase == "adopt held" {
					park()
				}
				if phase == "adopt held" {
					status, owner := "in_progress", "worker-1"
					if err := f.store.Update(root.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "hold during claim" || phase == "transferred during claim" {
					f.afterClaim = func(id string) {
						park()
						if phase == "transferred during claim" {
							owner := "other-worker"
							if err := f.store.Update(id, beads.UpdateOpts{Assignee: &owner}); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				old := hookClaimCommandRunnerWithEnvContext
				t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = old })
				hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
					return func(_ string, _ string, args ...string) ([]byte, error) {
						switch args[0] {
						case "show":
							b, err := f.store.Get(args[len(args)-1])
							if err != nil {
								return nil, err
							}
							return json.Marshal([]beads.Bead{b})
						case "dep":
							deps, err := f.store.DepList(args[2], "down")
							if err != nil {
								return nil, err
							}
							rows := make([]struct {
								ID   string `json:"id"`
								Type string `json:"dependency_type"`
							}, 0, len(deps))
							for _, dep := range deps {
								rows = append(rows, struct {
									ID   string `json:"id"`
									Type string `json:"dependency_type"`
								}{dep.DependsOnID, dep.Type})
							}
							return json.Marshal(rows)
						default:
							t.Fatalf("unexpected bd args: %v", args)
							return nil, nil
						}
					}
				}
				city := newHookCity(t.TempDir(), &config.City{})
				ops := f.ops()
				ops.InputAdmission = city.claimOps().InputAdmission
				ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
					return "", false, nil
				}
				stamps, starts := 0, 0
				ops.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error { stamps++; return nil }
				ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) { starts++ }
				var stdout, stderr bytes.Buffer
				code := doHookClaim("query", city.path, hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true}, ops, &stdout, &stderr)
				delivered := strings.Contains(stdout.String(), `"action":"work"`)
				if delivered != (phase == "ready") {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
				}
				if phase != "ready" && (stamps != 0 || starts != 0) {
					t.Fatalf("withheld root stamped=%d started=%d", stamps, starts)
				}
				after := f.get(root.ID)
				switch phase {
				case "held":
					if len(f.claims) != 0 {
						t.Fatal("held root claimed")
					}
				case "hold during claim":
					if len(f.claims) != 1 {
						t.Fatalf("claim transition not exercised: %v", f.claims)
					}
					if after.Status != "open" || after.Assignee != "" {
						t.Fatalf("fresh claim not unwound: %+v", after)
					}
				case "adopt held":
					if after.Status != "in_progress" || after.Assignee != "worker-1" {
						t.Fatalf("adopted claim changed: %+v", after)
					}
				case "transferred during claim":
					if after.Assignee != "other-worker" {
						t.Fatalf("foreign claim changed: %+v", after)
					}
				}
			})
		}
	}
}
