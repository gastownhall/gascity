# Session-row contention product tuning (ga-4b0p24)

## Goal and priority

Reduce avoidable user-visible failures when concurrent session lifecycle and
usage writes contend for the same Dolt row. This is P3 product tuning. The
investigation found a passing high-load run in which a usage-marker write
needed the third and final native-store attempt. It also found that an
exhausted lifecycle write returns the declared, retryable 503
`store_conflict`. A separate live-contract test defect is being deployed as
ga-upr9ql; this plan does not reopen or duplicate that fix.

The observed contention window includes API lifecycle writes, reconciler
patches, the asynchronous start commit, and usage sweep markers. The current
native-store policy makes three attempts with 25 ms and 50 ms backoffs.
These facts justify a bounded product decision; a passing load run alone does
not establish a desired retry policy or prove that exhaustion is gone.

## Work packages

| Order | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| 1 | ga-kq4duy | architect (`needs-architecture`) | Verify the affected client paths and choose bounded user-visible behavior. Rank native-store retry, usage-marker contention, client handling, and non-session error mapping with explicit adopt/defer/no-change decisions. |
| 2a | ga-yyev65 | validator (`needs-tests`) | Deterministic server-side regression coverage for the selected lifecycle-write behavior and exhaustion bound. |
| 2b | ga-dib7ae | validator (`needs-tests`) | Audit affected CLI, dashboard and MCP paths, then cover selected API/client conflict behavior with controlled responses. |
| 3a | ga-uw4mfi | builder (`ready-to-build`) | Deliver the selected server-side mitigation and show its effect under the reported contention scenario. |
| 3b | ga-40lb8n | builder (`ready-to-build`) | Deliver selected API/client behavior, preserving the typed wire contract and safe retry bounds. |

Blockers: ga-kq4duy → ga-yyev65 → ga-uw4mfi, and ga-kq4duy →
ga-dib7ae → ga-40lb8n. Each package has a `discovered-from:ga-4b0p24`
edge to preserve the investigation origin. The server and client branches may
proceed in parallel after the decision. A work package with an explicit
no-change decision closes with that citation and no code diff.

## Decision boundary and release evidence

PM is not choosing a retry count, backoff, writer layout, or new HTTP status.
The architect decides which changes are worth their latency and compatibility
cost and states the measurable contract first. The validator supplies
deterministic evidence before a builder changes behavior. Builders report
remaining 503 cases and what users see after retry exhaustion.

The existing declared lifecycle 503 contract remains the reference unless the
architect justifies a contract change. Client retry must be bounded and
restricted to operations that can safely be repeated. If non-session response
mapping changes, its OpenAPI and generated-client artifacts must agree.
Regression coverage should distinguish an ordinary transient conflict from
exhaustion at the chosen bound. A high-load run is supporting evidence, not
the sole release gate.

Source: investigator bead ga-4b0p24, earlier test-defect bead ga-19ii7x,
pending deploy ga-upr9ql, and current
`internal/beads/native_dolt_store.go`,
`internal/api/huma_handlers_sessions.go`,
`internal/api/envelope_compat.go`, and `cmd/gc/usage_compute.go`.
