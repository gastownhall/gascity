export const meta = {
  name: 'sdo-review',
  description: 'Fable adversarial review of one store-domain-objects wave: behavior-preservation + convention/completeness lenses over a git-ref delta, synthesized to a verdict',
  phases: [{ title: 'Review' }, { title: 'Synthesize' }],
}

// args = { key, base, head, opportunity, designPath, verifyPath }
const A = args || {}

const PREAMBLE = `You are reviewing a code change in the Gas City Go SDK. The repo root is your cwd (a git worktree). The change is one wave of a program eliminating the "raw beads leak into business logic" antipattern: business logic must route bead IDs through typed domain objects returned by the class store (e.g. session.Store.Get(id) -> session.Info), NOT read bead.Metadata["..."] inline. De/serialization is confined to the store edge codecs (InfoFromPersistedBead, PersistedResponseFromBead, etc). Work/Graph classes legitimately keep beads.Bead as their domain object — that is NOT a leak. Reading gc.* control metadata in internal/dispatch is substrate, NOT a leak.

FIRST, gather the actual change and ground yourself in real code (do not trust any summary):
  git --no-pager diff ${A.base}..${A.head} -- . ':(exclude)*openapi.json' ':(exclude)*generated/*'
  cat ${A.designPath}      # the intended design for this wave
  cat ${A.verifyPath}      # verification already run
IMPORTANT: the working-tree checkout is NOT necessarily at ${A.head} (multiple reviews share this worktree). Ground yourself against the head COMMIT directly, checkout-independently:
  - read a file at head:   git --no-pager show ${A.head}:<path>
  - search the tree at head: git grep -n PATTERN ${A.head} -- PATHSPEC   (quote globs under zsh)
  - find MISSED old-pattern sites: git grep -n OLD_PATTERN ${A.head} -- cmd/gc internal
Do NOT trust a plain working-tree rg/Read for head-state claims — it may show a different commit. Every claim must be verified against ${A.head} via git show / git grep.

OPPORTUNITY: ${A.opportunity || A.key}`

const LENS_SCHEMA = {
  type: 'object', additionalProperties: false,
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object', additionalProperties: false,
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor', 'nit'] },
          file: { type: 'string' }, anchor: { type: 'string' },
          issue: { type: 'string' }, fix: { type: 'string' },
        },
        required: ['severity', 'file', 'issue', 'fix'],
      },
    },
    lensSummary: { type: 'string' },
  },
  required: ['findings', 'lensSummary'],
}

phase('Review')
const lenses = [
  { key: 'behavior', prompt: `LENS: BEHAVIOR-PRESERVATION & CORRECTNESS. This refactor must be behavior-identical (same runtime decisions, same on-store bytes, same wire/CLI output) unless the design explicitly says otherwise. Check: (1) does any edit change a comparison, default, ordering, filter predicate, or emitted string/value? (2) is every retired inline crack routed through a codec that returns the IDENTICAL value? Pay special attention to the cache-first read-model tier (the #3939/#3941 dashboard-perf contract) — the cache-peek union must still hit BOTH legs and fall through identically; and to filter-then-enrich ORDER (enrichment downgrades stale active->asleep, and the state filter must see the persisted state exactly as ListFullFromBeads did). (3) the Manager.List "semantic upgrade" (label-only -> union feed that surfaces repairable type-lost beads): verify EVERY current caller was already pre-fed union rows so behavior is truly unchanged — find any caller that relied on the old label-only narrowing. (4) the GetPersistedResponse error-contract bridge: ErrSessionNotFound->ErrNotSession must hold the 400; absence must still 404; the conditional empty-type heal (RepairType) must fire at exactly the sites loadSessionBead healed. (5) partial-result envelope must keep serving degraded-but-nonempty. (6) any nil/empty/absent-metadata edge handled differently. Report blockers for any real behavior drift.` },
  { key: 'convention', prompt: `LENS: CONVENTION, COMPLETENESS & ANTIPATTERN-ADHERENCE. Check: (1) codec confined to the domain package; doc comments on new exported symbols. (2) is the wave FULLY addressed per its design, or left half-done (siblings still cracking raw beads in-scope)? Run the census: rg for ListAllSessionBeads(, InfoFromPersistedBead(, PersistedResponseFromBead(, GetWithBead(, ListFullFromBeads( across internal/api, cmd/gc, internal/worker — do the actual counts match what verify claims (0 where claimed 0)? (3) are the DEFERRALS sound — the WI-3 orders residuals (RunFromTrackingBead/MaxSeqFromLabels), the handler_mail mailbox-twin, and cmd_session InfoFromPersistedBead staying at 2 (reason+kill)? Is each genuinely out-of-scope/blocked, or is it silent scope-drop of an in-scope leak? (4) did it AVOID over-refactoring legitimate substrate? (5) any NEW leak or magic-string introduced (e.g. a raw bead.Metadata read added in the new code)? (6) were the guard/pin tests added as designed (the tier pin, the ListFromInfos oracle) and are they actually load-bearing (would they fail if the tier/behavior regressed)? gofmt/vet cleanliness. Flag over-reach as a blocker just as much as under-reach.` },
]
const lensResults = await parallel(lenses.map((l) => () =>
  agent(`${PREAMBLE}\n\n${l.prompt}\n\nReturn concrete findings (file+anchor+issue+fix) most-severe first, and a one-line lens summary. Empty findings array is the correct answer if the slice is clean.`,
    { schema: LENS_SCHEMA, model: 'fable', effort: 'high', label: `review:${A.key}:${l.key}`, phase: 'Review' })
)).then((r) => r.filter(Boolean))

phase('Synthesize')
const VERDICT_SCHEMA = {
  type: 'object', additionalProperties: false,
  properties: {
    verdict: { type: 'string', enum: ['approve', 'approve-with-nits', 'changes-needed'] },
    blockers: { type: 'array', items: { type: 'object', additionalProperties: false, properties: { file: { type: 'string' }, issue: { type: 'string' }, fix: { type: 'string' } }, required: ['file', 'issue', 'fix'] } },
    nits: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
  },
  required: ['verdict', 'blockers', 'summary'],
}
const verdict = await agent(`${PREAMBLE}

Two review lenses returned:
${JSON.stringify(lensResults)}

Synthesize a single verdict. De-duplicate. A finding is a BLOCKER only if it is a real behavior drift, a missed in-scope leak site, an introduced leak/regression, a broken/non-load-bearing guard pin, or an over-refactor of substrate. Style/doc issues are nits. A sound, genuinely-blocked deferral is NOT a blocker. verdict=changes-needed only if there is >=1 blocker. Be decisive and concrete — cite file:anchor.`,
  { schema: VERDICT_SCHEMA, model: 'fable', effort: 'high', phase: 'Synthesize' })

return { key: A.key, verdict, lensResults }
