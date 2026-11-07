# Plan: return the CLI JSON baseline change for bounded rework

- Work bead: `ga-oh3qev6`
- Date: 2026-11-07 UTC
- Original build: `ga-ok4ppoq`; amended contract: its R1–R11 architecture notes
- Previous rework/review: `ga-dwfu4z0` / `ga-utkn5t0`
- Rejected reviewed source: `630bbce576b08f2692c69f174dab7afa2094d3a6`
- Failed gate base: `5ce5db9a3528e5879ba86ebeabcbeff3e406657d`

The CLI JSON schema-baseline feature needs a fresh build/review handoff before
deployment can continue. Its merged instructions exceed the existing byte
budget, and a documentation commit lacks a citation accepted by the deployment
ancestry guard. Both constraints belong to the existing feature scope.

## Verified failure

The durable failed record and evidence are under the city's
`.gc/artifacts/gascity/ga-oh3qev6/gate-20261107-uncsb5sz/` directory:

| Constraint | Observed evidence | Required outcome |
| --- | --- | --- |
| Root instruction budget | `TestRootAgentsFileWithinBudget`: base 19,957 bytes, reviewed source 19,990, merged tree 20,075; limit 20,000. The feature adds 118 bytes. | The fresh canonical merge stays within the unchanged 20,000-byte limit and retains the required policy guidance. |
| Commit provenance | Ancestry guard exit 21 names `b8c95f51cea61aeff1dc798034341b06c0f0e2e9`, whose message cites only public issue #8453. | The rebuilt source passes the guard with actual, verified work lineage. |

The offending documentation commit belongs to the schema-baseline theme;
the helper's generic suggestion of an unrelated theme is not an established
finding. Naming a fabricated sibling or waiving the check would not resolve
the missing citation.

The failed gate stopped at the normal commit hook. Changed-package nogo
passed, but the full build, runtime tests, fast policy and drift checks did
not run. The previous review PASS supplies provenance; it does not clear the
new source or substitute for a complete deployment gate.

## One work item

| Bead | Scope | Intake and route |
| --- | --- | --- |
| `ga-oh3qev6` | Refresh this same feature, fit its instructions within the root budget, and repair the source range's work citations through the build/review workflow. | `ready-to-build` → `gascity/builder` → fresh review → fresh deployment gate |

Reuse the existing bead. No new implementation child or prerequisite is
needed, and no blocking dependency is added. The original build and review
records stay closed as historical evidence. PM releases its claim after
routing; the deployment work is not closed as if it had shipped.

## Acceptance and build scope

1. Work from current `origin/main` on an owned, scoped build branch. Preserve
   the rejected SHA and provenance branch as evidence; publish a newly
   reviewed source rather than repointing the old deployment approval.
2. The root `AGENTS.md` in the fresh canonical merge is at most 20,000 bytes.
   Preserve the original requirement to name the schema-baseline invariant
   and provide its owning instructions. Follow the existing instruction-file
   policy for area-specific detail and concise navigation. Do not raise the
   budget, weaken its test, drop unrelated guidance, or bypass normal hooks.
3. The candidate's complete feature range passes the deployment ancestry
   guard with genuine citations. Verified existing lineage is `ga-oh3qev6`,
   `ga-ok4ppoq`, `ga-dwfu4z0`, and `ga-86czip9`. Carry the same-theme docs
   changes through the authorized builder/reviewer process, preserving
   attribution. No guard exception or invented work item is authorized.
4. Preserve the architect's amended AC1–AC8 and R1–R11 on `ga-ok4ppoq`:
   breaking schema changes require a version signal and baseline note/ref;
   additive changes require regeneration without a bump; all live schemas
   are covered; the prescribed command-version constants and extractor/error
   contracts remain intact. The scoped policy and PR-template updates remain
   part of this feature. Existing schema and generator tests retain their
   behavioral assertions.
5. Refresh generated schema-baseline data against the final live schema tree
   under the first-seed ruling already recorded on `ga-dwfu4z0`. Record the
   generator's actual summary, an unchanged second run, and verbose real
   PASS evidence for the live baseline and relevant generator tests. Do not
   use the rejected branch's old seed or review results as current evidence.
6. The original merged-budget failure is the existing RED evidence. Record a
   passing budget test and full docs sync on the fresh merge, plus relevant
   schema/generator proofs and required repository gates. Run tests through
   Bazel. Any added files/imports require synchronized BUILD files. Complexity
   has zero growth against the pinned base under the full pack rule;
   `engdocs/complexity-baseline.json` and `.github/CODEOWNERS` remain outside
   the feature edit set.
7. Return the new commit to reviewer, then deployer for a complete fresh gate.
   The future public PR must state the rule, say `Refs #8453`, tag
   `@julianknutsen`, propose the baseline CODEOWNERS entry, and record its
   freshly computed merge base. Its text excludes internal bead IDs,
   agent/role names, downstream pack names, local paths, and emails.

## PM completion

Commit and push this plan on its bead-specific docs branch; verify the
document is tracked and clean and the remote SHA matches. Append this bounded
`Build:` scope to the existing bead, replace `needs-pm` with
`ready-to-build`, and use `gc sling --nudge` to route it to builder. Verify
the live route, release the PM claim, and peek-verify the context mail. A
reported build-budget refusal remains normal backpressure; preserve its
deferral and do not force or retry the admission.
