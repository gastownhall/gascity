# Rig provisioning

Keep validation and planning ahead of mutable city and rig operations. A
`DryRun` returns before directory creation, store preparation or initialization,
canonical topology and route writes, deferred import commits, hooks and runtime
reloads. It may resolve pack content into a derived cache. Preserve existing
queue data and the caller's input config during a preview. Check both an
existing queue and a missing destination when changing this boundary.

Adoption must keep the existing queue namespace. Resolve it before deriving a
prefix from the rig name. Use ReadStorePrefix for native stores; a YAML mirror
that disagrees with the stored namespace must be rejected.
A preview lookup must not start a provider or open a writable store. Preserve
explicit-prefix mismatch and city-prefix collision checks; an unreadable or
empty namespace is an error, never a reason to use the requested prefix.
