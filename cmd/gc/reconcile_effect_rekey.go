package main

import "github.com/gastownhall/gascity/internal/session"

// The rekey effect (CONTRACT v5 S4; owner ruling 3): arm A3's intent to
// align a row's instance_token with the runtime of the same row that carries
// an older token, so that runtime reads Current and its stop's L2 passes.
// It is a transaction (runTx): under the runtime name lock and the row's
// session mutation lock, which an in-process start or restart takes, through
// the CAS, it re-reads the runtime fresh (readRuntime): presence, then
// identity, then presence again on the same session object. It proceeds
// only while the runtime is still StaleSelf to the pass's row under S4's
// guards, with the token the pass saw (rekeyStillHolds). Then it is the row
// write: one CAS of instance_token, decided again on the fresh row, which
// the premise holds at the generation, token and lifecycle facts the pass
// saw (a pending_create_claim among them). It never writes generation.
// Residual: an out-of-process writer of the runtime's identity (`gc attach`
// relaunching it) takes neither lock; a token it rewrites in place on the
// same object after the read can still be overwritten, and the next pass's
// A3 re-keys the row to it. A probing effect (60s); not boot-gated, and it
// costs no token.

// causeIdentityChanged refuses a rekey whose fresh read no longer finds the
// runtime the pass saw. A refusal backs the row off (P4).
const causeIdentityChanged = "identity-changed"

// rekeySections are the rekey's one section.
var rekeySections = []section{{Decide: rekeyStep}}

// rekeyStep refuses unless the runtime read fresh still allows rekeying the
// pass's row to the pass's token (rekeyRefusal); then it is the row write's
// Decide, on the fresh row.
func rekeyStep(v txView) txStep {
	if cause := rekeyRefusal(v.RT, v.World.Census.Rows[v.It.Key].Info, v.It.Patch["instance_token"]); cause != "" {
		return txStep{Refuse: cause}
	}
	return redecideRow(v)
}

// rekeyRefusal is the cause that refuses a rekey of the pass's row to token
// on rt, or "": rt must read fresh, present, one session object around its
// identity read, and still rekeyable (S4).
func rekeyRefusal(rt *txRuntime, row session.Info, token string) string {
	switch {
	case rt.Class == rtUnsupported:
		return causeLivenessUnsupported
	case rt.Class == rtUnknown:
		return causeLivenessUnknown
	case rt.Class == rtAbsent || !rt.Same:
		return causeNotPresent
	case !rekeyStillHolds(row, rt.Identity, token):
		return causeIdentityChanged
	}
	return ""
}
