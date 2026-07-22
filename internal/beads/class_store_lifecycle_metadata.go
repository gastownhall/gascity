package beads

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s WorkStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s GraphStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s SessionStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s MailStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s OrdersStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

// WithLifecycleMetadataTransaction forwards the lifecycle metadata capability
// hidden by the typed Store embedding.
func (s NudgesStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return WithLifecycleMetadataTransaction(s.Store, id, fn)
}

var (
	_ LifecycleMetadataTransactionStore = WorkStore{}
	_ LifecycleMetadataTransactionStore = GraphStore{}
	_ LifecycleMetadataTransactionStore = SessionStore{}
	_ LifecycleMetadataTransactionStore = MailStore{}
	_ LifecycleMetadataTransactionStore = OrdersStore{}
	_ LifecycleMetadataTransactionStore = NudgesStore{}
)
