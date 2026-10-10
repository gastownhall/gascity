# Soft reload accepted-drift count in JSON

PM intake: `ga-eplc4k` · Delivery: `ga-cfmoy5` · Priority: P3

Automation callers of `gc reload --soft --json` should receive the accepted
config-drift count already available to callers using text output. A count of
zero means the controller evaluated acceptance and accepted no sessions. An
absent count means it did not report an evaluation; callers must be able to
distinguish those cases.

## Evidence and contract

The architect's source inspection at `cd253a8aa3` identified the omission in
the reload success projection. PM re-read the reply, CLI success branch and
reload result schema at intake. This is source evidence, not a runtime
reproduction. History searches found no existing projection of
`AcceptedDriftCount` in the lifecycle JSON envelope, and the open-bead search
found only this intake scope.

The accepted contract uses the reply's existing name:
`accepted_drift_count`. It is an optional non-negative integer, present only
for a successful soft request whose reply supplies a count. Zero is present
as `0`; an absent count stays absent. Non-soft requests omit the key.
The optional additive field keeps `schema_version` at `"1"`, consistent with
the architect's reload envelope ruling on `ga-bbhl94` and the existing schema
which permits additional properties.

| Request/reply | Expected JSON count |
| --- | --- |
| Soft, applied or no_change, count supplied | Exact count, including zero |
| Soft, any successful outcome, count absent | Key absent |
| Soft, async accepted, count supplied | Exact supplied count |
| Soft, usual async accepted before evaluation | Key absent; no synthesized zero |
| Soft with the v2 unsupported warning and no count | Key absent; warning retained |
| Non-soft, even if a synthetic reply supplies a count | Key absent |

The field does not change text output, error behavior or how the controller
calculates counts. The v2 fixture preserves the distinction between unsupported
acceptance and an evaluated zero result. Async success usually precedes
evaluation; forwarding a supplied count does not make async reload wait.

## Work package and order

This narrow goal needs one delivery bead rather than several overlapping
implementation scopes:

| Bead | Scope | Route | Estimate |
| --- | --- | --- | --- |
| `ga-cfmoy5` | Expose and validate accepted-drift count in reload JSON | `needs-tests` → validator → builder on the same bead | Small |

The dependency graph is:

```text
ga-bbhl94: reload JSON warnings delivery
    └─ blocks ga-cfmoy5: accepted-drift count delivery
ga-eplc4k: PM intake
    └─ discovered-from origin for ga-cfmoy5
```

The architect requested sequencing after `ga-bbhl94` lands because both scopes
edit the same projection and schema. PM encodes that order as a dependency on
the delivery bead under the PM dependency contract. This schedules shared
edits; it does not introduce a functional dependency. Before authoring RED
tests or building, verify the warnings implementation is actually in
`origin/main` and record its shipped SHA. Closing an architecture or test
handoff alone does not satisfy this guard. Re-point the dependency if the
upstream delivery identity changes. The session-timing notice `ga-h196xt` is
independent and adds no prerequisite here.

## Acceptance and verification

The delivery bead contains the full measurable acceptance criteria and a
`Build:` scope. Its required observations are:

1. Successful soft JSON replies forward exact counts, including zero, in one
   parseable JSON object with empty success stderr. Removing the count leaves
   the same payload as an equivalent reply with no count, including warnings.
2. Missing counts, non-soft requests and other lifecycle command payloads omit
   the field. The existing v2 warning remains available under the warnings
   contract delivered by `ga-bbhl94`.
3. The reload result schema declares an optional integer with minimum zero.
   Absent, zero and positive integer values validate. Negative, fractional,
   string, boolean and null values are rejected.
4. Text count lines and warnings, success exit status, failed/busy/timeout
   stdout, stderr and exit status retain their existing behavior.
5. Validator records assertion-based RED for the missing count and schema
   constraint, alongside passing omission and output controls. Use the
   existing isolated `cmdReload` hook harness and restore hooks on cleanup;
   no live city or supervisor reload is needed. Retain the upstream warnings
   tests through GREEN.

Builder implements this same bead after the validator handoff, with the
existing CLI reply and typed result schema as the scope boundary. Run focused
Bazel reload/schema checks, then required `make check` and active hooks;
update BUILD metadata if files or imports change. The PR explains the optional
additive field and the compatibility risk for callers using their own closed
schema. PM authors no implementation or tests.

## Risks and boundaries

The main risk is accidentally treating an absent count as zero; the presence
matrix and schema controls cover it. Shared-file sequencing and a confirmed
landing check avoid carrying a second independent edit over unlanded warnings
work. A caller's private closed JSON schema may reject an added key, although
the repository's reload schema permits it.

There is no new count computation, socket protocol, HTTP API, OpenAPI,
dashboard, generated-client or public documentation-page scope. No live
application of timing values, session drain or provider rebuild is introduced.

Tracker import was a no-op: no supported tracker source skill is installed.
No tracker-sync manifest was changed. Closing `ga-eplc4k` completes PM
packaging and publication only; implementation remains open on `ga-cfmoy5`.
