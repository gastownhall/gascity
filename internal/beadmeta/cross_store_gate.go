package beadmeta

// CrossStoreGateLabel marks a gate bead that mirrors a blocker held in another
// store: internal/dispatch mints one for a drain whose item waits on a bead
// another store holds, carrying the CrossStore*MetadataKey keys and the run's
// gc.root_bead_id. It is declared in this leaf so a package below the
// dispatcher can recognize a gate without importing it: member routing
// (internal/graphroute) leaves a gate unrouted, as a launch does.
const CrossStoreGateLabel = "gc:cross-store-gate"
