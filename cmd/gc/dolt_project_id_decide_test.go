package main

import "testing"

func TestReconcileDecisionTable(t *testing.T) {
	cases := []struct {
		name         string
		l1           string
		l1ok         bool
		l2           string
		l2ok         bool
		l3           string
		l3ok         bool
		wantAction   reconcileAction
		wantResolved string
		wantSource   string
		wantLayer    string
		wantWriteL1  bool
		wantWriteL2  bool
		wantWriteL3  bool
	}{
		{
			name:         "R1_L1L2L3_AllMatch_NoOp",
			l1:           "id-a",
			l1ok:         true,
			l2:           "id-a",
			l2ok:         true,
			l3:           "id-a",
			l3ok:         true,
			wantAction:   actionNoOp,
			wantResolved: "id-a",
			wantSource:   "match",
			wantLayer:    "l1",
		},
		{
			name:       "R2_L1L2L3_L1eqL2_L3Differs_RefuseL1L3",
			l1:         "id-a",
			l1ok:       true,
			l2:         "id-a",
			l2ok:       true,
			l3:         "id-b",
			l3ok:       true,
			wantAction: actionRefuseL1L3Mismatch,
		},
		{
			name:         "R3_L1L2L3_L2Wrong_L1eqL3_RepairL2",
			l1:           "id-a",
			l1ok:         true,
			l2:           "id-b",
			l2ok:         true,
			l3:           "id-a",
			l3ok:         true,
			wantAction:   actionRepairL2,
			wantResolved: "id-a",
			wantSource:   "l2-repair",
			wantLayer:    "l1",
			wantWriteL2:  true,
		},
		{
			name:       "R4_L1L2L3_BothWrong_RefuseL1L3",
			l1:         "id-a",
			l1ok:       true,
			l2:         "id-b",
			l2ok:       true,
			l3:         "id-c",
			l3ok:       true,
			wantAction: actionRefuseL1L3Mismatch,
		},
		{
			name:         "R5_L1L2_NoL3_L1eqL2_SeedL3",
			l1:           "id-a",
			l1ok:         true,
			l2:           "id-a",
			l2ok:         true,
			wantAction:   actionSeedL3,
			wantResolved: "id-a",
			wantSource:   "l3-seed",
			wantLayer:    "l1",
			wantWriteL3:  true,
		},
		{
			name:         "R6_L1L2_NoL3_L2Wrong_RepairL2SeedL3",
			l1:           "id-a",
			l1ok:         true,
			l2:           "id-b",
			l2ok:         true,
			wantAction:   actionRepairL2SeedL3,
			wantResolved: "id-a",
			wantSource:   "l2-repair-l3-seed",
			wantLayer:    "l1",
			wantWriteL2:  true,
			wantWriteL3:  true,
		},
		{
			name:         "R7_L1NoL2_L3_L1eqL3_SeedL2",
			l1:           "id-a",
			l1ok:         true,
			l3:           "id-a",
			l3ok:         true,
			wantAction:   actionSeedL2,
			wantResolved: "id-a",
			wantSource:   "l2-seed",
			wantLayer:    "l1",
			wantWriteL2:  true,
		},
		{
			name:       "R8_L1NoL2_L3_L1neqL3_RefuseL1L3",
			l1:         "id-a",
			l1ok:       true,
			l3:         "id-b",
			l3ok:       true,
			wantAction: actionRefuseL1L3Mismatch,
		},
		{
			name:         "R9_L1Only_SeedL2L3",
			l1:           "id-a",
			l1ok:         true,
			wantAction:   actionSeedL2L3,
			wantResolved: "id-a",
			wantSource:   "l2-l3-seed",
			wantLayer:    "l1",
			wantWriteL2:  true,
			wantWriteL3:  true,
		},
		{
			name:         "R10_NoL1_L2eqL3_MigrateL1FromL2",
			l2:           "id-a",
			l2ok:         true,
			l3:           "id-a",
			l3ok:         true,
			wantAction:   actionMigrateFromL2,
			wantResolved: "id-a",
			wantSource:   "l1-migrate-from-l2",
			wantLayer:    "l2",
			wantWriteL1:  true,
		},
		{
			name:       "R11_NoL1_L2neqL3_RefuseLegacy",
			l2:         "id-a",
			l2ok:       true,
			l3:         "id-b",
			l3ok:       true,
			wantAction: actionRefuseLegacyMismatch,
		},
		{
			name:         "R12_NoL1_L2Only_MigrateL1SeedL3",
			l2:           "id-a",
			l2ok:         true,
			wantAction:   actionMigrateL1SeedL3,
			wantResolved: "id-a",
			wantSource:   "l1-migrate-l3-seed",
			wantLayer:    "l2",
			wantWriteL1:  true,
			wantWriteL3:  true,
		},
		{
			name:         "R13_NoL1NoL2_L3Only_AdoptL1SeedL2",
			l3:           "id-a",
			l3ok:         true,
			wantAction:   actionAdoptFromL3SeedL2,
			wantResolved: "id-a",
			wantSource:   "l1-adopt-l2-seed",
			wantLayer:    "l3",
			wantWriteL1:  true,
			wantWriteL2:  true,
		},
		{
			name:        "R14_AllAbsent_Generate",
			wantAction:  actionGenerate,
			wantSource:  "generated",
			wantLayer:   "generated",
			wantWriteL1: true,
			wantWriteL2: true,
			wantWriteL3: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideReconcile(tc.l1, tc.l1ok, tc.l2, tc.l2ok, tc.l3, tc.l3ok)
			if got.Action != tc.wantAction {
				t.Fatalf("Action = %v, want %v", got.Action, tc.wantAction)
			}
			if got.ResolvedID != tc.wantResolved {
				t.Fatalf("ResolvedID = %q, want %q", got.ResolvedID, tc.wantResolved)
			}
			if got.Source != tc.wantSource {
				t.Fatalf("Source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Layer != tc.wantLayer {
				t.Fatalf("Layer = %q, want %q", got.Layer, tc.wantLayer)
			}
			if got.WriteL1 != tc.wantWriteL1 || got.WriteL2 != tc.wantWriteL2 || got.WriteL3 != tc.wantWriteL3 {
				t.Fatalf("writes = (L1:%v L2:%v L3:%v), want (L1:%v L2:%v L3:%v)", got.WriteL1, got.WriteL2, got.WriteL3, tc.wantWriteL1, tc.wantWriteL2, tc.wantWriteL3)
			}
		})
	}
}
