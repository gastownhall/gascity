package api

import "github.com/gastownhall/gascity/internal/beads"

// Per-domain Huma input/output types for the worker lifecycle route family
// (cr-gdeav.5.4 draft). Mirrors the layout of huma_types_beads.go.
//
// These are the four verbs a worker needs when it is NOT on the city host:
// acquire a claim, refresh its lease, hand a claim back, and close with a
// typed work record. Every other bead verb stays on the existing surface.

// workerSessionBody is the request shape shared by claim / heartbeat / release:
// the identity asking, the bead it names, and the session it acts for. The
// identity — not the transport credential — is what the store compares, which
// is what keeps release-if-current a compare-and-set rather than a name check.
type workerSessionBody struct {
	SessionID string `json:"session_id,omitempty" doc:"gc session the verb is issued for; recorded for attribution, never used as the ownership pointer."`
	Assignee  string `json:"assignee" doc:"Claimant identity (a pool seat or crew holder name). This is the value the store compares." minLength:"1"`
	BeadID    string `json:"bead_id" doc:"Bead the verb acts on." minLength:"1"`
}

// WorkerClaimInput is the Huma input for POST /v0/city/{cityName}/worker/claim.
type WorkerClaimInput struct {
	CityScope
	Body workerSessionBody
}

// WorkerClaimOutput is the claim response. The bead comes back whole and the
// precondition token travels as a header, because a CAS whose token can only
// be learned by a second read is decoration: the follow-up read can observe a
// different bead on a dual-resident store (the #6015 class), and it widens the
// window in which a losing claimer mistakes itself for the winner.
type WorkerClaimOutput struct {
	Index       uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	ETag        string `header:"ETag" doc:"Precondition token for this holder's snapshot; echo it in a conditional write. Absent when the backing store exposes no usable revision."`
	XGCRevision string `header:"X-GC-Revision" doc:"Store revision the ETag encodes, in plain form."`
	Body        struct {
		Status string     `json:"status" doc:"Claim result." example:"claimed"`
		Bead   beads.Bead `json:"bead" doc:"The bead as the store persisted it."`
	}
}

// WorkerHeartbeatInput is the Huma input for POST /v0/city/{cityName}/worker/heartbeat.
type WorkerHeartbeatInput struct {
	CityScope
	Body workerSessionBody
}

// workerLeaseScopeBeadMetadata is the only lease scope this route can honestly
// report at this commit: the write lands on the bead's lease metadata, not on
// bd's native lease table. Kept as a named constant because the value is a
// contract a client branches on, not a string in a doc comment.
const workerLeaseScopeBeadMetadata = "bead-metadata"

// WorkerHeartbeatOutput is the heartbeat response. A 200 here is the promise
// that the named identity still held the bead when the city answered and the
// bead's revision moved under a holder-only write — so the response carries the
// scope of what it renewed, and a client that needed bd's lease table extended
// can see in the payload that it did not get that (see
// humaHandleWorkerHeartbeat's contract block).
type WorkerHeartbeatOutput struct {
	Index       uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	ETag        string `header:"ETag" doc:"Precondition token for the refreshed snapshot."`
	XGCRevision string `header:"X-GC-Revision" doc:"Store revision the ETag encodes, in plain form."`
	Body        struct {
		Status     string `json:"status" doc:"Heartbeat result." example:"renewed"`
		LeaseScope string `json:"lease_scope" doc:"Which lease this refresh reached. bead-metadata means the bead's gc.lease_owner stamp and revision moved; bd's native lease table (bd reclaim's selector) is NOT reachable from this route."`
		ClaimedAt  string `json:"claimed_at,omitempty" doc:"First-claim instant (gc.claimed_at), RFC3339 UTC. Write-once: a heartbeat reports it and never re-stamps it."`
		LeaseOwner string `json:"lease_owner,omitempty" doc:"Lease holder the refresh re-affirmed (gc.lease_owner)."`
	}
}

// WorkerReleaseInput is the Huma input for DELETE /v0/city/{cityName}/worker/claim.
// Release keeps the edge family's spelling — the claim resource, DELETEd, with a
// body naming the expected holder — so the route name is pinned by the contract
// rather than invented twice.
type WorkerReleaseInput struct {
	CityScope
	Body workerSessionBody
}

// WorkerReleaseOutput reports released vs skipped. A skip is a RESULT, not an
// error: a worker unwinding a startup failure must be able to tell "I gave the
// bead back" from "someone else already holds it" without treating the second
// as a failure that masks the reason it is exiting.
type WorkerReleaseOutput struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  struct {
		Status string     `json:"status" doc:"Release result: released or skipped." example:"released"`
		Bead   beads.Bead `json:"bead" doc:"The bead after the release attempt."`
	}
}

// WorkerCloseInput is the Huma input for POST /v0/city/{cityName}/worker/close.
//
// outcome/commit/branch mirror the ADR-0009 work record the local close gate
// enforces (gc.work_outcome / gc.work_commit / gc.work_branch). Reason is
// required by the same discipline that makes an untyped close uncitable.
type WorkerCloseInput struct {
	CityScope
	Body struct {
		SessionID string `json:"session_id,omitempty" doc:"gc session the close is issued for. Enforced: when the bead carries a session stamp, a different session is refused."`
		Assignee  string `json:"assignee" doc:"The holder performing the close (the same identity the claim took). A close without a holder is refused: the record would attribute the work to whatever the caller typed." minLength:"1"`
		BeadID    string `json:"bead_id" doc:"Bead to close." minLength:"1"`
		Outcome   string `json:"outcome" doc:"Typed close disposition: shipped, no-op, blocked or abandoned."`
		Commit    string `json:"commit,omitempty" doc:"Commit that satisfies the close. Required by shipped."`
		Branch    string `json:"branch,omitempty" doc:"Branch the commit must be reachable on. Required by shipped."`
		Reason    string `json:"reason,omitempty" doc:"Why the close is what it is. Required for every disposition except shipped."`
	}
}

// WorkerCloseOutput echoes the record it wrote, so the caller can confirm what
// landed without a second read that could observe a different bead.
type WorkerCloseOutput struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  struct {
		Status string     `json:"status" doc:"Close result: closed, or already_closed when the same holder retries a close whose record already landed." example:"closed"`
		Bead   beads.Bead `json:"bead" doc:"The bead as the atomic terminal write persisted it."`
	}
}
