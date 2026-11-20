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
After matching a native namespace, preserve whether YAML records that prefix so
rename remedies include the required YAML update as well as the database rename.
A preview lookup must not start a provider or open a writable store. Preserve
explicit-prefix mismatch and city-prefix collision checks; an unreadable or
empty namespace is an error, never a reason to use the requested prefix.

Return ErrAdoptionRequired when registration encounters an existing queue
without explicit adoption, before all mutations. API adapters classify this
as a client validation refusal and offer adoption; never suggest deleting an
existing queue to complete registration.

## Explicit absolute clone destinations

An async clone with an explicit absolute path requires a durable body request_id
before admission. Persist the identity-required intent before reserving an absent
leaf under an existing parent, then persist its physical identity before fetch.
A recovered intent missing that identity retains data, including destinations
inside the city; it must not fall back to legacy contained-path cleanup. A live
request may roll back a directory whose physical identity it independently read,
only after rechecking that identity. Check ownership before reading or dropping
storage and again before removing the leaf. Register existing repositories in
place through the separate registration path. Relative/default clones and genuine
legacy cleanup keep their existing contracts.
