package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestContinuationPreassignmentLeavesHumanSiblingsUnassigned(t *testing.T) {
	for _, identity := range []struct{ name, sessionID, want string }{
		{"session-id", "session-1", "session-1"},
		{"claim-assignee-fallback", "", "worker-1"},
	} {
		t.Run(identity.name, func(t *testing.T) {
			claimed := humanContinuationRow("claimed-step", nil)
			claimed.Status, claimed.Assignee = "in_progress", "worker-1"
			rows := []beads.Bead{claimed}
			wantOwners := map[string]string{claimed.ID: claimed.Assignee}
			var wantAssigned []string
			for _, tc := range []struct {
				id       string
				labels   []string
				eligible bool
			}{
				{id: "human", labels: []string{beadmeta.HumanLabel}},
				{id: "human-after-label", labels: []string{"ready", beadmeta.HumanLabel}},
				{id: "clean", eligible: true},
				{id: "mixed-human", labels: []string{"Human"}, eligible: true},
				{id: "upper-human", labels: []string{"HUMAN"}, eligible: true},
				{id: "human-like", labels: []string{"human-review"}, eligible: true},
				{id: "hold-mayor", labels: []string{beadmeta.HoldMayorLabel}},
				{id: "hold-external", labels: []string{beadmeta.HoldExternalLabel}},
				{id: "hold-mayor-case", labels: []string{" Hold:Mayor "}},
				{id: "hold-external-case", labels: []string{"HOLD:EXTERNAL"}},
				{id: "unrelated-hold", labels: []string{"hold:review"}, eligible: true},
			} {
				rows = append(rows, humanContinuationRow(tc.id, tc.labels))
				wantOwners[tc.id] = ""
				if tc.eligible {
					wantOwners[tc.id] = identity.want
					wantAssigned = append(wantAssigned, tc.id)
				}
			}
			for _, id := range []string{"other-root", "other-group", "other-route", "already-owned", "closed"} {
				row := humanContinuationRow(id, nil)
				switch id {
				case "other-root":
					row.Metadata[beadmeta.RootBeadIDMetadataKey] = "another-root"
				case "other-group":
					row.Metadata[beadmeta.ContinuationGroupMetadataKey] = "another-group"
				case "other-route":
					row.Metadata[beadmeta.RoutedToMetadataKey] = "another-worker"
				case "already-owned":
					row.Assignee = "existing-owner"
					row.Labels = []string{beadmeta.HumanLabel}
				case "closed":
					row.Status = "closed"
				}
				rows = append(rows, row)
				wantOwners[id] = row.Assignee
			}
			store := beads.NewMemStoreFrom(len(rows), rows, nil)
			var mutations []string
			ops := hookClaimOps{
				ListContinuation: func(_ context.Context, _ string, _ []string, root, group string) ([]beads.Bead, error) {
					if root != "workflow-root" || group != "body" {
						t.Fatalf("continuation scope = %q/%q, want workflow-root/body", root, group)
					}
					found, err := store.List(beads.ListQuery{Status: "open", Metadata: map[string]string{
						beadmeta.RootBeadIDMetadataKey: root, beadmeta.ContinuationGroupMetadataKey: group,
					}})
					// A stale self/closed snapshot and an invalid ID still must not
					// reach the assignment mutation, even if a reader returns them.
					self := humanContinuationRow(claimed.ID, nil)
					closed, getErr := store.Get("closed")
					if getErr != nil {
						t.Fatal(getErr)
					}
					return append(found, self, closed, humanContinuationRow("", nil)), err
				},
				AssignContinuation: func(_ context.Context, _ string, _ []string, id, owner string) error {
					mutations = append(mutations, id)
					return store.Update(id, beads.UpdateOpts{Assignee: &owner})
				},
			}
			assigned, err := preassignHookContinuationGroup(claimed, hookClaimOptions{
				Assignee: claimed.Assignee, SessionID: identity.sessionID, RouteTargets: []string{"worker"},
			}, ops, "/work")
			if err != nil {
				t.Fatalf("preassignment: %v", err)
			}
			slices.Sort(wantAssigned)
			slices.Sort(assigned)
			slices.Sort(mutations)
			if !slices.Equal(mutations, wantAssigned) {
				t.Errorf("assignment mutations = %v, want %v; human siblings must never reach the write", mutations, wantAssigned)
			}
			if !slices.Equal(assigned, wantAssigned) {
				t.Errorf("reported assignments = %v, want %v", assigned, wantAssigned)
			}
			for _, before := range rows {
				after, err := store.Get(before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Assignee != wantOwners[before.ID] {
					t.Errorf("%s persisted owner = %q, want %q", before.ID, after.Assignee, wantOwners[before.ID])
				}
				if after.Status != before.Status || !maps.Equal(after.Metadata, before.Metadata) {
					t.Errorf("%s status/metadata changed during preassignment: %+v", before.ID, after)
				}
			}
			all, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
			if err != nil || len(all) != len(rows) {
				t.Errorf("store population = %d, err=%v, want %d unchanged rows and no formula attachments", len(all), err, len(rows))
			}
		})
	}
}

func TestHumanContinuationPreassignmentPreservesMissingScopeAndErrors(t *testing.T) {
	for _, key := range []string{beadmeta.RootBeadIDMetadataKey, beadmeta.ContinuationGroupMetadataKey} {
		t.Run("missing-"+key, func(t *testing.T) {
			claimed := humanContinuationRow("claimed-step", nil)
			delete(claimed.Metadata, key)
			got, err := preassignHookContinuationGroup(claimed, hookClaimOptions{}, hookClaimOps{
				ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
					t.Error("listed siblings without a continuation scope")
					return nil, nil
				},
			}, "/work")
			if err != nil || len(got) != 0 {
				t.Errorf("missing scope = %v, %v, want no assignments/error", got, err)
			}
		})
	}
	for _, stage := range []string{"list", "assign"} {
		t.Run(stage+"-error", func(t *testing.T) {
			fault := errors.New("store unavailable")
			var attempts []string
			ops := hookClaimOps{
				ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
					if stage == "list" {
						return nil, fault
					}
					return []beads.Bead{humanContinuationRow("clean-1", nil), humanContinuationRow("clean-2", nil)}, nil
				},
				AssignContinuation: func(_ context.Context, _ string, _ []string, id, _ string) error {
					attempts = append(attempts, id)
					return fault
				},
			}
			got, err := preassignHookContinuationGroup(humanContinuationRow("claimed-step", nil), hookClaimOptions{
				Assignee: "worker-1", RouteTargets: []string{"worker"},
			}, ops, "/work")
			if !errors.Is(err, fault) || len(got) != 0 {
				t.Fatalf("error result = %v, %v, want original fault and no successful assignments", got, err)
			}
			if stage == "list" && len(attempts) != 0 {
				t.Errorf("writes after list failure: %v", attempts)
			}
			if stage == "assign" && (!reflect.DeepEqual(attempts, []string{"clean-1"}) || !strings.Contains(err.Error(), "clean-1")) {
				t.Errorf("assign failure lost context or continued: attempts=%v err=%v", attempts, err)
			}
		})
	}
}

func humanContinuationRow(id string, labels []string) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: "task", Status: "open", Labels: labels, Metadata: map[string]string{
		beadmeta.KindMetadataKey:              "task",
		beadmeta.RoutedToMetadataKey:          "worker",
		beadmeta.RootBeadIDMetadataKey:        "workflow-root",
		beadmeta.ContinuationGroupMetadataKey: "body",
	}}
}
