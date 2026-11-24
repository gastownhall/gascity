# Closed tracking retention

Retention reads must push the creation cutoff into the store query, then
apply the close-time cutoff and retain at least ten closed runs per order.
Bound maintenance and watchdog deletion passes, and guard every candidate
against stranding live workflow descendants before deleting it. Live-descendant
refusals do not consume the deletion budget.

Delete the guarded set through `deleteWorkflowBeadsBatch` in chunks of at most
32 ids, so each chunk is rechecked at the set level right before its delete.
A failed chunk stops that store's pass and counts none of its cleared runs;
the per-bead fallback stops at its first failure. The cross-store bounded
sweep therefore does not charge a failing store's deletes against its budget,
and its worst case is stores times limit per pass.
