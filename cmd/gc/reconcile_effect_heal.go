package main

// The fresh heal: A6's heals whose patch rests on the runtime (CONTRACT v5
// A6; R2, reads propose and effects decide). The asleep heals rest on the
// inventory reading the name gone, which can lag a runtime that came up after
// its pass began: a start that settled deferred (S3) leaves its row creating
// at its own token with its runtime perhaps up, so the token fence alone
// would let the heal orphan it. The awake heal rests on the inventory reading
// the row's own runtime alive.
//
// So the heal is a transaction (runTx) that needs the runtime: under the
// runtime name lock, which a v2 start holds across its provider Start (an
// abandoned start until that call returns, P3), and the row's session
// mutation lock, which in-process wakes and row writers take, each attempt
// reads the name fresh (txRuntime) and then the row. The heal writes only
// when that read still proves it: nothing present on the leaf or any
// fall-through backend for an asleep heal (rtAbsent; no corpse until A12's
// classification lands), the row's own Current runtime alive for the awake
// heal. The default premise refuses a fresh row whose lifecycle facts
// differ from the pass's (legacy's ApplyPatchIfLifecycleUnchanged), so a
// wake request that lands before the CAS stops it.
//
// Residual, as in legacy: a process outside the controller (`gc session
// attach`, which starts a runtime at the row's token without the name lock)
// can bring a runtime up after the fresh read and before the CAS. The row is
// then asleep with its own live runtime, which the awake heal restores.

// Fresh-heal refusal causes, beside the shared ones (causeNameBusy,
// causeRouteUnknown, causeLivenessUnknown). A refusal backs the row off (P4);
// repeated ones alert (alertHealRefused).
const (
	causeLivenessUnsupported = "liveness-unsupported"
	causeRuntimePresent      = "runtime-present" // an asleep heal over a present runtime
	causeRuntimeNotOwn       = "runtime-not-own" // the awake heal over a runtime not alive and Current
)

// healSections are the fresh heal's one section: its arm decided again on
// the fresh row and the fresh runtime read (redecideRow; the rule, once),
// a refusal named by what that read found (healCause).
var healSections = []section{{Decide: func(v txView) txStep {
	step := redecideRow(v)
	if step.Refuse == causeRedecided {
		step.Refuse = healCause(v)
	}
	return step
}}}

// healCause names a fresh heal's refusal by its fresh read: a backend that
// cannot read fresh, a read that proved nothing, the awake heal's runtime
// not its own and alive, an asleep heal's runtime present; or redecided
// when the read stands and the row moved on.
func healCause(v txView) string {
	switch rt := v.RT; {
	case rt.Class == rtUnsupported:
		return causeLivenessUnsupported
	case v.It.Reason == decideAwakeHeal && (!rt.Alive() || compareIdentity(v.Row, rt.Identity) != identityCurrent):
		return causeRuntimeNotOwn
	case v.It.Reason == decideAwakeHeal:
	case rt.Class == rtUnknown:
		return causeLivenessUnknown
	case rt.Class != rtAbsent:
		return causeRuntimePresent
	}
	return causeRedecided
}
