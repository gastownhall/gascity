# Conditional agent suspension

`GET /v0/city/{cityName}/agent-suspension/{name}` returns the durable desired
state and a server-issued SHA-256 token. Rig-qualified agents use
`/agent-suspension/{dir}/{base}`. Suspension has a sibling namespace instead of
an `/agent/{name}/suspension` suffix because that suffix would shadow the
established detail URL of a valid rig-qualified agent named
`{dir}/suspension`.

`PUT` to the same path accepts exactly:

```json
{"expected_token":"<64 lowercase hex characters>","suspended":true}
```

The token binds the canonical agent identity, its resolved agent definition,
its origin, and the complete raw city configuration that governs overlays and
patches. Pack, patch, inline, site-bound, or convention-discovered `agent.toml`
changes that can alter the resolved target therefore change the token.

Comparison and mutation occur under the same per-city `configedit.Editor`
lock. A mismatch returns `409 Conflict`, performs no write, and does not change
configuration mtimes. A match returns exact `before` and `after` state/token
pairs. When the target is already in the requested state, the response status
is `already_desired`, the two pairs are identical, and no file is written.
This state-derived result is safe to adopt after a lost response or supervisor
restart: read a fresh token, verify the exact desired state, and issue the same
desired-state request with that token. It does not depend on a process-local
idempotency cache.

The token is a semantic content token, not a chronological counter. If every
governing source changes from A to B and then returns exactly to A, the A token
returns as well. That ABA is admissible only because the request is a
convergent set operation against the exact same target definition and state,
not a toggle. A release controller must additionally require its monotonic
sealed observation and immutable release identity before minting each grant;
it must not treat possession of an old content token as release authority.
Write grants are short-lived and request-bound, and a fresh grant is required
for every retry. Deployments whose threat model includes capture and replay of
an already-consumed grant across a supervisor restart must configure a durable
`citywriteauth.ReplayGuard`; the default in-memory replay guard deliberately
does not make that stronger promise.

The endpoint is still a city mutation. A hardened deployment must use the
existing `X-GC-City-Write` single-request grant, bound to the exact PUT path and
body, plus `X-GC-Request`. The token is concurrency control, not authority.

The lock coordinates every supported writer in the serving supervisor:
agent/rig/provider/formula mutations use the same `configedit.Editor`; pack
import uses `controllerState.SerializeConfigWrite`, which delegates to
`Editor.Do`; and a local CLI detects the running supervisor and routes its
mutation through HTTP. Direct file edits and an independently started offline
CLI do not participate in that process lock and are unsupported while the
supervisor owns configuration. The endpoint re-loads all relevant sources
while locked and fails closed on invalid or missing sources, but no userspace
API can make an unrelated process's raw filesystem writes transactional.

This mutable target token is deliberately separate from immutable release
identity such as the supervisor build, reviewed pack closure, or deployment
policy digest. Release controllers must bind both: immutable identity proves
which release is running; the suspension token fences the one desired-state
mutation.
