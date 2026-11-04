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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func scopedHookBead(id, root, status, owner string) beads.Bead {
	return beads.Bead{ID: id, Status: status, Assignee: owner, Metadata: map[string]string{
		"gc.root_bead_id": root, "gc.routed_to": "worker", "gc.continuation_group": "report",
	}}
}

func TestHookRootContinuation(t *testing.T) {
	for _, tc := range []struct {
		name, status, action, reason, want string
		rows                               []beads.Bead
	}{
		{"same root behind another root", "open", "work", "claimed", "step-a", []beads.Bead{scopedHookBead("step-b", "root-b", "open", ""), scopedHookBead("step-a", "root-a", "open", "")}},
		{"existing assignment", "open", "work", "existing_assignment", "step-a", []beads.Bead{scopedHookBead("step-a", "root-a", "in_progress", "seat")}},
		{"ready assignment", "open", "work", "ready_assignment", "step-a", []beads.Bead{scopedHookBead("step-a", "root-a", "open", "seat")}},
		{"other root only", "open", "wait", "workflow_no_ready", "", []beads.Bead{scopedHookBead("step-b", "root-b", "open", "")}},
		{"foreign owner", "open", "wait", "workflow_no_ready", "", []beads.Bead{scopedHookBead("step-a", "root-a", "in_progress", "another-seat")}},
		{"other assignment", "open", "wait", "different_workflow_assignment", "", []beads.Bead{scopedHookBead("step-b", "root-b", "in_progress", "seat"), scopedHookBead("step-a", "root-a", "open", "")}},
		{"other reservation", "open", "wait", "different_workflow_assignment", "", []beads.Bead{scopedHookBead("step-b", "root-b", "open", "seat"), scopedHookBead("step-a", "root-a", "open", "")}},
		{"readiness gap", "open", "wait", "workflow_no_ready", "", nil},
		{"blocked root", "blocked", "wait", "workflow_blocked", "", nil},
		{"terminal root", "closed", "complete", "workflow_complete", "", []beads.Bead{scopedHookBead("step-a", "root-a", "open", "")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, ops, run, claims := scopedHookFixture(t, tc.status, tc.rows)
			var out, errOut bytes.Buffer
			code := claimHookWorkWithRunner("unused generic query", "/rig", nil, []hookStore{{dir: "/rig"}}, opts, ops, run, nil, &out, &errOut)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &errOut)
			}
			var got hookRootJSONResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err, out.String())
			}
			validateJSONAgainstBuiltinSchema(t, []string{"hook"}, out.Bytes())
			if got.SchemaVersion != "2" || got.RootBeadID != "root-a" || got.Action != tc.action || got.Reason != tc.reason || got.BeadID != tc.want {
				t.Fatalf("result=%+v; want %s/%s bead=%s", got, tc.action, tc.reason, tc.want)
			}
			for _, id := range *claims {
				if id != tc.want {
					t.Fatalf("claimed outside requested root: %v", *claims)
				}
			}
		})
	}
}

func TestHookRootLostRaceAndReadFailures(t *testing.T) {
	for _, mode := range []string{"race", "query error", "claim error", "bad json", "null", "partial decode"} {
		t.Run(mode, func(t *testing.T) {
			rows := []beads.Bead{scopedHookBead("step-a", "root-a", "open", ""), scopedHookBead("step-b", "root-b", "open", "")}
			opts, ops, run, claims := scopedHookFixture(t, "open", rows)
			switch mode {
			case "race":
				ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return beads.Bead{}, false, nil
				}
			case "claim error":
				ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return beads.Bead{}, false, errors.New("write failed")
				}
			case "query error":
				run = func(string, string, []string) (string, error) { return "", errors.New("store unavailable") }
			case "bad json":
				run = func(string, string, []string) (string, error) { return "not json", nil }
			case "null":
				run = func(string, string, []string) (string, error) { return "null", nil }
			case "partial decode":
				run = func(string, string, []string) (string, error) { return `[{"id":123}]`, nil }
			}
			var out, errOut bytes.Buffer
			code := claimHookWorkWithRunner("unused generic query", "/rig", nil, nil, opts, ops, run, nil, &out, &errOut)
			if mode == "race" {
				if code != 0 || !strings.Contains(out.String(), `"action":"wait"`) {
					t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
				}
			} else if code == 0 || out.Len() != 0 {
				t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
			}
			if len(*claims) != 0 {
				t.Fatalf("unexpected claims %v", *claims)
			}
		})
	}
}

func TestHookRootWaitPreservesHoldsAndWorktrees(t *testing.T) {
	for _, mode := range []string{"held", "blocked", "worktree", "finalizer"} {
		t.Run(mode, func(t *testing.T) {
			b := scopedHookBead("step-a", "root-a", "open", "")
			switch mode {
			case "held":
				b.Labels = []string{beadmeta.HoldExternalLabel}
			case "blocked":
				b.Status = "blocked"
			case "worktree":
				b = managedWorktreeCandidate("step-a", "/another/worktree")
				b.Metadata[beadmeta.RootBeadIDMetadataKey] = "root-a"
				b.Metadata[beadmeta.RoutedToMetadataKey] = "worker"
			case "finalizer":
				b.Metadata[beadmeta.KindMetadataKey] = "workflow-finalize"
			}
			opts, ops, run, claims := scopedHookFixture(t, "open", []beads.Bead{b})
			opts.PoolSeat.WorkDir = "/this/worktree"
			ops.SeatInManagedWorktree = func(string) (bool, error) { return false, nil }
			var out, errOut bytes.Buffer
			code := claimHookWorkWithRunner("unused generic query", "/rig", nil, nil, opts, ops, run, nil, &out, &errOut)
			if code != 0 || !strings.Contains(out.String(), `"action":"wait"`) || len(*claims) != 0 {
				t.Fatalf("code=%d out=%s err=%s claims=%v", code, &out, &errOut, *claims)
			}
			if (mode == "held" || mode == "blocked") && !strings.Contains(out.String(), `"reason":"workflow_blocked"`) {
				t.Fatalf("blocked wait lost its reason: %s", &out)
			}
		})
	}
}

func TestHookRootContinuationReservationIsolation(t *testing.T) {
	rows := []beads.Bead{scopedHookBead("step-a", "root-a", "open", ""), scopedHookBead("next-a", "root-a", "open", ""), scopedHookBead("next-b", "root-b", "open", ""), scopedHookBead("owned", "root-a", "open", "another-seat"), scopedHookBead("held", "root-a", "open", "")}
	rows[4].Labels = []string{beadmeta.HoldExternalLabel}
	opts, ops, _, _ := scopedHookFixture(t, "open", rows)
	opts.SessionID = "session-a"
	store := ops.WorkflowStore(context.Background(), "/rig", nil, "root-a", "")
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return rows, nil }
	assigned, err := preassignHookContinuationGroup(rows[0], opts, ops, "/rig")
	if err != nil || len(assigned) != 1 || assigned[0] != "next-a" {
		t.Fatalf("assigned=%v err=%v", assigned, err)
	}
	for _, row := range rows[1:] {
		got, err := store.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := row.Assignee
		if row.ID == "next-a" {
			want = "session-a"
		}
		if got.Assignee != want {
			t.Fatalf("%s assignee=%q want %q", row.ID, got.Assignee, want)
		}
	}
}

func scopedHookFixture(t *testing.T, rootStatus string, rows []beads.Bead) (hookClaimOptions, hookClaimOps, hookStoreRunner, *[]string) {
	t.Helper()
	if rows == nil {
		rows = []beads.Bead{}
	}
	seed := append([]beads.Bead{{ID: "root-a", Status: rootStatus, Metadata: map[string]string{"gc.kind": "workflow"}}}, rows...)
	store := beads.NewMemStoreFrom(0, seed, nil)
	claims := []string{}
	opts := hookClaimOptions{RootBeadID: "root-a", Assignee: "seat", IdentityCandidates: []string{"seat"}, RouteTargets: []string{"worker"}, JSON: true}
	ops := hookClaimOps{
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) },
		Claim: func(_ context.Context, _ string, _ []string, id, actor string) (beads.Bead, bool, error) {
			claims = append(claims, id)
			status := "in_progress"
			if err := store.Update(id, beads.UpdateOpts{Assignee: &actor, Status: &status}); err != nil {
				return beads.Bead{}, false, err
			}
			b, err := store.Get(id)
			return b, err == nil, err
		},
		InputDone: func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
			return "", false, nil
		},
		WorkflowStore:    func(context.Context, string, []string, string, string) beads.Store { return store },
		ConfirmBlocked:   func(context.Context, string, []string, string, string) (bool, error) { return false, nil },
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return nil, nil },
		StampWorkMeta:    func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		PublishRunMap:    func(string, string, ...string) error { return nil },
		DrainAck:         func(io.Writer) error { t.Fatal("scoped continuation acknowledged drain"); return nil },
	}
	run := func(command, _ string, _ []string) (string, error) {
		if command == "unused generic query" {
			t.Fatal("scoped acquisition used generic query")
		}
		if !strings.Contains(command, "--status in_progress") && !strings.Contains(command, "gc.root_bead_id=root-a") {
			t.Fatalf("query lacks explicit root predicate: %s", command)
		}
		b, err := json.Marshal(rows)
		return string(b), err
	}
	return opts, ops, run, &claims
}

func TestHookRootReadErrors(t *testing.T) {
	for _, fault := range []error{beads.ErrNotFound, errors.New("store read failed")} {
		opts, ops, run, _ := scopedHookFixture(t, "open", nil)
		ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return beads.Bead{}, fault
		}
		var out, errOut bytes.Buffer
		if code := claimHookWorkWithRunner("unused generic query", "/rig", nil, []hookStore{{dir: "/rig"}}, opts, ops, run, nil, &out, &errOut); code == 0 || out.Len() != 0 {
			t.Fatalf("fault=%v code=%d stdout=%s stderr=%s", fault, code, &out, &errOut)
		}
	}
}

type rootListFaultStore struct {
	beads.Store
	membersOnly bool
}

func (s rootListFaultStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !s.membersOnly || len(q.Metadata) > 0 {
		return nil, errors.New("list unavailable")
	}
	return s.Store.List(q)
}

func TestHookRootAssignmentAndMemberReadErrors(t *testing.T) {
	for _, membersOnly := range []bool{false, true} {
		opts, ops, run, _ := scopedHookFixture(t, "open", nil)
		store := ops.WorkflowStore(context.Background(), "/rig", nil, "", "")
		ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
			return rootListFaultStore{Store: store, membersOnly: membersOnly}
		}
		var out, errOut bytes.Buffer
		if code := claimHookWorkWithRunner("unused generic query", "/rig", nil, nil, opts, ops, run, nil, &out, &errOut); code == 0 || out.Len() != 0 {
			t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
		}
	}
}

func TestHookRootStoreFallback(t *testing.T) {
	opts, ops, run, claims := scopedHookFixture(t, "open", []beads.Bead{scopedHookBead("step-a", "root-a", "open", ""), scopedHookBead("step-b", "root-b", "open", "")})
	read, claim := ops.ReadWorkMeta, ops.Claim
	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, error) {
		if dir == "/first" {
			return beads.Bead{}, beads.ErrNotFound
		}
		return read(ctx, dir, env, id, actor)
	}
	ops.Claim = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
		if dir == "/first" {
			return beads.Bead{}, false, beads.ErrNotFound
		}
		return claim(ctx, dir, env, id, actor)
	}
	var out, errOut bytes.Buffer
	code := claimHookWorkWithRunner("unused generic query", "/first", nil, []hookStore{{dir: "/first"}, {dir: "/second"}}, opts, ops, run, nil, &out, &errOut)
	if code != 0 || len(*claims) != 1 || (*claims)[0] != "step-a" {
		t.Fatalf("code=%d out=%s err=%s claims=%v", code, &out, &errOut, *claims)
	}
}

// rootGetFaultStore fails a workflow ancestry read without failing the root
// selector's separate canonical lookup or the assignment census.
type rootGetFaultStore struct {
	beads.Store
}

func (s rootGetFaultStore) Get(id string) (beads.Bead, error) {
	if id == "root-a" {
		return beads.Bead{}, errors.New("injected ancestry read failure")
	}
	return s.Store.Get(id)
}

func TestHookRootDomainReadFailures(t *testing.T) {
	for _, tier := range []string{"fresh", "ready", "existing"} {
		for _, fault := range []string{"input before claim", "input before delivery", "ancestry before claim", "ancestry before delivery", "reservation ancestry"} {
			t.Run(tier+"/"+fault, func(t *testing.T) {
				step := scopedHookBead("step-a", "root-a", "open", "")
				if tier != "fresh" {
					step.Assignee = "seat"
				}
				if tier == "existing" {
					step.Status = "in_progress"
				}
				opts, ops, run, claims := scopedHookFixture(t, "open", []beads.Bead{step})
				store := ops.WorkflowStore(context.Background(), "/rig", nil, "", "")
				opts.Env = []string{"GC_SESSION_ID=test-session"}
				opts.StartedFence = hookStartedFence{Token: "test-token"}
				ops.DrainPending = func(string) (bool, error) { return false, nil }
				ops.ResolveSessionWorkDir = func(string) string { return "" }
				ops.ResolveWorkBranch = func(hookClaimWorkTree) string { return "" }
				afterClaim := false
				claim := ops.Claim
				ops.Claim = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
					b, ok, err := claim(ctx, dir, env, id, actor)
					afterClaim = ok && err == nil
					return b, ok, err
				}
				// Adoption has no CAS; its canonical read marks the corresponding
				// boundary before the workflow/delivery checks.
				read := ops.ReadWorkMeta
				stepReads := 0
				ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, error) {
					b, err := read(ctx, dir, env, id, actor)
					if tier == "existing" && id == step.ID {
						stepReads++
						afterClaim = stepReads >= 2
					}
					return b, err
				}
				ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
					if fault == "input before claim" || (fault == "input before delivery" && afterClaim) {
						return "", false, errors.New("injected input read failure")
					}
					return "", false, nil
				}
				ops.WorkflowStore = func(_ context.Context, _ string, _ []string, id, actor string) beads.Store {
					if id != "" && ((fault == "ancestry before claim") || (fault == "ancestry before delivery" && afterClaim) || (fault == "reservation ancestry" && actor == "")) {
						return rootGetFaultStore{Store: store}
					}
					return store
				}
				sibling, err := store.Create(scopedHookBead("next-a", "root-a", "open", ""))
				if err != nil {
					t.Fatal(err)
				}
				ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
					return []beads.Bead{sibling}, nil
				}
				sideEffects := []string{}
				ops.RecordWorkStarted = func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
					sideEffects = append(sideEffects, "work started")
					return hookStartedRecorded, nil
				}
				ops.ForgetWorkStarted = func(string, hookStartedKey) error { return nil }
				ops.EmitClaimReleased = func(hookClaimReleaseRecord) {}
				ops.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error {
					sideEffects = append(sideEffects, "work stamp")
					return nil
				}
				ops.StampSessionClaim = func(string, string) error { sideEffects = append(sideEffects, "session stamp"); return nil }
				ops.PublishRunMap = func(string, string, ...string) error { sideEffects = append(sideEffects, "run map"); return nil }
				ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) { sideEffects = append(sideEffects, "step started") }
				ops.Release = func(_ context.Context, _ string, _ []string, id, actor string) (bool, error) {
					b, err := store.Get(id)
					if err != nil || b.Assignee != actor {
						return false, err
					}
					status, owner := "open", ""
					return true, store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &owner})
				}
				var out, errOut bytes.Buffer
				code := claimHookWorkWithRunner("unused generic query", "/rig", opts.Env, nil, opts, ops, run, nil, &out, &errOut)
				if code == 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "injected") {
					t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
				}
				if len(sideEffects) != 0 {
					t.Fatalf("execution exposed after failed domain read: %v", sideEffects)
				}
				if tier != "existing" && strings.Contains(fault, "before claim") && len(*claims) != 0 {
					t.Fatalf("claimed before resolving domain state: %v", *claims)
				}
				got, err := store.Get(sibling.ID)
				if err != nil || got.Assignee != "" {
					t.Fatalf("reserved sibling after unresolved read: %+v err=%v", got, err)
				}
			})
		}
	}
}

func TestHookUnscopedDomainReadFailuresKeepV1(t *testing.T) {
	for _, fault := range []string{"input", "ancestry"} {
		t.Run(fault, func(t *testing.T) {
			step := scopedHookBead("step-a", "root-a", "open", "")
			opts, ops, _, claims := scopedHookFixture(t, "open", []beads.Bead{step})
			opts.RootBeadID = ""
			rows, err := json.Marshal([]beads.Bead{step})
			if err != nil {
				t.Fatal(err)
			}
			ops.Runner = func(string, string) (string, error) { return string(rows), nil }
			if fault == "input" {
				ops.InputDone = func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
					return "", false, errors.New("input read failed")
				}
			} else {
				store := ops.WorkflowStore(context.Background(), "/rig", nil, "", "")
				ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
					return rootGetFaultStore{Store: store}
				}
			}
			var out, errOut bytes.Buffer
			code := doHookClaim("query", "/rig", opts, ops, &out, &errOut)
			var got hookClaimJSONResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err, out.String(), errOut.String())
			}
			if code != 0 || got.SchemaVersion != "1" || got.Action != "work" || got.BeadID != step.ID || len(*claims) != 1 {
				t.Fatalf("v1 changed: code=%d result=%+v err=%s claims=%v", code, got, &errOut, *claims)
			}
			validateJSONAgainstBuiltinSchema(t, []string{"hook"}, out.Bytes())
		})
	}
}

// Preserve ConditionalWriter so an unresolved reservation cannot pass merely
// because the fault wrapper hid the store's write capability.
type hookMissingAncestorStore struct {
	*beads.MemStore
	missingID    string
	fail         bool
	failedReads  *int
	reservations *[]string
}

var _ beads.ConditionalWriter = (*hookMissingAncestorStore)(nil)

func (s *hookMissingAncestorStore) Get(id string) (beads.Bead, error) {
	if s.fail && id == s.missingID {
		(*s.failedReads)++
		return beads.Bead{}, fmt.Errorf("injected missing ancestor %s: %w", id, beads.ErrNotFound)
	}
	return s.MemStore.Get(id)
}

func (s *hookMissingAncestorStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	*s.reservations = append(*s.reservations, id)
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func seedHookNestedAncestry(t *testing.T, store *beads.MemStore) {
	t.Helper()
	store.HonorExplicitIDs = true
	if err := store.SetMetadata("root-a", beadmeta.DrainControlIDMetadataKey, "drain-a"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []beads.Bead{
		{ID: "drain-a", Status: "open", Metadata: map[string]string{"gc.root_bead_id": "parent-a", "gc.kind": "drain"}},
		{ID: "parent-a", Status: "open", Metadata: map[string]string{"gc.kind": "workflow"}},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHookRootMissingAncestry(t *testing.T) {
	for _, tier := range []string{"fresh", "ready", "existing"} {
		for _, phase := range []string{"before claim", "after CAS", "final writer", "reservation", "live reservation"} {
			if tier == "existing" && phase == "after CAS" {
				continue // Adoption does not mint a CAS.
			}
			for _, missingID := range []string{"root-a", "drain-a", "parent-a"} {
				t.Run(tier+"/"+phase+"/"+missingID, func(t *testing.T) {
					step := scopedHookBead("step-a", "root-a", "open", "")
					if tier != "fresh" {
						step.Assignee = "seat"
					}
					if tier == "existing" {
						step.Status = "in_progress"
					}
					opts, ops, run, claims := scopedHookFixture(t, "open", []beads.Bead{step})
					store := ops.WorkflowStore(context.Background(), "/rig", nil, "", "").(*beads.MemStore)
					seedHookNestedAncestry(t, store)
					opts.Env = []string{"GC_SESSION_ID=test-session"}
					opts.StartedFence = hookStartedFence{Token: "test-token"}
					ops.DrainPending = func(string) (bool, error) { return false, nil }
					ops.ResolveSessionWorkDir = func(string) string { return "" }
					ops.ResolveWorkBranch = func(hookClaimWorkTree) string { return "" }
					checks, failedReads := 0, 0
					reservations := []string{}
					ops.WorkflowStore = func(_ context.Context, _ string, _ []string, id, actor string) beads.Store {
						if id != step.ID {
							return store
						}
						fail := phase == "reservation" && actor == ""
						if actor != "" {
							checks++
							writerCheck := 3
							if tier == "existing" {
								writerCheck = 2
							}
							fail = (phase == "before claim" && checks == 1) ||
								(phase == "after CAS" && checks == 2) ||
								(phase == "final writer" && checks == writerCheck)
						}
						return &hookMissingAncestorStore{MemStore: store, missingID: missingID, fail: fail, failedReads: &failedReads, reservations: &reservations}
					}
					var sibling beads.Bead
					if strings.Contains(phase, "reservation") {
						var err error
						sibling, err = store.Create(scopedHookBead("next-a", "root-a", "open", ""))
						if err != nil {
							t.Fatal(err)
						}
						ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
							return []beads.Bead{sibling}, nil
						}
					}
					// All other phases have no siblings, isolating execution delivery
					// from the separate reservation ancestry gate.
					published := []string{}
					ops.RecordWorkStarted = func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
						published = append(published, "work started")
						return hookStartedRecorded, nil
					}
					ops.ForgetWorkStarted = func(string, hookStartedKey) error { return nil }
					ops.EmitClaimReleased = func(hookClaimReleaseRecord) {}
					ops.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error {
						published = append(published, "work stamp")
						return nil
					}
					ops.StampSessionClaim = func(string, string) error { published = append(published, "session stamp"); return nil }
					ops.PublishRunMap = func(string, string, ...string) error { published = append(published, "run map"); return nil }
					ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) { published = append(published, "step started") }
					ops.Release = func(_ context.Context, _ string, _ []string, id, _ string) (bool, error) {
						status, owner := "open", ""
						return true, store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &owner})
					}
					var out, errOut bytes.Buffer
					code := claimHookWorkWithRunner("unused generic query", "/rig", opts.Env, nil, opts, ops, run, nil, &out, &errOut)
					if phase == "live reservation" {
						got, err := store.Get(sibling.ID)
						if code != 0 || !strings.Contains(out.String(), `"action":"work"`) || err != nil || got.Assignee != "seat" || len(reservations) != 1 || reservations[0] != sibling.ID {
							t.Fatalf("live reservation: code=%d out=%s err=%s sibling=%+v readErr=%v writes=%v", code, &out, &errOut, got, err, reservations)
						}
						return
					}
					if code == 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "injected missing ancestor "+missingID) || failedReads != 1 {
						t.Fatalf("missing ancestry: code=%d out=%s err=%s failedReads=%d", code, &out, &errOut, failedReads)
					}
					if len(published) != 0 || len(reservations) != 0 {
						t.Fatalf("unresolved ancestry published execution %v or reserved siblings %v", published, reservations)
					}
					wantClaims := 1
					if tier == "existing" || phase == "before claim" {
						wantClaims = 0
					}
					if len(*claims) != wantClaims {
						t.Fatalf("claims=%v, want %d before failure", *claims, wantClaims)
					}
					if sibling.ID != "" {
						got, err := store.Get(sibling.ID)
						if err != nil || got.Assignee != "" || got.Revision != sibling.Revision {
							t.Fatalf("unresolved ancestry changed sibling: %+v err=%v", got, err)
						}
					}
				})
			}
		}
	}
}

func TestHookUnscopedMissingAncestryKeepsV1(t *testing.T) {
	for _, missingID := range []string{"root-a", "drain-a", "parent-a"} {
		t.Run(missingID, func(t *testing.T) {
			step := scopedHookBead("step-a", "root-a", "open", "")
			opts, ops, _, claims := scopedHookFixture(t, "open", []beads.Bead{step})
			store := ops.WorkflowStore(context.Background(), "/rig", nil, "", "").(*beads.MemStore)
			seedHookNestedAncestry(t, store)
			failedReads, reservations := 0, []string{}
			ops.WorkflowStore = func(context.Context, string, []string, string, string) beads.Store {
				return &hookMissingAncestorStore{MemStore: store, missingID: missingID, fail: true, failedReads: &failedReads, reservations: &reservations}
			}
			opts.RootBeadID = ""
			rows, err := json.Marshal([]beads.Bead{step})
			if err != nil {
				t.Fatal(err)
			}
			ops.Runner = func(string, string) (string, error) { return string(rows), nil }
			var out, errOut bytes.Buffer
			code := doHookClaim("query", "/rig", opts, ops, &out, &errOut)
			var got hookClaimJSONResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err, out.String(), errOut.String())
			}
			if code != 0 || got.SchemaVersion != "1" || got.Action != "work" || got.BeadID != step.ID || len(*claims) != 1 || failedReads == 0 {
				t.Fatalf("v1 changed: code=%d result=%+v err=%s claims=%v failedReads=%d", code, got, &errOut, *claims, failedReads)
			}
			validateJSONAgainstBuiltinSchema(t, []string{"hook"}, out.Bytes())
		})
	}
}

func TestHookRootOwnedAssignmentStoreFallback(t *testing.T) {
	for _, status := range []string{"open", "in_progress"} {
		for _, fault := range []error{beads.ErrNotFound, errors.New("owning store lookup unavailable")} {
			t.Run(status+"/"+fault.Error(), func(t *testing.T) {
				opts, ops, run, claims := scopedHookFixture(t, "open", []beads.Bead{scopedHookBead("step-a", "root-a", status, "seat")})
				read, claim := ops.ReadWorkMeta, ops.Claim
				ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, error) {
					if dir == "/first" {
						if id == "root-a" {
							return beads.Bead{}, beads.ErrNotFound
						}
						return beads.Bead{}, fault
					}
					return read(ctx, dir, env, id, actor)
				}
				ops.Claim = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
					if dir == "/first" {
						return beads.Bead{}, false, beads.ErrNotFound
					}
					return claim(ctx, dir, env, id, actor)
				}
				var deliveredFrom string
				ops.StampWorkMeta = func(_ context.Context, dir string, _ []string, _, _ string, _ map[string]string) error {
					deliveredFrom = dir
					return nil
				}
				class := newClaimRouteClassStore(t)
				classCopy := mintClaimRouteBead(t, class, "step-a", map[string]string{"gc.root_bead_id": "root-a"})
				ops = classRoutedHookClaimOps(ops, newClaimRouteFor(t, class))
				var out, errOut bytes.Buffer
				code := claimHookWorkWithRunner("unused generic query", "/first", nil, []hookStore{{dir: "/first"}, {dir: "/second"}}, opts, ops, run, nil, &out, &errOut)
				unchanged, err := class.Get(classCopy.ID)
				if err != nil || unchanged.Revision != classCopy.Revision || unchanged.Assignee != classCopy.Assignee {
					t.Fatalf("fallback changed a class-store copy: %+v err=%v", unchanged, err)
				}
				if !errors.Is(fault, beads.ErrNotFound) {
					if code == 0 || out.Len() != 0 || len(*claims) != 0 || deliveredFrom != "" {
						t.Fatalf("read failure became work: code=%d out=%s err=%s claims=%v store=%s", code, &out, &errOut, *claims, deliveredFrom)
					}
					return
				}
				var got hookRootJSONResult
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err, out.String(), errOut.String())
				}
				wantClaims := 0
				if status == "open" {
					wantClaims = 1
				}
				if code != 0 || got.Action != "work" || got.BeadID != "step-a" || len(*claims) != wantClaims || deliveredFrom != "/second" {
					t.Fatalf("code=%d result=%+v err=%s claims=%v store=%s", code, got, &errOut, *claims, deliveredFrom)
				}
			})
		}
	}
}

func TestHookRootReadinessGapKeepsScope(t *testing.T) {
	rows := []beads.Bead{scopedHookBead("step-a", "root-a", "open", ""), scopedHookBead("step-b", "root-b", "open", "")}
	opts, ops, _, claims := scopedHookFixture(t, "open", rows)
	for phase := 0; phase < 2; phase++ {
		run := func(command, _ string, _ []string) (string, error) {
			if strings.Contains(command, "--status in_progress") {
				return "[]", nil
			}
			visible := rows
			if phase == 0 {
				visible = rows[1:]
			}
			b, err := json.Marshal(visible)
			return string(b), err
		}
		var out, errOut bytes.Buffer
		code := claimHookWorkWithRunner("unused generic query", "/rig", nil, nil, opts, ops, run, nil, &out, &errOut)
		want := "wait"
		if phase == 1 {
			want = "work"
		}
		if code != 0 || !strings.Contains(out.String(), `"action":"`+want+`"`) {
			t.Fatalf("phase=%d code=%d out=%s err=%s", phase, code, &out, &errOut)
		}
	}
	if len(*claims) != 1 || (*claims)[0] != "step-a" {
		t.Fatalf("claims=%v", *claims)
	}
}

func TestHookRootSchemaVersions(t *testing.T) {
	raw, err := readBuiltinSchema([]string{"hook"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("hook-result.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("hook-result.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"schema_version":"1","ok":true,"command":"hook","action":"drain","reason":"no_work"}`, true},
		{`{"schema_version":"1","ok":true,"command":"hook","action":"work","reason":"claimed","bead_id":"a","assignee":"s"}`, true},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"wait","reason":"workflow_no_ready","root_bead_id":"r"}`, true},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"complete","reason":"workflow_complete","root_bead_id":"r"}`, true},
		{`{"schema_version":"1","ok":true,"command":"hook","action":"wait","reason":"workflow_no_ready","root_bead_id":"r"}`, false},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"drain","reason":"no_work","root_bead_id":"r"}`, false},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"wait","reason":"workflow_no_ready"}`, false},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"wait","reason":"workflow_no_ready","root_bead_id":"r","drain_acknowledged":true}`, false},
		{`{"schema_version":"2","ok":true,"command":"hook","action":"wait","reason":"workflow_no_ready","root_bead_id":"r","bead_id":"a"}`, false},
	} {
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.data))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); (err == nil) != tc.valid {
			t.Errorf("valid=%v err=%v data=%s", tc.valid, err, tc.data)
		}
	}
}

func TestHookRootFlagValidation(t *testing.T) {
	for _, args := range [][]string{{"--root", "root-a"}, {"--claim", "--root", "root-a", "--drain-ack"}, {"--claim", "--root", " "}} {
		var out, errOut bytes.Buffer
		cmd := newHookCmd(&out, &errOut)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted invalid flags %v", args)
		}
		if out.Len() != 0 {
			t.Fatalf("invalid flags produced a result: %s", &out)
		}
	}
}
