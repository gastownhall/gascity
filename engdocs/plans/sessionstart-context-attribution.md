# Attribute added context while keeping the person's prompt visible

PM package for completed designer artifact ga-5hgak0fw, reconciled under
PM bead ga-cjji26i6 on 2026-11-04. The design is its bead description;
five wireframe scenes have durable JSON in its notes. The original plan is
commit `57fc526c4839582d1199c94128d466537afc0fbc`. This update starts from
fresh main `9936937d0b5e28e6c540443030634f03bb8d999e` and records the
architect's approved ruling on ga-7r7b7no5. The design's source traces were
pinned to `c10884f82bdcf3e8b1f9670519a6055746b77465`.

## Product disposition

Accept Option A: label added context and retain its full original text. The
dashboard collapses only positively attributed Gas City context. Other text
the host marked synthetic stays open and labelled, including MiMo checkpoint
copies of real prompts. Attribution comes from recorded provider facts, never
from text prefixes, ids, titles or part position. The label records a claim;
it grants no trust, access or redaction authority.

The design traces a material presentation failure: when injected context
sorts first, worker normalization selects it as the prompt and the structured
dashboard suppresses the person's actual text. That chain remains in the
fresh source. It is a source trace, not real-host evidence. The specific
SessionStart case is latent until source build ga-9x5bnoa6 lands and is used.
Raw and plain transcript access retain the text today.

Keep this follow-up P3 under normal machinery admission. Correct person-prompt
selection first within its scope. Retain source landing prerequisite
ga-1tklrva2 on every implementation carrier; add no rollout or deployment
blocker to the original delivery. Its budget deferral is unchanged. Existing
ga-dv5212wu remains the sole real-binary smoke scope, and ga-meilzh keeps its
separate work.

## Approved architecture and reconciliation

The ruling is the notes of ga-7r7b7no5, delivered by architect mail
gcg-528385, its serialized-output correction gcg-529409, and Amendment 1
notified by gcg-536577. The record deliberately remains open, unassigned and unrouted,
with parent-record, ruling-recorded and maintainer sync labels. Q1–Q5 are
decided and Q6, MiMo's message-level provenance, is deferred. Its open status
does not withhold approval and its eventual closure would not prove approval.

The architect approves the existing S1 → S2 → S3 chain as independently
shippable increments. Keep CLI and API together on the existing S2 carrier.
No atomic build or coordinated deployment is required; the SPA is embedded
in the binary. Accept the documented interim visibility below, keep P3 and
normal admission, and add no rollout blocker.

| Layer or surface | Approved contract |
|---|---|
| Reader | `ContentBlock.Origin *BlockOrigin`, JSON `gc_origin`, shape `{kind, hook, ignored}`; kind is `attested` or `host_marked` |
| Worker | Its own `HistoryBlockOrigin` mirror, JSON `origin`; preserve existing firstText semantics while skipping origin-bearing blocks |
| CLI | `message.content[].gc_origin` and `blocks[].gc_origin` share the reader shape; labels are `[TYPE]`, `[TYPE:context <hook>]` or `[TYPE:host-marked]`, with `,ignored` appended only to a claim inside the brackets |
| Structured API/SSE | Optional `origin` with explicit kind on text and unknown variants only; `schema_version` remains `session.structured.v1` |
| Dashboard | Collapse attested only; host-marked and unknown kinds stay open, labelled and available in full |

The design's proposed source/synthetic fields are superseded. The hook is
opaque validated data, never a Go allow-list. Origin never feeds turn
identity. All blocks, raw provider data and ordinary origin-less behavior
remain represented.

Amendment 1 resolves RC-12 in response to gcg-530433. Each typed field acts
independently: synthetic decides whether there is an origin, the valid hook
decides its tier, and only the literal true value of ignored sets Ignored.
A mistyped ignored field leaves the existing origin intact with Ignored
unset. A synthetic value other than literal true yields no origin. The
reader matrix is now 15 classes, RC-1–RC-15, with corrected RC-10/RC-12
and new RC-15. A1-11 covers the six mistyped flag values, both hook cases
and the worker person-prompt regression.

PM has verified the ruling and Amendment 1 and removed only the three
native blocks edges to the architecture record. All source/predecessor
actual-main conditions, routing labels and budget stamps stay intact.
The architecture record remains open as the approved durable reference.

## Compatibility while the stages land

| After | Visible behavior and public contract |
|---|---|
| S1 | Person-prompt selection is corrected. `gc_origin` is already public in CLI `message.content[]`; synthetic-only text moves from `text` to `blocks`. CLI plain remains unchanged, hand-copied `blocks[]` has no origin yet, and the API schema/generated clients do not change. Mixed added context is still hidden in the structured dashboard. |
| S2 | CLI labels and both JSON paths are complete. Structured HTTP/SSE exposes `origin` and generated clients. Dashboard context disclosure is still pending. |
| S3 | The accepted labels, visible host-marked text and attested-context disclosure are complete. |

Origin-less output, signatures and resume behavior remain unchanged. For
origin-bearing messages affected by S1 or S2, old structured resume tokens
can receive the existing history_rewritten reset or an upsert on reconnect.
The normalized reader/CLI shape change does not change verbatim raw output.
Document these changes with or after their stage; never describe the full
accepted presentation as shipped before S3.

## Residual surfaces

The ruling's R-1–R-7 remain explicit limits. None gains attribution by parsing
its text, and none adds work to these carriers.

| Residual | Behavior and lift condition |
|---|---|
| R-1 Conversation/text turns | Joined text without a marker; a future typed per-turn blocks option is needed to distinguish its parts. |
| R-2 Peek structured fallback | No per-block provenance; lift only when transcript-backed block identity is available. |
| R-3 Raw output | Provider bytes remain verbatim. |
| R-4 gc-built Peek/SSE frames | These frames have a different meaning of synthetic and carry no host-part origin. |
| R-5 Host title | Host-owned; reuse the title check in ga-dv5212wu. |
| R-6 Non-text parts | No attribution from these flags; revisit only on a provider-typed non-text contract. |
| R-7 Other providers | Origin stays nil until a provider supplies a typed marker; Claude turn origin is a separate identity fact. |

## Work packages

All are `ready-to-build` because the source is a completed design. Each
carrier includes builder-owned TDD and implementation; no separate validator
or overlapping build bead is created. The full acceptance is in the beads.

| Carrier | User outcome and scope | Additional prerequisite |
|---|---|---|
| ga-crlix2lh | The person prompt remains the person text even when context sorts first. Reader/worker attribution, normalization, representation and signature coverage. | Source actual-main condition ga-1tklrva2 |
| ga-dm5w848w | Plain/JSON logs and structured HTTP/SSE identify added context while preserving full text. Typed projection, generated contracts and associated reference material. | Reader/worker actual-main condition ga-94feofcg |
| ga-cdjy7ahl | The dashboard displays the person prompt and labelled original context with accessible disclosure. Presentation, integration proof and final user guidance. | Projection actual-main condition ga-2gzdcb0q |

The first increment addresses the prompt-selection failure. Until the final
presentation increment, context rows can remain incomplete in the dashboard;
that intermediate limitation is explicitly accepted under ruling section 1.4.

### Reader and worker acceptance

Cover the design's human-only, mixed, reversed-order, synthetic-only,
host-marked, ignored, unknown-metadata and MiMo checkpoint cases through the
OpenCode, MiMo and shared ZCode reader entry points. Preserve entry count,
ids, parents, role, order, selected branches, tool attachments/results and
provider failure/torn-read semantics. Never derive turn identity from the
delivered text or reuse raw provenance/derived flags for a different meaning.

Keep all text and raw provider content. Preserve ordinary single-text
representation and the approved origin-bearing block representation. A
synthetic-only message has no fabricated person prompt. Confirm signature
changes for attribution differences while preserving id-first equivalence
and snapshot overlap. Update the owning invariant and conformance material.

### CLI and structured API acceptance

Use the approved typed fact through the worker boundary. Keep full multiline
plain output and displayable-tail behavior. JSON, structured HTTP and SSE
retain all block text and carry consistent attribution. A human-only message
keeps its previous output. Domain classification stays in its owner.

Regenerate OpenAPI, clients, TypeScript/zod and generated reference output
from their authoritative sources in the same contract change. Generated
`docs/reference/cli.md` and schema files are never hand-edited. Pin raw,
conversation/text and fallback compatibility to the architect's explicit
decision; document accepted residuals instead of silently changing them.

### Dashboard acceptance

For mixed messages, display the person's prompt and the separate labelled
context. Gas City context has the specified collapsed disclosure and exact
original text. Other host-marked rows remain open. Synthetic-only messages
retain their identity and place without a USER PROMPT row. Human-only
rendering remains unchanged.

Verify click, Enter and Space; accessible button names, `aria-expanded` and
`aria-controls`; hidden collapsed content; visible focus retained on the
button; text labels and contrast; and disclosure state across an SSE refresh.
Update the existing suppression regression instead of weakening it. Finish
one both-provider example across reader, worker, structured API/SSE and the
dashboard. Host doubles do not establish real-host delivery.

For attested plus ignored, PM approves the title `Gas City - <hook>` and
caption `Added by Gas City. Not sent to the model (ignored).` The hook stays
visible; the qualifier describes model delivery without implying verification.
CLI and wire docs land with or after S2, dashboard guidance with or after S3.
Generated reference files are regenerated from their authoritative sources.

## Actual-main conditions

ga-94feofcg tracks actual landing of ga-crlix2lh; ga-2gzdcb0q tracks actual
landing of ga-dm5w848w. Both records remain open, unassigned and unrouted,
with `gate-tracker` and `parent-record` labels. Never sling them.

PM closes a condition only after independently verifying the reviewed
change on freshly fetched main, its merged SHA/source mapping and intended
owner changes. Builder/reviewer closure, a push or budget expiry is not a
landing. Carry each condition through review/deploy/MPR and notify PM on
landing or retirement. Conditions never block their own build, review or
deployment. Retirement or consolidation needs explicit scope reconciliation.

## Verification and scope controls

Use meaningful RED/GREEN evidence on each implementation carrier, appropriate
Bazel owner/conformance tests, generated-artifact checks and required hooks.
Run tests through the isolation wrapper. API/schema/dashboard scopes also
owe dashboard client/build drift checks and a locally served app. Public
documentation follows the Gas City docs skill. No public page or diagram is
required solely to publish this internal plan.

Complexity acceptance is zero growth against the actual pinned target merge
base, or `GATE_BASE_SHA` in a gate. The full complexity-zero-growth rule
supersedes the design's literal check-pass condition and historical base pin.
A green base requires a green merge. A red base requires retained base/merge
outputs with no worse row, plus a measured diff with no new or regressed
function and no deleted function that still lives outside the scan. Inspect
the diff lines rather than its exit status. Never refresh
`engdocs/complexity-baseline.json` in feature work.

No carrier changes the plugin transport, provider payload, runtime lifecycle
or supported host versions. No new smoke, budget exception or redundant
repair is authorized. Commit and re-verify this plan before closing the PM
authoring bead, then verify routes and context mail against live records.
