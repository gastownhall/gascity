package beads

import (
	"errors"
	"fmt"
)

// LifecycleReadTransaction adds arbitrary live reads to a lifecycle metadata
// transaction. Implementations must perform these reads without reacquiring any
// store mutex or lifecycle lease already held by the surrounding callback.
type LifecycleReadTransaction interface {
	LifecycleMetadataTransaction
	GetByID(id string) (Bead, error)
	List(query ListQuery) ([]Bead, error)
}

// ErrLifecycleReadUnsupported reports that a lifecycle transaction cannot read
// arbitrary live rows without leaving or recursively entering its lease.
var ErrLifecycleReadUnsupported = errors.New("lifecycle transaction live reads are unsupported")

// ErrLifecycleMultiStoreUnsupported reports that two stores do not expose
// stable lifecycle domains that can be acquired once in deterministic order.
var ErrLifecycleMultiStoreUnsupported = errors.New("multi-store lifecycle transaction is unsupported")

// WithLifecycleReadTransactions holds the source and root lifecycle domains
// across one callback. Equal domains are deduplicated into the root transaction;
// distinct domains are acquired in canonical order so concurrent source/root
// checks cannot deadlock by entering the same pair in reverse order.
func WithLifecycleReadTransactions(
	source Store,
	sourceID string,
	root Store,
	rootID string,
	fn func(sourceTx, rootTx LifecycleReadTransaction) error,
) error {
	if source == nil || root == nil {
		return errors.New("multi-store lifecycle transaction: nil store")
	}
	if fn == nil {
		return errors.New("multi-store lifecycle transaction: nil callback")
	}
	sourceDomain, sourceOK := lifecycleMutationDomainFor(source)
	rootDomain, rootOK := lifecycleMutationDomainFor(root)
	if !sourceOK || !rootOK {
		return ErrLifecycleMultiStoreUnsupported
	}

	if sourceDomain == rootDomain {
		return WithLifecycleMetadataTransaction(root, rootID, func(tx LifecycleMetadataTransaction) error {
			reader, ok := tx.(LifecycleReadTransaction)
			if !ok {
				return ErrLifecycleReadUnsupported
			}
			return fn(reader, reader)
		})
	}

	withSource := func(rootTx LifecycleReadTransaction) error {
		return WithLifecycleMetadataTransaction(source, sourceID, func(tx LifecycleMetadataTransaction) error {
			sourceTx, ok := tx.(LifecycleReadTransaction)
			if !ok {
				return ErrLifecycleReadUnsupported
			}
			return fn(sourceTx, rootTx)
		})
	}
	withRoot := func(sourceTx LifecycleReadTransaction) error {
		return WithLifecycleMetadataTransaction(root, rootID, func(tx LifecycleMetadataTransaction) error {
			rootTx, ok := tx.(LifecycleReadTransaction)
			if !ok {
				return ErrLifecycleReadUnsupported
			}
			return fn(sourceTx, rootTx)
		})
	}

	if sourceDomain < rootDomain {
		return WithLifecycleMetadataTransaction(source, sourceID, func(tx LifecycleMetadataTransaction) error {
			sourceTx, ok := tx.(LifecycleReadTransaction)
			if !ok {
				return ErrLifecycleReadUnsupported
			}
			return withRoot(sourceTx)
		})
	}
	return WithLifecycleMetadataTransaction(root, rootID, func(tx LifecycleMetadataTransaction) error {
		rootTx, ok := tx.(LifecycleReadTransaction)
		if !ok {
			return ErrLifecycleReadUnsupported
		}
		return withSource(rootTx)
	})
}

// lifecycleMutationDomainFor returns the canonical lock domain used by a store.
// Wrapper-declared resolution targets are followed so policy and class views of
// one backing deduplicate with the backing itself.
func lifecycleMutationDomainFor(store Store) (string, bool) {
	for range 8 {
		switch direct := store.(type) {
		case *CachingStore:
			if direct == nil || direct.backing == nil {
				return "", false
			}
			store = direct.backing
			continue
		case *MemStore:
			if direct == nil {
				return "", false
			}
			return closeTransitionScopeKey(memLifecycleMutationScope(direct)), true
		case *FileStore:
			if direct == nil || direct.path == "" {
				return "", false
			}
			return closeTransitionScopeKey(direct.path), true
		case *BdStore:
			if direct == nil || direct.dir == "" {
				return "", false
			}
			return closeTransitionScopeKey(direct.dir), true
		case *NativeDoltStore:
			if direct == nil {
				return "", false
			}
			return closeTransitionScopeKey(direct.closeTransitionScopeKey()), true
		}
		if provider, ok := store.(cacheMutationScopeProvider); ok {
			if scope := provider.CacheMutationScope(); scope != "" {
				return closeTransitionScopeKey(scope), true
			}
		}
		targeter, ok := store.(ConditionalWritesResolveTargeter)
		if !ok {
			return "", false
		}
		target := targeter.ConditionalWritesResolveTarget()
		if target == nil {
			return "", false
		}
		store = target
	}
	return "", false
}

func memLifecycleMutationScope(store *MemStore) string {
	return fmt.Sprintf("process-mem-store:%p", store)
}

var _ LifecycleReadTransaction = (*lifecycleMetadataDirectTransaction)(nil)
