package beads

import (
	"context"
	"fmt"
)

var _ GuardedBatcher = (*ProxiedStore)(nil)

// ApplyGuarded applies b through the guarded batcher of the leaf that owns
// writes, inside the same generation bracket every other mutation runs in.
//
// A batch is a write, so it follows rule 2 of the capability surface and goes
// to the write leaf, never to the read leaf, which is latched read-only. That
// is also where GuardedBatcherFor lands, because it follows this wrapper's
// ConditionalWritesResolveTarget: a direct call here and a resolved batcher
// reach the same store, and over a bd write leaf both report no capability.
//
// A demoted store answers ErrGuardedBatchUnsupported. The revisions a caller
// guards on come from native reads, and once the native leaf has stood down
// nothing reads them on this handle.
func (s *ProxiedStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	if s.nativeLeaf() == nil {
		return GuardedResult{}, fmt.Errorf("proxied store has stood down its native leaf: %w", ErrGuardedBatchUnsupported)
	}
	leaf := s.writeLeaf()
	batcher, ok := GuardedBatcherFor(leaf)
	if !ok {
		return GuardedResult{}, fmt.Errorf("proxied store write leaf %T: %w", leaf, ErrGuardedBatchUnsupported)
	}
	var res GuardedResult
	err := s.withMutation("guarded batch", func(Store) error {
		var err error
		res, err = batcher.ApplyGuarded(ctx, b)
		return err
	})
	if err != nil {
		return GuardedResult{}, err
	}
	return res, nil
}
