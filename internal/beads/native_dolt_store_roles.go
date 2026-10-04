package beads

import (
	"context"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The role-accessor front doors for the graph edges, deletion and reachability.
//
// Each of these replaced a raw beadslib.Storage call, and the reason is the
// same for all of them: the raw method is off the v0 served surface. A store
// reached over the wire refuses AddDependency, RemoveDependency,
// GetDependenciesWithMetadata, GetDependentsWithMetadata, DeleteIssue and
// DeleteIssues outright, so every one of these front doors was a hard failure
// on a served city. The role accessors are required members of the Storage
// contract — every backend implements all twenty-eight — so routing through
// them is not a fork of behavior by backend, it is the ONE path that all of
// them answer.
//
// Ping is the one that was not failing and moved anyway. GetStatistics IS
// served, but as the off-role bridge whose own documentation says it must never
// hard-fail: it swallows a busy server into a degraded answer, which is exactly
// the signal a reachability probe exists to report. StatsReporter.Stats is the
// role beneath it and reports its transport errors.

// maxNativeDeleteIDs bounds one Deleter request. The role takes a whole id list
// and gc's wisp GC hands it a molecule closure, which is unbounded; chunking
// here keeps a single oversized teardown from being refused as one request.
const maxNativeDeleteIDs = 1000

// DepAdd records a dependency between two beads through the dependency editor.
func (s *NativeDoltStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	editor, err := storage.DependencyEditor()
	if err != nil {
		return nativeStoreError(issueID, err)
	}
	_, err = editor.AddDependencies(ctx, issueops.AddDependenciesRequest{
		Actor: s.actor,
		Edges: []issueops.DependencyEdge{{
			IssueID:     issueID,
			DependsOnID: dependsOnID,
			// The raw AddDependency defaulted an empty type to "blocks" inside
			// the storage layer; the role validates the type instead, so the
			// default has to be applied here or a caller that omits it writes
			// an empty type onto the wire.
			Type: nativeGraphApplyDependencyType(depType),
		}},
	})
	return nativeStoreError(issueID, err)
}

// DepRemove removes a dependency between two beads through the dependency
// editor.
func (s *NativeDoltStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	editor, err := storage.DependencyEditor()
	if err != nil {
		return nativeStoreError(issueID, err)
	}
	_, err = editor.RemoveDependency(ctx, issueops.RemoveDependencyRequest{
		Actor:       s.actor,
		IssueID:     issueID,
		DependsOnID: dependsOnID,
	})
	return nativeStoreError(issueID, err)
}

// DepList returns dependencies for a bead.
//
// The two directions are two different questions and they are answered by two
// different roles, which is the whole shape of this method rather than an
// implementation detail:
//
//   - DOWN asks for the anchor's OWN EDGES. EdgeReader answers with the stored
//     rows, so an edge whose target is an "external:" reference or an id in
//     another repository survives — Relations would drop it, because it answers
//     with the issue on the far end and there is none. gc's dependency walks
//     read those edges.
//   - UP asks WHO DEPENDS ON the anchor. That is a neighbor question, and it is
//     the direction EdgeReader cannot answer at all: its read is source-keyed.
//
// Direction is passed to Relations explicitly because its zero value is
// invalid by design: a silent default is how a caller asks for "what blocks
// this" and is handed the exact inverse graph, with the same shape and no error.
func (s *NativeDoltStore) DepList(id, direction string) ([]Dep, error) {
	var out []Dep
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		deps, err := s.depList(ctx, storage, id, direction)
		if err != nil {
			return err
		}
		out = deps
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *NativeDoltStore) depList(ctx context.Context, storage beadslib.Storage, id, direction string) ([]Dep, error) {
	if direction == "up" {
		relations, err := storage.IssueRelations()
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		issues, err := relations.Related(ctx, issueops.RelatedRequest{
			ID:        id,
			Direction: issueops.RelationIn,
		})
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		deps := make([]Dep, 0, len(issues))
		for _, issue := range issues {
			if issue == nil {
				continue
			}
			deps = append(deps, Dep{
				IssueID:     issue.ID,
				DependsOnID: id,
				Type:        string(issue.DependencyType),
			})
		}
		return deps, nil
	}
	reader, err := storage.EdgeReader()
	if err != nil {
		return nil, nativeStoreError(id, err)
	}
	result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: []string{id}})
	if err != nil {
		return nil, nativeStoreError(id, err)
	}
	for _, anchor := range result.Anchors {
		if anchor.ID != id {
			continue
		}
		// A batch read reports a miss PER ANCHOR rather than failing the call.
		// Collapsing that into an empty slice would make a typo read as a clean
		// graph, which is the one thing every caller of this list acts on.
		if anchor.Missing {
			return nil, fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
		return anchorDeps(id, anchor), nil
	}
	return nil, fmt.Errorf("bead %q: %w", id, ErrNotFound)
}

// anchorDeps projects one anchor's stored edges into gc's Dep shape.
//
// The near id is the one the CALLER named rather than the row's own IssueID: the
// stored target may be spelled three ways and the anchor is what the request
// asked about, so attributing the edges to anything else is how a batch answer
// gets mis-keyed.
func anchorDeps(id string, anchor issueops.AnchorEdges) []Dep {
	deps := make([]Dep, 0, len(anchor.Edges))
	for _, edge := range anchor.Edges {
		if edge == nil {
			continue
		}
		deps = append(deps, Dep{
			IssueID:     id,
			DependsOnID: edge.DependsOnID,
			Type:        string(edge.Type),
		})
	}
	return deps
}

// nativeDepListBatchChunk caps how many anchors ride one read, matching
// DoltliteReadStore's cap beside it. It bounds the statement the backend builds
// without giving back the round-trip saving the batch exists for.
const nativeDepListBatchChunk = 500

// DepListBatch returns the DOWN edges of many anchors in one round trip.
//
// It is the batch shape of DepList's DOWN leg and rides the same
// EdgeReader.ReadEdges, which is a batch contract by construction rather than an
// optimization of a single read: the anchors' existence and their edges come
// from ONE read transaction, so the answer is one consistent snapshot instead of
// N snapshots stitched together.
//
// WHY IT EXISTS. The per-anchor loop callers ran instead cost one read
// transaction per bead — and, against a served store whose pool has gone cold or
// whose link drops handshakes, one CONNECT per bead. A walk over a few hundred
// beads then scales as (anchors x round trip) with withReadRetry's budget as the
// only ceiling, which is what made `gc storage recover-stranded` unable to
// finish over the Tailscale work-store path (ga-50tsx). Every other store in the
// tree already answers this question in one call; this one was the outlier, so
// internal/dispatch's scope-skip walk fell back to its per-id loop here too.
//
// MISS SEMANTICS ARE THE ONE PLACE IT CANNOT COPY DepList. A batch that answered
// ErrNotFound for one absent id would throw away the answers for every id it did
// find, so an anchor this store does not hold gets NO ENTRY — the same rule
// MemStore, FileStore, BdStore and DoltliteReadStore follow. An anchor that IS
// held and has no edges gets an entry carrying an empty slice, which is more
// than the thin stores report and less than a caller may rely on across store
// types: presence in this map is not a portable existence check.
//
// A FAILED CHUNK FAILS THE CALL. The partial map is dropped rather than returned
// alongside the error, because a caller that walks it reads the anchors the
// failure cost it as beads with no edges — a dropped link presenting as a clean
// graph is the one answer no dependency walk can survive.
func (s *NativeDoltStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	out := make(map[string][]Dep, len(ids))
	for start := 0; start < len(ids); start += nativeDepListBatchChunk {
		end := min(start+nativeDepListBatchChunk, len(ids))
		chunk := ids[start:end]
		err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
			reader, err := storage.EdgeReader()
			if err != nil {
				return err
			}
			result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: chunk})
			if err != nil {
				return err
			}
			for _, anchor := range result.Anchors {
				if anchor.Missing {
					continue
				}
				out[anchor.ID] = anchorDeps(anchor.ID, anchor)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("listing the dep edges of %d anchor(s) starting at %q: %w", len(chunk), chunk[0], err)
		}
	}
	return out, nil
}

// DepMetadata reads the opaque payload one graph-apply edge retained.
//
// It rides the SAME EdgeReader.ReadEdges that DepList's DOWN leg does, because
// the payload is a column of the edge ROW and the row door is the only door
// that publishes it. Relations answers with the issue on the far end and drops
// an edge that has none — which is exactly the "external:" reference and the
// foreign-repository id a graph-apply edge most often annotates, so a neighbor
// read would report "no payload" for the very edges the payload exists for.
//
// PARITY WITH A SERVED STORE IS VERBATIM, not conditional. Both legs select the
// row's metadata column against issueops.DepTargetExpr — which COALESCEs the
// three target spellings, so an unresolved or bare-slug target crosses as
// stored — and hand the rows to the same scanner. A store reached over the wire
// therefore answers the same rows the embedded backend does, and this front
// door needs no capability fork. (bd-enterprise ga-b8ddd.30 measured it as a
// dual run; before that flip the served leg refused the read outright.)
//
// ABSENCE IS ABSENCE ON THREE COUNTS — the anchor is not there, the anchor
// holds no such edge, or the edge exists and carried no payload — and all three
// are ("", false, nil), matching the SQLite front door beside it. The
// (string, bool) shape already has an absent channel, so none of them is an
// error, and the three need no separate branch: a missing anchor carries no
// edges by contract, so it falls out of the same walk. That is the one place
// this method departs from DepList, which reports a missing anchor as
// ErrNotFound: its callers WALK a graph, where a typo reading as a clean graph
// is the dangerous answer, and this method's callers look up one edge's payload
// by a target they already hold.
//
// A pair can hold more than one row (one per dep type), and the first CARRYING
// row wins, per DepMetadataCarries: Dolt defaults an edge's metadata column to
// an empty JSON object, which carries nothing, so a raw non-empty test would
// report a payload on every Dolt-sourced edge. That is SQLiteStore.DepMetadata's
// contract, to the letter, because the two are read through one interface.
//
// The answer is taken from the REQUESTED anchor and no other. The served leg
// regroups a flat edge array back into anchors by hand, so a mis-keyed group is
// a live failure mode, and the id it would attribute the payload to is the one
// thing this method must not get wrong.
func (s *NativeDoltStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	var payload string
	var carried bool
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		reader, err := storage.EdgeReader()
		if err != nil {
			return nativeStoreError(issueID, err)
		}
		result, err := reader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: []string{issueID}})
		if err != nil {
			return nativeStoreError(issueID, err)
		}
		payload, carried = "", false
		for _, anchor := range result.Anchors {
			if anchor.ID != issueID {
				continue
			}
			for _, edge := range anchor.Edges {
				if edge == nil || edge.DependsOnID != dependsOnID || !DepMetadataCarries(edge.Metadata) {
					continue
				}
				payload, carried = edge.Metadata, true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return payload, carried, nil
}

// Delete permanently removes a bead through the deleter role.
func (s *NativeDoltStore) Delete(id string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if err := s.deleteIDs([]string{id}); err != nil {
		return nativeStoreError(id, err)
	}
	if sidecarErr := s.localStrings.DeleteBead(id); sidecarErr != nil {
		return fmt.Errorf("deleting bead %q: cleaning up local strings: %w", id, sidecarErr)
	}
	return nil
}

// DeleteBatch removes exactly the given ids, orphaning dependents outside the
// batch. See BatchDeleter: this is `bd delete <ids...> --force`, never
// --cascade, because the wisp GC collects an ownership closure and must not
// reach live work outside it.
func (s *NativeDoltStore) DeleteBatch(ids []string) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	var committed []string
	for start := 0; start < len(ids); start += maxNativeDeleteIDs {
		end := min(start+maxNativeDeleteIDs, len(ids))
		chunk := ids[start:end]
		if err := s.deleteIDs(chunk); err != nil {
			// The sidecar of what DID land is swept before the failure is
			// reported, and it is reported either way. The clone-local strings
			// live outside the ledger, so nothing else ever collects them: an
			// id named in BatchDeleteError.Committed is one the caller is about
			// to tombstone, and its sidecar entry would outlive every trace of
			// the bead it belongs to. A sweep failure is joined to the delete
			// failure rather than replacing it, because the delete is the one
			// the caller retries on.
			return &BatchDeleteError{Committed: committed, Err: errors.Join(err, s.sweepLocalStrings(committed))}
		}
		committed = append(committed, chunk...)
	}
	return s.sweepLocalStrings(ids)
}

// sweepLocalStrings drops the clone-local sidecar rows of beads that are gone.
func (s *NativeDoltStore) sweepLocalStrings(ids []string) error {
	for _, id := range ids {
		if sidecarErr := s.localStrings.DeleteBead(id); sidecarErr != nil {
			return fmt.Errorf("deleting bead %q: cleaning up local strings: %w", id, sidecarErr)
		}
	}
	return nil
}

var _ BatchDeleter = (*NativeDoltStore)(nil)

// deleteIDs is the single Deleter call both delete front doors share.
//
// Force is set and Cascade is not, and neither is a preference. The raw
// DeleteIssue this replaced applied no dependents guard at all, so an unforced
// role call would start refusing deletes gc performs today — the role's guard
// is the role's, and adopting it here would be a behavior change smuggled in
// under a re-point. Cascade would go the other way and delete rows the caller
// never named.
func (s *NativeDoltStore) deleteIDs(ids []string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	deleter, err := storage.Deleter()
	if err != nil {
		return err
	}
	_, err = deleter.Delete(ctx, issueops.DeleteRequest{
		Actor: s.actor,
		IDs:   ids,
		Force: true,
	})
	return err
}

// Ping verifies that the upstream storage is reachable.
//
// It goes through withReadRetry like every other read on this store, and that
// is load-bearing rather than tidy (council B-F2). A Ping that reached
// acquireStorage directly sat outside BOTH mechanisms the proxied lane depends
// on:
//
//   - It did not honor poolStale. The guard tick's re-pin is adoptPin plus
//     markPoolStale, and the property the root-move row asserts is that no read
//     is served from the old generation before the mark is honored. A Ping is a
//     read, and it was served from the old generation's pool — so on the H7
//     root-move shape, where the old socket is still alive and serving the MOVED
//     database, `gc doctor` pinged the moved database, got a clean answer, and
//     reported the scope healthy after the tick already knew the generation had
//     changed.
//   - Its failures were never classified. proxiedReadVerdict and
//     proxiedReadBudgetVerdict are reached only from withReadRetry, so a Ping
//     against a dead proxy returned a raw driver error, ProxiedStore.Ping's
//     classifyReadError found no verdict, and a handle every other read would
//     have demoted stayed "native".
//
// It is also the lane's most-repeated read: P2-09 re-points ProxiedStore.Ping
// at this method so doctor's per-scope health check costs zero forks, where
// before PR2 it was BdStore.Ping — a `bd list --limit 0` carrying bd's own
// transient-read recovery. Swapping a hardened read for an unhardened one on
// that path is the trade this fixes.
//
// # The lane gate, and why Ping needs its own (council pr2 D-F1)
//
// Both reasons above are about the PROXIED lane, and routing Ping through
// withReadRetry unconditionally reintroduced on Ping the exact flag-off
// regression the lane gate on rungs 2 and 3 exists to prevent. Rung 7's
// substring table contains "dial tcp" and "connection refused" and applies on
// BOTH lanes, so a failing Ping on a direct/hosted handle became
// nativeReadTransient → reconnect → the injected reopen hook, which re-resolves
// the managed env with recovery enabled and can restart a city's Dolt server —
// looping on a context.Background()-derived 90s budget no caller deadline can
// cancel. On main a Ping was one acquireStorage plus one GetStatistics and
// returned on the first pass.
//
// Three shipping callers are built on that fail-fast:
// waitForRigStoreAccessible (cmd/gc/cmd_rig.go) and
// waitForBeadsScopeReadyAfterRecovery (cmd/gc/beads_provider_lifecycle.go) both
// poll `Ping(); if time.Now().After(deadline) { ... }; sleep(250ms)` — the
// deadline is checked AFTER the ping, so one failing iteration would cost up to
// 90s and hundreds of managed-Dolt restart attempts — and internal/doctor's
// BeadsStoreCheck would block 90s past --check-timeout, per scope. The second
// loop returns early for proxied scopes, so it is the flag-off lane by
// construction.
//
// So the ROUTING is gated, the same way the classifier's rungs are. A direct or
// hosted handle takes main's path verbatim; poolStale is only ever marked by the
// proxied guard tick, and proxiedReadVerdict is a no-op off the proxied lane, so
// the direct lane gives up nothing by skipping the wrapper.
//
// The probe itself is StatsReporter.Stats rather than the raw GetStatistics
// beside it. On a served backend GetStatistics is an off-role bridge documented
// as never hard-failing — it degrades a busy or unreachable server into an
// answer — which is precisely the condition a reachability probe exists to
// report.
func (s *NativeDoltStore) Ping() error {
	if s.readLane() == directNativeLane {
		storage, release, err := s.acquireStorage()
		if err != nil {
			return err
		}
		defer release()
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return s.pingUpstreamRead(ctx, storage)
	}
	return s.withReadRetry(s.pingUpstreamRead)
}

// pingUpstreamRead is the one upstream call a Ping makes.
func (s *NativeDoltStore) pingUpstreamRead(ctx context.Context, storage beadslib.Storage) error {
	reporter, err := storage.StatsReporter()
	if err != nil {
		return err
	}
	_, err = reporter.Stats(ctx, issueops.StatsRequest{})
	return err
}
