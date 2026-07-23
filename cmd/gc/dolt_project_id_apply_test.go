package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

type projectIdentityL3Double struct {
	projectID string
	seedErr   error
}

func (d *projectIdentityL3Double) read(context.Context) (string, bool, error) {
	if d.projectID == "" {
		return "", false, nil
	}
	return d.projectID, true, nil
}

func (d *projectIdentityL3Double) seed(_ context.Context, projectID string) (bool, error) {
	if d.seedErr != nil {
		return false, d.seedErr
	}
	if d.projectID != "" {
		if d.projectID != projectID {
			return false, fmt.Errorf("database _project_id %q does not match desired %q", d.projectID, projectID)
		}
		return false, nil
	}
	d.projectID = projectID
	return true, nil
}

func TestProjectIdentityL3DoubleConformsToSeedContract(t *testing.T) {
	l3 := &projectIdentityL3Double{}
	runProjectIdentityL3SeedContract(t, l3.read, l3.seed)
}

func TestApplyReconcileDecisionActions(t *testing.T) {
	type layerState struct {
		l1 string
		l2 string
		l3 string
	}
	tests := []struct {
		name           string
		decision       reconcileDecision
		initial        layerState
		want           layerState
		wantReport     managedDoltProjectIDReport
		wantError      string
		wantEvents     []projectIdentityApplyStampedEvent
		wantNoMutation bool
	}{
		{
			name: "no_op",
			decision: reconcileDecision{
				Action:     actionNoOp,
				ResolvedID: "canonical-id",
				Source:     "match",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID: "canonical-id",
				Source:    "match",
				Layer:     "l1",
			},
			wantNoMutation: true,
		},
		{
			name: "refuse_l1_l3_mismatch",
			decision: reconcileDecision{
				Action: actionRefuseL1L3Mismatch,
				L1ID:   "identity-id",
				L2ID:   "identity-id",
				L3ID:   "database-id",
			},
			initial: layerState{l1: "identity-id", l2: "identity-id", l3: "database-id"},
			want:    layerState{l1: "identity-id", l2: "identity-id", l3: "database-id"},
			wantError: "PROJECT IDENTITY MISMATCH — refusing to connect:\n" +
				"  canonical .beads/identity.toml#project.id = \"identity-id\"\n" +
				"  database metadata._project_id              = \"database-id\"\n" +
				"\n" +
				"The git-tracked identity does not match the database stamp. The database may belong to a different rig, or the identity file may have been changed without re-stamping the database. Inspect both values and resolve manually before reconnecting.",
			wantNoMutation: true,
		},
		{
			name: "repair_l2",
			decision: reconcileDecision{
				Action:     actionRepairL2,
				ResolvedID: "canonical-id",
				L1ID:       "canonical-id",
				L2ID:       "wrong-l2",
				L3ID:       "canonical-id",
				Source:     "l2-repair",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id", l2: "wrong-l2", l3: "canonical-id"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:       "canonical-id",
				MetadataUpdated: true,
				Source:          "l2-repair",
				Layer:           "l1",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L2", oldID: "wrong-l2", newID: "canonical-id"},
			},
		},
		{
			name: "seed_l3",
			decision: reconcileDecision{
				Action:     actionSeedL3,
				ResolvedID: "canonical-id",
				L1ID:       "canonical-id",
				L2ID:       "canonical-id",
				Source:     "l3-seed",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id", l2: "canonical-id"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:       "canonical-id",
				DatabaseUpdated: true,
				Source:          "l3-seed",
				Layer:           "l1",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L3", newID: "canonical-id"},
			},
		},
		{
			name: "repair_l2_and_seed_l3",
			decision: reconcileDecision{
				Action:     actionRepairL2SeedL3,
				ResolvedID: "canonical-id",
				L1ID:       "canonical-id",
				L2ID:       "wrong-l2",
				Source:     "l2-repair-l3-seed",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id", l2: "wrong-l2"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:       "canonical-id",
				MetadataUpdated: true,
				DatabaseUpdated: true,
				Source:          "l2-repair-l3-seed",
				Layer:           "l1",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L2", oldID: "wrong-l2", newID: "canonical-id"},
				{source: "cache_repair", layer: "L3", newID: "canonical-id"},
			},
		},
		{
			name: "seed_l2",
			decision: reconcileDecision{
				Action:     actionSeedL2,
				ResolvedID: "canonical-id",
				L1ID:       "canonical-id",
				L3ID:       "canonical-id",
				Source:     "l2-seed",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id", l3: "canonical-id"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:       "canonical-id",
				MetadataUpdated: true,
				Source:          "l2-seed",
				Layer:           "l1",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L2", newID: "canonical-id"},
			},
		},
		{
			name: "seed_l2_and_l3",
			decision: reconcileDecision{
				Action:     actionSeedL2L3,
				ResolvedID: "canonical-id",
				L1ID:       "canonical-id",
				Source:     "l2-l3-seed",
				Layer:      "l1",
			},
			initial: layerState{l1: "canonical-id"},
			want:    layerState{l1: "canonical-id", l2: "canonical-id", l3: "canonical-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:       "canonical-id",
				MetadataUpdated: true,
				DatabaseUpdated: true,
				Source:          "l2-l3-seed",
				Layer:           "l1",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "cache_repair", layer: "L2", newID: "canonical-id"},
				{source: "cache_repair", layer: "L3", newID: "canonical-id"},
			},
		},
		{
			name: "migrate_l1_from_l2",
			decision: reconcileDecision{
				Action:     actionMigrateFromL2,
				ResolvedID: "legacy-id",
				L2ID:       "legacy-id",
				L3ID:       "legacy-id",
				Source:     "l1-migrate-from-l2",
				Layer:      "l2",
			},
			initial: layerState{l2: "legacy-id", l3: "legacy-id"},
			want:    layerState{l1: "legacy-id", l2: "legacy-id", l3: "legacy-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:           "legacy-id",
				IdentityFileUpdated: true,
				Source:              "l1-migrate-from-l2",
				Layer:               "l2",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "migrated_from_metadata", layer: "L1", newID: "legacy-id"},
			},
		},
		{
			name: "refuse_legacy_l2_l3_mismatch",
			decision: reconcileDecision{
				Action: actionRefuseLegacyMismatch,
				L2ID:   "metadata-id",
				L3ID:   "database-id",
			},
			initial: layerState{l2: "metadata-id", l3: "database-id"},
			want:    layerState{l2: "metadata-id", l3: "database-id"},
			wantError: "LEGACY PROJECT IDENTITY MISMATCH — refusing to connect:\n" +
				"  metadata.json project_id      = \"metadata-id\"\n" +
				"  database metadata._project_id  = \"database-id\"\n" +
				"\n" +
				"This rig predates the canonical .beads/identity.toml file. The two legacy storage layers disagree, so we cannot safely seed the canonical layer from either one. Inspect both values and decide which is correct, then create .beads/identity.toml with the chosen value to unblock reconcile.",
			wantNoMutation: true,
		},
		{
			name: "migrate_l1_and_seed_l3",
			decision: reconcileDecision{
				Action:     actionMigrateL1SeedL3,
				ResolvedID: "metadata-id",
				L2ID:       "metadata-id",
				Source:     "l1-migrate-l3-seed",
				Layer:      "l2",
			},
			initial: layerState{l2: "metadata-id"},
			want:    layerState{l1: "metadata-id", l2: "metadata-id", l3: "metadata-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:           "metadata-id",
				DatabaseUpdated:     true,
				IdentityFileUpdated: true,
				Source:              "l1-migrate-l3-seed",
				Layer:               "l2",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "migrated_from_metadata", layer: "L1", newID: "metadata-id"},
				{source: "migrated_from_metadata", layer: "L3", newID: "metadata-id"},
			},
		},
		{
			name: "adopt_l1_and_l2_from_l3",
			decision: reconcileDecision{
				Action:     actionAdoptFromL3SeedL2,
				ResolvedID: "database-id",
				L3ID:       "database-id",
				Source:     "l1-adopt-l2-seed",
				Layer:      "l3",
			},
			initial: layerState{l3: "database-id"},
			want:    layerState{l1: "database-id", l2: "database-id", l3: "database-id"},
			wantReport: managedDoltProjectIDReport{
				ProjectID:           "database-id",
				MetadataUpdated:     true,
				IdentityFileUpdated: true,
				Source:              "l1-adopt-l2-seed",
				Layer:               "l3",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "migrated_from_database", layer: "L1", newID: "database-id"},
				{source: "migrated_from_database", layer: "L2", newID: "database-id"},
			},
		},
		{
			name: "generate_all_layers",
			decision: reconcileDecision{
				Action: actionGenerate,
				Source: "generated",
				Layer:  "generated",
			},
			wantReport: managedDoltProjectIDReport{
				MetadataUpdated:     true,
				DatabaseUpdated:     true,
				IdentityFileUpdated: true,
				Source:              "generated",
				Layer:               "generated",
			},
			wantEvents: []projectIdentityApplyStampedEvent{
				{source: "generated", layer: "L1"},
				{source: "generated", layer: "L2"},
				{source: "generated", layer: "L3"},
			},
		},
	}

	if len(tests) != int(actionGenerate)+1 {
		t.Fatalf("action cases = %d, want %d", len(tests), int(actionGenerate)+1)
	}
	seen := make(map[reconcileAction]struct{}, len(tests))
	for _, tc := range tests {
		if _, duplicate := seen[tc.decision.Action]; duplicate {
			t.Fatalf("duplicate action case for %d", tc.decision.Action)
		}
		seen[tc.decision.Action] = struct{}{}
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeRoot := filepath.Join(cityDir, "rigs", "demo")
			metadataPath := writeProjectIDMetadataFile(t, scopeRoot, tc.initial.l2)
			if tc.initial.l1 != "" {
				if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, tc.initial.l1); err != nil {
					t.Fatalf("WriteProjectIdentity: %v", err)
				}
			}
			l3 := &projectIdentityL3Double{projectID: tc.initial.l3}
			recorder := &projectIdentityApplyRecordingRecorder{}
			beforeIdentity := readOptionalProjectIdentityFile(t, scopeRoot)
			beforeMetadata := mustReadFile(t, metadataPath)

			report, err := applyReconcileDecision(
				context.Background(),
				fsys.OSFS{},
				scopeRoot,
				metadataPath,
				tc.decision,
				cityDir,
				recorder,
				l3.seed,
			)

			if tc.wantError != "" {
				if err == nil {
					t.Fatal("applyReconcileDecision unexpectedly succeeded")
				}
				if err.Error() != tc.wantError {
					t.Fatalf("error = %q, want %q", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatalf("applyReconcileDecision: %v", err)
			}

			wantReport := tc.wantReport
			wantState := tc.want
			wantEvents := append([]projectIdentityApplyStampedEvent(nil), tc.wantEvents...)
			if tc.decision.Action == actionGenerate {
				assertGeneratedProjectID(t, report.ProjectID)
				wantReport.ProjectID = report.ProjectID
				wantState = layerState{l1: report.ProjectID, l2: report.ProjectID, l3: report.ProjectID}
				for i := range wantEvents {
					wantEvents[i].newID = report.ProjectID
				}
			}
			if report != wantReport {
				t.Fatalf("report = %+v, want %+v", report, wantReport)
			}

			gotL1, gotL2, gotL3 := readProjectIdentityApplyState(t, scopeRoot, metadataPath, l3)
			gotState := layerState{l1: gotL1, l2: gotL2, l3: gotL3}
			if gotState != wantState {
				t.Fatalf("final state = %+v, want %+v", gotState, wantState)
			}
			assertProjectIdentityApplyStampedEvents(t, recorder.records, wantEvents)

			if tc.wantNoMutation {
				afterIdentity := readOptionalProjectIdentityFile(t, scopeRoot)
				afterMetadata := mustReadFile(t, metadataPath)
				if !bytes.Equal(afterIdentity, beforeIdentity) {
					t.Fatalf("identity file mutated for %s:\nbefore: %q\nafter:  %q", tc.name, beforeIdentity, afterIdentity)
				}
				if !bytes.Equal(afterMetadata, beforeMetadata) {
					t.Fatalf("metadata file mutated for %s:\nbefore: %q\nafter:  %q", tc.name, beforeMetadata, afterMetadata)
				}
				if l3.projectID != tc.initial.l3 {
					t.Fatalf("L3 mutated for %s: got %q, want %q", tc.name, l3.projectID, tc.initial.l3)
				}
			}
		})
	}
}

func TestApplyReconcileDecisionPreservesPartialSuccessWhenL3SeedFails(t *testing.T) {
	cityDir := t.TempDir()
	scopeRoot := filepath.Join(cityDir, "rigs", "demo")
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "wrong-l2")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	l3 := &projectIdentityL3Double{seedErr: errors.New("seed L3 unavailable")}
	recorder := &projectIdentityApplyRecordingRecorder{}

	report, err := applyReconcileDecision(
		context.Background(),
		fsys.OSFS{},
		scopeRoot,
		metadataPath,
		reconcileDecision{
			Action:     actionRepairL2SeedL3,
			ResolvedID: "canonical-id",
			L1ID:       "canonical-id",
			L2ID:       "wrong-l2",
			Source:     "l2-repair-l3-seed",
			Layer:      "l1",
		},
		cityDir,
		recorder,
		l3.seed,
	)
	if err == nil || err.Error() != "seed L3 unavailable" {
		t.Fatalf("error = %v, want seed L3 unavailable", err)
	}
	if report != (managedDoltProjectIDReport{}) {
		t.Fatalf("report = %+v, want zero report", report)
	}
	l1, l2, l3ID := readProjectIdentityApplyState(t, scopeRoot, metadataPath, l3)
	if l1 != "canonical-id" || l2 != "canonical-id" || l3ID != "" {
		t.Fatalf("partial state = (L1:%q L2:%q L3:%q), want canonical-id/canonical-id/absent", l1, l2, l3ID)
	}
	assertProjectIdentityApplyStampedEvents(t, recorder.records, []projectIdentityApplyStampedEvent{
		{source: "cache_repair", layer: "L2", oldID: "wrong-l2", newID: "canonical-id"},
	})
}

func runProjectIdentityL3SeedContract(
	t *testing.T,
	read func(context.Context) (string, bool, error),
	seed func(context.Context, string) (bool, error),
) {
	t.Helper()
	ctx := context.Background()

	projectID, ok, err := read(ctx)
	if err != nil {
		t.Fatalf("read absent L3: %v", err)
	}
	if ok || projectID != "" {
		t.Fatalf("absent L3 = (%q, %v), want (\"\", false)", projectID, ok)
	}

	updated, err := seed(ctx, "contract-id")
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if !updated {
		t.Fatal("first seed updated = false, want true")
	}
	assertProjectIdentityL3Read(ctx, t, read, "contract-id")

	updated, err = seed(ctx, "contract-id")
	if err != nil {
		t.Fatalf("same-ID seed: %v", err)
	}
	if updated {
		t.Fatal("same-ID seed updated = true, want false")
	}
	assertProjectIdentityL3Read(ctx, t, read, "contract-id")

	updated, err = seed(ctx, "different-id")
	if err == nil {
		t.Fatal("different-ID seed unexpectedly succeeded")
	}
	if updated {
		t.Fatal("different-ID seed updated = true, want false")
	}
	wantError := `database _project_id "contract-id" does not match desired "different-id"`
	if err.Error() != wantError {
		t.Fatalf("different-ID seed error = %q, want %q", err, wantError)
	}
	assertProjectIdentityL3Read(ctx, t, read, "contract-id")
}

func assertProjectIdentityL3Read(ctx context.Context, t *testing.T, read func(context.Context) (string, bool, error), want string) {
	t.Helper()
	projectID, ok, err := read(ctx)
	if err != nil {
		t.Fatalf("read L3: %v", err)
	}
	if !ok || projectID != want {
		t.Fatalf("L3 = (%q, %v), want (%q, true)", projectID, ok, want)
	}
}

func readProjectIdentityApplyState(t *testing.T, scopeRoot, metadataPath string, l3 *projectIdentityL3Double) (string, string, string) {
	t.Helper()
	l1, l1OK, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	if !l1OK {
		l1 = ""
	}
	l2, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readManagedMetadataProjectID: %v", err)
	}
	l3ID, l3OK, err := l3.read(context.Background())
	if err != nil {
		t.Fatalf("read L3 double: %v", err)
	}
	if !l3OK {
		l3ID = ""
	}
	return l1, l2, l3ID
}

func readOptionalProjectIdentityFile(t *testing.T, scopeRoot string) []byte {
	t.Helper()
	data, err := os.ReadFile(contract.ProjectIdentityPath(scopeRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadFile(identity): %v", err)
	}
	return data
}

func assertGeneratedProjectID(t *testing.T, projectID string) {
	t.Helper()
	const prefix = "gc-local-"
	if !strings.HasPrefix(projectID, prefix) {
		t.Fatalf("generated project ID = %q, want %s<32 hex digits>", projectID, prefix)
	}
	encoded := strings.TrimPrefix(projectID, prefix)
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("generated project ID = %q, want %s<32 hex digits>", projectID, prefix)
	}
}

type projectIdentityApplyRecordingRecorder struct {
	records []events.Event
}

func (r *projectIdentityApplyRecordingRecorder) Record(event events.Event) {
	r.records = append(r.records, event)
}

type projectIdentityApplyStampedEvent struct {
	source string
	layer  string
	oldID  string
	newID  string
}

func assertProjectIdentityApplyStampedEvents(t *testing.T, records []events.Event, want []projectIdentityApplyStampedEvent) {
	t.Helper()
	if len(records) != len(want) {
		t.Fatalf("recorded %d event(s), want %d: %+v", len(records), len(want), records)
	}
	for i, expected := range want {
		record := records[i]
		if record.Seq != 0 || !record.Ts.IsZero() || record.Type != events.ProjectIdentityStamped || record.Actor != "gc dolt-state ensure-project-id" || record.Subject != "rigs/demo" || record.Message != "" || record.RunID != "" || record.SessionID != "" || record.StepID != "" {
			t.Fatalf("record[%d] envelope = %+v, want exact project.identity.stamped envelope for rigs/demo", i, record)
		}
		wantPayload := gcapi.ProjectIdentityStampedPayload{
			ScopeRoot: "rigs/demo",
			Source:    expected.source,
			Layer:     expected.layer,
			OldID:     expected.oldID,
			NewID:     expected.newID,
		}
		encoded, err := json.Marshal(wantPayload)
		if err != nil {
			t.Fatalf("marshal expected payload[%d]: %v", i, err)
		}
		if !bytes.Equal(record.Payload, encoded) {
			t.Fatalf("record[%d].Payload = %s, want %s", i, record.Payload, encoded)
		}
	}
}
