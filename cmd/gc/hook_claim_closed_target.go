package main

import "errors"

// errHookClaimTargetClosed is what a seat's claim reports when the claim CAS
// was refused by a canonical row that is closed (#5857) — the readback
// hookClaimThroughStore makes after bd refuses "issue not claimable: status
// closed". That is not a lost race against a live claimant (BeadClaimRejected
// stays for those): the row's owning store already closed it, so the demand
// that spawned the seat was a stale replica's shadow. The seat drains
// claims_errored rather than no_work, and this failure is the claim-loop
// guard's input, so the spawn loop the stale replica mints is held once
// enough seats fail the same way (claim_loop_guard.go).
//
// The text names no seat, session or holder: the guard's identical-error
// streak must form across the different seats the spawn loop minted, and
// claimLoopComparable strips only the server's request id.
var errHookClaimTargetClosed = errors.New("claim target is closed in its owning store")
