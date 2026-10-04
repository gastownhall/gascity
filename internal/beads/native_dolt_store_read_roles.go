package beads

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The role-accessor front doors for the three issue reads: the detail view, the
// listing and the ready frontier.
//
// The reason is the one the graph and delete front doors already carry, and it
// is sharper here. SearchIssues over the v0 wire serves exactly TWO shapes —
// exact-ids and the parent walk — and refuses every other filter, so every gc
// listing except Children was a hard failure on a served city. GetReadyWork is
// served only as a bridge, and it refuses any WorkFilter.Status other than open
// by name (encode's E-WorkFilter.Status), which is precisely what the old
// two-status Ready loop sent on its second pass: Ready failed outright.
// issueops.Reader is a required member of the Storage contract, so routing
// through it is the ONE path every backend answers.
//
// WHAT MOVED WITH THE DOOR, stated once because all three reads share it: the
// role is a HIGH-LEVEL request, not a filter. It applies bd's own list policy
// — the closed and pinned status exclusions, the template, gate and infra-type
// suppressions, the ephemeral-plane decision, and a DEFAULT PAGE SIZE — inside,
// where the raw filter this replaced applied none of it. Every one of those is
// a NARROWING, and adopting a narrowing under a re-point would silently drop
// rows gc lists today. So the request below lifts all of them and gc's own
// ListQuery projection (ApplyListQuery) does the exact filtering it already
// did. What that costs is recorded on nativeListLimitPushdown.

// nativeListReadRequest is the shape every gc listing sends.
//
// AllFlag is doing two jobs, and the second is the one that decides the
// spelling. It drops the default STATUS exclusions (closed, pinned, and any
// workspace status in the done or frozen category), and it lifts the
// PINNED-FLAG default: workapi.BuildListFilter stamps Pinned=&false for every
// request that neither sets AllFlag nor names a pinned-selecting status, so a
// named status — even "closed", even the exact status gc asked for — would drop
// a pinned-flagged row that the raw search returned.
//
// Status:"all" IS AN EQUIVALENT SPELLING of that lift, not a second option and
// not a road not taken: BuildListFilter reads it through the same
// splitStatusSelector, and a lone "all" both skips the exclusion default (its
// len(statusParts)==0 arm) and satisfies statusSelectsPinned, so the two
// requests resolve to the same filter, and both cross the wire (`all` and
// `status` are each published). The only other lifts of the pinned half are
// "pinned" and "hooked", and neither lifts the status exclusions, so neither
// is a substitute. AllFlag is stated because it says what it does in the member
// name rather than in a magic status value.
//
// There is no ExcludeStatus member to spell "everything but closed"
// with, so the status axis cannot be pushed down at all; gc's three-value
// projection (mapBdStatus, ListQuery.Matches) is the only definition of it and
// it runs Go-side, where it always did.
//
// The four include flags lift the type and plane suppressions in the same
// spirit. They are stated individually rather than as IncludeAllTypes because
// that member is refused on the wire (E-ListRequest.IncludeAllTypes) and this
// request has to cross it. IncludeInfra alone would already open the ephemeral
// plane — it is strictly wider than IncludeEphemeral, admitting both the infra
// TYPES and the wisps TABLE — but IncludeEphemeral is stated beside it because
// the two are separate promises: one keeps message/agent/role rows, the other
// keeps the plane, and a future change to either upstream must not silently
// take the other with it.
func nativeListReadRequest() issueops.ListRequest {
	return issueops.ListRequest{
		AllFlag:          true,
		IncludeTemplates: true,
		IncludeGates:     true,
		IncludeInfra:     true,
		IncludeEphemeral: true,
	}
}

// Get retrieves a bead by ID through the reader role's detail view.
//
// THE DETAIL VIEW IS THE ONE TOKEN SOURCE. issueops.IssueDetails.Revision is
// the only place the optimistic-concurrency token is published to a client: the
// row's own RowVersion is json:"-" and never crosses a wire, so a store that
// read RowVersion off a served row would hand every guarded write a fabricated
// 0 — which, being a real value that matches an un-mutated legacy row, is the
// one wrong answer a CAS cannot detect. A LIST row publishes no token at all,
// and Bead.Revision is left at zero there deliberately: absent is absent, never
// 0, and a caller that means to act on a row it listed re-reads it here first.
//
// A miss is ErrNotFound on both sides of the door — the role promises it, and
// the wire's problem mapper turns a 404 into the same sentinel — and a backend
// failure passes through unchanged rather than decaying into not-found.
//
// The two include parameters stay off. Bead carries neither dependents nor
// comments, and both are the expensive row lists the detail view exists to
// avoid materializing; the DEPENDENCIES it needs for the parent-child scan are
// hydrated either way.
func (s *NativeDoltStore) Get(id string) (Bead, error) {
	var out Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		reader, err := storage.IssueReader()
		if err != nil {
			return nativeStoreError(id, err)
		}
		details, err := reader.Get(ctx, issueops.GetRequest{ID: id})
		if err != nil {
			return nativeReadNotFound(id, err)
		}
		if details == nil {
			return fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
		bead, err := beadFromNativeIssueDetails(details)
		if err != nil {
			return err
		}
		if bead.ParentID == "" && nativeDetailDependencyWasDropped(details) {
			parentID, err := s.nativeDetailParentFallback(ctx, storage, id)
			if err != nil {
				return err
			}
			bead.ParentID = parentID
		}
		out = bead
		return nil
	})
	return out, err
}

// nativeDetailDependencyWasDropped reports whether the detail view's
// RESOLVED Dependencies list is shorter than the backend's own raw edge
// count -- the one cheap, already-fetched signal that distinguishes "this
// bead really has no outgoing edges" from "the detail view's target-join
// silently dropped one or more it could not resolve" (see
// nativeDetailParentFallback).
//
// DependencyCount is a plain `COUNT(*) FROM dependencies WHERE issue_id = ?`
// (embeddeddolt's counts.go), unfiltered by whether the target resolves, so
// it never drops what Dependencies() drops. A backend that leaves
// DependencyCount nil -- every lightweight Storage double in this package's
// own tests, none of which populate it -- is treated as "nothing to recover"
// rather than as license to call EdgeReader, which is what keeps this gate
// from forcing every such double to implement a role it has no other reason
// to carry.
func nativeDetailDependencyWasDropped(details *issueops.IssueDetails) bool {
	if details == nil || details.DependencyCount == nil {
		return false
	}
	return *details.DependencyCount > int64(len(details.Dependencies))
}

// nativeDetailParentFallback recovers a parent-child edge that the detail
// view's target-RESOLVING Dependencies() dropped, because the far end is not
// a row the detail view's own join can resolve to an issue -- a cross-ledger
// molecule parent, most often, where nothing reconciles the two ledgers and
// this store never minted the row. beadFromNativeIssueDetails's doc comment
// explains why the detail view alone cannot answer this: its dependency list
// is the far-end ISSUES, not the edge rows, so an unresolvable target is not
// represented there at all, parent or not.
//
// EdgeReader answers from the EDGE instead, through issueops.DepTargetExpr,
// which COALESCEs the three target spellings (an issue id, a wisp id, or
// depends_on_external) -- so a genuinely foreign or unresolvable target
// crosses here exactly as a resolvable one does. It is the same door DepList's
// DOWN leg and DepMetadata already ride for this reason.
//
// Only called when the detail view produced no ParentID at all: a bead that
// legitimately has none pays one extra lookup confirming that, rather than
// Get silently trusting an empty field that might mean "dropped".
func (s *NativeDoltStore) nativeDetailParentFallback(ctx context.Context, storage beadslib.Storage, id string) (string, error) {
	reader, err := storage.EdgeReader()
	if err != nil {
		return "", nativeStoreError(id, err)
	}
	result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{
		IDs:   []string{id},
		Types: []issueops.DependencyType{issueops.DepParentChild},
	})
	if err != nil {
		return "", nativeStoreError(id, err)
	}
	for _, anchor := range result.Anchors {
		if anchor.ID != id || anchor.Missing {
			continue
		}
		for _, edge := range anchor.Edges {
			if edge == nil {
				continue
			}
			return edge.DependsOnID, nil
		}
	}
	return "", nil
}

// List returns beads matching the query, through the reader role's listing.
func (s *NativeDoltStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	var out []Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		reader, err := storage.IssueReader()
		if err != nil {
			return err
		}
		page, err := reader.List(ctx, nativeListRequestFromListQuery(query))
		if err != nil {
			return err
		}
		s.noteRows(len(page.Items))
		beads := make([]Bead, 0, len(page.Items))
		for _, row := range page.Items {
			bead, err := beadFromNativeIssueRow(row)
			if err != nil {
				if isNativeIssueMetadataParseError(err) {
					continue
				}
				return err
			}
			beads = append(beads, bead)
		}
		out = ApplyListQuery(beads, query)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Ready returns open, unblocked actionable beads through the reader role.
//
// G10, THE DEFERRED RE-EXPRESSION. This used to be a loop over two bd statuses.
// The wire publishes no status on the ready operation and
// workapi.BuildReadyFilter stamps StatusOpen for every caller of the role, so
// the second pass — status=deferred, keeping only the rows whose defer_until
// had already expired — is not askable here. It is also not NEEDED, and that is
// a measured fact rather than an assumption: every backend runs the lazy
// defer-wake sweep in its own write transaction immediately before the ready
// read (issueops.WakeExpiredDefersInTx, called from
// internal/storage/{dolt,embeddeddolt}/queries.go and the unit-of-work reader),
// and that sweep flips exactly the rows the second pass was looking for —
// status='deferred' AND defer_until <= now — to open. They arrive in the FIRST
// answer, already woken. What stays hidden is what stayed hidden before: an
// indefinite deferral, which carries no defer_until and which the sweep
// therefore never touches.
//
// THE ONE RESIDUE is a READ-ONLY store, where the sweep declines to run (it is
// a write) and an expired deferral keeps its status. gc no longer resurfaces it
// there. Neither does `bd ready` against the same store, which is the point:
// this door answers what bd answers, and the old second pass was gc diverging
// from it.
//
// IncludeDeferred stays OFF. It looks like the natural spelling for "let the
// deferred rows through and filter them here", and it is not: turning it on
// also drops the deferred-CHILD exclusion (sqlbuild.BuildReadyWorkWhere
// consults ReadyWorkWhereInputs.DeferredChildIDs only while the flag is off),
// which admits the children of a future-deferred parent — rows carrying no
// defer_until of their own, that no Go-side filter here can identify.
func (s *NativeDoltStore) Ready(queries ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(queries)
	var out []Bead
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		reader, err := storage.IssueReader()
		if err != nil {
			return err
		}
		page, err := reader.Ready(ctx, nativeReadyRequestFromReadyQuery(q))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var beads []Bead
		seen := make(map[string]bool, len(page.Items))
		for _, row := range page.Items {
			bead, err := beadFromNativeIssueRow(row)
			if err != nil {
				return err
			}
			if !IsReadyCandidateForTier(bead, now, q.TierMode) || seen[bead.ID] {
				continue
			}
			seen[bead.ID] = true
			beads = append(beads, bead)
		}
		// Work-outcome filtering must see the full candidate set before the
		// limit is applied — a candidate near the front can be vetoed below,
		// and truncating first would under-fill the result instead of
		// backfilling from the candidates behind it (mirrors BdStore.Ready's
		// candidates-then-filter-then-limit order).
		beads, err = s.filterReadyByWorkOutcome(ctx, storage, beads)
		if err != nil {
			return err
		}
		if q.Limit > 0 && len(beads) > q.Limit {
			beads = beads[:q.Limit]
		}
		out = beads
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nativeReadEdgesChunkSize bounds how many anchor ids one ReadEdges call
// carries. It is the shared nativeServerEdgeAnchorCap (bd-enterprise
// internal/httpapi/edges.go: maxDependencyAnchors = 100, enforced on both the
// dependencies and dependents edge endpoints as a 400 invalid_argument
// rejecting the WHOLE call, not just the ids past the limit) — confirmed
// against a real bd-serve at the pinned enterprise revision, where a 130-bead
// ready frontier failed outright with no fallback before this chunking
// existed (S5b-portrev item 1). filterReadyByWorkOutcome sends ReadEdges in
// chunks of at most this many candidate ids and merges the anchors, so a
// ready frontier of any size degrades to more calls rather than one failing
// call. Keeping this as an alias of nativeServerEdgeAnchorCap (defined beside
// DepListBatch's identical chunk in native_dolt_store_roles.go) means both of
// this package's ReadEdges chunk sizes move together if the server's cap ever
// changes (S5b-portrev item 2).
const nativeReadEdgesChunkSize = nativeServerEdgeAnchorCap

// nativeReadyEdgeFanoutLimit bounds concurrent per-blocker IssueReader.Get
// calls in filterReadyByWorkOutcome's blocker fetch. It matches the batch
// size gc uses elsewhere for wire fan-out (bounded, not per-candidate) so a
// wide ready frontier does not open one connection per distinct blocker.
const nativeReadyEdgeFanoutLimit = 8

// filterReadyByWorkOutcome removes candidates whose blocking dependencies are
// closed but recorded gc.work_outcome=blocked. The role's own readiness check
// only looks at status==closed, so it does not know that a blocked-outcome
// close should not satisfy a blocking dependency (ga-a7v0ex).
//
// This is a NARROW override on top of an already-authoritative verdict, not a
// from-scratch recompute of blocking status — see BdStore.filterReadyByWorkOutcome
// for the full rationale, which applies identically here. Deliberately NOT
// DependencySatisfied: a candidate is here because the ready read already
// cleared its gating, which is richer than "the target is closed" (a pinned
// blocker satisfies a blocks edge, and a waits-for edge gates on the spawner's
// children rather than the spawner's own status). Only the closed-and-blocked
// case — invisible to the store's own check — may override that verdict.
//
// It runs INSIDE Ready's withReadRetry closure on the caller's ctx/storage, so
// it must not call anything that re-enters withReadRetry.
//
// The read is two role requests whatever the frontier size: a chunked
// EdgeReader read of every candidate's own edges (nativeReadEdgesChunkSize,
// merged across chunks — the server rejects a ReadEdges call outright once its
// anchor count exceeds its own cap, rather than serving the first N and
// reporting the rest missing), and a blocker-outcome read that tries
// IssueReader.List(IDFilter) FIRST and falls back to a fan-out of
// IssueReader.Get calls — one per DISTINCT ready-blocking target id, bounded
// at nativeReadyEdgeFanoutLimit concurrency — only when that List refuses.
//
// List first, not Get-only, because a native (dolt or Postgres) IssueReader
// answers List+IDFilter in one SQL statement regardless of the blocker count,
// and an unconditional per-id Get fan-out against THAT backend was its own
// regression (S5b-fixrev item 1): six-ish round trips per blocker every
// controller tick, the same #6491 pattern the fan-out exists to avoid for an
// http-native backend. Only an http-native IssueReader refuses this shape —
// bd-enterprise's httpstore encode table, E-ListRequest.IDFilter: "an explicit
// id set on the LISTING refuses ... the exact-ids question is getIssue's,
// reached through the SearchIssues bridge's exact-ids shape, not through a
// listing filter" — and it refuses CLIENT-SIDE, before any round trip, with a
// *httpstore.InexpressibleError that unwraps to a typed *beadslib.ErrUnsupported
// (errors.As reaches it through the chain). Get is the wire's door for that
// exact-ids question instead, and it is a per-id door, so fan-out is the
// fallback's only route to it (S5b-portrev item 1 / S5b-fixrev item 1;
// re-ports 1aab10d3d0's ReadEdges chunking and fea3240cd5's probe-then-fallback
// shape, which still applies here even though this call site never had the
// capability-probed batched arm those commits guarded — the probe is now
// List+IDFilter itself, answered or refused in the same call, rather than a
// separate capability check). A per-candidate Get is one round trip each on a
// served store, which over a network link exhausted the read-retry budget
// every controller tick (#6491) — now paid only by the backend that actually
// requires it. A target the read does not return — an external reference,
// another ledger's row — is no evidence of blocking, on either leg.
//
// The result does not depend on edge or fetch order: every fetched target's
// metadata is parsed exactly once up front, and a malformed one is reported
// against the first candidate (in candidate order) that references it,
// choosing the lowest target id when that candidate references more than one.
// A real (non-ErrNotFound) blocker fetch failure fails the whole filter —
// never silently passing the candidate it would have vetoed — and cancels any
// still-queued fan-out calls rather than paying for every remaining blocker
// fetch before reporting an error it already had.
func (s *NativeDoltStore) filterReadyByWorkOutcome(ctx context.Context, storage beadslib.Storage, candidates []Bead) ([]Bead, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	edgeReader, err := storage.EdgeReader()
	if err != nil {
		return nil, fmt.Errorf("checking blocking dependency outcomes: %w", err)
	}
	edges := make(map[string][]*beadslib.Dependency, len(ids))
	for chunkStart := 0; chunkStart < len(ids); chunkStart += nativeReadEdgesChunkSize {
		chunkEnd := min(chunkStart+nativeReadEdgesChunkSize, len(ids))
		read, err := edgeReader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: ids[chunkStart:chunkEnd]})
		if err != nil {
			return nil, fmt.Errorf("checking blocking dependency outcomes: reading dependency edges: %w", err)
		}
		for _, anchor := range read.Anchors {
			if anchor.Missing {
				continue
			}
			edges[anchor.ID] = anchor.Edges
		}
	}
	var blockerIDs []string
	seen := make(map[string]bool)
	for _, id := range ids {
		for _, dep := range edges[id] {
			if dep == nil || !IsReadyBlockingDependencyType(string(dep.Type)) || seen[dep.DependsOnID] {
				continue
			}
			seen[dep.DependsOnID] = true
			blockerIDs = append(blockerIDs, dep.DependsOnID)
		}
	}
	if len(blockerIDs) == 0 {
		return candidates, nil
	}
	reader, err := storage.IssueReader()
	if err != nil {
		return nil, fmt.Errorf("checking blocking dependency outcomes: %w", err)
	}
	vetoes, malformed, err := filterReadyByWorkOutcomeListBlockers(ctx, reader, blockerIDs)
	if err != nil {
		// List+IDFilter is the cheap-first attempt; an http-native IssueReader
		// refuses it client-side, before any round trip, with a typed
		// *beadslib.ErrUnsupported (possibly wrapped in httpstore's own
		// InexpressibleError — errors.As reaches it through either). Anything
		// else is a real failure and must fail the whole filter, exactly as a
		// real Get failure below does.
		var unsupported *beadslib.ErrUnsupported
		if !errors.As(err, &unsupported) {
			return nil, fmt.Errorf("checking blocking dependency outcomes: listing blockers: %w", err)
		}
		vetoes, malformed, err = filterReadyByWorkOutcomeFetchBlockers(ctx, reader, blockerIDs)
		if err != nil {
			return nil, err
		}
	}
	result := make([]Bead, 0, len(candidates))
	for _, c := range candidates {
		blocked := false
		badBlocker := ""
		for _, dep := range edges[c.ID] {
			if dep == nil || !IsReadyBlockingDependencyType(string(dep.Type)) {
				continue
			}
			if _, bad := malformed[dep.DependsOnID]; bad && (badBlocker == "" || dep.DependsOnID < badBlocker) {
				badBlocker = dep.DependsOnID
			}
			if vetoes[dep.DependsOnID] {
				blocked = true
			}
		}
		if badBlocker != "" {
			return nil, fmt.Errorf("checking blocking dependency outcomes for %s: parsing blocker %s metadata: %w", c.ID, badBlocker, malformed[badBlocker])
		}
		if !blocked {
			result = append(result, c)
		}
	}
	return result, nil
}

// filterReadyByWorkOutcomeListBlockers is filterReadyByWorkOutcome's
// cheap-first attempt: one IssueReader.List(IDFilter) call answering every
// blocker's status and metadata at once, instead of a per-id Get fan-out.
//
// A native (dolt or Postgres) IssueReader serves this directly — one SQL
// statement regardless of blocker count. An http-native IssueReader refuses
// IDFilter on a listing unconditionally (bd-enterprise's httpstore encode
// table, E-ListRequest.IDFilter), client-side, before any round trip; the
// caller falls back to filterReadyByWorkOutcomeFetchBlockers on exactly that
// refusal (errors.As to *beadslib.ErrUnsupported).
//
// A blocker id the page does not return — external, another ledger's row, or
// simply absent — is no evidence of blocking, exactly as a Get miss is: it is
// silently left out of both returned maps.
func filterReadyByWorkOutcomeListBlockers(ctx context.Context, reader issueops.Reader, blockerIDs []string) (map[string]bool, map[string]error, error) {
	req := nativeListReadRequest()
	req.IDFilter = strings.Join(blockerIDs, ",")
	page, err := reader.List(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	vetoes := make(map[string]bool, len(blockerIDs))
	malformed := make(map[string]error)
	for _, row := range page.Items {
		if row == nil || row.Issue == nil {
			continue
		}
		metadata, parseErr := metadataMapFromNative(row.Metadata)
		if parseErr != nil {
			malformed[row.ID] = parseErr
			continue
		}
		vetoes[row.ID] = string(row.Status) == "closed" && metadata[beadmeta.WorkOutcomeMetadataKey] == beadmeta.WorkOutcomeBlocked
	}
	return vetoes, malformed, nil
}

// filterReadyByWorkOutcomeFetchBlockers is filterReadyByWorkOutcome's fallback
// leg for an IssueReader that refuses List+IDFilter (http-native): one
// IssueReader.Get call per DISTINCT ready-blocking target, bounded at
// nativeReadyEdgeFanoutLimit concurrency, canceling any still-queued calls on
// the first real (non-ErrNotFound) failure rather than paying for every
// remaining blocker fetch before reporting an error it already had.
//
// The result does not depend on fetch order: every fetched target's metadata
// is parsed exactly once, and a real (non-ErrNotFound) fetch failure fails the
// whole call — never silently dropped in favor of the vetoes/malformed the
// OTHER goroutines managed to record, which would otherwise read as "no
// blockers". A nil result with a nil error violates IssueReader.Get's own
// contract (a miss must be ErrNotFound); treating it as "no evidence of
// blocking" would silently trust a broken backend, and indexing into it would
// panic.
func filterReadyByWorkOutcomeFetchBlockers(ctx context.Context, reader issueops.Reader, blockerIDs []string) (map[string]bool, map[string]error, error) {
	vetoes := make(map[string]bool, len(blockerIDs))
	malformed := make(map[string]error)
	var mu sync.Mutex
	var fetchErr error
	fetchCtx, cancelFetch := context.WithCancel(ctx)
	defer cancelFetch()
	sem := make(chan struct{}, nativeReadyEdgeFanoutLimit)
	var wg sync.WaitGroup
	for _, blockerID := range blockerIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if fetchCtx.Err() != nil {
				// Canceled by an earlier failure while this call was still
				// queued behind the semaphore: skip it without dialing
				// IssueReader at all.
				return
			}
			details, getErr := reader.Get(fetchCtx, issueops.GetRequest{ID: id})
			if getErr != nil {
				if errors.Is(getErr, beadslib.ErrNotFound) {
					// Missing target: no evidence of blocking, not an error.
					return
				}
				mu.Lock()
				if fetchErr == nil {
					fetchErr = fmt.Errorf("checking blocking dependency outcomes: fetching blocker %s: %w", id, getErr)
				}
				mu.Unlock()
				cancelFetch()
				return
			}
			if details == nil {
				// A nil result with a nil error violates IssueReader.Get's
				// contract (a miss must be ErrNotFound); treating it as "no
				// evidence of blocking" would silently trust a broken
				// backend, and indexing into it would panic.
				mu.Lock()
				if fetchErr == nil {
					fetchErr = fmt.Errorf("checking blocking dependency outcomes: fetching blocker %s: IssueReader.Get returned neither a result nor an error", id)
				}
				mu.Unlock()
				cancelFetch()
				return
			}
			metadata, parseErr := metadataMapFromNative(details.Metadata)
			mu.Lock()
			defer mu.Unlock()
			if parseErr != nil {
				malformed[id] = parseErr
				return
			}
			vetoes[id] = string(details.Status) == "closed" && metadata[beadmeta.WorkOutcomeMetadataKey] == beadmeta.WorkOutcomeBlocked
		}(blockerID)
	}
	wg.Wait()
	if fetchErr != nil {
		return nil, nil, fetchErr
	}
	return vetoes, malformed, nil
}

// nativeListRequestFromListQuery projects a ListQuery onto the role's request.
//
// Only the predicates that push down EXACTLY travel: a parent, an assignee, one
// label, the metadata equality map and the created-before bound. Three of gc's
// selectors deliberately stay Go-side, and each has a reason the role's own
// contract supplies:
//
//   - STATUS, because the request has no ExcludeStatus and no way to name
//     "every status but closed", and because any named status re-arms the
//     pinned-flag default. See nativeListReadRequest.
//   - TYPE, because BuildListFilter VALIDATES it against the workspace
//     vocabulary and fails a request naming an unknown one, where the raw
//     filter answered empty — and because naming an INFRA type (agent, role,
//     message) routes the query to the ephemeral plane ALONE, which would hide
//     every durable mail bead. gc's type predicate is exact in ApplyListQuery.
//   - TIER, because the plane knobs only ADMIT. There is no member that
//     excludes the ephemeral rows a TierIssues read must drop, so that filter
//     is ApplyListQuery's, as the wisp-tier filter already was.
func nativeListRequestFromListQuery(query ListQuery) issueops.ListRequest {
	req := nativeListReadRequest()
	limit := nativeListLimitPushdown(query)
	req.Limit = &limit
	switch query.Sort {
	case SortCreatedDesc:
		req.SortBy = "created" // SortDefs["created"] defaults DESC
	case SortCreatedAsc:
		req.SortBy, req.Reverse = "created", true // flip the DESC default
	}
	if query.ParentID != "" {
		req.ParentID = query.ParentID
	}
	if query.Assignee != "" {
		req.Assignee = query.Assignee
	}
	if query.Label != "" {
		req.Labels = []string{query.Label}
	}
	if len(query.Metadata) > 0 {
		req.MetadataFields = query.Metadata
	}
	if !query.CreatedBefore.IsZero() {
		req.CreatedBefore = zeroTimePtr(query.CreatedBefore)
	}
	return req
}

// nativeListLimitPushdown reports the row limit to send, or 0 for the whole
// candidate set. It is ALWAYS sent, and that is the point of returning a value
// rather than a maybe: a nil ListRequest.Limit is not "unlimited", it is
// workapi.DefaultListLimit — fifty rows — so an unstated limit turns every
// unbounded gc listing into a silent 50-row page.
//
// A limit may only ride down when the backing set IS the query set, because the
// backing cuts its page before ApplyListQuery runs and a residual filter would
// then be dropping rows out of an already-truncated page. Under the request
// above that means a query that admits every status, names no type and reads
// both tiers — which is the shape the aggregate readers that fought for this
// pushdown already use (orders' RecentRuns/LastRun and the event cursor, all
// Label + IncludeClosed + TierBoth).
//
// THE COST THIS BUYS, recorded rather than discovered later: a durable-tier or
// typed or not-closed listing now fetches the whole candidate set and cuts the
// page in Go. That is the same class of cost as the wire's own L2 (a non-created
// display order pages to exhaustion), and it is the price of not narrowing. It
// ends when the request can spell an ExcludeStatus, a durable-plane selector and
// a non-validating type filter — the upstream ask this re-point files.
func nativeListLimitPushdown(query ListQuery) int {
	if query.Limit <= 0 {
		return 0
	}
	// The status, type and tier predicates below run only in ApplyListQuery, so
	// a backing limit under any of them cuts rows before the residual filter.
	if query.Status != "" || !query.IncludeClosed || query.Type != "" || query.TierMode != TierBoth {
		return 0
	}
	// SeekAfter, UpdatedBefore and plural Assignees are Go-side too
	// (ListQuery.Matches), and each is a residual filter for the same reason.
	if query.SeekAfter != nil || !query.UpdatedBefore.IsZero() || len(query.Assignees) > 0 {
		return 0
	}
	switch query.Sort {
	case SortCreatedAsc:
		// The backing renders created-asc ties as `id ASC`, matching the
		// canonical (created_at ASC, id ASC) order, so a bounded asc read is exact.
		return query.Limit
	case SortCreatedDesc:
		// The backing renders created-desc ties as `id ASC` (upstream
		// sqlbuild.OrderBy hardcodes the id tie-break), but Gas City's canonical
		// order and cursor continuation break created_at ties by `id DESC`
		// (sortBeadsForQuery / SeekBoundary.After). A bounded desc read therefore
		// keeps the smaller-id tie members at the boundary and drops the larger-id
		// ties, so an exact or cursor-paginated caller loses rows across the page
		// seam. Only push the limit when the caller opted into a bounded
		// newest-by-created_at sample (aggregates); otherwise fetch the full set
		// and let ApplyListQuery cut the exact (created_at DESC, id DESC) prefix.
		if query.AllowBackingCreatedLimit {
			return query.Limit
		}
		return 0
	default:
		// An unsorted request has no order to cut a stable prefix from: the wire
		// welds the listing to created order (its serverFixed SortBy) while a
		// local backend answers in its own default, so a bounded unsorted read
		// would return a different page per backend.
		return 0
	}
}

// nativeReadyRequestFromReadyQuery projects a ReadyQuery onto the role's
// request. The limit is always stated for the same reason the listing's is —
// nil means workapi.DefaultReadyLimit, a hundred rows — and it is stated as 0:
// gc filters its own excluded types and tiers out of the answer before applying
// the caller's limit, so a backing page would cut the candidates first.
func nativeReadyRequestFromReadyQuery(q ReadyQuery) issueops.ReadyRequest {
	unlimited := 0
	req := issueops.ReadyRequest{
		Limit: &unlimited,
		// The ready order is a POLICY rather than a display order: it decides
		// which rows a truncation KEEPS, not just how they print. It is stated
		// as the concrete `hybrid` the raw WorkFilter this replaced resolved to
		// by leaving SortPolicy empty — never omitted, because an absent policy
		// is hybrid to the storage layer and `priority` to the wire's handler,
		// and those two answer with different item SETS. gc re-sorts nothing
		// here, so a silent flip between them would change which work the
		// dispatcher is offered.
		Sort: "hybrid",
	}
	if q.TierMode == TierBoth || q.TierMode == TierWisps {
		req.IncludeEphemeral = true
	}
	if q.Assignee != "" {
		req.Assignee = q.Assignee
	}
	return req
}

// beadFromNativeIssueDetails converts the reader role's detail view.
//
// The detail view carries its labels and its edges BESIDE the row rather than
// on it, and its dependency list is the far-end ISSUES rather than the edge
// rows — so an edge whose target is an "external:" reference or an id in
// another repository is not represented here. DepList remains the edge-row door
// for the callers that walk those; this one exists to answer "what is this
// bead", and its parent-child scan reads a target that is always resident.
func beadFromNativeIssueDetails(details *issueops.IssueDetails) (Bead, error) {
	if details == nil {
		return Bead{}, nil
	}
	issue := details.Issue
	if len(details.Labels) > 0 {
		issue.Labels = details.Labels
	}
	issue.Dependencies = nil
	for _, dep := range details.Dependencies {
		if dep == nil {
			continue
		}
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     details.ID,
			DependsOnID: dep.ID,
			Type:        dep.DependencyType,
		})
	}
	bead, err := beadFromNativeIssue(&issue)
	if err != nil {
		return Bead{}, err
	}
	// issueops.IssueDetails.Revision is a decimal string wire token in beads
	// v1.3.x (RevisionToken), not the int64 Bead.Revision carries. Parse it
	// when the detail view actually published one; an empty token means this
	// row never crossed the wire (an in-process row), so leave the
	// RowVersion stamp beadFromNativeIssue already set rather than fencing a
	// guarded write against a parsed zero that means "absent".
	if details.Revision != "" {
		rev, err := strconv.ParseInt(details.Revision, 10, 64)
		if err != nil {
			return Bead{}, fmt.Errorf("beadFromNativeIssueDetails: parsing revision token %q: %w", details.Revision, err)
		}
		bead.Revision = rev
	}
	return bead, nil
}

// beadFromNativeIssueRow converts one row of a listing or a ready page.
//
// Bead.Revision is cleared, not merely left alone: the page publishes no token,
// and the row's RowVersion — zero on every served row, because it never crosses
// the wire — would fence a guarded write against a value that means "absent"
// while reading as the real revision 0. beadFromNativeIssue stamps it for the
// detail view, so the row path has to undo that rather than rely on a zero.
//
// The computed parent beside the row is preferred over the edge scan when the
// row carries one: a page hydrates it with a dedicated join, and a projection
// that dropped the edge list would otherwise lose the parent with it.
func beadFromNativeIssueRow(row *issueops.IssueWithCounts) (Bead, error) {
	if row == nil || row.Issue == nil {
		return Bead{}, nil
	}
	bead, err := beadFromNativeIssue(row.Issue)
	if err != nil {
		return Bead{}, err
	}
	bead.Revision = 0
	if row.Parent != nil && *row.Parent != "" {
		bead.ParentID = *row.Parent
	}
	return bead, nil
}

// nativeReadNotFound maps a read refusal onto gc's own not-found sentinel.
//
// The role's miss is issueops.ErrNotFound on both routes — a local probe of
// both planes and the wire's `not_found` problem code answer with the same
// value — so the classification is an errors.Is rather than the message
// matching the raw doors still need. A backend failure passes through
// unchanged: the role promises a transport error never decays into a miss, and
// a caller that read one as "the bead is gone" would tear down live work.
func nativeReadNotFound(id string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, issueops.ErrNotFound) {
		return fmt.Errorf("bead %q: %w: %w", id, ErrNotFound, err)
	}
	return nativeStoreError(id, err)
}
