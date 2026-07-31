package cityartifact

import "fmt"

// ResetDeclaration is the attributable statement that a source epoch advance is
// a deliberate reset and not a misconfigured redeploy.
//
// A bare epoch number cannot carry that distinction: a config typo, a second
// deployment reading a stale value and a real reset all look identical once they
// reach the adapter as "the number went up". So the declaration is carried
// separately and checked, and an epoch advance without one is drift.
type ResetDeclaration struct {
	// FromEpoch is the epoch the declarer believes is checkpointed. It must
	// match what is actually there, which is what proves the declarer knew
	// which frontier was being discarded instead of having guessed.
	FromEpoch uint64
	// ToEpoch is the epoch being declared. It must equal the source epoch, so a
	// declaration left behind in config cannot silently authorize the next bump
	// as well.
	ToEpoch uint64
	// Reason and DeclaredBy are recorded in the checkpoint. A reset nobody can
	// attribute afterwards is indistinguishable from checkpoint corruption.
	Reason     string
	DeclaredBy string
}

// ResetRecord is what the checkpoint keeps about the last honoured reset. It is
// written into the new checkpoint at the moment the reset is applied, so the
// question "why did this frontier restart" has a durable answer.
type ResetRecord struct {
	FromEpoch  uint64 `json:"from_epoch"`
	ToEpoch    uint64 `json:"to_epoch"`
	Reason     string `json:"reason"`
	DeclaredBy string `json:"declared_by"`
}

// checkResetDeclaration decides whether an epoch advance is a declared reset.
//
// This is the whole of the reset contract, and it is identical in all three City
// adapters. Honouring an undeclared advance would let a typo erase a frontier;
// refusing a declared one — and, worse, refusing it forever without recording
// why — is an outage rather than a safety property.
func checkResetDeclaration(d *ResetDeclaration, from, to uint64) (ResetRecord, error) {
	if d == nil {
		return ResetRecord{}, fmt.Errorf("%w: undeclared epoch advance %d -> %d: an epoch advance is honoured only with a reset declaration",
			ErrIdentityDrift, from, to)
	}
	if d.FromEpoch != from {
		return ResetRecord{}, fmt.Errorf("%w: reset declares from epoch %d, checkpoint holds %d",
			ErrIdentityDrift, d.FromEpoch, from)
	}
	if d.ToEpoch != to {
		return ResetRecord{}, fmt.Errorf("%w: reset declares to epoch %d, source epoch is %d",
			ErrIdentityDrift, d.ToEpoch, to)
	}
	if d.Reason == "" || d.DeclaredBy == "" {
		return ResetRecord{}, fmt.Errorf("%w: reset %d -> %d names no reason or no declarer",
			ErrIdentityDrift, from, to)
	}
	return ResetRecord{FromEpoch: from, ToEpoch: to, Reason: d.Reason, DeclaredBy: d.DeclaredBy}, nil
}
