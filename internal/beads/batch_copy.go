package beads

import (
	"context"
	"errors"
	"fmt"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The batch capabilities of a store-to-store copy (S5 §3.2). The infra-class
// migration copies tens of thousands of rows; one transaction, and on Dolt one
// commit, per row and per edge is what made a 40k-row city take minutes. The
// capabilities are optional on a store; the migration requires the copy's two
// on its binding, and reads and removes edge by edge where a work store lacks
// the others.

// ErrBatchCopyUnsupported signals that a wrapper implements a batch copy
// capability only to forward it, and the store under it has none: the caller
// answers as it would had the wrapper not advertised it.
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

// DepBatchRemover removes edges, all or nothing per call, and on Dolt in one
// commit: DepRemove for a batch. An edge that is not there is already removed,
// as DepRemove treats it. A store, or a wrapper over one, that cannot remove
// in one transaction reports ErrDepRemoveBatchUnsupported, and the caller
// removes edge by edge.
type DepBatchRemover interface {
	DepRemoveBatch(pairs []DepPair) error
}

// ErrDepRemoveBatchUnsupported signals a DepRemoveBatch with no transaction
// under it: a forwarding wrapper over a store without the capability, or a
// native store served over the v0 wire, whose RunInTransaction is off the
// served surface.
var ErrDepRemoveBatchUnsupported = errors.New("batch dep remove unsupported by backing store")

var (
	_ DepBatchRemover        = (*SQLiteStore)(nil)
	_ DepBatchRemover        = (*NativeDoltStore)(nil)
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

// DepRemoveBatch removes the edges in one transaction, one Dolt commit, each
// through the transaction's RemoveDependency. Unlike DepRemove, whose editor
// records a dependency_removed history event per edge, it records none: the
// transaction's RemoveDependency is the library's structural teardown, and a
// batch removal is structural (the clear detaching rows it moved), not an edit
// an operator made. The deadline grows with the batch, as a graph apply's does
// (nativeDepRemoveBatchDeadline). A backend with no transaction to open reports
// ErrDepRemoveBatchUnsupported before anything is written.
func (s *NativeDoltStore) DepRemoveBatch(pairs []DepPair) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if len(pairs) == 0 {
		return nil
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	err = retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), nativeDepRemoveBatchDeadline(len(pairs)))
		defer cancel()
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: remove %d dependencies", len(pairs)), func(tx beadslib.Transaction) error {
			for _, p := range pairs {
				if err := tx.RemoveDependency(ctx, p.IssueID, p.DependsOnID, s.actor); err != nil {
					return nativeStoreError(p.IssueID, err)
				}
			}
			return nil
		})
	})
	var unsupported *beadslib.ErrUnsupported
	if errors.As(err, &unsupported) {
		return fmt.Errorf("%w: %w", ErrDepRemoveBatchUnsupported, err)
	}
	return err
}

// nativeDepRemoveBatchDeadline scales DepRemoveBatch's budget with the edges
// it removes: the flat operation budget plus a second per edge, about fifteen
// times the 55-65 ms an edge measured on a loaded host, so a slow host does
// not hit the wall inside one batch.
func nativeDepRemoveBatchDeadline(edges int) time.Duration {
	return bdCommandTimeout + time.Duration(edges)*time.Second
}
