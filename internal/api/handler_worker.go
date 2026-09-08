package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The worker lifecycle family (cr-gdeav.5.6 rework of the cr-gdeav.5.4 draft):
// claim, heartbeat, release, typed close, for a worker that is not on the city
// host.
//
// THE RULE THESE HANDLERS OBEY. Each route is transport for a store capability
// that already exists and is already conformance-tested; none of them is a
// second implementation of it. A store without the capability gets a typed 501
// and NOTHING is written — never a read-then-write that looks like the verb.
// Emulating a claim loses single-winner; emulating a release is the lost update
// ConditionalAssignmentReleaser exists to prevent; emulating an atomic close is
// how a bead ends up closed with no work record. The adapter states the same
// rule for the graph class (internal/storebinding/beads_adapter.go:487-512);
// these handlers state it for the HTTP surface.
//
// EVERY WRITE IN THIS FAMILY IS CONDITIONAL. The cr-gdeav.5.5 review bounced the
// draft because stampWorkerLease called the unconditional Store.Update after
// Claim or after a holder read: a delayed claim stamp or a stale heartbeat
// could then overwrite gc.lease_owner / gc.claimed_at on a bead that had
// already been released and re-claimed, which puts the lease keys — the only
// thing naming who a reaper should ask — on the wrong holder. There is no
// fallback path here: the lease write goes through beads.ConditionalWriter at
// the revision the handler just observed, and a bead that moved under it is a
// 409 with the lease untouched. A store that cannot fence the stamp cannot
// serve the family at all, and says so BEFORE writing anything (see
// workerLeaseWriter), rather than claiming first and failing to stamp.
//
// WHY A SEPARATE FAMILY INSTEAD OF FLAGS ON THE BEAD VERBS. POST
// /bead/{id}/assign is last-write-wins (huma_handlers_beads.go:792-806), POST
// /bead/{id}/close calls store.Close(id) with no reason and no metadata
// (:755-761), and there is no release verb on the wire at all — so the worker
// contract cannot be expressed by reusing them without changing what every
// existing caller observes.

// workerAssignmentClaimer is the acquire half of the conditional-assignment
// pair, discovered on the RESOLVED store the same way
// beads.ConditionalAssignmentReleaser is. It is restated here rather than
// imported because the canonical beads.Store surface deliberately has no claim
// method (internal/storebinding/beads_adapter.go:487-491): the capability
// belongs to the backend, and each front door asserts it.
//
// POPULATION AT THIS COMMIT: the two-argument claim exists on SQLiteStore
// (internal/beads/sqlite_store_claim.go:22) and on the graph adapter that
// delegates to it (internal/storebinding/beads_adapter.go:500). BdStore's claim
// takes a different shape — the assignee is implicit in the bd subprocess
// invocation (internal/beads/bdstore.go:1726) — and NativeDoltStore has none.
// Those backends answer 501 and write nothing; that boundary is the open half
// of #5737 and is recorded, not papered over.
type workerAssignmentClaimer interface {
	Claim(id, assignee string) (beads.Bead, bool, error)
}

// workerCloseReasonMetadataKey is the durable close reason. beads.Bead has no
// close-reason field and Store.Close takes none, but the key is NOT invented
// here: "close_reason" is the metadata key gc already writes a close reason
// under — internal/mail/beadmail/beadmail.go:491,913 (the retention sweep),
// cmd/gc/cmd_convoy.go:1758, internal/dispatch/runtime.go:348,365,1013,
// internal/sourceworkflow/sourceworkflow.go:458 — and BdStore forwards
// metadata["close_reason"] to bd's own --reason on close
// (internal/beads/bdstore.go:2415,2439,2518-2542). Writing it inside the atomic
// terminal write therefore persists the reason in the same transaction as the
// close, in the spelling the rest of gc already reads, rather than in a second
// envelope only this route understands.
const workerCloseReasonMetadataKey = "close_reason"

// workerLeaseWriter returns the fence the lease stamp must go through, or a
// typed 501 that writes nothing. It is called before the first write of claim
// and heartbeat for exactly that reason: a store that can claim but cannot
// CAS would otherwise leave a bead assigned with no lease, which is the
// leaseless-claim hazard the keys exist to remove.
func workerLeaseWriter(store beads.Store) (beads.ConditionalWriter, error) {
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return nil, apierr.NotImplemented.Msg("worker verb: the resolved store exposes no revision-fenced conditional write, so its lease stamp could not be fenced against a newer holder; refusing to stamp the lease with an unconditional update")
	}
	return writer, nil
}

// humaHandleWorkerClaim serves POST /v0/city/{cityName}/worker/claim.
//
// Delegates to the store's two-argument claim, so single-winner and
// same-holder idempotence are the store's properties, not this handler's. The
// loser is a 409, not a 200 with a warning: an off-host worker has no way to
// notice it was displaced, and a displaced worker that believes it owns the
// bead writes over the real owner.
//
// The claim also stamps the two metadata keys the city reads — gc.claimed_at
// and gc.lease_owner (internal/beadmeta/keys.go:61, :177) — as ONE
// revision-fenced write that is fenced to the holder the claim produced. A
// claim that writes neither is leaseless to every reaper in the estate, and a
// leaseless claim is invisible to the primitives that would otherwise reclaim
// it when the seat dies (AGENTS.md: a missing lease is not an infinitely stale
// one). gc.claimed_at keeps its documented write-once contract
// (internal/beadmeta/keys.go:54-61): a same-holder re-claim refreshes the
// holder stamp and leaves the first-claim instant alone, because that instant
// feeds the created→claimed latency transitions and re-stamping it silently
// rewrites history.
func (s *Server) humaHandleWorkerClaim(_ context.Context, input *WorkerClaimInput) (*WorkerClaimOutput, error) {
	id, assignee, err := workerSessionIntent(input.Body)
	if err != nil {
		return nil, err
	}
	store, _, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	claimer, ok := store.(workerAssignmentClaimer)
	if !ok {
		return nil, apierr.NotImplemented.Msg("worker claim: the resolved store does not implement the two-argument assignment claim; refusing to emulate it with a read-then-write, which would lose the single-winner guarantee")
	}
	writer, err := workerLeaseWriter(store)
	if err != nil {
		return nil, err
	}
	claimed, acquired, err := claimer.Claim(id, assignee)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("worker claim: bead " + id + " not found")
		}
		return nil, apierr.Internal.Msg("worker claim: " + err.Error())
	}
	// A same-holder re-claim reports "not freshly acquired" and is idempotent
	// success — a retry after a dropped response must not conflict. Anything
	// else that did not acquire the bead lost it to someone else.
	if !acquired && strings.TrimSpace(claimed.Assignee) != assignee {
		return nil, apierr.ConflictWrongState.Msg("worker claim: bead " + id + " is held by " + quotedOrEmpty(claimed.Assignee) + ", not " + quotedOrEmpty(assignee))
	}
	// The stamp is fenced to the row this claim actually produced. claimed is
	// not trusted for the revision: the two-argument claim's contract returns
	// the held bead on the idempotent path and a store is free to hand back the
	// snapshot it compared, so the handler re-reads and CASes at the revision
	// of THAT row. The read is not what makes this safe — the fence is.
	fence, err := readWorkerFence(store, id, assignee, "worker claim")
	if err != nil {
		return nil, err
	}
	if err := stampWorkerLease(writer, fence, assignee, nowUTC()); err != nil {
		return nil, err
	}
	final, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("worker claim: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg("worker claim: reading the claimed bead: " + err.Error())
	}
	out := &WorkerClaimOutput{Index: s.latestIndex()}
	setRevisionPrecondition(&out.ETag, &out.XGCRevision, final.Revision)
	out.Body.Status = "claimed"
	out.Body.Bead = final
	return out, nil
}

// humaHandleWorkerHeartbeat serves POST /v0/city/{cityName}/worker/heartbeat.
//
// WHAT THIS ROUTE PROMISES, EXACTLY. A 200 means: at the instant the city
// answered, the named identity still held the bead, and the bead's revision
// moved under a write that only that holder could have made. That is the whole
// promise, and it is a real one — it is a holder-attested liveness mark that a
// concurrent reclaim cannot be mistaken for.
//
// WHAT IT EXPLICITLY DOES NOT PROMISE: that bd's native lease table was
// extended. That table is bd's own and is what `bd reclaim` selects on; gc's
// bead layer has no heartbeat and no lease at all at this commit (rg
// 'Heartbeat|Lease' over internal/beads/beads.go returns nothing, and BdStore
// exposes no heartbeat method — the LOCAL orthodox form is `gc bd heartbeat`,
// which forwards to bd's native heartbeat precisely so a green heartbeat cannot
// leave the claim stale mid-task, cmd/gc/cmd_bd.go:188-210). The city API has
// no path to that table from off-host, and this route does not pretend
// otherwise: the response carries lease_scope="bead-metadata" so a client can
// tell which lease it renewed, and the claim's OWN liveness anchor upstream is
// gc's session registry — releaseOrphanedPoolAssignments
// (cmd/gc/pool_session_name.go:169,240-246) decides an orphan from whether the
// ASSIGNEE STRING still maps to an open session bead, not from bead metadata.
// Extending the lease table over the wire is the half of #5737 this patch does
// not close; it is named there rather than faked here.
//
// gc.claimed_at is deliberately NOT re-stamped: it is write-once by contract
// (internal/beadmeta/keys.go:54-61) and a heartbeat that moved it would rewrite
// the first-claim latency fact to "now" for as long as the worker kept beating.
func (s *Server) humaHandleWorkerHeartbeat(_ context.Context, input *WorkerHeartbeatInput) (*WorkerHeartbeatOutput, error) {
	id, assignee, err := workerSessionIntent(input.Body)
	if err != nil {
		return nil, err
	}
	store, _, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	writer, err := workerLeaseWriter(store)
	if err != nil {
		return nil, err
	}
	// The verb is fenced to the holder, and the fence covers that exact read:
	// a non-holder is refused, and a holder whose bead moved between this read
	// and the stamp loses the write instead of landing on the new holder.
	fence, err := readWorkerFence(store, id, assignee, "worker heartbeat")
	if err != nil {
		return nil, err
	}
	if err := stampWorkerLease(writer, fence, assignee, nowUTC()); err != nil {
		return nil, err
	}
	after, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("worker heartbeat: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg("worker heartbeat: reading the refreshed bead: " + err.Error())
	}
	out := &WorkerHeartbeatOutput{Index: s.latestIndex()}
	setRevisionPrecondition(&out.ETag, &out.XGCRevision, after.Revision)
	out.Body.Status = "renewed"
	out.Body.LeaseScope = workerLeaseScopeBeadMetadata
	out.Body.ClaimedAt = strings.TrimSpace(after.Metadata[beadmeta.ClaimedAtMetadataKey])
	out.Body.LeaseOwner = assignee
	return out, nil
}

// humaHandleWorkerRelease serves DELETE /v0/city/{cityName}/worker/claim.
//
// Delegates to beads.ConditionalAssignmentReleaser, which clears the
// assignment only while that identity still holds it and returns the bead to
// open. A caller that is not the holder gets 200 + "skipped": reporting a
// stranger's failed release as an error would mask the reason the caller is
// unwinding, and reporting it as released would strand a bead another seat
// owns.
func (s *Server) humaHandleWorkerRelease(_ context.Context, input *WorkerReleaseInput) (*WorkerReleaseOutput, error) {
	id, assignee, err := workerSessionIntent(input.Body)
	if err != nil {
		return nil, err
	}
	store, _, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return nil, apierr.NotImplemented.Msg("worker release: the resolved store does not implement ConditionalAssignmentReleaser; refusing to emulate a conditional release with a read-then-write")
	}
	released, err := releaser.ReleaseIfCurrent(id, assignee)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("worker release: bead " + id + " not found")
		}
		return nil, apierr.Internal.Msg("worker release: " + err.Error())
	}
	bead, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("worker release: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg("worker release: reading the released bead: " + err.Error())
	}
	out := &WorkerReleaseOutput{Index: s.latestIndex()}
	out.Body.Status = "skipped"
	if released {
		out.Body.Status = "released"
	}
	out.Body.Bead = bead
	return out, nil
}

// humaHandleWorkerClose serves POST /v0/city/{cityName}/worker/close.
//
// This is the ADR-0009 close gate moved to the server (cr-gdeav.5.2 §4: the
// gate is a property of the write, not of the CLI). Today the work record is
// enforced only client-side, so every close made from off the host bypasses it
// — which is exactly the untyped close the city's audit cannot cite.
//
// OWNERSHIP IS PART OF THE FENCE. The draft fenced only the observed revision,
// so any caller that could name the bead could close it; the record it wrote
// would then attribute the work to whichever identity it typed. The close
// therefore requires the SAME identity the claim took (assignee, BEADS_ACTOR on
// the CLI), refuses a stranger with a 409, and — when the caller names a
// session and the bead carries one — refuses a different session of the same
// name. That ownership check is not a check-then-act race because it is a
// check of the row whose revision the terminal write then CASes against: a
// release, a re-claim, or any other move between the read and the close makes
// the revision differ and the close fails with 409 writing nothing.
//
// The record and the status flip land in ONE write through
// beads.AtomicConditionalCloser, whose contract is "the metadata and the close
// commit together, or neither". A store that cannot prove that gets a 501; the
// alternative — set metadata, then close — is the split write that produces a
// closed bead with no work record. The close reason rides in that same map
// under "close_reason" (see workerCloseReasonMetadataKey), which is how the
// reason becomes durable in a store whose Bead row has no reason column.
//
// WHAT THIS HANDLER DOES NOT CHECK: that a shipped commit is an ancestor of
// its branch. That is a git question and the API server may not host the rig
// the commit lives in (README open question 3). Presence is the server's rule,
// reachability stays with the caller's git until the review lane decides
// otherwise.
func (s *Server) humaHandleWorkerClose(_ context.Context, input *WorkerCloseInput) (*WorkerCloseOutput, error) {
	id := strings.TrimSpace(input.Body.BeadID)
	if id == "" {
		return nil, apierr.InvalidRequest.Msg("worker close requires bead_id")
	}
	assignee := strings.TrimSpace(input.Body.Assignee)
	if assignee == "" {
		return nil, apierr.InvalidRequest.Msg("worker close requires the assignee that holds the claim; a close that names no holder cannot be told apart from a stranger closing someone else's bead")
	}
	record, err := workerCloseRecord(*input)
	if err != nil {
		return nil, err
	}
	store, bead, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	if err := workerOwnership(bead, assignee, strings.TrimSpace(input.Body.SessionID), "worker close"); err != nil {
		return nil, err
	}
	closer, ok := beads.AtomicConditionalCloserFor(store)
	if !ok {
		return nil, apierr.NotImplemented.Msg("worker close: the resolved store cannot prove the metadata write and the close share one transaction; refusing to write them as two")
	}
	if strings.EqualFold(strings.TrimSpace(bead.Status), "closed") {
		// A retry after a dropped response: the holder, the record and the
		// close all already agree, so the second call reports the first rather
		// than a conflict or a second revision. Anything else is a stranger
		// trying to overwrite a finished record.
		if !closeRecordAlreadyPresent(bead, record) {
			return nil, apierr.ConflictWrongState.Msg("worker close: bead " + id + " is already closed with a different work record; a close never rewrites a finished record")
		}
		out := &WorkerCloseOutput{Index: s.latestIndex()}
		out.Body.Status = "already_closed"
		out.Body.Bead = bead
		return out, nil
	}
	closed, err := closer.CloseWithMetadataIfMatch(id, bead.Revision, record)
	if err != nil {
		var precondition *beads.PreconditionFailedError
		if errors.As(err, &precondition) {
			return nil, apierr.ConflictConcurrentModify.Msg("worker close: bead " + id + " changed under the close's revision fence")
		}
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("worker close: bead " + id + " not found")
		}
		return nil, apierr.Internal.Msg("worker close: " + err.Error())
	}
	out := &WorkerCloseOutput{Index: s.latestIndex()}
	out.Body.Status = "closed"
	out.Body.Bead = closed
	return out, nil
}

// workerSessionIntent validates the shared claim/heartbeat/release intent
// shape. Both halves are required: a verb that names a bead but no identity has
// nothing to compare, and a verb that names an identity but no bead is a typo
// that would otherwise reach the by-id resolver as an empty path.
func workerSessionIntent(body workerSessionBody) (id, assignee string, err error) {
	id = strings.TrimSpace(body.BeadID)
	assignee = strings.TrimSpace(body.Assignee)
	if id == "" || assignee == "" {
		return "", "", apierr.InvalidRequest.Msg("worker verb requires both bead_id and assignee")
	}
	return id, assignee, nil
}

// workerFence is the row a lease stamp is allowed to land on: the bead, its
// revision, and the metadata already on it.
type workerFence struct {
	bead beads.Bead
}

// readWorkerFence reads the row the lease write will CAS against and refuses
// anything the caller does not own. Read + conditional write is the shape
// beads.ConditionalWriter is specified for (internal/beads/beads.go:210-249):
// the read picks the expected revision, the write is where the guarantee lives.
func readWorkerFence(store beads.Store, id, assignee, verb string) (workerFence, error) {
	bead, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return workerFence{}, apierr.BeadNotFound.Msg(verb + ": bead " + id + " not found")
		}
		return workerFence{}, apierr.Internal.Msg(verb + ": reading the bead: " + err.Error())
	}
	if err := workerOwnership(bead, assignee, "", verb); err != nil {
		return workerFence{}, err
	}
	return workerFence{bead: bead}, nil
}

// workerOwnership is the one holder check the family shares, so claim,
// heartbeat and close cannot drift into three different definitions of "mine".
// A session is compared only when BOTH sides name one: a bead the hook claim
// stamped with a session cannot be worked by a different session wearing the
// same pool name, and a bead with no session stamp carries no more information
// than the assignee already gives.
func workerOwnership(bead beads.Bead, assignee, sessionID, verb string) error {
	if strings.TrimSpace(bead.Assignee) != assignee {
		return apierr.ConflictWrongState.Msg(verb + ": bead " + bead.ID + " is held by " + quotedOrEmpty(bead.Assignee) + ", not " + quotedOrEmpty(assignee))
	}
	if sessionID == "" {
		return nil
	}
	for _, key := range []string{beadmeta.SessionIDMetadataKey, beadmeta.SessionNameMetadataKey} {
		stamped := strings.TrimSpace(bead.Metadata[key])
		if stamped == "" {
			continue
		}
		if stamped == sessionID {
			return nil
		}
	}
	if _, hasSession := bead.Metadata[beadmeta.SessionIDMetadataKey]; !hasSession {
		if _, hasName := bead.Metadata[beadmeta.SessionNameMetadataKey]; !hasName {
			return nil
		}
	}
	return apierr.ConflictWrongState.Msg(verb + ": bead " + bead.ID + " is claimed by session " + strconv.Quote(strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey])) + ", not " + strconv.Quote(sessionID))
}

// workerCloseRecord validates the typed close and projects it onto the
// gc.work_* metadata the close gate reads. The enum values are beadmeta's
// (internal/beadmeta/values.go:77-80); this handler does not own a second
// vocabulary, only the rule that a close without one is refused.
func workerCloseRecord(body WorkerCloseInput) (map[string]string, error) {
	outcome := strings.TrimSpace(body.Body.Outcome)
	switch outcome {
	case beadmeta.WorkOutcomeShipped, beadmeta.WorkOutcomeNoOp, beadmeta.WorkOutcomeBlocked, beadmeta.WorkOutcomeAbandoned:
	default:
		return nil, apierr.InvalidRequest.Msg("worker close needs gc.work_outcome in shipped|no-op|blocked|abandoned, got " + quotedOrEmpty(outcome) + "; an untyped or mistyped close is a record nobody can cite or retype")
	}
	commit := strings.TrimSpace(body.Body.Commit)
	branch := strings.TrimSpace(body.Body.Branch)
	reason := strings.TrimSpace(body.Body.Reason)
	if outcome == beadmeta.WorkOutcomeShipped && (commit == "" || branch == "") {
		return nil, apierr.InvalidRequest.Msg("a shipped close must name both the commit and the branch it is reachable on; a disposition, dedupe or sweep close is no-op with its reason")
	}
	if outcome != beadmeta.WorkOutcomeShipped && reason == "" {
		return nil, apierr.InvalidRequest.Msg("a " + outcome + " close must state why in reason \u2014 a typed close with no reason is still uncitable")
	}
	record := map[string]string{beadmeta.WorkOutcomeMetadataKey: outcome}
	if commit != "" {
		record[beadmeta.WorkCommitMetadataKey] = commit
	}
	if branch != "" {
		record[beadmeta.WorkBranchMetadataKey] = branch
	}
	if reason != "" {
		record[workerCloseReasonMetadataKey] = reason
	}
	return record, nil
}

// closeRecordAlreadyPresent reports whether every field of the attempted close
// is already on the bead with the same value. It is the idempotence test for a
// retried close, and it is deliberately a subset test rather than an equality
// test: a later writer may add metadata, and that is not a reason to refuse the
// caller's own retry.
func closeRecordAlreadyPresent(bead beads.Bead, record map[string]string) bool {
	for key, want := range record {
		if strings.TrimSpace(bead.Metadata[key]) != want {
			return false
		}
	}
	return len(record) > 0
}

// stampWorkerLease writes the lease keys as ONE revision-fenced metadata write
// against the row that was observed to be held by this caller. Metadata-only,
// so it moves the revision without touching the ownership fence
// (isOwnershipTransition, internal/beads/memstore.go:211-223): a heartbeat must
// not be able to disarm a stale incarnation's guarded release.
//
// Two rules the cr-gdeav.5.5 bounce forced:
//
//   - NO UNCONDITIONAL WRITE, AND NO FALLBACK TO ONE. The write goes through
//     beads.ConditionalWriter.UpdateIfMatch at the fence revision. A
//     *beads.PreconditionFailedError means somebody else moved this bead
//     between the read and the stamp, and the answer is a 409 with the lease
//     keys untouched — never a re-read-and-write-anyway, which is exactly how a
//     stale heartbeat lands gc.lease_owner on a bead a different seat already
//     holds.
//   - gc.claimed_at STAYS WRITE-ONCE (internal/beadmeta/keys.go:54-61): it is
//     written only when absent. gc.lease_owner is the compare-and-overwrite key
//     and carries the holder on every pass.
func stampWorkerLease(writer beads.ConditionalWriter, fence workerFence, assignee, at string) error {
	id := fence.bead.ID
	fields := map[string]string{beadmeta.LeaseOwnerMetadataKey: assignee}
	if strings.TrimSpace(fence.bead.Metadata[beadmeta.ClaimedAtMetadataKey]) == "" {
		fields[beadmeta.ClaimedAtMetadataKey] = at
	}
	if err := writer.UpdateIfMatch(id, fence.bead.Revision, beads.UpdateOpts{Metadata: fields}); err != nil {
		var precondition *beads.PreconditionFailedError
		if errors.As(err, &precondition) {
			return apierr.ConflictConcurrentModify.Msg("worker verb: bead " + id + " changed under the lease stamp's revision fence; the lease was not written over the newer holder")
		}
		if errors.Is(err, beads.ErrNotFound) {
			return apierr.BeadNotFound.Msg("worker verb: bead " + id + " not found")
		}
		if errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			return apierr.NotImplemented.Msg("worker verb: the resolved store's conditional write is disabled, so the lease could not be fenced; refusing to stamp it unconditionally")
		}
		return apierr.Internal.Msg("worker verb: stamping the lease on " + id + ": " + err.Error())
	}
	return nil
}

// setRevisionPrecondition publishes the store revision as the conditional-write
// token. A store that does not maintain a usable revision (a bd-backed store on
// a bd build that does not emit one leaves it 0, internal/beads/beads.go:160-174)
// publishes NOTHING rather than a token that cannot be compared — advertising a
// CAS that is not there is worse than admitting the gap.
func setRevisionPrecondition(etag, revision *string, rev int64) {
	if rev <= 0 {
		return
	}
	*etag = `"rev-` + strconv.FormatInt(rev, 10) + `"`
	*revision = strconv.FormatInt(rev, 10)
}

func quotedOrEmpty(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(nobody)"
	}
	return strconv.Quote(v)
}

// nowUTC is the lease clock, stated once so claim and heartbeat cannot drift
// apart on formatting: gc.claimed_at is RFC3339 UTC everywhere it is read.
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
