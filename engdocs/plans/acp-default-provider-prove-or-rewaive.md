# ACP default provider prove-or-rewaive plan

*Status: implementation-ready decomposition. Producing bead:
`ga-uz5t3a.9`. Durable governance owner: `ga-uz5t3a` (keep open).*

## Outcome

Resolve the remaining `runtime.Provider` waiver for the exact city-less ACP
constructor. `internal/runtime/acp.NewSeamBacked` either gains a direct,
cross-platform, hermetic proof through
`internal/runtime/runtimetest.RunProviderTests`, or retains a short waiver whose
reason names the current verified barrier.

This is one implementation package, `ga-uz5t3a.10`. The proof investigation and
the final ledger disposition stay together because they are two outcomes of the
same evidence check. Splitting them would allow the test, ledger claim, binding
assertion, and generated `TESTING.md` row to disagree.

## Grounded current state

| Boundary | Current evidence | Consequence |
| --- | --- | --- |
| `cmd/gc/runtime_registry.go` | The exact `acp` route returns `NewSeamBackedWithDir` when a city path exists and `NewSeamBacked` when it does not. | The default constructor is live production wiring and needs its own disposition. |
| `internal/runtime/acp.NewSeamBacked` | It wraps `NewProvider`, whose state root is `os.TempDir()/gc-acp-<euid>`. | A proof must exercise shared default state; injecting another directory proves a different constructor. |
| `internal/runtime/acp/conformance_test.go#TestACPConformance` | The full shared contract already runs against `NewSeamBackedWithDir` and an isolated fake-ACP fixture. | Reuse the fixture where possible, but do not cite this test as proof of `NewSeamBacked`. |
| Commit `450c2b5f22` / PR #5198 | The analogous subprocess default constructor was proved with PID-scoped names and test-owned cleanup. The same investigation retained ACP's waiver because its default socket path lacked a Darwin-safe short-root fallback. | Process-unique naming is a useful precedent; the old Darwin finding must be re-verified against current code and CI rather than assumed away. |
| `ga-uz5t3a.8` | The active builder package staggers all eight waiver dates and adds the date-distinctness guard in the same ledger and generated documentation. | ACP disposition work starts only after `.8` is present on `origin/main`, avoiding overlapping edits and stale expiry assumptions. |

## Work package

`ga-uz5t3a.10` is a direct child of the durable governance bead, matching the
other per-constructor packages in
`engdocs/plans/runtime-provider-waiver-prove-or-retire.md`.

The builder must first re-confirm the live registration and the historical
Darwin constraint. A proved result is eligible only when a named integration
test directly calls `RunProviderTests`, its factory directly returns
`NewSeamBacked`, and required Linux and macOS execution is ungated and safe in
the shared per-user directory. The proof must isolate names and cleanup without
mutating `TMPDIR`, injecting a state root, deleting the shared provider root, or
disturbing foreign ACP sessions.

If that exact composition cannot be proved hermetically, the builder retains
the waiver under `ga-uz5t3a`, records the reproducible blocker in the waiver
reason and bead notes, and chooses a new short expiry distinct from every live
runtime-provider waiver date. A production path-selection change, new fallback,
or generic contract-runner change is a separate architecture question rather
than hidden scope in this package.

The child bead carries the full measurable acceptance criteria, including the
ledger binding assertion, repeated and concurrent-process evidence for a proof,
focused ACP integration coverage, provider-ledger synchronization, the relevant
integration shard, the fast unit baseline, and `go vet ./...`.

## Dependency graph

| Work item | Blocked by | Release condition |
| --- | --- | --- |
| `ga-uz5t3a.10` | `ga-uz5t3a.8` | The staggered expiries and distinctness guard are present on `origin/main`; bead closure alone is not sufficient. |

No dependency on the other provider-specific siblings is needed. ACP's process
fixture and default-directory behavior are independent of the T3 bridge, K8s,
herdr, tmux, SSH, and hybrid dispositions.

## Risks and controls

- **Shared-state damage:** the default provider root may contain live sessions
  from another process. The proof path uses collision-proof names and removes
  only test-owned sessions and artifacts; deleting or resetting the shared root
  is out of scope.
- **False Linux-only proof:** the existing isolated fixture explicitly shortens
  its root on Darwin. A green Linux run cannot retire the waiver without a
  Darwin-capable required-lane result.
- **Central-ledger conflict:** `.8` and `.10` both affect the provider ledger,
  its binding tests, and generated `TESTING.md`. `.10` rebases from the landed
  `.8` result before editing those files.
- **Expiry drift:** 2026-09-12 is placeholder runway, not an automatic renewal.
  A retained waiver gets a newly justified, bounded, non-colliding date.

## Handoff

- `ga-uz5t3a.10` is labeled `ready-to-build`, routed to `gascity/builder`, and
  blocked by `ga-uz5t3a.8`.
- `ga-uz5t3a.11` was created during recovery before the pre-existing `.10` was
  rediscovered; it was immediately closed as an unassigned duplicate and has no
  implementation work attached.
- Close `ga-uz5t3a.9` only after this plan is committed and its exact path is
  re-verified clean.
