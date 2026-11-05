# Closed tracking retention

Retention reads must push the creation cutoff into the store query, then
apply the close-time cutoff and retain at least ten closed runs per order.
Bound maintenance and watchdog deletion passes, and guard every candidate
against stranding live workflow descendants before deleting it. Live-descendant
refusals do not consume the deletion budget.
