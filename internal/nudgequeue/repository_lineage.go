package nudgequeue

import "errors"

// ErrCommandRepositoryLineage reports that repository state and independent
// restore-anchor evidence do not prove the same immutable lineage.
var ErrCommandRepositoryLineage = errors.New("nudge command repository lineage mismatch")

// CommandStoreBinding identifies one durable command repository lineage.
// StoreUUID must be a canonical UUID and RestoreEpoch must be positive.
type CommandStoreBinding struct {
	StoreUUID    string
	RestoreEpoch uint64
}

// CommandRepositoryLineageState is the repository evidence to compare with an
// independent restore anchor. SequenceHighWater must not exceed Revision.
type CommandRepositoryLineageState struct {
	Store             CommandStoreBinding
	Revision          uint64
	SequenceHighWater uint64
}

// RestoreAnchor is independent evidence of the highest command order accepted
// for one repository lineage. HighestAcceptedSequence must not exceed
// HighestAcceptedRevision.
type RestoreAnchor struct {
	Store                   CommandStoreBinding
	HighestAcceptedRevision uint64
	HighestAcceptedSequence uint64
}

// CommandRepositoryLineageClassification is a bounded, redacted reason that
// repository and anchor evidence cannot be accepted as exact equals.
type CommandRepositoryLineageClassification string

const (
	// CommandRepositoryLineageInvalidEvidence means either input is malformed
	// or internally inconsistent.
	CommandRepositoryLineageInvalidEvidence CommandRepositoryLineageClassification = "invalid_evidence"
	// CommandRepositoryLineageForeignUUID means the two inputs name different
	// repository lineages.
	CommandRepositoryLineageForeignUUID CommandRepositoryLineageClassification = "foreign_uuid"
	// CommandRepositoryLineageRestoreEpochRewind means repository evidence has
	// an older restore epoch than its anchor.
	CommandRepositoryLineageRestoreEpochRewind CommandRepositoryLineageClassification = "restore_epoch_rewind"
	// CommandRepositoryLineageRestoreEpochAhead means repository evidence has
	// a newer restore epoch than its anchor.
	CommandRepositoryLineageRestoreEpochAhead CommandRepositoryLineageClassification = "restore_epoch_ahead"
	// CommandRepositoryLineageRevisionRewind means repository evidence has an
	// older revision than its anchor.
	CommandRepositoryLineageRevisionRewind CommandRepositoryLineageClassification = "revision_rewind"
	// CommandRepositoryLineageRevisionAhead means repository evidence has a
	// newer revision than its anchor.
	CommandRepositoryLineageRevisionAhead CommandRepositoryLineageClassification = "revision_ahead"
	// CommandRepositoryLineageSequenceRewind means repository evidence has an
	// older sequence high-water than its anchor.
	CommandRepositoryLineageSequenceRewind CommandRepositoryLineageClassification = "sequence_rewind"
	// CommandRepositoryLineageSequenceAhead means repository evidence has a
	// newer sequence high-water than its anchor.
	CommandRepositoryLineageSequenceAhead CommandRepositoryLineageClassification = "sequence_ahead"
)

// CommandRepositoryLineageError is a typed, value-free classification of a
// repository/anchor mismatch. It intentionally retains no UUIDs or counters.
type CommandRepositoryLineageError struct {
	Classification CommandRepositoryLineageClassification
}

// Error implements error without revealing repository identity or progress.
func (e *CommandRepositoryLineageError) Error() string {
	if e == nil {
		return ErrCommandRepositoryLineage.Error()
	}
	return ErrCommandRepositoryLineage.Error() + ": " + string(e.Classification)
}

// Unwrap exposes ErrCommandRepositoryLineage.
func (e *CommandRepositoryLineageError) Unwrap() error {
	return ErrCommandRepositoryLineage
}

// VerifyCommandRepositoryLineage succeeds only when the repository state and
// restore anchor exactly match in canonical UUID, restore epoch, revision, and
// sequence high-water. It is pure and does not mutate either input.
func VerifyCommandRepositoryLineage(state CommandRepositoryLineageState, anchor RestoreAnchor) error {
	if !validCommandRepositoryState(state) || !validRestoreAnchor(anchor) {
		return commandRepositoryLineageError(CommandRepositoryLineageInvalidEvidence)
	}
	if state.Store.StoreUUID != anchor.Store.StoreUUID {
		return commandRepositoryLineageError(CommandRepositoryLineageForeignUUID)
	}
	if state.Store.RestoreEpoch < anchor.Store.RestoreEpoch {
		return commandRepositoryLineageError(CommandRepositoryLineageRestoreEpochRewind)
	}
	if state.Store.RestoreEpoch > anchor.Store.RestoreEpoch {
		return commandRepositoryLineageError(CommandRepositoryLineageRestoreEpochAhead)
	}
	if state.Revision < anchor.HighestAcceptedRevision {
		return commandRepositoryLineageError(CommandRepositoryLineageRevisionRewind)
	}
	if state.SequenceHighWater < anchor.HighestAcceptedSequence {
		return commandRepositoryLineageError(CommandRepositoryLineageSequenceRewind)
	}
	if state.Revision > anchor.HighestAcceptedRevision {
		return commandRepositoryLineageError(CommandRepositoryLineageRevisionAhead)
	}
	if state.SequenceHighWater > anchor.HighestAcceptedSequence {
		return commandRepositoryLineageError(CommandRepositoryLineageSequenceAhead)
	}
	return nil
}

func commandRepositoryLineageError(classification CommandRepositoryLineageClassification) error {
	return &CommandRepositoryLineageError{Classification: classification}
}

func validCommandRepositoryState(state CommandRepositoryLineageState) bool {
	return validCommandStoreBinding(state.Store) && state.SequenceHighWater <= state.Revision
}

func validRestoreAnchor(anchor RestoreAnchor) bool {
	return validCommandStoreBinding(anchor.Store) && anchor.HighestAcceptedSequence <= anchor.HighestAcceptedRevision
}

func validCommandStoreBinding(binding CommandStoreBinding) bool {
	return binding.RestoreEpoch != 0 && canonicalUUID(binding.StoreUUID)
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value[index] != '-' {
				return false
			}
			continue
		}
		if !lowerHex(value[index]) {
			return false
		}
	}
	return true
}

func lowerHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f'
}
