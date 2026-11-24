package session

// runtimeOwnerKey is the session metadata key holding the runtime-owner
// record: the uid that launched the session's runtime, the boot it ran in,
// and the identity of each of its process-table roots.
const runtimeOwnerKey = "runtime_owner"
