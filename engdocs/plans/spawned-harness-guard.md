# Preserve host protections in integration-spawned gc processes

Owner: `ga-g5tames`, the mayor's follow-up to `ga-fuh0b8` and review
`ga-292odm0`. Planning base:
`b916251bcfa24beed7be2a61f385bd012915ff2a`.

An integration harness must mark the gc processes it launches so existing
host protections remain active. This follow-up pins that environment
contract and covers the additional host protections in `internal/supervisor`.
The mayor's narrowed-design ruling on `ga-fuh0b8` is authoritative: extend
host protection without changing the argv-based behavioral predicate or
disabling session-scoped process scans.

## Wait for the prerequisite's actual landing

The follow-up is blocked by same-store landing guard `ga-fbmitnu`.
`ga-fuh0b8` is closed at its builder handoff; review `ga-292odm0` is still
in progress. Fresh origin/main at the planning base lacks
`proctable.TestSpawnedEnv`. A closed build bead does not satisfy the mayor's
explicit instruction to build this follow-up only after that change lands.

Clear the guard only after verifying the source-to-main landing through the
existing review/deploy or merged-PR record against freshly fetched main,
and checking the shipped signal and harness stamp. Record the main SHA and
the source mapping, including a squash mapping where applicable. A source
branch, review close or push alone is insufficient.

PM then resumes this same carrier against the landed tree and creates its
concrete work packages. No implementation or test-authoring child is
dispatched before that condition clears. Preserve the existing prerequisite
review/deploy path and priority; this follow-up does not block its review.

## Supervisor boundary audit

The supervisor predicate has both host-protection and behavioral uses.
The following findings come from reading the planning base, not from
executing host operations:

| Use | Observed boundary | Follow-up requirement |
|---|---|---|
| `DefaultHome`, `internal/supervisor/config.go` | The test check prevents fallback to the user's real `~/.gc` when `GC_HOME` is absent | A marked spawned process must receive that protection too |
| `Registry.refuseHostRegistryDuringTests`, `internal/supervisor/registry.go` | The check prevents writes beneath the host registry path; registry mutators call it | A marked spawned process must receive that protection too |
| `shouldSeedIsolatedSupervisorConfig`, `internal/supervisor/config.go` | The predicate also controls automatic isolated configuration seeding | Preserve its existing behavior; the safety requirement does not authorize a blanket predicate change |

The root's behavior-only exit cannot cover the first two uses: their code
explicitly protects host state. If the landed prerequisite changes these
facts, re-read and record the new boundary before authoring tests.

## Acceptance contract after landing

1. A regression test exercises the integration environment-building path
   used for exec'd gc processes and fails if it stops providing a non-empty
   `GC_TEST_SPAWNED`. An ambient marker must not make a missing harness
   stamp look correct. Cover the managed-Dolt and non-Dolt environment
   variants without starting real services.
2. A spawned gc process with a normal non-test executable name and the
   shared non-empty spawned signal receives the existing supervisor
   protections for missing `GC_HOME` and host-registry mutations. Refusal
   happens before host I/O. Tests use temporary homes and registry fixtures,
   never the operator's actual home or registry.
3. Preserve normal production behavior with no spawned marker and a normal
   executable name. Preserve existing Go/Bazel test-binary protection,
   isolated configuration behavior and the prerequisite's permitted
   session-scoped scans. Any non-empty marker has the fail-closed meaning
   ratified on `ga-fuh0b8`; do not invent a special disabling value.
4. Coverage of behavior already working on the landed base records truthful
   base PASS, deliberate local mutation failure and restored PASS. A real
   missing guard uses a genuine failing test before implementation. Do not
   manufacture a defect RED for the harness stamp. Tests and implementation
   for each scope travel on the same carrier through validator and builder.
5. Use owning Bazel targets with named verbose PASS evidence for new cases:
   `//internal/supervisor:supervisor_test` and the appropriate
   `--config=integration //test/integration:integration_test` selection.
   Check the owning BUILD file and retain the harness leak guards. The
   matching integration smoke tier and full required repository gates must
   also pass or carry the established failure attribution. Regenerate BUILD
   inputs for added files/imports and respect resource ratchets.
6. Complexity has zero growth against the pinned target merge base. Never
   edit `engdocs/complexity-baseline.json` in this follow-up.

## Resume and handoff

Keep `ga-g5tames` open, unassigned, labelled `needs-pm`, and routed to PM
behind the landing guard. After verified landing, re-check phase and existing
work before decomposition. Separate coverage-only characterization from a
real missing-guard fix using the supported validator/builder workflows;
avoid duplicate test and implementation beads for one scope. Any new
architectural question goes to the architect rather than expanding the
mayor's ruling.

Tracker import is a no-op: no tracker-to-beads or sibling tracker skill was
found in the session or Actual pack skill paths. No tracker manifest is
created. This plan authorizes no runtime installation, deployment, service
restart, live host process scan or unrelated safety-policy change.
