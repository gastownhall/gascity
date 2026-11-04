package main

import (
	"bytes"
	"context"
	"errors"
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
