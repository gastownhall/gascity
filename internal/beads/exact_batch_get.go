package beads

import "errors"

// ExactBatchGetter is the optional capability of a store that can read many
// beads by exact ID in one round trip. Mutation guards use it to verify a bulk
// id list with one read instead of one per id.
//
// GetExactBatch returns the beads whose ID matched a requested id exactly.
// Every requested id it did not answer exactly is listed in unresolved, in
// input order; the caller resolves those one at a time with Get, which is
// what distinguishes an absent bead from a substring collision.
type ExactBatchGetter interface {
	GetExactBatch(ids []string) (found map[string]Bead, unresolved []string, err error)
}

// ErrExactBatchGetUnsupported is returned by a wrapping store whose inner
// store has no exact batch read. Callers fall back to per-id Get.
var ErrExactBatchGetUnsupported = errors.New("exact batch get unsupported by this store")

var _ ExactBatchGetter = (*BdStore)(nil)

// PrefetchExact reads ids in one round trip when store has GetExactBatch and
// returns the beads it answered exactly, keyed by id. It returns an empty map
// for a store without the batch (or a single id), so a caller resolves every id
// it does not find here with Get, in its own order and with Get's errors.
func PrefetchExact(store Store, ids []string) (map[string]Bead, error) {
	getter, ok := store.(ExactBatchGetter)
	if !ok || len(ids) < 2 {
		return map[string]Bead{}, nil
	}
	found, _, err := getter.GetExactBatch(ids)
	if err != nil {
		if errors.Is(err, ErrExactBatchGetUnsupported) {
			return map[string]Bead{}, nil
		}
		return nil, err
	}
	return found, nil
}
