# Status JSON schema for controller-only fields

**Origin:** ga-3j7v31, architect follow-up to ga-y9qakg.  
**Priority:** P4.  
**Sequence:** start after ga-y9qakg lands.

## User impact

`gc status --json` can emit `summary.store_health` and top-level `conditional_writes` when the controller is running. The published status schema forbids both because it has `additionalProperties: false` and does not define them. A client validating real controller output against that schema can reject a healthy status response. The existing runtime-schema test misses the problem because it exercises a local fallback where both fields are absent.

The architect's ga-y9qakg change will add `stuck_creating` to the same schema and its populated-case tests. This P4 repair waits for that change to land, then adds coverage for the two existing fields. The sequencing keeps the schema changes from competing in flight.

## Work packages

| Bead | Owner | Acceptance | Blocked by |
| --- | --- | --- | --- |
| ga-r9tb4v | Validator | A real serialized status with populated `store_health`, including `live_rows_unknown`, is rejected by today's schema and accepted after repair; the absent case still passes. | ga-y9qakg |
| ga-2q33xk | Validator | A real serialized status with populated `conditional_writes`, including a store verdict and notice, is rejected by today's schema and accepted after repair; the absent case still passes. | ga-y9qakg |
| ga-0p7qcu | Builder | The published schema describes both optional blocks and their nested values, preserves `additionalProperties: false`, and passes both populated tests plus the existing status schema test. | ga-y9qakg, ga-r9tb4v, ga-2q33xk |

The two test beads isolate distinct output contracts. A validator may implement them together once both become ready. Each package carries a `discovered-from` edge to ga-3j7v31, and scheduling blockers are recorded as `blocks` dependencies.

## Completion

The builder verifies that the status schema still accepts the status shape added by ga-y9qakg. This work changes the published contract to match existing output; it does not change what `gc status` emits.
