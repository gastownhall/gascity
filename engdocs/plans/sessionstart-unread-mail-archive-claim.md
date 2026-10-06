# SessionStart unread-mail archive claim

**Goal:** An agent reading the SessionStart mail reminder should be told that a message was archived only when that delivery path archives it. Source: reviewer finding `ga-kvai2o` from `ga-70qwum`.

## User impact

When a session ID is unresolved, SessionStart can show auto-handoff mail through its read-only ordinary-mail block. The reviewed change would tell the agent that those messages were archived and no longer in the inbox, although all remain unread. The next prompt can still deliver and archive them, so the impact is transient, but the reminder is false at the moment the agent sees it.

The reviewer reproduced this with `GC_SESSION_ID` unset, `GC_ALIAS` set, two empty-body handoffs, and one body-bearing handoff. With the session ID resolved, the ordinary delivery and archive claim are accurate. The accepted review is on commit `4f42910731bd2d4e2cbe5fc8de5d15b1e60ccaa7`; as of 2026-10-06 08:20 UTC, that commit is not in `origin/main`. Its deploy path is `ga-yw3kxx`.

## Work packages

| Order | Bead | Route | Acceptance |
| --- | --- | --- | --- |
| 1 | `ga-cmt6iq` | validator (`needs-tests`) | A focused regression test fails on the unresolved-session case before the fix, confirms the read-only block leaves all three messages unread, and keeps resolved SessionStart and UserPromptSubmit as controls. |
| 2 | `ga-lxx1g0` | builder (`ready-to-build`) | The unresolved-session reminder makes no archive or inbox-removal claim; actual archiving paths keep their existing wording and behavior; the validator test and relevant existing tests pass. |

Both packages trace to `ga-kvai2o`. Both depend on the original deploy bead `ga-yw3kxx`, and the builder package also depends on the validator package. The deploy bead can close before its PR merges, so the downstream agent must confirm the original change is present on `origin/main` before basing work on it. If it has not landed, keep the package waiting; do not infer landing from bead closure.

## Scope and risks

The fix concerns the accuracy of the reminder and the associated regression tests. The resolved-session and check-path output must stay as reviewed. There is no UX design or new architecture decision. The main sequencing risk is writing against the review branch before its accepted feature lands; the explicit merge check above protects that boundary.
