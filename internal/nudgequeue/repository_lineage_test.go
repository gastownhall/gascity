package nudgequeue

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyCommandRepositoryLineage(t *testing.T) {
	baseState := CommandRepositoryLineageState{
		Store:             CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174000", RestoreEpoch: 7},
		Revision:          41,
		SequenceHighWater: 17,
	}
	baseAnchor := RestoreAnchor{
		Store:                   baseState.Store,
		HighestAcceptedRevision: baseState.Revision,
		HighestAcceptedSequence: baseState.SequenceHighWater,
	}

	tests := []struct {
		name      string
		state     CommandRepositoryLineageState
		anchor    RestoreAnchor
		wantClass CommandRepositoryLineageClassification
	}{
		{name: "exact canonical equality", state: baseState, anchor: baseAnchor},
		{
			name:      "missing repository UUID",
			state:     CommandRepositoryLineageState{Store: CommandStoreBinding{RestoreEpoch: 7}, Revision: 41, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "missing anchor UUID",
			state:     baseState,
			anchor:    RestoreAnchor{Store: CommandStoreBinding{RestoreEpoch: 7}, HighestAcceptedRevision: 41, HighestAcceptedSequence: 17},
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "noncanonical repository UUID",
			state:     CommandRepositoryLineageState{Store: CommandStoreBinding{StoreUUID: "123E4567-E89B-12D3-A456-426614174000", RestoreEpoch: 7}, Revision: 41, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "zero anchor epoch",
			state:     baseState,
			anchor:    RestoreAnchor{Store: CommandStoreBinding{StoreUUID: baseState.Store.StoreUUID}, HighestAcceptedRevision: 41, HighestAcceptedSequence: 17},
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "repository sequence exceeds revision",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 16, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "anchor sequence exceeds revision",
			state:     baseState,
			anchor:    RestoreAnchor{Store: baseState.Store, HighestAcceptedRevision: 16, HighestAcceptedSequence: 17},
			wantClass: CommandRepositoryLineageInvalidEvidence,
		},
		{
			name:      "foreign UUID",
			state:     CommandRepositoryLineageState{Store: CommandStoreBinding{StoreUUID: "123e4567-e89b-12d3-a456-426614174001", RestoreEpoch: 7}, Revision: 41, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageForeignUUID,
		},
		{
			name:      "restore epoch rewind",
			state:     CommandRepositoryLineageState{Store: CommandStoreBinding{StoreUUID: baseState.Store.StoreUUID, RestoreEpoch: 6}, Revision: 41, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageRestoreEpochRewind,
		},
		{
			name:      "restore epoch database ahead",
			state:     CommandRepositoryLineageState{Store: CommandStoreBinding{StoreUUID: baseState.Store.StoreUUID, RestoreEpoch: 8}, Revision: 41, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageRestoreEpochAhead,
		},
		{
			name:      "revision rewind",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 40, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageRevisionRewind,
		},
		{
			name:      "revision database ahead",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 42, SequenceHighWater: 17},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageRevisionAhead,
		},
		{
			name:      "sequence rewind dominates revision database ahead",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 42, SequenceHighWater: 16},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageSequenceRewind,
		},
		{
			name:      "sequence rewind",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 41, SequenceHighWater: 16},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageSequenceRewind,
		},
		{
			name:      "sequence database ahead",
			state:     CommandRepositoryLineageState{Store: baseState.Store, Revision: 41, SequenceHighWater: 18},
			anchor:    baseAnchor,
			wantClass: CommandRepositoryLineageSequenceAhead,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, anchor := tc.state, tc.anchor
			err := VerifyCommandRepositoryLineage(state, anchor)
			if state != tc.state || anchor != tc.anchor {
				t.Fatal("VerifyCommandRepositoryLineage mutated its input")
			}
			if tc.wantClass == "" {
				if err != nil {
					t.Fatalf("VerifyCommandRepositoryLineage() error = %v, want nil", err)
				}
				return
			}
			var lineageErr *CommandRepositoryLineageError
			if !errors.As(err, &lineageErr) {
				t.Fatalf("error = %T %v, want CommandRepositoryLineageError", err, err)
			}
			if !errors.Is(err, ErrCommandRepositoryLineage) {
				t.Fatalf("error = %v, want ErrCommandRepositoryLineage", err)
			}
			if lineageErr.Classification != tc.wantClass {
				t.Fatalf("classification = %q, want %q", lineageErr.Classification, tc.wantClass)
			}
			if strings.Contains(err.Error(), "123e4567") || strings.Contains(err.Error(), "41") {
				t.Fatalf("error leaks lineage evidence: %q", err)
			}
		})
	}
}
