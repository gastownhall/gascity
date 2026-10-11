package beads

import (
	"fmt"
	"strings"
)

// A wisp claim on a REMOTE work store (DESIGN C5 G8, S24).
//
// gc claims wisps in the city's graph store (formula and molecule steps are
// ClassGraph), so a claim that reaches a remote work store for a wisp id is a
// claim gc should never have to make there. It can happen two ways, and both
// must be LOUD rather than read as "the bead is elsewhere":
//
//   - the native store's Claimer role excludes the wisp plane on every
//     backend (issueops.Claimer: a wisp id is ErrNotFound), and
//     NativeDoltStore.Claim already disambiguates that into
//     ErrWispNotClaimable with a second read;
//   - the bd CLI over http, against a server that predates claiming through
//     its update operation (no issues.update.claim), falls back to the claim
//     operation and refuses a wisp by name: the divergence-ledger row
//     W-ClaimRequest.Wisp, whose text is wispClaimRefusalText.
//
// A bare not-found would be worse than a failure: the hook's claim-time class
// route escalates on ErrNotFound alone, so a wisp the work store DOES hold
// would be searched for in the graph binding, not found there, and skipped as
// if it were absent everywhere. WispClaimRefusedError is never ErrNotFound; it
// unwraps to ErrWispNotClaimable, which the hook skips with this text on
// stderr.

// wispClaimRefusalText is the What of beads' W-ClaimRequest.Wisp ledger row
// (internal/httpclient/encode/ledger.go): the bd CLI prints it inside
// "Error updating <id>: updateIssue: ..." when `bd update <wisp> --claim`
// reaches a server without issues.update.claim.
const wispClaimRefusalText = "claiming a wisp is not supported by this bd serve"

// isBdWispClaimRefusal reports whether bd's output is the W-ClaimRequest.Wisp
// refusal.
func isBdWispClaimRefusal(msg string) bool {
	return strings.Contains(strings.ToLower(msg), wispClaimRefusalText)
}

// WispClaimRefusedError is the named refusal for claiming a wisp on a remote
// work store. It unwraps to ErrWispNotClaimable and is never ErrNotFound.
type WispClaimRefusedError struct {
	// ID is the wisp id the claim named.
	ID string
	// ScopeRoot is the work-store scope the claim ran against, when known.
	ScopeRoot string
	// Detail is what the store said (the bd refusal, or the native role's).
	Detail string
}

// Error implements error.
func (e *WispClaimRefusedError) Error() string {
	if e == nil {
		return ""
	}
	where := "the remote work store"
	if strings.TrimSpace(e.ScopeRoot) != "" {
		where += " at " + e.ScopeRoot
	}
	msg := fmt.Sprintf("refusing to claim wisp %q on %s: gc claims wisps in the city's graph store, and this work store cannot claim one", e.ID, where)
	if detail := strings.TrimSpace(e.Detail); detail != "" {
		msg += " (" + detail + ")"
	}
	return msg + "; upgrade bd serve to one advertising issues.update.claim, or move the wisp to the graph store"
}

// Unwrap makes the refusal match ErrWispNotClaimable.
func (e *WispClaimRefusedError) Unwrap() error { return ErrWispNotClaimable }
