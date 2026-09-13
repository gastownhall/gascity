package session

import (
	"strconv"
	"strings"
)

const (
	// CanonicalInstanceNameMetadata is the durable metadata key holding a
	// session's canonical qualified instance name — the one identity record the
	// reconciler resolves and stamps at create/adoption time and (from S19
	// Stage 3 on) heals on later ticks.
	//
	// It is the level-triggered replacement (S19) for re-deriving identity every
	// tick from up to six competing metadata/label sources through the precedence
	// ladders in cmd/gc. When the record is present every read collapses to one
	// field read; the config-derived ladder is consulted only to heal an absent
	// record. Stage 2 is WRITE-ONLY: this key is stamped but no decision path
	// reads it yet (the reader cutover is Stage 5).
	CanonicalInstanceNameMetadata = "canonical_instance_name"
	// CanonicalPoolSlotMetadata is the durable metadata key holding a session's
	// canonical pool slot (a positive integer; absent/empty/<=0 means unslotted,
	// i.e. a singleton). It is written and read alongside CanonicalInstanceNameMetadata.
	CanonicalPoolSlotMetadata = "canonical_pool_slot"
)

// freeCanonicalIdentityMetadata clears both durable canonical-identity keys on a
// metadata patch/update map (empty values clear at the store layer). Every
// named-session retirement path routes through this one helper so the two keys
// are always freed together and the "canonical identity is freed on retirement"
// invariant (S19) cannot drift between the RetireNamedSessionPatch builder and
// the hand-rolled Manager.Close configured-named-session path.
func freeCanonicalIdentityMetadata(meta map[string]string) {
	meta[CanonicalInstanceNameMetadata] = ""
	meta[CanonicalPoolSlotMetadata] = ""
}

// CanonicalIdentity is the single durable identity record for a session bead:
// the canonical qualified instance name plus pool slot the reconciler resolved
// once and stamped, rather than a value re-inferred from competing sources each
// tick. Present reports whether a record was actually persisted; when it is
// false the caller falls back to the quarantined legacy config-derivation
// (from Stage 5) exactly once and then heals the record, so subsequent ticks
// read the field directly and every arrival path agrees by construction.
type CanonicalIdentity struct {
	// QualifiedInstanceName is the canonical "dir/name" (or singleton) identity.
	QualifiedInstanceName string
	// PoolSlot is the canonical pool slot; 0 means unslotted (singleton).
	PoolSlot int
	// Present is true iff a canonical record was persisted (a non-empty
	// qualified instance name is the record's existence signal).
	Present bool
}

// CanonicalIdentityFromMetadata reads the persisted canonical identity record
// from raw session-bead metadata. It reads exactly the two canonical keys and
// performs no config-derivation or precedence laddering — when the record is
// present it is authoritative. The record exists iff a non-empty canonical
// qualified instance name was stamped; an empty name yields the zero record
// (Present false) regardless of any stray slot value, because a canonical
// identity is meaningless without its name.
func CanonicalIdentityFromMetadata(meta map[string]string) CanonicalIdentity {
	if meta == nil {
		return CanonicalIdentity{}
	}
	return canonicalIdentityFrom(meta[CanonicalInstanceNameMetadata], meta[CanonicalPoolSlotMetadata])
}

// canonicalIdentityFrom is the single record-existence + slot-parse rule shared
// by CanonicalIdentityFromMetadata (over a raw bead map) and Info.CanonicalIdentity
// (over the two verbatim Info mirrors), so the two projections can never drift.
func canonicalIdentityFrom(rawName, rawSlot string) CanonicalIdentity {
	name := strings.TrimSpace(rawName)
	if name == "" {
		return CanonicalIdentity{}
	}
	return CanonicalIdentity{
		QualifiedInstanceName: name,
		PoolSlot:              parseCanonicalSlot(rawSlot),
		Present:               true,
	}
}

// parseCanonicalSlot parses a canonical pool-slot metadata value. A missing,
// non-numeric, or non-positive value is unslotted (0).
func parseCanonicalSlot(raw string) int {
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// CanonicalIdentity projects the canonical identity record from a session
// Info's two verbatim raw mirrors. It is a pure accessor over the mirrors —
// nothing is stored derived — so a folded ApplyPatch snapshot and a full
// re-projection agree by construction (TestInfoApplyPatchMatchesReprojection).
// Stage 2 is WRITE-ONLY: this accessor is computed but consulted by nothing
// outside tests.
func (i Info) CanonicalIdentity() CanonicalIdentity {
	return canonicalIdentityFrom(i.CanonicalInstanceNameMetadata, i.CanonicalPoolSlotMetadata)
}
