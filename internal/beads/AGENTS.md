# internal/beads — cache and live-read boundaries

For readiness observations, distinguish a complete active-row census from
complete dependency and ready projections. A live, clean, nonpartial census
can prove an empty result when the normal tier/status/label/assignee filters
leave no eligible candidate, including a claimed-only work store without a
dependency witness. Eligible candidates still require complete dependencies
and a known ready projection. Preserve canceled-context, busy-cache, dirty-row,
partial-prime and degraded-projection errors. Uncertainty is never empty
success, and this observation grants no mutation or execution admission.

For an exact current, closed or historical bead, use the authoritative live
point-read handle with the existing caller/backend bounds. The context-free
Live.Get contract adds no request-cancellation guarantee and does not require
a complete active-cache prime. A cached miss is not authoritative absence.
Preserve exact ID/store identity and backing errors; never substitute stale
data, an empty row or an unbounded background read. Federated point reads keep
their declared residency plan and refusal policy; independent stores are not
an atomic snapshot.

An existing GC endpoint-origin stamp excludes provider-owned classification,
including when an inherited mirror has drifted and boot normalization must
repair it. This ownership claim does not authorize adoption, connection or
mutation; those boundaries retain strict canonical validation.
