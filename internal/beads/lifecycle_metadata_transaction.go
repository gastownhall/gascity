package beads

import "errors"

// LifecycleMetadataTransaction exposes live reads and metadata-only mutations
// for one bead while a store-specific cooperative critical section is held.
// The critical section is not an atomic database transaction: successful
// writes remain durable when a later operation or the callback returns an
// error. Callers must not retain the transaction after the callback returns or
// recursively enter another lifecycle metadata transaction from the callback.
type LifecycleMetadataTransaction interface {
	Get() (Bead, error)
	SetMetadata(key, value string) error
	SetMetadataBatch(values map[string]string) error
}

// LifecycleCloseResult is the authoritative state returned by a close that
// runs inside a LifecycleMetadataTransaction's already-held mutation lease.
// Transitioned is true only when this operation observed a live Before row and
// an authoritative closed After row. CloseSucceeded records the close call's
// acknowledgement; an authoritative transition remains usable when a backing
// transport reports an ancillary error after commit.
type LifecycleCloseResult struct {
	Before           Bead
	After            Bead
	Transitioned     bool
	CloseSucceeded   bool
	ObserverDelivery CloseObserverDelivery
}

// AuthoritativeClosed reports whether the transaction returned an exact,
// durable closed snapshot for id.
func (r LifecycleCloseResult) AuthoritativeClosed(id string) bool {
	return id != "" && r.After.ID == id && r.After.Status == "closed"
}

// LifecycleCloseTransaction extends a lifecycle metadata transaction with an
// observer-suppressed close. The close, its authoritative snapshots, and any
// observer-order receipt are produced before the transaction releases its
// cooperative lifecycle mutation lease. Implementations must not invoke cache
// observers while the lease is held.
type LifecycleCloseTransaction interface {
	LifecycleMetadataTransaction
	CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error)
}

// ErrLifecycleCloseUnsupported reports that a lifecycle transaction cannot
// close through its already-held mutation lease.
var ErrLifecycleCloseUnsupported = errors.New("lifecycle transaction close is unsupported")

// CloseWithinLifecycleMetadataTransaction invokes tx's lease-preserving close
// capability without falling back to Store.Close, which would reacquire the
// same non-reentrant scope lease on BdStore and NativeDoltStore.
func CloseWithinLifecycleMetadataTransaction(tx LifecycleMetadataTransaction, reason string) (LifecycleCloseResult, error) {
	closer, ok := tx.(LifecycleCloseTransaction)
	if !ok {
		return LifecycleCloseResult{}, ErrLifecycleCloseUnsupported
	}
	return closer.CloseWithReasonWithoutObserver(reason)
}

// LifecycleMetadataTransactionStore serializes a metadata-only lifecycle
// callback for one bead. It is an optional Store capability.
type LifecycleMetadataTransactionStore interface {
	WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error
}

// WithLifecycleMutationLease holds the cooperative lifecycle lease for scope
// and gives fn the command-local environment required by a synchronous
// mutating child to join that lease. The returned values must be applied only
// to that child; callers must not mutate the parent process environment.
func WithLifecycleMutationLease(scope string, fn func(commandEnv map[string]string) error) error {
	if fn == nil {
		return errors.New("lifecycle mutation lease: nil callback")
	}
	lease, err := acquireLifecycleMutationLease(scope, inheritedLifecycleMutationFromEnv())
	if err != nil {
		return err
	}
	defer lease.Unlock()
	return fn(lease.CommandEnv())
}

// WithLifecycleMetadataTransaction runs fn through a store's lifecycle
// metadata capability when available. Stores without the optional capability
// use a process-local, exact-ID critical section around their explicit
// live-read and write handles; only capable stores can add cross-process
// serialization.
func WithLifecycleMetadataTransaction(store Store, id string, fn func(LifecycleMetadataTransaction) error) error {
	if store == nil {
		return errors.New("lifecycle metadata transaction: nil store")
	}
	if fn == nil {
		return errors.New("lifecycle metadata transaction: nil callback")
	}
	switch direct := store.(type) {
	case *MemStore:
		return direct.withLifecycleMetadataTransaction(id, fn)
	case *FileStore:
		return direct.withLifecycleMetadataTransaction(id, fn)
	}
	if transactional, ok := store.(LifecycleMetadataTransactionStore); ok {
		return transactional.WithLifecycleMetadataTransaction(id, fn)
	}
	handles := HandlesFor(store)
	if handles.Live == nil {
		return errors.New("lifecycle metadata transaction: store has no live reader")
	}
	if handles.Writer == nil {
		return errors.New("lifecycle metadata transaction: store has no writer")
	}
	// Capability-less stores have no filesystem scope to coordinate. Reuse the
	// direct-store process scope so fallback callbacks serialize with any
	// scope-less native handle for the same bead ID.
	unlock, err := lockLifecycleMetadata("", id)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(&lifecycleMetadataDirectTransaction{
		id:     id,
		store:  store,
		reader: handles.Live,
		writer: handles.Writer,
	})
}

type lifecycleMetadataDirectTransaction struct {
	id     string
	store  Store
	reader LiveReader
	writer Writer
}

func (tx lifecycleMetadataDirectTransaction) Get() (Bead, error) {
	return tx.reader.Get(tx.id)
}

func (tx lifecycleMetadataDirectTransaction) GetByID(id string) (Bead, error) {
	return tx.reader.Get(id)
}

func (tx lifecycleMetadataDirectTransaction) List(query ListQuery) ([]Bead, error) {
	query.Live = true
	query.TierMode = TierBoth
	return tx.reader.List(query)
}

func (tx lifecycleMetadataDirectTransaction) SetMetadata(key, value string) error {
	return tx.writer.SetMetadata(tx.id, key, value)
}

func (tx lifecycleMetadataDirectTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.writer.SetMetadataBatch(tx.id, values)
}

func (tx lifecycleMetadataDirectTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	return closeLifecycleMetadataTransaction(tx, func() (CloseObserverDelivery, error) {
		if suppressor, ok := CloseObserverSuppressorFor(tx.store); ok {
			if sequenced, ok := suppressor.(SequencedCloseObserverSuppressor); ok {
				return sequenced.CloseWithoutObserverWithDelivery(tx.id)
			}
			return nil, suppressor.CloseWithoutObserver(tx.id)
		}
		if closer, ok := tx.writer.(interface {
			CloseWithReason(string, string) error
		}); ok {
			return nil, closer.CloseWithReason(tx.id, reason)
		}
		return nil, tx.writer.Close(tx.id)
	})
}

func closeLifecycleMetadataTransaction(
	tx LifecycleMetadataTransaction,
	closeFn func() (CloseObserverDelivery, error),
) (LifecycleCloseResult, error) {
	before, err := tx.Get()
	if err != nil {
		return LifecycleCloseResult{}, err
	}
	result := LifecycleCloseResult{Before: before}
	if before.Status == "closed" {
		result.After = before
		return result, nil
	}
	delivery, closeErr := closeFn()
	result.CloseSucceeded = closeErr == nil
	result.ObserverDelivery = delivery
	after, readErr := tx.Get()
	if readErr == nil {
		result.After = after
		result.Transitioned = before.Status != "closed" && after.Status == "closed"
	}
	// A failed post-close read is not a failed close acknowledgement. The caller
	// retains the durable intent and may perform bounded authoritative reads
	// through this same transaction before deciding whether to publish.
	return result, closeErr
}

var _ LifecycleCloseTransaction = (*lifecycleMetadataDirectTransaction)(nil)
