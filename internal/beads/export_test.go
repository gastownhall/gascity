package beads

// NewNativeDoltStoreForConformance returns a NativeDoltStore backed by the
// in-memory native storage fixture for the external conformance suite.
func NewNativeDoltStoreForConformance() Store {
	return newNativeDoltStoreForTest(newNativeDoltMemStorage())
}
