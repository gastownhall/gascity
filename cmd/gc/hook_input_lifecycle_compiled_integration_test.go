//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// This fixture compiles and materializes real edges and retry/check controls.
// Persistent SQLite graph storage is separate from persistent FileStore input.
// The worker query is the store's actual ready projection. Every hook seam is
// private; no default read or mutation can reach a live store.
func TestInputLifecycleCompiledSettlementToNotice(t *testing.T) {
	for _, scenario := range []string{"success", "finalize-fails", "cancel", "failed-cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = 'fixture'\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			verified := filepath.Join(dir, "report-verified")
			if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte(fmt.Sprintf("#!/bin/sh\ntest -f %q\n", verified)), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `formula = "lifecycle"
input_lifecycle = "graph"
[requires]
formula_compiler = ">=2.1.0"
[[steps]]
id = "settle"
title = "Settle sources"
metadata = { "fixture.stage" = "settle", "gc.run_target" = "worker", "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }
[[steps]]
id = "finalize-local"
title = "Finalize local result"
needs = ["settle"]
metadata = { "fixture.stage" = "finalize", "gc.input_convoy_id" = "{{convoy_id}}", "gc.run_target" = "worker", "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }
[[steps]]
id = "body"
title = "Body"
needs = ["finalize-local"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "body" }
[[steps]]
id = "report"
title = "Checked report"
needs = ["body"]
metadata = { "fixture.stage" = "report", "gc.run_target" = "worker", "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }
[steps.check]
max_attempts = 2
[steps.check.check]
mode = "exec"
path = "check.sh"
timeout = "10s"
[[steps]]
id = "notice"
title = "Dependent notice"
needs = ["report"]
metadata = { "fixture.stage" = "notice", "gc.run_target" = "worker", "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }
`
			if err := os.WriteFile(filepath.Join(dir, "lifecycle.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, "work.json"))
			if err != nil {
				t.Fatal(err)
			}
			graph, err := beads.OpenSQLiteStore(filepath.Join(dir, "graph"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := closeBeadStoreHandle(graph); err != nil {
					t.Error(err)
				}
			})
			source, err := work.Create(beads.Bead{Title: "source"})
			if err != nil {
				t.Fatal(err)
			}
			input, err := work.Create(beads.Bead{Title: "input", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			if err := convoy.TrackItemIn(convoy.MemberClasses{Convoy: beadstest.Work(work)}, input.ID, source.ID); err != nil {
				t.Fatal(err)
			}
			result, err := molecule.Cook(context.Background(), beadstest.GraphMolecule(graph), "lifecycle", []string{dir}, molecule.Options{Vars: map[string]string{"convoy_id": input.ID}, RootMetadata: map[string]string{"gc.input_convoy_id": input.ID, "gc.root_store_ref": "rig:fixture", "gc.work_dir": dir}})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := molecule.ListSubtree(beadstest.GraphMolecule(graph), result.RootID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range initial {
				if row.Metadata["fixture.stage"] != "" && !beadmeta.IsControlKind(row.Metadata["gc.kind"]) {
					if err := graph.SetMetadata(row.ID, "gc.routed_to", "worker"); err != nil {
						t.Fatal(err)
					}
				}
			}
			f := newHookWorkflowStopFixture(t)
			f.store = graph
			ops := f.ops()
			ops.InputVerdict = func(ctx context.Context, _ string, _ []string, b beads.Bead, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
				return workflowInputDoneAcross(ctx, graph, beadstest.Work(work), b, ids, prove)
			}
			ops.RetireWorkflow = func(ctx context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
				if verdict.State == workflowInputPurged {
					return failPurgedWorkflow(ctx, graph, verdict.RootID)
				}
				return skipDoneWorkflow(graph, verdict.RootID)
			}
			ops.Runner = func(string, string) (string, error) {
				ready, err := graph.Ready()
				if err != nil {
					return "", err
				}
				var rows []beads.Bead
				for _, row := range ready {
					if row.ID != result.RootID && row.Metadata["fixture.stage"] != "" && !beadmeta.IsControlKind(row.Metadata["gc.kind"]) {
						rows = append(rows, row)
					}
				}
				data, err := json.Marshal(rows)
				return string(data), err
			}
			reports, notices, finalizes := 0, 0, 0
			for round := 0; round < 100; round++ {
				ready, err := graph.Ready()
				if err != nil {
					t.Fatal(err)
				}
				progress := false
				for _, row := range ready {
					if row.ID == result.RootID || !beadmeta.IsControlKind(row.Metadata["gc.kind"]) {
						continue
					}
					processed, err := dispatch.ProcessControl(beadstest.Graph(graph), row, dispatch.ProcessOptions{Context: t.Context(), CityPath: dir, StorePath: dir, FormulaSearchPaths: []string{dir}, Work: beadstest.Work(work), MemberTail: true})
					if errors.Is(err, dispatch.ErrControlPending) {
						continue
					}
					if err != nil {
						t.Fatalf("control %s kind=%s: %v", row.ID, row.Metadata["gc.kind"], err)
					}
					if processed.Processed {
						t.Logf("control %s %s", row.ID, processed.Action)
						progress = true
						break
					}
				}
				if progress {
					continue
				}
				var stdout, stderr bytes.Buffer
				code := doHookClaim("fixture", dir, hookClaimOptions{Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true, DrainAck: true}, ops, &stdout, &stderr)
				if code != 0 {
					t.Fatalf("hook code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
				}
				var claim hookClaimJSONResult
				if err := json.Unmarshal(stdout.Bytes(), &claim); err != nil {
					t.Fatal(err)
				}
				if claim.BeadID == "" {
					t.Logf("hook drained: %s", stderr.String())
					// Retirement can release teardown that was blocked in this
					// query snapshot. The next poll must use fresh readiness.
					next, err := graph.Ready()
					if err != nil {
						t.Fatal(err)
					}
					if len(next) > 0 {
						continue
					}
					break
				}
				row, err := graph.Get(claim.BeadID)
				if err != nil {
					t.Fatal(err)
				}
				stage := row.Metadata["fixture.stage"]
				t.Logf("claim %s stage=%s attempt=%s", row.ID, stage, row.Metadata["gc.attempt"])
				meta := map[string]string{"gc.outcome": "pass"}
				switch stage {
				case "settle":
					if err := work.Close(source.ID); err != nil {
						t.Fatal(err)
					}
				case "finalize":
					finalizes++
					if strings.Contains(scenario, "cancel") {
						t.Fatal("stopped ordinary finalization claimed")
					}
					if scenario == "finalize-fails" {
						meta = map[string]string{"gc.outcome": "fail", "gc.failure_class": "hard", "gc.failure_reason": "fixture_finalize_failed"}
					}
				case "report":
					reports++
					root, err := graph.Get(result.RootID)
					if err != nil || root.Status != "closed" {
						t.Fatalf("report before settlement: %+v %v", root, err)
					}
					if reports == 2 {
						if err := os.WriteFile(verified, []byte("checked report\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				case "notice":
					notices++
					if reports != 2 {
						t.Fatalf("notice before checked report retries: reports=%d", reports)
					}
					if _, err := os.Stat(verified); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "notice"), []byte("checked report complete\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown worker stage %q", stage)
				}
				if _, err := graph.CloseAll([]string{row.ID}, meta); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(scenario, "cancel") && stage == "settle" {
					if err := graph.SetMetadata(result.RootID, "gc.cancel_requested", "true"); err != nil {
						t.Fatal(err)
					}
					exclude, err := molecule.TeardownTailExclusion(beadstest.GraphMolecule(graph), result.RootID)
					if err != nil {
						t.Fatal(err)
					}
					cancelStore := beads.Store(graph)
					if scenario == "failed-cancel" {
						cancelStore = &inputLifecycleCancelFailure{Store: graph, failures: 1}
					}
					_, err = sourceworkflow.CloseWorkflowSubtreeAsExcept(beadstest.GraphMolecule(cancelStore), result.RootID, "canceled", "fixture cancel", map[string]string{"gc.cancel_requested": "true"}, exclude)
					if (err != nil) != (scenario == "failed-cancel") {
						t.Fatalf("first cancel err=%v", err)
					}
				}
			}
			want := "pass"
			if scenario == "finalize-fails" {
				want = "fail"
			}
			if strings.Contains(scenario, "cancel") {
				want = "canceled"
			}
			root, err := graph.Get(result.RootID)
			if err != nil {
				t.Fatal(err)
			}
			if root.Status != "closed" || root.Metadata["gc.outcome"] != want || reports != 2 || notices != 1 {
				t.Fatalf("root=%s/%s want=%s report attempts=%d notice=%d finalize=%d", root.Status, root.Metadata["gc.outcome"], want, reports, notices, finalizes)
			}
			if (scenario == "success" || scenario == "finalize-fails") && finalizes != 1 {
				t.Fatal("ordinary finalize did not execute exactly once")
			}
			members, err := molecule.ListSubtree(beadstest.GraphMolecule(graph), result.RootID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range members {
				if row.Status != "closed" && !strings.EqualFold(row.Metadata["gc.kind"], "spec") {
					t.Errorf("residue %s kind=%s status=%s", row.ID, row.Metadata["gc.kind"], row.Status)
				}
			}
		})
	}
}
