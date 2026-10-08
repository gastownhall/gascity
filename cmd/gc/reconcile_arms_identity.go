package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// Arm A3 (CONTRACT v5 §4, S4, O2): the row's runtime identity. It runs
// before the stop request (A4), so a StaleSelf stop-pending row is re-keyed
// and then stopped (v5.1 B3): the stop's L2 passes only on Current.

// A3's reasons.
const (
	decideRekey     = "rekey"
	decideNewerSelf = "newer-self"
)

// armIdentity is A3. A present runtime of the row that carries an older
// token (StaleSelf: a warm-reuse residue, or `gc attach` with the old token)
// is re-keyed under S4's guards (rekeyable), to the token the inventory read;
// a newer incarnation (NewerSelf: census lag) holds. A StaleSelf row the
// guards refuse falls through: a pending create's runtime is S7's (A10). The
// verdict is computed here on the row as read, so the rekey effect's CAS,
// which re-decides on the fresh row, re-checks it. No effect for the row is
// in flight: the pass loop skips such rows (R5).
func armIdentity(r *rowFacts) (intent, bool) {
	rt := r.w.Observed[r.k].Identity
	switch compareIdentity(r.row.Info, rt) {
	case identityNewerSelf:
		return intent{Reason: decideNewerSelf}, true
	case identityStaleSelf:
		if !rekeyable(r.row.Info, rt) {
			return intent{}, false
		}
		basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
		patch := session.MetadataPatch{"instance_token": strings.TrimSpace(rt.Token)}
		return intent{Kind: intentRekey, Reason: decideRekey, Basis: basis, Patch: patch}, true
	}
	return intent{}, false
}

// rekeyable is S4's guards on one identity read of row's runtime. StaleSelf
// is three of them (compareIdentity): the row's session ID, a non-empty
// runtime token (v5.1 B6), and the runtime's epoch at most the row's
// generation. The fourth: the row holds no pending_create_claim, since a
// pending create's runtime is resolved by S1 and C8.2(a), never re-keyed
// (X2). The arm and the effect's fresh read both apply it.
func rekeyable(row session.Info, rt runtimeIdentity) bool {
	return compareIdentity(row, rt) == identityStaleSelf && !row.PendingCreateClaim
}
