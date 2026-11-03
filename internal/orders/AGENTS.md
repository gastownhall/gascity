# Closed tracking retention

Retention reads must push the creation cutoff into the store query, then
apply the close-time cutoff and retain at least ten closed runs per order.
Bound maintenance and watchdog deletion passes, and guard every candidate
against stranding live workflow descendants before deleting it. Live-descendant
refusals do not consume the deletion budget.

Order dispatch must progress independently of tracking and retention sweeps.
Each maintenance lane owns its cadence, suspension gate, pressure episode, and
shutdown cancellation; stop and join the lanes when the runtime exits.
