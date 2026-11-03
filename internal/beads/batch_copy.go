package beads

import (
	"context"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The batch capabilities of a store-to-store copy (S5 §3.2). The infra-class
// migration copies tens of thousands of rows; one transaction, and on Dolt one
// commit, per row and per edge is what made a 40k-row city take minutes. Each
// capability is optional: a store without it takes the per-row path.

// ErrBatchCopyUnsupported signals that a wrapper implements a batch copy
// capability only to forward it, and the store under it has none: the caller
// takes the per-row path, as it would had the wrapper not advertised it.
var ErrBatchCopyUnsupported = errors.New("batch copy unsupported by backing store")

// DepPair names one dependency edge by its two ends.
type DepPair struct {
	IssueID     string
	DependsOnID string
}

// DepWithPayload is one edge to add together with the payload it carries;
// an empty Payload adds the edge carrying nothing, as DepAddWithMetadata does.
type DepWithPayload struct {
	Dep
	Payload string
}

// ForeignIDBatchCreator creates rows keeping their explicit ids, all or nothing
// per call: ForeignIDCreator for a batch. Every id must be new to the store.
type ForeignIDBatchCreator interface {
	CreateWithForeignIDBatch(rows []Bead) error
}

// DepBatchWriter adds edges, each with its payload, all or nothing per call.
type DepBatchWriter interface {
	DepAddBatch(edges []DepWithPayload) error
}

// DepMetadataBatchReader reads, in one round trip, the payload on every DOWN
// edge of the given anchors that carries one (DepMetadataCarries). An edge
// that carries none, and an anchor that is not there, have no entry: absence
// is "carries nothing", as DepMetadataReader answers ("", false, nil). A store
// that cannot be asked does not implement it.
type DepMetadataBatchReader interface {
	DepMetadataBatch(ids []string) (map[DepPair]string, error)
}

var (
	_ ForeignIDBatchCreator  = (*SQLiteStore)(nil)
	_ DepBatchWriter         = (*SQLiteStore)(nil)
	_ DepMetadataBatchReader = (*SQLiteStore)(nil)
	_ DependencyBatchLister  = (*SQLiteStore)(nil)
	_ DepMetadataBatchReader = (*NativeDoltStore)(nil)
)

// MemStore deliberately has no DepMetadataBatch, though it holds no payload:
// FileStore and test doubles embed it and override DepMetadata, and a
// promoted batch read answering "none" would contradict their own.

// DepMetadataBatch reads the payloads of the anchors' down edges through the
// same EdgeReader.ReadEdges DepMetadata rides, one read per chunk of anchors.
// A failed chunk fails the call: a partial map would read the anchors it cost
// as edges carrying nothing.
func (s *NativeDoltStore) DepMetadataBatch(ids []string) (map[DepPair]string, error) {
	out := map[DepPair]string{}
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
				for _, edge := range anchor.Edges {
					if edge == nil || !DepMetadataCarries(edge.Metadata) {
						continue
					}
					// The first carrying row of a pair wins, as in DepMetadata.
					pair := DepPair{IssueID: anchor.ID, DependsOnID: edge.DependsOnID}
					if _, seen := out[pair]; !seen {
						out[pair] = edge.Metadata
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading the edge payloads of %d anchor(s) starting at %q: %w", len(chunk), chunk[0], err)
		}
	}
	return out, nil
}
