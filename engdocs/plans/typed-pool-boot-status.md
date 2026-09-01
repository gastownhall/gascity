# Typed pool-boot status plan

*Status: implementation-ready decomposition. Producing bead:
`ga-r1l8y2.1`. Architecture source: `ga-r1l8y2` Decision 1.*

## Outcome

Scripts, the dashboard, and supervision tooling can read startup progress from
typed fields on both `GET /v0/cities` and `GET /health`. The existing
`status`/`phase` fields remain bare phase tokens; pool-hook completion is carried
as one optional `PoolBootStatus {done, total, agent}` shape shared by both
endpoints. The CLI renders that structure for people without inventing or
parsing a delimiter grammar.

The work also closes the pre-existing startup-phase allowlist gap for
`running_pool_on_boot`, regenerates every contract consumer, and updates the
control-plane worked example.

## Grounded current state

| Boundary | Current evidence | Consequence |
| --- | --- | --- |
| PR #5332, head `94d01b5d60` | The proposed progress callback writes `running_pool_on_boot:<done>/<total>:<agent>` into `cityInitProgress.status`; `statusDisplayText` parses it with prefix and split operations. | Preserve the progress signal while replacing its packed transport before it becomes a contract. |
| `internal/api.CityInfo` and `internal/api.SupervisorStartup` | Both currently expose only the bare startup token plus completed phases. `/health` copies `CityInfo.Status` into `SupervisorStartup.Phase`. | Add one shared optional typed structure and keep both existing token fields unchanged. |
| `cmd/gc` startup projection | `cityInitProgress`, `ListCities`, pool on-boot execution, readiness waiting, and CLI display form the state-to-operator path. | Carry structured progress through the existing projection; do not add a parallel state model. |
| `startupPhaseOrder` | `running_pool_on_boot` is a real phase but is absent from the ordered allowlist. | Add the phase and lock the completed-phase result with a regression test. |
| Dashboard source audit | Hand-written dashboard TypeScript contains no `running_pool_on_boot`, `pool_boot`, status-colon split, or matching regex consumer. Only generated `phases_completed` fields are present. | The expected dashboard change is generated contract refresh plus an audited no-parser result unless later work introduces a consumer. |
| Generated contract | OpenAPI, the generated Go client, the committed reference schema, and dashboard TypeScript derive from the Huma Go types. | Regenerate from source and pass the full dashboard/schema freshness gates; never hand-edit generated output. |

The repository has no remote named `upstream`; its canonical
`gastownhall/gascity` remote is `origin`, so archaeology and PR comparison used
`origin/main` and `origin/pr/5332`.

## Work packages

### 1. RED contract tests — `ga-r1l8y2.1.1`

The validator authors failing tests for the two endpoint shapes, optional-field
omission, special-character agent names, phase completion, completion-driven
pool progress, and CLI formatting from structured values. The bead records
focused RED commands so builders inherit a precise contract instead of
reinterpreting the ruling.

### 2. Shared API wire shape — `ga-r1l8y2.1.2`

Add the single exported `PoolBootStatus` wire type and reuse it on `CityInfo`
and `SupervisorStartup`. Keep `Status` and `Phase` as bare tokens, copy the same
`CityInfo.PoolBoot` value into the health projection, and preserve
`omitempty`. This package owns only the typed API contract and focused API
behavior; it does not invent another internal mirror.

### 3. Startup-state projection — `ga-r1l8y2.1.3`

Report pool-hook completions as structured values, persist them beside the bare
phase in `cityInitProgress`, project them through the registry only during the
matching phase, and add `running_pool_on_boot` to `startupPhaseOrder`. Existing
parallelism, failure, panic, and no-callback behavior stay intact.

### 4. CLI rendering and wait diagnostics — `ga-r1l8y2.1.4`

Teach the readiness wait and display path to observe and format structured
progress. Normal phase rendering, deadline renewal, completion, stop-wait
semantics, and timeout diagnostics remain covered. No prefix, split, regex, or
round-trip packed string is permitted.

### 5. Contract, dashboard, docs, and gates — `ga-r1l8y2.1.5`

Regenerate both OpenAPI copies, the generated Go client, and dashboard
TypeScript clients from the source contract; repeat and record the dashboard
parser audit; update the API control-plane startup example; and run the
schema, dashboard, docs, focused Go, vet, and local dashboard-preview gates.
This is the final consistency package after behavior is present.

Each child bead carries its full measurable acceptance criteria in its notes.

## Dependency graph

```text
ga-r1l8y2.1.1  RED tests (validator)
        |
        v
ga-r1l8y2.1.2  shared API wire shape
        |
        v
ga-r1l8y2.1.3  startup-state projection
        |
        v
ga-r1l8y2.1.4  CLI rendering and wait diagnostics
        |
        v
ga-r1l8y2.1.5  generated contracts, dashboard audit, docs, gates
```

The chain is deliberate. It prevents generated artifacts from racing the source
shape, prevents CLI work from fabricating a second progress model, and gives the
final package one complete behavior to regenerate and document.

## Acceptance mapping

| Parent acceptance | Owning package |
| --- | --- |
| One shared `PoolBootStatus` on both endpoint types | `.2` |
| Bare `status`/`phase`; optional same-source `pool_boot` | `.1`, `.2`, `.3` |
| `running_pool_on_boot` phase-order fix | `.1`, `.3` |
| CLI formatting from structured progress | `.1`, `.4` |
| Both OpenAPI copies and generated clients in sync | `.5` |
| Dashboard parser audit and `make dashboard-ci` | `.5` |
| API control-plane example updated | `.5` |

## Risks and controls

- **Packed grammar survives internally:** the final diff audit rejects any
  colon-packed pool status or parser, including a private intermediate string.
- **Two endpoint shapes drift:** both fields use the same named Go type and
  `/health` copies the city-list projection rather than rebuilding progress.
- **Stale progress leaks across phases:** registry tests require `pool_boot` to
  be present only during `running_pool_on_boot` and omitted afterward.
- **Concurrency changes progress order:** callback tests require monotonic,
  serialized completion counts without weakening existing hook concurrency.
- **Generated artifacts hide source drift:** regeneration is deferred until the
  source behavior is complete, then checked by OpenAPI/client freshness and
  dashboard CI.
- **PR #5332 lands first:** these packages are a fast follow to its progress
  work; builders rebase on current `origin/main` and port the smallest proven
  slice rather than reimplementing unrelated reload/wait changes.

## Handoff

- `ga-r1l8y2.1.1` is labeled `needs-tests` for `gascity/validator` and is the
  only immediately ready child.
- `ga-r1l8y2.1.2` through `.5` are labeled `ready-to-build` for
  `gascity/builder` and become ready in dependency order.
- Close `ga-r1l8y2.1` only after this exact plan path is committed and
  re-verified clean, then dispatch every child with `gc sling --nudge` and
  verify each route in the ledger.
