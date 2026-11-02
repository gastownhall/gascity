package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

var _ ExactBatchGetter = (*NativeDoltStore)(nil)

// GetExactBatch reads ids by exact id: per chunk of nativeServerEdgeAnchorCap
// ids, one keyed listing and one edge read of the rows it found. Each id it
// finds is answered as Get answers it, from the listing's row and the edge
// read's anchor through Get's own conversion (beadFromNativeIssueDetails). It
// may not be thinner: a CachingStore installs what its backing's batch answers
// as that bead's Get row, edges and all (readBackingRows).
//
// Every id it does not answer is unresolved, in input order, for the caller's
// Get to settle: an id this store does not hold, a row the edge read reports
// gone (deleted between the two reads, where Get reports a miss), and a row
// whose metadata does not parse (Get reports that error itself).
//
// The listing is nativeListReadRequest — every status, both planes, every type
// — narrowed by IDFilter, with an explicit unlimited page (a nil limit is a
// fifty-row page). An IDFilter keys the request (NativeListRequestKeyed), and
// the list-request stats count it. An http-native reader refuses an IDFilter on
// a listing client-side, before any round trip (filterReadyByWorkOutcome): the
// batch then reports ErrExactBatchGetUnsupported, and callers read per id.
func (s *NativeDoltStore) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	found := make(map[string]Bead, len(ids))
	var asked []string
	for _, id := range ids {
		// An empty anchor is a validation error to the edge read, not a miss.
		if id != "" {
			asked = append(asked, id)
		}
	}
	for start := 0; start < len(asked); start += nativeServerEdgeAnchorCap {
		chunk := asked[start:min(start+nativeServerEdgeAnchorCap, len(asked))]
		var (
			rows  map[string]Bead
			tally nativeListTally
		)
		err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
			tally = nativeListTally{}
			var err error
			rows, err = s.exactBatchChunk(ctx, storage, chunk, &tally)
			return err
		})
		s.listCounters.commit(tally)
		if err != nil {
			var unsupported *beadslib.ErrUnsupported
			if errors.As(err, &unsupported) {
				return nil, nil, fmt.Errorf("%w: %w", ErrExactBatchGetUnsupported, err)
			}
			return nil, nil, fmt.Errorf("getting beads %v: %w", chunk, err)
		}
		for id, b := range rows {
			found[id] = b
		}
	}
	var unresolved []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			unresolved = append(unresolved, id)
		}
	}
	return found, unresolved, nil
}

// exactBatchChunk is one chunk of GetExactBatch: the keyed listing, then the
// edges of the rows it found.
func (s *NativeDoltStore) exactBatchChunk(ctx context.Context, storage beadslib.Storage, ids []string, tally *nativeListTally) (map[string]Bead, error) {
	reader, err := storage.IssueReader()
	if err != nil {
		return nil, err
	}
	req := nativeListReadRequest()
	req.IDFilter = strings.Join(ids, ",")
	unlimited := 0
	req.Limit = &unlimited
	page, err := reader.List(ctx, req)
	tally.noteRequest(req, len(page.Items))
	if err != nil {
		return nil, err
	}
	s.noteRows(len(page.Items))
	byID := make(map[string]*issueops.IssueWithCounts, len(page.Items))
	for _, row := range page.Items {
		if row != nil && row.Issue != nil {
			byID[row.ID] = row
		}
	}
	var anchors []string
	for _, id := range ids {
		if _, ok := byID[id]; ok {
			anchors = append(anchors, id)
		}
	}
	out := make(map[string]Bead, len(anchors))
	if len(anchors) == 0 {
		return out, nil
	}
	edgeReader, err := storage.EdgeReader()
	if err != nil {
		return nil, err
	}
	edges, err := edgeReader.ReadEdges(ctx, issueops.EdgeReadRequest{IDs: anchors})
	if err != nil {
		return nil, err
	}
	for _, anchor := range edges.Anchors {
		row, ok := byID[anchor.ID]
		if !ok || anchor.Missing {
			continue
		}
		bead, err := beadFromNativeIssueDetails(&issueops.IssueDetails{Issue: *row.Issue, Labels: row.Labels}, anchor)
		if err != nil {
			if isNativeIssueMetadataParseError(err) {
				continue
			}
			return nil, err
		}
		out[anchor.ID] = bead
	}
	return out, nil
}
