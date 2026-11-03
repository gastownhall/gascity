package beads

// IDPrefixReporter exposes only the namespace in which a store mints IDs.
// It cannot create a row or change its coordination class, so typed class
// gates permit this exact read-only capability without exposing a writer.
type IDPrefixReporter interface{ IDPrefix() string }
