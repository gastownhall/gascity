package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func inputPolicyFixture(t *testing.T, policy string) (*hookWorkflowStopFixture, beads.Bead, beads.Bead) {
	t.Helper()
	f := newHookWorkflowStopFixture(t)
	source := f.create(beads.Bead{Title: "source"})
	input := f.create(beads.Bead{Title: "input", Type: "convoy"})
	if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(f.store)}, input.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	root := f.root("workflow", "")
	if err := f.store.Update(root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.input_convoy_id": input.ID, "gc.input_lifecycle": policy}}); err != nil {
		t.Fatal(err)
	}
	f.closeAs(source.ID, "pass", nil)
	return f, f.get(root.ID), f.get(source.ID)
}

func claimInputPolicy(t *testing.T, f *hookWorkflowStopFixture, beforeCAS func()) string {
	t.Helper()
	ops := f.ops()
	ops.EmitClaimReleased = func(hookClaimReleaseRecord) {}
	ops.InputDone = func(_ context.Context, _ string, _ []string, b beads.Bead, ids []string) (string, bool, error) {
		return workflowInputDone(beadstest.Work(f.store), b, ids)
	}
	ops.SkipDoneWorkflow = func(_ context.Context, _ string, _ []string, id string) error { return skipDoneWorkflow(f.store, id) }
	if beforeCAS != nil {
		claim := ops.Claim
		ops.Claim = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
			beforeCAS()
			return claim(ctx, dir, env, id, actor)
		}
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("fixture", t.TempDir(), hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true, DrainAck: true}, ops, &stdout, &stderr)
	t.Logf("code=%d stdout=%s stderr=%s claims=%v", code, &stdout, &stderr, f.claims)
	if code != 0 {
		t.Fatal("claim failed")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.BeadID
}

func TestHookInputLifecyclePolicy(t *testing.T) {
	for _, policy := range []string{"graph", "external", "", "grahp"} {
		t.Run("policy="+policy, func(t *testing.T) {
			f, root, source := inputPolicyFixture(t, policy)
			body := f.step("ordinary finalize", root.ID, nil)
			f.served = []string{body.ID}
			got := claimInputPolicy(t, f, nil)
			if policy == "graph" {
				if got != body.ID {
					t.Errorf("claim=%q want=%q", got, body.ID)
				}
			} else if got != "" || len(f.claims) != 0 {
				t.Errorf("unexpected claim=%q CAS=%v", got, f.claims)
			}
			if policy == "graph" || policy == "grahp" {
				if !reflect.DeepEqual(root, f.get(root.ID)) {
					t.Error("policy evaluation mutated root")
				}
			} else {
				f.wantClosed(root.ID, "skipped")
				f.wantClosed(body.ID, "skipped")
			}
			if !reflect.DeepEqual(source, f.get(source.ID)) {
				t.Error("policy evaluation mutated source")
			}
		})
	}
}

// inputLifecycleSeatReads records the actor on the store used for step seats.
type inputLifecycleSeatReads struct {
	beads.Store
	actor string
	reads *[]string
}

func (s inputLifecycleSeatReads) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata["gc.root_bead_id"] != "" {
		*s.reads = append(*s.reads, s.actor)
	}
	return s.Store.List(q)
}

func TestHookInputLifecycleStopStoreContract(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, closed := range []bool{false, true} {
			name := "single/open"
			if split {
				name = "split/open"
			}
			if closed {
				name = strings.TrimSuffix(name, "open") + "closed"
			}
			t.Run(name, func(t *testing.T) {
				f, root, _ := inputPolicyFixture(t, "graph")
				if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
					t.Fatal(err)
				}
				if closed {
					f.closeAs(root.ID, "canceled", nil)
				}
				root = f.get(root.ID)
				var opened []string
				probes := 0
				env := []string{"BD_BIN=/usr/bin/false"} // An unexpected default-store read cannot reach a live ledger.
				dir := t.TempDir()
				ctx := t.Context()
				ops := f.ops()
				ops.WorkflowStore = func(gotCtx context.Context, gotDir string, gotEnv []string, id, actor string) beads.Store {
					if gotCtx != ctx || gotDir != dir || !reflect.DeepEqual(gotEnv, env) {
						t.Error("workflow opener context changed")
					}
					opened = append(opened, id+"/"+actor)
					return f.store
				}
				ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
					probes++
					return beads.Bead{}, errors.New("unexpected residence probe")
				}
				ops.applyDefaults()
				if split {
					// Work-resident root in a split city, deliberately NOT memoized.
					ops = classRoutedHookClaimOps(ops, newClaimRouteFor(t, newSplitEnvClassLeaf(t)))
				}
				var stderr bytes.Buffer
				if !hookClaimWorkflowStop(ctx, root, "worker-1", false, hookClaimOptions{Env: env}, ops, dir, &stderr) {
					t.Fatalf("stopped root delivered: %s", &stderr)
				}
				if probes != 0 {
					t.Errorf("extra residence probes=%d", probes)
				}
				want := []string{root.ID + "/worker-1", root.Metadata["gc.input_convoy_id"] + "/"}
				if !reflect.DeepEqual(opened, want) {
					t.Errorf("openers=%v want=%v", opened, want)
				}
				f.wantClosed(root.ID, "canceled")
			})
		}
	}
	t.Run("split/memoized-convoy-key", func(t *testing.T) {
		f := newDoneWorkflowSplitFixture(t, true)
		if err := f.binding.Update(f.root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.input_lifecycle": "graph", "gc.cancel_requested": "true"}}); err != nil {
			t.Fatal(err)
		}
		root, err := f.binding.Get(f.root.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.route.resident[root.ID] = true
		var keys []string
		base := f.baseOps(t)
		base.WorkflowStore = func(_ context.Context, _ string, _ []string, key, actor string) beads.Store {
			keys = append(keys, key+"/"+actor)
			return f.work
		}
		base.applyDefaults()
		ops := classRoutedHookClaimOps(base, f.route)
		var stderr bytes.Buffer
		if !hookClaimWorkflowStop(t.Context(), root, "worker-1", false, hookClaimOptions{}, ops, "/work", &stderr) {
			t.Fatalf("stopped root delivered: %s", &stderr)
		}
		if !reflect.DeepEqual(keys, []string{f.convoyID + "/"}) {
			t.Errorf("work opener keys=%v want convoy %s and empty actor", keys, f.convoyID)
		}
		got, err := f.binding.Get(root.ID)
		if err != nil || got.Status != "closed" || got.Metadata["gc.outcome"] != "canceled" {
			t.Fatalf("root=%+v err=%v stderr=%s", got, err, &stderr)
		}
	})
	t.Run("single/actor-bearing-step-seats", func(t *testing.T) {
		f, root, source := inputPolicyFixture(t, "graph")
		status, seat := "open", "old-worker"
		if err := f.store.Update(source.ID, beads.UpdateOpts{Status: &status, Assignee: &seat}); err != nil {
			t.Fatal(err)
		}
		step := f.step("previous worker", root.ID, nil)
		if err := f.store.Update(step.ID, beads.UpdateOpts{Assignee: &seat}); err != nil {
			t.Fatal(err)
		}
		f.closeAs(step.ID, "pass", nil)
		if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
			t.Fatal(err)
		}
		root = f.get(root.ID)
		var reads []string
		ops := f.ops()
		ops.WorkflowStore = func(_ context.Context, _ string, _ []string, _ string, actor string) beads.Store {
			return inputLifecycleSeatReads{Store: f.store, actor: actor, reads: &reads}
		}
		ops.applyDefaults()
		var stderr bytes.Buffer
		opts := hookClaimOptions{Env: []string{"BD_BIN=/usr/bin/false"}, IdentityCandidates: []string{"worker-1"}}
		if !hookClaimWorkflowStop(t.Context(), root, "worker-1", false, opts, ops, t.TempDir(), &stderr) {
			t.Fatal("stopped root delivered")
		}
		if !reflect.DeepEqual(reads, []string{"worker-1"}) {
			t.Errorf("step-seat read actors=%v want [worker-1]; stderr=%s", reads, &stderr)
		}
		if !reflect.DeepEqual(root, f.get(root.ID)) || !strings.Contains(stderr.String(), "waiting for terminal input") {
			t.Fatalf("own prior seat was treated as a foreign handoff: %s", &stderr)
		}
	})
}

func TestHookInputLifecycleAuthoritativeRoot(t *testing.T) {
	f := newDoneWorkflowSplitFixture(t, true)
	if err := f.binding.SetMetadata(f.root.ID, "gc.input_lifecycle", "graph"); err != nil {
		t.Fatal(err)
	}
	ops := classRoutedHookClaimOps(f.baseOps(t), f.route)
	var diagnostic bytes.Buffer
	for i := 0; i < 2; i++ {
		if hookClaimWorkflowStop(t.Context(), f.root, "worker-1", false, hookClaimOptions{}, ops, "/work", &diagnostic) {
			t.Fatal("pre-memo root miss withheld work")
		}
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("not-found should be silent: %s", &diagnostic)
	}
	kept, stderr := f.retire(t, ops, f.step)
	if len(kept) != 1 {
		t.Fatalf("split graph retired: %s", stderr)
	}
	_, done, err := ops.InputDone(context.Background(), "/work", nil, f.root, []string{"worker-1"})
	if err != nil || done {
		t.Fatalf("stale root snapshot: done=%v err=%v", done, err)
	}
	f.assertOpen(t, f.root.ID, f.step.ID)
}

func TestHookInputLifecycleCopiedStepInput(t *testing.T) {
	for _, policy := range []string{"graph", "external"} {
		t.Run(policy, func(t *testing.T) {
			f, root, _ := inputPolicyFixture(t, policy)
			body := f.step("copied input metadata", root.ID, map[string]string{"gc.input_convoy_id": root.Metadata["gc.input_convoy_id"]})
			f.served = []string{body.ID}
			got := claimInputPolicy(t, f, nil)
			if policy == "graph" {
				if got != body.ID || !reflect.DeepEqual(root, f.get(root.ID)) {
					t.Fatalf("claim=%q want=%q or authoritative graph root changed", got, body.ID)
				}
			} else {
				if got != "" || len(f.claims) != 0 {
					t.Fatalf("obsolete claim=%q CAS=%v", got, f.claims)
				}
				f.wantClosed(root.ID, "skipped")
				f.wantClosed(body.ID, "skipped")
			}
		})
	}
}

func TestHookInputLifecycleStopAtCAS(t *testing.T) {
	for _, assigned := range []string{"", "worker-1"} {
		t.Run("standalone/assigned="+assigned, func(t *testing.T) {
			f, root, source := inputPolicyFixture(t, "graph")
			status := "open"
			if err := f.store.Update(source.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Update(root.ID, beads.UpdateOpts{Assignee: &assigned, Metadata: map[string]string{"gc.routed_to": "worker"}}); err != nil {
				t.Fatal(err)
			}
			f.served = []string{root.ID}
			got := claimInputPolicy(t, f, func() {
				if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
					t.Fatal(err)
				}
			})
			var stderr bytes.Buffer
			if closed, err := hookResumeGraphCancellation(beadstest.Graph(f.store), beadstest.Work(f.store), f.get(root.ID), []string{"worker-1"}, &stderr); closed || err != nil || !strings.Contains(stderr.String(), "waiting for terminal input or an API retry") {
				t.Fatalf("withhold diagnostic: closed=%v err=%v stderr=%s", closed, err, &stderr)
			}
			current := f.get(root.ID)
			if got != "" || current.Status != "open" || current.Assignee != "" || !reflect.DeepEqual(f.claims, []string{root.ID}) {
				t.Fatalf("undelivered claim not released: delivered=%q root=%+v claims=%v", got, current, f.claims)
			}
		})
	}
	for _, policy := range []string{"", "graph"} {
		for _, when := range []string{"before", "after"} {
			for _, stop := range []string{"cancel", "fail"} {
				t.Run(policy+"/"+when+"/"+stop, func(t *testing.T) {
					f, root, source := inputPolicyFixture(t, policy)
					if policy == "" {
						status := "open"
						if err := f.store.Update(source.ID, beads.UpdateOpts{Status: &status}); err != nil {
							t.Fatal(err)
						}
					}
					body := f.step("body", root.ID, nil)
					f.served = []string{body.ID}
					setStop := func() {
						if stop == "cancel" {
							if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
								t.Fatal(err)
							}
						} else {
							f.closeAs(root.ID, "fail", nil)
						}
					}
					var before func()
					if when == "before" {
						before = setStop
					} else {
						f.afterClaim = func(string) { setStop() }
					}
					if got := claimInputPolicy(t, f, before); got != "" {
						t.Fatalf("stopped work delivered: %s", got)
					}
					want := "skipped"
					if stop == "cancel" {
						want = "canceled"
					}
					f.wantClosed(body.ID, want)
				})
			}
		}
	}
}

type inputLifecycleCancelFailure struct {
	beads.Store
	failures  int
	batchOnly bool
}

func (s *inputLifecycleCancelFailure) CloseAll(ids []string, meta map[string]string) (int, error) {
	if s.failures > 0 && (!s.batchOnly || len(ids) > 1) {
		s.failures--
		return 0, errors.New("injected cancel close failure")
	}
	return s.Store.CloseAll(ids, meta)
}

func TestHookInputLifecycleStandaloneRootCancel(t *testing.T) {
	for _, offered := range []string{"root", "body", "nonterminal-root", "nonterminal-body"} {
		t.Run(offered, func(t *testing.T) {
			f, root, source := inputPolicyFixture(t, "graph")
			if err := f.store.Update(root.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.cancel_requested": "true", "gc.routed_to": "worker"}}); err != nil {
				t.Fatal(err)
			}
			body := f.step("ordinary", root.ID, nil)
			tail := f.step("teardown", root.ID, map[string]string{"gc.scope_role": "teardown"})
			drain := f.step("drain", root.ID, map[string]string{"gc.kind": "drain"})
			item := f.root("nested item", drain.ID)
			nested := f.step("nested ordinary", item.ID, nil)
			nestedTail := f.step("nested teardown", item.ID, map[string]string{"gc.scope_role": "teardown"})
			holder := f.step("foreign holder", root.ID, nil)
			status, actor := "in_progress", "foreign-session"
			if err := f.store.Update(holder.ID, beads.UpdateOpts{Status: &status, Assignee: &actor}); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(offered, "nonterminal") {
				status = "open"
				if err := f.store.Update(source.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatal(err)
				}
				source = f.get(source.ID)
			}
			store := &inputLifecycleCancelFailure{Store: f.store, failures: 1}
			f.store = store
			exclude, err := molecule.TeardownTailExclusion(beadstest.GraphMolecule(store), root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(beadstest.GraphMolecule(store), root.ID, "canceled", sourceworkflow.RunCanceledCloseReason, map[string]string{"gc.cancel_requested": "true"}, exclude); err == nil {
				t.Fatal("initial cancel must fail after its intent stamp")
			}
			f.served = []string{root.ID}
			if offered == "body" || offered == "nonterminal-body" {
				f.served = []string{body.ID}
			}
			store.failures = 1
			if got := claimInputPolicy(t, f, nil); got != "" {
				t.Fatalf("failed recovery delivered %s", got)
			}
			if f.get(root.ID).Status == "closed" {
				t.Fatal("failure injection not exercised")
			}
			if got := claimInputPolicy(t, f, nil); got != "" {
				t.Fatalf("canceled work delivered %s", got)
			}
			if offered == "body" {
				f.served = []string{root.ID}
				claimInputPolicy(t, f, nil)
			}
			if strings.HasPrefix(offered, "nonterminal") {
				for _, id := range []string{root.ID, holder.ID, item.ID, nested.ID} {
					if f.get(id).Status == "closed" {
						t.Fatalf("nonterminal recovery closed %s before API retry", id)
					}
				}
				if offered == "nonterminal-root" && store.failures != 1 {
					t.Fatal("nonterminal root attempted subtree close")
				}
				store.failures = 0
				cs := &controllerState{cfg: &config.City{}, cityName: "fixture", cityPath: t.TempDir(), cityBeadStore: store}
				req := httptest.NewRequest(http.MethodPost, "/v0/city/fixture/runs/"+root.ID+"/cancel", nil)
				req.Header.Set("X-GC-Request", "true")
				rec := httptest.NewRecorder()
				cityAPI(cs).ServeHTTP(rec, req)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("API retry = %d: %s", rec.Code, rec.Body.String())
				}
			}
			for _, id := range []string{root.ID, body.ID, item.ID, nested.ID, holder.ID} {
				f.wantClosed(id, "canceled")
				reason := f.get(id).CloseReason
				memberStop := id == body.ID && strings.Contains(reason, "was canceled before this step started")
				if reason != sourceworkflow.RunCanceledCloseReason && !memberStop {
					t.Fatalf("cancel reason lost on %s", id)
				}
			}
			if len(f.claims) != 0 {
				t.Fatalf("ordinary CAS after cancel: %v", f.claims)
			}
			before := f.get(root.ID)
			for poll := 0; poll < 3; poll++ {
				claimInputPolicy(t, f, nil)
			}
			if !reflect.DeepEqual(before, f.get(root.ID)) || !reflect.DeepEqual(source, f.get(source.ID)) {
				t.Fatal("repeat recovery mutated root/source")
			}
			f.served = []string{tail.ID}
			if got := claimInputPolicy(t, f, nil); got != tail.ID {
				t.Fatalf("teardown stranded: %q", got)
			}
			f.closeAs(tail.ID, "pass", nil)
			f.served = []string{nestedTail.ID}
			if got := claimInputPolicy(t, f, nil); got != nestedTail.ID {
				t.Fatalf("nested teardown stranded: %q", got)
			}
			f.closeAs(nestedTail.ID, "pass", nil)
		})
	}
}

type inputLifecycleCancelBarrier struct {
	beads.Store
	seen, resume chan struct{}
	once         sync.Once
	closes       int
}

func (s *inputLifecycleCancelBarrier) CloseAll(ids []string, meta map[string]string) (int, error) {
	// Both callers share this wrapper. Pause only the hook, after its list.
	block := false
	s.once.Do(func() { block = true })
	if block {
		close(s.seen)
		<-s.resume
	}
	s.closes++
	return s.Store.CloseAll(ids, meta)
}

func TestHookInputLifecycleCancellationLateWinddown(t *testing.T) {
	f, root, _ := inputPolicyFixture(t, "graph")
	if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
		t.Fatal(err)
	}
	root = f.get(root.ID)
	body := f.step("ordinary", root.ID, nil)
	tail := f.step("teardown", root.ID, map[string]string{"gc.scope_role": "teardown"})
	store := &inputLifecycleCancelBarrier{Store: f.store, seen: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := hookResumeGraphCancellation(beadstest.Graph(store), beadstest.Work(store), root, []string{"worker-1"}, io.Discard)
		done <- err
	}()
	awaitClose(t, store.seen, "hook observed recorded cancel")
	exclude, err := molecule.TeardownTailExclusion(beadstest.GraphMolecule(store), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(beadstest.GraphMolecule(store), root.ID, "canceled", sourceworkflow.RunCanceledCloseReason, map[string]string{"gc.cancel_requested": "true"}, exclude); err != nil {
		t.Fatal(err)
	}
	before := []beads.Bead{f.get(root.ID), f.get(body.ID), f.get(tail.ID)}
	close(store.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-t.Context().Done():
		t.Fatal("hook recovery did not return")
	}
	if _, err := hookResumeGraphCancellation(beadstest.Graph(store), beadstest.Work(store), root, []string{"worker-1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Logf("post-list overlap CloseAll calls=%d; stale member close remains B1", store.closes)
	if store.closes < 3 {
		t.Fatal("overlap schedule not exercised through shared wrapper")
	}
	if !reflect.DeepEqual(before, []beads.Bead{f.get(root.ID), f.get(body.ID), f.get(tail.ID)}) {
		t.Fatal("retry rewrote terminal outcome or teardown")
	}
	f.wantClosed(root.ID, "canceled")
	f.wantClosed(body.ID, "canceled")
	if f.get(tail.ID).Status != "open" {
		t.Fatal("teardown swept")
	}
}

func TestHookInputLifecycleResumeMemberFallback(t *testing.T) {
	f, root, _ := inputPolicyFixture(t, "graph")
	if err := f.store.SetMetadata(root.ID, "gc.cancel_requested", "true"); err != nil {
		t.Fatal(err)
	}
	drain := f.step("drain", root.ID, map[string]string{"gc.kind": "drain"})
	item := f.root("item", drain.ID)
	nested := f.step("adopted step", item.ID, nil)
	holder := f.step("foreign sibling", root.ID, nil)
	status, actor := "in_progress", "foreign-session"
	if err := f.store.Update(holder.ID, beads.UpdateOpts{Status: &status, Assignee: &actor}); err != nil {
		t.Fatal(err)
	}
	actor = "worker-1"
	if err := f.store.Update(nested.ID, beads.UpdateOpts{Status: &status, Assignee: &actor}); err != nil {
		t.Fatal(err)
	}
	f.store = &inputLifecycleCancelFailure{Store: f.store, failures: 1, batchOnly: true}
	f.served = []string{nested.ID}
	if got := claimInputPolicy(t, f, nil); got != "" {
		t.Fatalf("stopped step delivered: %s", got)
	}
	f.wantClosed(nested.ID, "canceled")
}

type inputLifecycleGetFailure struct{ beads.Store }

func (s inputLifecycleGetFailure) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("injected Get failure")
}

func TestHookInputLifecycleSnapshotReadError(t *testing.T) {
	f, root, _ := inputPolicyFixture(t, "external")
	_, done, err := workflowInputDoneAcross(inputLifecycleGetFailure{f.store}, beadstest.Work(f.store), root, []string{"worker-1"})
	if err != nil || !done {
		t.Fatalf("external snapshot fallback done=%v err=%v", done, err)
	}
}

func TestHookInputLifecycleOutcomeOnlyAndDiagnostic(t *testing.T) {
	for _, marker := range []string{"gc.cancel_requested", "gc.outcome"} {
		f, root, _ := inputPolicyFixture(t, "graph")
		value := "true"
		if marker == "gc.outcome" {
			value = "canceled"
		}
		if err := f.store.SetMetadata(root.ID, marker, value); err != nil {
			t.Fatal(err)
		}
		root = f.get(root.ID)
		var stderr bytes.Buffer
		if !hookClaimWorkflowStop(t.Context(), root, "worker-1", false, hookClaimOptions{IdentityCandidates: []string{"worker-1"}}, f.ops(), "fixture", &stderr) {
			t.Fatal("stopped root delivered")
		}
		f.wantClosed(root.ID, "canceled")
		if !bytes.Contains(stderr.Bytes(), []byte("resumed cancellation of "+root.ID)) {
			t.Fatalf("missing successful recovery diagnostic: %s", &stderr)
		}
		stderr.Reset()
		if _, err := hookResumeGraphCancellation(beadstest.Graph(f.store), beadstest.Work(f.store), root, []string{"worker-1"}, &stderr); err != nil || stderr.Len() != 0 {
			t.Fatalf("stale snapshot logged a close: err=%v stderr=%s", err, &stderr)
		}
	}
}

func TestHookInputLifecycleParentItemIndependent(t *testing.T) {
	f, parent, _ := inputPolicyFixture(t, "graph")
	item := f.root("item", "")
	if err := f.store.Update(item.ID, beads.UpdateOpts{Metadata: map[string]string{"gc.input_convoy_id": parent.Metadata["gc.input_convoy_id"], "gc.root_bead_id": item.ID}}); err != nil {
		t.Fatal(err)
	}
	child := f.step("external child", item.ID, nil)
	_, done, err := workflowInputDone(beadstest.Work(f.store), child, []string{"worker-1"})
	if err != nil || !done {
		t.Fatalf("parent graph leaked into item: done=%v err=%v", done, err)
	}
}
