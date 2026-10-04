# Session-row contention product tuning (ga-4b0p24)

## Goal and priority

Reduce avoidable user-visible failures when concurrent session lifecycle and
usage writes contend for the same Dolt row. This is P3 product tuning. The
investigation found a passing high-load run in which a usage-marker write
needed the third and final native-store attempt. Architecture review found a
contract gap: serialization exhaustion returns the declared, retryable 503
`store_conflict`, but CAS exhaustion on wake or close can return 500. A
separate live-contract test defect is tracked by ga-upr9ql; this plan does not
reopen or duplicate that fix.

Five session-row writers were identified: API lifecycle writes, reconciler
patches, the asynchronous start commit, usage sweep markers, and the usage
invocation cursor. Usage writes are competitors for the row, not direct
user-visible failures. The native-store policy remains three attempts with
25 ms and 50 ms backoffs.

The architect recorded the decision and evidence in ga-kq4duy. That bead
remains open, unrouted, and labelled `parent-record` so the ruling can be read
by open-record sweeps; it is no longer a blocker for implementation.

## Work packages

| Order | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| Decision | ga-kq4duy | architect (recorded) | Chose three bounded changes and explicit deferrals; kept open as a parent record. |
| Server vehicle | ga-yyev65 | builder (`ready-to-build`) | Validator published deterministic RED tests on `validator/ga-yyev65-tests`; the same bead now carries the server implementation and one combined tests-plus-fix PR. |
| CLI vehicle | ga-dib7ae | builder (`ready-to-build`) | Validator published RED tests on `validator/ga-dib7ae-tests`; the same bead now carries the CLI implementation after the server classifier merges. |
| Server shadow | ga-uw4mfi | builder (`ready-to-build`) | Original PM build package. Verify the server work reached `main`, then close no-op with the merge commit; do not implement it twice. |
| CLI shadow | ga-40lb8n | builder (`ready-to-build`) | Original PM build package. Verify the CLI work reached `main`, then close no-op with the merge commit; do not implement it twice. |

Live blockers: ga-yyev65 → ga-dib7ae and ga-uw4mfi; ga-dib7ae and
ga-uw4mfi → ga-40lb8n. The architect's open parent record has no blocks edge
to the vehicles. Each package retains its `discovered-from:ga-4b0p24` edge.
The CLI change must wait for the server classifier to **merge**, even if the
server vehicle closes earlier at review handoff. The shadow beads exist only
to account for the duplicate packages created before the validator formula's
same-bead builder handoff. The formula mismatch is tracked as ga-6hblrz.

## Decision boundary and release evidence

Adopted outcomes:

1. One store-level `beads.IsRetryableConflict` classifier covers safe
   serialization conflicts and CAS exhaustion. Wake, suspend, and close return
   the declared 503 `store-unavailable` with `store_conflict:` detail after
   exhaustion. Ambiguous connection failures must not be retried or mapped as
   known-safe conflicts.
2. The in-process `gc session wake`, `suspend`, and `close` commands retry
   their whole action once after 250–500 ms of jitter, re-reading lifecycle
   state. A second conflict yields a stable `session store busy` message and
   exit 1. Non-idempotent operations receive no automatic retry.
3. Record retry-exhaustion telemetry so later tuning uses observed failures.

Deferred outcomes: widening the shared store retry budget, relocating usage
markers or cursor, mapping non-session conflicts, and adding a typed problem
code. Dashboard lifecycle UI, MCP mail, and legacy test-only handlers receive
no change: the dashboard has no lifecycle controls, MCP mail uses an external
server, and the legacy handlers are not the production route.

The server and CLI RED tests have separate validator branches but must land
with their implementations, not alone. A high-load run supports the result;
deterministic conflict tests and the existing contract are the release
evidence. After retry-exhaustion telemetry deploys, review 14 days of data
before changing the shared retry budget or usage-row layout.

Source: investigator bead ga-4b0p24, earlier test-defect bead ga-19ii7x,
pending deploy ga-upr9ql, and current
`internal/beads/native_dolt_store.go`,
`internal/api/huma_handlers_sessions.go`,
`internal/api/envelope_compat.go`, and `cmd/gc/usage_compute.go`.
